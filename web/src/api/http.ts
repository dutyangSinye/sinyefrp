import type { ApiErrorBody } from './types'

// 统一的 HTTP 层。
//
// 三条从后端实现推导出来的约定，不能想当然：
//
// 1. 会话靠 HttpOnly Cookie，因此必须 credentials: 'same-origin'；
//    后端不下发任何 CORS 头，跨源请求会被浏览器直接拦下。
// 2. 写接口必须是 application/json，否则后端返回 415 —— 它同时充当
//    轻量 CSRF 防护（跨站表单发不出 application/json）。
// 3. 未初始化返回 409 not_initialized，未登录返回 401 unauthorized。
//    这两种不是「错误」，而是两种不同的界面引导，需分别处理。

export class HttpError extends Error {
  readonly kind: string
  readonly status: number
  /**
   * 后端下发的逐条细节（例如每一条端口冲突的说明）；没有时是空数组。
   *
   * 默认是空数组而不是 undefined：调用方几乎总要遍历它，
   * 每次都得写 `err.details ?? []` 只会让遗漏变成运行时崩溃。
   */
  readonly details: string[]

  constructor(kind: string, message: string, status: number, details: string[] = []) {
    super(message)
    this.name = 'HttpError'
    this.kind = kind
    this.status = status
    this.details = details
  }
}

export interface RawResult<T> {
  status: number
  ok: boolean
  data: T | null
  error: ApiErrorBody | null
}

/** 未登录（401）时的回调，由会话层注册，用于跳回登录页。 */
let unauthorizedHandler: (() => void) | null = null
export function onUnauthorized(fn: () => void): void {
  unauthorizedHandler = fn
}

async function parseBody(res: Response): Promise<unknown> {
  const text = await res.text()
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    return { error: { kind: 'bad_response', message: text.slice(0, 300) } }
  }
}

function extractError(body: unknown, status: number): ApiErrorBody {
  const b = body as { error?: ApiErrorBody } | null
  if (b && b.error && typeof b.error.message === 'string') {
    return {
      kind: b.error.kind || 'internal',
      message: b.error.message,
      details: Array.isArray(b.error.details) ? b.error.details : [],
    }
  }
  return { kind: 'internal', message: `请求失败（HTTP ${status}）`, details: [] }
}

/** 发一次请求并保留状态码与错误体，不做抛错。 */
export async function raw<T>(method: string, path: string, body?: unknown): Promise<RawResult<T>> {
  const init: RequestInit = {
    method,
    credentials: 'same-origin',
    headers: {},
  }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }

  let res: Response
  try {
    res = await fetch(path, init)
  } catch {
    return {
      status: 0,
      ok: false,
      data: null,
      error: { kind: 'unreachable', message: '无法连接管理服务，请确认它正在运行' },
    }
  }

  const parsed = await parseBody(res)

  if (res.status === 401 && unauthorizedHandler) {
    unauthorizedHandler()
  }

  if (!res.ok) {
    return { status: res.status, ok: false, data: null, error: extractError(parsed, res.status) }
  }
  return { status: res.status, ok: true, data: parsed as T, error: null }
}

/** 发一次请求，失败即抛 HttpError。 */
export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const r = await raw<T>(method, path, body)
  if (!r.ok) {
    const e = r.error ?? { kind: 'internal', message: '请求失败', details: [] }
    throw new HttpError(e.kind, e.message, r.status, e.details ?? [])
  }
  return r.data as T
}

/** 宽松请求：503 等「带正文的非 2xx」也返回数据，供总览/客户端页展示健康度。 */
export async function soft<T>(method: string, path: string, body?: unknown): Promise<RawResult<T>> {
  return raw<T>(method, path, body)
}

export const http = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  // DELETE 不带正文：后端的删除接口不解析请求体。跨站 HTML 表单只能发
  // GET/POST，因此这个动词本身不需要额外的 CSRF 处理。
  del: <T>(path: string) => request<T>('DELETE', path),
}

// ---------- 文件下载 ----------

export interface DownloadResult {
  blob: Blob
  fileName: string
}

/** 从 Content-Disposition 中取出文件名，取不到时回退到默认名。 */
function fileNameFrom(res: Response, fallback: string): string {
  const cd = res.headers.get('Content-Disposition') || ''
  const star = /filename\*=UTF-8''([^;]+)/i.exec(cd)
  if (star) {
    try {
      return decodeURIComponent(star[1])
    } catch {
      /* 落到下一个分支 */
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(cd)
  return plain ? plain[1] : fallback
}

/**
 * POST 并下载返回的 ZIP。
 *
 * 不能复用 request()：失败时后端返回的是 JSON 错误体而不是 ZIP，
 * 若直接当成 blob 下载，用户会得到一个内容是错误信息的「zip 文件」。
 * 因此这里先看 Content-Type，是 JSON 就按错误处理。
 */
export async function postDownload(path: string, body: unknown, fallbackName: string): Promise<DownloadResult> {
  let res: Response
  try {
    res = await fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
  } catch {
    throw new HttpError('unreachable', '无法连接管理服务，请确认它正在运行', 0)
  }

  const contentType = res.headers.get('Content-Type') || ''

  if (!res.ok || contentType.includes('application/json')) {
    const parsed = await parseBody(res)
    const e = extractError(parsed, res.status)
    if (res.status === 401 && unauthorizedHandler) unauthorizedHandler()
    throw new HttpError(e.kind, e.message, res.status, e.details ?? [])
  }

  return { blob: await res.blob(), fileName: fileNameFrom(res, fallbackName) }
}

/** 触发浏览器下载一个 Blob。 */
export function saveBlob(blob: Blob, fileName: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = fileName
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  // 立即 revoke 在部分浏览器上会中断下载，延后一帧更稳。
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
