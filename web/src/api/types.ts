// 全部接口的响应类型定义。
//
// 字段名与大小写一律以 Go 端的 json tag 为准，不做任何「看起来更顺眼」的重命名：
//   - 客户端与代理混用了两套时间表示（Unix 秒 int64 / "MM-DD HH:mm:ss" 无年份字符串），
//     见 docs/v1.0/03-frp-api-contract.md §4；这里刻意保持原样，避免在类型层制造
//     「已经统一了」的错觉。
//   - 代理在线是字符串 status，客户端在线是布尔 online，同样不抽象。

// ---------- 通用 ----------

/** 服务端统一错误体：{ error: { kind, message } }。 */
export interface ApiErrorBody {
  kind: string
  message: string
  /**
   * 逐条细节（例如每一条端口冲突各自的说明），可选。
   *
   * 与 message 的分工：message 是要在一行里读出完整意思的摘要，
   * details 供界面按列表渲染。合成一句话会变成一整段无处下口的文字。
   */
  details?: string[]
}

/** 采集健康度。known=false 表示从未采集成功，界面必须显示「状态未知」。 */
export interface Health {
  known: boolean
  healthy: boolean
  stale: boolean
  last_success?: string | null
  last_attempt?: string | null
  fail_streak: number
  error_kind?: string
  message?: string
}

// ---------- FRP 只读数据 ----------

export interface ServerInfo {
  version: string
  bindPort: number
  vhostHTTPPort: number
  vhostHTTPSPort: number
  tcpmuxHTTPConnectPort: number
  kcpBindPort: number
  quicBindPort: number
  subdomainHost: string
  maxPoolCount: number
  maxPortsPerClient: number
  /** -1 表示使用 FRP 内置默认值。 */
  heartbeatTimeout: number
  /** 字节数；FRP 进程重启后归零。 */
  totalTrafficIn: number
  totalTrafficOut: number
  curConns: number
  /** 是「在线」客户端数，不是注册总数。 */
  clientCounts: number
  proxyTypeCount: Record<string, number>
}

export interface ClientStats {
  total: number
  online: number
  offline: number
}

export interface ClientInfo {
  /** user 与 clientID 的拼接，用作详情接口的路径参数。 */
  key: string
  user: string
  clientID: string
  runID: string
  version: string
  /** 通信协议版本（实测 "v1"），不是代理类型。 */
  wireProtocol: string
  hostname: string
  clientIP: string
  /** Unix 秒级时间戳。 */
  firstConnectedAt: number
  lastConnectedAt: number
  /** 仅在离线时存在；在线时字段完全缺失。 */
  disconnectedAt?: number | null
  online: boolean
  /**
   * 台账里管理员填写的可读名称，如「财务部-前台机」（01-prd F19）。
   * 由后端按 clientID 关联本地台账得到；台账里没填名称时为空串。
   *
   * **不要用 `!display_name` 判断「未登记」** —— 名称留空与压根没登记是两回事，
   * 后者看 `registered`。
   */
  display_name?: string
  /**
   * 这台设备在台账里的机器人类型（01-prd F20）；空串表示未分类。
   *
   * 与 display_name 一样由后端按 clientID 关联台账得到 —— FRP 没有这个概念。
   * 客户端列表按它分组展示。
   *
   * **不要用它的空值判断「未登记」**：未登记的客户端这里同样是空串，
   * 要区分的是 `registered`。
   */
  robot_type?: string
  /** 该 clientID 是否在本平台台账里登记过（false 多为别处手工配置的 frpc）。 */
  registered?: boolean
}

export interface ProxyConf {
  name: string
  type: string
  localIP: string
  remotePort: number
  transport: { bandwidthLimit: string; bandwidthLimitMode: string }
  loadBalancer: { group: string }
  healthCheck: { type: string; intervalSeconds: number }
  plugin: unknown
}

export interface ProxyInfo {
  /** "user.proxyname" 形式的完整名，内部一律用它作主键。 */
  name: string
  /** 代理离线时为 null，此时端点与类型信息全部不可用。 */
  conf: ProxyConf | null
  user: string
  clientID: string
  todayTrafficIn: number
  todayTrafficOut: number
  curConns: number
  /** 形如 "09-21 14:35:54"，无年份。 */
  lastStartTime: string
  lastCloseTime: string
  /** 字符串 "online" / "offline"。 */
  status: string
  /** 所属客户端在台账里的显示名称（01-prd F19）；语义同 ClientInfo。 */
  display_name?: string
  /** 所属 clientID 是否在本平台台账里登记过。 */
  registered?: boolean
  /**
   * 该代理在**目标机器上**的本地端口（frpc 转发的那个端口）。
   *
   * 它**不来自 FRP**：Dashboard 的 conf 只有 localIP 与 remotePort，没有
   * localPort（03-frp-api-contract.md §4-⑦ 实测）。唯一来源是本平台台账，
   * 按 (clientID, 代理短名) 关联 —— 与 display_name 走的是同一条路。
   */
  local_port?: number
  /**
   * 台账里是否登记过这条代理的本地端口。
   *
   * 必须看它，不能用 `local_port > 0` 代替：包不是本平台签发的（别人手工配的
   * frpc）就永远关联不上，界面应显示「未登记」而不是「0」。
   */
  local_port_known?: boolean
}

export interface OverviewResponse {
  health: Health
  server: ServerInfo | null
  clients: ClientStats
  proxy_count: number
  fetched_at?: string | null
}

export interface ClientsResponse {
  health: Health
  stats: ClientStats
  clients: ClientInfo[]
  fetched_at?: string | null
}

export interface ClientDetailResponse {
  health: Health
  client: ClientInfo | null
  proxies: ProxyInfo[]
}

/** 一条代理「从外面怎么访问」的答案。 */
export interface ClientLink {
  /** FRP 的完整名（user.proxyname）；界面展示用 short_name。 */
  proxy_name: string
  /** 管理员在表单里填的短名。 */
  short_name: string
  proxy_type: string
  remote_port: number
  online: boolean
  /**
   * 这条代理在**目标机器上**的端口。只用于让现场认出「这是哪个服务」，
   * 它不在访问地址里，也不是从外面能连的东西。
   *
   * FRP 的接口不返回本地端口，来源是本平台台账按 (clientID, 代理短名) 关联；
   * **未登记时为 0**，配合 local_port_known 使用。
   */
  local_port: number
  /**
   * 台账里是否真的登记了本地端口。
   *
   * **不许用 local_port > 0 代替**：未登记要显示「未登记」，不能显示成 0 号端口。
   */
  local_port_known: boolean
  /**
   * 可直接访问的地址，形如 `111.55.49.119:12312`。
   *
   * **空串表示这条代理给不出地址**，原因见 reason —— 不要画成空白，
   * 空白会被读成「加载失败」。给不出地址是常态（HTTP 靠域名、STCP 要访问者端），
   * 不是错误。
   */
  access_url: string
  reason: string
}

export interface ClientLinksResponse {
  client_id: string
  /** 台账里的可读名称（如 RCS02），用于「XXX 的访问地址为」这句文案。 */
  display_name: string
  /** 接入点的对外地址。**不是** Dashboard 地址（NAT 下两者端口不同）。 */
  endpoint: string
  /** false 时所有地址都给不出，界面要提示去设置页补全。 */
  endpoint_configured: boolean
  links: ClientLink[]
}

export interface ProxiesResponse {
  health: Health
  proxies: ProxyInfo[]
  fetched_at?: string | null
}

export interface RefreshResponse {
  refreshed: boolean
  health: Health
  known: boolean
}

// ---------- 认证与初始化 ----------

export interface PasswordPolicy {
  min_username_len: number
  max_username_len: number
  min_password_len: number
  max_password_len: number
}

export interface SetupStatus {
  initialized: boolean
  policy: PasswordPolicy
  session_ttl_seconds: number
}

export interface SessionResponse {
  user: { username: string }
  expires_at: string
}

export interface MeResponse {
  user: { username: string }
  node: { name: string; source: string }
}

// ---------- 节点配置 ----------

/** 节点配置的对外表示（不含凭据，只有 password_configured 布尔）。 */
export interface NodeView {
  name: string
  api_url: string
  username: string
  password_configured: boolean
  timeout_seconds: number
  enabled: boolean
}

export interface NodeConfigResponse {
  node: NodeView
  source: string
  persisted?: unknown
  /** active=true 表示当前进程被命令行参数或环境变量覆盖，保存的配置需重启才生效。 */
  overridden: { active: boolean }
}

export interface NodeSaveResponse {
  node: NodeView
  source: string
  needs_restart: boolean
  message: string
}

/**
 * 客户端接入点（GET/POST /api/pkg/endpoint）。
 *
 * 它与节点配置（NodeView）刻意分开，因为描述的是两件事：
 *   - node.api_url 是管理平台采集时访问的 FRP Dashboard 地址，通常走环回或内网；
 *   - 这里是 frpc（可能装在任意一台外网机器上）要连接的地址与端口。
 * frps 前面有 NAT 映射时连端口都不同（内部 bindPort 7000、对外可能 12123），
 * 所以不能用 Dashboard 地址去推接入点。
 */
export interface EndpointConfig {
  /** 客户端要连接的对外地址；空串表示未配置。 */
  client_addr: string
  /** 客户端要连接的对外端口；0 表示未配置，后端按 FRP 默认值 7000 处理。 */
  client_port: number
  /** token 只返回「是否已配置」，明文永不下发（01-prd F17）。 */
  token_configured: boolean
  /** 是否已配置（以地址为准）。false 时客户端表单会提示去设置页补全。 */
  configured: boolean
  /** true 表示已存的 token 密文解不开（DPAPI 与写入它的机器绑定），需要重新填写。 */
  token_decrypt_failed?: boolean
}

export interface EndpointSaveResponse {
  endpoint: EndpointConfig
  message: string
}

export interface NodeTestResult {
  ok: boolean
  compatible: boolean
  /** unreachable / timeout / unauthorized / not_found / server_error / bad_response / incompatible */
  kind?: string
  message: string
  frp_version?: string
  client_count: number
  proxy_count: number
  latency_ms: number
  tested_at: string
  target_api_url: string
}

// ---------- 审计与生成记录 ----------

export interface AuditItem {
  id: number
  at: string
  actor: string
  action: string
  target: string
  result: string
  client_ip: string
  detail: string
}

export interface PkgRecord {
  id: number
  at: string
  actor: string
  kind: string
  client_id: string
  display_name: string
  frp_version: string
  exe_sha256: string
  summary: string
}

// ---------- 数据存储状态 ----------

/**
 * 数据库文件的落盘信息。
 *
 * 存在的理由：现场排障「重启后记录没了」时，第一件必须确认的事是
 * 「平台到底在用哪个数据库文件」。相对路径的解析基准是配置文件所在目录
 * （不是进程工作目录），所以路径必须由后端给出绝对值，界面不许自己拼。
 */
export interface DataFileStatus {
  path: string
  size_bytes: number
  size_text: string
  modified_at: string | null
  exists: boolean
}

export interface DataStatusResponse {
  database: DataFileStatus
  wal: DataFileStatus & {
    /**
     * WAL 相对主库偏大。
     *
     * WAL 里是「尚未回写主库、且暂无第二副本」的数据；它越大，
     * 一次异常掉电的暴露面越大。为 true 时应建议运维触发一次正常关闭。
     */
    heavy: boolean
  }
  hint: string
}

// ---------- 部署包（F09-F17） ----------

export interface PkgMeta {
  frp_version: string
  /**
   * 正式支持的 FRP 版本前缀集合。
   *
   * 刻意是数组而不是单个字符串：frps 与平台内嵌的 frpc 是两份独立分发的
   * 二进制，升级只能分批做，过渡期两端版本必然不一致（实测 0.69 与 0.71
   * 可混跑）。只给一个前缀会让合法的那一半被误报成「不兼容」。
   * 取值形如 ["0.69.", "0.71."]，含末尾的点。
   */
  supported_prefixes: string[]
  version_warning: string
  /**
   * 二进制来源。取值由 internal/pack/assets.go 决定，只有两种形态：
   *   - "embedded"      编译期内嵌（正式交付件的唯一形态）；
   *   - "disk:<目录>"   内嵌缺失时回退读盘（仅开发期可能命中）。
   * 这里刻意不写联合字面量类型：`disk:` 后面跟目录，无法穷举。
   * 但也正因为它是开放字符串，界面不能用猜的值去比对——曾经把 'embedded'
   * 误写成 'embed'，中文标签就再没生效过，界面直接露出英文原文。
   */
  source: string
  client_exe_size: number
  server_exe_size: number
  client_exe_sha256: string
  server_exe_sha256: string
  proxy_types: string[]
  /**
   * 客户端包 / 服务器包可选的目标平台（PLATFORM_LABEL 的键）。
   *
   * 只下发**真的有二进制**的平台：下拉里放一个选了必然失败的选项，
   * 比不放更糟——管理员要到装包那一步才知道这台机器不被支持。
   * 为空表示尚未加载 FRP 二进制（此时界面应禁用打包入口）。
   */
  client_platforms: string[]
  server_platforms: string[]

  // —— 管理平台自身（F18）——
  //
  // 这里的「二进制」是当前正在运行的这个 exe，不是内嵌资源，
  // 因此多一组可用性字段：读不到自己时（文件被安全软件锁住、已被删除）
  // 界面要把下载按钮置灰并说明原因，而不是等管理员点了才收到 503。
  platform_version: string
  platform_exe_size: number
  platform_exe_sha256: string
  platform_available: boolean
  /** 不可用原因；platform_available 为 true 时是空串。 */
  platform_unavailable_reason: string
}

export interface ProxySpec {
  name: string
  type: string
  local_ip: string
  local_port: number
  remote_port: number
  custom_domains: string[]
  subdomain: string
  locations: string[]
  secret_key: string
  use_encryption: boolean
  use_compression: boolean
}

export interface ClientSpec {
  server_addr: string
  server_port: number
  name: string
  client_id: string
  /**
   * 机器人类型（ROBOT_TYPE_LABEL 的键），空串表示未分类。
   *
   * 它**不会**出现在包内的 frpc.toml 里：FRP 对未知字段是严格解码的，
   * 多一个键就能让整包被拒（02-技术选型 §6.5 记过同类事故）。
   * 它的去处是客户端台账与包内 README.txt。
   */
  robot_type: string
  token: string
  enable_tls: boolean
  tls_server_name: string
  log_level: string
  log_max_days: number
  proxies: ProxySpec[]
  /**
   * 目标运行平台（PLATFORM_LABEL 的键），决定包里装哪个二进制与哪套脚本。
   *
   * 空串由后端补成 windows-amd64，因此前端不必强制填充；
   * 但一旦选了 Linux，包内就是 .sh + systemd unit 而不是 .ps1 + 计划任务，
   * 界面展示的文件清单与实际产物都随之改变。
   */
  platform: string
}

export interface PortRange {
  start: number
  end: number
}

export interface ServerSpec {
  bind_addr: string
  bind_port: number
  token: string
  dashboard_addr: string
  dashboard_port: number
  dashboard_user: string
  dashboard_password: string
  log_level: string
  log_max_days: number
  subdomain_host: string
  allow_ports: PortRange[]
}

export interface PkgPreview {
  file_name: string
  /** 已遮蔽敏感值的 TOML。 */
  toml: string
  warnings: string[]
  /**
   * 会**阻止下载**的问题，与 warnings 分开。
   *
   * warnings 是「包能生成，但你该知道」；blocking 是「生成出来也跑不起来」。
   * 当前只有一类：远端端口与已签发或已在线的代理撞了（F19）。
   * 预览本身不因它报错（否则管理员看不到 TOML 就没法判断该改到哪个端口），
   * 真正的拦截发生在下载接口（409 conflict）。
   */
  blocking: string[]
  frp_version: string
  summary: string
  files: string[]
  size_bytes: number
  /**
   * 本次预览对应的目标平台，由后端回显。
   *
   * 回显的意义在于可核对：files 与 size_bytes 都随平台变化，
   * 前端若拿本地选中的值与预览结果混用，就会出现「清单对不上产物」——
   * 而这种不一致在界面上完全看不出来。
   */
  platform: string
}

// ---------- 客户端台账与端口占用（F19） ----------

/**
 * 机器人类型的中文名。
 *
 * 值域与 Go 端 internal/pack/spec.go 的 AllRobotTypes 一一对应，
 * 必须**逐字一致**（同 REGISTRY_STATUS_LABEL / PROXY_TYPE_LABEL 的分工方式）。
 * 新增类型时后端常量、后端 robotTypeLabels、这里三处都要改：
 * 少改一处不会编译失败，只会让界面出现一个选不出中文名的值。
 *
 * 注意这是**纯业务分类**：它不进 frpc.toml（FRP 严格解码，多一个键就拒包），
 * 也不参与端口冲突判定，只用于让管理员在列表里认出这台是什么设备。
 * 空串表示「未分类」，是合法取值。
 */
export const ROBOT_TYPE_LABEL: Record<string, string> = {
  spray: '喷涂机器人',
  rebar: '钢筋加工机器人',
  transport: '运输机器人',
  welding: '焊接机器人',
  other: '其他',
}

/**
 * 机器人类型下拉的选项顺序（= 后端 AllRobotTypes 的顺序）。
 *
 * 刻意把四类具体机器人排在「其他」之前：让具体项先被看到，
 * 「其他」作为兜底项放在最后。
 */
export const ROBOT_TYPES: string[] = ['spray', 'rebar', 'transport', 'welding', 'other']

/** 未分类时的展示文案。空串本身合法，但不能在界面上画成一个空格。 */
export const ROBOT_TYPE_NONE = '未分类'

/** 机器人类型的中文标签；空值落到「未分类」，未知值原样返回。 */
export function robotTypeLabel(v: string | null | undefined): string {
  if (!v) return ROBOT_TYPE_NONE
  return ROBOT_TYPE_LABEL[v] ?? v
}

/**
 * 台账里一条端口占用明细。
 *
 * remote_port 为 0 表示该代理类型不占 frps 上的全局端口
 * （HTTP/HTTPS/STCP/XTCP/TCPMUX）；只有 TCP/UDP 才有远端端口。
 * local_port 属于目标机器自己的地址空间，不同客户端可以重复，不参与冲突判定。
 */
export interface RegistryPort {
  id: number
  proxy_name: string
  proxy_type: string
  remote_port: number
  local_port: number
}

/**
 * 台账记录的状态取值，对应 Go 端 store.ClientStatusActive / ClientStatusRetired。
 * 只有 active 的记录参与端口冲突判定；retired 表示设备已下线、端口视为已释放。
 * 重新生成一个 retired 客户端的包会自动把它恢复为 active。
 */
export const REGISTRY_STATUS_LABEL: Record<string, string> = {
  active: '启用',
  retired: '停用',
}

export interface RegistryClient {
  id: number
  client_id: string
  display_name: string
  /** 机器人类型取值，见 ROBOT_TYPE_LABEL；空串表示未分类。 */
  robot_type: string
  server_addr: string
  server_port: number
  /**
   * 部署包的**目标运行平台**（windows-amd64 / linux-amd64 / linux-arm64），
   * 决定下一次「在这个基础上改包」产出的是哪一种形态的包。
   *
   * **空串表示「未记录」**，不是 Windows：它是本平台开始记录平台之前
   * 签发的包。界面按 Windows 展示这类记录，但改包时必须让管理员确认一次 ——
   * 猜错的代价是给 Linux 盒子产出一个装不上的包，而安装脚本不会报错。
   */
  platform: string
  status: string
  note: string
  /** 重新生成的累计次数，首次生成为 1。 */
  generate_count: number
  /**
   * 该 clientID 是否出现在 FRP 当前快照里。
   *
   * **只在 health.known 为 true 时才有意义**：从未采集成功时它是 false，
   * 但那是「不知道」而不是「不在线」，界面必须显示「—」而不是「未见」。
   */
  seen_in_frp: boolean
  /**
   * 该 clientID **此刻**在 FRP 里是否在线。
   *
   * 与 `seen_in_frp` 是两件事：`seen_in_frp` 连「已断开但尚未被 FRP 清理」的
   * 客户端也算（回答「装过没有」），`online` 只表示现在通不通。
   * 两者同样只在 health.known 为 true 时才有意义。
   */
  online: boolean
  first_generated_at: string | null
  last_generated_at: string | null
  ports: RegistryPort[]
}

export interface RegistryClientsResponse {
  clients: RegistryClient[]
  health: Health
}

/**
 * 「拿台账记录重开一张打包表单」的响应。
 *
 * 部署包 ZIP 从不落盘、服务端也不保留当时的密钥，所以「编辑旧包」物理上
 * 做不到；能重建的只有**当初的输入**，而它唯一的去处就是台账。
 * 因此表单里必然有一部分内容是这台填进去的默认值，`defaults_applied`
 * 与 `proxy_defaults` 就是把这部分如实标出来 —— 否则管理员会以为
 * 每一项都是原样还原的。
 */
export interface RegistrySpecResponse {
  spec: ClientSpec
  /** 按默认值补齐的顶层字段（token 不在台账里、日志级别属于当下选择）。 */
  defaults_applied: string[]
  /** 逐条代理列出没能还原的字段，只列这条代理用得上的。 */
  proxy_defaults: ProxyDefaultHint[]
  /** 需要管理员先确认的事项，例如「这条记录没登记目标平台」。 */
  warnings: string[]
}

export interface ProxyDefaultHint {
  proxy_name: string
  fields: string[]
}

/**
 * 某个远端端口的占用者。
 *
 * source 区分两个**不可合并**的口径：
 *   - "registry" 本平台台账里的占用（可能包已签发但还没装到机器上）；
 *   - "live"     FRP 此刻真实在线的代理（可能是在别处手工配的，不在台账里）。
 * 二者的处理方式完全不同：前者可以到台账里停用/删除来释放，后者只能改端口。
 */
export interface PortOwner {
  /** 台账端口占用记录的编号，用于「只释放这一条」；live 口径恒为 0。 */
  id: number
  /**
   * **远端端口**（frps 上的那个）。两个口径的表都是按它组织的：
   * 远端端口在同一个 frps 上是全局资源，冲突判定只看它。
   */
  port: number
  client_id: string
  client_name: string
  proxy_name: string
  proxy_type: string
  /**
   * 这条代理在目标机器上的本地端口，来源同 ProxyInfo.local_port：
   * 只能由后端按 (clientID, 代理短名) 关联台账得到，FRP 不返回。
   */
  local_port: number
  /** 台账里是否登记过本地端口；务必看它，别用 `local_port > 0` 代替。 */
  local_port_known: boolean
  /** registry 口径取 active/retired；live 口径取 online/offline。 */
  status: string
  source: string
}

export interface RegistryPortsResponse {
  registry: PortOwner[]
  live: PortOwner[]
  health: Health
}

/** 代理类型的中文说明，用于表单下拉与代理列表。 */
export const PROXY_TYPE_LABEL: Record<string, string> = {
  tcp: 'TCP',
  udp: 'UDP',
  http: 'HTTP',
  https: 'HTTPS',
  stcp: 'STCP（点对点，需密钥）',
  xtcp: 'XTCP（点对点，需密钥）',
  // tcpmux 已移除（2026-09-25）：平台渲染的 frpc.toml 不含它必需的
  // multiplexer 字段，服务端也没有 tcpmuxHTTPConnectPort，选它打包必被
  // frpc 拒绝。见 internal/pack/spec.go 的 AllProxyTypes 注释。
}

/** 需要显式远端端口的类型（对应 Go 端 NeedsRemotePort）。 */
export const PROXY_TYPES_WITH_REMOTE_PORT = ['tcp', 'udp']
/** 需要预共享密钥的类型（对应 Go 端 NeedsSecretKey）。 */
export const PROXY_TYPES_WITH_SECRET_KEY = ['stcp', 'xtcp']
/** 需要域名或子域的类型（对应 Go 端 NeedsHostRule）。 */
export const PROXY_TYPES_WITH_HOST = ['http', 'https']

/**
 * 目标平台的中文名。
 *
 * 值域与 Go 端 internal/pack/target.go 的 AllPlatforms 一一对应，必须**逐字一致**
 * （同 ROBOT_TYPE_LABEL / PROXY_TYPE_LABEL 的分工方式）。
 * 新增平台时后端常量、后端 platformLabels、这里三处都要改：
 * 少改一处不会编译失败，只会让界面出现一个选不出中文名的值，
 * 而 vue-tsc 与截图都查不出来。
 */
export const PLATFORM_LABEL: Record<string, string> = {
  'windows-amd64': 'Windows x64',
  'linux-amd64': 'Linux x64',
  'linux-arm64': 'Linux ARM64',
}

/** 未指定平台时的取值，与 Go 端 DefaultPlatform 一致。 */
export const DEFAULT_PLATFORM = 'windows-amd64'

export const LOG_LEVELS = ['trace', 'debug', 'info', 'warn', 'error']
