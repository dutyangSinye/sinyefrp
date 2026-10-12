<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError, saveBlob } from '@/api/http'
import type { PkgMeta, PkgPreview } from '@/api/types'
import { formatBytes } from '@/utils/format'

// 管理平台自身部署包（01-prd F18）。
//
// 与另外两个包最大的不同：这里没有表单。平台自身的监听地址、节点地址都取自
// 当前部署，管理员唯一要做的事是「确认即将发出去的是什么」然后下载 ——
// 所以本组件把配置全文摊开，而不是给一堆输入框。

const props = defineProps<{ meta: PkgMeta | null }>()

const preview = ref<PkgPreview | null>(null)
const loading = ref(false)
const downloading = ref(false)
const errMsg = ref('')

async function loadPreview() {
  loading.value = true
  errMsg.value = ''
  try {
    preview.value = await api.platformPreview()
  } catch (err) {
    // 失败时清掉旧预览：留着一份过期的配置全文，比什么都不显示更容易误导。
    preview.value = null
    errMsg.value = err instanceof HttpError ? err.message : '读取平台部署包信息失败'
  } finally {
    loading.value = false
  }
}

async function doDownload() {
  downloading.value = true
  try {
    const r = await api.platformDownload()
    saveBlob(r.blob, r.fileName)
    ElMessage.success(`已生成并开始下载：${r.fileName}`)
  } catch (err) {
    ElMessage.error(err instanceof HttpError ? err.message : '生成部署包失败')
  } finally {
    downloading.value = false
  }
}

onMounted(loadPreview)
</script>

<template>
  <div class="sf-stack">
    <el-alert
      v-if="props.meta && !props.meta.platform_available"
      type="error"
      show-icon
      :closable="false"
      title="无法读取管理平台自身的可执行文件，暂不能生成平台部署包"
      :description="
        props.meta.platform_unavailable_reason ||
        '请确认程序目录下的 sinyefrp.exe 存在且可读（安全软件可能锁住了它）。'
      "
    />

    <el-alert v-if="errMsg" :title="errMsg" type="error" show-icon :closable="false" />

    <div class="sf-row-between">
      <p class="sf-page-sub" style="margin: 0; max-width: 760px">
        包内含一份<strong>当前正在运行的主程序</strong>，以及开机自启所需的生命周期脚本。
        解压到目标 Windows 主机后，<strong>双击包内的「一键安装.bat」</strong>即可安装为 SYSTEM 计划任务：它会自动请求管理员权限，不需要你打开 PowerShell，也不受脚本执行策略影响。命令行方式仍然可用，以管理员身份执行 <code>.\install.ps1</code> 效果完全相同。
      </p>
      <el-button :loading="loading" @click="loadPreview">
        <el-icon><Refresh /></el-icon>
        <span style="margin-left: 4px">刷新</span>
      </el-button>
    </div>

    <el-card v-if="preview" v-loading="loading" shadow="never">
      <div class="sf-kv">
        <div>
          <span>平台版本</span>
          <b class="sf-mono">{{ props.meta?.platform_version || '未知' }}</b>
          <div class="sf-cell-sub">与当前界面同一版本</div>
        </div>
        <div>
          <span>主程序</span>
          <b class="sf-mono" style="font-size: 13px">{{ formatBytes(props.meta?.platform_exe_size ?? 0) }}</b>
          <div class="sf-cell-sub">sinyefrp.exe（已内嵌前端与 FRP 二进制）</div>
        </div>
        <div>
          <span>包内文件</span>
          <b>{{ preview.files?.length ?? 0 }} 个</b>
          <div class="sf-cell-sub">
            含 {{ (preview?.files ?? []).filter((f) => /\.(ps1|bat)$/i.test(f)).length }} 个脚本（.bat 即双击入口）
          </div>
        </div>
        <div>
          <span>解压后总大小</span>
          <b class="sf-mono" style="font-size: 13px">{{ formatBytes(preview.size_bytes) }}</b>
          <div class="sf-cell-sub">下载的是 ZIP 压缩包</div>
        </div>
      </div>

      <el-collapse style="margin-top: 12px">
        <el-collapse-item title="查看主程序 SHA256（将写入包内 SHA256SUMS.txt）" name="hash">
          <pre class="sf-code" style="max-height: none">{{ props.meta?.platform_exe_sha256 || '（未计算）' }}</pre>
        </el-collapse-item>
      </el-collapse>

      <div class="sf-row" style="margin-top: 14px">
        <el-button
          type="primary"
          :loading="downloading"
          :disabled="props.meta ? !props.meta.platform_available : false"
          @click="doDownload"
        >
          <el-icon><Download /></el-icon>
          <span style="margin-left: 4px">生成并下载 ZIP</span>
        </el-button>
      </div>
    </el-card>

    <!-- 预览结果：平台配置没有敏感字段，这里给的是写入 ZIP 的原文 -->
    <el-card v-if="preview" shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">包内 config.toml · {{ preview.file_name }}</span>
          <span class="sf-muted" style="font-size: 12px">{{ preview.summary }}</span>
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
        以下就是 ZIP 内的配置文件原文。平台配置里没有 token 与密码，因此这里不需要遮蔽。
      </div>
      <pre class="sf-code">{{ preview.toml }}</pre>

      <el-divider content-position="left">包内文件清单</el-divider>
      <div class="sf-row" style="flex-wrap: wrap; gap: 6px">
        <el-tag v-for="f in preview.files" :key="f" size="small" type="info" effect="plain">{{ f }}</el-tag>
      </div>
    </el-card>
  </div>
</template>
