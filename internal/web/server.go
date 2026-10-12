// Package web 提供管理平台的 HTTP 接口与静态资源。
//
// 安全边界（来自 01-prd §8.4/§8.5）：
//   - 管理服务是 FRP Dashboard 凭据的唯一持有方，浏览器永不接触 7500；
//   - 除健康检查、初始化状态与登录外，所有接口都要求有效会话；
//   - 会话 Cookie 为 HttpOnly + SameSite=Strict，写操作要求 JSON 正文，
//     配合「不下发任何 CORS 头」，使跨站请求无法携带凭据完成写操作。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"sinyefrp/internal/auth"
	"sinyefrp/internal/collector"
	"sinyefrp/internal/config"
	"sinyefrp/internal/node"
	"sinyefrp/internal/pack"
	"sinyefrp/internal/store"
)

// maxJSONBody 限制写接口的请求体大小，避免异常请求占用内存。
const maxJSONBody = 64 << 10 // 64 KiB

// 错误分类。与 frpapi.Kind 保持同一命名风格，供前端统一处理。
const (
	errKindBadRequest     = "bad_request"
	errKindUnauthorized   = "unauthorized"
	errKindForbidden      = "forbidden"
	errKindNotFound       = "not_found"
	errKindConflict       = "conflict"
	errKindNotInitialized = "not_initialized"
	errKindRateLimited    = "rate_limited"
	errKindInternal       = "internal"
	errKindUnavailable    = "unavailable"
)

// Deps 是构造 HTTP 服务所需的依赖。
type Deps struct {
	Config    *config.Config
	Store     *store.Store
	Auth      *auth.Service
	Nodes     *node.Manager
	Collector *collector.Collector
	Binaries  *pack.Binaries
	// Self 是当前进程自己的可执行文件，供生成「平台自身部署包」使用（01-prd F18）。
	// 可能读不到（文件被占用/已删除），此时只让那一个接口返回 503，平台其余功能不受影响。
	Self   *pack.Self
	Logger *slog.Logger
}
// Server 聚合 HTTP 依赖。
type Server struct {
	cfg   *config.Config
	st    *store.Store
	auth  *auth.Service
	nodes *node.Manager
	coll  *collector.Collector
	bins  *pack.Binaries
	self  *pack.Self
	log   *slog.Logger
	mux   *http.ServeMux

	// dbPath 是数据库主文件的绝对路径，供设置页「数据存储」区展示。
	//
	// 存绝对路径而不是每次从 cfg 现算：相对路径的解析基准是**配置文件所在目录**
	// （internal/config.resolvePaths），不是进程工作目录；现场排障时
	// 「我用的到底是哪个库」必须一眼可见，不能再靠推测。
	dbPath string
}

// New 构造 HTTP 服务。
func New(d Deps) *Server {
	s := &Server{
		cfg:   d.Config,
		st:    d.Store,
		auth:  d.Auth,
		nodes: d.Nodes,
		coll:  d.Collector,
		bins:  d.Binaries,
		self:  d.Self,
		log:   d.Logger,
		mux:   http.NewServeMux(),
	}
	if d.Config != nil {
		s.dbPath = d.Config.Database.Path
	}
	s.routes()
	return s
}

// Handler 返回可挂载到 http.Server 的处理器。
func (s *Server) Handler() http.Handler {
	// 顺序：panic 兜底 → 安全响应头 → 访问日志 → 路由。
	return s.withRecovery(s.withSecurityHeaders(s.withAccessLog(s.mux)))
}

func (s *Server) routes() {
	// —— 公开接口（无需登录）——
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/setup/status", s.handleSetupStatus)
	s.mux.HandleFunc("POST /api/setup/admin", s.handleSetupAdmin)
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	// —— 需要会话的接口 ——
	s.mux.Handle("GET /api/auth/me", s.protected(s.handleMe))
	s.mux.Handle("POST /api/auth/logout", s.protected(s.handleLogout))

	s.mux.Handle("GET /api/node", s.protected(s.handleNode))
	s.mux.Handle("GET /api/node/config", s.protected(s.handleGetNodeConfig))
	s.mux.Handle("POST /api/node/config", s.protected(s.handleSaveNodeConfig))
	s.mux.Handle("POST /api/node/test", s.protected(s.handleTestNode))

	s.mux.Handle("GET /api/overview", s.protected(s.handleOverview))
	s.mux.Handle("GET /api/clients", s.protected(s.handleClients))
	s.mux.Handle("GET /api/clients/{key}", s.protected(s.handleClientDetail))
	s.mux.Handle("GET /api/clients/{key}/links", s.protected(s.handleClientLinks))
	s.mux.Handle("GET /api/proxies", s.protected(s.handleProxies))
	s.mux.Handle("POST /api/refresh", s.protected(s.handleRefresh))

	s.mux.Handle("GET /api/audit", s.protected(s.handleAudit))
	s.mux.Handle("GET /api/data/status", s.protected(s.handleDataStatus))

	// —— 部署包生成（01-prd F09-F17）——
	s.mux.Handle("GET /api/pkg/meta", s.protected(s.handlePkgMeta))
	s.mux.Handle("GET /api/pkg/endpoint", s.protected(s.handleGetEndpoint))
	s.mux.Handle("POST /api/pkg/endpoint", s.protected(s.handleSaveEndpoint))
	s.mux.Handle("GET /api/pkg/client/defaults", s.protected(s.handleClientDefaults))
	s.mux.Handle("GET /api/pkg/server/defaults", s.protected(s.handleServerDefaults))
	s.mux.Handle("POST /api/pkg/client/preview", s.protected(s.handleClientPreview))
	s.mux.Handle("POST /api/pkg/client", s.protected(s.handleClientDownload))
	s.mux.Handle("POST /api/pkg/server/preview", s.protected(s.handleServerPreview))
	s.mux.Handle("POST /api/pkg/server", s.protected(s.handleServerDownload))
	s.mux.Handle("GET /api/pkg/records", s.protected(s.handlePkgRecords))

	// —— 客户端台账与端口占用（01-prd F19）——
	//
	// 写操作一律用 POST 而不是 PUT/DELETE 之外的动词：
	// decodeJSON 要求 application/json，这一条同时充当轻量 CSRF 防护，
	// 而跨站的 HTML 表单发不出 application/json。DELETE 请求由前端带
	// 正文发出，走的是同一套判定。
	s.mux.Handle("GET /api/registry/clients", s.protected(s.handleRegistryClients))
	// 手工补录台账（没有 POST /api/registry/clients/{id} 那样的 id 段，是新增而非修改）。
	s.mux.Handle("POST /api/registry/clients", s.protected(s.handleRegistryCreate))
	s.mux.Handle("POST /api/registry/clients/{id}", s.protected(s.handleRegistryUpdate))
	s.mux.Handle("DELETE /api/registry/clients/{id}", s.protected(s.handleRegistryDelete))
	s.mux.Handle("GET /api/registry/ports", s.protected(s.handleRegistryPorts))
	// 台账记录 → 打包草稿。「重新生成一个已有客户端的包」的入口。
	s.mux.Handle("GET /api/registry/clients/{id}/spec", s.protected(s.handleRegistrySpec))
	s.mux.Handle("DELETE /api/registry/ports/{id}", s.protected(s.handleRegistryPortRelease))

	// —— 管理平台自身部署包（01-prd F18）——
	// 预览用 GET（无请求体），下载用 POST：与客户端/服务端包保持同一调用形态，
	// 将来若要加参数（例如自定义监听端口）不必改语义。
	s.mux.Handle("GET /api/pkg/platform/preview", s.protected(s.handlePlatformPreview))
	s.mux.Handle("POST /api/pkg/platform", s.protected(s.handlePlatformDownload))

	// —— 前端静态资源与 SPA 回退 ——
	s.mux.HandleFunc("GET /", s.handleStatic)
}

// ---------- 中间件 ----------

// userCtxKey 是注入请求上下文的登录用户。
type userCtxKey struct{}

// currentUser 取出当前登录用户。
func currentUser(ctx context.Context) *store.User {
	u, _ := ctx.Value(userCtxKey{}).(*store.User)
	return u
}

// protected 包装需要登录的处理器。
//
// 未初始化时返回 409 not_initialized（而不是 401），前端据此跳转初始化向导；
// 这样「没登录」与「还没法登录」在界面上是两种不同的引导。
func (s *Server) protected(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.auth.Initialized(r.Context())
		if err != nil {
			s.log.Error("查询初始化状态失败", "err", err)
			writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
			return
		}
		if !ok {
			writeError(w, http.StatusConflict, errKindNotInitialized, "管理平台尚未完成初始化")
			return
		}

		user, err := s.auth.Authenticate(r.Context(), sessionToken(r))
		if err != nil {
			if errors.Is(err, auth.ErrSessionInvalid) {
				writeError(w, http.StatusUnauthorized, errKindUnauthorized, "请先登录")
				return
			}
			s.log.Error("校验会话失败", "err", err)
			writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
			return
		}

		h(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, user)))
	})
}

// withSecurityHeaders 设置基础安全响应头。
//
// 不设置任何 CORS 头是有意为之：同源策略 + 不下发 CORS 头
// 使其他站点无法带着 Cookie 发起可用的跨站请求。
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

// withRecovery 捕获处理器 panic，避免单个请求拖垮服务。
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("处理请求时发生 panic", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withAccessLog 记录访问日志（不记录查询串中的潜在敏感值）。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "took", time.Since(start))
	})
}

// ---------- 辅助 ----------

type errorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	// Details 是可逐条罗列的补充说明（例如逐条端口冲突）。
	//
	// 存在的理由：Message 是要在单行里读出完整意思的摘要，
	// 而像「3 个远端端口都被占了」这种情况，合成一句话会变成一整段
	// 无处下口的文字。前端拿到 Details 后按列表渲染，并可直接把
	// 条数写进标题。为空时不下发该字段。
	Details []string `json:"details,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, errorResponse{Error: errorBody{Kind: kind, Message: msg}})
}

// writeErrorDetails 与 writeError 相同，但额外下发可逐条罗列的细节。
func writeErrorDetails(w http.ResponseWriter, code int, kind, msg string, details []string) {
	writeJSON(w, code, errorResponse{Error: errorBody{Kind: kind, Message: msg, Details: details}})
}

// decodeJSON 解析写接口的请求体。
//
// 同时起到轻量 CSRF 防护的作用：跨站表单无法发出 application/json，
// 而跨站 fetch 会先触发预检，本服务不下发 CORS 头因而被浏览器拦下。
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, errKindBadRequest,
			"请求需使用 application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		msg := "请求内容不是合法 JSON"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			msg = "请求内容过大"
		} else if !errors.Is(err, io.EOF) {
			msg = "请求内容不合法：" + err.Error()
		}
		writeError(w, http.StatusBadRequest, errKindBadRequest, msg)
		return false
	}
	return true
}

// sessionToken 从 Cookie 中取出会话令牌。
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// setSessionCookie 下发会话 Cookie。
//
// Secure 仅在请求本身是 TLS 时设置：默认部署是 http://127.0.0.1，
// 若强制 Secure，浏览器会直接丢弃该 Cookie，导致登录静默失效。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	})
}

// clearSessionCookie 使 Cookie 立即失效。
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// clientIP 提取来源 IP。
//
// 只信任 RemoteAddr：本项目默认没有反向代理，若将来前置 Nginx，
// 必须显式配置可信代理后再解析 X-Forwarded-For，否则该头可被伪造，
// 会导致登录限速被轻易绕过。这里刻意不读任何转发头。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusFor 在从未采集成功时返回 503，避免前端把空数据当成「零客户端」。
func statusFor(st collector.State) int {
	if st.Snapshot == nil {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// timePtr 把零值时间转成 nil，避免序列化出 0001-01-01。
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
