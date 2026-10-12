<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError, saveBlob } from '@/api/http'
import type {
  ClientSpec,
  EndpointConfig,
  PkgMeta,
  PkgPreview,
  PortOwner,
  ProxySpec,
  RegistrySpecResponse,
} from '@/api/types'
import {
  DEFAULT_PLATFORM,
  LOG_LEVELS,
  PLATFORM_LABEL,
  PROXY_TYPE_LABEL,
  PROXY_TYPES_WITH_HOST,
  PROXY_TYPES_WITH_REMOTE_PORT,
  PROXY_TYPES_WITH_SECRET_KEY,
  ROBOT_TYPE_LABEL,
  ROBOT_TYPES,
} from '@/api/types'
import { formatBytes } from '@/utils/format'
import SecretField from './SecretField.vue'

// 客户端配置表单与部署包生成（F09 / F10 / F15 / F17）。
//
// 本组件由「部署包」页以**弹窗**形式挂载（每次打开都是全新的客户端，
// 见 PackagesView 的 destroy-on-close），因此不需要自己管开关状态；
// 生成成功后通过 generated 事件通知外层关闭弹窗。

// draft 非空时表示「在某个已有客户端的基础上改包」：它带着后端从台账
// 重建出来的 spec，以及哪些字段其实是这台替补的默认值。
//
// 为什么不在这个组件里按 id 自己去拉：路的另一头是「弹窗打开前草稿必须已经在」，
// 否则表单会先闪一版空白默认值再被草稿覆盖 —— 管理员看到的就是一次抖动，
// 而他要改的正是这些值，抖动会让人怀疑自己的修改没生效。由父组件先取好再传进来。
const props = defineProps<{ meta: PkgMeta | null; draft?: RegistrySpecResponse | null }>()

const emit = defineEmits<{ generated: [fileName: string] }>()

const formRef = ref<FormInstance>()
const spec = ref<ClientSpec | null>(null)
const loading = ref(false)
const previewing = ref(false)
const downloading = ref(false)
const preview = ref<PkgPreview | null>(null)
const errMsg = ref('')
/** 失败时的逐条细节（当前只有一类：逐条端口冲突）。 */
const errDetails = ref<string[]>([])

function setError(err: unknown, fallback: string) {
  if (err instanceof HttpError) {
    errMsg.value = err.message
    errDetails.value = err.details
  } else {
    errMsg.value = fallback
    errDetails.value = []
  }
}

function clearError() {
  errMsg.value = ''
  errDetails.value = []
}

function blankProxy(): ProxySpec {
  return {
    name: '',
    type: 'tcp',
    local_ip: '127.0.0.1',
    local_port: 0,
    remote_port: 0,
    custom_domains: [],
    subdomain: '',
    locations: [],
    secret_key: '',
    use_encryption: false,
    use_compression: false,
  }
}

/**
 * 兜住「数组字段返回 null」这一类破坏性响应（第二道防线，见 ServerPackageForm 同名函数）。
 * proxies 一旦是 null，添加/删除按钮的回调会直接抛，模板里的 .length 也会让整页崩掉。
 */
function normalize(s: ClientSpec): ClientSpec {
  return {
    ...s,
    // robot_type 也一并兜住：Go 端解码 null 到 string 会静默留空，
    // 但历史库/别的调用方真发出 null 时，el-select 绑 null 会报
    // 「Cannot read properties of null」，与数组字段是同一类风险。
    robot_type: s.robot_type ?? '',
    // platform 同样兜住：后端会为空串补默认值，但历史调用方或代理层
    // 真发出 null 时，el-select 绑 null 会报「Cannot read properties of null」，
    // 与数组字段是同一类风险。且它决定整包的形态，不能留成一个悬空值。
    platform: s.platform || DEFAULT_PLATFORM,
    proxies: (s.proxies ?? []).map((p) => ({
      ...p,
      custom_domains: p.custom_domains ?? [],
      locations: p.locations ?? [],
    })),
  }
}

async function loadDefaults() {
  // 已有草稿时不拉默认值：那会把 clientID 冲掉，而 clientID 正是
  // 「改的是同一台设备的同一份东西」这件事的全部依据。
  if (props.draft) return
  loading.value = true
  clearError()
  try {
    spec.value = normalize(await api.clientDefaults())
    applyPlatformFallback()
    preview.value = null
  } catch (err) {
    setError(err, '加载默认值失败')
  } finally {
    loading.value = false
  }
}

// ---------- 在已有部署包的基础上改（「改包」） ----------
//
// 部署包 ZIP 从不落盘、服务端也不保留当时的密钥，所以「把旧包解出来改」
// 物理上做不到。能重建的只有**当初的输入**，而它唯一的去处是台账。
// 于是这张表单里的东西分成两类，界面必须分清楚，否则管理员会以为
// 每一项都是原样还原的：
//   - 台账里存的（clientID、名称、类型、平台、接入点、各代理的端口）—— 真实还原；
//   - 台账里根本没有的（token、日志级别、HTTP 域名、STCP 密钥等）—— 按默认值补齐。
//
// 后一类的危险在于它看起来和前者完全一样。一行默认值填好了两个数字的差别，
// 只有在这个提示里才看得出来。

/** 改包提示文案：逐条代理列出缺什么，而不是笼统说一句「部分字段用了默认值」。 */
const restoreNotes = ref<{ title: string; items: string[] } | null>(null)

const FIELD_LABEL: Record<string, string> = {
  token: '连接 token（服务端不留存旧包的密钥，取自当前接入点）',
  log_level: '日志级别',
  log_max_days: '日志保留天数',
  enable_tls: 'TLS 开关',
  local_ip: '本地 IP（按 127.0.0.1 补）',
  custom_domains: '绑定域名',
  subdomain: '子域名',
  locations: 'URL 路由前缀',
  secret_key: '访问密钥',
}

function describeDefaultField(f: string): string {
  return FIELD_LABEL[f] ?? f
}

function buildRestoreNotes(d: RegistrySpecResponse) {
  const items: string[] = []
  if (d.defaults_applied.length) {
    items.push('以下项按默认值补齐：' + d.defaults_applied.map(describeDefaultField).join('、'))
  }
  // token 单独强调：它是唯一一处「填了但可能与原包不同」的东西 ——
  // 值来自当前接入点，接入点若被改过，新包与旧包的 token 就不一致，
  // 而这台设备上如果还装着旧包，旧包会连不上。必须说清。
  for (const p of d.proxy_defaults) {
    const shown = p.fields.map(describeDefaultField)
    if (shown.length) {
      items.push(`代理「${p.proxy_name}」的 ${shown.join('、')} 未能还原`)
    }
  }
  restoreNotes.value = items.length ? { title: '这些不是原包的样子', items } : null
}

// draft 由父组件异步取得后传入，到达时间晚于挂载，所以要 watch 而不是只在
// onMounted 里读一次；它也可能在一次会话里被换掉（连点两台设备的「生成包」）。
watch(
  () => props.draft,
  (d) => {
    clearError()
    restoreNotes.value = null
    if (!d) return
    spec.value = normalize(d.spec)
    applyPlatformFallback()
    buildRestoreNotes(d)
    preview.value = null
  },
  { immediate: true },
)

// ---------- 接入点来源提示 ----------
//
// 地址、端口与 token 的默认值由服务端从设置页的「客户端接入点」带出。
// 这里只读取接入点的配置状态用于提示，不再单独拉一次默认值——
// 值本身已经在 clientDefaults 的响应里了，重复请求只会带来不一致的风险。

const epState = ref<EndpointConfig | null>(null)

async function loadEndpointState() {
  try {
    epState.value = await api.pkgEndpoint()
  } catch {
    // 接入点状态纯属提示信息，读不到不该挡住打包主流程。
    epState.value = null
  }
}

const endpointWarn = computed(() => !!epState.value && !epState.value.configured)

const endpointHint = computed(() => {
  const ep = epState.value
  if (!ep) return ''
  if (!ep.configured) {
    return '尚未配置客户端接入点：地址与端口需要手工填写。建议先到「设置」页配置接入点，之后生成的包会自动带上地址、端口与 token。'
  }
  const tokenPart = ep.token_configured ? '、token' : ''
  return `地址、端口${tokenPart}已从「设置」页的客户端接入点自动填入（${ep.client_addr}:${ep.client_port}），可直接使用。`
})

// ---------- 目标平台 ----------
//
// 可选平台由 meta 下发而不是前端写死：能不能出 Linux 包，取决于平台 exe 里
// 内嵌了哪些二进制，那只有后端知道。前端写死一份清单，就会在换了一份构建
// （或降级部署）之后，出现一个选了必然失败的选项。

const platformOptions = computed<string[]>(() => props.meta?.client_platforms ?? [])

/**
 * 把 spec.platform 收敛到当前可用集合内。
 *
 * 换一份平台构建后，上次选的 linux-arm64 可能这次没有对应二进制。
 * 不收敛就会出现「界面显示一个值、提交后后端报不受支持」，
 * 而管理员完全不知道自己选错了什么——这种错误发生在提交时，离现场很远。
 */
function applyPlatformFallback() {
  const s = spec.value
  if (!s) return
  const list = platformOptions.value
  if (list.length === 0) return
  if (!list.includes(s.platform)) {
    s.platform = list.includes(DEFAULT_PLATFORM) ? DEFAULT_PLATFORM : list[0]
  }
}

// meta 由父组件异步加载后传入，因此集合可能晚于默认值到达。
watch(platformOptions, applyPlatformFallback)

const platformHint = computed(() => {
  if (platformOptions.value.length === 0) {
    return '未加载 FRP 二进制，无法选择目标平台。'
  }
  if (spec.value?.platform && !spec.value.platform.startsWith('windows')) {
    return 'Linux 包内是 .sh 脚本与 systemd unit，需以 root 执行 sudo ./install.sh；目标机器需带 systemd。'
  }
  return '决定包内二进制与脚本类型：Windows 包是 .ps1 与计划任务，Linux 包是 .sh 与 systemd 服务。'
})

// ---------- 端口占用（01-prd F19） ----------
//
// 远端端口在同一个 frps 上是全局资源，两个客户端占同一个端口时后注册的代理
// 会被 FRP 拒绝 —— 而失败发生在目标机器上，管理员在管理台里看不到征兆。
//
// 这里在**填表阶段**就把占用标出来，而不是等到点「生成」才报错。后端在生成时
// 仍会做同一套判定并硬拦（409），前端这一层只负责让冲突在管理员眼前显形。
//
// 两个口径合并成一张端口表用于判定，但保留 owner.source：台账里的占用可以
// 到「客户端台账」停用/删除来释放，FRP 上的在线占用只能改端口。

const portOwners = ref<PortOwner[]>([])
/** 端口占用是否读取失败。读不到时不做提示，但生成仍由服务端把关。 */
const portsUnavailable = ref(false)

async function loadPorts() {
  try {
    const r = await api.registryPorts()
    // 台账在前：同一个端口两边都出现时（客户端已装好并在线），
    // 台账那条是可操作的那条，优先展示它。
    portOwners.value = [...(r.registry ?? []), ...(r.live ?? [])]
    portsUnavailable.value = false
  } catch {
    portOwners.value = []
    portsUnavailable.value = true
  }
}

/** 端口 → 占用者。同一端口多条时取第一条（台账优先）。 */
const portOwnerMap = computed(() => {
  const m = new Map<number, PortOwner>()
  for (const o of portOwners.value) {
    if (o.port > 0 && !m.has(o.port)) m.set(o.port, o)
  }
  return m
})

/**
 * 判断某条代理的远端端口是否被**别人**占用。
 *
 * 属于同一个 client_id 的占用不算冲突：那正是「改一条代理后重新出包」，
 * 是必须允许的路径。后端用的是同一条判据。
 */
function portConflict(p: ProxySpec): PortOwner | null {
  const s = spec.value
  if (!s || !needsRemotePort(p.type) || !p.remote_port) return null
  const o = portOwnerMap.value.get(p.remote_port)
  if (!o) return null
  if (s.client_id && o.client_id === s.client_id) return null
  return o
}

interface ConflictRow {
  name: string
  port: number
  owner: PortOwner
}

const conflicts = computed<ConflictRow[]>(() => {
  const s = spec.value
  if (!s) return []
  const out: ConflictRow[] = []
  for (const p of s.proxies) {
    const o = portConflict(p)
    if (o) out.push({ name: p.name || '（未命名）', port: p.remote_port, owner: o })
  }
  return out
})

/** 预览返回的阻断项 + 本地发现的冲突，共同决定下载按钮是否可用。 */
const blockingPreview = computed(() => preview.value?.blocking ?? [])
const cannotDownload = computed(() => conflicts.value.length > 0 || blockingPreview.value.length > 0)

function ownerLabel(o: PortOwner): string {
  return o.client_name || o.client_id || '未知客户端'
}

function ownerHint(o: PortOwner): string {
  return o.source === 'registry'
    ? '台账记录（可能是包已签发但还没装）；可到「客户端台账」把它停用或删除以释放端口'
    : 'FRP 上当前在线的代理，不在本平台台账里；只能改用其他端口'
}

// 模板里用它们取文案，避免在模板中写 `portConflict(row)!` 这类非空断言。
function conflictLabel(p: ProxySpec): string {
  const o = portConflict(p)
  return o ? `已被「${ownerLabel(o)}」占用` : ''
}

function conflictTip(p: ProxySpec): string {
  const o = portConflict(p)
  return o ? ownerHint(o) : ''
}

/** 找一个当前没被占用的远端端口，用于「改用 N」。 */
function nextFreePort(): number {
  const used = new Set<number>()
  for (const o of portOwners.value) {
    if (o.port > 0) used.add(o.port)
  }
  // 本次表单里其他代理已填的端口也要避开，否则只是把冲突换到了表单内部。
  for (const p of spec.value?.proxies ?? []) {
    if (needsRemotePort(p.type) && p.remote_port > 0) used.add(p.remote_port)
  }
  for (let p = 6000; p <= 65535; p++) {
    if (!used.has(p)) return p
  }
  return 0
}

/**
 * 重新生成 clientID。
 *
 * 不能只是把它清空：服务端确实会在 client_id 为空时自动生成，但提交前界面上
 * 是一个空输入框，管理员无法确认到底生成了什么，也无法先复制走。
 * 因此这里向服务端要一份新的默认值，只取 client_id，其余编辑内容原样保留。
 */
async function regenerateClientID() {
  const s = spec.value
  if (!s) return
  try {
    const fresh = await api.clientDefaults()
    s.client_id = fresh.client_id
    preview.value = null
    ElMessage.info('已生成新的 clientID')
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '生成 clientID 失败')
  }
}

onMounted(() => {
  loadDefaults()
  loadEndpointState()
  loadPorts()
})

const rules: FormRules = {
  server_addr: [{ required: true, message: '请填写 FRP 服务器地址', trigger: 'blur' }],
  client_id: [{ required: true, message: '客户端标识不能为空', trigger: 'blur' }],
  token: [{ required: true, message: '认证 token 不能为空', trigger: 'blur' }],
  name: [{ required: true, message: '请填写显示名称，便于在管理台识别这台设备', trigger: 'blur' }],
}

const proxyTypes = computed(() => props.meta?.proxy_types ?? Object.keys(PROXY_TYPE_LABEL))

function needsRemotePort(t: string) {
  return PROXY_TYPES_WITH_REMOTE_PORT.includes(t)
}
function needsSecret(t: string) {
  return PROXY_TYPES_WITH_SECRET_KEY.includes(t)
}
function needsHost(t: string) {
  return PROXY_TYPES_WITH_HOST.includes(t)
}

function addProxy() {
  const s = spec.value
  if (!s) return
  s.proxies.push(blankProxy())
  preview.value = null
}

function removeProxy(idx: number) {
  spec.value?.proxies.splice(idx, 1)
  preview.value = null
}

/** 把冲突行的端口换成下一个空闲端口。 */
function useFreePort(p: ProxySpec) {
  const free = nextFreePort()
  if (!free) {
    ElMessage.warning('6000-65535 之间已没有可用端口')
    return
  }
  p.remote_port = free
  preview.value = null
}

/** 自定义域名在表单里用逗号分隔的单行文本编辑。 */
function hostsText(p: ProxySpec): string {
  return (p.custom_domains || []).join(', ')
}
function setHosts(p: ProxySpec, v: string) {
  p.custom_domains = v
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

async function doPreview() {
  const s = spec.value
  if (!s) return
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  previewing.value = true
  clearError()
  try {
    const r = await api.clientPreview(s)
    preview.value = r
    // 端口冲突不阻止预览（否则看不到 TOML 就没法判断该改到哪个端口），
    // 但绝不能报「校验通过」—— 它确实过不了生成这一关。
    if ((r.blocking ?? []).length) {
      ElMessage.warning('配置本身合法，但存在会阻止生成的端口冲突')
    } else {
      ElMessage.success('配置校验通过（已由 frpc verify -c 确认）')
    }
  } catch (err) {
    preview.value = null
    setError(err, '预览失败')
  } finally {
    previewing.value = false
  }
}

async function doDownload() {
  const s = spec.value
  if (!s) return
  // 按钮已被置灰，这里再挡一次：Enter 提交或改端口后立即点，
  // 都可能绕过 disabled 的时序。
  if (cannotDownload.value) {
    ElMessage.warning('存在端口冲突，请先调整远端端口')
    return
  }
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  downloading.value = true
  clearError()
  try {
    const r = await api.clientDownload(s)
    saveBlob(r.blob, r.fileName)
    ElMessage.success(`已生成并开始下载：${r.fileName}`)
    // 刚签发的端口已进入台账，刷新一次，避免连续出包时表单还按旧占用提示。
    await loadPorts()
    // 通知外层：这台客户端的「新增」已经完成，由外层决定是否关闭弹窗。
    emit('generated', r.fileName)
  } catch (err) {
    setError(err, '生成失败')
  } finally {
    downloading.value = false
  }
}

const exeSizeText = computed(() =>
  props.meta ? `${formatBytes(props.meta.client_exe_size)}（frpc.exe）` : '—',
)

/** 同一份包复制到多台机器会造成身份冲突（01-prd §8.1）。 */
const oneDeviceNotice = '一个部署包只能用于一台设备。请勿把同一个包复制到多台机器上安装，否则它们会共用同一个 clientID，在管理台里互相顶替。'
</script>

<template>
  <div v-loading="loading" class="sf-stack">
    <el-alert v-if="errMsg" type="error" show-icon :closable="false" :title="errMsg">
      <template #default>
        <div v-for="(d, i) in errDetails" :key="i" style="margin-top: 4px">· {{ d }}</div>
      </template>
    </el-alert>

    <el-alert type="warning" show-icon :closable="false" :title="oneDeviceNotice" />

    <el-alert
      v-if="conflicts.length"
      type="error"
      show-icon
      :closable="false"
      :title="`有 ${conflicts.length} 个远端端口已被占用，无法生成部署包`"
    >
      <template #default>
        <div v-for="c in conflicts" :key="c.port" style="margin-top: 4px">
          · 端口 <b class="sf-mono">{{ c.port }}</b>（代理 {{ c.name }}）已被「{{
            ownerLabel(c.owner)
          }}」占用 —— {{ ownerHint(c.owner) }}
        </div>
      </template>
    </el-alert>

    <el-alert
      v-if="portsUnavailable"
      type="info"
      show-icon
      :closable="false"
      title="端口占用信息读取失败，界面暂时不做冲突提示；生成时服务端仍会按台账校验并拦截。"
    />

    <el-card v-if="spec" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">
            {{ draft ? '在这台设备原有的基础上改包' : '连接与身份' }}
          </span>
          <span class="sf-muted" style="font-size: 12px">包内将包含 {{ exeSizeText }}</span>
        </div>
      </template>

      <!-- 改包时先说清两件事：这份表单是从台账重建的，以及哪些项并非原样。
           后一条尤其重要 —— 它看起来与其它输入框毫无区别，
           不说的话管理员会把它当成「原本就这样」直接提交。 -->
      <el-alert
        v-if="draft"
        type="warning"
        show-icon
        :closable="false"
        style="margin-bottom: 12px"
        title="这是按台账记录重建的表单，不是原包本身"
        description="部署包不留存，真正能改的只有当初的输入。改完生成的是一个新包：clientID 保持不变，到目标机器上覆盖安装即可。"
      >
        <template v-if="restoreNotes">
          <div style="margin-top: 8px; font-weight: 600">{{ restoreNotes.title }}</div>
          <ul style="margin: 4px 0 0; padding-left: 18px">
            <li v-for="(it, i) in restoreNotes.items" :key="i">{{ it }}</li>
          </ul>
        </template>
      </el-alert>

      <el-alert
        v-for="(w, i) in draft?.warnings ?? []"
        :key="'w' + i"
        type="warning"
        show-icon
        :closable="false"
        :title="w"
        style="margin-bottom: 12px"
      />

      <el-alert
        v-if="endpointHint"
        :type="endpointWarn ? 'warning' : 'info'"
        show-icon
        :closable="false"
        :title="endpointHint"
        style="margin-bottom: 12px"
      />

      <el-form ref="formRef" :model="spec" :rules="rules" label-position="top">
        <div class="sf-form-grid">
          <el-form-item label="服务器地址" prop="server_addr">
            <el-input v-model="spec.server_addr" placeholder="例如 frp.example.com 或 116.169.217.89" spellcheck="false" />
          </el-form-item>
          <el-form-item label="服务器端口" prop="server_port">
            <el-input-number v-model="spec.server_port" :min="1" :max="65535" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="显示名称" prop="name">
            <el-input v-model="spec.name" placeholder="例如 财务部-前台机" />
          </el-form-item>
          <el-form-item label="目标平台" prop="platform">
            <el-select
              v-model="spec.platform"
              placeholder="请选择目标机器的系统"
              :disabled="platformOptions.length === 0"
              style="width: 100%"
            >
              <el-option
                v-for="p in platformOptions"
                :key="p"
                :value="p"
                :label="PLATFORM_LABEL[p] || p"
              />
            </el-select>
            <div class="sf-cell-sub" style="margin-top: 4px">{{ platformHint }}</div>
          </el-form-item>
          <el-form-item label="机器人类型" prop="robot_type">
            <el-select
              v-model="spec.robot_type"
              placeholder="请选择这台设备承载的机器人类型"
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
              仅用于在客户端台账与包内 README 中标识设备，不影响连接与端口；留空即「未分类」。
            </div>
          </el-form-item>
          <el-form-item label="客户端标识 clientID" prop="client_id">
            <el-input v-model="spec.client_id" spellcheck="false">
              <template #append>
                <el-tooltip content="重新生成一个随机标识" placement="top">
                  <el-button @click="regenerateClientID">
                    <el-icon><Refresh /></el-icon>
                  </el-button>
                </el-tooltip>
              </template>
            </el-input>
            <div class="sf-cell-sub" style="margin-top: 4px">
              该值用于跨重连识别同一台设备，创建后不要随意改动。
            </div>
          </el-form-item>
          <el-form-item label="认证 token" prop="token">
            <SecretField
              v-model="spec.token"
              hint="必须与服务器端 frps.toml 中的 token 完全一致，否则客户端无法登录。"
            />
          </el-form-item>
        </div>

        <el-divider content-position="left">传输与日志</el-divider>

        <div class="sf-form-grid">
          <el-form-item label="启用传输加密">
            <el-switch v-model="spec.enable_tls" />
            <div class="sf-cell-sub" style="margin-top: 4px">
              frps 与 frpc 两侧需同时启用；仅在公网链路下才需要。
            </div>
          </el-form-item>
          <el-form-item v-if="spec.enable_tls" label="TLS Server Name">
            <el-input v-model="spec.tls_server_name" placeholder="留空则使用服务器地址" spellcheck="false" />
          </el-form-item>
          <el-form-item label="日志级别">
            <el-select v-model="spec.log_level" style="width: 100%">
              <el-option v-for="l in LOG_LEVELS" :key="l" :value="l" :label="l" />
            </el-select>
          </el-form-item>
          <el-form-item label="日志保留天数">
            <el-input-number v-model="spec.log_max_days" :min="1" :max="365" controls-position="right" style="width: 100%" />
          </el-form-item>
        </div>
      </el-form>
    </el-card>

    <el-card v-if="spec" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">代理规则（{{ spec.proxies.length }}）</span>
          <el-button type="primary" plain size="small" @click="addProxy">
            <el-icon><Plus /></el-icon>
            <span style="margin-left: 4px">添加代理</span>
          </el-button>
        </div>
      </template>

      <el-table :data="spec.proxies" size="small" border>
        <el-table-column label="代理名" min-width="150">
          <template #default="{ row }">
            <el-input v-model="row.name" size="small" placeholder="例如 app-tcp" spellcheck="false" />
            <div class="sf-cell-sub">仅限字母、数字、. _ -</div>
          </template>
        </el-table-column>
        <el-table-column label="类型" width="140">
          <template #default="{ row }">
            <el-select v-model="row.type" size="small" style="width: 100%">
              <el-option v-for="t in proxyTypes" :key="t" :value="t" :label="PROXY_TYPE_LABEL[t] || t" />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column label="本地地址" width="150">
          <template #default="{ row }">
            <el-input v-model="row.local_ip" size="small" placeholder="127.0.0.1" spellcheck="false" />
          </template>
        </el-table-column>
        <el-table-column label="本地端口" width="110">
          <template #default="{ row }">
            <el-input-number v-model="row.local_port" size="small" :min="0" :max="65535" :controls="false" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="远端端口" width="168">
          <template #default="{ row }">
            <template v-if="needsRemotePort(row.type)">
              <el-input-number
                v-model="row.remote_port"
                size="small"
                :min="0"
                :max="65535"
                :controls="false"
                style="width: 100%"
              />
              <template v-if="portConflict(row)">
                <el-tooltip :content="conflictTip(row)" placement="top">
                  <div class="sf-cell-sub" style="color: var(--el-color-danger); margin-top: 4px">
                    {{ conflictLabel(row) }}
                  </div>
                </el-tooltip>
                <el-button link type="primary" size="small" @click="useFreePort(row)">
                  改用 {{ nextFreePort() }}
                </el-button>
              </template>
            </template>
            <span v-else class="sf-cell-sub">不适用</span>
          </template>
        </el-table-column>
        <el-table-column label="域名 / 子域" min-width="170">
          <template #default="{ row }">
            <template v-if="needsHost(row.type)">
              <el-input
                :model-value="hostsText(row)"
                size="small"
                placeholder="a.example.com, b.example.com"
                spellcheck="false"
                @update:model-value="(v: string) => setHosts(row, v)"
              />
              <el-input
                v-model="row.subdomain"
                size="small"
                placeholder="或子域，例如 demo"
                spellcheck="false"
                style="margin-top: 4px"
              />
            </template>
            <span v-else class="sf-cell-sub">不适用</span>
          </template>
        </el-table-column>
        <el-table-column label="预共享密钥" min-width="150">
          <template #default="{ row }">
            <SecretField v-if="needsSecret(row.type)" v-model="row.secret_key" placeholder="双方必须一致" />
            <span v-else class="sf-cell-sub">不适用</span>
          </template>
        </el-table-column>
        <el-table-column label="加密" width="66" align="center">
          <template #default="{ row }"><el-checkbox v-model="row.use_encryption" /></template>
        </el-table-column>
        <el-table-column label="压缩" width="66" align="center">
          <template #default="{ row }"><el-checkbox v-model="row.use_compression" /></template>
        </el-table-column>
        <el-table-column label="" width="64" align="center">
          <template #default="{ $index }">
            <el-button link type="danger" size="small" @click="removeProxy($index)">
              <el-icon><Delete /></el-icon>
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 14px 0">至少需要一条代理规则</div>
        </template>
      </el-table>
    </el-card>

    <div v-if="spec" class="sf-row">
      <el-button :loading="previewing" @click="doPreview">
        <el-icon><View /></el-icon>
        <span style="margin-left: 4px">校验并预览</span>
      </el-button>
      <el-button type="primary" :loading="downloading" :disabled="cannotDownload" @click="doDownload">
        <el-icon><Download /></el-icon>
        <span style="margin-left: 4px">生成并下载 ZIP</span>
      </el-button>
      <el-button link @click="loadDefaults">恢复默认值</el-button>
      <span v-if="cannotDownload" class="sf-muted" style="font-size: 12px">
        存在端口冲突，调整远端端口后即可生成
      </span>
    </div>

    <!-- 预览结果 -->
    <el-card v-if="preview" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">配置预览 · {{ preview.file_name }}</span>
          <span class="sf-muted" style="font-size: 12px">
            FRP {{ preview.frp_version }} 校验通过 · 包内共 {{ preview.files?.length ?? 0 }} 个文件
          </span>
        </div>
      </template>

      <el-alert
        v-for="(b, i) in blockingPreview"
        :key="'b' + i"
        :title="b"
        type="error"
        show-icon
        :closable="false"
        style="margin-bottom: 8px"
      />

      <el-alert
        v-for="(w, i) in preview.warnings"
        :key="i"
        :title="w"
        type="warning"
        show-icon
        :closable="false"
        style="margin-bottom: 8px"
      />

      <div class="sf-cell-sub" style="margin-bottom: 8px">
        以下内容中的 token 等敏感字段已被遮蔽；实际写入 ZIP 的是明文（客户端需要能直接运行）。
      </div>
      <pre class="sf-code">{{ preview.toml }}</pre>

      <el-divider content-position="left">包内文件清单</el-divider>
      <div class="sf-row" style="flex-wrap: wrap; gap: 6px">
        <el-tag v-for="f in preview.files" :key="f" size="small" type="info" effect="plain">{{ f }}</el-tag>
      </div>
    </el-card>
  </div>
</template>
