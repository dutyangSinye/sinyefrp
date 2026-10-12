// Package store 提供管理平台的本地持久化。
//
// 存储选型：modernc.org/sqlite（纯 Go 实现，无 CGO）+ WAL 模式。
// 目的是让单个 exe 在任何 Windows 主机上解压即用，不依赖外置运行库。
//
// 本包只负责「读写」，不做业务判断：
//   - 凭据字段（node.password_enc）存密文，加解密由 internal/secret 负责；
//   - 管理员口令只存 bcrypt 哈希，由 internal/auth 负责生成与比对。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 注册 database/sql 驱动 "sqlite"
)

// driverName 是 modernc.org/sqlite 注册的驱动名。
const driverName = "sqlite"

// timeLayout 统一用 RFC3339Nano(UTC) 存储时间：可读、可直接比较、无时区歧义。
const timeLayout = time.RFC3339Nano

// Store 封装数据库句柄。
type Store struct {
	db  *sql.DB
	log *slog.Logger

	// FreshlyCreated 为 true 表示这次 Open 是**从零新建**了一个数据库文件：
	// 调用前该路径不存在，迁移表是空的。这本身是正常情况（全新部署），
	// 但也是「data\ 被误删后平台静默重建空库」的同一形态 —— 上层据此
	// 决定要不要提示，而不是让运维自己去猜记录为什么没了。
	FreshlyCreated bool
}

// Open 打开（必要时创建）数据库并完成迁移。
func Open(path string, log *slog.Logger) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("数据库路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
		}
	}

	// 记录打开前是否已有库文件：迁移表为空 + 文件原先不存在 ⇒ 全新库。
	// 两者要同时成立：文件在但迁移表空，只可能是文件被截断/损坏，
	// 那种情况应该走 migrate 正常建表并在日志里留痕，不算「全新部署」。
	_, statErr := os.Stat(path)
	fileExisted := statErr == nil

	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 的写入是串行的。本项目写入量极小（审计、采样、配置），
	// 限制为单连接可彻底消除 "database is locked"，代价可忽略。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db, log: log}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if !fileExisted && s.appliedMigrationCount(ctx) > 0 {
		s.FreshlyCreated = true
	}

	// 启动即把上一次运行遗留的 WAL 折叠回主库。
	//
	// 为什么必须在启动时做：WAL 模式下，已提交的数据先落在 -wal 文件里，
	// 只有 checkpoint 才回写主库。若上一次进程是被 Stop-Process -Force
	// 或直接断电终止的，defer st.Close() 根本没执行，WAL 会原样留到下次启动。
	// 这份 WAL 就是「唯一副本」—— 一旦它被误删、或再次异常掉电导致
	// 文件系统层面的写入丢失，那些记录就永久消失，而登录账号仍在
	// （账号在更早的 checkpoint 里），表现为「能登录，但记录没了」。
	// 启动时先收敛一次，把这个脆弱窗口缩短到「本次运行期间」。
	s.Checkpoint()
	return s, nil
}

// Checkpoint 把 WAL 内容折叠回主库，并截断 WAL 文件。
//
// 用 TRUNCATE 而不是 PASSIVE：PASSIVE 只在没有其它读者时才尽量回写、
// 且不保证截断，WAL 仍会保持原有大小；TRUNCATE 在回写后把 WAL 清零，
// 磁盘占用与「未落盘窗口」都降到最小。
//
// 本方法的定位是「降低风险」而非「保证不丢」：真正保证数据安全的
// 仍然是让进程收到正常关闭信号（见 pack/scripts.go 的 Stop-InstanceGracefully）。
// 一个成年的 WAL 突然消失仍然会丢数据，所以本方法只负责别让它长大。
//
// 失败不返回错误：checkpoint 是优化动作，失败（例如恰好有长事务在读）
// 不影响业务可用性，只记日志。
func (s *Store) Checkpoint() {
	if s == nil || s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var busy, logPages, checkpointed int
	row := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err := row.Scan(&busy, &logPages, &checkpointed); err != nil {
		if s.log != nil {
			s.log.Warn("WAL checkpoint 失败", "err", err)
		}
		return
	}
	// busy=1 表示有读者占用，回写被跳过；这不影响正确性，只说明 WAL 没瘦下来。
	if busy != 0 && s.log != nil {
		s.log.Warn("WAL checkpoint 被占用跳过，稍后重试", "log", logPages, "checkpointed", checkpointed)
	}
}

// StartCheckpointLoop 启动后台定期 checkpoint，直到 ctx 结束。
//
// 兜住「进程长期运行且从未正常关闭」的场景：即使运维一直把机器硬重启、
// 脚本的优雅关闭被绕过，WAL 也不会无限增长。
// interval <= 0 时使用默认 5 分钟。
func (s *Store) StartCheckpointLoop(ctx context.Context, interval time.Duration) {
	if s == nil || s.db == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Checkpoint()
			}
		}
	}()
}

// dsn 拼装 modernc.org/sqlite 的连接串。
//
// 路径经 url 转义，避免含空格或中文的部署目录导致连接失败。
func dsn(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.ToSlash(abs)

	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + abs + "?" + q.Encode()
}

// Close 关闭数据库。
//
// 关闭前先 checkpoint：这是**最后一次**把 WAL 收敛回主库的机会，
// 也是「正常关闭」相对「被强杀」唯一的实质差别。不在这里做的话，
// 本进程写入的全部数据都会留在 -wal 里随文件一起裸奔到下次启动。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.Checkpoint()
	return s.db.Close()
}

// DB 暴露底层句柄，仅供同一包内的仓储实现使用。
func (s *Store) DB() *sql.DB { return s.db }

// appliedMigrationCount 返回已记录的迁移条数。
//
// 只在 Open 里用来判断「库是不是刚从零建起来的」：迁移表存在且有条目，
// 说明迁移确实跑过 —— 配合「打开前文件不存在」，就能确定这是全新库，
// 而不是打开了一个被截断的旧文件。
func (s *Store) appliedMigrationCount(ctx context.Context) int {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ---------- 迁移 ----------

// migration 是一次结构变更。已应用的版本不会重复执行。
type migration struct {
	version int
	name    string
	stmts   []string
}

// migrations 是全部结构变更，按 version 升序执行。
//
// 约定：已发布的 migration 不再修改，只追加新版本。
var migrations = []migration{
	{
		version: 1,
		name:    "初始结构：管理员、会话、节点、设置、审计、包记录",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS app_user (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				username      TEXT    NOT NULL UNIQUE,
				password_hash TEXT    NOT NULL,
				created_at    TEXT    NOT NULL,
				updated_at    TEXT    NOT NULL,
				last_login_at TEXT
			)`,

			`CREATE TABLE IF NOT EXISTS session (
				token_hash   TEXT    PRIMARY KEY,
				user_id      INTEGER NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
				created_at   TEXT    NOT NULL,
				expires_at   TEXT    NOT NULL,
				last_seen_at TEXT    NOT NULL,
				client_ip    TEXT    NOT NULL DEFAULT '',
				user_agent   TEXT    NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_session_expires ON session(expires_at)`,

			// password_enc 存密文（内部 secret 包的 enc:v1: 前缀格式），永不存明文。
			`CREATE TABLE IF NOT EXISTS node (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				name            TEXT    NOT NULL,
				api_url         TEXT    NOT NULL,
				username        TEXT    NOT NULL DEFAULT '',
				password_enc    TEXT    NOT NULL DEFAULT '',
				timeout_seconds INTEGER NOT NULL DEFAULT 5,
				enabled         INTEGER NOT NULL DEFAULT 1,
				is_primary      INTEGER NOT NULL DEFAULT 1,
				created_at      TEXT    NOT NULL,
				updated_at      TEXT    NOT NULL
			)`,

			`CREATE TABLE IF NOT EXISTS setting (
				key        TEXT PRIMARY KEY,
				value      TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,

			// detail 只放非敏感摘要，禁止写入明文密码或 token（01-prd F16/F17）。
			`CREATE TABLE IF NOT EXISTS audit (
				id        INTEGER PRIMARY KEY AUTOINCREMENT,
				at        TEXT NOT NULL,
				actor     TEXT NOT NULL DEFAULT '',
				action    TEXT NOT NULL,
				target    TEXT NOT NULL DEFAULT '',
				result    TEXT NOT NULL DEFAULT '',
				client_ip TEXT NOT NULL DEFAULT '',
				detail    TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at DESC)`,

			`CREATE TABLE IF NOT EXISTS pkg_record (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				at          TEXT    NOT NULL,
				actor       TEXT    NOT NULL DEFAULT '',
				kind        TEXT    NOT NULL,
				client_id   TEXT    NOT NULL DEFAULT '',
				display_name TEXT   NOT NULL DEFAULT '',
				frp_version TEXT    NOT NULL DEFAULT '',
				exe_sha256  TEXT    NOT NULL DEFAULT '',
				summary     TEXT    NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_pkg_at ON pkg_record(at DESC)`,
		},
	},
	{
		version: 2,
		name:    "客户端台账与端口占用（F19）",
		stmts: []string{
			// 一个 clientID 一行。client_id 建 UNIQUE 而不是主键：
			// 主键用自增 id，界面的编辑/删除都按 id 走，
			// client_id 只是业务键（它本身允许被管理员手工改过，但改的是新客户端的新标识）。
			`CREATE TABLE IF NOT EXISTS client_registry (
				id                 INTEGER PRIMARY KEY AUTOINCREMENT,
				client_id          TEXT    NOT NULL UNIQUE,
				display_name       TEXT    NOT NULL DEFAULT '',
				server_addr        TEXT    NOT NULL DEFAULT '',
				server_port        INTEGER NOT NULL DEFAULT 0,
				status             TEXT    NOT NULL DEFAULT 'active',
				note               TEXT    NOT NULL DEFAULT '',
				generate_count     INTEGER NOT NULL DEFAULT 0,
				first_generated_at TEXT,
				last_generated_at  TEXT,
				created_at         TEXT    NOT NULL,
				updated_at         TEXT    NOT NULL
			)`,
			// 冲突判定每次都按 status 过滤，给它建索引。
			`CREATE INDEX IF NOT EXISTS idx_client_registry_status ON client_registry(status)`,

			// 端口占用明细。client_id 外键指向台账并级联删除：
			// 台账记录一删，它占的端口必须同时消失，否则会留下一堆
			// 指向不存在客户端的端口占用，把端口永久锁死。
			// （库连接已开 foreign_keys(1)，见 dsn()。）
			`CREATE TABLE IF NOT EXISTS client_port (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				client_id   TEXT    NOT NULL REFERENCES client_registry(client_id) ON DELETE CASCADE,
				proxy_name  TEXT    NOT NULL,
				proxy_type  TEXT    NOT NULL DEFAULT '',
				remote_port INTEGER NOT NULL DEFAULT 0,
				local_port  INTEGER NOT NULL DEFAULT 0,
				updated_at  TEXT    NOT NULL,
				UNIQUE(client_id, proxy_name)
			)`,
			// 端口是全局资源，按 remote_port 建索引供「这个端口归谁」查询；
			// 按 client_id 建索引供台账列表一次性取回某客户端全部端口。
			`CREATE INDEX IF NOT EXISTS idx_client_port_remote ON client_port(remote_port)`,
			`CREATE INDEX IF NOT EXISTS idx_client_port_client ON client_port(client_id)`,
		},
	},
	{
		version: 3,
		name:    "台账记录机器人类型（F19 扩展）",
		stmts: []string{
			// 用 ALTER 追加而不是重建表：v2 已发布，且在别人机器上可能已经有数据，
			// 重建表要搬全量数据、还要处理外键，风险远大于收益。
			//
			// 默认 '' 表示「未分类」。既有记录（v3 之前签发的包）全部落到这个值上 ——
			// 这是正确的：那批包确实没有机器人类型信息，不能拿一个猜测值去填。
			`ALTER TABLE client_registry ADD COLUMN robot_type TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		version: 4,
		name:    "台账记录目标平台（改包功能的地基）",
		stmts: []string{
			// 为什么必须记住平台：
			// 部署包的内容由目标平台决定 —— Windows x64 / Linux x64 / Linux ARM64
			// 三者的二进制、生命周期脚本、文本行尾全都不一样。「重新生成包」时若
			// 拿不到这个值，只能给默认值 Windows-amd64，而这意味着给一台 Linux
			// 盒子生成一个装不上的包：安装脚本不会报错（它会安静地执行 .ps1），
			// 症状要等上线才发现 —— 这正是本功能最容易静默失效的一环。
			//
			// 空串的语义 = 「未记录」，对应 v4 之前签发的包。它们几乎全是 Windows
			// （平台选择是 2026-09-26 才有的），所以界面把空串按 Windows 展示，
			// 但**首次改包时必须让管理员确认**，不能替他默认。
			`ALTER TABLE client_registry ADD COLUMN platform TEXT NOT NULL DEFAULT ''`,
		},
	},
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migration (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("创建迁移表失败: %w", err)
	}

	applied := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migration`)
	if err != nil {
		return fmt.Errorf("读取迁移记录失败: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmt.Errorf("解析迁移记录失败: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("遍历迁移记录失败: %w", err)
	}
	_ = rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
		s.log.Info("数据库迁移完成", "version", m.version, "name", m.name)
	}
	return nil
}

// applyMigration 在单个事务内执行一次迁移，失败整体回滚。
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行迁移 v%d 失败: %w", m.version, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migration(version, name, applied_at) VALUES(?,?,?)`,
		m.version, m.name, nowText()); err != nil {
		return fmt.Errorf("记录迁移 v%d 失败: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移 v%d 失败: %w", m.version, err)
	}
	return nil
}

// ---------- 时间辅助 ----------

func nowText() string { return formatTime(time.Now()) }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

// parseTime 解析存储时间；空串或非法值返回零值。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseNullTime 解析可空时间列。
func parseNullTime(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return parseTime(ns.String)
}
