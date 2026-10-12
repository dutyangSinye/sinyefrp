<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import type { Health, OverviewResponse } from '@/api/types'
import { describeHealth, formatAgo, formatBytes, formatDateTime } from '@/utils/format'

// 总览（F04）。
//
// 关键口径（01-prd §8.4 / KR5）：/api/overview 在「从未采集成功」时返回 503，
// 但正文里带着 health，因此不能因为状态码不是 200 就把数据丢掉——
// 那样界面就只能显示一个空白错误，用户看不出「是没数据还是服务挂了」。
// 所以这里用 api.overview()（soft），把状态码与正文一并处理。

const data = ref<OverviewResponse | null>(null)
const httpStatus = ref(0)
const loadErr = ref('')
const loading = ref(false)
const refreshing = ref(false)

const health = computed<Health | null>(() => data.value?.health ?? null)
const server = computed(() => data.value?.server ?? null)
const desc = computed(() => describeHealth(health.value))

/** 从未采集成功：界面显示引导而不是一堆 0，避免把「未知」渲染成「零客户端」。 */
const noSnapshot = computed(() => !health.value?.known)

async function load() {
  loading.value = true
  loadErr.value = ''
  try {
    const r = await api.overview()
    httpStatus.value = r.status
    data.value = r.data
    if (!r.ok && r.status !== 503) {
      loadErr.value = r.error?.message || `请求失败（HTTP ${r.status}）`
      data.value = null
    }
  } catch (err) {
    loadErr.value = err instanceof HttpError ? err.message : '加载失败'
    data.value = null
  } finally {
    loading.value = false
  }
}

/** 手动触发一次立即采集，再重新拉取。 */
async function refreshNow() {
  refreshing.value = true
  try {
    const r = await api.refresh()
    await load()
    if (r.refreshed) ElMessage.success('已刷新节点数据')
    else ElMessage.warning('采集未成功，界面展示的仍是上一次成功的数据')
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '刷新失败')
  } finally {
    refreshing.value = false
  }
}

onMounted(load)

const summaryCards = computed(() => {
  const s = server.value
  const c = data.value?.clients
  return [
    { label: 'FRP 版本', value: s?.version ?? '—' },
    {
      label: '客户端',
      value: noSnapshot.value ? '—' : `${c?.total ?? 0}`,
      sub: noSnapshot.value ? '' : `在线 ${c?.online ?? 0} / 离线 ${c?.offline ?? 0}`,
    },
    { label: '代理数', value: noSnapshot.value ? '—' : `${data.value?.proxy_count ?? 0}` },
    { label: '当前连接数', value: s ? `${s.curConns}` : '—' },
    { label: '累计入流量', value: s ? formatBytes(s.totalTrafficIn) : '—', sub: 'frps 启动后累计' },
    { label: '累计出流量', value: s ? formatBytes(s.totalTrafficOut) : '—', sub: 'frps 启动后累计' },
    { label: '最后成功采集', value: health.value?.known ? formatAgo(health.value.last_success) : '从未成功' },
    { label: '本次数据时间', value: formatDateTime(data.value?.fetched_at) },
  ]
})

const serverDetails = computed(() => {
  const s = server.value
  if (!s) return []
  return [
    { label: 'bind 端口', value: String(s.bindPort) },
    { label: 'vhost HTTP 端口', value: s.vhostHTTPPort ? String(s.vhostHTTPPort) : '未开启' },
    { label: 'vhost HTTPS 端口', value: s.vhostHTTPSPort ? String(s.vhostHTTPSPort) : '未开启' },
    { label: 'TCPMUX 端口', value: s.tcpmuxHTTPConnectPort ? String(s.tcpmuxHTTPConnectPort) : '未开启' },
    { label: 'KCP 端口', value: s.kcpBindPort ? String(s.kcpBindPort) : '未开启' },
    { label: 'QUIC 端口', value: s.quicBindPort ? String(s.quicBindPort) : '未开启' },
    { label: '子域后缀', value: s.subdomainHost || '未配置' },
    { label: '心跳超时', value: s.heartbeatTimeout < 0 ? 'FRP 默认' : `${s.heartbeatTimeout} 秒` },
    { label: '连接池上限', value: s.maxPoolCount > 0 ? String(s.maxPoolCount) : '不限制' },
    { label: '单客户端端口上限', value: s.maxPortsPerClient > 0 ? String(s.maxPortsPerClient) : '不限制' },
    { label: '在线客户端数（FRP 口径）', value: String(s.clientCounts) },
  ]
})

const proxyTypeRows = computed(() => {
  const m = server.value?.proxyTypeCount || {}
  return Object.entries(m).sort((a, b) => b[1] - a[1])
})
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">节点总览</h2>
        <p class="sf-page-sub" style="margin: 0">
          数据来自 frps 内置 Dashboard API，管理平台只读采集，不修改 FRP 配置。
        </p>
      </div>
      <div class="sf-row">
        <el-tag :type="desc.type" effect="light">{{ desc.label }}</el-tag>
        <el-button :loading="refreshing" @click="refreshNow">
          <el-icon><Refresh /></el-icon>
          <span style="margin-left: 4px">立即采集</span>
        </el-button>
      </div>
    </div>

    <el-alert v-if="loadErr" :title="loadErr" type="error" show-icon :closable="false" />

    <!-- 从未采集成功时的引导：明确告诉用户「这不是零客户端，而是没有数据」 -->
    <el-alert
      v-if="noSnapshot && !loadErr"
      type="info"
      show-icon
      :closable="false"
      title="尚未取得节点数据，因此无法判断客户端在线状态"
    >
      <template #default>
        <div style="font-size: 13px">
          {{ desc.detail }}
          <template v-if="health?.error_kind">
            （错误分类：<code>{{ health.error_kind }}</code>）
          </template>
          <div style="margin-top: 6px">
            请到「设置」检查 FRP Dashboard 地址、用户名与密码，并用「测试连通性」确认可达。
          </div>
        </div>
      </template>
    </el-alert>

    <el-card v-loading="loading" shadow="never">
      <div class="sf-kv">
        <div v-for="c in summaryCards" :key="c.label">
          <span>{{ c.label }}</span>
          <b>{{ c.value }}</b>
          <div v-if="c.sub" class="sf-cell-sub">{{ c.sub }}</div>
        </div>
      </div>
    </el-card>

    <el-card v-if="serverDetails.length" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">服务器信息</span>
          <span class="sf-muted" style="font-size: 12px">来自 /api/serverinfo</span>
        </div>
      </template>
      <div class="sf-kv">
        <div v-for="d in serverDetails" :key="d.label">
          <span>{{ d.label }}</span>
          <b class="sf-mono" style="font-size: 14px">{{ d.value }}</b>
        </div>
      </div>
    </el-card>

    <el-card v-if="proxyTypeRows.length" shadow="never">
      <template #header>
        <span style="font-weight: 600">各类型代理数量</span>
      </template>
      <el-table :data="proxyTypeRows.map(([type, count]) => ({ type, count }))" size="small" border>
        <el-table-column prop="type" label="类型" min-width="140" />
        <el-table-column prop="count" label="数量" />
      </el-table>
      <div class="sf-cell-sub" style="margin-top: 8px">
        注：该映射的键集合会随 FRP 版本变化，值为 0 的类型也可能出现，不能据此枚举支持的代理类型。
      </div>
    </el-card>
  </div>
</template>
