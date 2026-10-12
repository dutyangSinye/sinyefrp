<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessageBox } from 'element-plus'
import { useSessionStore } from '@/stores/session'

// 主框架：左侧导航 + 顶栏。
//
// 顶栏刻意不显示采集健康度：健康度是「按页」的概念（每个页面自己拉取并展示），
// 在这里再轮询一次会与服务端为单节点设计的采集节奏重复，还会制造两个口径。
// 顶栏只回答「我是谁、我在管哪个节点」。

const session = useSessionStore()
const route = useRoute()
const router = useRouter()

const NAV = [
  { name: 'overview', path: '/overview', label: '总览', icon: 'Odometer' },
  { name: 'clients', path: '/clients', label: '客户端', icon: 'Monitor' },
  { name: 'proxies', path: '/proxies', label: '代理', icon: 'Connection' },
  { name: 'registry', path: '/registry', label: '客户端台账', icon: 'Collection' },
  { name: 'packages', path: '/packages', label: '部署包', icon: 'Box' },
  { name: 'records', path: '/records', label: '生成记录', icon: 'Tickets' },
  { name: 'settings', path: '/settings', label: '设置', icon: 'Setting' },
] as const

const activePath = computed(() => '/' + (route.path.split('/')[1] || 'overview'))
const pageTitle = computed(() => (route.meta.title as string) || '')

const SOURCE_LABEL: Record<string, string> = {
  default: '内置默认',
  config: '配置文件',
  db: '管理台设置',
  override: '命令行/环境变量',
}

const sourceLabel = computed(() => SOURCE_LABEL[session.nodeSource] || session.nodeSource || '—')

const loggingOut = ref(false)

async function handleLogout() {
  try {
    await ElMessageBox.confirm('退出后当前会话立即失效，需要重新登录。', '确认退出', {
      confirmButtonText: '退出登录',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return // 用户取消
  }
  loggingOut.value = true
  try {
    await session.logout()
    await router.replace('/gate')
  } finally {
    loggingOut.value = false
  }
}
</script>

<template>
  <el-container style="height: 100%">
    <el-aside width="196px" class="sf-aside">
      <div class="sf-brand">
        <span class="sf-brand-mark">SF</span>
        <span class="sf-brand-text">SinyeFRP</span>
      </div>

      <el-menu :default-active="activePath" router class="sf-menu">
        <el-menu-item v-for="n in NAV" :key="n.name" :index="n.path">
          <el-icon><component :is="n.icon" /></el-icon>
          <span>{{ n.label }}</span>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header class="sf-header">
        <div class="sf-row" style="gap: 10px">
          <span style="font-weight: 600">{{ pageTitle }}</span>
        </div>

        <div class="sf-row" style="gap: 14px">
          <el-tooltip content="当前生效的 FRP 节点配置来源" placement="bottom">
            <span class="sf-muted" style="font-size: 12px">
              节点 {{ session.nodeName || '—' }} · {{ sourceLabel }}
            </span>
          </el-tooltip>
          <el-divider direction="vertical" />
          <span class="sf-muted" style="font-size: 12px">{{ session.username }}</span>
          <el-button link type="primary" :loading="loggingOut" @click="handleLogout">退出</el-button>
        </div>
      </el-header>

      <el-main class="sf-main">
        <router-view v-slot="{ Component }">
          <component :is="Component" />
        </router-view>
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.sf-aside {
  background: #fff;
  border-right: 1px solid var(--sf-line);
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.sf-brand {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 16px 18px 14px;
  border-bottom: 1px solid var(--sf-line);
}

.sf-brand-mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border-radius: 6px;
  background: var(--el-color-primary);
  color: #fff;
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.5px;
}

.sf-brand-text {
  font-size: 15px;
  font-weight: 600;
}

.sf-menu {
  border-right: none;
  flex: 1;
}

.sf-header {
  height: 52px;
  background: #fff;
  border-bottom: 1px solid var(--sf-line);
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.sf-main {
  padding: 18px;
  overflow: auto;
}
</style>
