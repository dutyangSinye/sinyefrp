import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useSessionStore } from '@/stores/session'

// 使用 history 模式：后端的 handleStatic 已对所有未命中路径回退到 index.html，
// 因此直接刷新 /clients 这类前端路由不会 404。

const routes: RouteRecordRaw[] = [
  {
    path: '/gate',
    name: 'gate',
    component: () => import('@/views/GateView.vue'),
    meta: { public: true, title: '登录' },
  },
  {
    path: '/',
    component: () => import('@/layouts/DefaultLayout.vue'),
    children: [
      { path: '', redirect: '/overview' },
      {
        path: 'overview',
        name: 'overview',
        component: () => import('@/views/OverviewView.vue'),
        meta: { title: '总览' },
      },
      {
        path: 'clients',
        name: 'clients',
        component: () => import('@/views/ClientsView.vue'),
        meta: { title: '客户端' },
      },
      {
        path: 'proxies',
        name: 'proxies',
        component: () => import('@/views/ProxiesView.vue'),
        meta: { title: '代理' },
      },
      {
        path: 'registry',
        name: 'registry',
        component: () => import('@/views/RegistryView.vue'),
        meta: { title: '客户端台账' },
      },
      {
        path: 'packages',
        name: 'packages',
        component: () => import('@/views/PackagesView.vue'),
        meta: { title: '部署包' },
      },
      {
        path: 'records',
        name: 'records',
        component: () => import('@/views/RecordsView.vue'),
        meta: { title: '生成记录' },
      },
      {
        path: 'settings',
        name: 'settings',
        component: () => import('@/views/SettingsView.vue'),
        meta: { title: '设置' },
      },
    ],
  },
  // 未匹配路径回总览；真正未初始化的用户会在守卫里被送到 /gate。
  { path: '/:pathMatch(.*)*', redirect: '/overview' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach(async (to) => {
  const session = useSessionStore()

  // 首次进入（含直接刷新深层路由）时先问一次服务端状态，
  // 否则守卫会把已登录用户误判成未登录。
  if (session.phase === 'unknown') {
    await session.boot()
  }

  const isPublic = to.meta.public === true

  if (!isPublic && session.phase !== 'ready') {
    return { path: '/gate', query: to.fullPath === '/' ? undefined : { next: to.fullPath } }
  }
  if (isPublic && session.phase === 'ready') {
    return { path: '/overview' }
  }
  return true
})

router.afterEach((to) => {
  const t = to.meta.title as string | undefined
  document.title = t ? `${t} · SinyeFRP` : 'SinyeFRP 管理平台'
})
