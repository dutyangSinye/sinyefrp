<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '@/api/endpoints'
import { HttpError } from '@/api/http'
import type { AuditItem, PkgRecord } from '@/api/types'
import { formatDateTime } from '@/utils/format'

// 生成记录与审计（F16）。
//
// 两个表刻意分开，因为它们回答不同的问题：
//   - 生成记录：业务台账，「发出去过哪些包」，只含成功；
//   - 操作审计：安全流水，「谁在什么时候做了什么」，包含失败尝试。
// 页面也明确提示「无法重新下载历史包」——ZIP 从未落盘，服务端也不保留当时的密钥。

const records = ref<PkgRecord[]>([])
const audit = ref<AuditItem[]>([])
const loading = ref(false)
const errMsg = ref('')

async function load() {
  loading.value = true
  errMsg.value = ''
  try {
    const [r, a] = await Promise.all([api.pkgRecords(), api.audit(200)])
    records.value = r.records ?? []
    audit.value = a.items ?? []
  } catch (err) {
    errMsg.value = err instanceof HttpError ? err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(load)

const KIND_LABEL: Record<string, string> = {
  client: '客户端包',
  server: '服务器包',
}

const RESULT_LABEL: Record<string, string> = {
  success: '成功',
  ok: '成功',
  error: '失败',
  fail: '失败',
}

function resultType(r: string): 'success' | 'danger' | 'info' {
  if (r === 'success' || r === 'ok') return 'success'
  if (r === 'error' || r === 'fail') return 'danger'
  return 'info'
}

function shortHash(h: string): string {
  if (!h) return '—'
  return h.length > 16 ? `${h.slice(0, 16)}…` : h
}

async function copyHash(h: string) {
  if (!h) return
  try {
    await navigator.clipboard.writeText(h)
    ElMessage.success('已复制完整哈希')
  } catch {
    ElMessage.error('复制失败：浏览器拒绝了剪贴板访问')
  }
}
</script>

<template>
  <div class="sf-stack">
    <div class="sf-row-between">
      <div>
        <h2 class="sf-page-title">生成记录</h2>
        <p class="sf-page-sub" style="margin: 0">
          部署包含明文密钥，服务端只即时生成并流式下载，不落盘保存，因此
          <strong>无法重新下载历史包</strong>；记录仅保留非敏感摘要。
        </p>
      </div>
      <el-button :loading="loading" @click="load">
        <el-icon><Refresh /></el-icon>
        <span style="margin-left: 4px">刷新</span>
      </el-button>
    </div>

    <el-alert v-if="errMsg" :title="errMsg" type="error" show-icon :closable="false" />

    <el-card shadow="never">
      <template #header>
        <span style="font-weight: 600">已生成的部署包（{{ records.length }}）</span>
      </template>

      <el-table v-loading="loading" :data="records" size="small" border>
        <el-table-column label="时间" min-width="170">
          <template #default="{ row }">{{ formatDateTime(row.at) }}</template>
        </el-table-column>
        <el-table-column label="类型" min-width="100">
          <template #default="{ row }">
            <el-tag size="small" effect="plain" :type="row.kind === 'client' ? 'primary' : 'warning'">
              {{ KIND_LABEL[row.kind] || row.kind }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="display_name" label="名称" min-width="150" show-overflow-tooltip>
          <template #default="{ row }">{{ row.display_name || '—' }}</template>
        </el-table-column>
        <el-table-column prop="client_id" label="clientID" min-width="150" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.client_id || '—' }}</span></template>
        </el-table-column>
        <el-table-column prop="frp_version" label="FRP 版本" min-width="100">
          <template #default="{ row }"><span class="sf-mono">{{ row.frp_version || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="frpc/frps SHA256" min-width="180">
          <template #default="{ row }">
            <el-tooltip v-if="row.exe_sha256" :content="row.exe_sha256" placement="top">
              <el-button link type="primary" size="small" class="sf-mono" @click="copyHash(row.exe_sha256)">
                {{ shortHash(row.exe_sha256) }}
              </el-button>
            </el-tooltip>
            <span v-else class="sf-cell-sub">—</span>
          </template>
        </el-table-column>
        <el-table-column prop="actor" label="操作人" min-width="110">
          <template #default="{ row }">{{ row.actor || '—' }}</template>
        </el-table-column>
        <el-table-column prop="summary" label="摘要" min-width="200" show-overflow-tooltip />
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">还没有生成过部署包</div>
        </template>
      </el-table>
    </el-card>

    <el-card shadow="never">
      <template #header>
        <div class="sf-row-between">
          <span style="font-weight: 600">操作审计（最近 {{ audit.length }} 条）</span>
          <span class="sf-muted" style="font-size: 12px">含失败的打包尝试；不记录任何明文密钥</span>
        </div>
      </template>

      <el-table v-loading="loading" :data="audit" size="small" border>
        <el-table-column label="时间" min-width="170">
          <template #default="{ row }">{{ formatDateTime(row.at) }}</template>
        </el-table-column>
        <el-table-column prop="actor" label="操作人" min-width="110">
          <template #default="{ row }">{{ row.actor || '—' }}</template>
        </el-table-column>
        <el-table-column label="动作" min-width="140">
          <template #default="{ row }"><span class="sf-mono">{{ row.action }}</span></template>
        </el-table-column>
        <el-table-column prop="target" label="目标" min-width="140" show-overflow-tooltip>
          <template #default="{ row }"><span class="sf-mono">{{ row.target || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="结果" min-width="86">
          <template #default="{ row }">
            <el-tag size="small" effect="light" :type="resultType(row.result)">
              {{ RESULT_LABEL[row.result] || row.result || '—' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="client_ip" label="来源 IP" min-width="130">
          <template #default="{ row }"><span class="sf-mono">{{ row.client_ip || '—' }}</span></template>
        </el-table-column>
        <el-table-column prop="detail" label="详情" min-width="240" show-overflow-tooltip>
          <template #default="{ row }">{{ row.detail || '—' }}</template>
        </el-table-column>
        <template #empty>
          <div class="sf-muted" style="padding: 18px 0">暂无审计记录</div>
        </template>
      </el-table>
    </el-card>
  </div>
</template>
