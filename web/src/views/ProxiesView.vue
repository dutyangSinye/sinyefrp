<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import type { ClientsResponse, Health, ProxyInfo, ProxiesResponse } from '@/api/types'
import { PROXY_TYPE_LABEL } from '@/api/types'
import { describeHealth, formatBytes, formatProxyTime } from '@/utils/format'

// 代理列表（F07）。
//
// 类型与所属客户端支持服务端筛选（/api/proxies 的 type / client_id 查询参数），
// 在线的筛选只能在前端做 —— 后端没有这个参数，且代理的在线判定是字符串 status、
// 与客户端的布尔 online 不是同一套语义（见 03-frp-api-contract §4），
// 强行在服务端抽象会掩盖这个差异。

const data = ref<ProxiesResponse | null>(null)
const clients = ref<ClientsResponse | null>(null)
const loading = ref(false)
const loadErr = ref('')

const filterType = ref('')
const filterClient = ref('')
const filterStatus = ref<'all' | 'online' | 'offline'>('all')

const proxyTypes = ref<string[]>(['tcp', 'udp', 'http', 'https', 'stcp', 'xtcp'])

const health = computed<Health | null>(() => data.value?.health ?? null)
const healthDesc = computed(() => describeHealth(health.value))
const noSnapshot = computed(() => !health.value?.known)

async function loadProxies() {
  loading.value = true
  loadErr.value = ''
  try {
    const r = await api.proxies({ type: filterType.value, clientId: filterClient.value })
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

async function loadSideData() {
  // 代理类型的权威清单来自 /api/pkg/meta（Go 端硬编码的白名单）。
  // 不能用 /api/serverinfo 的 proxyTypeCount 的键来枚举：它的键集合随版本漂移。
  try {
    const meta = await api.pkgMeta()
    if (meta.proxy_types?.length) proxyTypes.value = meta.proxy_types
  } catch {
    /* 拿不到就退回默认清单 */
  }
  try {
    // api.clients() 走 soft，返回 RawResult；这里只要正文，503 时 data 里仍带 health。
    clients.value = (await api.clients()).data
  } catch {
    /* 客户端下拉是辅助信息，失败不阻塞代理列表 */
  }
}

onMounted(async () => {
  await Promise.all([loadProxies(), loadSideData()])
})

watch([filterType, filterClient], loadProxies)

const rows = computed<ProxyInfo[]>(() => {
  const list = data.value?.proxies ?? []
  if (filterStatus.value === 'all') return list
  return list.filter((p) => (p.status === 'online') === (filterStatus.value === 'online'))
})

const clientOptions = computed(() =>
  (clients.value?.clients ?? []).map((c) => ({
    value: c.clientID,
    // 有台账名称时用「名称（clientID）」：名称好认，clientID 保留在后面以便核对，
    // 两者也都能被 el-select 的 filterable 搜到。
    label: c.display_name
      ? `${c.display_name}（${c.clientID}）`
      : c.clientID + (c.hostname ? `（${c.hostname}）` : ''),
  })),
)

function typeText(p: ProxyInfo): string {
  const t = p.conf?.type || ''
  // conf 为 null（代理离线）时类型不可知，显示「不适用」而不是报错（F07 验收）。
  if (!t) return '不适用'
  return PROXY_TYPE_LABEL[t] || t
}

/**
 * 端点：本地端口 → 远端端口。
 *
 * 远端端口来自 FRP 的 `conf.remotePort`；**本地端口 FRP 不返回**
 * （conf 只有 localIP 与 remotePort，见 03-frp-api-contract §4-⑦），
 * 只能由后端按代理名关联本平台台账得到。所以未登记时显示「未登记」
 * 而不是画成 0 —— 那会被读成「本地端口是 0 号」。
 */
function endpoint(p: ProxyInfo): string {
  if (!p.conf) return '不适用'
  const remote = p.conf.remotePort ? String(p.conf.remotePort) : '未分配'
  const local = p.local_port_known ? String(p.local_port) : '未登记'
  return `本地 ${local} → 远端 ${remote}`
}

/**
 * 目标地址（本地 IP）。
 *
 * 单独一行显示：它 FRP 一定给得出，与本地端口是否登记无关，
 * 两者混在一行会让人以为「有 IP 就等于本地端口也拿到了」。
 */
function localAddr(p: ProxyInfo): string {
  if (!p.conf) return '不适用'
  return p.conf.localIP || '—'
}
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">代理</h2>
        <p class="sf-page-sub" style="margin: 0">
          代理名采用 FRP 返回的完整名（<code>user.proxyname</code>）。代理离线时其配置不可用，相关字段显示为「不适用」。
          本地端口来自本平台台账的关联（FRP Dashboard 不返回该字段），不是本平台签发的包会显示「未登记」。
        </p>
      </div>
      <div class="sf-row">
        <!-- 与客户端页同一条约定：标签只给结论，原因放在 tooltip 与下面的提示条里。 -->
        <el-tooltip :content="healthDesc.detail" placement="bottom">
          <el-tag :type="healthDesc.type" effect="light">{{ healthDesc.label }}</el-tag>
        </el-tooltip>
        <el-button :loading="loading" @click="loadProxies">
          <el-icon><Refresh /></el-icon>
          <span style="margin-left: 4px">刷新</span>
        </el-button>
      </div>
    </div>

    <el-alert v-if="loadErr" :title="loadErr" type="error" show-icon :closable="false" />
    <el-alert
      v-if="noSnapshot && !loadErr"
      type="info"
      show-icon
      :closable="false"
      title="尚未取得节点数据"
      description="当前无法判断任何代理的状态。"
    />

    <el-alert
      v-if="(healthDesc.level === 'error' || healthDesc.level === 'stale') && !loadErr"
      :type="healthDesc.type === 'danger' ? 'error' : healthDesc.type"
      show-icon
      :closable="false"
      :title="healthDesc.detail"
      description="下面列出的是最后一次成功采集的数据，不是当前实时状态。"
    />

    <el-card shadow="never">
      <div class="sf-row-between" style="margin-bottom: 14px">
        <div class="sf-row" style="flex-wrap: wrap">
          <el-select v-model="filterType" placeholder="全部类型" clearable style="width: 160px">
            <el-option v-for="t in proxyTypes" :key="t" :value="t" :label="PROXY_TYPE_LABEL[t] || t" />
          </el-select>
          <el-select v-model="filterClient" placeholder="全部客户端" clearable filterable style="width: 220px">
            <el-option v-for="c in clientOptions" :key="c.value" :value="c.value" :label="c.label" />
          </el-select>
          <el-radio-group v-model="filterStatus">
            <el-radio-button value="all">全部</el-radio-button>
            <el-radio-button value="online">在线</el-radio-button>
            <el-radio-button value="offline">离线</el-radio-button>
          </el-radio-group>
        </div>
        <div class="sf-muted" style="font-size: 12px">共 {{ rows.length }} 条</div>
      </div>

      <el-table v-loading="loading" :data="rows" size="small" border>
        <el-table-column prop="name" label="代理名" min-width="180" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.name }}</span></template>
        </el-table-column>
        <el-table-column label="类型" min-width="110">
          <template #default="{ row }">{{ typeText(row) }}</template>
        </el-table-column>
        <el-table-column prop="clientID" label="所属客户端" min-width="180">
          <template #default="{ row }">
            <!-- 台账名称来自 01-prd F19；FRP 只给 clientID，后端按 clientID 关联。 -->
            <template v-if="row.display_name">
              <div>{{ row.display_name }}</div>
              <div class="sf-mono sf-cell-sub" style="word-break: break-all">
                {{ row.clientID || '—' }}
              </div>
            </template>
            <span v-else class="sf-mono" style="word-break: break-all">{{ row.clientID || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="本地 → 远端端口" min-width="210">
          <template #default="{ row }">
            <div class="sf-mono">{{ endpoint(row) }}</div>
            <div class="sf-cell-sub sf-mono">{{ localAddr(row) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="状态" min-width="84">
          <template #default="{ row }">
            <el-tag
              :type="row.status === 'online' ? 'success' : 'info'"
              size="small"
              effect="light"
              disable-transitions
            >
              {{ row.status === 'online' ? '在线' : '离线' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="连接数" min-width="80">
          <template #default="{ row }">{{ row.curConns }}</template>
        </el-table-column>
        <el-table-column label="今日流量 出/入" min-width="170">
          <template #default="{ row }">
            <span class="sf-cell-sub">
              {{ formatBytes(row.todayTrafficOut) }} / {{ formatBytes(row.todayTrafficIn) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="最近启动" min-width="130">
          <template #default="{ row }"><span class="sf-mono">{{ formatProxyTime(row.lastStartTime) }}</span></template>
        </el-table-column>
        <el-table-column label="最近关闭" min-width="130">
          <template #default="{ row }"><span class="sf-mono">{{ formatProxyTime(row.lastCloseTime) }}</span></template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">
            {{ noSnapshot ? '尚未取得节点数据' : '没有符合条件的代理' }}
          </div>
        </template>
      </el-table>

      <div class="sf-cell-sub" style="margin-top: 10px">
        流量字段是「今日」口径，跨日或 frps 重启后归零；连接数为当前瞬时值。
      </div>
    </el-card>
  </div>
</template>
