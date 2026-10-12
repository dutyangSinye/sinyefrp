// Package node 管理被监控的 frps 节点配置。
//
// 职责边界：
//   - 维护「当前生效」的节点配置（内存中唯一一份），并热切换采集器；
//   - 落库时把 Dashboard 密码加密（internal/secret），绝不明文写盘；
//   - 提供连通性测试，区分连接失败 / 超时 / 401 / 版本不兼容 / 成功。
//
// 对应 01-prd F03，以及 §10.2「首版只管理一个 frps 节点」。
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"sinyefrp/internal/collector"
	"sinyefrp/internal/config"
	"sinyefrp/internal/frpapi"
	"sinyefrp/internal/secret"
	"sinyefrp/internal/store"
)

// DefaultTimeoutSeconds 是节点请求的默认超时。
const DefaultTimeoutSeconds = 5

// Source 说明当前生效配置的来源，用于界面与排障。
type Source string

const (
	// SourceDefault 表示使用内置默认值（尚未配置任何节点）。
	SourceDefault Source = "default"
	// SourceConfig 表示来自 config.toml 或环境变量。
	SourceConfig Source = "config"
	// SourceDB 表示来自数据库（由初始化向导或设置页保存）。
	SourceDB Source = "db"
	// SourceOverride 表示被命令行参数或环境变量临时覆盖（仅本次进程有效）。
	SourceOverride Source = "override"
)

// Settings 是一份节点配置。Password 在内存中以明文存在，仅在落库前加密。
type Settings struct {
	Name           string `json:"name"`
	APIURL         string `json:"api_url"`
	Username       string `json:"username"`
	Password       string `json:"-"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Enabled        bool   `json:"enabled"`
}

// Redacted 返回可安全返回给前端的副本：只保留「是否已设置密码」。
func (s Settings) Redacted() map[string]any {
	return map[string]any{
		"name":                s.Name,
		"api_url":             s.APIURL,
		"username":            s.Username,
		"password_configured": s.Password != "",
		"timeout_seconds":     s.TimeoutSeconds,
		"enabled":             s.Enabled,
	}
}

// Timeout 返回规范化后的超时。
func (s Settings) Timeout() time.Duration {
	if s.TimeoutSeconds <= 0 {
		return DefaultTimeoutSeconds * time.Second
	}
	return time.Duration(s.TimeoutSeconds) * time.Second
}

// Effective 是当前生效配置及其来源。
type Effective struct {
	Settings
	Source Source `json:"source"`
}

// Overrides 是只作用于当前进程的临时覆盖。
//
// 用途：开发期把管理平台指向本机探测实例，而不污染已保存的生产配置。
type Overrides struct {
	APIURL         string
	Username       string
	Password       string
	TimeoutSeconds int
}

// set 报告是否存在任何覆盖。
func (o Overrides) set() bool {
	return o.APIURL != "" || o.Username != "" || o.Password != "" || o.TimeoutSeconds > 0
}

// Manager 持有生效节点配置并管理采集器切换。
type Manager struct {
	st   *store.Store
	coll *collector.Collector
	log  *slog.Logger

	mu  sync.RWMutex
	eff Effective
	ov  Overrides

	// lastPassword 保存内存中的明文密码，供「只改地址不改密码」的保存复用，
	// 避免为了拿回明文而反复解密数据库。
	lastPassword string
}

// NewManager 解析生效配置并应用到采集器。
//
// 解析优先级（由低到高）：
//  1. config.toml 中的 [node]（含内置默认值）
//  2. 数据库中的主节点（由初始化向导或设置页写入）
//  3. 本进程的临时覆盖（命令行参数 / SINYEFRP_NODE_* 环境变量）
func NewManager(ctx context.Context, st *store.Store, coll *collector.Collector,
	cfg *config.Config, ov Overrides, log *slog.Logger) (*Manager, error) {

	m := &Manager{st: st, coll: coll, log: log, ov: ov}

	eff, err := m.resolve(ctx, cfg)
	if err != nil {
		return nil, err
	}
	m.eff = eff
	m.lastPassword = eff.Password

	if err := m.applyToCollector(); err != nil {
		return nil, err
	}

	log.Info("节点配置已生效",
		"source", eff.Source,
		"name", eff.Name,
		"api_url", eff.APIURL,
		"password_configured", eff.Password != "",
		"timeout_seconds", eff.TimeoutSeconds,
	)
	return m, nil
}

// resolve 按优先级合成生效配置。
func (m *Manager) resolve(ctx context.Context, cfg *config.Config) (Effective, error) {
	eff := Effective{
		Settings: Settings{
			Name:           cfg.Node.Name,
			APIURL:         cfg.Node.APIURL,
			Username:       cfg.Node.Username,
			TimeoutSeconds: cfg.Node.Timeout,
			Enabled:        true,
		},
		Source: SourceConfig,
	}
	if eff.Name == "" && eff.APIURL == "" {
		eff.Source = SourceDefault
	}

	// 第二层：数据库（运维在界面上保存过的配置是权威来源）。
	rec, err := m.st.PrimaryNode(ctx)
	switch {
	case err == nil:
		pw, decErr := secret.Decrypt(rec.PasswordEnc)
		if decErr != nil {
			// 解密失败通常意味着换了机器或运行身份（DPAPI 机器范围）。
			// 此时不静默降级成空密码，而是明确报错，避免把问题伪装成 401。
			return Effective{}, fmt.Errorf("解密节点密码失败（可能数据库来自其他机器）: %w", decErr)
		}
		if pw.Legacy {
			m.log.Warn("节点密码以历史明文保存，建议在设置页重新保存以启用加密")
		}
		eff.Settings = Settings{
			Name:           rec.Name,
			APIURL:         rec.APIURL,
			Username:       rec.Username,
			Password:       pw.Plain,
			TimeoutSeconds: rec.TimeoutSeconds,
			Enabled:        rec.Enabled,
		}
		eff.Source = SourceDB
	case errors.Is(err, store.ErrNotFound):
		// 数据库里还没有节点：沿用 config.toml。
	default:
		return Effective{}, err
	}

	// 第三层：临时覆盖。
	if m.ov.set() {
		if m.ov.APIURL != "" {
			eff.APIURL = m.ov.APIURL
		}
		if m.ov.Username != "" {
			eff.Username = m.ov.Username
		}
		if m.ov.Password != "" {
			eff.Password = m.ov.Password
		}
		if m.ov.TimeoutSeconds > 0 {
			eff.TimeoutSeconds = m.ov.TimeoutSeconds
		}
		eff.Source = SourceOverride
	}

	if eff.TimeoutSeconds <= 0 {
		eff.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if eff.Name == "" {
		eff.Name = "local"
	}
	return eff, nil
}

// Current 返回当前生效配置（含明文密码，仅供进程内部使用）。
func (m *Manager) Current() Effective {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.eff
}

// Overridden 报告当前进程是否存在临时覆盖（命令行参数或环境变量）。
//
// 存在覆盖时，界面上保存的配置不会立即改变实际采集目标，
// 因此保存接口需要据此给出「需重启才生效」的提示。
func (m *Manager) Overridden() bool { return m.ov.set() }

// Persisted 报告数据库中是否已保存过节点配置。
func (m *Manager) Persisted(ctx context.Context) (bool, error) {
	_, err := m.st.PrimaryNode(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// applyToCollector 按生效配置重建 FRP 客户端并热切换。
func (m *Manager) applyToCollector() error {
	eff := m.Current()

	cl, err := frpapi.New(frpapi.Options{
		BaseURL:  eff.APIURL,
		Username: eff.Username,
		Password: eff.Password,
		Timeout:  eff.Timeout(),
	})
	if err != nil {
		return fmt.Errorf("构建 FRP 客户端失败: %w", err)
	}
	m.coll.SetClient(cl)
	return nil
}

// Save 持久化节点配置并立即生效。
//
// password 为空表示「保持原密码不变」：界面只返回 password_configured，
// 因此管理员编辑其他字段时无需重新输入密码。
//
// 返回值中的 needsRestart 表示生效配置被临时覆盖挡住（开发期场景），
// 此时本次保存不会改变实际采集目标，需要提示使用者。
func (m *Manager) Save(ctx context.Context, in Settings, actor, clientIP string) (needsRestart bool, err error) {
	in.Name = defaultString(in.Name, "local")
	if err := validate(in); err != nil {
		return false, err
	}
	if in.TimeoutSeconds <= 0 {
		in.TimeoutSeconds = DefaultTimeoutSeconds
	}

	// 解析要落库的密码密文。
	var cipher string
	if in.Password == "" {
		// 保持原密码：读出旧记录重新加密写入，避免因加密不可用而丢凭据。
		old, err := m.st.PrimaryNode(ctx)
		switch {
		case err == nil:
			cipher = old.PasswordEnc
		case errors.Is(err, store.ErrNotFound):
			cipher = ""
		default:
			return false, err
		}
	} else {
		cipher, err = secret.Encrypt(in.Password)
		if err != nil {
			return false, err
		}
	}

	rec, err := m.st.SavePrimaryNode(ctx, store.Node{
		Name:           in.Name,
		APIURL:         in.APIURL,
		Username:       in.Username,
		PasswordEnc:    cipher,
		TimeoutSeconds: in.TimeoutSeconds,
		Enabled:        in.Enabled,
		IsPrimary:      true,
	})
	if err != nil {
		return false, err
	}

	// 生效配置：以刚保存的记录为权威，再叠加本进程的临时覆盖。
	m.mu.Lock()
	m.eff.Settings = Settings{
		Name:           rec.Name,
		APIURL:         rec.APIURL,
		Username:       rec.Username,
		Password:       in.Password,
		TimeoutSeconds: rec.TimeoutSeconds,
		Enabled:        rec.Enabled,
	}
	if m.eff.Password == "" {
		// 未改密码时沿用内存中的旧明文。
		if old := m.lastPassword; old != "" {
			m.eff.Password = old
		}
	}
	if m.ov.set() {
		if m.ov.APIURL != "" {
			m.eff.APIURL = m.ov.APIURL
		}
		if m.ov.Username != "" {
			m.eff.Username = m.ov.Username
		}
		if m.ov.Password != "" {
			m.eff.Password = m.ov.Password
		}
		m.eff.Source = SourceOverride
	} else {
		m.eff.Source = SourceDB
	}
	m.lastPassword = m.eff.Password
	m.mu.Unlock()

	// 若本进程存在临时覆盖（命令行/环境变量），刚保存的配置不会立即生效，
	// 必须显式告知使用者，否则会出现「保存成功但采集目标没变」的困惑。
	needsRestart = m.ov.set()

	if err := m.applyToCollector(); err != nil {
		return false, err
	}

	// 切换节点后旧快照属于另一个节点，必须丢弃后立即重新采集，
	// 否则界面会短暂展示上一个节点的数据。
	m.coll.Reset()

	_ = m.st.AddAudit(ctx, store.Audit{
		Action:   "node.save",
		Actor:    actor,
		Target:   rec.Name,
		Result:   "ok",
		ClientIP: clientIP,
		Detail: fmt.Sprintf("api_url=%s username=%s timeout=%ds password_changed=%t",
			rec.APIURL, rec.Username, rec.TimeoutSeconds, in.Password != ""),
	})
	return needsRestart, nil
}

// TestResult 是一次连通性测试的结果。
//
// 语义区分（01-prd F03）：
//   - OK：连通且认证通过；
//   - Compatible：FRP 版本在正式支持范围内（0.69.x）。
//
// 版本不兼容时 OK 仍为 true——因为接口是可用的，只是未经验证，
// 界面应显示警告而不是「失败」，否则会掩盖真实的连通性结论。
type TestResult struct {
	OK           bool      `json:"ok"`
	Compatible   bool      `json:"compatible"`
	Kind         string    `json:"kind,omitempty"`
	Message      string    `json:"message"`
	FRPVersion   string    `json:"frp_version,omitempty"`
	ClientCount  int       `json:"client_count"`
	ProxyCount   int       `json:"proxy_count"`
	LatencyMS    int64     `json:"latency_ms"`
	TestedAt     time.Time `json:"tested_at"`
	TargetAPIURL string    `json:"target_api_url"`
}

// Test 用给定配置试连一次，不落库、不改变生效配置。
//
// password 为空时沿用当前生效配置的密码，使「只改地址后点测试」也能工作。
func (m *Manager) Test(ctx context.Context, in Settings) TestResult {
	res := TestResult{TestedAt: time.Now()}

	in.APIURL = defaultString(in.APIURL, m.Current().APIURL)
	in.Username = defaultString(in.Username, m.Current().Username)
	if in.Password == "" {
		in.Password = m.Current().Password
	}
	if in.TimeoutSeconds <= 0 {
		in.TimeoutSeconds = m.Current().TimeoutSeconds
	}
	if in.TimeoutSeconds <= 0 {
		in.TimeoutSeconds = DefaultTimeoutSeconds
	}
	res.TargetAPIURL = in.APIURL

	if err := validate(in); err != nil {
		res.Kind = string(frpapi.KindBadResponse)
		res.Message = err.Error()
		return res
	}

	cl, err := frpapi.New(frpapi.Options{
		BaseURL:  in.APIURL,
		Username: in.Username,
		Password: in.Password,
		Timeout:  in.Timeout(),
	})
	if err != nil {
		res.Kind = string(frpapi.KindBadResponse)
		res.Message = err.Error()
		return res
	}

	// 测试用独立超时，避免节点极慢时把界面卡死。
	testCtx, cancel := context.WithTimeout(ctx, in.Timeout()+5*time.Second)
	defer cancel()

	start := time.Now()
	info, err := cl.ServerInfo(testCtx)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Kind = string(kindOf(err))
		res.Message = err.Error()
		return res
	}

	res.OK = true
	res.FRPVersion = info.Version
	res.Compatible = frpapi.VersionCompatible(info.Version)

	if clients, err := cl.Clients(testCtx); err == nil {
		res.ClientCount = len(clients)
	}
	if proxies, err := cl.ProxyListAll(testCtx); err == nil {
		res.ProxyCount = len(proxies)
	}

	switch {
	case !res.Compatible:
		res.Kind = string(frpapi.KindIncompatible)
		// 范围文案取 frpapi 的同一处定义：写死「0.69.x」会在升级后
		// 与实际判定口径不一致，界面上出现自相矛盾的提示。
		res.Message = fmt.Sprintf("连接成功，但 FRP 版本 %s 不在正式支持范围（%s），请先做兼容性验证",
			info.Version, frpapi.SupportedVersionsText())
	default:
		res.Message = fmt.Sprintf("连接成功，FRP %s，当前 %d 个客户端、%d 个代理",
			info.Version, res.ClientCount, res.ProxyCount)
	}
	return res
}

// kindOf 提取错误分类；非 frpapi 错误归为「响应异常」。
func kindOf(err error) frpapi.Kind {
	var e *frpapi.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return frpapi.KindBadResponse
}

// validate 校验节点配置。
func validate(s Settings) error {
	if s.APIURL == "" {
		return errors.New("FRP Dashboard 地址不能为空")
	}
	if _, err := frpapi.New(frpapi.Options{BaseURL: s.APIURL}); err != nil {
		return errors.New("FRP Dashboard 地址不合法，需形如 http://127.0.0.1:7500")
	}
	if s.TimeoutSeconds < 0 || s.TimeoutSeconds > 300 {
		return errors.New("超时需在 0-300 秒之间")
	}
	if len(s.Name) > 64 {
		return errors.New("节点名称不能超过 64 个字符")
	}
	return nil
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
