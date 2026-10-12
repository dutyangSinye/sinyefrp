// Package endpoint 管理「客户端接入点」：客户端部署包要连接的对外地址、
// 对外端口与 FRP auth token。
//
// 为什么不复用 node 配置（frps Dashboard 的接入信息）：
// 二者描述的是同一台 frps，但用途和取值都可能不同——
//   - node.api_url 是管理平台去采集状态时访问的地址，通常走环回或内网；
//   - 接入点是 frpc（可能装在任意一台外网机器上）要连接的地址。
//
// frps 前面存在 NAT 映射时，连端口都不一样：内部 bindPort = 7000，
// 而客户端实际连的是对外映射的 12123。把两者混在一处，
// 会让「改采集地址」意外改掉打包默认值，也会让人误以为 Dashboard 采集到的
// bindPort 就是客户端该用的端口。
//
// token 是两端必须一致的共享密钥：frps 的 auth.token 与 frpc 的 auth.token
// 不一致时 frpc 直接连不上。平台把它作为唯一权威值保存，客户端包与服务端包
// 都从这里取，避免出现「两次生成各随机一个 token、装到机器上才发现连不通」。
package endpoint

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"sinyefrp/internal/secret"
	"sinyefrp/internal/store"
)

// setting 表中的存储键。
const (
	keyAddr  = "pack.endpoint.addr"
	keyPort  = "pack.endpoint.port"
	keyToken = "pack.endpoint.token_enc"
)

// DefaultPort 是接入点未显式配置端口时的回退值，
// 与 FRP 自身的 bindPort 默认值一致。
const DefaultPort = 7000

// Endpoint 是客户端接入点配置。
type Endpoint struct {
	Addr  string // 客户端要连接的地址，域名或 IP 字面量
	Port  int    // 客户端要连接的端口
	Token string // FRP auth token，需与 frps 的 auth.token 一致
}

// Configured 表示是否已配置。以地址为准：只有端口没有地址无法连接。
func (e Endpoint) Configured() bool { return strings.TrimSpace(e.Addr) != "" }

// PortOrDefault 在未配置端口时回退到 DefaultPort。
func (e Endpoint) PortOrDefault() int {
	if e.Port > 0 {
		return e.Port
	}
	return DefaultPort
}

// Redacted 返回可安全下发给前端的副本：token 只暴露「是否已配置」，
// 不返回明文，也不返回密文（01-prd F17）。
func (e Endpoint) Redacted() map[string]any {
	return map[string]any{
		"client_addr":      e.Addr,
		"client_port":      e.Port,
		"token_configured": e.Token != "",
		"configured":       e.Configured(),
	}
}

// DecryptError 表示 token 已存储但无法解密。
//
// 最常见的原因是换了机器：DPAPI 的密文与写入它的机器（及用户）绑定，
// 把 SQLite 文件拷到另一台机器后密文就解不开了。
//
// Load 会在返回它的同时把 Token 留空而不是直接失败——否则管理员
// 连地址和端口都读不出来，也就无法在界面上重新填写 token。
type DecryptError struct{ Err error }

func (e *DecryptError) Error() string {
	return "接入点 token 无法解密（密文与写入它的机器绑定，可能需要重新填写）: " + e.Err.Error()
}

func (e *DecryptError) Unwrap() error { return e.Err }

// Load 读取接入点配置。未配置时返回零值，不报错。
func Load(ctx context.Context, st *store.Store) (Endpoint, error) {
	var e Endpoint

	addr, err := st.Setting(ctx, keyAddr)
	if err != nil {
		return e, err
	}
	e.Addr = strings.TrimSpace(addr)

	portStr, err := st.Setting(ctx, keyPort)
	if err != nil {
		return e, err
	}
	if portStr = strings.TrimSpace(portStr); portStr != "" {
		p, convErr := strconv.Atoi(portStr)
		if convErr != nil {
			return e, fmt.Errorf("接入端口存储值不是合法整数: %q", portStr)
		}
		e.Port = p
	}

	enc, err := st.Setting(ctx, keyToken)
	if err != nil {
		return e, err
	}
	if strings.TrimSpace(enc) != "" {
		res, decErr := secret.Decrypt(enc)
		if decErr != nil {
			return e, &DecryptError{Err: decErr}
		}
		e.Token = res.Plain
	}

	return e, nil
}

// SaveInput 是保存接入点的输入。
//
// Token 为空表示「保持原 token 不变」——前端只拿得到 token_configured，
// 管理员改地址或端口时不该被迫重新输入 token。
// 需要真正清空 token（例如 frps 侧没有配 auth.token）时显式传 ClearToken。
type SaveInput struct {
	Addr       string
	Port       int
	Token      string
	ClearToken bool
}

// Save 校验并写入接入点配置，返回落库后重新读取的结果。
//
// 先完成全部校验与加密封装，再逐键写入：宁可整组不写，
// 也不要出现「地址更新了但 token 还是旧的」这种半写状态。
func Save(ctx context.Context, st *store.Store, in SaveInput) (Endpoint, error) {
	addr := strings.TrimSpace(in.Addr)
	if !validAddr(addr) {
		return Endpoint{}, fmt.Errorf("接入地址不合法：%q（应填域名或 IP，不含端口与路径）", in.Addr)
	}
	if in.Port != 0 && (in.Port < 1 || in.Port > 65535) {
		return Endpoint{}, fmt.Errorf("接入端口必须在 1-65535 之间，收到 %d", in.Port)
	}

	token := strings.TrimSpace(in.Token)
	if strings.ContainsAny(token, " \t\r\n") {
		return Endpoint{}, fmt.Errorf("接入 token 不允许包含空白字符")
	}

	var tokenEnc string
	switch {
	case token != "":
		enc, err := secret.Encrypt(token)
		if err != nil {
			return Endpoint{}, err
		}
		tokenEnc = enc
	case in.ClearToken:
		tokenEnc = ""
	default:
		cur, err := st.Setting(ctx, keyToken)
		if err != nil {
			return Endpoint{}, err
		}
		tokenEnc = cur
	}

	portStr := ""
	if in.Port != 0 {
		portStr = strconv.Itoa(in.Port)
	}

	if err := st.SetSetting(ctx, keyAddr, addr); err != nil {
		return Endpoint{}, err
	}
	if err := st.SetSetting(ctx, keyPort, portStr); err != nil {
		return Endpoint{}, err
	}
	if err := st.SetSetting(ctx, keyToken, tokenEnc); err != nil {
		return Endpoint{}, err
	}

	return Load(ctx, st)
}

// reHost 与 pack 的 reHostName 保持同一套字符集约束：
// 只允许字母数字与 . - ，且首尾不能是分隔符。
var reHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// validAddr 校验接入地址。空值视为「未配置」，合法。
//
// 允许域名、IPv4 与 IPv6 字面量；不在此处解析 DNS——
// 管理员可能填一个稍后才建好的内网域名，当场解析失败不代表配置错误。
func validAddr(s string) bool {
	if s == "" {
		return true
	}
	if net.ParseIP(s) != nil {
		return true
	}
	return reHost.MatchString(s)
}
