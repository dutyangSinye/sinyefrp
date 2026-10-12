package frpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxResponseBytes 限制单次响应体大小，避免异常服务端拖垮管理平台。
	maxResponseBytes = 4 << 20 // 4 MiB

)

// SupportedVersionPrefixes 是正式支持的 FRP 版本前缀，见 01-prd §9。
//
// 刻意用**集合而不是单个前缀**：管理平台读的是外部 frps 的 Dashboard，
// 而 frps 与平台内嵌的 frpc 是两份独立分发的二进制，升级只能分批做
// ——先升服务端、再逐台重装客户端，过渡期两端版本必然不一致。
// 实测（2026-09-25，.workbuddy/test/_frp071）frps 0.69 + frpc 0.71
// 与 frps 0.71 + frpc 0.69 两组混合均可正常建立隧道，且 serverinfo /
// /api/proxy/{type} 的响应形状逐字节一致，因此两个版本线同时受支持。
// 只留一个前缀会让过渡期必然出现的那一半节点被误报为「不兼容」。
//
// 用 var 而不是 const：切片不是常量表达式。
// 升级 FRP 时只需在这里加一个前缀，判定与全部界面文案一起生效
// （此前 pack 与 frpapi 各存一份前缀、还另有写死的文案，改一处漏一处
// 不会报错，只在界面上留下自相矛盾的提示）。
var SupportedVersionPrefixes = []string{"0.69.", "0.71."}

// Options 描述如何连接一个 frps 节点。
type Options struct {
	BaseURL  string
	Username string
	Password string
	Timeout  time.Duration
}

// Client 是 FRP Dashboard API 的只读客户端。
//
// 它不持有任何业务状态，只负责 HTTP 通信与错误分类；
// 快照、采样与持久化由上层负责。这样即使管理平台停止，
// 已运行的 frps/frpc 也不受影响（01-prd §7.1）。
type Client struct {
	baseURL  string
	username string
	password string
	hc       *http.Client
}

// New 构造客户端。BaseURL 形如 http://127.0.0.1:7500。
func New(opt Options) (*Client, error) {
	raw := strings.TrimRight(strings.TrimSpace(opt.BaseURL), "/")
	if raw == "" {
		return nil, errors.New("frpapi: BaseURL 不能为空")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("frpapi: BaseURL 不合法: %q", opt.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("frpapi: BaseURL 仅支持 http/https: %q", opt.BaseURL)
	}

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	return &Client{
		baseURL:  raw,
		username: opt.Username,
		password: opt.Password,
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}, nil
}

// BaseURL 返回该客户端的节点地址。
func (c *Client) BaseURL() string { return c.baseURL }

// ServerInfo 读取 GET /api/serverinfo。
func (c *Client) ServerInfo(ctx context.Context) (*ServerInfo, error) {
	var out ServerInfo
	if err := c.get(ctx, "/api/serverinfo", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Clients 读取 GET /api/clients。
//
// 返回裸数组，且包含离线客户端；ClientCounts 只统计在线数量。
func (c *Client) Clients(ctx context.Context) ([]ClientInfo, error) {
	var out []ClientInfo
	if err := c.get(ctx, "/api/clients", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Client 读取 GET /api/clients/{key}。
func (c *Client) Client(ctx context.Context, key string) (*ClientInfo, error) {
	var out ClientInfo
	if err := c.get(ctx, "/api/clients/"+url.PathEscape(key), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProxyList 读取 GET /api/proxy/{type}。
//
// 响应为 {"proxies":[...]} 对象包裹，与 /api/clients 的裸数组不同。
func (c *Client) ProxyList(ctx context.Context, ptype string) ([]ProxyInfo, error) {
	if !IsValidProxyType(ptype) {
		return nil, fmt.Errorf("frpapi: 不支持的代理类型 %q", ptype)
	}

	var out proxyListResponse
	if err := c.get(ctx, "/api/proxy/"+url.PathEscape(ptype), &out); err != nil {
		return nil, err
	}
	return out.Proxies, nil
}

// ProxyListAll 依次读取全部白名单类型并合并结果。
//
// 由于离线代理的 conf 为 null，其类型无法从返回值推断，
// 因此调用方需要按 03-frp-api-contract.md §5-③ 自行留存类型。
func (c *Client) ProxyListAll(ctx context.Context) ([]ProxyInfo, error) {
	all := make([]ProxyInfo, 0, 16)
	var firstErr error

	for _, t := range ProxyTypes {
		list, err := c.ProxyList(ctx, t)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		all = append(all, list...)
	}

	// 全部类型都失败才算失败；部分类型无数据不算错误。
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}

// Proxy 读取 GET /api/proxy/{type}/{name}。
//
// name 必须是 API 返回的完整名（user.proxyname），不是表单里填写的短名。
func (c *Client) Proxy(ctx context.Context, ptype, name string) (*ProxyInfo, error) {
	if !IsValidProxyType(ptype) {
		return nil, fmt.Errorf("frpapi: 不支持的代理类型 %q", ptype)
	}

	var out ProxyInfo
	path := "/api/proxy/" + url.PathEscape(ptype) + "/" + url.PathEscape(name)
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Traffic 读取 GET /api/traffic/{name}。
//
// name 必须是完整名，传短名会得到 404。
func (c *Client) Traffic(ctx context.Context, name string) (*Traffic, error) {
	var out Traffic
	if err := c.get(ctx, "/api/traffic/"+url.PathEscape(name), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VersionCompatible 判断 FRP 版本是否在正式支持范围内。
//
// 不在范围内不代表不可用，而是需要先做兼容测试（01-prd §9）。
func VersionCompatible(version string) bool {
	v := strings.TrimSpace(version)
	for _, p := range SupportedVersionPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// SupportedVersionsText 把支持范围拼成人类可读的清单，用于错误与警告文案。
//
// 与 spec.go 的 ProxyTypeChoices / RobotTypeChoices 同取一处定义渲染文案，
// 避免升级版本时文案与判定口径各说各话。
func SupportedVersionsText() string {
	parts := make([]string, 0, len(SupportedVersionPrefixes))
	for _, p := range SupportedVersionPrefixes {
		parts = append(parts, strings.TrimSuffix(p, ".")+".x")
	}
	return strings.Join(parts, " / ")
}

// get 执行一次带 Basic Auth 的 GET 请求，并把各类失败归入 Kind。
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return &Error{Kind: KindBadResponse, Message: "构造请求失败", Err: err}
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Kind: KindBadResponse, Message: "读取 FRP 响应失败", Err: err}
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		// 正常，继续解析。
	case resp.StatusCode == http.StatusUnauthorized:
		return &Error{
			Kind:       KindUnauthorized,
			StatusCode: resp.StatusCode,
			Message:    "FRP Dashboard 认证失败，用户名或密码不正确",
		}
	case resp.StatusCode == http.StatusNotFound:
		return &Error{
			Kind:       KindNotFound,
			StatusCode: resp.StatusCode,
			Message:    errorBodyMessage(body, "请求的 FRP 资源不存在"),
		}
	case resp.StatusCode >= 500:
		return &Error{
			Kind:       KindServerError,
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("FRP Dashboard 返回服务器错误 %d", resp.StatusCode),
		}
	default:
		return &Error{
			Kind:       KindBadResponse,
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("FRP Dashboard 返回异常状态 %d", resp.StatusCode),
		}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{
			Kind:    KindBadResponse,
			Message: "解析 FRP 响应失败，接口形态可能与当前支持的版本不一致",
			Err:     err,
		}
	}
	return nil
}

// errorBodyMessage 尝试从 FRP 的错误响应体里取出可读信息。
//
// 错误体使用大写字段名，例如 {"Code":404,"Msg":"no proxy info found"}。
func errorBodyMessage(body []byte, fallback string) string {
	var eb apiErrorBody
	if err := json.Unmarshal(body, &eb); err == nil && eb.Msg != "" {
		return fmt.Sprintf("%s（FRP 返回：%s）", fallback, eb.Msg)
	}
	return fallback
}
