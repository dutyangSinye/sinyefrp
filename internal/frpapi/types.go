// Package frpapi 实现 FRP Dashboard API 的只读客户端。
//
// 本包所有字段定义均以 2026-09-21 对 frps 0.69.0 的真实响应取证为准，
// 不照抄上游文档描述。取证记录见 docs/v1.0/03-frp-api-contract.md，
// 原始响应见 .workbuddy/probe/api-dump*.txt。
//
// 使用本包前请先读 03-frp-api-contract.md 的 §4：FRP 的接口存在多处
// 「同一概念两种表示」的不一致，例如客户端在线是 bool online，
// 而代理在线是字符串 status，二者不可统一抽象。
package frpapi

import "strings"

// 代理在线状态取值。注意 Status 是字符串，与 ClientInfo.Online 的布尔语义不同。
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// ProxyTypes 是 FRP 支持的代理类型白名单。
//
// 实测结论：对任意非白名单类型（例如 "bogus"）请求 /api/proxy/{type}，
// frps 同样返回 HTTP 200 与空列表，因此无法通过探测判断类型是否合法，
// 必须依赖本白名单（见 03-frp-api-contract.md §4-⑩）。
//
// 2026-09-25 移除 tcpmux：平台生成的配置里从来不含 tcpmux 必需的
// multiplexer 字段（服务端侧也从未渲染 tcpmuxHTTPConnectPort），
// 管理员一旦选它就会撞上 frpc 的 `not support multiplexer` 而被拒。
// 该类型在本平台从未可用，留在这份白名单里只会让采集侧去查一个
// 永远为空的类型（frps 返回 200 + 空列表，不会报错，因此静默）。
var ProxyTypes = []string{"tcp", "udp", "http", "https", "stcp", "xtcp"}

// IsValidProxyType 判断代理类型是否在白名单内。
func IsValidProxyType(t string) bool {
	for _, v := range ProxyTypes {
		if v == t {
			return true
		}
	}
	return false
}

// ServerInfo 对应 GET /api/serverinfo，为扁平对象。
type ServerInfo struct {
	Version               string `json:"version"`
	BindPort              int    `json:"bindPort"`
	VhostHTTPPort         int    `json:"vhostHTTPPort"`
	VhostHTTPSPort        int    `json:"vhostHTTPSPort"`
	TCPMuxHTTPConnectPort int    `json:"tcpmuxHTTPConnectPort"`
	KCPBindPort           int    `json:"kcpBindPort"`
	QUICBindPort          int    `json:"quicBindPort"`
	SubdomainHost         string `json:"subdomainHost"`
	MaxPoolCount          int64  `json:"maxPoolCount"`
	MaxPortsPerClient     int64  `json:"maxPortsPerClient"`
	// HeartbeatTimeout 为 -1 时表示使用 FRP 内置默认值。
	HeartbeatTimeout int64 `json:"heartbeatTimeout"`
	// TotalTrafficIn / TotalTrafficOut 为字节数，FRP 进程重启后归零。
	TotalTrafficIn  int64 `json:"totalTrafficIn"`
	TotalTrafficOut int64 `json:"totalTrafficOut"`
	CurConns        int64 `json:"curConns"`
	// ClientCounts 是「在线」客户端数，不是注册总数。
	// 离线客户端仍保留在 /api/clients 中，总数需由调用方自行统计。
	ClientCounts int64 `json:"clientCounts"`
	// ProxyTypeCount 的 key 集合会随历史变化，值可能为 0，不可用于枚举类型。
	ProxyTypeCount map[string]int64 `json:"proxyTypeCount"`
}

// ClientInfo 对应 GET /api/clients 的数组元素，也与 GET /api/clients/{key} 同构。
type ClientInfo struct {
	// Key 是 user 与 clientID 的拼接，用作详情接口的路径参数。
	Key string `json:"key"`
	// User 未在 frpc 中配置时为空串。
	User string `json:"user"`
	// ClientID 是跨重连稳定的设备标识（01-prd §8.1）。
	ClientID string `json:"clientID"`
	// RunID 在客户端离线后会被清空，不可作为持久引用。
	RunID   string `json:"runID"`
	Version string `json:"version"`
	// WireProtocol 是通信协议版本（实测为 "v1"），不是代理类型。
	WireProtocol string `json:"wireProtocol"`
	Hostname     string `json:"hostname"`
	ClientIP     string `json:"clientIP"`
	// FirstConnectedAt / LastConnectedAt 是 Unix 秒级时间戳。
	FirstConnectedAt int64 `json:"firstConnectedAt"`
	LastConnectedAt  int64 `json:"lastConnectedAt"`
	// DisconnectedAt 仅在客户端离线时出现；在线时字段完全不存在，
	// 因此必须用指针以区分「未离线」与「时间戳为 0」。
	DisconnectedAt *int64 `json:"disconnectedAt"`
	// Online 是客户端在线与否的唯一判定依据。
	Online bool `json:"online"`
}

// DisplayName 返回适合界面展示的标识：优先 clientID，其次 hostname。
func (c ClientInfo) DisplayName() string {
	if c.ClientID != "" {
		return c.ClientID
	}
	if c.Hostname != "" {
		return c.Hostname
	}
	return c.Key
}

// ProxyInfo 对应 GET /api/proxy/{type} 的 proxies 数组元素。
type ProxyInfo struct {
	// Name 是 "user.proxyname" 形式的完整名，内部一律用它作主键，
	// 不是用户在表单里填写的短名（03-frp-api-contract.md §4-⑪）。
	Name string `json:"name"`
	// Conf 在代理离线时为 null，此时端点与类型信息全部不可用。
	Conf     *ProxyConf `json:"conf"`
	User     string     `json:"user"`
	ClientID string     `json:"clientID"`
	// TodayTrafficIn / TodayTrafficOut 是「今日」流量，跨日或 FRP 重启后归零。
	TodayTrafficIn  int64 `json:"todayTrafficIn"`
	TodayTrafficOut int64 `json:"todayTrafficOut"`
	CurConns        int64 `json:"curConns"`
	// LastStartTime / LastCloseTime 形如 "09-21 14:35:54"，无年份，
	// 与客户端侧的 Unix 时间戳格式不同，不可混用。
	LastStartTime string `json:"lastStartTime"`
	LastCloseTime string `json:"lastCloseTime"`
	// Status 是字符串 "online"/"offline"。
	Status string `json:"status"`
}

// IsOnline 判断代理是否在线。
func (p ProxyInfo) IsOnline() bool { return p.Status == StatusOnline }

// Type 返回代理类型；离线代理的 conf 为 null，此时返回空串。
func (p ProxyInfo) Type() string {
	if p.Conf == nil {
		return ""
	}
	return p.Conf.Type
}

// RemotePort 返回远端端口；离线代理返回 0。
func (p ProxyInfo) RemotePort() int {
	if p.Conf == nil {
		return 0
	}
	return p.Conf.RemotePort
}

// ProxyConf 是代理的配置详情，仅在代理在线时可用。
type ProxyConf struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	LocalIP    string `json:"localIP"`
	RemotePort int    `json:"remotePort"`
	Transport  struct {
		BandwidthLimit     string `json:"bandwidthLimit"`
		BandwidthLimitMode string `json:"bandwidthLimitMode"`
	} `json:"transport"`
	LoadBalancer struct {
		Group string `json:"group"`
	} `json:"loadBalancer"`
	HealthCheck struct {
		Type            string `json:"type"`
		IntervalSeconds int    `json:"intervalSeconds"`
	} `json:"healthCheck"`
	// Plugin 在未配置时为 null。
	Plugin any `json:"plugin"`
}

// Traffic 对应 GET /api/traffic/{name}，两个数组长度实测均为 7。
type Traffic struct {
	Name       string  `json:"name"`
	TrafficIn  []int64 `json:"trafficIn"`
	TrafficOut []int64 `json:"trafficOut"`
}

// proxyListResponse 对应 GET /api/proxy/{type}。
//
// 注意：此处是对象包裹，而 /api/clients 返回裸数组，两者解码方式不同。
type proxyListResponse struct {
	Proxies []ProxyInfo `json:"proxies"`
}

// apiErrorBody 是 FRP 的错误响应体。
//
// 注意字段名是大写的 Code / Msg，与正常响应的小写风格不一致。
type apiErrorBody struct {
	Code int    `json:"Code"`
	Msg  string `json:"Msg"`
}

// SplitQualifiedName 把 "user.proxyname" 拆成用户与短名。
//
// 实测中当 frpc 未配置 user 时，完整名就是短名本身，
// 因此短名的正确来源始终是 API 返回的 Name，而不是自行拼接。
func SplitQualifiedName(qualified string) (user, short string) {
	if i := strings.Index(qualified, "."); i >= 0 {
		return qualified[:i], qualified[i+1:]
	}
	return "", qualified
}
