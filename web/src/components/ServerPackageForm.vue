<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError, saveBlob } from '@/api/http'
import type { PkgMeta, PkgPreview, ServerSpec } from '@/api/types'
import { LOG_LEVELS } from '@/api/types'
import { formatBytes } from '@/utils/format'
import SecretField from './SecretField.vue'

// 服务器配置表单与部署包生成（F11 / F12 / F15 / F17）。

const props = defineProps<{ meta: PkgMeta | null }>()

/**
 * 与客户端表单同形：只在真正拿到 ZIP 之后才发这个事件，由外层决定何时关弹窗。
 *
 * 为什么不能「点了下载就关」：生成接口可能因配置非法返回 400，
 * 此时表单里要留着报错让管理员改，弹窗消失等于把错误信息一起吞掉。
 */
const emit = defineEmits<{ generated: [fileName: string] }>()

const formRef = ref<FormInstance>()
const spec = ref<ServerSpec | null>(null)
const loading = ref(false)
const previewing = ref(false)
const downloading = ref(false)
const preview = ref<PkgPreview | null>(null)
const errMsg = ref('')

/**
 * 兜住「数组字段返回 null」这一类破坏性响应。
 *
 * 服务端已经保证切片序列化为 []（见 internal/pack 的 emptyIfNil），这里是第二道防线：
 * 模板与按钮回调都直接对 allow_ports 解引用，一旦它是 null，
 * 不只是这个面板崩，整个部署包页都会渲染失败——而 el-tabs 默认把两个面板都渲染出来，
 * 隐藏的服务器面板照样能把页面带崩。
 */
function normalize(s: ServerSpec): ServerSpec {
  return { ...s, allow_ports: s.allow_ports ?? [] }
}

async function loadDefaults() {
  loading.value = true
  errMsg.value = ''
  try {
    spec.value = normalize(await api.serverDefaults())
    preview.value = null
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '加载默认值失败'
  } finally {
    loading.value = false
  }
}

onMounted(loadDefaults)

const rules: FormRules = {
  bind_addr: [{ required: true, message: '请填写监听地址', trigger: 'blur' }],
  token: [{ required: true, message: '认证 token 不能为空', trigger: 'blur' }],
  dashboard_user: [{ required: true, message: 'Dashboard 用户名不能为空', trigger: 'blur' }],
  dashboard_password: [
    { required: true, message: 'Dashboard 密码不能为空', trigger: 'blur' },
    { min: 12, message: 'Dashboard 密码至少 12 位', trigger: 'blur' },
  ],
}

const exeSizeText = computed(() =>
  props.meta ? `${formatBytes(props.meta.server_exe_size)}（frps.exe）` : '—',
)

function addRange() {
  spec.value?.allow_ports.push({ start: 0, end: 0 })
  preview.value = null
}

function removeRange(idx: number) {
  spec.value?.allow_ports.splice(idx, 1)
  preview.value = null
}

async function doPreview() {
  const s = spec.value
  if (!s) return
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  previewing.value = true
  errMsg.value = ''
  try {
    preview.value = await api.serverPreview(s)
    ElMessage.success('配置校验通过（已由 frps verify -c 确认）')
  } catch (err) {
    preview.value = null
    errMsg.value = err instanceof HttpError ? err.message : '预览失败'
  } finally {
    previewing.value = false
  }
}

async function doDownload() {
  const s = spec.value
  if (!s) return
  const ok = await formRef.value?.validate().catch(() => false)
  if (!ok) return

  downloading.value = true
  errMsg.value = ''
  try {
    const r = await api.serverDownload(s)
    saveBlob(r.blob, r.fileName)
    ElMessage.success(`已生成并开始下载：${r.fileName}`)
    // 通知外层：这次「新增」已经完成，由外层决定是否关闭弹窗并刷新列表。
    emit('generated', r.fileName)
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '生成失败'
  } finally {
    downloading.value = false
  }
}

/** token 已经改了意味着已部署的客户端会集体失联（01-prd P-3）。 */
const tokenRotateNotice =
  '这里的 token / Dashboard 密码在默认值中由服务端随机生成。若服务器已上线，改动 token 会让所有已部署客户端立即无法登录，请仅在明确要轮换密钥时修改。'
</script>

<template>
  <div v-loading="loading" class="sf-stack">
    <el-alert v-if="errMsg" :title="errMsg" type="error" show-icon :closable="false" />

    <el-alert type="warning" show-icon :closable="false" :title="tokenRotateNotice" />

    <el-card v-if="spec" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">监听与认证</span>
          <span class="sf-muted" style="font-size: 12px">包内将包含 {{ exeSizeText }}</span>
        </div>
      </template>

      <el-form ref="formRef" :model="spec" :rules="rules" label-position="top">
        <div class="sf-form-grid">
          <el-form-item label="bind 监听地址" prop="bind_addr">
            <el-input v-model="spec.bind_addr" placeholder="0.0.0.0" spellcheck="false" />
          </el-form-item>
          <el-form-item label="bind 端口" prop="bind_port">
            <el-input-number v-model="spec.bind_port" :min="1" :max="65535" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="认证 token" prop="token">
            <SecretField v-model="spec.token" hint="客户端 frpc.toml 中的 token 必须与此处一致。" />
          </el-form-item>
        </div>

        <el-divider content-position="left">Dashboard（管理平台据此读取状态）</el-divider>

        <div class="sf-form-grid">
          <el-form-item label="Dashboard 监听地址">
            <el-input v-model="spec.dashboard_addr" spellcheck="false" />
            <div class="sf-cell-sub" style="margin-top: 4px">
              建议保持 127.0.0.1。改为对外地址会让 Dashboard 端口暴露在网络中。
            </div>
          </el-form-item>
          <el-form-item label="Dashboard 端口">
            <el-input-number
              v-model="spec.dashboard_port"
              :min="1"
              :max="65535"
              controls-position="right"
              style="width: 100%"
            />
          </el-form-item>
          <el-form-item label="Dashboard 用户名" prop="dashboard_user">
            <el-input v-model="spec.dashboard_user" spellcheck="false" />
          </el-form-item>
          <el-form-item label="Dashboard 密码" prop="dashboard_password">
            <SecretField v-model="spec.dashboard_password" hint="至少 12 位。该凭据供管理平台采集状态使用。" />
          </el-form-item>
        </div>

        <el-divider content-position="left">日志与端口策略</el-divider>

        <div class="sf-form-grid">
          <el-form-item label="日志级别">
            <el-select v-model="spec.log_level" style="width: 100%">
              <el-option v-for="l in LOG_LEVELS" :key="l" :value="l" :label="l" />
            </el-select>
          </el-form-item>
          <el-form-item label="日志保留天数">
            <el-input-number v-model="spec.log_max_days" :min="1" :max="365" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="子域后缀">
            <el-input v-model="spec.subdomain_host" placeholder="例如 frp.example.com（可留空）" spellcheck="false" />
          </el-form-item>
        </div>
      </el-form>
    </el-card>

    <el-card v-if="spec" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">允许的远端端口区间（{{ spec.allow_ports.length }}）</span>
          <el-button type="primary" plain size="small" @click="addRange">
            <el-icon><Plus /></el-icon>
            <span style="margin-left: 4px">添加区间</span>
          </el-button>
        </div>
      </template>

      <el-table :data="spec.allow_ports" size="small" border>
        <el-table-column label="起始端口" width="160">
          <template #default="{ row }">
            <el-input-number v-model="row.start" size="small" :min="1" :max="65535" :controls="false" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="结束端口" width="160">
          <template #default="{ row }">
            <el-input-number v-model="row.end" size="small" :min="1" :max="65535" :controls="false" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="说明">
          <template #default>
            <span class="sf-cell-sub">
              客户端可申请的远端端口范围；留空表示不限制（安全性由 token 保证）。
            </span>
          </template>
        </el-table-column>
        <el-table-column label="" width="64" align="center">
          <template #default="{ $index }">
            <el-button link type="danger" size="small" @click="removeRange($index)">
              <el-icon><Delete /></el-icon>
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 14px 0">未限制可申请端口</div>
        </template>
      </el-table>
    </el-card>

    <div v-if="spec" class="sf-row">
      <el-button :loading="previewing" @click="doPreview">
        <el-icon><View /></el-icon>
        <span style="margin-left: 4px">校验并预览</span>
      </el-button>
      <el-button type="primary" :loading="downloading" @click="doDownload">
        <el-icon><Download /></el-icon>
        <span style="margin-left: 4px">生成并下载 ZIP</span>
      </el-button>
      <el-button link @click="loadDefaults">重新随机生成密钥</el-button>
    </div>

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
        v-for="(w, i) in preview.warnings"
        :key="i"
        :title="w"
        type="warning"
        show-icon
        :closable="false"
        style="margin-bottom: 8px"
      />

      <div class="sf-cell-sub" style="margin-bottom: 8px">
        以下内容中的 token 与 Dashboard 密码已被遮蔽；实际写入 ZIP 的是明文（frps 需要能直接运行）。
      </div>
      <pre class="sf-code">{{ preview.toml }}</pre>

      <el-divider content-position="left">包内文件清单</el-divider>
      <div class="sf-row" style="flex-wrap: wrap; gap: 6px">
        <el-tag v-for="f in preview.files" :key="f" size="small" type="info" effect="plain">{{ f }}</el-tag>
      </div>
    </el-card>
  </div>
</template>
