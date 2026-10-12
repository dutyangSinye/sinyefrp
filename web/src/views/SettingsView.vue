<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import type { DataStatusResponse, NodeTestResult } from '@/api/types'
import { errorKindLabel, formatDateTime } from '@/utils/format'

// 节点配置与连通性测试（F03）。
//
// 两条容易踩空的交互，界面必须显式表达：
//   1. 密码留空 = 保持原密码。界面只拿到 password_configured 布尔，
//      因此不能把密码字段回填成掩码字符串再提交（那样会把真密码覆盖成 "******"）。
//   2. 当进程被命令行参数或环境变量覆盖时（overridden.active），保存会成功
//      但不会生效，必须提示重启，否则用户会陷入「保存了却没反应」。

const SOURCE_LABEL: Record<string, string> = {
  default: '内置默认值',
  config: 'config.toml',
  db: '管理台设置',
  override: '命令行参数 / 环境变量',
}

const formRef = ref<FormInstance>()
const loading = ref(false)
const saving = ref(false)
const testing = ref(false)
const errMsg = ref('')
const okMsg = ref('')
const testResult = ref<NodeTestResult | null>(null)

const source = ref('')
const overridden = ref(false)
const passwordConfigured = ref(false)

const form = reactive({
  name: '',
  api_url: '',
  username: '',
  password: '',
  timeout_seconds: 5,
  enabled: true,
})

const rules: FormRules = {
  api_url: [
    { required: true, message: '请填写 FRP Dashboard 地址', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) => {
        if (!v) return cb()
        if (!/^https?:\/\/.+/i.test(v)) {
          return cb(new Error('需以 http:// 或 https:// 开头，例如 http://127.0.0.1:7500'))
        }
        cb()
      },
      trigger: 'blur',
    },
  ],
  timeout_seconds: [{ required: true, message: '请填写超时时间', trigger: 'blur' }],
}

async function load() {
  loading.value = true
  errMsg.value = ''
  try {
    const r = await api.nodeConfig()
    form.name = r.node.name || ''
    form.api_url = r.node.api_url || ''
    form.username = r.node.username || ''
    form.timeout_seconds = r.node.timeout_seconds || 5
    form.enabled = r.node.enabled
    passwordConfigured.value = r.node.password_configured
    // 刻意不回填密码：留空即「不修改」。
    form.password = ''
    source.value = r.source
    overridden.value = !!r.overridden?.active
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '加载节点配置失败'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  loadEndpoint()
  loadDataStatus()
})

/** 提交体：密码为空时不带 password 字段语义上等价于「保持不变」，后端也是这样处理的。 */
function payload(): Record<string, unknown> {
  return {
    name: form.name.trim(),
    api_url: form.api_url.trim(),
    username: form.username.trim(),
    password: form.password,
    timeout_seconds: form.timeout_seconds,
    enabled: form.enabled,
  }
}

async function doTest() {
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  testing.value = true
  errMsg.value = ''
  okMsg.value = ''
  testResult.value = null
  try {
    testResult.value = await api.testNode(payload())
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '测试失败'
  } finally {
    testing.value = false
  }
}

async function doSave() {
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  saving.value = true
  errMsg.value = ''
  okMsg.value = ''
  try {
    const r = await api.saveNodeConfig(payload())
    okMsg.value = r.message
    source.value = r.source
    overridden.value = !!r.needs_restart
    form.password = ''
    passwordConfigured.value = true
    ElMessage.success(r.needs_restart ? '已保存，但需重启后生效' : '已保存并立即生效')
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

// ---------- 客户端接入点 ----------
//
// 保存「客户端部署包要连的地址、端口与 token」。与上面的节点配置刻意分开：
// 那是管理平台去采集状态的 Dashboard 地址（常为环回/内网），
// 这是 frpc 在目标机器上要连的地址。生产 frps 前面有 NAT 映射时连端口都不同
// （内部 bindPort 7000、对外可能 12123），因此不能互相推导。

const epLoading = ref(false)
const epSaving = ref(false)
const epErr = ref('')
const epOk = ref('')
const epConfigured = ref(false)
const epTokenConfigured = ref(false)
const epDecryptFailed = ref(false)

const epForm = reactive({
  client_addr: '',
  client_port: 7000,
  token: '',
})

async function loadEndpoint() {
  epLoading.value = true
  epErr.value = ''
  try {
    const r = await api.pkgEndpoint()
    epForm.client_addr = r.client_addr || ''
    epForm.client_port = r.client_port || 7000
    // 刻意不回填 token：留空即「不修改」，避免把真 token 覆盖成掩码串。
    epForm.token = ''
    epConfigured.value = r.configured
    epTokenConfigured.value = r.token_configured
    epDecryptFailed.value = !!r.token_decrypt_failed
  } catch (err) {
    epErr.value = err instanceof HttpError ? err.message : '加载接入点配置失败'
  } finally {
    epLoading.value = false
  }
}

function epPayload(clearToken: boolean): Record<string, unknown> {
  return {
    client_addr: epForm.client_addr.trim(),
    client_port: epForm.client_port,
    token: epForm.token,
    clear_token: clearToken,
  }
}

function applyEndpoint(r: { endpoint: { configured: boolean; token_configured: boolean } }, msg: string) {
  epOk.value = msg
  epConfigured.value = r.endpoint.configured
  epTokenConfigured.value = r.endpoint.token_configured
  epDecryptFailed.value = false
  epForm.token = ''
}

async function saveEndpoint() {
  epSaving.value = true
  epErr.value = ''
  epOk.value = ''
  try {
    const r = await api.savePkgEndpoint(epPayload(false))
    applyEndpoint(r, r.message)
    ElMessage.success('接入点已保存')
  } catch (err) {
    epErr.value = err instanceof HttpError ? err.message : '保存失败'
  } finally {
    epSaving.value = false
  }
}

async function clearEndpointToken() {
  try {
    await ElMessageBox.confirm(
      '确定清空接入点 token？若 frps 侧配置了 auth.token，之后生成的客户端包将连不上，直到重新填写一致的 token。',
      '清空 token',
      { type: 'warning', confirmButtonText: '清空', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  epSaving.value = true
  epErr.value = ''
  epOk.value = ''
  try {
    const r = await api.savePkgEndpoint(epPayload(true))
    applyEndpoint(r, '接入点 token 已清空。')
    ElMessage.success('已清空 token')
  } catch (err) {
    epErr.value = err instanceof HttpError ? err.message : '清空失败'
  } finally {
    epSaving.value = false
  }
}

const sourceLabel = computed(() => SOURCE_LABEL[source.value] || source.value || '—')

const testOk = computed(() => testResult.value?.ok === true)
const testCompatible = computed(() => testResult.value?.compatible === true)

// ---------- 数据存储（排障用）----------
//
// 这一块不是「锦上添花」，是这次事故的直接产物。
//
// 现场反馈「重启后下午签发的记录没了」，但当时**没有任何界面能看到
// 平台正在使用哪个数据库文件**：相对路径按配置文件所在目录解析而不是进程
// 工作目录，只能靠翻磁盘、比时间戳去反推。三分钟能确认的事拖成了几轮排查。
//
// 因此这里把「库在哪、多大、WAL 多大」直接摆出来。WAL 偏大时给出明确建议 ——
// WAL 中的数据尚无第二副本，只有正常关闭（restart.ps1 / stop.ps1）才会回写主库。

const dataLoading = ref(false)
const dataErr = ref('')
const dataStatus = ref<DataStatusResponse | null>(null)

async function loadDataStatus() {
  dataLoading.value = true
  dataErr.value = ''
  try {
    dataStatus.value = await api.dataStatus()
  } catch (err) {
    dataErr.value = err instanceof HttpError ? err.message : '加载数据存储状态失败'
  } finally {
    dataLoading.value = false
  }
}

/** 目录部分——路径很长时单行显示会被挤断，拆成目录 + 文件名分两行更易读。 */
const dbDir = computed(() => {
  const p = dataStatus.value?.database.path || ''
  const i = Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/'))
  return i > 0 ? p.slice(0, i) : ''
})

const dbFile = computed(() => {
  const p = dataStatus.value?.database.path || ''
  const i = Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/'))
  return i >= 0 ? p.slice(i + 1) : p
})

async function copyDbPath() {
  const p = dataStatus.value?.database.path
  if (!p) return
  // 优先用 Clipboard API；它需要安全上下文，HTTP 访问（非 localhost）下不可用，
  // 因此退回 execCommand 这条老路，否则「复制路径」在某些场景会静默失效。
  try {
    await navigator.clipboard.writeText(p)
    ElMessage.success('路径已复制')
    return
  } catch {
    /* 继续走兜底 */
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = p
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
    ElMessage.success('路径已复制')
  } catch {
    ElMessage.warning('当前浏览器不允许自动复制，请手工选取路径')
  }
}
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">设置</h2>
        <p class="sf-page-sub" style="margin: 0">
          管理平台通过 frps 内置的 Dashboard API 只读获取状态。凭据由管理服务持有，浏览器不会直接访问 FRP 端口。
        </p>
      </div>
      <el-button :loading="loading" @click="load">
        <el-icon><Refresh /></el-icon>
        <span style="margin-left: 4px">重新载入</span>
      </el-button>
    </div>

    <el-alert v-if="errMsg" :title="errMsg" type="error" show-icon :closable="false" />
    <el-alert v-if="okMsg" :title="okMsg" type="success" show-icon :closable="false" />

    <el-alert
      v-if="overridden"
      type="warning"
      show-icon
      :closable="false"
      title="当前进程的节点配置被命令行参数或环境变量覆盖"
      description="此处保存的配置会写库，但在重启管理平台之前不会生效。界面与采集结果仍使用覆盖值。"
    />

    <el-card v-loading="loading" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">FRP 节点</span>
          <span class="sf-muted" style="font-size: 12px">当前生效来源：{{ sourceLabel }}</span>
        </div>
      </template>

      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <div class="sf-form-grid">
          <el-form-item label="节点名称">
            <el-input v-model="form.name" placeholder="例如 本机 frps" />
            <div class="sf-cell-sub" style="margin-top: 4px">仅用于界面识别，不影响采集。</div>
          </el-form-item>

          <el-form-item label="Dashboard 地址" prop="api_url">
            <el-input v-model="form.api_url" placeholder="http://127.0.0.1:7500" spellcheck="false" />
            <div class="sf-cell-sub" style="margin-top: 4px">
              对应 frps.toml 中 webServer 的地址与端口。
            </div>
          </el-form-item>

          <el-form-item label="Dashboard 用户名">
            <el-input v-model="form.username" spellcheck="false" autocomplete="off" />
          </el-form-item>

          <el-form-item label="Dashboard 密码">
            <el-input
              v-model="form.password"
              type="password"
              show-password
              autocomplete="new-password"
              :placeholder="passwordConfigured ? '已设置（留空表示不修改）' : '尚未设置'"
            />
            <div class="sf-cell-sub" style="margin-top: 4px">
              出于安全考虑不会回填已有密码；留空提交即保持原值不变。
            </div>
          </el-form-item>

          <el-form-item label="请求超时（秒）" prop="timeout_seconds">
            <el-input-number
              v-model="form.timeout_seconds"
              :min="1"
              :max="300"
              controls-position="right"
              style="width: 100%"
            />
          </el-form-item>

          <el-form-item label="启用采集">
            <el-switch v-model="form.enabled" />
            <div class="sf-cell-sub" style="margin-top: 4px">关闭后将停止定时采集，界面显示为状态未知。</div>
          </el-form-item>
        </div>

        <div class="sf-row" style="margin-top: 6px">
          <el-button :loading="testing" @click="doTest">
            <el-icon><Connection /></el-icon>
            <span style="margin-left: 4px">测试连通性</span>
          </el-button>
          <el-button type="primary" :loading="saving" @click="doSave">
            <el-icon><Check /></el-icon>
            <span style="margin-left: 4px">保存并生效</span>
          </el-button>
        </div>
      </el-form>
    </el-card>

    <el-card v-loading="epLoading" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">客户端接入点</span>
          <el-tag :type="epConfigured ? 'success' : 'warning'" effect="light" size="small">
            {{ epConfigured ? '已配置' : '尚未配置' }}
          </el-tag>
        </div>
      </template>

      <p class="sf-muted" style="margin: 0 0 12px; font-size: 13px; line-height: 1.8">
        这里填客户端部署包要连的地址与端口，也就是 frpc 所在机器能访问到的 frps 入口。
        它与上面的 Dashboard 地址是两回事：Dashboard 通常只监听环回；而 frps 前面若有 NAT 映射，
        对外端口与内部 bindPort 也不相同（例如内部 7000、对外 12123）。
        填好之后，生成的客户端包会默认带上这里的地址、端口与 token，不需要再手工回填；
        服务端包的 token 也从这里取，因此两端天然一致。
      </p>

      <el-alert v-if="epErr" :title="epErr" type="error" show-icon :closable="false" style="margin-bottom: 12px" />
      <el-alert v-if="epOk" :title="epOk" type="success" show-icon :closable="false" style="margin-bottom: 12px" />
      <el-alert
        v-if="epDecryptFailed"
        type="warning"
        show-icon
        :closable="false"
        style="margin-bottom: 12px"
        title="已保存的 token 无法解密"
        description="密文与写入它的机器绑定（Windows DPAPI）。若 SQLite 是从别的机器拷过来的，需要重新填写 token；地址与端口不受影响。"
      />

      <el-form label-position="top">
        <div class="sf-form-grid">
          <el-form-item label="客户端连接地址">
            <el-input
              v-model="epForm.client_addr"
              placeholder="例如 116.169.217.89 或 frp.example.com"
              spellcheck="false"
            />
            <div class="sf-cell-sub" style="margin-top: 4px">域名或 IP，不要带端口与协议前缀。</div>
          </el-form-item>

          <el-form-item label="客户端连接端口">
            <el-input-number
              v-model="epForm.client_port"
              :min="1"
              :max="65535"
              controls-position="right"
              style="width: 100%"
            />
            <div class="sf-cell-sub" style="margin-top: 4px">frpc 实际连接的端口，未必等于 frps 的 bindPort。</div>
          </el-form-item>

          <el-form-item label="FRP auth token">
            <el-input
              v-model="epForm.token"
              type="password"
              show-password
              autocomplete="new-password"
              :placeholder="epTokenConfigured ? '已设置（留空表示不修改）' : '尚未设置'"
            />
            <div class="sf-cell-sub" style="margin-top: 4px">
              必须与 frps 的 auth.token 一致，否则客户端连不上。出于安全考虑不回填已有 token。
            </div>
          </el-form-item>
        </div>

        <div class="sf-row" style="margin-top: 6px">
          <el-button type="primary" :loading="epSaving" @click="saveEndpoint">
            <el-icon><Check /></el-icon>
            <span style="margin-left: 4px">保存接入点</span>
          </el-button>
          <el-button :disabled="!epTokenConfigured || epSaving" @click="clearEndpointToken">清空 token</el-button>
        </div>
      </el-form>
    </el-card>

    <el-card v-if="testResult" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">连通性测试结果</span>
          <el-tag :type="testOk ? (testCompatible ? 'success' : 'warning') : 'danger'" effect="light">
            {{ testOk ? (testCompatible ? '连通且版本兼容' : '连通但版本可疑') : '失败' }}
          </el-tag>
        </div>
      </template>

      <el-alert
        :title="testResult.message"
        :type="testOk ? (testCompatible ? 'success' : 'warning') : 'error'"
        show-icon
        :closable="false"
        style="margin-bottom: 12px"
      />

      <el-descriptions :column="3" border size="small">
        <el-descriptions-item label="目标地址">
          <span class="sf-mono">{{ testResult.target_api_url }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="耗时">{{ testResult.latency_ms }} ms</el-descriptions-item>
        <el-descriptions-item label="FRP 版本">
          <span class="sf-mono">{{ testResult.frp_version || '未知' }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="错误分类">
          {{ testResult.kind ? errorKindLabel(testResult.kind) : '无' }}
        </el-descriptions-item>
        <el-descriptions-item label="客户端数">{{ testResult.client_count }}</el-descriptions-item>
        <el-descriptions-item label="代理数">{{ testResult.proxy_count }}</el-descriptions-item>
        <el-descriptions-item label="测试时间" :span="3">
          {{ formatDateTime(testResult.tested_at) }}
        </el-descriptions-item>
      </el-descriptions>

      <div class="sf-cell-sub" style="margin-top: 10px">
        若提示版本不兼容，说明该 frps 的 Dashboard 响应结构与本平台实测的 0.69.x 契约不一致，采集字段可能缺失。
      </div>
    </el-card>

    <el-card v-loading="dataLoading" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">数据存储</span>
          <el-button link size="small" @click="loadDataStatus">刷新</el-button>
        </div>
      </template>

      <el-alert v-if="dataErr" :title="dataErr" type="error" show-icon :closable="false" style="margin-bottom: 12px" />

      <template v-if="dataStatus">
        <el-alert
          :title="dataStatus.hint"
          :type="dataStatus.wal.heavy ? 'warning' : 'info'"
          show-icon
          :closable="false"
          style="margin-bottom: 12px"
        />

        <el-descriptions :column="1" border size="small">
          <el-descriptions-item label="数据库文件">
            <div class="sf-mono" style="word-break: break-all">{{ dbDir }}</div>
            <div class="sf-mono" style="font-weight: 600">{{ dbFile }}</div>
            <div class="sf-row" style="margin-top: 6px; gap: 8px">
              <el-button link size="small" @click="copyDbPath">复制完整路径</el-button>
            </div>
          </el-descriptions-item>
          <el-descriptions-item label="主库大小">
            {{ dataStatus.database.size_text }}
            <span v-if="!dataStatus.database.exists" class="sf-muted">（文件不存在，重启会自动新建空库）</span>
          </el-descriptions-item>
          <el-descriptions-item label="主库最后写入">
            {{ dataStatus.database.modified_at ? formatDateTime(dataStatus.database.modified_at) : '—' }}
          </el-descriptions-item>
          <el-descriptions-item label="预写日志 WAL">
            <span :style="dataStatus.wal.heavy ? 'color: var(--el-color-warning); font-weight: 600' : ''">
              {{ dataStatus.wal.exists ? dataStatus.wal.size_text : '当前无（数据已全部回写主库）' }}
            </span>
          </el-descriptions-item>
          <el-descriptions-item v-if="dataStatus.wal.exists" label="WAL 最后写入">
            {{ dataStatus.wal.modified_at ? formatDateTime(dataStatus.wal.modified_at) : '—' }}
          </el-descriptions-item>
        </el-descriptions>

        <div class="sf-cell-sub" style="margin-top: 10px; line-height: 1.9">
          相对路径按 <span class="sf-mono">config.toml</span> 所在目录解析，与启动时的工作目录无关。
          备份时请连同 <span class="sf-mono">-wal</span>、<span class="sf-mono">-shm</span> 两个文件一起复制；
          只拷主库会丢失尚未回写的记录。
        </div>
      </template>
    </el-card>

    <el-card shadow="never">
      <template #header>
        <span style="font-weight: 600">关于本页安全边界</span>
      </template>
      <ul class="sf-muted" style="margin: 0; padding-left: 18px; font-size: 13px; line-height: 1.9">
        <li>管理服务是 FRP Dashboard 凭据的唯一持有方，浏览器永不直接调用 <code>127.0.0.1:7500</code>。</li>
        <li>密码以 DPAPI 加密后落库（与当前机器绑定），接口只返回「是否已设置」。</li>
        <li>修改 token 或 Dashboard 密码会使已部署的客户端/管理平台立即失联，请谨慎轮换。</li>
        <li>本服务不下发任何 CORS 头且写接口要求 <code>application/json</code>，跨站请求无法携带会话完成写操作。</li>
      </ul>
    </el-card>
  </div>
</template>
