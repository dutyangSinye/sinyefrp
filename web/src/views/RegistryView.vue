<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import {
  DEFAULT_PLATFORM,
  PLATFORM_LABEL,
  REGISTRY_STATUS_LABEL,
  ROBOT_TYPE_LABEL,
  ROBOT_TYPES,
  type Health,
  type PortOwner,
  type RegistryClient,
} from '@/api/types'
import { describeHealth, formatDateTime } from '@/utils/format'
import {
  ROBOT_FILTER_ALL,
  arrangeByRobotType,
  matchesRobotFilter,
  robotFilterOptions,
  robotLabel,
  robotSpanMethod,
} from '@/utils/robotGroup'

// 客户端台账与端口占用（F19）。
//
// 这一页回答的是两个不同的问题，所以分成两块，刻意不合并：
//   - 台账：谁被签发过、什么状态、什么时候生成过几次 —— 这是**签发记录**；
//   - 端口：哪个远端端口归谁 —— 又分「台账」与「FRP 在线」两个口径。
//
// 台账不是运行时真相：管理员之后可能在目标机器上手工改过 frpc.toml，
// 那时台账与实际部署会分叉。两个端口口径正是为暴露这种分叉而并列的：
// 只在台账里 = 包已签发但还没装；只在 FRP 里 = 机器在跑但不在本平台管理范围内。

const clients = ref<RegistryClient[]>([])
const registryPorts = ref<PortOwner[]>([])
const livePorts = ref<PortOwner[]>([])
const health = ref<Health | null>(null)
const loading = ref(false)
const errMsg = ref('')

/** 采集是否成功过。为 false 时「FRP 中可见」一列必须显示「—」而不是「未见」。 */
const healthKnown = computed(() => health.value?.known === true)
const healthDesc = computed(() => describeHealth(health.value))

/** 台账里处于启用状态、真正占着端口的记录数。 */
const activeCount = computed(() => clients.value.filter((c) => c.status === 'active').length)

/** 机器人类型分组筛选（F20）。取值见 utils/robotGroup：'all' / 类型取值 / 'none'。 */
const robotFilter = ref(ROBOT_FILTER_ALL)

/** 命中类型筛选的台账记录。 */
const visibleClients = computed(() =>
  clients.value.filter((c) => matchesRobotFilter(c.robot_type, robotFilter.value)),
)

// 分组视图：同类型的行排在一起，类型列纵向合并成一个格子（F20）。
// 分组在筛选之后做，合并块只包含当前可见的行。
const arrangedClients = computed(() =>
  arrangeByRobotType(visibleClients.value, (c) => c.robot_type),
)

/**
 * 「机器人类型」列在台账表里的下标（从 0 起），必须与模板中的列顺序一致。
 * 写错的表现是整列内容消失（所有行都被判成「被合并掉」），不报任何错。
 */
const TYPE_COLUMN_INDEX = 1

const spanMethod = computed(() =>
  robotSpanMethod(arrangedClients.value.span, TYPE_COLUMN_INDEX),
)

/**
 * 筛选条选项。计数基于**全量**台账而不是筛选后的集合 ——
 * 否则筛了某类之后其余选项全变成 0，看着像那些类型下面没有设备。
 */
const robotOptions = computed(() => robotFilterOptions(clients.value, (c) => c.robot_type))

async function load() {
  loading.value = true
  errMsg.value = ''
  try {
    const [c, p] = await Promise.all([api.registryClients(), api.registryPorts()])
    clients.value = c.clients ?? []
    registryPorts.value = p.registry ?? []
    livePorts.value = p.live ?? []
    health.value = p.health ?? c.health ?? null
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '加载台账失败'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  loadEndpointDefaults()
})

// ---------- 展示辅助 ----------

/**
 * 台账里的本地端口文案。
 *
 * 有值写 `本地 8080`；为 0 时写「本地未填」——0 是非法端口
 * （pack 的校验要求 1-65535），画成「本地 0」只会让人以为程序算错了。
 */
function localPortText(v: number): string {
  return v > 0 ? `本地 ${v}` : '本地未填'
}

/** 取该客户端占用的远端端口（remote_port > 0 的才是真正占端口的）。 */
function occupiedPorts(c: RegistryClient) {
  return (c.ports ?? []).filter((p) => p.remote_port > 0)
}

/** 不占全局端口的代理条数（HTTP/HTTPS/STCP/XTCP/TCPMUX）。 */
function portlessCount(c: RegistryClient) {
  return (c.ports ?? []).length - occupiedPorts(c).length
}

function statusLabel(s: string): string {
  // 开放式字符串：后端新增取值时这里回退到原值，绝不凭猜测比对。
  return REGISTRY_STATUS_LABEL[s] || s || '—'
}

function statusTagType(s: string): 'success' | 'info' {
  return s === 'active' ? 'success' : 'info'
}

/**
 * 在线状态四态：未知 / 在线 / 离线 / 从未上线。
 *
 * 后两态的区别是这个列存在的理由 —— 它们都是「此刻不通」，但处置方式完全不同：
 * 「离线」是连过后来断了，该查网络、frps 或目标机器上的 frpc；
 * 「从未上线」是包压根没装上，该去现场装。合并成一个布尔值就会把前者显示成后者。
 */
function onlineText(c: RegistryClient): string {
  if (!healthKnown.value) return '—'
  if (c.online) return '在线'
  return c.seen_in_frp ? '离线' : '从未上线'
}

function onlineTagType(c: RegistryClient): 'info' | 'success' | 'warning' {
  if (!healthKnown.value) return 'info'
  if (c.online) return 'success'
  // 「离线」用橙色：本来是通的现在断了，是异常。
  // 「从未上线」用灰色：批量出包后还没装是常态，不该报警 —— 它由文字承担区分。
  return c.seen_in_frp ? 'warning' : 'info'
}

function onlineTip(c: RegistryClient): string {
  if (!healthKnown.value) return '尚未成功采集 FRP 状态，无法判断这个客户端有没有装上'
  if (c.online) return '此刻在 FRP 里在线：这台设备已经装上并且连着'
  return c.seen_in_frp
    ? 'FRP 里见过这个 clientID，但此刻不在线：包装上并连过，后来断了 —— 该查网络、frps 或目标机器上的 frpc'
    : 'FRP 里从未见过这个 clientID：包可能已签发但还没安装，也可能装上后从未连上'
}

// ---------- 编辑 ----------

const router = useRouter()

const editVisible = ref(false)
const editing = ref<RegistryClient | null>(null)

// 平台的选项与中文标签来自 types.ts 这一份权威（键要与 Go 端 AllPlatforms 逐字一致）。
// 「未记录」刻意**不在选项里**：它不是一种平台，而是历史数据的状态。
// 抽屉里选了具体平台后就再也回不到空串，除非后端那条记录从未签发过。
const platformOptions = computed(() => Object.keys(PLATFORM_LABEL))
function platformLabel(p: string): string {
  return PLATFORM_LABEL[p] || p
}
/** 列表里显示用：空串（未记录）按默认平台展示，但会带上一个提示 tag。 */
function platformText(p: string | undefined): string {
  const v = p ?? ''
  if (v === '') return PLATFORM_LABEL[DEFAULT_PLATFORM] ?? DEFAULT_PLATFORM
  return PLATFORM_LABEL[v] || v
}
const editForm = ref({
  display_name: '',
  robot_type: '',
  platform: '',
  note: '',
  status: 'active',
})
const saving = ref(false)

function openEdit(c: RegistryClient) {
  editing.value = c
  editForm.value = {
    display_name: c.display_name,
    // 旧记录（v3 迁移之前签发的包）这一列是空的，下拉会落到「未分类」。
    robot_type: c.robot_type ?? '',
    // platform 同理：空串是「未记录」（比 v3 更晚 —— 从记录平台这一版起签发的
    // 包才带这一列）。必须把台账当前值带出来：漏了会让这次保存把它清成「未记录」，
    // 而下一次改包就得重新确认平台。
    platform: c.platform ?? '',
    note: c.note,
    status: c.status,
  }
  editVisible.value = true
}

// 在这台设备原来的基础上重新出包：带着台账记录跳到打包页。
//
// 传的是台账 id 而不是把整份 spec 塞进路由：spec 里有 token，落在地址栏或
// 浏览器历史里等于把它写给所有能看到屏幕的人。打包页自己按 id 去后端取。
//
// 也不走 localStorage —— 那会让「刷新页面」与「新开标签」行为不一致，
// 而这条路上的东西（要发到现场的部署包）值得多花一次请求换可靠性。
function repack(c: RegistryClient) {
  router.push({ path: '/packages', query: { from: String(c.id) } })
}

async function submitEdit() {
  const c = editing.value
  if (!c) return
  if (!editForm.value.display_name.trim()) {
    ElMessage.warning('显示名称不能为空')
    return
  }
  saving.value = true
  try {
    await api.updateRegistryClient(c.id, {
      display_name: editForm.value.display_name.trim(),
      // 空串是合法值（清空为未分类），因此这里不做「空则不提交」的省略，
      // 否则管理员改错了类型就再也清不掉。
      robot_type: editForm.value.robot_type,
      // platform 与 robot_type 一样始终显式提交。它决定下一次改包产出什么形态的包，
      // 被顺手清空时症状要等到下次改包才出现，而且那时候看不出为什么。
      platform: editForm.value.platform,
      note: editForm.value.note.trim(),
      status: editForm.value.status,
    })
    ElMessage.success('已保存')
    editVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}

// ---------- 状态与删除 ----------

async function toggleStatus(c: RegistryClient) {
  const retiring = c.status === 'active'
  const next = retiring ? 'retired' : 'active'
  try {
    await ElMessageBox.confirm(
      retiring
        ? `停用「${c.display_name}」后，它占用的远端端口会被视为已释放，可以被其他客户端使用。` +
            '如果这台设备还在跑，请不要停用它。'
        : `启用「${c.display_name}」后，它占用的远端端口重新受保护，其他客户端不能再占用。`,
      retiring ? '确认停用' : '确认启用',
      { confirmButtonText: '确认', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return // 用户取消
  }
  try {
    await api.updateRegistryClient(c.id, {
      display_name: c.display_name,
      // 停用/启用只改状态，其余字段必须原样回传：
      // 这个接口是「整体替换」语义，漏传就等于把它清空。
      robot_type: c.robot_type ?? '',
      // platform 尤其不能漏：漏了会把「目标平台」清成未记录，
      // 而它只在下一次改包时才被用到 —— 于是停用一次之后，
      // 再给这台 Linux 盒子改包就会默认产出装不上的 Windows 包，
      // 起因却在很久以前的一次「停用」。
      platform: c.platform ?? '',
      note: c.note,
      status: next,
    })
    ElMessage.success(retiring ? '已停用，端口已释放' : '已启用')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '操作失败')
  }
}

async function removeClient(c: RegistryClient) {
  const ports = occupiedPorts(c)
  const portNote = ports.length
    ? `它占用的 ${ports.map((p) => p.remote_port).join('、')} 号端口会被释放，其他客户端随后可以占用。`
    : '它没有占用任何远端端口。'
  try {
    await ElMessageBox.confirm(
      `将从台账中删除「${c.display_name}」(${c.client_id})。${portNote}` +
        '删除只影响台账，不会动已经在运行的客户端。' +
        // 这三项丢失后无法从 FRP 找回，必须明确讲清楚：
        // 显示名称、机器人类型只存在于台账；本地端口 FRP 根本不返回
        // （03-frp-api-contract §4-⑦ 实测），台账是它的唯一副本。
        // 现场曾因误删两条记录，导致端口只能靠人工回忆复填。
        '注意：该客户端的名称、机器人类型与本地端口只在台账里保留，' +
        '删除后无法从 FRP 恢复，需要重新登记填写。',
      '确认删除台账记录',
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await api.deleteRegistryClient(c.id)
    ElMessage.success('已删除，端口已释放')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '删除失败')
  }
}

async function releasePort(p: PortOwner) {
  try {
    await ElMessageBox.confirm(
      `只释放端口记录：${p.port}（${p.proxy_name}）不再算作被「${p.client_name || p.client_id}」占用，` +
        '客户端记录本身保留。如果这个端口其实还在用，请改成停用整个客户端。' +
        '注意：这条记录里的本地端口也会一并删除，之后无法从 FRP 恢复。',
      '确认释放这条端口',
      { confirmButtonText: '释放', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await api.releaseRegistryPort(p.id)
    ElMessage.success('端口已释放')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '释放失败')
  }
}

// ---------- 手工补录台账 ----------
//
// 为什么需要这个入口：设备已经在跑，但台账记录丢了（数据库异常、误删）。
// 旧版本想让一台设备重新出现在台账里，唯一办法是「再生成一个客户端包」——
// 而管理员根本不想再出一个包、更不想重新部署。
// 结果就是这台设备永久不在台账里：界面显示不出名称，端口也不受冲突保护。
//
// 注意补录出来的记录 generate_count = 0，界面对此显式标注「手工补录」，
// 避免把它误当成「已签发过、只是还没装」。

const createVisible = ref(false)
const creating = ref(false)
const createErr = ref('')
const createConflicts = ref<string[]>([])

// 补录表单里的接入点默认值，取自「设置 → 客户端接入点」。
// 不硬编码 7000：生产环境的对外端口与 bindPort 常常不同（NAT），
// 填错会让补录的记录与实际部署对不上。
const epAddr = ref('')
const epPort = ref(7000)

async function loadEndpointDefaults() {
  try {
    const r = await api.pkgEndpoint()
    epAddr.value = r.client_addr || ''
    epPort.value = r.client_port || 7000
  } catch {
    // 接入点没配也不该拦住补录：管理员可以手填
    epAddr.value = ''
    epPort.value = 7000
  }
}

const createForm = ref({
  client_id: '',
  display_name: '',
  robot_type: '',
  // 补录的设备就在现场跑着，它是什么系统管理员看得见，
  // 默认给 Windows 而不是留空：留空会让这条记录永远处在「未记录」状态，
  // 之后每次改包都要重新确认一次。
  platform: DEFAULT_PLATFORM,
  note: '',
  server_addr: '',
  server_port: 7000,
  // 端口行用一个可增删的列表。默认给一行，因为最常见的补录就是一台机器一条代理。
  ports: [{ proxy_name: '', proxy_type: 'tcp', remote_port: 0, local_port: 0 }] as {
    proxy_name: string
    proxy_type: string
    remote_port: number
    local_port: number
  }[],
})

/**
 * 代理类型取值**必须与 pack 的枚举一致（小写）**。
 *
 * 这里吃过大亏：写成 'TCP' 时后端 pt.Valid() 为假，那条代理既不进端口冲突
 * 判定、也不报错，等于端口被静默放行 —— 实测两条记录都登记 12311 却都创建成功。
 * 后端现已对大小写做归一化，前端仍按契约送小写，不给两边留解释空间。
 */
const PROXY_TYPES = ['tcp', 'udp', 'http', 'https', 'stcp', 'xtcp']

function proxyNeedsRemotePort(t: string) {
  return t === 'tcp' || t === 'udp'
}

function openCreate() {
  creating.value = false
  createErr.value = ''
  createConflicts.value = []
  createForm.value = {
    client_id: '',
    display_name: '',
    robot_type: '',
    // 重置也要带上：少了它，这次重置后 platform 会变成 undefined，
    // 提交时 JSON 里没有这个字段，后端按「未记录」处理 ——
    // 一条补录记录就这样失去了平台信息，而界面上看不出任何异常。
    platform: DEFAULT_PLATFORM,
    note: '',
    // 默认带上已配置的接入点，管理员通常不用改
    server_addr: epAddr.value,
    server_port: epPort.value,
    ports: [{ proxy_name: '', proxy_type: 'tcp', remote_port: 0, local_port: 0 }],
  }
  createVisible.value = true
}

function addPortRow() {
  createForm.value.ports.push({ proxy_name: '', proxy_type: 'tcp', remote_port: 0, local_port: 0 })
}

function removePortRow(idx: number) {
  createForm.value.ports.splice(idx, 1)
}

async function submitCreate() {
  const f = createForm.value
  creating.value = true
  createErr.value = ''
  createConflicts.value = []
  try {
    // 直接构造字面量：不要先声明成 Record<string, unknown>，
    // 那会丢掉字段形状，接口参数就不再受类型检查保护。
    await api.createRegistryClient({
      client_id: f.client_id.trim(),
      display_name: f.display_name.trim(),
      robot_type: f.robot_type,
      platform: f.platform,
      note: f.note.trim(),
      server_addr: f.server_addr.trim(),
      server_port: f.server_port,
      // 只提交填了代理名的行：空行是界面上的占位，不该变成一条脏记录
      ports: f.ports
        .filter((p) => p.proxy_name.trim() !== '')
        .map((p) => ({
          proxy_name: p.proxy_name.trim(),
          proxy_type: p.proxy_type,
          // 不需要远端端口的类型，远端端口强制归零：
          // 否则会平白占住一个端口，挡住别人。
          remote_port: proxyNeedsRemotePort(p.proxy_type) ? p.remote_port : 0,
          local_port: p.local_port,
        })),
    })
    ElMessage.success('台账记录已补录')
    createVisible.value = false
    await load()
  } catch (err) {
    if (err instanceof HttpError) {
      createErr.value = err.message
      // 端口冲突时后端会带 details，逐条列出来比一句「端口冲突」有用得多
      createConflicts.value = err.details ?? []
    } else {
      createErr.value = '补录失败'
    }
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">客户端台账</h2>
        <p class="sf-page-sub" style="margin: 0">
          这里是本平台<strong>签发记录</strong>：记录已经生成过哪些客户端、分别占用了哪些远端端口。
          远端端口在同一个 frps 上是全局资源，两个客户端占用同一个端口时，后注册的代理会被 FRP 拒绝，
          所以在生成客户端包时会按这份台账把冲突拦下来。
        </p>
      </div>
      <div class="sf-row" style="gap: 8px">
        <el-button type="primary" @click="openCreate">
          <el-icon><Plus /></el-icon>
          <span style="margin-left: 4px">手工补录台账</span>
        </el-button>
        <el-button :loading="loading" @click="load">
          <el-icon><Refresh /></el-icon>
          <span style="margin-left: 4px">刷新</span>
        </el-button>
      </div>
    </div>

    <el-alert v-if="errMsg" :title="errMsg" type="error" show-icon :closable="false" />

    <el-alert
      :title="healthDesc.detail"
      :type="healthDesc.type"
      show-icon
      :closable="false"
    />

    <el-alert type="info" show-icon :closable="false">
      <template #title>
        台账是签发记录，不等于机器上的实际情况：管理员之后可能在目标机器上手工改过
        <code>frpc.toml</code>。「FRP 在线端口占用」一块是 FRP 此刻真实的代理列表，
        两者对不上正是需要留意的地方。
      </template>
    </el-alert>

    <!-- 台账 -->
    <el-card shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">
            已签发的客户端（{{ clients.length }}，其中启用 {{ activeCount }}）
          </span>
          <span class="sf-muted" style="font-size: 12px">
            只有「启用」的客户端占用端口；停用表示设备已下线，端口视为已释放
          </span>
        </div>
      </template>

      <!--
        机器人类型分组筛选（F20）。计数是全量口径，不随当前筛选变化。
        「停用」的记录也按类型归组 —— 类型是设备属性，与启停状态无关。
      -->
      <div class="sf-row" style="flex-wrap: wrap; gap: 8px; margin-bottom: 12px">
        <span class="sf-muted" style="font-size: 12px">机器人类型</span>
        <el-radio-group v-model="robotFilter" size="small">
          <el-radio-button v-for="o in robotOptions" :key="o.value" :value="o.value">
            {{ o.label }} {{ o.count }}
          </el-radio-button>
        </el-radio-group>
        <span
          v-if="visibleClients.length !== clients.length"
          class="sf-muted"
          style="font-size: 12px"
        >
          当前显示 {{ visibleClients.length }} 个
        </span>
      </div>

      <el-table
        v-loading="loading"
        :data="arrangedClients.flat"
        size="small"
        border
        :span-method="spanMethod"
      >
        <el-table-column label="显示名称" min-width="150">
          <template #default="{ row }">
            <div>{{ row.display_name || '—' }}</div>
            <div v-if="row.note" class="sf-cell-sub">{{ row.note }}</div>
          </template>
        </el-table-column>
        <!--
          机器人类型：纯业务分类，不参与端口判定，因此放在名称旁边而不是端口附近。

          同一类型的行在这一列上纵向合并：组内首行显示类型名与台数，其余行被
          合并掉 —— 这就是「按机器人类型分组」的可视形态。未分类的记录不画成
          一个空格（空白会被读成「加载失败」或「没填」），而是归入「未分类」组。

          合并信息取自 arrangedClients.span，不在这里现算 —— 现算就要把分组规则
          再写一遍，与客户端页那份迟早不一致。
        -->
        <el-table-column label="机器人类型" min-width="140">
          <template #default="{ row }">
            <div v-if="arrangedClients.span.has(row)">
              <el-tag size="small" effect="plain" :type="row.robot_type ? 'primary' : 'info'">
                {{ robotLabel(row.robot_type) }}
              </el-tag>
              <div v-if="(arrangedClients.span.get(row) ?? 1) > 1" class="sf-cell-sub">
                {{ arrangedClients.span.get(row) }} 台
              </div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="目标平台" min-width="120">
          <template #default="{ row }">
            <span>{{ platformText(row.platform) }}</span>
            <!-- 「未记录」不是一个平台，而是「这条记录的包早于平台记录功能」。
                 不标出来的话，界面上它和一个明确的 Windows 长得一模一样，
                 直到改完包发现装不上才会被察觉。 -->
            <el-tag v-if="!row.platform" size="small" type="warning" effect="plain">
              未记录
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="client_id" label="clientID" min-width="160" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="sf-mono">{{ row.client_id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" min-width="80">
          <template #default="{ row }">
            <el-tag size="small" effect="light" :type="statusTagType(row.status)">
              {{ statusLabel(row.status) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="占用的远端端口" min-width="180">
          <template #default="{ row }">
            <div class="sf-row" style="flex-wrap: wrap; gap: 4px">
              <el-tag
                v-for="p in occupiedPorts(row)"
                :key="p.id"
                size="small"
                effect="plain"
                :type="row.status === 'active' ? 'primary' : 'info'"
              >
                <span class="sf-mono">{{ p.remote_port }}</span>/{{ p.proxy_type }}
                <span style="margin-left: 4px; opacity: 0.75">{{ localPortText(p.local_port) }}</span>
              </el-tag>
              <span v-if="!occupiedPorts(row).length" class="sf-cell-sub">不占用</span>
            </div>
            <div v-if="portlessCount(row) > 0" class="sf-cell-sub">
              另有 {{ portlessCount(row) }} 条不占端口的代理
            </div>
          </template>
        </el-table-column>
        <el-table-column label="在线状态" min-width="110" align="center">
          <template #default="{ row }">
            <el-tooltip :content="onlineTip(row)" placement="top">
              <el-tag size="small" effect="plain" :type="onlineTagType(row)">
                {{ onlineText(row) }}
              </el-tag>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="生成次数" min-width="90" align="center">
          <template #default="{ row }"><span class="sf-mono">{{ row.generate_count }}</span></template>
        </el-table-column>
        <el-table-column label="最近生成" min-width="170">
          <template #default="{ row }">{{ formatDateTime(row.last_generated_at) }}</template>
        </el-table-column>
        <!-- 「生成包」放在第一个：它是这一页最高频的动作之一（改配置后重新出包），
             也是「意图最重」的一个 —— 会产生一个新的可分发产物。 -->
        <el-table-column label="操作" min-width="240" align="center">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="repack(row)">生成包</el-button>
            <el-button link type="primary" size="small" @click="openEdit(row)">编辑</el-button>
            <el-button link type="primary" size="small" @click="toggleStatus(row)">
              {{ row.status === 'active' ? '停用' : '启用' }}
            </el-button>
            <el-button link type="danger" size="small" @click="removeClient(row)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">
            还没有签发的客户端。到「部署包 → 客户端」生成一个，生成后会自动登记到这里。
          </div>
        </template>
      </el-table>
    </el-card>

    <!-- 台账端口 -->
    <el-card shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">台账端口占用（{{ registryPorts.length }}）</span>
          <span class="sf-muted" style="font-size: 12px">
            这些远端端口已被保留，生成新客户端时不能再用；本地端口只作比对，不参与保留判定
          </span>
        </div>
      </template>

      <el-table v-loading="loading" :data="registryPorts" size="small" border>
        <el-table-column label="远端端口" min-width="110">
          <template #default="{ row }">
            <span class="sf-mono" style="font-weight: 600">{{ row.port }}</span>
          </template>
        </el-table-column>
        <el-table-column label="本地端口" min-width="100">
          <template #default="{ row }">
            <!-- 台账登记值。FRP 不返回 localPort，实时口径也是靠这条台账关联出来的。
                 它不参与冲突判定：本地端口属于目标机器自己的地址空间，可重复。 -->
            <span class="sf-mono">{{ row.local_port_known ? row.local_port : '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="client_name" label="客户端" min-width="140" show-overflow-tooltip>
          <template #default="{ row }">{{ row.client_name || '—' }}</template>
        </el-table-column>
        <el-table-column prop="client_id" label="clientID" min-width="160" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.client_id }}</span></template>
        </el-table-column>
        <el-table-column prop="proxy_name" label="代理名" min-width="130" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.proxy_name || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="类型" min-width="90">
          <template #default="{ row }"><span class="sf-mono">{{ row.proxy_type || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="操作" min-width="90" align="center">
          <template #default="{ row }">
            <el-button link type="danger" size="small" @click="releasePort(row)">释放</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">台账里没有任何端口占用</div>
        </template>
      </el-table>
    </el-card>

    <!-- FRP 实时端口 -->
    <el-card shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">FRP 在线端口占用（{{ livePorts.length }}）</span>
          <span class="sf-muted" style="font-size: 12px">
            只列出当前在线、且能取到远端端口的代理；不在本平台台账里出现的，多半是在别处手工配置的
          </span>
        </div>
      </template>

      <el-table v-loading="loading" :data="livePorts" size="small" border>
        <el-table-column label="远端端口" min-width="110">
          <template #default="{ row }">
            <span class="sf-mono" style="font-weight: 600">{{ row.port }}</span>
          </template>
        </el-table-column>
        <el-table-column label="本地端口" min-width="100">
          <template #default="{ row }">
            <!-- 在线代理也可能没有本地端口：它是别处手工配的 frpc、包不是本平台
                 签发的，就永远关联不上台账，只能显示「—」而不是 0。 -->
            <span class="sf-mono">{{ row.local_port_known ? row.local_port : '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="client_name" label="客户端" min-width="140" show-overflow-tooltip>
          <template #default="{ row }">{{ row.client_name || '—' }}</template>
        </el-table-column>
        <el-table-column prop="client_id" label="clientID" min-width="160" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.client_id || '—' }}</span></template>
        </el-table-column>
        <el-table-column prop="proxy_name" label="代理名" min-width="130" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.proxy_name || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="类型" min-width="90">
          <template #default="{ row }"><span class="sf-mono">{{ row.proxy_type || '—' }}</span></template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">
            {{ healthKnown ? '当前没有在线代理占用远端端口' : '尚未成功采集 FRP 状态，无法列出' }}
          </div>
        </template>
      </el-table>
    </el-card>

    <!-- 编辑台账 -->
    <el-dialog v-model="editVisible" title="编辑台账记录" width="520px">
      <el-alert
        v-if="editing"
        type="info"
        show-icon
        :closable="false"
        style="margin-bottom: 12px"
        :title="`clientID 不可修改：${editing.client_id}`"
      />
      <el-form label-position="top">
        <el-form-item label="显示名称">
          <el-input v-model="editForm.display_name" maxlength="100" placeholder="例如 财务部-前台机" />
        </el-form-item>
        <el-form-item label="机器人类型">
          <el-select
            v-model="editForm.robot_type"
            placeholder="未分类"
            clearable
            style="width: 100%"
          >
            <el-option
              v-for="t in ROBOT_TYPES"
              :key="t"
              :value="t"
              :label="ROBOT_TYPE_LABEL[t] || t"
            />
          </el-select>
          <div class="sf-cell-sub" style="margin-top: 4px">
            仅用于标识设备，不影响连接与端口。清空即「未分类」。
          </div>
        </el-form-item>
        <el-form-item label="目标平台">
          <el-select v-model="editForm.platform" style="width: 100%">
            <el-option v-for="p in platformOptions" :key="p" :value="p" :label="platformLabel(p)" />
          </el-select>
          <div class="sf-cell-sub" style="margin-top: 4px">
            决定在这台设备上再生包时产出哪种形态：Windows 与 Linux 的二进制、
            安装脚本、文本行尾都不一样。选错时安装脚本不会报错，要等上线才发现。
          </div>
          <el-alert
            v-if="!editForm.platform"
            type="warning"
            show-icon
            :closable="false"
            style="margin-top: 8px"
            title="这条记录还没有登记目标平台"
            description="它是本平台开始记录平台之前签发的。请确认这台设备实际运行的系统后选一项 —— 现在选好，以后改包就不用每次再问。"
          />
        </el-form-item>
        <el-form-item label="状态">
          <el-radio-group v-model="editForm.status">
            <el-radio-button value="active">启用（端口受保护）</el-radio-button>
            <el-radio-button value="retired">停用（端口已释放）</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="备注">
          <el-input
            v-model="editForm.note"
            type="textarea"
            :rows="3"
            maxlength="500"
            show-word-limit
            placeholder="例如 财务部前台机，2026-10 报废"
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="submitEdit">保存</el-button>
      </template>
    </el-dialog>

    <!--
      手工补录台账（F19 补充）。
      destroy-on-close：每次打开都是全新表单，避免上一次输错的 clientID 残留下来
      —— 补录最怕的就是抄错 ID，残留会让管理员以为填过了。
    -->
    <el-dialog v-model="createVisible" title="手工补录台账记录" width="720px" destroy-on-close>
      <el-alert type="info" show-icon :closable="false" style="margin-bottom: 14px">
        <template #title>
          用于设备已经在运行、但台账记录丢失（或从未签发）的情况。
          补录只写台账，不会生成任何部署包、也不会影响正在运行的客户端。
        </template>
      </el-alert>

      <el-alert v-if="createErr" :title="createErr" type="error" show-icon :closable="false" style="margin-bottom: 12px">
        <template v-if="createConflicts.length" #default>
          <ul style="margin: 6px 0 0; padding-left: 18px">
            <li v-for="(m, i) in createConflicts" :key="i">{{ m }}</li>
          </ul>
        </template>
      </el-alert>

      <el-form label-position="top">
        <div class="sf-form-grid">
          <el-form-item label="clientID（必填）">
            <el-input v-model="createForm.client_id" placeholder="例如 sf-df88cm8j2bab" spellcheck="false" />
            <div class="sf-cell-sub" style="margin-top: 4px">
              在 FRP Dashboard 的客户端列表里可以看到。必须与现场完全一致，抄错会与真实设备对不上。
            </div>
          </el-form-item>

          <el-form-item label="显示名称">
            <el-input v-model="createForm.display_name" placeholder="例如 运输机器人RCS01" />
            <div class="sf-cell-sub" style="margin-top: 4px">仅用于界面识别，FRP 不返回这个名称。</div>
          </el-form-item>

          <el-form-item label="机器人类型">
            <el-select v-model="createForm.robot_type" placeholder="未分类" style="width: 100%">
              <el-option label="未分类" value="" />
              <el-option v-for="t in ROBOT_TYPES" :key="t" :label="ROBOT_TYPE_LABEL[t]" :value="t" />
            </el-select>
          </el-form-item>

          <el-form-item label="接入点地址">
            <el-input v-model="createForm.server_addr" placeholder="例如 116.169.217.89" spellcheck="false" />
            <div class="sf-cell-sub" style="margin-top: 4px">默认取自设置页的客户端接入点，按需修改。</div>
          </el-form-item>

          <el-form-item label="目标平台">
            <el-select v-model="createForm.platform" style="width: 100%">
              <el-option v-for="p in platformOptions" :key="p" :value="p" :label="platformLabel(p)" />
            </el-select>
            <div class="sf-cell-sub" style="margin-top: 4px">
              补录的设备就在现场，它跑什么系统是可以确认的事实 —— 填对了，以后在这条记录上
              重新出包就直接在正确的形态上起步。
            </div>
          </el-form-item>

          <el-form-item label="接入点端口">
            <el-input-number
              v-model="createForm.server_port"
              :min="0"
              :max="65535"
              controls-position="right"
              style="width: 100%"
            />
          </el-form-item>
        </div>

        <el-form-item label="备注">
          <el-input
            v-model="createForm.note"
            type="textarea"
            :rows="2"
            maxlength="500"
            show-word-limit
            placeholder="例如 现场已部署，台账丢失后补录"
          />
        </el-form-item>

        <el-divider content-position="left">端口占用</el-divider>
        <div class="sf-cell-sub" style="margin-bottom: 8px">
          远端端口是 frps 上的全局资源，冲突会被直接拒绝。<strong>本地端口</strong>是目标机器上的服务端口，
          FRP 不会返回，只有这里登记之后界面才能显示 —— 建议一并填上。
        </div>

        <el-table :data="createForm.ports" size="small" border>
          <el-table-column label="代理名称" min-width="130">
            <template #default="{ row }">
              <el-input v-model="row.proxy_name" size="small" placeholder="例如 rcs01" spellcheck="false" />
            </template>
          </el-table-column>
          <el-table-column label="类型" min-width="110">
            <template #default="{ row }">
              <el-select v-model="row.proxy_type" size="small" style="width: 100%">
                <el-option v-for="t in PROXY_TYPES" :key="t" :label="t.toUpperCase()" :value="t" />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column label="远端端口" min-width="130">
            <template #default="{ row }">
              <el-input-number
                v-model="row.remote_port"
                size="small"
                :min="0"
                :max="65535"
                :disabled="!proxyNeedsRemotePort(row.proxy_type)"
                controls-position="right"
                style="width: 100%"
              />
            </template>
          </el-table-column>
          <el-table-column label="本地端口" min-width="130">
            <template #default="{ row }">
              <el-input-number
                v-model="row.local_port"
                size="small"
                :min="0"
                :max="65535"
                controls-position="right"
                style="width: 100%"
              />
            </template>
          </el-table-column>
          <el-table-column label="" min-width="70">
            <template #default="{ $index }">
              <el-button
                link
                type="danger"
                :disabled="createForm.ports.length <= 1"
                @click="removePortRow($index)"
              >
                删除
              </el-button>
            </template>
          </el-table-column>
        </el-table>

        <el-button link type="primary" style="margin-top: 8px" @click="addPortRow">
          <el-icon><Plus /></el-icon>
          <span style="margin-left: 4px">再加一条代理</span>
        </el-button>
      </el-form>

      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="creating" @click="submitCreate">补录</el-button>
      </template>
    </el-dialog>
  </div>
</template>
