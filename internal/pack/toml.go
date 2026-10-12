package pack

import (
	"bytes"
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// 渲染层刻意与 spec.go 的 API 模型分开：
// API 模型跟着界面字段走（含显示名等非 FRP 概念），
// 渲染模型只包含 FRP 真正认识的键，字段顺序即输出顺序。
//
// 字段名依据 FRP 0.69.x 官方文档。这里不做「字段是否正确」的判断，
// 一切以打包前的 verify -c 结果为准（01-prd F15）——把权威留在 FRP 手里，
// 而不是在本包里复制一份会随版本漂移的定义。

type tomlAuth struct {
	Method string `toml:"method"`
	Token  string `toml:"token"`
}

type tomlTLS struct {
	Enable     bool   `toml:"enable"`
	ServerName string `toml:"serverName,omitempty"`
}

type tomlTransport struct {
	TLS *tomlTLS `toml:"tls,omitempty"`
}

type tomlLog struct {
	To      string `toml:"to"`
	Level   string `toml:"level"`
	MaxDays int    `toml:"maxDays"`
}

type tomlProxyTransport struct {
	UseEncryption  bool `toml:"useEncryption,omitempty"`
	UseCompression bool `toml:"useCompression,omitempty"`
}

type tomlProxy struct {
	// 字段顺序 = TOML 输出顺序，调整顺序会让生成物的 diff 变难读。
	Name      string `toml:"name"`
	Type      string `toml:"type"`
	LocalIP   string `toml:"localIP"`
	LocalPort int    `toml:"localPort"`
	// 远程端口用指针：本 TOML 编码库的 omitempty 对数字类型**不生效**
	// （见 RenderClientTOML 里的详细说明），值类型会被照写成 `remotePort = 0`。
	RemotePort  *int                `toml:"remotePort,omitempty"`
	CustomHosts []string            `toml:"customDomains,omitempty"`
	Subdomain   string              `toml:"subdomain,omitempty"`
	Locations   []string            `toml:"locations,omitempty"`
	SecretKey   string              `toml:"secretKey,omitempty"`
	Transport   *tomlProxyTransport `toml:"transport,omitempty"`
}

type tomlClient struct {
	ServerAddr    string         `toml:"serverAddr"`
	ServerPort    int            `toml:"serverPort"`
	ClientID      string         `toml:"clientID,omitempty"`
	LoginFailExit bool           `toml:"loginFailExit"`
	Auth          tomlAuth       `toml:"auth"`
	Transport     *tomlTransport `toml:"transport,omitempty"`
	Log           tomlLog        `toml:"log"`
	Proxies       []tomlProxy    `toml:"proxies"`
}

type tomlPortRange struct {
	Start int `toml:"start"`
	End   int `toml:"end"`
}

type tomlWebServer struct {
	Addr     string `toml:"addr"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

type tomlServer struct {
	BindAddr      string          `toml:"bindAddr"`
	BindPort      int             `toml:"bindPort"`
	Auth          tomlAuth        `toml:"auth"`
	WebServer     tomlWebServer   `toml:"webServer"`
	Log           tomlLog         `toml:"log"`
	AllowPorts    []tomlPortRange `toml:"allowPorts,omitempty"`
	SubDomainHost string          `toml:"subDomainHost,omitempty"`
}

// RenderClientTOML 生成 frpc.toml 内容。
func RenderClientTOML(c *ClientSpec) ([]byte, error) {
	doc := tomlClient{
		ServerAddr: c.ServerAddr,
		ServerPort: c.ServerPort,
		ClientID:   c.ClientID,
		// 连不上服务器时持续重试而不是退出。
		// frpc 默认值为 true，在「计划任务以 SYSTEM 身份常驻」的部署方式下，
		// 一旦 frps 正在重启就会导致进程直接结束、任务不再拉起，
		// 表现为客户端静默永久离线——这是本平台必须规避的运维陷阱。
		LoginFailExit: false,
		Auth:          tomlAuth{Method: "token", Token: c.Token},
		Log: tomlLog{
			To:      "./frpc.log",
			Level:   c.LogLevel,
			MaxDays: c.LogMaxDays,
		},
	}
	if c.EnableTLS {
		doc.Transport = &tomlTransport{TLS: &tomlTLS{Enable: true, ServerName: c.TLSServerName}}
	}

	doc.Proxies = make([]tomlProxy, 0, len(c.Proxies))
	for _, p := range c.Proxies {
		out := tomlProxy{
			Name:        p.Name,
			Type:        string(p.Type),
			LocalIP:     p.LocalIP,
			LocalPort:   p.LocalPort,
			CustomHosts: p.CustomHosts,
			Subdomain:   p.Subdomain,
			Locations:   p.Locations,
			SecretKey:   p.SecretKey,
		}
		// 远端端口只对 tcp/udp 输出。
		//
		// 这里必须用 *int 而不是 int：本仓库用的 BurntSushi/toml 对
		// `omitempty` 的判定函数 isEmpty 只处理 数组/切片/map/字符串/结构体/
		// 布尔/指针，整数没有分支、直接落到 return false ——
		// 也就是**数字字段的 omitempty 永远不生效**，值类型会被照写成
		// `remotePort = 0`。而 FRP 对 http/https/stcp/xtcp/tcpmux 的代理配置
		// 是严格解码的，多出这个键就直接拒收：
		//     decode proxy at index 0: unmarshal ProxyConfig error:
		//     json: unknown field "remotePort"
		// 结果就是「网站穿透」这类最常用的代理类型反而生成不出可用的包。
		// 实测由 scripts\registry-test.ps1 第 9 段（http 类型）暴露。
		if p.Type.NeedsRemotePort() {
			rp := p.RemotePort
			out.RemotePort = &rp
		}
		if p.Encryption || p.Compression {
			out.Transport = &tomlProxyTransport{
				UseEncryption:  p.Encryption,
				UseCompression: p.Compression,
			}
		}
		doc.Proxies = append(doc.Proxies, out)
	}

	return encodeTOML(doc, "frpc", c.Name)
}

// RenderServerTOML 生成 frps.toml 内容。
func RenderServerTOML(s *ServerSpec) ([]byte, error) {
	doc := tomlServer{
		BindAddr: s.BindAddr,
		BindPort: s.BindPort,
		Auth:     tomlAuth{Method: "token", Token: s.Token},
		WebServer: tomlWebServer{
			Addr:     s.DashboardAddr,
			Port:     s.DashboardPort,
			User:     s.DashboardUser,
			Password: s.DashboardPassword,
		},
		Log: tomlLog{
			To:      "./frps.log",
			Level:   s.LogLevel,
			MaxDays: s.LogMaxDays,
		},
		SubDomainHost: s.SubDomainHost,
	}
	for _, r := range s.AllowPorts {
		doc.AllowPorts = append(doc.AllowPorts, tomlPortRange{Start: r.Start, End: r.End})
	}
	return encodeTOML(doc, "frps", s.DashboardUser)
}

// encodeTOML 在正文前加一段来源注释。
//
// 注释只包含非敏感信息：生成时间与用途。密钥出现在正文里是不可避免的
// （ZIP 本就是敏感件），但注释会被复制粘贴到工单、截图与聊天记录中，
// 因此绝不写入 token / 密码（01-prd §8.5）。
func encodeTOML(doc any, role, name string) ([]byte, error) {
	var body bytes.Buffer
	enc := toml.NewEncoder(&body)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("渲染 %s 配置失败: %w", role, err)
	}

	var out bytes.Buffer
	fmt.Fprintf(&out, "# SinyeFRP 生成的 %s 配置\n", role)
	fmt.Fprintf(&out, "# 生成时间: %s\n", time.Now().Format(time.RFC3339))
	if name != "" {
		fmt.Fprintf(&out, "# 标识: %s\n", name)
	}
	out.WriteString("# 请通过管理平台重新生成，不要手工修改结构性字段。\n\n")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}
