package store

import "time"

// User 是管理平台的管理员账号。
//
// PasswordHash 是 bcrypt 哈希，永不返回给前端（01-prd F01/F17）。
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  time.Time
}

// Session 是一次登录会话。
//
// 数据库只保存 token 的 SHA-256，明文 token 仅存在于浏览器 Cookie 中：
// 即使数据库泄露，也无法据此伪造会话。
type Session struct {
	TokenHash  string
	UserID     int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	ClientIP   string
	UserAgent  string
}

// Node 是被管理的 frps 节点。
//
// PasswordEnc 保存的是密文（secret 包的 enc:v1: 格式）；把明文取出来是
// 调用方（auth.NodeSettings）的职责，本包不接触明文。
type Node struct {
	ID             int64
	Name           string
	APIURL         string
	Username       string
	PasswordEnc    string
	TimeoutSeconds int
	Enabled        bool
	IsPrimary      bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Audit 是一条操作审计记录。
//
// Detail 只允许写入非敏感摘要（01-prd F16）。
//
// 必须带 json tag：本结构体会被 /api/audit 直接序列化，而 Go 的默认行为是
// 输出 PascalCase 字段名（Action / ClientIP / Detail …），与其余接口一律
// snake_case 的风格不一致。前端 JS 的属性访问是大小写敏感的，PascalCase
// 会让审计页面读到 undefined，因此这里显式对齐为 snake_case。
type Audit struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Target   string    `json:"target"`
	Result   string    `json:"result"`
	ClientIP string    `json:"client_ip"`
	Detail   string    `json:"detail"`
}

// PkgRecord 是一次部署包生成记录。
//
// 只记录非敏感摘要；ZIP 本体不落盘，因此这里不存文件路径（01-prd F16/§8.5）。
type PkgRecord struct {
	ID          int64
	At          time.Time
	Actor       string
	Kind        string
	ClientID    string
	DisplayName string
	FRPVersion  string
	ExeSHA256   string
	Summary     string
}

// 部署包类型。
//
// 取值必须与 internal/pack 里写入 Package.Kind 的字面量逐字一致 ——
// 它们跨越了一层包边界，编译器帮不上忙，前端还会拿它來选中文标签。
const (
	PkgKindClient = "client"
	PkgKindServer = "server"
	// PkgKindPlatform 是管理平台自身部署包（01-prd F18）。
	PkgKindPlatform = "platform"
)

// ---------- 客户端台账（01-prd F19） ----------

// ClientRegistry 是「本平台签发过的客户端」的台账记录。
//
// 它存在的理由只有一个：远端端口在同一个 frps 上是全局资源，
// 一旦两个客户端占同一个端口，后注册的那个代理会被 FRP 拒绝。
// 而 FRP 的 Dashboard 只在代理**已在线**时才能看到它占的端口，
// 对于「包已生成、还没装到目标机器」的那段时间完全无据可查。
// 因此必须由本平台自己记账，在生成阶段就把冲突拦下来。
//
// 重要语义：这是一份「签发记录」，不是运行时真相。
// 管理员之后可能在目标机器上手工改过 frpc.toml，此时台账与实际情况会分叉。
// 界面必须把两者分开呈现（台账 vs 当前在线代理），不可混为一个口径。
type ClientRegistry struct {
	ID          int64
	ClientID    string
	DisplayName string
	// RobotType 是这台设备承载的机器人类型（internal/pack 的 RobotType 取值，
	// 如 spray / rebar / transport / welding / other）。
	//
	// 空串表示未分类 —— 这一列**不参与任何判定**，纯粹是给管理员看的分类信息，
	// 因此允许为空。store 这里存字符串而不是引入 pack 的类型：
	// store 不该依赖 pack（方向反了），值域校验由 pack 与 web 层负责。
	RobotType string
	// Platform 记录这台设备的部署包目标运行平台（windows-amd64 / linux-amd64 / linux-arm64），
	// 取值见 internal/pack 的 Platform 常量；store 这里存字符串，不依赖 pack（方向反了）。
	//
	// 空串表示「未记录」：v4 之前签发的包没有这一信息。它必须在改包时回填，
	// 否则生成的包可能根本装不上（给 Linux 盒子产 Windows 包）。
	Platform string
	// ServerAddr / ServerPort 记录签发时使用的接入点，便于识别「这批包是发给哪个节点的」。
	ServerAddr string
	ServerPort int
	// Status 取 ClientStatusActive / ClientStatusRetired。
	// 只有 active 的记录参与端口冲突判定。
	Status string
	// Note 是管理员备注（例如「财务部前台机，2026-10 报废」），非敏感。
	Note string
	// GenerateCount 是重新生成的次数累计，首次生成为 1。
	GenerateCount int
	// FirstGeneratedAt 为首次生成时间；LastGeneratedAt 为最近一次生成时间。
	FirstGeneratedAt time.Time
	LastGeneratedAt  time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// 台账状态取值。前端拿它选中文标签，必须与 web/src/api/types.ts 的注释一致。
const (
	// ClientStatusActive 表示该客户端在用，其端口参与冲突判定。
	ClientStatusActive = "active"
	// ClientStatusRetired 表示该客户端已下线，端口视为已释放。
	ClientStatusRetired = "retired"
)

// ClientPort 是台账里某个客户端占用的一条端口记录。
//
// 只对「需要远端端口」的代理类型（TCP/UDP）才有 RemotePort；
// 其余类型（HTTP/HTTPS/STCP/XTCP/TCPMUX）不占用全局端口，这里的 RemotePort 为 0。
// LocalPort 一并记下来供运维比对，但它属于目标机器自己的地址空间，
// 不同客户端之间可以重复，因此**不参与冲突判定**。
type ClientPort struct {
	ID         int64
	ClientID   string
	ProxyName  string
	ProxyType  string
	RemotePort int
	LocalPort  int
	UpdatedAt  time.Time
}

// PortOwner 描述某个远端端口当前被谁占用。
//
// Source 区分两个不同性质的口径，界面与错误文案都要据此说明：
//   - PortSourceRegistry：本平台台账里的占用（可能是「已签发但还没装」）；
//   - PortSourceLive：FRP 当前真实在线的代理（可能是别人手工配的，不在台账里）。
type PortOwner struct {
	// ID 是台账端口占用记录（client_port）的编号，供「只释放这一条」使用；
	// live 口径没有对应的台账记录，恒为 0。
	ID         int64
	Port       int
	ClientID   string
	ClientName string
	ProxyName  string
	ProxyType  string
	Status     string
	Source     string
}

// 端口占用来源。
const (
	PortSourceRegistry = "registry"
	PortSourceLive     = "live"
)
