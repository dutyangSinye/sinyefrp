import { http, postDownload, raw, soft, type RawResult, type DownloadResult } from './http'
import type {
  AuditItem,
  ClientDetailResponse,
  ClientLinksResponse,
  ClientsResponse,
  ClientSpec,
  DataStatusResponse,
  EndpointConfig,
  EndpointSaveResponse,
  MeResponse,
  NodeConfigResponse,
  NodeSaveResponse,
  NodeTestResult,
  OverviewResponse,
  PkgMeta,
  PkgPreview,
  PkgRecord,
  ProxiesResponse,
  RefreshResponse,
  RegistryClient,
  RegistryClientsResponse,
  RegistryPortsResponse,
  RegistrySpecResponse,
  ServerSpec,
  SessionResponse,
  SetupStatus,
} from './types'

// 全部接口路径集中在此，避免路径字符串散落在各视图里。

export const api = {
  // —— 初始化与登录（公开）——
  setupStatus: () => http.get<SetupStatus>('/api/setup/status'),
  setupAdmin: (username: string, password: string) =>
    http.post<SessionResponse>('/api/setup/admin', { username, password }),
  login: (username: string, password: string) =>
    http.post<SessionResponse>('/api/auth/login', { username, password }),
  me: () => http.get<MeResponse>('/api/auth/me'),
  logout: () => http.post<{ ok: boolean }>('/api/auth/logout', {}),

  // —— 节点配置（F03）——
  nodeConfig: () => http.get<NodeConfigResponse>('/api/node/config'),
  saveNodeConfig: (body: Record<string, unknown>) => http.post<NodeSaveResponse>('/api/node/config', body),
  testNode: (body: Record<string, unknown>) => http.post<NodeTestResult>('/api/node/test', body),
  node: () => http.get<Record<string, unknown>>('/api/node'),

  /** 当前数据库的落盘状态（路径 / 大小 / WAL）。排障「记录没了」的第一站。 */
  dataStatus: () => http.get<DataStatusResponse>('/api/data/status'),

  // —— 只读监控（F04-F07）——
  // 这三个接口在「从未采集成功」时返回 503 + 可用正文（health.known=false）。
  // 因此用 soft，把状态码交给视图决定如何提示，而不是在这里抛错丢掉健康度。
  overview: (): Promise<RawResult<OverviewResponse>> => soft<OverviewResponse>('GET', '/api/overview'),
  clients: (): Promise<RawResult<ClientsResponse>> => soft<ClientsResponse>('GET', '/api/clients'),
  clientDetail: (key: string) =>
    http.get<ClientDetailResponse>(`/api/clients/${encodeURIComponent(key)}`),
  clientLinks: (key: string) =>
    http.get<ClientLinksResponse>(`/api/clients/${encodeURIComponent(key)}/links`),
  proxies: (params: { type?: string; clientId?: string } = {}): Promise<RawResult<ProxiesResponse>> => {
    const q = new URLSearchParams()
    if (params.type) q.set('type', params.type)
    if (params.clientId) q.set('client_id', params.clientId)
    const qs = q.toString()
    return soft<ProxiesResponse>('GET', '/api/proxies' + (qs ? `?${qs}` : ''))
  },
  refresh: () => http.post<RefreshResponse>('/api/refresh', {}),

  // —— 审计与生成记录（F16）——
  audit: (limit = 100) => http.get<{ items: AuditItem[] }>(`/api/audit?limit=${limit}`),
  pkgRecords: () => http.get<{ records: PkgRecord[] }>('/api/pkg/records'),

  // —— 部署包（F09-F17）——
  pkgMeta: () => http.get<PkgMeta>('/api/pkg/meta'),
  pkgEndpoint: () => http.get<EndpointConfig>('/api/pkg/endpoint'),
  savePkgEndpoint: (body: Record<string, unknown>) =>
    http.post<EndpointSaveResponse>('/api/pkg/endpoint', body),
  clientDefaults: () => http.get<ClientSpec>('/api/pkg/client/defaults'),
  serverDefaults: () => http.get<ServerSpec>('/api/pkg/server/defaults'),
  clientPreview: (spec: ClientSpec) => http.post<PkgPreview>('/api/pkg/client/preview', spec),
  serverPreview: (spec: ServerSpec) => http.post<PkgPreview>('/api/pkg/server/preview', spec),
  clientDownload: (spec: ClientSpec): Promise<DownloadResult> =>
    postDownload('/api/pkg/client', spec, `sinyefrp-client-${spec.client_id || 'package'}.zip`),
  serverDownload: (spec: ServerSpec): Promise<DownloadResult> =>
    postDownload('/api/pkg/server', spec, 'sinyefrp-server.zip'),

  // —— 管理平台自身部署包（F18）——
  // 平台包没有可填参数，但下载仍走 POST：与另外两个包保持同一调用形态，
  // 将来若要加参数（例如自定义监听端口）不必改语义。
  platformPreview: () => http.get<PkgPreview>('/api/pkg/platform/preview'),
  platformDownload: (): Promise<DownloadResult> =>
    postDownload('/api/pkg/platform', {}, 'SinyeFRP-platform.zip'),

  // —— 客户端台账与端口占用（F19）——
  //
  // 两个接口刻意分开：台账回答「我们签发过什么」，端口回答「哪个端口归谁」。
  // 客户端表单只需要后者（判断填的端口有没有被占），不需要把备注之类的
  // 台账内容拉进打包页。
  registryClients: () => http.get<RegistryClientsResponse>('/api/registry/clients'),
  registryPorts: () => http.get<RegistryPortsResponse>('/api/registry/ports'),
  /**
   * 手工补录台账记录。
   *
   * 用于「设备已在运行、但台账记录丢失」的场景 —— 旧版本只能靠重新生成客户端包
   * 才能让一台设备出现在台账里，那条路走不通（不想再出包、更不想重新部署）。
   * 补录出来的记录 generate_count 为 0，界面标注为「手工补录」。
   */
  createRegistryClient: (body: {
    client_id: string
    display_name: string
    robot_type: string
    platform: string
    note: string
    server_addr: string
    server_port: number
    ports: { proxy_name: string; proxy_type: string; remote_port: number; local_port: number }[]
  }) => http.post<{ client: RegistryClient; message: string }>('/api/registry/clients', body),
  updateRegistryClient: (
    id: number,
    // platform 必须始终带上当前值：后端把空串解释成「清空为未记录」，
    // 漏传会让一次「改个备注」顺手把目标平台抹掉，而下一次改包就产错形态的包。
    body: {
      display_name: string
      robot_type: string
      platform: string
      note: string
      status: string
    },
  ) => http.post<{ client: RegistryClient; message: string }>(`/api/registry/clients/${id}`, body),
  // 台账记录 → 打包草稿。用于「在这台设备原有的包基础上改一版」。
  registrySpec: (id: number) =>
    http.get<RegistrySpecResponse>(`/api/registry/clients/${id}/spec`),
  deleteRegistryClient: (id: number) =>
    http.del<{ ok: boolean; message: string }>(`/api/registry/clients/${id}`),
  releaseRegistryPort: (id: number) =>
    http.del<{ ok: boolean; message: string }>(`/api/registry/ports/${id}`),

  // —— 健康检查 ——
  health: () => raw<{ status: string; time: string }>('GET', '/api/health'),
}
