// Package pack 实现部署包生成：客户端与服务器两套配置表单、
// TOML 渲染、生命周期脚本、二进制校验（verify -c）与 ZIP 组装。
//
// 设计边界（来自 01-prd §8 与 §9）：
//   - 不修改、不重新编译、不 fork frps/frpc；
//   - 配置字段由本包受控定义，打包前一律经对应二进制 verify -c 把关，
//     因此「字段是否足以生成合法配置」由 FRP 自身裁决，而不是由本包猜测；
//   - ZIP 含明文密钥，只即时生成并流式下载，不落盘、不留档。
package pack

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ProxyType 是 FRP 支持的代理类型（01-prd F07/F09）。
type ProxyType string

const (
	ProxyTCP   ProxyType = "tcp"
	ProxyUDP   ProxyType = "udp"
	ProxyHTTP  ProxyType = "http"
	ProxyHTTPS ProxyType = "https"
	ProxySTCP  ProxyType = "stcp"
	ProxyXTCP  ProxyType = "xtcp"
)

// AllProxyTypes 按固定顺序列出全部类型，供前端下拉与测试遍历。
//
// 必须硬编码：实测发现 /api/proxy/{type} 对任意类型（含不存在的 bogus）
// 都返回 200，无法用探测判断类型合法性（见 03-frp-api-contract 约束 7）。
//
// 2026-09-25 移除 tcpmux：它在本平台从未生成过可用配置。
// 直接原因是渲染结构 tomlProxy 里没有 multiplexer 字段，frpc verify 会拒
// （`proxy X: not support multiplexer:`）；更根本的是服务端也没渲染
// tcpmuxHTTPConnectPort，即使补上客户端那一半仍然不通。
// 于是管理员在下拉里选它 ⇒ 打包必被拒，且错误信息是 FRP 的原始英文，
// 界面上看不出是自己选错了类型。这条缺陷此前没暴露，是因为回归脚本
// 遍历代理类型时压根没覆盖 tcpmux（scripts/ 下零命中）。
// 与其补两个字段去支持一个本平台用不到的类型，不如不提供它。
var AllProxyTypes = []ProxyType{ProxyTCP, ProxyUDP, ProxyHTTP, ProxyHTTPS, ProxySTCP, ProxyXTCP}

// Valid 判断类型是否在清单内。
func (t ProxyType) Valid() bool {
	for _, x := range AllProxyTypes {
		if t == x {
			return true
		}
	}
	return false
}

// NeedsRemotePort 表示该类型需要显式远端端口。
func (t ProxyType) NeedsRemotePort() bool { return t == ProxyTCP || t == ProxyUDP }

// ProxyTypeChoices 把全部代理类型拼成一句人类可读的清单，用于错误提示。
//
// 与 RobotTypeChoices 对称：错误文案里给出「可选项有哪些」，
// 比只说「不支持」更能让管理员一次改对，也避免他们去翻文档。
func ProxyTypeChoices() string {
	parts := make([]string, 0, len(AllProxyTypes))
	for _, t := range AllProxyTypes {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, "、")
}

// NeedsSecretKey 表示该类型需要预共享密钥。
func (t ProxyType) NeedsSecretKey() bool { return t == ProxySTCP || t == ProxyXTCP }

// NeedsHostRule 表示该类型需要域名或子域。
func (t ProxyType) NeedsHostRule() bool { return t == ProxyHTTP || t == ProxyHTTPS }

// ---------- 机器人类型（01-prd F19 扩展） ----------
//
// 这是**业务分类**，与 FRP 毫无关系：它既不进 frpc.toml，也不影响任何代理行为，
// 只是让管理员在一堆 clientID 里认出「这台是焊接机器人」。
//
// 必须写清这一点，因为它非常容易被顺手塞进 TOML —— 而 FRP 对未知字段是严格
// 解码的，多一个键就让整包被拒（见 02-技术选型 §6.5 记录的 remotePort 事故）。
// 该字段的去处只有两个：客户端台账（供界面展示与筛选）、包内 README（供现场核对）。
// 它同样**不参与端口冲突判定** —— 端口是全局资源，与设备是什么机器人无关。

// RobotType 是被管理设备所承载的机器人类型。
type RobotType string

const (
	RobotSpray     RobotType = "spray"     // 喷涂机器人
	RobotRebar     RobotType = "rebar"     // 钢筋加工机器人
	RobotTransport RobotType = "transport" // 运输机器人
	RobotWelding   RobotType = "welding"   // 焊接机器人
	RobotOther     RobotType = "other"     // 其他
)

// AllRobotTypes 按固定顺序列出全部类型，供前端下拉与测试遍历。
//
// 顺序即下拉展示顺序，刻意把四类具体机器人排在「其他」之前。
var AllRobotTypes = []RobotType{RobotSpray, RobotRebar, RobotTransport, RobotWelding, RobotOther}

// robotTypeLabels 是各类型的中文名，用于后端生成的中文文案（校验错误、README）。
//
// 前端下拉另有一份同构的标签表（web/src/api/types.ts 的 ROBOT_TYPE_LABEL）：
// 这是本项目的既有分工方式（同 REGISTRY_STATUS_LABEL / PROXY_TYPE_LABEL）。
// 新增类型时**三处都要改**：这里的常量与标签、AllRobotTypes、前端标签表，
// 否则界面会出现一个选不出中文名的值 —— 而 vue-tsc 查不出来。
var robotTypeLabels = map[RobotType]string{
	RobotSpray:     "喷涂机器人",
	RobotRebar:     "钢筋加工机器人",
	RobotTransport: "运输机器人",
	RobotWelding:   "焊接机器人",
	RobotOther:     "其他",
}

// Valid 判断类型是否在清单内。
func (t RobotType) Valid() bool {
	for _, x := range AllRobotTypes {
		if t == x {
			return true
		}
	}
	return false
}

// ValidRobotType 判断一个字符串是否是合法的机器人类型。
//
// **空串是合法的**，表示「未分类」：这一列不参与任何判定，
// 强制必填只会让存量调用方（含全部回归用例）一齐失败，
// 而拿不到任何实际收益。界面提示建议填写，但不拦。
func ValidRobotType(v string) bool {
	return v == "" || RobotType(v).Valid()
}

// RobotTypeLabel 返回中文名；未分类返回空串，未知值原样返回。
//
// 未知值原样返回而不是显示「未知」：真出现这种情况说明库里存了一个
// 本版本不认识的值（多半是降级部署），把原始值透出来比一个笼统的
// 「未知」更容易定位问题。
func RobotTypeLabel(v string) string {
	if v == "" {
		return ""
	}
	if label, ok := robotTypeLabels[RobotType(v)]; ok {
		return label
	}
	return v
}

// RobotTypeChoices 生成「喷涂机器人(spray)、…」形式的提示串，用于校验错误消息。
//
// 导出是给 web 层复用的：台账编辑接口也要给出同样的值域说明，
// 两处各拼一份迟早会漂移。
func RobotTypeChoices() string {
	parts := make([]string, 0, len(AllRobotTypes))
	for _, t := range AllRobotTypes {
		parts = append(parts, robotTypeLabels[t]+"("+string(t)+")")
	}
	return strings.Join(parts, "、")
}

// ---------- 客户端配置（01-prd F09） ----------

// ProxySpec 描述一条代理规则。
type ProxySpec struct {
	Name        string    `json:"name"`
	Type        ProxyType `json:"type"`
	LocalIP     string    `json:"local_ip"`
	LocalPort   int       `json:"local_port"`
	RemotePort  int       `json:"remote_port"`
	CustomHosts []string  `json:"custom_domains"`
	Subdomain   string    `json:"subdomain"`
	Locations   []string  `json:"locations"`
	SecretKey   string    `json:"secret_key"`
	Encryption  bool      `json:"use_encryption"`
	Compression bool      `json:"use_compression"`
}

// ClientSpec 是一次客户端部署包的完整输入。
type ClientSpec struct {
	// 连接目标
	ServerAddr string `json:"server_addr"`
	ServerPort int    `json:"server_port"`
	// 身份
	Name     string `json:"name"`      // 显示名，允许中文，仅用于包名与任务名
	ClientID string `json:"client_id"` // 跨重连识别同一台设备（01-prd §8.1）
	// RobotType 是这台设备承载的机器人类型，见文件上方 RobotType 的说明。
	// 空串表示未分类，非空时必须是 AllRobotTypes 之一。
	RobotType string `json:"robot_type"`
	Token     string `json:"token"`
	// 传输
	EnableTLS     bool   `json:"enable_tls"`
	TLSServerName string `json:"tls_server_name"`
	// 日志
	LogLevel   string `json:"log_level"`
	LogMaxDays int    `json:"log_max_days"`
	// 代理
	Proxies []ProxySpec `json:"proxies"`

	// Platform 是部署包的**目标运行平台**（Windows x64 / Linux x64 / Linux ARM64）。
	//
	// 它决定包里装的是哪个二进制、用哪套生命周期脚本、文本是 CRLF 还是 LF，
	// 与「本平台跑在什么系统上」无关 —— 平台自己跑在 Windows Server 上，
	// 照样要能产出一包跑在 Linux 机器人盒子上的东西。
	//
	// 空串由 Normalize 补成 DefaultPlatform：存量调用方与已保存的表单草稿
	// 都不带这个字段，判为错误会让它们一齐失败，而拿不到任何实际收益。
	Platform Platform `json:"platform"`
}

// ---------- 服务器配置（01-prd F11） ----------

// PortRange 是 allowPorts 的一段区间。
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ServerSpec 是一次服务器部署包的完整输入。
type ServerSpec struct {
	BindAddr string `json:"bind_addr"`
	BindPort int    `json:"bind_port"`
	Token    string `json:"token"`

	DashboardAddr     string `json:"dashboard_addr"`
	DashboardPort     int    `json:"dashboard_port"`
	DashboardUser     string `json:"dashboard_user"`
	DashboardPassword string `json:"dashboard_password"`

	LogLevel   string `json:"log_level"`
	LogMaxDays int    `json:"log_max_days"`

	SubDomainHost string      `json:"subdomain_host"`
	AllowPorts    []PortRange `json:"allow_ports"`
}

// ---------- 校验 ----------

// 安全标识：代理名与 clientID 都会进入 TOML、文件名与计划任务名，
// 因此限定为 ASCII 安全字符，中文只允许出现在「显示名」里（01-prd §8.1）。
var (
	reSafeIdent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	reHostName  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
)

// ErrInvalid 是配置校验失败的基类型，便于上层统一映射为 400。
var ErrInvalid = errors.New("配置校验失败")

// invalidf 构造带字段定位的校验错误。
func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w：%s", ErrInvalid, fmt.Sprintf(format, a...))
}

// emptyIfNil 把 nil 切片换成空切片。
//
// encoding/json 会把 nil 切片编码成 null，而契约上「没有元素」必须是 []：
// 前端模板会直接读 .length，拿到 null 就抛
// "Cannot read properties of null (reading 'length')" 并让整页渲染失败。
// Element Plus 的 el-tab-pane 默认同时渲染所有面板（隐藏而非卸载），
// 所以一个隐藏面板里的 null.length 照样能把整个部署包页打崩。
func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Normalize 填充客户端配置的默认值并清理空白。
func (c *ClientSpec) Normalize() {
	c.Platform = Platform(strings.TrimSpace(string(c.Platform)))
	if c.Platform == "" {
		c.Platform = DefaultPlatform
	}
	c.ServerAddr = strings.TrimSpace(c.ServerAddr)
	c.Name = strings.TrimSpace(c.Name)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.RobotType = strings.TrimSpace(c.RobotType)
	c.Token = strings.TrimSpace(c.Token)
	c.TLSServerName = strings.TrimSpace(c.TLSServerName)
	if c.ClientID == "" {
		c.ClientID = NewClientID()
	}
	if c.Name == "" {
		c.Name = c.ClientID
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogMaxDays == 0 {
		c.LogMaxDays = 7
	}
	for i := range c.Proxies {
		p := &c.Proxies[i]
		p.Name = strings.TrimSpace(p.Name)
		p.LocalIP = strings.TrimSpace(p.LocalIP)
		p.Subdomain = strings.TrimSpace(p.Subdomain)
		p.SecretKey = strings.TrimSpace(p.SecretKey)
		if p.LocalIP == "" {
			p.LocalIP = "127.0.0.1"
		}
		p.CustomHosts = emptyIfNil(p.CustomHosts)
		p.Locations = emptyIfNil(p.Locations)
	}
	c.Proxies = emptyIfNil(c.Proxies)
}

// Validate 校验客户端配置（01-prd F09 验收：必填项、端口范围、代理名格式、本地地址）。
func (c *ClientSpec) Validate() []error {
	var errs []error

	// 平台先判，且失败就单独给出可选项：它决定后续所有产物的形态，
	// 选错了比填错一个端口更彻底 —— 整包都装不到目标机器上。
	if !c.Platform.Valid() {
		errs = append(errs, invalidf("目标平台 %q 不受支持，仅支持：%s", string(c.Platform), PlatformChoices()))
	}

	if c.ServerAddr == "" {
		errs = append(errs, invalidf("服务器地址不能为空"))
	} else if !reHostName.MatchString(c.ServerAddr) {
		errs = append(errs, invalidf("服务器地址 %q 不是合法的域名或 IP", c.ServerAddr))
	}
	if err := checkPort("服务器端口", c.ServerPort); err != nil {
		errs = append(errs, err)
	}
	if !reSafeIdent.MatchString(c.ClientID) {
		errs = append(errs, invalidf("客户端标识需为 1-64 位，仅可包含字母、数字、点、下划线和连字符，且以字母或数字开头"))
	}
	if c.Token == "" {
		errs = append(errs, invalidf("认证 token 不能为空；服务器端与客户端必须一致"))
	}
	if !ValidRobotType(c.RobotType) {
		errs = append(errs, invalidf("机器人类型 %q 不受支持，仅支持：%s", c.RobotType, RobotTypeChoices()))
	}
	if !validLogLevel(c.LogLevel) {
		errs = append(errs, invalidf("日志级别仅支持 trace/debug/info/warn/error"))
	}
	if c.LogMaxDays < 1 || c.LogMaxDays > 365 {
		errs = append(errs, invalidf("日志保留天数需为 1-365"))
	}

	if len(c.Proxies) == 0 {
		errs = append(errs, invalidf("至少需要配置一条代理"))
	}
	seenName := map[string]bool{}
	seenPort := map[int]string{}
	for i, p := range c.Proxies {
		where := fmt.Sprintf("代理 #%d", i+1)
		if p.Name != "" {
			where = fmt.Sprintf("代理 %q", p.Name)
		}
		if !reSafeIdent.MatchString(p.Name) {
			errs = append(errs, invalidf("%s 的名称需为 1-64 位 ASCII 安全字符，且以字母或数字开头", where))
		} else if seenName[p.Name] {
			errs = append(errs, invalidf("代理名 %q 重复", p.Name))
		} else {
			seenName[p.Name] = true
		}

		if !p.Type.Valid() {
			errs = append(errs, invalidf("%s 的类型 %q 不受支持", where, p.Type))
			continue
		}
		if err := checkPort(where+" 的本地端口", p.LocalPort); err != nil {
			errs = append(errs, err)
		}
		if p.Type.NeedsRemotePort() {
			if err := checkPort(where+" 的远端端口", p.RemotePort); err != nil {
				errs = append(errs, err)
			} else if owner, dup := seenPort[p.RemotePort]; dup {
				errs = append(errs, invalidf("%s 的远端端口 %d 与 %s 重复", where, p.RemotePort, owner))
			} else {
				seenPort[p.RemotePort] = where
			}
		}
		if p.Type.NeedsHostRule() && len(p.CustomHosts) == 0 && p.Subdomain == "" {
			errs = append(errs, invalidf("%s 为 %s 类型，需填写自定义域名或子域", where, p.Type))
		}
		if p.Type.NeedsSecretKey() && p.SecretKey == "" {
			errs = append(errs, invalidf("%s 为 %s 类型，需填写预共享密钥", where, p.Type))
		}
	}
	return errs
}

// Normalize 填充服务器配置的默认值。
func (s *ServerSpec) Normalize() {
	s.BindAddr = strings.TrimSpace(s.BindAddr)
	s.Token = strings.TrimSpace(s.Token)
	s.DashboardAddr = strings.TrimSpace(s.DashboardAddr)
	s.DashboardUser = strings.TrimSpace(s.DashboardUser)
	s.DashboardPassword = strings.TrimSpace(s.DashboardPassword)
	s.SubDomainHost = strings.TrimSpace(s.SubDomainHost)
	if s.BindAddr == "" {
		s.BindAddr = "0.0.0.0"
	}
	if s.BindPort == 0 {
		s.BindPort = 7000
	}
	// Dashboard 默认只监听环回（01-prd F11/§8.5）：管理服务是凭据唯一持有方，
	// 浏览器永不直接访问该端口。
	if s.DashboardAddr == "" {
		s.DashboardAddr = "127.0.0.1"
	}
	if s.DashboardPort == 0 {
		s.DashboardPort = 7500
	}
	if s.DashboardUser == "" {
		s.DashboardUser = "admin"
	}
	if s.DashboardPassword == "" {
		s.DashboardPassword = NewPassword()
	}
	if s.Token == "" {
		s.Token = NewToken()
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	if s.LogMaxDays == 0 {
		s.LogMaxDays = 7
	}
	s.AllowPorts = emptyIfNil(s.AllowPorts)
}

// Validate 校验服务器配置（01-prd F11）。
func (s *ServerSpec) Validate() []error {
	var errs []error
	if s.BindAddr == "" {
		errs = append(errs, invalidf("监听地址不能为空"))
	}
	if err := checkPort("bind 端口", s.BindPort); err != nil {
		errs = append(errs, err)
	}
	if err := checkPort("Dashboard 端口", s.DashboardPort); err != nil {
		errs = append(errs, err)
	}
	if s.BindPort == s.DashboardPort {
		errs = append(errs, invalidf("bind 端口与 Dashboard 端口不能相同（%d）", s.BindPort))
	}
	if s.Token == "" {
		errs = append(errs, invalidf("认证 token 不能为空"))
	}
	if s.DashboardUser == "" {
		errs = append(errs, invalidf("Dashboard 用户名不能为空"))
	}
	if len(s.DashboardPassword) < 12 {
		errs = append(errs, invalidf("Dashboard 密码至少 12 位"))
	}
	if !validLogLevel(s.LogLevel) {
		errs = append(errs, invalidf("日志级别仅支持 trace/debug/info/warn/error"))
	}
	for i, r := range s.AllowPorts {
		if err := checkPort(fmt.Sprintf("端口区间 #%d 的起始", i+1), r.Start); err != nil {
			errs = append(errs, err)
		}
		if err := checkPort(fmt.Sprintf("端口区间 #%d 的结束", i+1), r.End); err != nil {
			errs = append(errs, err)
		}
		if r.Start > r.End {
			errs = append(errs, invalidf("端口区间 #%d 的起始 %d 大于结束 %d", i+1, r.Start, r.End))
		}
	}
	return errs
}

// Warnings 返回不阻止生成、但需要提醒管理员的项（01-prd §8.2）。
//
// 例如 7000/7500 被 FRP 自身占用，而 80/443 在 Windows 上常被 IIS 或
// 其他服务占用；生成包本身没问题，但管理员应提前知道要放行哪些端口。
func (s *ServerSpec) Warnings() []string {
	var out []string
	reserved := map[int]string{
		80:   "常被 IIS 或其他 Web 服务占用",
		443:  "常被 IIS 或其他 Web 服务占用",
		3389: "Windows 远程桌面默认端口",
	}
	for _, p := range []struct {
		name string
		port int
	}{
		{"bind 端口", s.BindPort},
		{"Dashboard 端口", s.DashboardPort},
	} {
		if why, ok := reserved[p.port]; ok {
			out = append(out, fmt.Sprintf("%s %d：%s", p.name, p.port, why))
		}
	}
	if s.DashboardAddr != "127.0.0.1" && s.DashboardAddr != "localhost" {
		out = append(out, fmt.Sprintf(
			"Dashboard 监听 %s 而非 127.0.0.1：该端口将暴露在网络中，请确认已用防火墙限制来源",
			s.DashboardAddr))
	}
	return out
}

func checkPort(label string, p int) error {
	if p < 1 || p > 65535 {
		return invalidf("%s 需在 1-65535 之间，当前为 %d", label, p)
	}
	return nil
}

func validLogLevel(l string) bool {
	switch l {
	case "trace", "debug", "info", "warn", "error":
		return true
	}
	return false
}

// ---------- 密钥生成（01-prd §8.5：密码学安全随机数） ----------

// NewToken 生成 32 字节熵的 FRP 认证 token。
func NewToken() string { return randB64(32) }

// NewClientID 生成客户端标识。
//
// 前缀固定便于运维在 frps 侧一眼认出由本平台签发的客户端；
// 随机段用 base32 风格字符集，避免出现容易被误抄的 0/1/i/l。
func NewClientID() string { return "sf-" + randFrom("abcdefghjkmnpqrstuvwxyz23456789", 12) }

// NewPassword 生成含大小写、数字与符号的 Dashboard 强密码。
//
// 逐类取样再打散，保证每一类至少出现一次，避免随机结果恰好全是字母。
func NewPassword() string {
	const (
		lower  = "abcdefghijkmnopqrstuvwxyz"
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		digit  = "23456789"
		symbol = "!@#$%^&*-_=+"
	)
	all := lower + upper + digit + symbol
	buf := make([]byte, 0, 24)
	buf = append(buf, randFrom(lower, 6)...)
	buf = append(buf, randFrom(upper, 5)...)
	buf = append(buf, randFrom(digit, 5)...)
	buf = append(buf, randFrom(symbol, 3)...)
	buf = append(buf, randFrom(all, 5)...)
	return shuffle(buf)
}

func randB64(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在所有受支持平台上不会失败；真失败说明系统熵源不可用，
		// 此时生成弱密钥比直接失败更危险。
		panic("crypto/rand 不可用: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randFrom(alphabet string, n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	out := make([]byte, n)
	for i, v := range buf {
		// len(alphabet) < 256，取模偏差可忽略；字母表长度均非 2 的幂时
		// 偏差量级约 1e-2，对 token 熵的损失远小于 1 bit。
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out)
}

// shuffle 用 Fisher-Yates 打乱字节切片，掩盖字符类别的位置规律。
func shuffle(b []byte) string {
	for i := len(b) - 1; i > 0; i-- {
		var k [1]byte
		if _, err := rand.Read(k[:]); err != nil {
			panic("crypto/rand 不可用: " + err.Error())
		}
		j := int(k[0]) % (i + 1)
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
