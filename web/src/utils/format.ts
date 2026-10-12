// 格式化工具。
//
// 注意时间有两种来源，不可混用（见 docs/v1.0/03-frp-api-contract.md §4）：
//   - 客户端：Unix 秒级时间戳（firstConnectedAt / lastConnectedAt / disconnectedAt）
//   - 代理：形如 "09-21 14:35:54" 的字符串，且没有年份
//   - 平台自身时间（采集时间、审计时间）：RFC3339 字符串
// 三者分别由 formatUnixSec / formatProxyTime / formatDateTime 处理。

import type { Health } from '@/api/types'

/** 字节数转可读单位。用于流量显示。 */
export function formatBytes(v: number | null | undefined): string {
  if (v === null || v === undefined || Number.isNaN(v)) return '—'
  if (v === 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let n = Math.abs(v)
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  const sign = v < 0 ? '-' : ''
  return `${sign}${n.toFixed(i === 0 ? 0 : n >= 100 ? 0 : 1)} ${units[i]}`
}

/** Unix 秒 → 本地时间字符串。0 或空值返回占位符。 */
export function formatUnixSec(v: number | null | undefined): string {
  if (!v) return '—'
  // FRP 侧为秒；乘 1000 转毫秒。
  return new Date(v * 1000).toLocaleString()
}

/** RFC3339 / ISO → 本地时间字符串。 */
export function formatDateTime(v: string | null | undefined): string {
  if (!v) return '—'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString()
}

/** 距现在多久，用于「最后成功采集」。 */
export function formatAgo(v: string | null | undefined): string {
  if (!v) return '从未'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return '—'
  const sec = Math.round((Date.now() - d.getTime()) / 1000)
  if (sec < 0) return '刚刚'
  if (sec < 60) return `${sec} 秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`
  return `${Math.floor(sec / 86400)} 天前`
}

/** 代理的 "MM-DD HH:mm:ss" 原样展示；空值给占位符。 */
export function formatProxyTime(v: string | null | undefined): string {
  const s = (v || '').trim()
  return s === '' ? '—' : s
}

/** 把秒数转成简短的中文时长（如会话有效期）。 */
export function formatDuration(seconds: number): string {
  if (!seconds || seconds <= 0) return '—'
  if (seconds < 60) return `${seconds} 秒`
  if (seconds < 3600) return `${Math.round(seconds / 60)} 分钟`
  if (seconds < 86400) return `${(seconds / 3600).toFixed(seconds % 3600 === 0 ? 0 : 1)} 小时`
  return `${(seconds / 86400).toFixed(1)} 天`
}

export type HealthLevel = 'unknown' | 'healthy' | 'stale' | 'error'

/**
 * 把采集健康度映射为一个等级。
 *
 * 关键点：known=false 必须落到 'unknown'，绝不能落到 'error' 或让调用方
 * 当成「没有客户端」—— 这是 01-prd §8.4 / KR5 的硬要求。
 */
export function healthLevel(h: Health | null | undefined): HealthLevel {
  if (!h || !h.known) return 'unknown'
  if (h.healthy) return 'healthy'
  return 'error'
}

export interface HealthDesc {
  level: HealthLevel
  /** 标签颜色类型（Element Plus）。 */
  type: 'success' | 'warning' | 'danger' | 'info'
  label: string
  /** 详细说明，通常包含错误分类与最后成功时间。 */
  detail: string
}

const ERROR_KIND_LABEL: Record<string, string> = {
  unreachable: '无法连接',
  timeout: '连接超时',
  unauthorized: '认证失败',
  not_found: '接口或资源不存在',
  server_error: 'FRP 服务端错误',
  bad_response: '响应格式异常',
  incompatible: '版本不兼容',
  not_initialized: '尚未初始化',
  rate_limited: '请求过于频繁',
  bad_request: '请求不合法',
  unavailable: '数据不可用',
  internal: '内部错误',
}

/** 错误分类的中文说明，缺省回退到原值。 */
export function errorKindLabel(kind: string | undefined): string {
  if (!kind) return ''
  return ERROR_KIND_LABEL[kind] || kind
}

/** 生成健康度的展示文案。 */
export function describeHealth(h: Health | null | undefined): HealthDesc {
  const level = healthLevel(h)
  if (level === 'unknown') {
    return {
      level,
      type: 'info',
      label: '状态未知',
      detail: h?.message
        ? `尚未成功采集：${errorKindLabel(h.error_kind) || h.message}`
        : '尚未成功采集，无法判断客户端在线状态',
    }
  }
  if (level === 'healthy') {
    const staleNote = h?.stale ? '（数据已过期）' : ''
    return {
      level: h?.stale ? 'stale' : 'healthy',
      type: h?.stale ? 'warning' : 'success',
      label: h?.stale ? '数据过期' : '正常',
      detail: `最后成功采集：${formatAgo(h?.last_success)}${staleNote}`,
    }
  }
  return {
    level,
    type: 'danger',
    label: '采集异常',
    detail: `${
      errorKindLabel(h?.error_kind) || '采集失败'
    }${h?.message ? `：${h.message}` : ''}（已连续失败 ${h?.fail_streak ?? 0} 次，最后成功：${formatAgo(
      h?.last_success,
    )}）`,
  }
}
