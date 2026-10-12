// Package config 负责 SinyeFRP 管理平台的运行配置加载与校验。
//
// 设计要点：开发环境与生产环境共用同一份代码，差异只体现在配置上。
// 开发期默认指向本机 FRP Dashboard（http://127.0.0.1:7500），
// 生产期只需替换配置文件中的节点地址，代码不做任何改动。
//
// 配置来源优先级（由低到高）：
//  1. 内置默认值
//  2. TOML 配置文件
//  3. 环境变量（SINYEFRP_ 前缀）
//  4. 命令行参数
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// 内置默认值。
const (
	// DefaultConfigFile 是未显式指定时读取的配置文件名。
	DefaultConfigFile = "config.toml"

	// DefaultListenAddr 默认仅监听环回地址。生产环境请勿改为 0.0.0.0。
	DefaultListenAddr = "127.0.0.1"

	// DefaultPort 刻意避开 frps 的 7000 与 Dashboard 的 7500。
	DefaultPort = 7800

	DefaultDBPath = "data/sinyefrp.db"

	DefaultLogDir   = "logs"
	DefaultLogLevel = "info"

	DefaultNodeName    = "local"
	DefaultNodeAPIURL  = "http://127.0.0.1:7500"
	DefaultNodeTimeout = 5 * time.Second

	// DefaultPackFallbackDir 是开发期查找 FRP 二进制的默认目录。
	DefaultPackFallbackDir = "frp"
)

// EnvPrefix 是环境变量前缀，例如 SINYEFRP_SERVER_PORT=7801。
const EnvPrefix = "SINYEFRP_"

// 与 FRP 端口冲突的保留端口，避免管理平台抢占。
var reservedPorts = map[int]string{
	7000: "frps bindPort",
	7500: "FRP Dashboard",
}

// Server 是管理平台自身的 HTTP 监听配置。
type Server struct {
	ListenAddr string `toml:"listen_addr"`
	Port       int    `toml:"port"`
}

// Address 返回可直接传给 net.Listen 的监听地址。
func (s Server) Address() string {
	return fmt.Sprintf("%s:%d", s.ListenAddr, s.Port)
}

// IsLoopback 判断监听地址是否仅限本机。
func (s Server) IsLoopback() bool {
	switch strings.ToLower(strings.TrimSpace(s.ListenAddr)) {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// Database 是本地持久化配置。
type Database struct {
	Path string `toml:"path"`
}

// Log 是日志配置。
type Log struct {
	Dir   string `toml:"dir"`
	Level string `toml:"level"`
}

// Security 是认证相关的安全参数（01-prd F02）。
type Security struct {
	// SessionHours 是会话有效期；每次请求会滑动续期。
	SessionHours int `toml:"session_hours"`
	// MaxLoginFailures 是触发锁定前的连续失败次数。
	MaxLoginFailures int `toml:"max_login_failures"`
	// FailWindowMinutes 是失败计数的统计窗口。
	FailWindowMinutes int `toml:"fail_window_minutes"`
	// LockMinutes 是触发上限后的锁定时长。
	LockMinutes int `toml:"lock_minutes"`
}

// SessionTTL 返回会话有效期。
func (s Security) SessionTTL() time.Duration {
	if s.SessionHours <= 0 {
		return 12 * time.Hour
	}
	return time.Duration(s.SessionHours) * time.Hour
}

// FailWindow 返回失败计数窗口。
func (s Security) FailWindow() time.Duration {
	if s.FailWindowMinutes <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(s.FailWindowMinutes) * time.Minute
}

// LockDuration 返回锁定时长。
func (s Security) LockDuration() time.Duration {
	if s.LockMinutes <= 0 {
		return 15 * time.Minute
	}
	return time.Duration(s.LockMinutes) * time.Minute
}

// Node 是被管理的 frps 节点。
//
// 首版只允许一个节点（见 01-prd §8.5），因此这里是单值而非数组。
// 数据库表结构已预留多节点，升级到 P1 时无需迁移数据。
type Node struct {
	Name     string `toml:"name"`
	APIURL   string `toml:"api_url"`
	Username string `toml:"username"`
	Password string `toml:"password"`
	Timeout  int    `toml:"timeout_seconds"`
}

// TimeoutDuration 返回节点请求超时。
func (n Node) TimeoutDuration() time.Duration {
	if n.Timeout <= 0 {
		return DefaultNodeTimeout
	}
	return time.Duration(n.Timeout) * time.Second
}

// Pack 是部署包生成的相关配置（01-prd F09-F15）。
type Pack struct {
	// FallbackDir 是内嵌二进制缺失时查找 frpc.exe / frps.exe 的目录。
	//
	// 只服务于开发期联调：正式交付件把二进制编译进 exe 本体（满足 F18
	// 「解压即用、不装运行时」），运行时根本不会读这个目录。
	// 开发时把它指向仓库里的 frp/ 目录，就不必每次构建都复制 36 MB 二进制。
	FallbackDir string `toml:"fallback_dir"`
}

// Config 是管理平台的完整配置。
type Config struct {
	Server   Server   `toml:"server"`
	Database Database `toml:"database"`
	Log      Log      `toml:"log"`
	Security Security `toml:"security"`
	Node     Node     `toml:"node"`
	Pack     Pack     `toml:"pack"`

	// Path 记录配置文件来源，便于错误提示与后台展示。
	Path string `toml:"-"`
}

// Default 返回带全部默认值的配置。
func Default() *Config {
	return &Config{
		Server: Server{
			ListenAddr: DefaultListenAddr,
			Port:       DefaultPort,
		},
		Database: Database{Path: DefaultDBPath},
		Log:      Log{Dir: DefaultLogDir, Level: DefaultLogLevel},
		Security: Security{
			SessionHours:      12,
			MaxLoginFailures:  5,
			FailWindowMinutes: 10,
			LockMinutes:       15,
		},
		Node: Node{
			Name:    DefaultNodeName,
			APIURL:  DefaultNodeAPIURL,
			Timeout: int(DefaultNodeTimeout / time.Second),
		},
		Pack: Pack{FallbackDir: DefaultPackFallbackDir},
	}
}

// Load 按优先级合并各配置来源。
//
// path 为空时使用默认的 config.toml；该文件不存在不算错误，
// 此时程序以内置默认值启动（首次初始化流程会引导补全）。
func Load(path string) (*Config, error) {
	cfg := Default()

	explicit := path != ""
	if !explicit {
		path = DefaultConfigFile
	}

	switch _, err := os.Stat(path); {
	case err == nil:
		md, decErr := toml.DecodeFile(path, cfg)
		if decErr != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, decErr)
		}
		// 未知字段直接报错，避免配置写错却静默生效。
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, 0, len(undecoded))
			for _, k := range undecoded {
				keys = append(keys, k.String())
			}
			return nil, fmt.Errorf("配置文件 %s 含未知字段: %s", path, strings.Join(keys, ", "))
		}
		cfg.Path = path
	case errors.Is(err, os.ErrNotExist):
		if explicit {
			return nil, fmt.Errorf("指定的配置文件不存在: %s", path)
		}
	default:
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.resolvePaths(); err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// resolvePaths 把配置里的相对路径统一按「配置文件所在目录」解析成绝对路径。
//
// 为什么不能按进程工作目录解析：工作目录取决于谁来拉起进程——
// 手工执行时是 shell 的当前目录，而由 Windows 计划任务（SYSTEM 身份、开机自启）
// 拉起时是 C:\Windows\System32。同一份配置在两种启动方式下会指向不同的数据库
// 与日志目录，表现为「重启之后配置丢了」「数据找不到」，甚至两个实例各写一个库。
// 以配置文件为基准之后，整个部署目录搬走也不会错位。
//
// 环境变量覆盖进来的路径同样按此规则解析：只保留一个基准，
// 避免出现「有的按工作目录、有的按配置文件」这种无法预测的组合。
func (c *Config) resolvePaths() error {
	base, err := c.baseDir()
	if err != nil {
		return err
	}

	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}

	c.Database.Path = abs(c.Database.Path)
	c.Log.Dir = abs(c.Log.Dir)
	c.Pack.FallbackDir = abs(c.Pack.FallbackDir)
	return nil
}

// baseDir 返回路径解析的基准目录，即配置文件所在目录。
//
// 未指定配置文件时回退到进程工作目录，保持「没有配置文件也能启动」的语义。
func (c *Config) baseDir() (string, error) {
	if c.Path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("获取当前工作目录失败: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(c.Path)
	if err != nil {
		return "", fmt.Errorf("解析配置文件路径失败: %w", err)
	}
	c.Path = abs
	return filepath.Dir(abs), nil
}

// applyEnv 用 SINYEFRP_ 前缀的环境变量覆盖配置。
//
// 注意 SINYEFRP_NODE_* 是调试旁路：它在 internal/node 中还会再覆盖一次
// 数据库里保存的节点配置，使开发期可以把管理平台指向探测实例而不改动
// 生产配置。因此生产环境不应设置这几个变量，详见 02-技术选型.md。
func (c *Config) applyEnv() error {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
			*dst = v
		}
	}

	str("SERVER_LISTEN_ADDR", &c.Server.ListenAddr)
	str("DATABASE_PATH", &c.Database.Path)
	str("LOG_DIR", &c.Log.Dir)
	str("LOG_LEVEL", &c.Log.Level)
	str("NODE_NAME", &c.Node.Name)
	str("NODE_API_URL", &c.Node.APIURL)
	str("NODE_USERNAME", &c.Node.Username)
	str("NODE_PASSWORD", &c.Node.Password)

	if v, ok := os.LookupEnv(EnvPrefix + "SERVER_PORT"); ok && v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("环境变量 %sSERVER_PORT 不是合法端口: %q", EnvPrefix, v)
		}
		c.Server.Port = p
	}

	if v, ok := os.LookupEnv(EnvPrefix + "NODE_TIMEOUT_SECONDS"); ok && v != "" {
		t, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("环境变量 %sNODE_TIMEOUT_SECONDS 不是合法秒数: %q", EnvPrefix, v)
		}
		c.Node.Timeout = t
	}

	return nil
}

// Validate 校验配置的合法性。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.ListenAddr) == "" {
		return errors.New("server.listen_addr 不能为空")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port 超出范围 1-65535: %d", c.Server.Port)
	}
	if what, clash := reservedPorts[c.Server.Port]; clash {
		return fmt.Errorf("server.port %d 与 %s 冲突，请改用其他端口", c.Server.Port, what)
	}

	u, err := url.Parse(c.Node.APIURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("node.api_url 不是合法的 URL: %q", c.Node.APIURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("node.api_url 仅支持 http/https: %q", c.Node.APIURL)
	}

	if strings.TrimSpace(c.Database.Path) == "" {
		return errors.New("database.path 不能为空")
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level 仅支持 debug/info/warn/error: %q", c.Log.Level)
	}

	return nil
}

// EnsureDirs 创建配置涉及的目录。
func (c *Config) EnsureDirs() error {
	dirs := []string{filepath.Dir(c.Database.Path)}
	if c.Log.Dir != "" {
		dirs = append(dirs, c.Log.Dir)
	}
	for _, dir := range dirs {
		if dir == "" || dir == "." {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	return nil
}

// Redacted 返回可安全记录到日志或返回给前端的副本，凭据以占位符替代。
//
// 对应 01-prd F17：日志、错误响应和审计不得包含完整密钥。
func (c *Config) Redacted() *Config {
	cp := *c
	if cp.Node.Password != "" {
		cp.Node.Password = "******"
	}
	return &cp
}
