package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("记录不存在")

// ---------- 管理员账号 ----------

// AdminCount 返回管理员数量，用于判断是否已完成首次初始化（01-prd F01）。
func (s *Store) AdminCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM app_user`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计管理员数量失败: %w", err)
	}
	return n, nil
}

// CreateUser 创建管理员账号，返回带 ID 的实体。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	now := time.Now()
	at := formatTime(now)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO app_user(username, password_hash, created_at, updated_at) VALUES(?,?,?,?)`,
		username, passwordHash, at, at)
	if err != nil {
		return nil, fmt.Errorf("创建管理员失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取管理员 ID 失败: %w", err)
	}
	return &User{
		ID:           id,
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// UserByUsername 按用户名查询管理员。
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	var (
		u       User
		created string
		updated string
		last    sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, updated_at, last_login_at
		   FROM app_user WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &created, &updated, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询管理员失败: %w", err)
	}
	u.CreatedAt = parseTime(created)
	u.UpdatedAt = parseTime(updated)
	u.LastLoginAt = parseNullTime(last)
	return &u, nil
}

// TouchLogin 记录一次成功登录时间。
func (s *Store) TouchLogin(ctx context.Context, userID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE app_user SET last_login_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(at), formatTime(at), userID)
	if err != nil {
		return fmt.Errorf("更新登录时间失败: %w", err)
	}
	return nil
}

// ---------- 会话 ----------

// CreateSession 写入一条会话记录（存 token 哈希，不存明文）。
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session(token_hash, user_id, created_at, expires_at, last_seen_at, client_ip, user_agent)
		 VALUES(?,?,?,?,?,?,?)`,
		sess.TokenHash, sess.UserID,
		formatTime(sess.CreatedAt), formatTime(sess.ExpiresAt), formatTime(sess.LastSeenAt),
		sess.ClientIP, sess.UserAgent)
	if err != nil {
		return fmt.Errorf("创建会话失败: %w", err)
	}
	return nil
}

// SessionByHash 按 token 哈希查询未过期会话。
func (s *Store) SessionByHash(ctx context.Context, tokenHash string) (*Session, error) {
	var (
		sess    Session
		created string
		expires string
		seen    string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, expires_at, last_seen_at, client_ip, user_agent
		   FROM session WHERE token_hash = ?`, tokenHash).
		Scan(&sess.TokenHash, &sess.UserID, &created, &expires, &seen, &sess.ClientIP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询会话失败: %w", err)
	}
	sess.CreatedAt = parseTime(created)
	sess.ExpiresAt = parseTime(expires)
	sess.LastSeenAt = parseTime(seen)
	return &sess, nil
}

// TouchSession 续期会话：滑动过期，避免管理员操作中被登出。
func (s *Store) TouchSession(ctx context.Context, tokenHash string, at, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`,
		formatTime(at), formatTime(expires), tokenHash)
	if err != nil {
		return fmt.Errorf("续期会话失败: %w", err)
	}
	return nil
}

// DeleteSession 使单个会话立即失效（退出登录）。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("删除会话失败: %w", err)
	}
	return nil
}

// DeleteSessionsByUser 使某管理员的全部会话失效（改密等场景）。
func (s *Store) DeleteSessionsByUser(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("删除管理员会话失败: %w", err)
	}
	return nil
}

// DeleteExpiredSessions 清理过期会话，返回清理条数。
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("清理过期会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// ---------- 节点 ----------

// PrimaryNode 返回当前启用节点。
//
// 首版只允许一个节点（01-prd F03/§9），因此这里取 is_primary=1 的那条。
// 表结构已预留多节点，升级到 P1 时无需迁移数据。
func (s *Store) PrimaryNode(ctx context.Context) (*Node, error) {
	var (
		n       Node
		created string
		updated string
		enabled int
		primary int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, api_url, username, password_enc, timeout_seconds,
		        enabled, is_primary, created_at, updated_at
		   FROM node WHERE is_primary = 1
		  ORDER BY id LIMIT 1`).
		Scan(&n.ID, &n.Name, &n.APIURL, &n.Username, &n.PasswordEnc, &n.TimeoutSeconds,
			&enabled, &primary, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询节点失败: %w", err)
	}
	n.Enabled = enabled != 0
	n.IsPrimary = primary != 0
	n.CreatedAt = parseTime(created)
	n.UpdatedAt = parseTime(updated)
	return &n, nil
}

// SavePrimaryNode 写入或更新唯一的主节点配置。
func (s *Store) SavePrimaryNode(ctx context.Context, n Node) (*Node, error) {
	now := time.Now()
	at := formatTime(now)

	existing, err := s.PrimaryNode(ctx)
	switch {
	case err == nil:
		if _, err := s.db.ExecContext(ctx,
			`UPDATE node SET name=?, api_url=?, username=?, password_enc=?,
			        timeout_seconds=?, enabled=?, updated_at=?
			  WHERE id=?`,
			n.Name, n.APIURL, n.Username, n.PasswordEnc,
			n.TimeoutSeconds, boolToInt(n.Enabled), at, existing.ID); err != nil {
			return nil, fmt.Errorf("更新节点失败: %w", err)
		}
		existing.Name = n.Name
		existing.APIURL = n.APIURL
		existing.Username = n.Username
		existing.PasswordEnc = n.PasswordEnc
		existing.TimeoutSeconds = n.TimeoutSeconds
		existing.Enabled = n.Enabled
		existing.UpdatedAt = now
		return existing, nil
	case errors.Is(err, ErrNotFound):
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO node(name, api_url, username, password_enc, timeout_seconds,
			                  enabled, is_primary, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,1,?,?)`,
			n.Name, n.APIURL, n.Username, n.PasswordEnc, n.TimeoutSeconds,
			boolToInt(n.Enabled), at, at)
		if err != nil {
			return nil, fmt.Errorf("创建节点失败: %w", err)
		}
		id, _ := res.LastInsertId()
		n.ID = id
		n.IsPrimary = true
		n.CreatedAt = now
		n.UpdatedAt = now
		return &n, nil
	default:
		return nil, err
	}
}

// ---------- 设置（键值） ----------

// Setting 读取设置项；不存在时返回空串。
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM setting WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取设置 %s 失败: %w", key, err)
	}
	return v, nil
}

// SetSetting 写入设置项。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO setting(key, value, updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowText())
	if err != nil {
		return fmt.Errorf("写入设置 %s 失败: %w", key, err)
	}
	return nil
}

// ---------- 审计 ----------

// AddAudit 追加一条审计记录。审计失败不应中断业务，因此调用方通常只记录警告。
func (s *Store) AddAudit(ctx context.Context, a Audit) error {
	if a.At.IsZero() {
		a.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit(at, actor, action, target, result, client_ip, detail)
		 VALUES(?,?,?,?,?,?,?)`,
		formatTime(a.At), a.Actor, a.Action, a.Target, a.Result, a.ClientIP, a.Detail)
	if err != nil {
		return fmt.Errorf("写入审计失败: %w", err)
	}
	return nil
}

// ListAudit 按时间倒序返回最近 limit 条审计记录。
func (s *Store) ListAudit(ctx context.Context, limit int) ([]Audit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, actor, action, target, result, client_ip, detail
		   FROM audit ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询审计失败: %w", err)
	}
	defer rows.Close()

	out := make([]Audit, 0, limit)
	for rows.Next() {
		var (
			a  Audit
			at string
		)
		if err := rows.Scan(&a.ID, &at, &a.Actor, &a.Action, &a.Target,
			&a.Result, &a.ClientIP, &a.Detail); err != nil {
			return nil, fmt.Errorf("解析审计记录失败: %w", err)
		}
		a.At = parseTime(at)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审计记录失败: %w", err)
	}
	return out, nil
}

// ---------- 部署包记录 ----------

// AddPkgRecord 记录一次部署包生成。
func (s *Store) AddPkgRecord(ctx context.Context, p PkgRecord) error {
	if p.At.IsZero() {
		p.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pkg_record(at, actor, kind, client_id, display_name, frp_version, exe_sha256, summary)
		 VALUES(?,?,?,?,?,?,?,?)`,
		formatTime(p.At), p.Actor, p.Kind, p.ClientID, p.DisplayName,
		p.FRPVersion, p.ExeSHA256, p.Summary)
	if err != nil {
		return fmt.Errorf("写入部署包记录失败: %w", err)
	}
	return nil
}

// ListPkgRecords 按时间倒序返回最近的部署包记录。
func (s *Store) ListPkgRecords(ctx context.Context, limit int) ([]PkgRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, actor, kind, client_id, display_name, frp_version, exe_sha256, summary
		   FROM pkg_record ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询部署包记录失败: %w", err)
	}
	defer rows.Close()

	out := make([]PkgRecord, 0, limit)
	for rows.Next() {
		var (
			p  PkgRecord
			at string
		)
		if err := rows.Scan(&p.ID, &at, &p.Actor, &p.Kind, &p.ClientID,
			&p.DisplayName, &p.FRPVersion, &p.ExeSHA256, &p.Summary); err != nil {
			return nil, fmt.Errorf("解析部署包记录失败: %w", err)
		}
		p.At = parseTime(at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历部署包记录失败: %w", err)
	}
	return out, nil
}

// ErrAlreadyInitialized 表示已存在管理员账号，不允许再次初始化。
var ErrAlreadyInitialized = errors.New("管理平台已完成初始化")

// CreateFirstAdmin 在「尚无任何管理员」的前提下创建首个管理员。
//
// 判定与写入在同一个事务内完成，避免并发的初始化请求绕过 01-prd F01
// 的「未初始化才可初始化」约束、凭空多出不受控的管理员账号。
func (s *Store) CreateFirstAdmin(ctx context.Context, username, passwordHash string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开始初始化事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM app_user`).Scan(&n); err != nil {
		return nil, fmt.Errorf("统计管理员数量失败: %w", err)
	}
	if n > 0 {
		return nil, ErrAlreadyInitialized
	}

	now := time.Now()
	at := formatTime(now)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO app_user(username, password_hash, created_at, updated_at) VALUES(?,?,?,?)`,
		username, passwordHash, at, at)
	if err != nil {
		return nil, fmt.Errorf("创建管理员失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取管理员 ID 失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交初始化事务失败: %w", err)
	}

	return &User{
		ID:           id,
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// ---------- 客户端台账（01-prd F19） ----------

// UpsertClient 记录一次客户端包签发：不存在则新建，存在则更新并把 generate_count 加一。
//
// ports 是本次配置里全部代理的端口占用，整体替换该客户端的旧记录 ——
// 管理员重新生成时可能删掉了某条代理，那它原先占的端口就必须跟着释放，
// 否则会被一条已经不存在于任何包里的记录永久锁住。
//
// 已在台账里的 retired 客户端被重新生成时自动恢复为 active：
// 管理员既然又在给它出包，说明这台设备要重新上线，端口理应重新受保护。
func (s *Store) UpsertClient(ctx context.Context, c ClientRegistry, ports []ClientPort) (*ClientRegistry, error) {
	if c.ClientID == "" {
		return nil, fmt.Errorf("clientID 不能为空")
	}
	if c.Status == "" {
		c.Status = ClientStatusActive
	}

	now := time.Now()
	at := formatTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开始台账事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id      int64
		count   int
		first   sql.NullString
		created string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, generate_count, first_generated_at, created_at
		   FROM client_registry WHERE client_id = ?`, c.ClientID).
		Scan(&id, &count, &first, &created)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, ierr := tx.ExecContext(ctx,
			`INSERT INTO client_registry(client_id, display_name, robot_type, platform,
			                             server_addr, server_port,
			                             status, note, generate_count, first_generated_at,
			                             last_generated_at, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,1,?,?,?,?)`,
			c.ClientID, c.DisplayName, c.RobotType, c.Platform, c.ServerAddr, c.ServerPort,
			c.Status, c.Note, at, at, at, at)
		if ierr != nil {
			return nil, fmt.Errorf("新增台账记录失败: %w", ierr)
		}
		if id, ierr = res.LastInsertId(); ierr != nil {
			return nil, fmt.Errorf("读取台账记录 ID 失败: %w", ierr)
		}
		count = 1
		c.FirstGeneratedAt = now
	case err != nil:
		return nil, fmt.Errorf("查询台账记录失败: %w", err)
	default:
		// note 不覆盖：它是管理员自己写的备注，与本次表单无关。
		// robot_type 则覆盖：它来自本次表单，管理员在表单里改了类型就应该生效。
		if _, uerr := tx.ExecContext(ctx,
			// platform 与 robot_type 同样对待：都来自本次表单，管理员改了就该生效
			// （note 相反 —— 它是管理员自己写的备注，不能被表单覆盖）。
			`UPDATE client_registry
			    SET display_name = ?, robot_type = ?, platform = ?, server_addr = ?, server_port = ?,
			        status = ?, generate_count = ?, last_generated_at = ?, updated_at = ?
			  WHERE id = ?`,
			c.DisplayName, c.RobotType, c.Platform, c.ServerAddr, c.ServerPort,
			c.Status, count+1, at, at, id); uerr != nil {
			return nil, fmt.Errorf("更新台账记录失败: %w", uerr)
		}
		count++
		if first.Valid {
			c.FirstGeneratedAt = parseTime(first.String)
		}
		c.CreatedAt = parseTime(created)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM client_port WHERE client_id = ?`, c.ClientID); err != nil {
		return nil, fmt.Errorf("清理旧端口占用失败: %w", err)
	}
	for _, p := range ports {
		if p.ProxyName == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO client_port(client_id, proxy_name, proxy_type, remote_port, local_port, updated_at)
			 VALUES(?,?,?,?,?,?)`,
			c.ClientID, p.ProxyName, p.ProxyType, p.RemotePort, p.LocalPort, at); err != nil {
			return nil, fmt.Errorf("写入端口占用失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交台账事务失败: %w", err)
	}

	c.ID = id
	c.GenerateCount = count
	c.LastGeneratedAt = now
	c.UpdatedAt = now
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	return &c, nil
}

// ErrClientExists 表示要手工登记的 clientID 已经在台账里。
var ErrClientExists = errors.New("该 clientID 已在台账中")

// CreateClient 手工新增一条台账记录（不从签发流程走）。
//
// 为什么不复用 UpsertClient：那是「签发」语义，会把 generate_count 记成 1
// 并刷新 last_generated_at —— 手工登记明明没有生成过任何包，那样记会让
// 「签发次数」这个字段失去含义，事后看审计也分不清哪些是发出去的、哪些是补录的。
// 因此这里 generate_count 固定为 0，时间字段只写 created_at / updated_at。
//
// 存在的必要性（来自一次真实的现场事故）：客户端已经部署好并在运行，
// 但对应的台账记录因为数据库异常丢失，此时**旧版本没有任何办法把它加回来** ——
// 唯一入口是「生成客户端包」，而管理员并不想再生成一个包、更不想重新部署。
// 结果就是这台设备在台账里永久缺席，既不显示名称、也不受端口冲突保护。
//
// generate_count = 0 同时也是给界面的信号：这条记录是补录的，
// 没有对应的签发历史，不能按「已签发」来推断现场状态。
func (s *Store) CreateClient(ctx context.Context, c ClientRegistry, ports []ClientPort) (*ClientRegistry, error) {
	if c.ClientID == "" {
		return nil, fmt.Errorf("clientID 不能为空")
	}
	if c.Status == "" {
		c.Status = ClientStatusActive
	}

	now := time.Now()
	at := formatTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开始台账事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先探一次是否存在：直接 INSERT 撞 UNIQUE 约束虽然也能拦住，
	// 但拿不到「是这个 clientID 重复」之外的上下文，错误文案会很难懂。
	var exists int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM client_registry WHERE client_id = ?`, c.ClientID).Scan(&exists)
	switch {
	case err == nil:
		return nil, ErrClientExists
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("查询台账记录失败: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO client_registry(client_id, display_name, robot_type, platform,
		                             server_addr, server_port,
		                             status, note, generate_count, first_generated_at,
		                             last_generated_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,0,NULL,NULL,?,?)`,
		c.ClientID, c.DisplayName, c.RobotType, c.Platform, c.ServerAddr, c.ServerPort,
		c.Status, c.Note, at, at)
	if err != nil {
		return nil, fmt.Errorf("新增台账记录失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取台账记录 ID 失败: %w", err)
	}

	for _, p := range ports {
		if p.ProxyName == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO client_port(client_id, proxy_name, proxy_type, remote_port, local_port, updated_at)
			 VALUES(?,?,?,?,?,?)`,
			c.ClientID, p.ProxyName, p.ProxyType, p.RemotePort, p.LocalPort, at); err != nil {
			return nil, fmt.Errorf("写入端口占用失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交台账事务失败: %w", err)
	}

	c.ID = id
	c.GenerateCount = 0
	c.CreatedAt = now
	c.UpdatedAt = now
	return &c, nil
}

// ListClients 按最近生成时间倒序返回台账记录。
func (s *Store) ListClients(ctx context.Context, limit int) ([]ClientRegistry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, display_name, robot_type, platform, server_addr, server_port, status, note,
		        generate_count, first_generated_at, last_generated_at, created_at, updated_at
		   FROM client_registry
		  ORDER BY (last_generated_at IS NULL), last_generated_at DESC, id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询客户端台账失败: %w", err)
	}
	defer rows.Close()

	out := make([]ClientRegistry, 0, 32)
	for rows.Next() {
		var (
			c                ClientRegistry
			first, last      sql.NullString
			created, updated string
		)
		if err := rows.Scan(&c.ID, &c.ClientID, &c.DisplayName, &c.RobotType, &c.Platform,
			&c.ServerAddr, &c.ServerPort,
			&c.Status, &c.Note, &c.GenerateCount, &first, &last, &created, &updated); err != nil {
			return nil, fmt.Errorf("解析台账记录失败: %w", err)
		}
		c.FirstGeneratedAt = parseNullTime(first)
		c.LastGeneratedAt = parseNullTime(last)
		c.CreatedAt = parseTime(created)
		c.UpdatedAt = parseTime(updated)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历台账记录失败: %w", err)
	}
	return out, nil
}

// ClientMeta 是「给 FRP 实时数据打标签」所需的最小台账信息。
//
// 刻意不做成完整的 RegistryClient：关联场景只要知道「这台叫什么」「是什么设备」，
// 时间戳、备注、端口、状态在这里都没有用途 —— 多取一列就多一处扫描目标，
// 也多一处日后改表时容易忘记同步的地方。
type ClientMeta struct {
	// DisplayName 是管理员填写的可读名称。空串表示登记过但没填名称
	// （这与「根本没登记」是两回事，两者在界面上的提示不同）。
	DisplayName string
	// RobotType 是机器人类型取值（值域见 internal/pack.AllRobotTypes），
	// 空串表示未分类。store 只做透传，不校验值域 —— 权威在 internal/pack。
	RobotType string
}

// ClientMetaIndex 返回 clientID → 台账元数据的全量映射。
//
// 与 ListClients 的差别有两点，都是刻意的：
//
//   - **不设条数上限**。这个方法服务于「给 FRP 实时数据打上名称与类型」这类
//     整表关联（客户端列表、代理列表、端口占用）。若沿用 ListClients 的 500 条
//     上限，第 501 个之后的客户端会被当成「未登记」——界面上显示成一个漂在
//     外面的陌生 clientID，而且不报任何错。台账总条数是「本平台签发过的客户端
//     数」，全量读取没有任何压力。
//   - 只取三列（clientID + 名称 + 类型）。关联用不到时间戳、备注与端口明细。
//
// 为什么名称与类型必须**同一次查询**取回：分成两个索引会让客户端列表的
// 「名称」与「机器人类型」可能来自台账的两个不同版本 —— 管理员在两次查询之间
// 改了类型，列表上就会出现「新类型配旧名称」的短暂错位。一次查询没有这个窗口。
//
// 返回的映射**包含名称为空、类型为空的记录**：调用方要能区分「登记过但没填」
// 与「根本没登记」，前者在设计上就允许存在。
func (s *Store) ClientMetaIndex(ctx context.Context) (map[string]ClientMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT client_id, display_name, robot_type FROM client_registry`)
	if err != nil {
		return nil, fmt.Errorf("查询客户端台账元数据失败: %w", err)
	}
	defer rows.Close()

	out := map[string]ClientMeta{}
	for rows.Next() {
		var id string
		var m ClientMeta
		if err := rows.Scan(&id, &m.DisplayName, &m.RobotType); err != nil {
			return nil, fmt.Errorf("解析客户端台账元数据失败: %w", err)
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历客户端台账元数据失败: %w", err)
	}
	return out, nil
}

// ClientPortMeta 是「给 FRP 实时代理补上本地端口」所需的最小台账信息。
//
// 只取本地端口：FRP Dashboard 的代理 conf 返回 localIP 与 remotePort，
// 唯独**不返回 localPort**（03-frp-api-contract.md §4-⑦ 实测），而本地端口
// 恰恰是现场最想知道的那一个 —— 代理有没有填错，只能靠它跟目标机器上的服务对上。
type ClientPortMeta struct {
	// LocalPort 是台账里登记的本地端口。pack 的校验要求它在 1-65535 之间，
	// 所以合法数据不会是 0；但判断「有没有登记」仍请用索引是否命中，
	// 不要写成 LocalPort > 0 —— 那会把「登记了 0」这种脏数据悄悄当成未登记。
	LocalPort int
}

// ClientPortIndex 返回 clientID → (代理短名 → 台账端口信息) 的两级映射。
//
// 键用**代理短名**：client_port.proxy_name 存的是表单里那个短名，而 FRP 返回的
// Name 是 "user.proxyname" 形式，关联前必须先用 frpapi.SplitQualifiedName 拆开
// （调用方统一走 web 侧的 shortProxyName）。
//
// 与 ListClientPorts 的分工：那个返回有序切片，供界面直接展示端口总览；
// 这个返回映射，供「给实时数据打标签」的整表关联用。一次调用只发一条 SQL，
// 不要在循环里反复调。
func (s *Store) ClientPortIndex(ctx context.Context) (map[string]map[string]ClientPortMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT client_id, proxy_name, local_port FROM client_port`)
	if err != nil {
		return nil, fmt.Errorf("查询端口占用索引失败: %w", err)
	}
	defer rows.Close()

	out := map[string]map[string]ClientPortMeta{}
	for rows.Next() {
		var id, name string
		var m ClientPortMeta
		if err := rows.Scan(&id, &name, &m.LocalPort); err != nil {
			return nil, fmt.Errorf("解析端口占用索引失败: %w", err)
		}
		if out[id] == nil {
			out[id] = map[string]ClientPortMeta{}
		}
		out[id][name] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历端口占用索引失败: %w", err)
	}
	return out, nil
}

// ListClientPorts 返回台账里的全部端口占用明细。
//
// 一次性取全量而不是按客户端逐个查：量级是「几十个客户端 × 几条代理」，
// 逐条往返的开销远大于把它整个读进内存。调用方（界面）本来也要展示端口总览。
func (s *Store) ListClientPorts(ctx context.Context) ([]ClientPort, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, proxy_name, proxy_type, remote_port, local_port, updated_at
		   FROM client_port ORDER BY remote_port, client_id, proxy_name`)
	if err != nil {
		return nil, fmt.Errorf("查询端口占用失败: %w", err)
	}
	defer rows.Close()

	out := make([]ClientPort, 0, 64)
	for rows.Next() {
		var (
			p  ClientPort
			at string
		)
		if err := rows.Scan(&p.ID, &p.ClientID, &p.ProxyName, &p.ProxyType,
			&p.RemotePort, &p.LocalPort, &at); err != nil {
			return nil, fmt.Errorf("解析端口占用失败: %w", err)
		}
		p.UpdatedAt = parseTime(at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历端口占用失败: %w", err)
	}
	return out, nil
}

// ListClientPortsByClient 返回某个客户端的端口占用明细，按代理名排序。
//
// 单独开了这个方法而不是复用全量 ListClientPorts：改包时要的是「这一台设备的
// 代理列表」，用全量版本得在调用方过滤一遍 —— 那份过滤逻辑散在各调用点，
// 迟早有某处漏掉 client_id 判断而把别人的代理塞进表单（症状是改包时凭空多出几行）。
// client_port 已有 idx_client_port_client 索引，单查代价可以忽略。
func (s *Store) ListClientPortsByClient(ctx context.Context, clientID string) ([]ClientPort, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, proxy_name, proxy_type, remote_port, local_port, updated_at
		   FROM client_port WHERE client_id = ?
		  ORDER BY proxy_name`, clientID)
	if err != nil {
		return nil, fmt.Errorf("查询客户端端口明细失败: %w", err)
	}
	defer rows.Close()

	out := make([]ClientPort, 0, 8)
	for rows.Next() {
		var (
			p  ClientPort
			at string
		)
		if err := rows.Scan(&p.ID, &p.ClientID, &p.ProxyName, &p.ProxyType,
			&p.RemotePort, &p.LocalPort, &at); err != nil {
			return nil, fmt.Errorf("解析端口占用失败: %w", err)
		}
		p.UpdatedAt = parseTime(at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历端口占用失败: %w", err)
	}
	return out, nil
}

// ActivePortOwners 返回台账中所有启用客户端占用的远端端口。
//
// 只认 status='active' 且 remote_port > 0 的行：
//   - retired 表示设备已下线，端口视为已释放；
//   - remote_port 为 0 的是 HTTP/HTTPS/STCP 等不占全局端口的代理。
//
// 结果按端口升序，冲突提示可以稳定按端口大小罗列。
func (s *Store) ActivePortOwners(ctx context.Context) ([]PortOwner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.remote_port, p.client_id, r.display_name, p.proxy_name, p.proxy_type
		   FROM client_port p
		   JOIN client_registry r ON r.client_id = p.client_id
		  WHERE r.status = ? AND p.remote_port > 0
		  ORDER BY p.remote_port`, ClientStatusActive)
	if err != nil {
		return nil, fmt.Errorf("查询端口占用者失败: %w", err)
	}
	defer rows.Close()

	out := make([]PortOwner, 0, 32)
	for rows.Next() {
		o := PortOwner{Status: ClientStatusActive, Source: PortSourceRegistry}
		if err := rows.Scan(&o.ID, &o.Port, &o.ClientID, &o.ClientName, &o.ProxyName, &o.ProxyType); err != nil {
			return nil, fmt.Errorf("解析端口占用者失败: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历端口占用者失败: %w", err)
	}
	return out, nil
}

// ClientByID 按主键查询台账记录。
func (s *Store) ClientByID(ctx context.Context, id int64) (*ClientRegistry, error) {
	var (
		c                ClientRegistry
		first, last      sql.NullString
		created, updated string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, display_name, robot_type, platform, server_addr, server_port, status, note,
		        generate_count, first_generated_at, last_generated_at, created_at, updated_at
		   FROM client_registry WHERE id = ?`, id).
		Scan(&c.ID, &c.ClientID, &c.DisplayName, &c.RobotType, &c.Platform, &c.ServerAddr, &c.ServerPort,
			&c.Status, &c.Note, &c.GenerateCount, &first, &last, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询台账记录失败: %w", err)
	}
	c.FirstGeneratedAt = parseNullTime(first)
	c.LastGeneratedAt = parseNullTime(last)
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return &c, nil
}

// UpdateClient 修改台账记录的显示名、机器人类型、目标平台、备注与状态。
//
// clientID 不允许改：它是目标机器上 frpc.toml 里的身份标识，
// 在台账里改掉只会让记录与真实机器对不上，需要换标识时应新建一个客户端。
//
// robotType 传空串表示清空为「未分类」—— 管理员确实可能发现选错了类型而想改回去，
// 若把空串当成「不修改」，界面上就会出现一个怎么也清不掉的下拉框。
//
// platform 同理传空串清空为「未记录」。但注意它与 robotType 的差别：
// 「未记录」不是一种合法的目标平台，而是历史数据状态（v4 之前的记录），
// 界面不该提供这个选项 —— 它只用于把旧的改包请求如实退回初始状态。
func (s *Store) UpdateClient(ctx context.Context, id int64, displayName, robotType, platform, note, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE client_registry
		    SET display_name = ?, robot_type = ?, platform = ?, note = ?, status = ?, updated_at = ?
		  WHERE id = ?`, displayName, robotType, platform, note, status, nowText(), id)
	if err != nil {
		return fmt.Errorf("更新台账记录失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteClient 删除台账记录，其端口占用随之级联删除（释放端口）。
func (s *Store) DeleteClient(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_registry WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除台账记录失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteClientPort 释放单条端口占用。
//
// 用于「客户端上的某条代理已经被手工删掉」这类情况：
// 只删端口、保留客户端记录，下次该客户端重新生成时端口会按新配置重新登记。
func (s *Store) DeleteClientPort(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_port WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("释放端口占用失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
