// Command sinyefrp 是 SinyeFRP 管理平台的服务端入口。
//
// 它只读调用 FRP Dashboard API 并对外提供管理界面所需的数据，
// 不参与 FRP 的数据转发，因此本程序停止不会影响既有隧道（01-prd §9）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sinyefrp/internal/auth"
	"sinyefrp/internal/buildinfo"
	"sinyefrp/internal/collector"
	"sinyefrp/internal/config"
	"sinyefrp/internal/node"
	"sinyefrp/internal/pack"
	"sinyefrp/internal/store"
	"sinyefrp/internal/web"
)

// 产品标识统一取自 internal/buildinfo：版本号还出现在部署包命名与包内 README 里，
// 在这里另写一份会有漂移风险。
const (
	appName = buildinfo.Name
	version = buildinfo.Version

	// stopFileName 是「请求本实例优雅退出」的信号文件名，位于数据目录。
	//
	// 生命周期脚本（stop.ps1 / restart.ps1 / uninstall.ps1）在需要停实例时
	// 创建该文件，进程轮询到后走与 Ctrl+C 完全相同的关闭路径，
	// 从而保证 SQLite 完成 WAL checkpoint。
	//
	// 名字与 .db 文件并列，运维一眼能看出它属于哪个实例。
	stopFileName = "sinyefrp.stop"

	// stopPollInterval 是轮询间隔。取 300ms 是折中：
	// 再密没有意义（stop.ps1 本身还会等进程退出），再疏会让停机变慢。
	stopPollInterval = 300 * time.Millisecond
)

// watchStopFile 轮询关停信号文件，出现即触发 stop()。
//
// 触发后会**删除该文件**：否则下次启动时文件还在，进程会在启动瞬间
// 立刻退出，表现为「怎么启动都起不来」这种极难排查的症状。
// 删除时机放在读取之后、触发之前，保证「触发即消费」。
func watchStopFile(ctx context.Context, path string, stop func(), log *slog.Logger) {
	go func() {
		t := time.NewTicker(stopPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := os.Stat(path); err != nil {
					continue
				}
				// 先删再触发：文件是「一次性信号」，不是状态标记。
				if rmErr := os.Remove(path); rmErr != nil {
					// 删不掉（被杀软锁住等）就放弃本次触发，避免退出后
					// 残留文件把下一次启动也拖下水。
					if log != nil {
						log.Warn("检测到关停信号文件但无法删除，已忽略本次请求",
							"path", path, "err", rmErr)
					}
					continue
				}
				if log != nil {
					log.Info("收到关停信号文件，正在优雅关闭", "path", path)
				}
				stop()
				return
			}
		}
	}()
}

func main() {
	// 子命令在进入 flag 解析**之前**分流。
	//
	// 不能放进 run() 之后再判断：「pkg」是位置参数，flag.Parse() 遇到它会立刻
	// 停止解析，后面的 -o / -config 就永远读不到 —— 表现为「参数被静默忽略」，
	// 既不报错也不生效，是最难排查的一类失败。
	if len(os.Args) > 1 && os.Args[1] == "pkg" {
		if err := runPkg(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%s pkg: %v\n", appName, err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s 启动失败: %v\n", appName, err)
		os.Exit(1)
	}
}

// runPkg 是 pkg 子命令的入口。
func runPkg(args []string) error {
	if len(args) == 0 {
		printPkgUsage(os.Stderr)
		return errors.New("缺少子命令")
	}
	switch args[0] {
	case "app":
		return runPkgApp(args[1:])
	case "help", "-h", "--help":
		printPkgUsage(os.Stdout)
		return nil
	default:
		printPkgUsage(os.Stderr)
		return fmt.Errorf("未知子命令 %q", args[0])
	}
}

// printPkgUsage 打印 pkg 子命令用法。
func printPkgUsage(w io.Writer) {
	fmt.Fprint(w, `用法: sinyefrp pkg <子命令> [参数]

子命令:
  app    生成管理平台自身部署包（Windows ZIP，含 install/uninstall/start/stop/restart/status）

示例:
  sinyefrp pkg app -config config.toml -o SinyeFRP-platform.zip
`)
}

// runPkgApp 生成管理平台自身部署包（01-prd F18）。
//
// 为什么必须是运行期生成而不是构建期：包内要放一份「平台自己」，而这份副本只
// 存在于当前正在运行的这个 exe 里。发布时在交付机跑一次本命令，产出的 ZIP
// 才是真正带着这一版二进制的可交付件。
//
// 配置里的监听地址、节点地址会被写进包内 config.toml，因此同一份 exe
// 可以为不同监听端口的部署各生成一份包，而不必回头改模板。
func runPkgApp(args []string) error {
	fs := flag.NewFlagSet("pkg app", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "", "配置文件路径（默认 ./config.toml），其监听地址等设置会写入包内 config.toml")
		out        = fs.String("o", "", "输出 ZIP 路径（默认 ./SinyeFRP-platform-<版本>.zip）")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	self := pack.LoadSelf()
	if !self.Available() {
		return fmt.Errorf("%w：%s", pack.ErrSelfMissing, self.Reason())
	}

	// FRP 版本只用于 README 里的一句说明，探测失败不值得让整个打包失败。
	frpVersion := ""
	if bins, lerr := pack.Load(cfg.Pack.FallbackDir); lerr != nil {
		fmt.Fprintf(os.Stderr, "提示: 未能加载 FRP 二进制，README 将把内嵌 FRP 版本记为未知：%v\n", lerr)
	} else if v, verr := bins.EnsureVersion(context.Background()); verr == nil {
		frpVersion = v
	}

	spec := pack.PlatformSpec{
		Version:    version,
		FRPVersion: frpVersion,
		ListenAddr: cfg.Server.ListenAddr,
		Port:       cfg.Server.Port,
		NodeName:   cfg.Node.Name,
		NodeAPIURL: cfg.Node.APIURL,
		LogLevel:   cfg.Log.Level,
	}

	pkg, err := pack.BuildPlatformPackage(self, &spec)
	if err != nil {
		return err
	}

	target := *out
	if strings.TrimSpace(target) == "" {
		target = pkg.FileName
	}
	if dir := filepath.Dir(target); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建输出目录失败: %w", err)
		}
	}

	if err := writePackageFile(target, pkg); err != nil {
		return err
	}

	abs, _ := filepath.Abs(target)
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	fmt.Printf("已生成平台自身部署包: %s\n", abs)
	fmt.Printf("  平台版本  : %s\n", pkg.Version)
	fmt.Printf("  内嵌 FRP  : %s\n", frpVersion)
	fmt.Printf("  文件大小  : %.2f MB\n", float64(st.Size())/(1<<20))
	fmt.Printf("  主程序哈希: %s\n", self.SHA256())
	fmt.Printf("  包含文件  : %d 个\n", len(pkg.Files))
	return nil
}

// writePackageFile 把部署包写到磁盘。
func writePackageFile(path string, pkg *pack.Package) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %w", err)
	}
	if err := pkg.WriteZIP(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭输出文件失败: %w", err)
	}
	return nil
}

func run() error {
	var (
		configPath  = flag.String("config", "", "配置文件路径（默认 ./config.toml）")
		listenAddr  = flag.String("listen", "", "覆盖监听地址")
		port        = flag.Int("port", 0, "覆盖监听端口")
		nodeURL     = flag.String("node-url", "", "覆盖 FRP Dashboard 地址")
		nodeUser    = flag.String("node-user", "", "覆盖 FRP Dashboard 用户名")
		nodePass    = flag.String("node-password", "", "覆盖 FRP Dashboard 密码")
		showVersion = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", appName, version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// 命令行参数优先级最高，覆盖配置文件与内置默认值。
	if *listenAddr != "" {
		cfg.Server.ListenAddr = *listenAddr
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}
	if *nodeURL != "" {
		cfg.Node.APIURL = *nodeURL
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	logCloser, err := setupLogger(cfg)
	if err != nil {
		return err
	}
	defer logCloser()
	log := newLogger(cfg)

	log.Info("启动",
		"app", appName,
		"version", version,
		"config", cfg.Path,
		"listen", cfg.Server.Address(),
		"database", cfg.Database.Path,
	)

	if !cfg.Server.IsLoopback() {
		log.Warn("管理平台未监听环回地址，而 frps 主机可能带有公网 IP。"+
			"请确认已通过防火墙或反向代理限制访问（01-prd §8.4）",
			"listen_addr", cfg.Server.ListenAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 关停信号文件（优雅停止的可靠入口）。
	//
	// 为什么除了信号之外还需要这个：本平台在生产环境由 Windows 计划任务以
	// SYSTEM 身份、隐藏窗口方式拉起，这样的进程**没有控制台、也没有主窗口**。
	// 实测结论（.workbuddy/test/sig-check.ps1，2026-09-24）：
	//   - taskkill /PID（不带 /F）：进程纹丝不动，完全无效；
	//   - Stop-Process（不带 -Force）：进程退出了，但那只是 CloseMainWindow
	//     对无窗口进程的等价硬终止，WAL 一个字都没回写。
	// 也就是说脚本层面根本没有办法让这种进程优雅退出，20 秒等待只会
	// 白白拖长停机时间然后仍走强杀。因此改由进程自己提供入口：
	// stop.ps1 只需创建一个文件，进程轮询到即触发与 Ctrl+C 完全相同的关闭路径。
	//
	// 位置放在**数据目录**而不是临时目录：数据目录必然可写（数据库就在那儿），
	// 且随部署目录走，天然一机一实例一份，不会两个实例抢同一个信号文件。
	stopFile := filepath.Join(filepath.Dir(cfg.Database.Path), stopFileName)
	watchStopFile(ctx, stopFile, stop, log)

	st, err := store.Open(cfg.Database.Path, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Warn("关闭数据库失败", "err", err)
		}
	}()

	// 定期把 WAL 折叠回主库。见 store.Checkpoint 的说明：
	// WAL 里尚未回写的数据是「唯一副本」，进程若被强杀或掉电就会丢，
	// 表现为「能登录但记录没了」。这里兜住长期运行、从不正常关闭的场景。
	st.StartCheckpointLoop(ctx, 5*time.Minute)

	// 全新库要**明确喊出来**，不能静悄悄地走初始化向导。
	//
	// 为什么值得专门一条日志：`uninstall.ps1 -RemoveData` 曾在实例仍在运行时
	// 删掉 data\（见 pack/scripts.go 的顺序说明），而 Windows 允许删除被打开的
	// 文件 —— 平台照常运行、脚本报成功，数据已经在磁盘上没了。等下次重启，
	// 平台发现路径不存在，就**安静地新建一个空库**并进入初始化向导：
	// 运维看到的是「记录凭空消失」，日志里却一个字都没有，只能当悬案查。
	// 全新部署和误删重建在这条路径上长得一模一样，所以两种可能都要留痕，
	// 并给出可操作的下一步。
	if st.FreshlyCreated {
		log.Warn("数据库不存在，已新建空库",
			"path", cfg.Database.Path,
			"hint", "若是全新部署请忽略；若预期这里应有既有数据，说明 data\\ 目录曾被删除或移动，"+
				"请立即停止平台并从备份恢复 sinyefrp.db（连同 -wal/-shm 一起）。"+
				"注意：台账无法从 FRP 侧恢复，FRP 不返回客户端名称/类型/本地端口。")
	}

	log.Info("数据库就绪", "path", cfg.Database.Path)

	authSvc := auth.New(st, auth.Options{
		SessionTTL:       cfg.Security.SessionTTL(),
		MaxLoginFailures: cfg.Security.MaxLoginFailures,
		LockFor:          cfg.Security.LockDuration(),
		FailWindow:       cfg.Security.FailWindow(),
	}, log)

	// 采集器先以空客户端构造，随后由节点管理器按生效配置注入。
	// 这样「节点配置」只有一处真相（internal/node），不必在两个地方各解析一次。
	coll := collector.New(nil, collector.DefaultInterval, log)

	nodes, err := node.NewManager(ctx, st, coll, cfg, debugOverrides(*nodeURL, *nodeUser, *nodePass), log)
	if err != nil {
		return err
	}
	if !nodes.Current().Enabled {
		log.Warn("当前节点配置被标记为停用，采集将不会成功", "name", nodes.Current().Name)
	}
	if nodes.Current().Password == "" {
		log.Warn("未配置 FRP Dashboard 密码，采集会因认证失败而不可用；" +
			"请在管理台初始化向导或设置页补全")
	}

	initialized, err := authSvc.Initialized(ctx)
	if err != nil {
		return err
	}

	go coll.Run(ctx)

	// 加载 FRP 二进制，供部署包生成使用（01-prd F15）。
	// 加载失败不阻断启动：监控与打包是两条独立通路，
	// 缺二进制时只是打包接口返回 503，而不是整个平台不可用。
	bins, err := pack.Load(cfg.Pack.FallbackDir)
	if err != nil {
		log.Warn("未能加载 FRP 二进制，部署包生成功能将不可用",
			"err", err, "fallback_dir", cfg.Pack.FallbackDir)
	} else {
		log.Info("FRP 二进制已加载",
			"source", bins.Source,
			"client_bytes", bins.ClientSize(),
			"server_bytes", bins.ServerSize())
	}

	// 载入「平台自己」，供生成平台自身部署包使用（01-prd F18）。
	//
	// 与上面的 FRP 二进制同样按「附加能力」处理：读不到自己（文件被安全软件
	// 锁住、或已被删除）时不让平台启动失败，只让那一个下载接口返回 503。
	self := pack.LoadSelf()
	if !self.Available() {
		log.Warn("未能读取自身可执行文件，平台自身部署包功能将不可用",
			"err", self.Reason(), "path", self.Path())
	} else {
		log.Info("已载入自身可执行文件", "path", self.Path(), "bytes", self.Size())
	}

	handler := web.New(web.Deps{
		Config:    cfg,
		Store:     st,
		Auth:      authSvc,
		Nodes:     nodes,
		Collector: coll,
		Binaries:  bins,
		Self:      self,
		Logger:    log,
	}).Handler()

	srv := &http.Server{
		Addr:              cfg.Server.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		url := "http://" + cfg.Server.Address()
		log.Info("HTTP 服务已就绪", "console", url)
		if !initialized {
			// 首次启动时把引导地址打到日志，避免管理员去猜端口。
			log.Info("尚未初始化，请在浏览器打开该地址完成初始化向导", "console", url)
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case <-ctx.Done():
		log.Info("收到退出信号，正在优雅关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}

	log.Info("已退出")
	return nil
}

// debugOverrides 汇总本次进程的临时节点覆盖。
//
// 来源与优先级：命令行参数 > SINYEFRP_NODE_* 环境变量。
// 这两者都会被 internal/node 视为「覆盖」，即持久化配置保存后仍需重启才生效——
// 这是刻意的：覆盖是调试旁路，不应悄悄变成生产路径的隐式配置来源。
func debugOverrides(flagURL, flagUser, flagPass string) node.Overrides {
	return node.Overrides{
		APIURL:   firstNonEmpty(flagURL, os.Getenv(config.EnvPrefix+"NODE_API_URL")),
		Username: firstNonEmpty(flagUser, os.Getenv(config.EnvPrefix+"NODE_USERNAME")),
		Password: firstNonEmpty(flagPass, os.Getenv(config.EnvPrefix+"NODE_PASSWORD")),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// setupLogger 把日志同时写入标准输出与日志文件。
//
// 返回的清理函数负责关闭文件句柄。
func setupLogger(cfg *config.Config) (func(), error) {
	noop := func() {}
	if cfg.Log.Dir == "" {
		return noop, nil
	}
	if err := os.MkdirAll(cfg.Log.Dir, 0o750); err != nil {
		return noop, fmt.Errorf("创建日志目录失败: %w", err)
	}

	path := filepath.Join(cfg.Log.Dir, appName+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return noop, fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
	}

	// 同时写入控制台与文件，便于计划任务场景下也能在文件里追溯。
	prevStdout = io.MultiWriter(os.Stdout, f)
	return func() { _ = f.Close() }, nil
}

// prevStdout 承载日志输出目标，供 newLogger 使用。
var prevStdout io.Writer = os.Stdout

// newLogger 构造结构化日志器。
//
// 注意：日志中不得出现 Dashboard 密码或 FRP token（01-prd F17），
// 因此这里只记录地址与状态，不记录凭据。
func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	handler := slog.NewTextHandler(prevStdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
