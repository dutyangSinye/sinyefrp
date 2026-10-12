<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useSessionStore } from '@/stores/session'
import { HttpError } from '@/api/http'
import { formatDuration } from '@/utils/format'

// 首次初始化（F01）与管理员登录（F02）。
//
// 两种模式共用一页：它们的差别只有标题、按钮文案和「要不要确认密码」，
// 拆成两个路由反而会让守卫多一层判断。密码规则由 /api/setup/status 下发，
// 不在前端硬编码 —— 避免前后端两套规则各说各话。

const session = useSessionStore()
const router = useRouter()
const route = useRoute()

const isSetup = computed(() => session.phase === 'setup')
const policy = computed(() => session.policy)

const formRef = ref<FormInstance>()
const form = reactive({ username: '', password: '', confirm: '' })
const submitting = ref(false)
const errMsg = ref('')

// 首次进入时也把服务端状态问一遍：用户可能直接打开 /gate。
onMounted(async () => {
  if (session.phase === 'unknown') {
    await session.boot()
  }
  if (session.bootError) errMsg.value = session.bootError
})

/** 密码须包含四类字符中的至少两类，与后端 auth.ErrWeakPasswordClasses 对应。 */
function classesCount(pw: string): number {
  let n = 0
  if (/[a-z]/.test(pw)) n++
  if (/[A-Z]/.test(pw)) n++
  if (/[0-9]/.test(pw)) n++
  if (/[^A-Za-z0-9]/.test(pw)) n++
  return n
}

const rules = computed<FormRules>(() => {
  const base: FormRules = {
    username: [
      { required: true, message: '请输入管理员账号', trigger: 'blur' },
      {
        min: policy.value.min_username_len,
        max: policy.value.max_username_len,
        message: `账号长度需为 ${policy.value.min_username_len}-${policy.value.max_username_len} 位`,
        trigger: 'blur',
      },
    ],
    password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
  }

  if (isSetup.value) {
    base.password = [
      { required: true, message: '请输入密码', trigger: 'blur' },
      {
        min: policy.value.min_password_len,
        max: policy.value.max_password_len,
        message: `密码长度需为 ${policy.value.min_password_len}-${policy.value.max_password_len} 位`,
        trigger: 'blur',
      },
      {
        validator: (_r, value: string, cb) => {
          if (!value) return cb()
          if (classesCount(value) < 2) {
            return cb(new Error('密码需包含大小写字母、数字、符号中的至少两类'))
          }
          cb()
        },
        trigger: 'blur',
      },
    ]
    base.confirm = [
      { required: true, message: '请再次输入密码', trigger: 'blur' },
      {
        validator: (_r, value: string, cb) => {
          if (value !== form.password) return cb(new Error('两次输入的密码不一致'))
          cb()
        },
        trigger: 'blur',
      },
    ]
  }
  return base
})

async function submit() {
  const formEl = formRef.value
  if (!formEl) return
  const ok = await formEl.validate().catch(() => false)
  if (!ok) return

  submitting.value = true
  errMsg.value = ''
  try {
    if (isSetup.value) {
      await session.setupAdmin(form.username.trim(), form.password)
      ElMessage.success('初始化完成，已进入管理后台')
    } else {
      await session.login(form.username.trim(), form.password)
    }
    const next = typeof route.query.next === 'string' ? route.query.next : '/overview'
    await router.replace(next)
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '操作失败，请重试'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="gate-wrap">
    <el-card class="gate-card" shadow="never">
      <h1 class="gate-title">{{ isSetup ? '首次初始化' : '管理员登录' }}</h1>
      <p class="gate-sub">
        {{
          isSetup
            ? '请创建管理员账号。该账号用于访问本管理平台，与 FRP 的 Dashboard 凭据相互独立。'
            : '请输入管理员账号与密码进入管理后台。'
        }}
      </p>

      <el-alert v-if="errMsg" :title="errMsg" type="error" show-icon :closable="false" style="margin-bottom: 14px" />

      <el-form ref="formRef" :model="form" :rules="rules" label-position="top" @submit.prevent="submit">
        <el-form-item label="管理员账号" prop="username">
          <el-input v-model="form.username" autocomplete="username" spellcheck="false" placeholder="例如 admin" />
        </el-form-item>

        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            autocomplete="current-password"
            placeholder="请输入密码"
          />
        </el-form-item>

        <el-form-item v-if="isSetup" label="确认密码" prop="confirm">
          <el-input v-model="form.confirm" type="password" show-password autocomplete="new-password" placeholder="请再次输入密码" />
        </el-form-item>

        <el-alert v-if="isSetup" type="info" :closable="false" show-icon style="margin-bottom: 16px">
          <template #title>
            密码至少 {{ policy.min_password_len }} 位，且需包含大小写字母、数字、符号中的至少两类。
          </template>
        </el-alert>

        <el-button type="primary" :loading="submitting" style="width: 100%" @click="submit">
          {{ isSetup ? '创建并进入' : '登录' }}
        </el-button>
      </el-form>

      <div class="gate-foot sf-muted">
        <template v-if="isSetup">
          初始化完成后无法通过本页面重置账号；如需重置请直接操作数据库。
        </template>
        <template v-else>
          登录会话有效期 {{ formatDuration(session.sessionTtlSeconds) }}，退出后立即失效。
        </template>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.gate-wrap {
  min-height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}

.gate-card {
  width: 100%;
  max-width: 420px;
  border-radius: 10px;
}

.gate-title {
  font-size: 18px;
  font-weight: 600;
  margin: 0 0 4px;
}

.gate-sub {
  color: var(--sf-muted);
  font-size: 13px;
  margin: 0 0 18px;
}

.gate-foot {
  margin-top: 16px;
  font-size: 12px;
  text-align: center;
}
</style>
