package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"sinyefrp/internal/auth"
	"sinyefrp/internal/store"
)

// ---------- 初始化（01-prd F01）----------

// setupStatusResponse 让前端判断该进登录页还是初始化向导。
//
// 只暴露布尔值与公开的策略常量，不泄露任何账号信息。
type setupStatusResponse struct {
	Initialized bool           `json:"initialized"`
	Policy      passwordPolicy `json:"policy"`
	SessionTTL  int            `json:"session_ttl_seconds"`
}

// passwordPolicy 把口令规则下发给前端，避免前后端两套校验各说各话。
type passwordPolicy struct {
	MinUsernameLen int `json:"min_username_len"`
	MaxUsernameLen int `json:"max_username_len"`
	MinPasswordLen int `json:"min_password_len"`
	MaxPasswordLen int `json:"max_password_len"`
}

func currentPolicy() passwordPolicy {
	return passwordPolicy{
		MinUsernameLen: auth.MinUsernameLen,
		MaxUsernameLen: auth.MaxUsernameLen,
		MinPasswordLen: auth.MinPasswordLen,
		MaxPasswordLen: 72, // bcrypt 只取前 72 字节
	}
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ok, err := s.auth.Initialized(r.Context())
	if err != nil {
		s.log.Error("查询初始化状态失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
		return
	}
	writeJSON(w, http.StatusOK, setupStatusResponse{
		Initialized: ok,
		Policy:      currentPolicy(),
		SessionTTL:  int(s.auth.SessionTTL() / time.Second),
	})
}

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	ip := clientIP(r)
	token, expires, err := s.auth.Setup(r.Context(), req.Username, req.Password, ip, r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrAlreadyInitialized):
			// 已初始化后该接口永久失效，避免被用来追加或重置管理员。
			writeError(w, http.StatusConflict, errKindConflict, "管理平台已完成初始化")
		case errors.Is(err, auth.ErrWeakUsername),
			errors.Is(err, auth.ErrWeakPassword),
			errors.Is(err, auth.ErrWeakPasswordClasses):
			writeError(w, http.StatusBadRequest, errKindBadRequest, err.Error())
		default:
			s.log.Error("初始化失败", "err", err)
			writeError(w, http.StatusInternalServerError, errKindInternal, "初始化失败，请查看服务日志")
		}
		return
	}

	s.setSessionCookie(w, r, token, expires)
	s.log.Info("管理平台初始化完成", "client_ip", ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       map[string]any{"username": req.Username},
		"expires_at": expires,
	})
}

// ---------- 登录（01-prd F02）----------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	ip := clientIP(r)
	token, expires, err := s.auth.Login(r.Context(), req.Username, req.Password, ip, r.UserAgent())
	if err != nil {
		var rl *auth.RateLimitError
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			// 统一话术，不透露账号是否存在。
			writeError(w, http.StatusUnauthorized, errKindUnauthorized, "用户名或密码不正确")
		case errors.As(err, &rl):
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, errKindRateLimited, rl.Error())
		default:
			s.log.Error("登录失败", "err", err, "client_ip", ip)
			writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
		}
		return
	}

	s.setSessionCookie(w, r, token, expires)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       map[string]any{"username": req.Username},
		"expires_at": expires,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), sessionToken(r)); err != nil {
		s.log.Warn("退出登录时撤销会话失败", "err", err)
	}
	s.clearSessionCookie(w, r)

	if user := currentUser(r.Context()); user != nil {
		s.audit(r, "auth.logout", "session", "ok", "")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMe 返回当前登录身份，供前端刷新页面后恢复状态。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r.Context())

	// 附带生效节点来源，前端可据此提示「配置被命令行/环境变量临时覆盖」。
	eff := s.nodes.Current()

	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{"username": user.Username},
		"node": map[string]any{
			"name":   eff.Name,
			"source": eff.Source,
		},
	})
}

// ---------- 审计辅助 ----------

// audit 写入一条审计记录。
//
// 审计失败不影响业务，因此只记警告：把日志失败放大成接口失败，
// 会让「操作系统日志」变成可用性的单点。
func (s *Server) audit(r *http.Request, action, target, result, detail string) {
	actor := ""
	if u := currentUser(r.Context()); u != nil {
		actor = u.Username
	}
	if err := s.st.AddAudit(r.Context(), store.Audit{
		Action:   action,
		Actor:    actor,
		Target:   target,
		Result:   result,
		ClientIP: clientIP(r),
		Detail:   detail,
	}); err != nil {
		s.log.Warn("写入审计失败", "action", action, "err", err)
	}
}
