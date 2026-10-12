// Package collector 周期性从 FRP Dashboard 拉取状态，并维护「最后一次成功快照」。
//
// 对应 01-prd F08 与 §8.4 的四条硬性要求：
//   - 默认 10 秒采样；
//   - 连续失败不覆盖最后一次成功数据；
//   - 记录错误类型和时间；
//   - 超过 30 秒未成功采集即标记为过期。
//
// 关键语义来自 §8.4：API 不可达时节点状态是「未知」，
// 绝不能把全部客户端批量标记为离线（KR5）。
package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"sinyefrp/internal/frpapi"
)

// 采集参数默认值。
const (
	DefaultInterval   = 10 * time.Second
	DefaultStaleAfter = 30 * time.Second
)

// Snapshot 是一次成功采集的完整结果。
type Snapshot struct {
	CollectedAt time.Time
	ServerInfo  *frpapi.ServerInfo
	Clients     []frpapi.ClientInfo
	Proxies     []frpapi.ProxyInfo
}

// ClientStats 是对 Snapshot.Clients 的统计结果。
//
// 之所以要自行统计：FRP 的 serverinfo.clientCounts 只统计「在线」客户端，
// 离线客户端仍保留在 /api/clients 中（见 03-frp-api-contract.md §4-⑨）。
type ClientStats struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
}

// Stats 统计快照中的客户端分布。
func (s *Snapshot) Stats() ClientStats {
	var st ClientStats
	for _, c := range s.Clients {
		st.Total++
		if c.Online {
			st.Online++
		} else {
			st.Offline++
		}
	}
	return st
}

// Health 描述节点当前的采集健康度。
type Health struct {
	// Healthy 表示最近一次采集是否成功。
	Healthy bool
	// Stale 表示距上次成功采集已超过 StaleAfter。
	Stale bool
	// LastSuccess 是最近一次成功采集的时间；零值表示从未成功过。
	LastSuccess time.Time
	// LastAttempt 是最近一次尝试采集的时间。
	LastAttempt time.Time
	// FailStreak 是连续失败次数。
	FailStreak int
	// ErrorKind 与 LastError 描述最近一次失败。
	ErrorKind frpapi.Kind
	LastError string
}

// State 表示节点对外的完整状态。
//
// Snapshot 为 nil 表示从未采集成功过，此时界面必须显示「未知」而非「离线」。
type State struct {
	Health   Health
	Snapshot *Snapshot
}

// Collector 维护采集循环与共享状态。
type Collector struct {
	client     *frpapi.Client
	interval   time.Duration
	staleAfter time.Duration
	// log 用于把采集失败写进平台日志。
	//
	// 这是 2026-09-27 生产上暴露的短板补的：此前失败只更新内存里的健康度，
	// 一行日志都不写，运维看到界面上的「采集异常」后翻遍日志什么也找不到，
	// 只能靠界面上那行红字 —— 而它一旦被关掉就再没有第二个线索。
	log *slog.Logger

	// kick 用于打断等待、立即触发一次采集（切换节点或管理员手动刷新时）。
	kick chan struct{}

	mu    sync.RWMutex
	state State
}

// New 构造采集器。log 传 nil 时使用丢弃输出的 logger（便于单测）。
func New(client *frpapi.Client, interval time.Duration, log *slog.Logger) *Collector {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Collector{
		client:     client,
		interval:   interval,
		staleAfter: DefaultStaleAfter,
		log:        log,
		kick:       make(chan struct{}, 1),
	}
}

// Interval 返回采集间隔。
func (c *Collector) Interval() time.Duration { return c.interval }

// SetClient 热切换底层 FRP 客户端（节点配置变更后调用）。
func (c *Collector) SetClient(cl *frpapi.Client) {
	if cl == nil {
		return
	}
	c.mu.Lock()
	c.client = cl
	c.mu.Unlock()
}

// Reset 丢弃当前快照与健康度，并立即触发一次采集。
//
// 切换节点时必须调用：旧的快照属于另一个 frps，继续展示会误导管理员
// （例如把 A 节点的客户端算到 B 节点头上）。
func (c *Collector) Reset() {
	c.mu.Lock()
	c.state = State{}
	c.mu.Unlock()
	c.Kick()
}

// Kick 请求立即采集一次；若已有待处理的请求则直接返回。
func (c *Collector) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Refresh 触发一次立即采集并等待其完成。
//
// 返回是否在超时前观察到新的采集尝试。界面上的「手动刷新」必须等待，
// 否则用户点完按钮看到的仍是旧快照，会误以为刷新无效。
func (c *Collector) Refresh(wait time.Duration) bool {
	if wait <= 0 {
		wait = 3 * time.Second
	}
	before := c.State().Health.LastAttempt
	c.Kick()

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if after := c.State().Health.LastAttempt; after.After(before) {
			return true
		}
	}
	return false
}

// State 返回当前状态的副本。
//
// Snap 里的切片按值复制，避免调用方读到采集器正在替换的数据。
func (c *Collector) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st := c.state
	st.Health.Stale = st.Health.LastSuccess.IsZero() ||
		time.Since(st.Health.LastSuccess) > c.staleAfter

	if st.Snapshot != nil {
		cp := *st.Snapshot
		cp.Clients = append([]frpapi.ClientInfo(nil), st.Snapshot.Clients...)
		cp.Proxies = append([]frpapi.ProxyInfo(nil), st.Snapshot.Proxies...)
		st.Snapshot = &cp
	}
	return st
}

// Run 阻塞运行采集循环，直到 ctx 被取消。
func (c *Collector) Run(ctx context.Context) {
	// 启动时立即采集一次，避免首屏长时间空白。
	c.CollectOnce(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CollectOnce(ctx)
		case <-c.kick:
			c.CollectOnce(ctx)
		}
	}
}

// CollectOnce 执行一次采集。
//
// 任一请求失败都视为本次采集失败：此时只更新健康度，保留上一次的成功快照。
func (c *Collector) CollectOnce(ctx context.Context) {
	// 在锁内取出当前客户端：SetClient 可能随时替换它。
	c.mu.RLock()
	cl := c.client
	c.mu.RUnlock()
	if cl == nil {
		return
	}

	attemptAt := time.Now()

	info, err := cl.ServerInfo(ctx)
	if err == nil {
		var clients []frpapi.ClientInfo
		clients, err = cl.Clients(ctx)
		if err == nil {
			var proxies []frpapi.ProxyInfo
			proxies, err = cl.ProxyListAll(ctx)
			if err == nil {
				c.mu.Lock()
				// 恢复的时间点必须留痕：只记「从某时开始失败」不记「何时自己好了」，
				// 运维事后复盘就无法判断故障持续了多久、是不是自行恢复的。
				prevStreak := c.state.Health.FailStreak
				wasFailing := prevStreak > 0
				c.state = State{
					Health: Health{
						Healthy:     true,
						LastSuccess: time.Now(),
						LastAttempt: attemptAt,
					},
					Snapshot: &Snapshot{
						CollectedAt: time.Now(),
						ServerInfo:  info,
						Clients:     clients,
						Proxies:     proxies,
					},
				}
				c.mu.Unlock()
				if wasFailing {
					c.log.Info("采集已恢复", "target", cl.BaseURL(),
						"recovered_after_failures", prevStreak)
				}
				return
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// 只更新健康度，state.Snapshot 原样保留。
	c.state.Health.Healthy = false
	c.state.Health.LastAttempt = attemptAt
	c.state.Health.FailStreak++
	c.state.Health.LastError = err.Error()
	c.state.Health.ErrorKind = kindOf(err)

	// 日志节流：采集每 10 秒一轮，连续失败若逐条记录，一天能刷出近万行，
	// 把同一时段发生的其它错误全埋掉（与「systemd 默认无限重启会掩盖真实故障」
	// 是同一类教训）。只在**第一次失败**与之后每 10 次记一条，其余降级为 Debug。
	//
	// 第一次尤其重要：它才是「故障从这一刻开始」的证据，被节流掉就没了。
	streak := c.state.Health.FailStreak
	if streak == 1 || streak%10 == 0 {
		c.log.Warn("采集失败",
			"target", cl.BaseURL(),
			"kind", string(c.state.Health.ErrorKind),
			"err", err.Error(),
			"fail_streak", streak,
			"last_success", lastSuccessText(c.state.Health.LastSuccess),
			"hint", hintFor(c.state.Health.ErrorKind))
	} else {
		c.log.Debug("采集失败（已节流）", "target", cl.BaseURL(),
			"kind", string(c.state.Health.ErrorKind), "fail_streak", streak)
	}
}

// lastSuccessText 把「最后一次成功时间」写成可空字符串。
//
// 从未成功过时写成 RFC3339 的零值（0001-01-01…）会被读成「一千年前成功过一次」，
// 直接给「从未成功」更符合排查时的思路。
func lastSuccessText(t time.Time) string {
	if t.IsZero() {
		return "从未成功"
	}
	return t.Format(time.RFC3339)
}

// hintFor 给出该错误分类下最该先看的那一处。
//
// 分类本身只说明「哪一层坏了」，而运维真正需要的是「下一步看哪里」。
// 这些提示来自本项目实测踩过的坑，不是泛泛的排障建议。
func hintFor(k frpapi.Kind) string {
	switch k {
	case frpapi.KindUnreachable, frpapi.KindTimeout:
		return "检查 frps 是否在运行、节点配置里的 Dashboard 地址与端口是否可达（含防火墙入站）"
	case frpapi.KindUnauthorized:
		return "检查 Dashboard 用户名与密码。注意平台侧的节点密码只认环境变量 SINYEFRP_NODE_PASSWORD，写在 config.toml 里不生效"
	case frpapi.KindNotFound:
		return "Dashboard 地址可能指向了非 FRP 的服务或路径前缀不对"
	case frpapi.KindServerError:
		return "frps 自身返回 5xx，去看 frps 的日志"
	case frpapi.KindIncompatible:
		return "frps 版本不在支持范围内，请升级或降级到受支持的版本"
	case frpapi.KindBadResponse:
		return "响应无法解析，通常是 frps 版本与平台预期不一致"
	}
	return ""
}

// kindOf 提取错误分类；非 frpapi 错误归为「响应异常」。
func kindOf(err error) frpapi.Kind {
	var e *frpapi.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return frpapi.KindBadResponse
}
