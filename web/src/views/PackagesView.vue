<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import type { PkgMeta, PkgRecord, RegistryClient, RegistrySpecResponse } from '@/api/types'
import { REGISTRY_STATUS_LABEL, robotTypeLabel } from '@/api/types'
import { formatBytes, formatDateTime } from '@/utils/format'
import ClientPackageForm from '@/components/ClientPackageForm.vue'
import ServerPackageForm from '@/components/ServerPackageForm.vue'
import PlatformPackageCard from '@/components/PlatformPackageCard.vue'

// 部署包生成（F09-F12 / F15 / F17 / F19）。
//
// 页面顶部先把「打包能力」摊开：内嵌的 FRP 版本、二进制哈希与大小。
// 原因是部署包一旦发出就无法回收，管理员在点下载之前应当能确认
// 「我发出去的是哪个版本的 frpc」——这正是 F15 要求记录版本与哈希的理由。
//
// 客户端表单放在**弹窗**里而不是常驻在页面上（F19）：
//   - 一次出一台设备，表单本身就代表「新增一台客户端」，弹窗是最贴合的形态；
//   - 弹窗用 destroy-on-close，每次打开都会重新挂载表单、重新取一份默认值，
//     于是每次「新增」天然是一个全新的 clientID。常驻表单做不到这一点 ——
//     管理员很容易在上一次填的内容上继续改，最后发出两个几乎同名的包。
//   - 关掉弹窗就回到列表，「已签发过哪些」始终在视线里。

const router = useRouter()
const route = useRoute()

const meta = ref<PkgMeta | null>(null)
const metaErr = ref('')
const loadingMeta = ref(false)
const tab = ref<'client' | 'server' | 'platform'>('client')

// ---------- 客户端台账摘要（F19） ----------
//
// 这里只展示最近签发的几条，回答「我刚发出去的包在不在台账里」。
// 完整的编辑/停用/端口释放都在「客户端台账」页，不在这里重复一遍。

const recent = ref<RegistryClient[]>([])
const registeredTotal = ref(0)
const loadingRecent = ref(false)

async function loadRecent() {
  loadingRecent.value = true
  try {
    const r = await api.registryClients()
    registeredTotal.value = r.clients?.length ?? 0
    recent.value = (r.clients ?? []).slice(0, 5)
  } catch {
    // 台账摘要纯属辅助信息：读不出来不该挡住打包主流程，
    // 更不能让整页报错（打包才是这个页面的主任务）。
    recent.value = []
    registeredTotal.value = 0
  } finally {
    loadingRecent.value = false
  }
}

// ---------- 服务器部署包：最近生成记录 ----------
//
// 与客户端 tab 保持同形：上面是「新增」按钮，下面是「已经生成过哪些」的列表。
//
// 列表数据源和客户端**刻意不同**：客户端列的是台账（可停用/可释放端口，是活的状态），
// 服务器包没有台账 —— 它只有 F16 的生成流水（pkg_record，只读历史）。
// 这不是偷懒：服务器包发出的那一刻起，服务端就不再持有它的密钥，
// 无法也不应重发（见 handlePkgRecords 的注释），所以它只能是流水，不能是台账。

const recentServer = ref<PkgRecord[]>([])
const loadingRecentServer = ref(false)

async function loadRecentServer() {
  loadingRecentServer.value = true
  try {
    const r = await api.pkgRecords()
    recentServer.value = (r.records ?? [])
      .filter((x) => x.kind === 'server')
      .slice(0, 5)
  } catch {
    // 同为辅助信息：读不出来不该挡住打包主流程。
    recentServer.value = []
  } finally {
    loadingRecentServer.value = false
  }
}

async function loadMeta() {
  loadingMeta.value = true
  metaErr.value = ''
  try {
    meta.value = await api.pkgMeta()
  } catch (err) {
    metaErr.value = err instanceof HttpError ? err.message : '读取打包能力信息失败'
  } finally {
    loadingMeta.value = false
  }
}

// ---------- 在已有客户端的基础上改包（来自台账页的「生成包」） ----------
//
// 台账页跳转时只带台账 id（/packages?from=<id>），不带任何包名或密钥 ——
// spec 里有 token，塞进地址栏等于把它写进浏览器历史与截图里。
// 这里自己按 id 取草稿，取到后再打开弹窗：顺序反了会先闪一版全新默认值。
const repackDraft = ref<RegistrySpecResponse | null>(null)
/** 改包目标的显示名，取到了才用得上。 */
const repackName = ref('')

async function tryOpenRepack() {
  const raw = route.query.from
  const id = Number(Array.isArray(raw) ? raw[0] : raw)
  if (!Number.isFinite(id) || id <= 0) return
  try {
    const d = await api.registrySpec(id)
    repackDraft.value = d
    repackName.value = d.spec.name || d.spec.client_id
    tab.value = 'client'
    clientDialog.value = true
  } catch (err) {
    // 取不到草稿就退回「新增」而不是卡住：那条台账记录可能刚被删，
    // 但管理员的意图（我能理解：他想出个包）不该被这个失败挡死。
    ElMessage.error(err instanceof HttpError ? err.message : '读取原有配置失败，已退回新增')
  }
}

onMounted(() => {
  loadMeta()
  loadRecent()
  loadRecentServer()
  tryOpenRepack()
})

// ---------- 新增客户端弹窗 ----------

const clientDialog = ref(false)

function openClientDialog() {
  // 「新增」必须清掉上一份草稿，否则第二次打开会带着第一次设备的 clientID ——
  // 而它的样子完全正常，只是一个新包悄悄挂到了另一台设备的名下。
  repackDraft.value = null
  repackName.value = ''
  clientDialog.value = true
}

/**
 * 一次客户端包生成完成。
 *
 * 关弹窗的时机刻意放在这里而不是「点了下载就关」：生成接口可能因为端口冲突
 * 返回 409（此时表单里要显示逐条冲突并让管理员改端口），失败时窗口不能消失。
 * ClientPackageForm 只在真正拿到 ZIP 之后才发这个事件。
 */
async function onClientGenerated(_fileName: string) {
  // 成功提示由表单自己发（它知道文件名与下载结果），这里再发一条会叠成两个 toast。
  clientDialog.value = false
  // 退场时把 from 从地址栏抹掉：留着的话刷新页面会又弹出同一个改包窗口，
  // 而管理员以为页面只是回到列表 —— 那种「我没点它怎么自己开了」的困惑，
  // 在一个要发到现场的产物旁边格外不该出现。
  if (route.query.from !== undefined) {
    router.replace({ path: '/packages' })
  }
  repackDraft.value = null
  repackName.value = ''
  await loadRecent()
}

function goRegistry() {
  router.push('/registry')
}

// ---------- 新增服务器部署包弹窗 ----------
//
// 与客户端弹窗同形同机理（destroy-on-close）：
// 每次打开都重新挂载表单、重新取一份默认值，因此不存在「在上一次填的内容上继续改」
// 这种半新半旧的状态；关掉弹窗就回到列表。
//
// 一个必须知道的副作用，已在表单内的告警里写明：默认值里的 token 由服务端随机生成，
// 每次打开弹窗都会是**新** token。服务器已上线时重新出包会让已部署客户端集体失联 ——
// 这不是 bug，而是密钥轮换本身就该是一次慎重的操作。

const serverDialog = ref(false)

function openServerDialog() {
  serverDialog.value = true
}

async function onServerGenerated(_fileName: string) {
  serverDialog.value = false
  await loadRecentServer()
}

function goRecords() {
  router.push('/records')
}

/** 状态标签：停用的记录虽然在台账里，但它的端口已被视为释放。 */
function statusLabel(s: string) {
  // 与台账页同源，取值一律查表后回退原值，不凭猜测写死字符串（F19 教训）。
  return REGISTRY_STATUS_LABEL[s] || s || '—'
}

function statusTagType(s: string) {
  return s === 'retired' ? 'info' : 'success'
}

function occupiedPorts(c: RegistryClient): number[] {
  return (c.ports ?? []).filter((p) => p.remote_port > 0).map((p) => p.remote_port)
}

/**
 * 二进制来源的中文说明。
 *
 * 取值必须与 internal/pack/assets.go 逐字一致：内嵌是 "embedded"（不是 "embed"），
 * 回退读盘是 "disk:<目录>"。这里做前缀判断而不是等值判断，
 * 免得开发期的 fallback 形态被当成未知值原样透出来。
 */
const sourceText = computed(() => {
  const s = meta.value?.source
  if (!s) return '—'
  if (s === 'embedded') return '编译期内嵌'
  if (s.startsWith('disk:')) return `读盘回退（${s.slice(5)}）`
  return s
})

// 支持范围是一个集合：frps 与内嵌 frpc 分批升级时两端版本必然不一致，
// 只按单个前缀 startsWith 会把过渡期合法的那半边误报成「不兼容」。
const versionMismatch = computed(() => {
  const m = meta.value
  if (!m || !m.frp_version) return false
  const prefixes = m.supported_prefixes ?? []
  if (prefixes.length === 0) return false
  return !prefixes.some((p) => m.frp_version.startsWith(p))
})

const supportedVersionsText = computed(() => {
  const prefixes = meta.value?.supported_prefixes ?? []
  if (prefixes.length === 0) return '未知'
  return prefixes.map((p) => `${p.replace(/\.$/, '')}.x`).join(' / ')
})
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">部署包</h2>
        <p class="sf-page-sub" style="margin: 0">
          表单生成的 ZIP 可直接解压到目标 Windows 主机，<strong>双击包内的「一键安装.bat」</strong>即可完成安装并注册开机自启：它会自动请求管理员权限，不需要你打开 PowerShell，也不受脚本执行策略影响。命令行方式仍然可用，以管理员身份执行 <code>.\install.ps1</code> 效果完全相同。
        </p>
      </div>
      <el-button :loading="loadingMeta" @click="loadMeta">
        <el-icon><Refresh /></el-icon>
        <span style="margin-left: 4px">刷新能力信息</span>
      </el-button>
    </div>

    <el-alert v-if="metaErr" :title="metaErr" type="error" show-icon :closable="false" />

    <el-alert
      v-if="versionMismatch"
      type="warning"
      show-icon
      :closable="false"
      :title="meta?.version_warning || '内嵌的 FRP 版本与当前正式支持的版本不一致'"
    />

    <el-alert
      v-if="meta && !meta.platform_available"
      type="warning"
      show-icon
      :closable="false"
      title="平台自身部署包暂不可用（不影响客户端包与服务器包）"
      :description="meta.platform_unavailable_reason"
    />

    <el-card v-if="meta" v-loading="loadingMeta" shadow="never">
      <div class="sf-kv">
        <div>
          <span>内嵌 FRP 版本</span>
          <b class="sf-mono">{{ meta.frp_version || '未知' }}</b>
          <div class="sf-cell-sub">正式支持 {{ supportedVersionsText }}</div>
        </div>
        <div>
          <span>二进制来源</span>
          <b>{{ sourceText }}</b>
          <div class="sf-cell-sub">生成包时不依赖外部目录</div>
        </div>
        <div>
          <span>frpc.exe</span>
          <b class="sf-mono" style="font-size: 13px">{{ formatBytes(meta.client_exe_size) }}</b>
        </div>
        <div>
          <span>frps.exe</span>
          <b class="sf-mono" style="font-size: 13px">{{ formatBytes(meta.server_exe_size) }}</b>
        </div>
        <div>
          <span>平台主程序</span>
          <b class="sf-mono" style="font-size: 13px">
            {{ meta.platform_available ? formatBytes(meta.platform_exe_size) : '不可用' }}
          </b>
          <div class="sf-cell-sub">{{ meta.platform_version || '未知版本' }}</div>
        </div>
      </div>

      <el-collapse style="margin-top: 12px">
        <el-collapse-item title="查看二进制 SHA256（将写入包内 SHA256SUMS.txt）" name="hash">
          <div class="sf-cell-sub" style="margin-bottom: 6px">frpc.exe</div>
          <pre class="sf-code" style="max-height: none">{{ meta.client_exe_sha256 || '（未计算）' }}</pre>
          <div class="sf-cell-sub" style="margin: 10px 0 6px">frps.exe</div>
          <pre class="sf-code" style="max-height: none">{{ meta.server_exe_sha256 || '（未计算）' }}</pre>
        </el-collapse-item>
      </el-collapse>
    </el-card>

    <el-card shadow="never">
      <el-tabs v-model="tab">
        <el-tab-pane label="客户端部署包" name="client">
          <div class="sf-stack">
            <div class="sf-row-between">
              <div>
                <div style="font-weight: 600">客户端部署包</div>
                <div class="sf-cell-sub" style="margin-top: 2px">
                  一个包对应一台设备：包内带有唯一的 clientID 与认证 token，并会记入客户端台账。
                </div>
              </div>
              <div class="sf-row">
                <el-button type="primary" @click="openClientDialog">
                  <el-icon><Plus /></el-icon>
                  <span style="margin-left: 4px">新增客户端</span>
                </el-button>
                <el-button link @click="goRegistry">
                  查看客户端台账（已签发 {{ registeredTotal }} 台）
                </el-button>
              </div>
            </div>

            <el-card v-loading="loadingRecent" shadow="never">
              <template #header>
                <div class="sf-row-between">
                  <span style="font-weight: 600">最近签发的客户端</span>
                  <span class="sf-muted" style="font-size: 12px">
                    点击「新增客户端」填写配置并生成部署包
                  </span>
                </div>
              </template>

              <el-table :data="recent" size="small" border>
                <el-table-column label="显示名称" min-width="150">
                  <template #default="{ row }">
                    <div>{{ row.display_name || '—' }}</div>
                    <div v-if="row.note" class="sf-cell-sub">{{ row.note }}</div>
                  </template>
                </el-table-column>
                <el-table-column label="机器人类型" min-width="130">
                  <template #default="{ row }">
                    <el-tag v-if="row.robot_type" size="small" effect="plain">
                      {{ robotTypeLabel(row.robot_type) }}
                    </el-tag>
                    <span v-else class="sf-cell-sub">未分类</span>
                  </template>
                </el-table-column>
                <el-table-column prop="client_id" label="clientID" min-width="170" show-overflow-tooltip>
                  <template #default="{ row }">
                    <span class="sf-mono">{{ row.client_id }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="占用的远端端口" min-width="150">
                  <template #default="{ row }">
                    <div class="sf-row" style="flex-wrap: wrap; gap: 4px">
                      <el-tag
                        v-for="p in occupiedPorts(row)"
                        :key="p"
                        size="small"
                        effect="plain"
                        :type="row.status === 'active' ? 'primary' : 'info'"
                      >
                        <span class="sf-mono">{{ p }}</span>
                      </el-tag>
                      <span v-if="!occupiedPorts(row).length" class="sf-cell-sub">不占用</span>
                    </div>
                  </template>
                </el-table-column>
                <el-table-column label="状态" min-width="80">
                  <template #default="{ row }">
                    <el-tag size="small" effect="light" :type="statusTagType(row.status)">
                      {{ statusLabel(row.status) }}
                    </el-tag>
                  </template>
                </el-table-column>
                <template #empty>
                  <div class="sf-muted" style="padding: 14px 0">
                    还没有签发过客户端部署包。点击右上角「新增客户端」开始。
                  </div>
                </template>
              </el-table>

              <div v-if="registeredTotal > recent.length" class="sf-cell-sub" style="margin-top: 8px">
                另有 {{ registeredTotal - recent.length }} 台已签发客户端未在此列出，请到「客户端台账」查看。
              </div>
            </el-card>
          </div>
        </el-tab-pane>
        <el-tab-pane label="服务器部署包" name="server">
          <div class="sf-stack">
            <div class="sf-row-between">
              <div>
                <div style="font-weight: 600">服务器部署包</div>
                <div class="sf-cell-sub" style="margin-top: 2px">
                  一台 frps 只需一个包：包内带有 bind 端口、认证 token 与 Dashboard 凭据。
                </div>
              </div>
              <div class="sf-row">
                <el-button type="primary" @click="openServerDialog">
                  <el-icon><Plus /></el-icon>
                  <span style="margin-left: 4px">新增服务器部署包</span>
                </el-button>
                <el-button link @click="goRecords">
                  查看全部生成记录
                </el-button>
              </div>
            </div>

            <el-card v-loading="loadingRecentServer" shadow="never">
              <template #header>
                <div class="sf-row-between">
                  <span style="font-weight: 600">最近生成的服务器部署包</span>
                  <span class="sf-muted" style="font-size: 12px">
                    点击「新增服务器部署包」填写配置并生成部署包
                  </span>
                </div>
              </template>

              <el-table :data="recentServer" size="small" border>
                <el-table-column label="生成时间" min-width="170">
                  <template #default="{ row }">{{ formatDateTime(row.at) }}</template>
                </el-table-column>
                <el-table-column label="摘要" min-width="300" show-overflow-tooltip>
                  <template #default="{ row }">
                    <span>{{ row.summary || '—' }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="FRP 版本" min-width="100">
                  <template #default="{ row }">
                    <span class="sf-mono">{{ row.frp_version || '—' }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="操作人" min-width="110">
                  <template #default="{ row }">{{ row.actor || '—' }}</template>
                </el-table-column>
                <template #empty>
                  <div class="sf-muted" style="padding: 14px 0">
                    还没有生成过服务器部署包。点击右上角「新增服务器部署包」开始。
                  </div>
                </template>
              </el-table>

              <div class="sf-cell-sub" style="margin-top: 8px">
                历史 ZIP 从不落盘，服务端也不保留当时的密钥，因此无法重新下载旧包 ——
                需要再装一台 frps 时请重新生成一个。
              </div>
            </el-card>
          </div>
        </el-tab-pane>
        <el-tab-pane label="管理平台自身" name="platform">
          <PlatformPackageCard :meta="meta" />
        </el-tab-pane>
      </el-tabs>
    </el-card>

    <!--
      新增客户端弹窗。
      destroy-on-close 是这里的关键：关闭即销毁表单，下次打开时组件重新挂载、
      重新向服务端取一份默认值，于是每次「新增客户端」都天然是一个新的 clientID。
      若改成常驻（或保留内容），管理员很容易在上一次填写的结果上继续修改，
      最后连着发出两个身份几乎相同的包 —— 而它们会被当成同一台设备互相顶替。
      close-on-click-modal 关掉：表单内容较多，误点空白处丢失填写代价太大。
    -->
    <el-dialog
      v-model="clientDialog"
      :title="repackDraft ? `重新生成「${repackName}」的部署包` : '新增客户端部署包'"
      width="min(1180px, 94vw)"
      top="4vh"
      :close-on-click-modal="false"
      destroy-on-close
      @closed="repackDraft = null"
    >
      <div style="max-height: 74vh; overflow-y: auto; padding-right: 4px">
        <ClientPackageForm :meta="meta" :draft="repackDraft" @generated="onClientGenerated" />
      </div>
    </el-dialog>

    <!--
      新增服务器部署包弹窗。与上面客户端弹窗同形同参数：
      destroy-on-close、关掉 click-modal、同样的 74vh 滚动区。
      唯一的差别是它不需要「每次都是新身份」—— 服务器包没有 clientID 概念，
      用 destroy-on-close 是为了让每次打开都拿到一份全新的默认密钥，
      而不是让管理员在上一次填的内容上继续改。
    -->
    <el-dialog
      v-model="serverDialog"
      title="新增服务器部署包"
      width="min(1180px, 94vw)"
      top="4vh"
      :close-on-click-modal="false"
      destroy-on-close
    >
      <div style="max-height: 74vh; overflow-y: auto; padding-right: 4px">
        <ServerPackageForm :meta="meta" @generated="onServerGenerated" />
      </div>
    </el-dialog>
  </div>
</template>
