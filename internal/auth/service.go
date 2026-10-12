package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"sinyefrp/internal/store"
)

// SessionCookieName 是会话 Cookie 名。带 __Host- 前缀的 Cookie 会被浏览器
// 强制要求 Secure + Path=/ 且不带 Domain，但管理台默认走 http，
// 因此这里使用普通名，并由服务端强制校验 SameSite。
const SessionCookieName = "sinyefrp_session"

// sessionTokenBytes 是会话令牌的随机字节数（256 位）。
const sessionTokenBytes = 32

// renewThresholdRatio 控制滑动续期的写入频率。
//
// 每次请求都写库没有意义且增加磨损，因此只有距上次活跃超过 TTL 的 1/4 时才续期。
const renewThresholdRatio = 4

// 认证相关错误。
var (
	// ErrAlreadyInitialized 表示管理平台已完成初始化（01-prd F01）。
	ErrAlreadyInitialized = errors.New("管理平台已完成初始化")
	// ErrNotInitialized 表示尚未创建管理员，必须先进初始化向导。
	ErrNotInitialized = errors.New("管理平台尚未初始化")
	// ErrInvalidCredentials 是登录失败的统一对外错误。
	//
	// 刻意不区分「账号不存在」与「密码错误」，对应 01-prd F02 的验收口径。
	ErrInvalidCredentials = errors.New("用户名或密码不正确")
	// ErrSessionInvalid 表示会话不存在、已过期或已被撤销。
	ErrSessionInvalid = errors.New("会话无效或已过期")
)

// RateLimitError 表示因连续失败而被限速。
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("登录失败次数过多，请在 %d 分钟后重试", int(e.RetryAfter.Minutes())+1)
}

// Options 是认证服务的可调参数。
type Options struct {
	SessionTTL       time.Duration
	MaxLoginFailures int
	LockFor          time.Duration
	FailWindow       time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		SessionTTL:       12 * time.Hour,
		MaxLoginFailures: 5,
		LockFor:          15 * time.Minute,
		FailWindow:       10 * time.Minute,
	}
}

// Service 提供初始化、登录、会话校验能力。
type Service struct {
	st  *store.Store
	lim *Limiter
	ttl time.Duration
	log *slog.Logger

	// now 便于测试注入时间。
	now func() time.Time
}

// New 构造认证服务。
func New(st *store.Store, opt Options, log *slog.Logger) *Service {
	if opt.SessionTTL <= 0 {
		opt.SessionTTL = DefaultOptions().SessionTTL
	}
	return &Service{
		st:  st,
		lim: NewLimiter(opt.MaxLoginFailures, opt.FailWindow, opt.LockFor),
		ttl: opt.SessionTTL,
		log: log,
		now: time.Now,
	}
}

// SessionTTL 返回会话有效期。
func (s *Service) SessionTTL() time.Duration { return s.ttl }

// Initialized 报告是否已完成首次初始化（01-prd F01：未初始化不能进入后台）。
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	n, err := s.st.AdminCount(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Setup 创建首个管理员并直接建立会话。
//
// 仅在尚无任何管理员时可用；重复调用返回 ErrAlreadyInitialized，
// 防止初始化接口被用来重置或追加管理员。
func (s *Service) Setup(ctx context.Context, username, password, clientIP, userAgent string) (string, time.Time, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return "", time.Time{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return "", time.Time{}, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return "", time.Time{}, err
	}

	user, err := s.st.CreateFirstAdmin(ctx, username, hash)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyInitialized) {
			return "", time.Time{}, ErrAlreadyInitialized
		}
		return "", time.Time{}, err
	}

	s.log.Info("已创建管理员账号", "username", user.Username, "client_ip", clientIP)
	_ = s.st.AddAudit(ctx, store.Audit{
		Action:   "setup.admin",
		Actor:    user.Username,
		Target:   user.Username,
		Result:   "ok",
		ClientIP: clientIP,
		Detail:   "创建首个管理员账号",
	})

	// 初始化成功后直接登录，避免用户刚设完密码又被要求再输一次。
	return s.issue(ctx, user, clientIP, userAgent)
}

// Login 校验凭据并签发会话。
func (s *Service) Login(ctx context.Context, username, password, clientIP, userAgent string) (string, time.Time, error) {
	username = strings.TrimSpace(username)
	now := s.now()

	// 限速按「用户名 + 来源 IP」双键计数：既挡住针对单账号的爆破，
	// 也挡住同一来源对多个账号的撞库。对外只暴露组合后的结论。
	keys := []string{"user:" + strings.ToLower(username), "ip:" + clientIP}
	for _, k := range keys {
		if ok, retry := s.lim.Allow(k, now); !ok {
			_ = s.st.AddAudit(ctx, store.Audit{
				Action: "auth.login", Actor: username, Target: "session",
				Result: "rate_limited", ClientIP: clientIP,
			})
			return "", time.Time{}, &RateLimitError{RetryAfter: retry}
		}
	}

	user, err := s.st.UserByUsername(ctx, username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// 账号不存在时也消耗一次同代价的哈希比对，抹平响应时间差异。
		burnTime(password)
		s.fail(ctx, keys, username, clientIP, "user_not_found")
		return "", time.Time{}, ErrInvalidCredentials
	case err != nil:
		return "", time.Time{}, err
	}

	if !VerifyPassword(user.PasswordHash, password) {
		s.fail(ctx, keys, username, clientIP, "bad_password")
		return "", time.Time{}, ErrInvalidCredentials
	}

	for _, k := range keys {
		s.lim.Reset(k)
	}
	// 顺手清理过期会话，避免会话表无限增长。
	if n, err := s.st.DeleteExpiredSessions(ctx, now); err == nil && n > 0 {
		s.log.Debug("已清理过期会话", "count", n)
	}
	if err := s.st.TouchLogin(ctx, user.ID, now); err != nil {
		s.log.Warn("更新登录时间失败", "err", err)
	}

	_ = s.st.AddAudit(ctx, store.Audit{
		Action: "auth.login", Actor: user.Username, Target: "session",
		Result: "ok", ClientIP: clientIP,
	})

	return s.issue(ctx, user, clientIP, userAgent)
}

// fail 记录一次失败尝试。
func (s *Service) fail(ctx context.Context, keys []string, username, clientIP, reason string) {
	now := s.now()
	for _, k := range keys {
		s.lim.Fail(k, now)
	}
	_ = s.st.AddAudit(ctx, store.Audit{
		Action: "auth.login", Actor: username, Target: "session",
		Result: "fail", ClientIP: clientIP, Detail: reason,
	})
}

// issue 生成会话令牌并落库，返回明文令牌与到期时间。
func (s *Service) issue(ctx context.Context, user *store.User, clientIP, userAgent string) (string, time.Time, error) {
	token, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}

	now := s.now()
	expires := now.Add(s.ttl)

	// 只存哈希：数据库泄露也无法据此伪造会话。
	if err := s.st.CreateSession(ctx, store.Session{
		TokenHash:  hashToken(token),
		UserID:     user.ID,
		CreatedAt:  now,
		ExpiresAt:  expires,
		LastSeenAt: now,
		ClientIP:   clientIP,
		UserAgent:  truncate(userAgent, 255),
	}); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Logout 立即撤销会话（01-prd F02：退出后会话失效）。
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.st.DeleteSession(ctx, hashToken(token)); err != nil {
		return err
	}
	return nil
}

// Authenticate 校验会话令牌并返回所属管理员，同时做滑动续期。
func (s *Service) Authenticate(ctx context.Context, token string) (*store.User, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}

	sess, err := s.st.SessionByHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}

	now := s.now()
	if !sess.ExpiresAt.IsZero() && now.After(sess.ExpiresAt) {
		_ = s.st.DeleteSession(ctx, sess.TokenHash)
		return nil, ErrSessionInvalid
	}

	// 滑动续期：仅在距上次活跃超过 TTL/4 时才写库。
	if now.Sub(sess.LastSeenAt) > s.ttl/renewThresholdRatio {
		if err := s.st.TouchSession(ctx, sess.TokenHash, now, now.Add(s.ttl)); err != nil {
			s.log.Warn("续期会话失败", "err", err)
		}
	}

	var user *store.User
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT username FROM app_user WHERE id = ?`, sess.UserID)
	if err != nil {
		return nil, fmt.Errorf("查询会话所属管理员失败: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("解析会话所属管理员失败: %w", err)
		}
		user = &store.User{ID: sess.UserID, Username: name}
	}
	if user == nil {
		// 管理员已被删除：会话随之失效。
		_ = s.st.DeleteSession(ctx, sess.TokenHash)
		return nil, ErrSessionInvalid
	}
	return user, nil
}

// EnsureInitialized 在未初始化时返回 ErrNotInitialized，供受保护接口统一前置校验。
func (s *Service) EnsureInitialized(ctx context.Context) error {
	ok, err := s.Initialized(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotInitialized
	}
	return nil
}

// newToken 生成 URL 安全的高熵令牌。
func newToken() (string, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成会话令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken 计算令牌的 SHA-256 十六进制表示。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
