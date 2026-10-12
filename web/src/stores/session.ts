import { defineStore } from 'pinia'
import { api } from '@/api/endpoints'
import { HttpError, onUnauthorized } from '@/api/http'
import type { PasswordPolicy } from '@/api/types'

// 会话与初始化状态。
//
// 三态是刻意区分的，不能合并成一个 loggedIn 布尔：
//   unknown → 还没问过服务端
//   setup   → 尚未初始化，必须走初始化向导（后端对受保护接口返回 409）
//   login   → 已初始化但未登录（401）
//   ready   → 已登录
// 把 setup 和 login 分开，用户才能看到「首次初始化」与「管理员登录」两种不同的引导。

export type SessionPhase = 'unknown' | 'setup' | 'login' | 'ready'

const DEFAULT_POLICY: PasswordPolicy = {
  min_username_len: 3,
  max_username_len: 32,
  min_password_len: 10,
  max_password_len: 72,
}

export const useSessionStore = defineStore('session', {
  state: () => ({
    phase: 'unknown' as SessionPhase,
    username: '',
    policy: { ...DEFAULT_POLICY } as PasswordPolicy,
    sessionTtlSeconds: 0,
    /** 生效节点名与来源，显示在顶栏，便于识别「配置被环境变量覆盖」。 */
    nodeName: '',
    nodeSource: '',
    bootError: '',
  }),

  getters: {
    authed: (s) => s.phase === 'ready',
  },

  actions: {
    /** 注册 401 回调：会话过期时自动退回登录页。 */
    installInterceptor() {
      onUnauthorized(() => {
        if (this.phase === 'ready') {
          this.phase = 'login'
          this.username = ''
        }
      })
    },

    async boot() {
      this.bootError = ''
      try {
        const st = await api.setupStatus()
        this.policy = st.policy
        this.sessionTtlSeconds = st.session_ttl_seconds

        if (!st.initialized) {
          this.phase = 'setup'
          return
        }

        try {
          const me = await api.me()
          this.username = me.user.username
          this.nodeName = me.node.name
          this.nodeSource = me.node.source
          this.phase = 'ready'
        } catch (err) {
          // 401 是正常路径（没登录），其余错误要让用户看见。
          if (err instanceof HttpError && err.status === 401) {
            this.phase = 'login'
          } else {
            this.phase = 'login'
            this.bootError = err instanceof Error ? err.message : '恢复会话失败'
          }
        }
      } catch (err) {
        // 连 /api/setup/status 都拿不到：服务本身有问题，仍显示登录页并提示原因。
        this.phase = 'login'
        this.bootError = err instanceof Error ? err.message : '无法连接管理服务'
      }
    },

    async setupAdmin(username: string, password: string) {
      const r = await api.setupAdmin(username, password)
      this.username = r.user.username
      this.phase = 'ready'
      await this.refreshIdentity()
    },

    async login(username: string, password: string) {
      const r = await api.login(username, password)
      this.username = r.user.username
      this.phase = 'ready'
      await this.refreshIdentity()
    },

    async logout() {
      try {
        await api.logout()
      } finally {
        this.phase = 'login'
        this.username = ''
        this.nodeName = ''
        this.nodeSource = ''
      }
    },

    /** 登录后补齐顶栏所需的身份与节点来源。失败不影响进入后台。 */
    async refreshIdentity() {
      try {
        const me = await api.me()
        this.username = me.user.username
        this.nodeName = me.node.name
        this.nodeSource = me.node.source
      } catch {
        /* 顶栏信息是锦上添花，失败不阻塞 */
      }
    },
  },
})
