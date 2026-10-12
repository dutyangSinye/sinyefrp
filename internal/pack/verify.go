package pack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sinyefrp/internal/frpapi"
)

// verifyTimeout 是单次配置校验的上限。
//
// frp 的 verify 只做语法与字段解析，正常在毫秒级完成；
// 设 10 秒是为了容忍首次执行时的磁盘抖动与杀毒软件扫描，
// 而不是因为校验本身慢。
const verifyTimeout = 10 * time.Second

// VerifyError 表示 FRP 拒绝了这份配置。
//
// 刻意保留原始输出：01-prd F15 要求「校验失败禁止下载并显示具体错误」，
// 而 FRP 自己给出的错误信息（哪个字段、什么值不合法）远比本包能猜测的准确。
type VerifyError struct {
	Role   string
	Output string
	Err    error
}

func (e *VerifyError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("FRP 拒绝了%s配置: %v", e.Role, e.Err)
	}
	return fmt.Sprintf("FRP 拒绝了%s配置: %s", e.Role, e.Output)
}

// Unwrap 保留底层退出错误，便于 errors.Is/As 判断。
func (e *VerifyError) Unwrap() error { return e.Err }

// VerifyResult 是一次成功校验的结果。
type VerifyResult struct {
	Role    string
	Version string
	Output  string
}

// 版本探测串行化并缓存：探测要跑一次子进程，没必要每个请求都跑；
// 但也不能用 sync.Once —— 那样首次探测因故失败后，进程余生都拿不到版本。
var versionMu sync.Mutex

// VerifyClientConfig 用内嵌的 frpc 校验客户端配置。
//
// 刻意用**宿主平台**那份二进制，与目标平台无关：校验的是 TOML 的语法与字段
// 是否被该版本接受，这份语义在所有平台上完全一致，而本机只能 exec 自己这一种格式。
// 因此不必为了校验去内嵌各平台的可执行副本 —— 那是白白多背几十兆体积。
func (b *Binaries) VerifyClientConfig(ctx context.Context, tomlContent []byte) (*VerifyResult, error) {
	return b.verify(ctx, "客户端", b.host.ClientExeName(), b.frpc, tomlContent)
}

// VerifyServerConfig 用内嵌的 frps 校验服务器配置。
func (b *Binaries) VerifyServerConfig(ctx context.Context, tomlContent []byte) (*VerifyResult, error) {
	return b.verify(ctx, "服务器", b.host.ServerExeName(), b.frps, tomlContent)
}

// verify 执行 `<exe> verify -c <file>` 并对结果分类。
//
// 这是本项目的核心防线：配置字段是否与 FRP 版本匹配，由 FRP 二进制自己裁决。
// 本项目因此在 spec.go 里可以只做业务规则校验（端口范围、名称格式等），
// 而不必（也不可能）复制一份随版本漂移的官方字段定义。
func (b *Binaries) verify(ctx context.Context, role, exeName string, data, tomlContent []byte) (*VerifyResult, error) {
	exe, err := b.materialize(exeName, data)
	if err != nil {
		return nil, err
	}

	// 校验用的配置写在临时目录，并让 frp 以该目录为工作目录：
	// 配置里 log.to 是相对路径，这样它写出的日志会一并被清理，不污染部署目录。
	tmpDir, err := os.MkdirTemp("", "sinyefrp-verify-")
	if err != nil {
		return nil, fmt.Errorf("创建校验临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// 待校验配置与二进制同名、扩展名换成 .toml。
	// Windows 是 frpc.exe → frpc.toml；Linux 是 frpc → frpc.toml
	//（无 .exe 后缀时 TrimSuffix 原样返回，结果恰好一致）。
	// 这个巧合以前一直成立，但目标平台一多就该显式说清楚，
	// 否则将来谁给 Linux 二进制加了后缀，这里会静默拼错文件名。
	confPath := filepath.Join(tmpDir, strings.TrimSuffix(exeName, ".exe")+".toml")
	if err := os.WriteFile(confPath, tomlContent, 0o600); err != nil {
		return nil, fmt.Errorf("写入待校验配置失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, "verify", "-c", confPath)
	cmd.Dir = tmpDir
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := cmd.Run()
	text := strings.TrimSpace(out.String())

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("校验%s配置超时（超过 %s）", role, verifyTimeout)
	}
	if runErr != nil {
		return nil, &VerifyError{Role: role, Output: text, Err: runErr}
	}

	version, err := b.ensureVersion(ctx, exe)
	if err != nil {
		// 版本探测失败不阻断打包：配置已经通过校验，这是本步骤的硬目标；
		// 版本号仅用于写入 README 与审计记录。
		version = "unknown"
	}

	return &VerifyResult{Role: role, Version: version, Output: text}, nil
}

// EnsureVersion 探测并缓存 FRP 版本号（01-prd F15：记录 FRP 版本）。
//
// 刻意不放在启动路径上：探测要执行一次子进程，而服务就绪不该被它拖慢。
// 由首次真正需要版本的接口触发，之后命中缓存；探测失败不会留下永久性失败状态。
func (b *Binaries) EnsureVersion(ctx context.Context) (string, error) {
	if v := b.Version(); v != "" {
		return v, nil
	}
	if len(b.frpc) == 0 {
		return "", fmt.Errorf("%w：frpc.exe 内容为空", ErrBinaryMissing)
	}
	exe, err := b.materialize(b.host.ClientExeName(), b.frpc)
	if err != nil {
		return "", err
	}
	return b.ensureVersion(ctx, exe)
}

func (b *Binaries) ensureVersion(ctx context.Context, exe string) (string, error) {
	versionMu.Lock()
	defer versionMu.Unlock()

	if b.version != "" {
		return b.version, nil
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	cmd := exec.CommandContext(cctx, exe, "--version")
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("探测 FRP 版本失败: %w（输出: %s）", err, strings.TrimSpace(out.String()))
	}
	// --version 输出形如 "0.69.0"，可能带前缀文字，取最后一个字段。
	fields := strings.Fields(strings.TrimSpace(out.String()))
	if len(fields) == 0 {
		return "", fmt.Errorf("FRP --version 输出为空")
	}
	b.version = fields[len(fields)-1]
	return b.version, nil
}

// VersionWarning 在内嵌二进制版本超出正式支持范围时返回提示，否则返回空串。
//
// 与已打包的二进制实际版本做比对，落在范围外时给出警告而不是静默通过——
// 让「不保证兼容」这件事在界面上可见，而不是等到线上出问题才发现。
//
// 支持范围**直接复用 frpapi 的定义，本包不再另存一份**：
// 2026-09-25 升级 0.71.0 时，这两处各有一个 SupportedVersionPrefix = "0.69."，
// 改一处漏一处不会编译报错、只会在界面上留下自相矛盾的提示。
func (b *Binaries) VersionWarning() string {
	v := b.Version()
	if v == "" || v == "unknown" {
		return "未能探测到 FRP 版本，无法确认兼容性。"
	}
	if !frpapi.VersionCompatible(v) {
		return fmt.Sprintf("当前内嵌的 FRP 版本为 %s，不在正式支持的 %s 范围内；"+
			"生成与监控功能可用，但字段兼容性未经测试。", v, frpapi.SupportedVersionsText())
	}
	return ""
}
