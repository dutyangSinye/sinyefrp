<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import type {
  ClientDetailResponse,
  ClientInfo,
  ClientLink,
  ClientLinksResponse,
  ClientsResponse,
  Health,
  ProxyInfo,
} from '@/api/types'
import { describeHealth, formatBytes, formatUnixSec } from '@/utils/format'
import { PROXY_TYPE_LABEL } from '@/api/types'
import {
  ROBOT_FILTER_ALL,
  arrangeByRobotType,
  matchesRobotFilter,
  robotFilterOptions,
  robotLabel,
  robotSpanMethod,
} from '@/utils/robotGroup'

// 客户端列表（F05）与详情（F06）。
//
// 筛选在前端做：FRP 的 /api/clients 每次都返回全量，而单节点的客户端量级
// 在数十到数百之间，前端筛选比每次带参数回后端更快，也避免了「筛选状态
// 与服务端快照不一致」的窗口期。
//
// 列表里的「显示名称」来自本平台台账（01-prd F19），由后端按 clientID 关联；
// FRP 本身只有 clientID 这种设备标识，光看它认不出是哪台机器。

const data = ref<ClientsResponse | null>(null)
const loading = ref(false)
const loadErr = ref('')

const keyword = ref('')
const onlineFilter = ref<'all' | 'online' | 'offline'>('all')

/**
 * 机器人类型分组筛选（F20）。取值见 utils/robotGroup：'all' / 类型取值 / 'none'。
 *
 * 与「在线」筛选是两个正交维度 —— 「看哪些设备」与「看哪些类型的设备」，
 * 同时生效而不是互相覆盖。
 */
const robotFilter = ref(ROBOT_FILTER_ALL)

const health = computed<Health | null>(() => data.value?.health ?? null)
const healthDesc = computed(() => describeHealth(health.value))
const noSnapshot = computed(() => !health.value?.known)

async function load() {
  loading.value = true
  loadErr.value = ''
  try {
    const r = await api.clients()
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

const allClients = computed<ClientInfo[]>(() => data.value?.clients ?? [])

// 未登记的客户端数，用于在页头说明「有多少台不认识」。
//
// 只在 registered 明确为 false 时计入：接口异常时该字段会缺失（undefined），
// 那种情况下不能把全部客户端都算成未登记 —— 那会凭空造出一批告警。
const unregisteredCount = computed(
  () => allClients.value.filter((c) => c.registered === false).length,
)

const filtered = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return allClients.value.filter((c) => {
    if (onlineFilter.value === 'online' && !c.online) return false
    if (onlineFilter.value === 'offline' && c.online) return false
    if (!matchesRobotFilter(c.robot_type, robotFilter.value)) return false
    if (!kw) return true
    // display_name 必须参与搜索：有了可读名称之后，管理员自然是按名称找机器，
    // 而不是抄 clientID。
    return [c.clientID, c.display_name, c.hostname, c.clientIP, c.key, c.user]
      .filter(Boolean)
      .some((v) => String(v).toLowerCase().includes(kw))
  })
})

// ---------- 按机器人类型分组（F20）----------
//
// 分组在筛选**之后**做，所以合并块只包含当前可见的行：被筛掉的行不参与合并，
// 不会出现「格子跨了 3 行、其中只有 1 行有数据」的错位。

const arranged = computed(() => arrangeByRobotType(filtered.value, (c) => c.robot_type))

/**
 * 「机器人类型」列在表格里的下标（从 0 起）。
 *
 * el-table 的合并方法是按列下标分派的，所以这个数字与模板里的列顺序**必须**
 * 一致 —— 在它前面插一列就得同步改这里。写错的表现是整列内容消失（所有行都
 * 被判成「被合并掉」），不会报任何错，因此单独提出来命名而不是内联。
 */
const TYPE_COLUMN_INDEX = 1

const spanMethod = computed(() => robotSpanMethod(arranged.value.span, TYPE_COLUMN_INDEX))

/**
 * 行样式函数。
 *
 * 必须**提到外面命名**，不能写成 `:row-class-name="() => 'sf-clickable-row'"`
 * 内联箭头函数：内联写法每次渲染都是一个新的函数引用，el-table 会认为
 * row-class-name 变了并对每一行重算 class，而这个重算与同一次更新里的
 * `:data` 替换（筛选/分组后 arranged.flat 整体换新）之间存在时序竞争。
 * 表现是偶发的 `Cannot read properties of undefined (reading 'id')` ——
 * 走查里就是这么冒出来的，间歇、难复现。这里复用同一个函数引用即可消除。
 */
const rowClassName = () => 'sf-clickable-row'

/**
 * 筛选条选项。计数基于**全量** allClients，不是筛选后的 filtered：
 * 按 filtered 计数的话，筛了某个类型之后其它选项会全部变成 0，
 * 看起来就像那些类型下面没有设备。
 */
const robotOptions = computed(() => robotFilterOptions(allClients.value, (c) => c.robot_type))

// ---------- 详情抽屉 ----------
//
// 宽度是算出来的，不是拍出来的：抽屉里最宽的东西是「关联代理」表格，
// 七列的 min-width 相加是 738px，再加抽屉左右内边距约 40px —— 620px 时
// 内容区只有 580px，表格必然出现横向滚动条，而横向滚动条在一个 7 行的小表里
// 只会让人以为「右边还有内容没显示」，去拖它却什么也没多出来。
// 900px 让内容区有约 860px，表格与两列的 el-descriptions 都能铺满不折行。
//
// 上限不能用固定 px：远程桌面的窗口可能只有 1024 宽，那时 900px 的抽屉会
// 顶到屏幕边缘连关闭按钮都变形。故取 min(92vw, 900px) —— 屏宽够就用 900，
// 不够就退到屏宽的 92%（那种情况下横向滚动无法避免，是屏幕不够宽，不是布局问题）。
const DRAWER_SIZE = 'min(92vw, 900px)'

const drawerOpen = ref(false)
const detailLoading = ref(false)
const detailErr = ref('')
const detail = ref<ClientDetailResponse | null>(null)
const detailClient = ref<ClientInfo | null>(null)

async function openDetail(row: ClientInfo) {
  drawerOpen.value = true
  detailLoading.value = true
  detailErr.value = ''
  detail.value = null
  detailClient.value = row
  try {
    detail.value = await api.clientDetail(row.key)
  } catch (err) {
    detailErr.value = err instanceof HttpError ? err.message : '加载详情失败'
  } finally {
    detailLoading.value = false
  }
}

const detailProxies = computed<ProxyInfo[]>(() => detail.value?.proxies ?? [])
const shownClient = computed<ClientInfo | null>(() => detail.value?.client ?? detailClient.value)

function proxyTypeLabel(t: string): string {
  return PROXY_TYPE_LABEL[t] || t || '—'
}

// ---------- 穿透链接弹窗 ----------
//
// 现场要的是一句话：「RCS02 的访问地址为 xxx:12312」。此前只能自己去代理列表
// 找端口、再手敲接入点 IP 拼起来 —— 手拼错了没人会发现，要等对方说连不上。
//
// 地址由后端按代理类型算（/api/clients/{key}/links），前端不认得
// 「哪些类型能拼成 IP:端口」：那是 internal/pack 的类型清单，前端再抄一份
// 迟早漂移（同类教训见 tcpmux）。
const linksOpen = ref(false)
const linksLoading = ref(false)
const linksErr = ref('')
const linksData = ref<ClientLinksResponse | null>(null)
const linksClient = ref<ClientInfo | null>(null)

async function openLinks(row: ClientInfo) {
  linksOpen.value = true
  linksLoading.value = true
  linksErr.value = ''
  linksData.value = null
  linksClient.value = row
  try {
    linksData.value = await api.clientLinks(row.key)
  } catch (err) {
    linksErr.value = err instanceof HttpError ? err.message : '加载穿透链接失败'
  } finally {
    linksLoading.value = false
  }
}

/** 弹窗标题：有台账名称用名称，没有就退回 clientID（它至少是唯一标识）。 */
const linksTitle = computed(() => {
  const n = (linksData.value?.display_name ?? '').trim()
  if (n) return `${n} 的穿透链接`
  return `${linksClient.value?.clientID ?? '该设备'} 的穿透链接`
})

/** 真正给得出地址的那几条；给不出的按类型另有说明，不混在一起。 */
const copyableLinks = computed<ClientLink[]>(
  () => (linksData.value?.links ?? []).filter((l) => l.access_url),
)

/** 「复制全部」的文本，与界面上一行一条保持一致，可直接贴给现场。 */
const allLinksText = computed(() => {
  const name =
    (linksData.value?.display_name ?? '').trim() || linksClient.value?.clientID || ''
  // 每条都标上本机端口：只给「名字：地址」时，收到的人分不清这条对应
  // 目标机器上哪个服务。未登记就不标，不能标成 0。
  const lines = copyableLinks.value.map((l) => {
    const local = l.local_port_known ? `（本机 ${l.local_port}）` : ''
    return `${l.short_name}${local}：${l.access_url}`
  })
  if (!lines.length) return ''
  return [`${name} 的访问地址为：`, ...lines].join('\n')
})

async function copyText(text: string, what: string) {
  if (!text) {
    ElMessage.warning('当前没有可复制的内容')
    return
  }
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(what + '已复制')
  } catch {
    // 剪贴板只在安全上下文（HTTPS / localhost）可用。通过局域网 IP 打开管理台时
    // 它会失败 —— 这时必须告诉运维还有「手选」这条路，否则他只会以为按钮坏了。
    ElMessage.error('复制失败：浏览器拒绝了剪贴板访问，请手动选中地址复制')
  }
}

async function refreshNow() {
  loading.value = true
  try {
    const r = await api.refresh()
    await load()
    if (!r.refreshed) ElMessage.warning('采集未成功，展示的仍是上一次成功的数据')
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '刷新失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)

function proxyTypeText(p: ProxyInfo): string {
  const t = p.conf?.type || ''
  if (!t) return '—'
  return PROXY_TYPE_LABEL[t] || t
}
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">客户端</h2>
        <p class="sf-page-sub" style="margin: 0">
          数据来自 <code>/api/clients</code>；离线客户端仍会保留在列表中，便于查看最后一次已知信息。
        </p>
      </div>
      <div class="sf-row">
        <!--
          原因必须能看到：只写「采集异常」四个字，管理员除了知道坏了什么也做不了。
          detail 里有错误分类、原始报错、连续失败次数与最后成功时间。
        -->
        <el-tooltip :content="healthDesc.detail" placement="bottom">
          <el-tag :type="healthDesc.type" effect="light">{{ healthDesc.label }}</el-tag>
        </el-tooltip>
        <el-button :loading="loading" @click="refreshNow">
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
      description="当前无法判断任何客户端的在线状态。这不是「没有客户端」，请先到「设置」确认节点连通性。"
    />

    <!--
      采集失败但**曾经成功过**时，界面此前只有右上角一个红色标签、没有任何说明
      —— 列表里还铺着最后一次成功的数据，看上去一切正常，只有那个标签变色。
      这里必须把原因摊开，并明确「下面不是实时数据」：
      否则管理员会把一份过期快照当成当前状态去判断谁在线。
    -->
    <el-alert
      v-if="(healthDesc.level === 'error' || healthDesc.level === 'stale') && !loadErr"
      :type="healthDesc.type === 'danger' ? 'error' : healthDesc.type"
      show-icon
      :closable="false"
      :title="healthDesc.detail"
      description="下面列出的是最后一次成功采集的数据，不是当前实时状态，不能据此判断客户端现在是否在线。"
    />

    <el-card shadow="never">
      <div class="sf-stack" style="margin-bottom: 14px">
        <div class="sf-row-between">
          <div class="sf-row">
            <el-input
              v-model="keyword"
              placeholder="搜索名称 / clientID / 主机名 / IP"
              clearable
              style="width: 260px"
            >
              <template #prefix><el-icon><Search /></el-icon></template>
            </el-input>
            <el-radio-group v-model="onlineFilter">
              <el-radio-button value="all">全部</el-radio-button>
              <el-radio-button value="online">在线</el-radio-button>
              <el-radio-button value="offline">离线</el-radio-button>
            </el-radio-group>
          </div>
          <div class="sf-muted" style="font-size: 12px">
            共 {{ data?.stats.total ?? 0 }} 个 · 在线 {{ data?.stats.online ?? 0 }} · 离线
            {{ data?.stats.offline ?? 0 }}
            <template v-if="unregisteredCount > 0"> · 未登记 {{ unregisteredCount }} 个</template>
            <template v-if="filtered.length !== allClients.length"> · 当前显示 {{ filtered.length }} 个</template>
          </div>
        </div>

        <!--
          机器人类型分组筛选。计数是**全量**口径（不随当前筛选变化），
          否则筛了某一类之后其它选项会全变成 0。
        -->
        <div class="sf-row" style="flex-wrap: wrap; gap: 8px">
          <span class="sf-muted" style="font-size: 12px">机器人类型</span>
          <el-radio-group v-model="robotFilter" size="small">
            <el-radio-button v-for="o in robotOptions" :key="o.value" :value="o.value">
              {{ o.label }} {{ o.count }}
            </el-radio-button>
          </el-radio-group>
        </div>
      </div>

      <el-table
        v-loading="loading"
        :data="arranged.flat"
        size="small"
        border
        row-key="key"
        :span-method="spanMethod"
        :row-class-name="rowClassName"
        @row-click="openDetail"
      >
        <!--
          ⚠️ 本表所有列**只允许用 min-width，不允许出现 width**（已实测，别改回去）。

          原因在 Element Plus 的 colgroup 渲染器（h-helper.mjs）：
              const isAuto = props.tableLayout === 'auto'          // 默认就是 auto
              let columns = props.columns || []
              if (isAuto && columns.every(({ width }) => isUndefined(width))) columns = []
              const getPropsData = (column) => ({ key: `${tableLayout}_${column.id}`, ... })
              return h('colgroup', {}, columns.map((column) => h('col', getPropsData(column))))

          只要**有任意一列**写了 width，every() 就是 false，走不到 columns = []，
          于是 columns.map(...) 会对每一列读 column.id。而 EP 的 _columns 是
          一个被 insertColumn/removeColumn 用 splice **原地增删**的数组，
          .hidden-columns 上的 MutationObserver 触发 updateColumns() 时，
          数组可能正处在半重建状态而含一个 undefined 空洞 —— 那一刻
          column.id 就抛 "Cannot read properties of undefined (reading 'id')"。

          表现：**间歇性**控制台异常（Uncaught in promise），发生在 /clients，
          走查里报为「全程无控制台异常与告警」偶发失败；因为报错点在被压缩过的
          EP 内部、且 URL 落在文档本身（无有效堆栈），极难按堆栈定位。
          实测里它成对出现，相隔约 2ms 的另一条是 reading 'url'（同一份空洞
          数组流到别的列属性读取点，具体落点未逐一定位）。

          全部列都只写 min-width 时，every() 为 true ⇒ columns 被清空 ⇒
          map 空转 ⇒ 这条路径上永远不会读到 column.id。表格观感不变
          （min-width 本来就承担了伸缩布局，width 在这里只是把列钉死）。
        -->
        <el-table-column label="显示名称" min-width="150">
          <template #default="{ row }">
            <div v-if="row.display_name">{{ row.display_name }}</div>
            <!--
              名称留空与压根没登记要分开说：前者是台账里有这条记录、只是没填名称；
              后者说明这个客户端不是本平台签发的包，多半在别的机器上手工配了 frpc。
            -->
            <div v-else-if="row.registered" class="sf-cell-sub">已登记，未填名称</div>
            <el-tooltip
              v-else
              content="该客户端不在本平台台账中，通常是在别处手工配置的 frpc"
              placement="top"
            >
              <span class="sf-cell-sub">未登记</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <!--
          机器人类型（F20）：由后端按 clientID 关联台账下发 —— FRP 本身没有这个概念。

          同一类型的行在这一列上纵向合并成一个格子，读起来就是一组：组内首行
          显示类型名与台数，其余行被合并掉。合并信息来自 arranged.span（按分组
          算出的「组首行 → 组内行数」），不在这里现算 —— 现算就要重复一遍分组
          规则，两处迟早不一致。

          未分类组里既包含「台账里没填类型」，也包含「压根没登记」的客户端
          （未登记自然也没有类型）。要区分请看上一列的「未登记」标记。
        -->
        <el-table-column label="机器人类型" min-width="140">
          <template #default="{ row }">
            <div v-if="arranged.span.has(row)">
              <el-tag size="small" effect="plain" :type="row.robot_type ? 'primary' : 'info'">
                {{ robotLabel(row.robot_type) }}
              </el-tag>
              <div v-if="(arranged.span.get(row) ?? 1) > 1" class="sf-cell-sub">
                {{ arranged.span.get(row) }} 台
              </div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="客户端标识" min-width="170">
          <template #default="{ row }">
            <div class="sf-mono">{{ row.clientID || '—' }}</div>
            <div v-if="row.user" class="sf-cell-sub">user: {{ row.user }}</div>
          </template>
        </el-table-column>
        <el-table-column prop="hostname" label="主机名" min-width="150" show-overflow-tooltip>
          <template #default="{ row }">{{ row.hostname || '—' }}</template>
        </el-table-column>
        <el-table-column prop="clientIP" label="IP" min-width="140">
          <template #default="{ row }"><span class="sf-mono">{{ row.clientIP || '—' }}</span></template>
        </el-table-column>
        <el-table-column prop="version" label="版本" min-width="110">
          <template #default="{ row }"><span class="sf-mono">{{ row.version || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="状态" min-width="90">
          <template #default="{ row }">
            <el-tag :type="row.online ? 'success' : 'info'" size="small" effect="light" disable-transitions>
              {{ row.online ? '在线' : '离线' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="首次连接" min-width="170">
          <template #default="{ row }">{{ formatUnixSec(row.firstConnectedAt) }}</template>
        </el-table-column>
        <el-table-column label="最近连接" min-width="170">
          <template #default="{ row }">{{ formatUnixSec(row.lastConnectedAt) }}</template>
        </el-table-column>
        <el-table-column label="断线时间" min-width="170">
          <template #default="{ row }">
            <!-- disconnectedAt 仅离线时存在；在线时显示占位而不是 1970 -->
            <span v-if="!row.online && row.disconnectedAt">{{ formatUnixSec(row.disconnectedAt) }}</span>
            <span v-else-if="row.online" class="sf-cell-sub">在线中</span>
            <span v-else class="sf-cell-sub">—</span>
          </template>
        </el-table-column>
        <el-table-column label="" min-width="130" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click.stop="openDetail(row)">详情</el-button>
            <el-button link type="primary" size="small" @click.stop="openLinks(row)">链接</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">
            {{ noSnapshot ? '尚未取得节点数据' : allClients.length ? '没有符合条件的客户端' : '暂无客户端' }}
          </div>
        </template>
      </el-table>
    </el-card>

    <el-drawer v-model="drawerOpen" :size="DRAWER_SIZE" :title="'客户端详情'" destroy-on-close>
      <div v-loading="detailLoading">
        <el-alert v-if="detailErr" :title="detailErr" type="error" show-icon :closable="false" style="margin-bottom: 14px" />

        <template v-if="shownClient">
          <el-descriptions :column="2" border size="small">
            <el-descriptions-item label="显示名称" :span="2">
              <template v-if="shownClient.display_name">{{ shownClient.display_name }}</template>
              <template v-else-if="shownClient.registered">
                <span class="sf-muted">台账里有这台机器，但没有填写名称</span>
              </template>
              <template v-else>
                <span class="sf-muted">
                  未登记 —— 该客户端不在本平台台账中，通常是在别处手工配置的 frpc
                </span>
              </template>
            </el-descriptions-item>
            <el-descriptions-item label="机器人类型">
              <el-tag v-if="shownClient.robot_type" size="small" effect="plain">
                {{ robotLabel(shownClient.robot_type) }}
              </el-tag>
              <span v-else class="sf-muted">未分类</span>
            </el-descriptions-item>
            <el-descriptions-item label="客户端标识">
              <span class="sf-mono">{{ shownClient.clientID || '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="user">{{ shownClient.user || '未配置' }}</el-descriptions-item>
            <el-descriptions-item label="主机名">{{ shownClient.hostname || '—' }}</el-descriptions-item>
            <el-descriptions-item label="IP">
              <span class="sf-mono">{{ shownClient.clientIP || '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="FRP 版本">
              <span class="sf-mono">{{ shownClient.version || '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="通信协议版本">
              <span class="sf-mono">{{ shownClient.wireProtocol || '—' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="在线状态">
              <el-tag :type="shownClient.online ? 'success' : 'info'" size="small" effect="light">
                {{ shownClient.online ? '在线' : '离线' }}
              </el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="runID">
              <span class="sf-mono">{{ shownClient.runID || '—' }}</span>
              <div v-if="!shownClient.online" class="sf-cell-sub">离线后该值会被清空，不可作为持久引用</div>
            </el-descriptions-item>
            <el-descriptions-item label="首次连接">{{ formatUnixSec(shownClient.firstConnectedAt) }}</el-descriptions-item>
            <el-descriptions-item label="最近连接">{{ formatUnixSec(shownClient.lastConnectedAt) }}</el-descriptions-item>
            <el-descriptions-item label="断线时间" :span="2">
              <template v-if="!shownClient.online && shownClient.disconnectedAt">
                {{ formatUnixSec(shownClient.disconnectedAt) }}
              </template>
              <template v-else-if="shownClient.online">在线中</template>
              <template v-else>—</template>
            </el-descriptions-item>
            <el-descriptions-item label="详情键" :span="2">
              <span class="sf-mono">{{ shownClient.key }}</span>
            </el-descriptions-item>
          </el-descriptions>
        </template>

        <div style="margin-top: 18px">
          <div class="sf-row-between" style="margin-bottom: 8px">
            <span style="font-weight: 600">关联代理（{{ detailProxies.length }}）</span>
            <span class="sf-muted" style="font-size: 12px">按 clientID 关联</span>
          </div>

          <el-table v-if="detailProxies.length" :data="detailProxies" size="small" border>
            <el-table-column prop="name" label="代理名" min-width="150" show-overflow-tooltip>
              <template #default="{ row }"><span class="sf-mono">{{ row.name }}</span></template>
            </el-table-column>
            <el-table-column label="类型" min-width="90">
              <template #default="{ row }">{{ proxyTypeText(row) }}</template>
            </el-table-column>
            <el-table-column label="本地端口" min-width="92">
              <template #default="{ row }">
                <!-- FRP 不返回 localPort（03-frp-api-contract §4-⑦），
                     这是后端按代理名关联台账得到的；未登记就画「—」而不是 0。 -->
                <span class="sf-mono">{{ row.local_port_known ? row.local_port : '—' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="远端端口" min-width="92">
              <template #default="{ row }">
                <span class="sf-mono">{{ row.conf?.remotePort ?? '—' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="状态" min-width="84">
              <template #default="{ row }">
                <el-tag :type="row.status === 'online' ? 'success' : 'info'" size="small" effect="light" disable-transitions>
                  {{ row.status === 'online' ? '在线' : '离线' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="连接数" min-width="80">
              <template #default="{ row }">{{ row.curConns }}</template>
            </el-table-column>
            <el-table-column label="今日出/入" min-width="150">
              <template #default="{ row }">
                <div class="sf-cell-sub">
                  出 {{ formatBytes(row.todayTrafficOut) }} / 入 {{ formatBytes(row.todayTrafficIn) }}
                </div>
              </template>
            </el-table-column>
          </el-table>

          <el-empty v-else :description="detailLoading ? '加载中' : '该客户端暂无代理'" :image-size="72" />
        </div>
      </div>
    </el-drawer>

    <!-- 穿透链接：按设备给出「从外面怎么访问」，一条代理一行。 -->
    <el-dialog v-model="linksOpen" :title="linksTitle" width="640px" destroy-on-close>
      <div v-loading="linksLoading">
        <el-alert
          v-if="linksErr"
          :title="linksErr"
          type="error"
          show-icon
          :closable="false"
          style="margin-bottom: 14px"
        />

        <template v-if="linksData">
          <el-alert
            v-if="!linksData.endpoint_configured"
            type="warning"
            show-icon
            :closable="false"
            style="margin-bottom: 14px"
            title="尚未配置客户端接入点"
            description="穿透链接要用 frpc 实际连接的对外地址来拼，请先到「设置 → 客户端接入点」补全。"
          />

          <div class="sf-muted" style="font-size: 12px; margin-bottom: 8px">
            接入点 <code>{{ linksData.endpoint || '未配置' }}</code>
            <span v-if="copyableLinks.length">
              · 共 {{ copyableLinks.length }} 条可直接访问的地址
            </span>
          </div>

          <div v-if="!linksData.links.length" class="sf-muted" style="padding: 12px 0">
            这台设备当前没有代理。
          </div>

          <div
            v-for="l in linksData.links"
            :key="l.proxy_name"
            class="sf-row-between"
            style="padding: 9px 0; border-top: 1px solid var(--el-border-color-lighter)"
          >
            <div style="min-width: 0; flex: 1">
              <el-tag size="small" effect="plain">{{ proxyTypeLabel(l.proxy_type) }}</el-tag>
              <span style="margin-left: 6px">{{ l.short_name }}</span>
              <!-- 本地端口：只给「IP:端口」时，现场分不清这一条对应目标机器上
                   哪个服务，所以必须带上。它不在访问地址里，也不是从外面能连的
                   东西 —— 纯粹用来认人。FRP 不返回本地端口，来源是本平台台账，
                   未登记时显示「未登记」而不是 0 号端口。 -->
              <span v-if="l.local_port_known" class="sf-cell-sub" style="margin-left: 6px">
                · 本机 {{ l.local_port }}
              </span>
              <span v-else class="sf-cell-sub" style="margin-left: 6px">· 本机未登记</span>
              <el-tag
                v-if="!l.online"
                size="small"
                type="info"
                effect="light"
                style="margin-left: 6px"
              >
                离线
              </el-tag>
              <!-- user-select: all —— 剪贴板不可用时（用局域网 IP 打开管理台），
                   点一下就整段选中，手动复制仍然可行。 -->
              <div
                v-if="l.access_url"
                class="sf-mono"
                style="margin-top: 4px; user-select: all"
              >
                {{ l.access_url }}
              </div>
              <div v-else class="sf-cell-sub" style="margin-top: 4px">{{ l.reason }}</div>
            </div>
            <el-button
              v-if="l.access_url"
              link
              type="primary"
              size="small"
              @click="copyText(l.access_url, '地址')"
            >
              复制
            </el-button>
          </div>
        </template>
      </div>

      <template #footer>
        <el-button @click="linksOpen = false">关闭</el-button>
        <el-button
          type="primary"
          :disabled="!copyableLinks.length"
          @click="copyText(allLinksText, '全部地址')"
        >
          复制全部
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
:deep(.sf-clickable-row) {
  cursor: pointer;
}
</style>
