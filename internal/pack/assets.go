package pack

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 内嵌的 FRP 二进制。构建脚本负责把 frp/ 下的二进制复制到 assets/ 下
// （见 assets/PLACEHOLDER.txt）。
//
// 目录布局按目标平台分：
//
//	assets/frpc.exe            宿主平台 Windows x64（既有布局，不动）
//	assets/frps.exe
//	assets/linux-amd64/frpc    Linux x64 客户端
//	assets/linux-arm64/frpc    Linux ARM64 客户端
//
// 分平台是必需的：部署包里交付的二进制必须与目标机器匹配，
// 而 Windows 上根本 exec 不了 Linux 的 ELF（反之亦然）。
//
//go:embed assets
var assetsFS embed.FS

// 二进制在包内的固定名字（宿主/默认平台，即 Windows x64）。
//
// 保留为常量是为了让既有代码（verify.go 的落盘执行、README 生成）保持稳定；
// 涉及**目标平台**的地方一律改用 Platform.ClientExeName() 等方法，不要用这两个。
const (
	clientExeName = "frpc.exe"
	serverExeName = "frps.exe"
)

// ErrBinaryMissing 表示既没有内嵌二进制，磁盘回退目录里也找不到。
var ErrBinaryMissing = errors.New("未找到 FRP 二进制")

// ErrPlatformUnsupported 表示目标平台没有可用的二进制。
//
// 与 ErrBinaryMissing 分开：后者是「平台自己没装好」（部署错误，致命），
// 前者是「管理员选了一个本平台没带二进制的目标」（使用错误，可恢复 ——
// 换个平台就行）。混成一个会让前者也被当成部署故障去查，白跑一趟。
var ErrPlatformUnsupported = errors.New("目标平台缺少可用二进制")

// binPair 是某目标平台上的一对 FRP 二进制。
type binPair struct {
	frpc []byte
	frps []byte
}

// Binaries 持有可用的 FRP 二进制内容。
//
// frpc / frps 两个字段固定为**宿主平台**（编译期平台）的二进制：
// 它们是唯一能被本机 exec 的副本，verify -c 必须靠它们落盘执行。
//
// deliver 则是**交付给目标机器**的副本，按平台分。两者性质完全不同 ——
// 校验永远只能用宿主的（语义与部署端无关，TOML 语法校验不涉及平台行为），
// 交付则必须用目标平台的（能不能跑起来全看它）。
type Binaries struct {
	host    Platform // 宿主平台，由编译期 GOOS/GOARCH 推导
	frpc    []byte   // 宿主平台客户端二进制
	frps    []byte   // 宿主平台服务端二进制
	deliver map[Platform]binPair

	Source  string // 来源说明，用于启动日志与故障排查
	version string // 由 Verify* 首次探测后缓存
}

// hostPlatformOf 由编译期的 GOOS/GOARCH 推导宿主平台。
//
// 用编译期而不是运行期探测：go:embed 是编译期行为，内嵌了什么由构建时的
// GOOS 决定，运行期再探测只会得出一个与资产不符的结论。
// 推导不出（例如 darwin 上编译）则回退 Windows x64 —— 那种组合本来
// 就不是受支持的部署形态，让它继续走到「缺二进制」的明确报错即可。
func hostPlatformOf() Platform {
	switch runtime.GOOS {
	case "windows":
		return PlatformWindowsAMD64
	case "linux":
		if runtime.GOARCH == "arm64" {
			return PlatformLinuxARM64
		}
		return PlatformLinuxAMD64
	}
	return PlatformWindowsAMD64
}

// HostPlatform 返回宿主平台，供 verify 与其它需要用本机二进制的地方查询。
func (b *Binaries) HostPlatform() Platform {
	if b == nil {
		return DefaultPlatform
	}
	return b.host
}

// Load 加载 FRP 二进制。
//
// 优先使用内嵌副本；内嵌缺失时回退到 fallbackDir（通常是项目根的 frp/ 目录），
// 使开发期无需每次构建都复制二进制即可联调。
//
// 宿主平台的一对二进制缺失即视为致命（verify -c 无从执行）；
// 其它目标平台缺失只是该平台不可用，不阻断启动 ——
// 一台没带 Linux 二进制的平台仍应能正常签发 Windows 包。
func Load(fallbackDir string) (*Binaries, error) {
	host := hostPlatformOf()
	b := &Binaries{host: host, Source: "embedded", deliver: map[Platform]binPair{}}

	// 1) 宿主平台：必须齐全。
	var missing []string
	for _, kind := range []string{"frpc", "frps"} {
		data, err := assetsFS.ReadFile(host.AssetPath(kind))
		if err == nil && len(data) > 0 {
			b.setHost(kind, data)
			continue
		}
		missing = append(missing, host.AssetPath(kind))
	}

	if len(missing) > 0 {
		if fallbackDir == "" {
			return nil, fmt.Errorf("%w：内嵌副本缺少 %s，且未配置回退目录",
				ErrBinaryMissing, strings.Join(missing, "、"))
		}
		b.Source = "disk:" + fallbackDir
		for _, rel := range missing {
			kind := "frpc"
			if strings.HasSuffix(rel, "frps") || strings.HasSuffix(rel, "frps.exe") {
				kind = "frps"
			}
			// 回退目录的布局与资产目录一致：宿主平台平铺，其它平台分子目录。
			p := joinRel(fallbackDir, host, kind)
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("%w：内嵌副本缺少 %s，回退路径 %s 也不可读: %v",
					ErrBinaryMissing, rel, p, err)
			}
			b.setHost(kind, data)
		}
	}

	// 2) 其它目标平台：可选，缺了就只是不支持。
	for _, p := range AllPlatforms {
		if p == host {
			// 宿主那份已读入 frpc/frps，登记进 deliver 使交付路径统一。
			b.deliver[p] = binPair{frpc: b.frpc, frps: b.frps}
			continue
		}
		var pair binPair
		for _, kind := range []string{"frpc", "frps"} {
			data, err := assetsFS.ReadFile(p.AssetPath(kind))
			if err != nil || len(data) == 0 {
				if fallbackDir != "" {
					fp := joinRel(fallbackDir, p, kind)
					if fb, ferr := os.ReadFile(fp); ferr == nil && len(fb) > 0 {
						data = fb
					}
				}
			}
			if kind == "frpc" {
				pair.frpc = data
			} else {
				pair.frps = data
			}
		}
		if len(pair.frpc) > 0 || len(pair.frps) > 0 {
			b.deliver[p] = pair
		}
	}
	return b, nil
}

// joinRel 把某平台二进制的相对路径拼到给定根目录上。
//
// 单独抽出来是因为 filepath.Join 不接受「一个字符串 + 一个切片展开」的写法，
// 而这里两处调用都要这么做；内联写两遍 append 反而更容易抄错。
func joinRel(root string, p Platform, kind string) string {
	return filepath.Join(append([]string{root}, p.relPath(kind)...)...)
}

func (b *Binaries) setHost(kind string, data []byte) {
	if kind == "frpc" {
		b.frpc = data
	} else {
		b.frps = data
	}
}

// ClientBinaryFor 返回指定目标平台的客户端二进制。
//
// 拿不到就报错而不是返回空切片：空二进制会被写进 ZIP 并算出一个哈希，
// 包看起来一切正常、装上去才跑不起来 —— 那是本项目最贵的一类缺陷。
func (b *Binaries) ClientBinaryFor(p Platform) ([]byte, error) {
	if b == nil {
		return nil, fmt.Errorf("%w：二进制未加载", ErrBinaryMissing)
	}
	pair, ok := b.deliver[p]
	if !ok || len(pair.frpc) == 0 {
		return nil, fmt.Errorf("%w：%s 上没有客户端二进制（frpc）", ErrPlatformUnsupported, p.Label())
	}
	return pair.frpc, nil
}

// ServerBinaryFor 返回指定目标平台的服务端二进制。
func (b *Binaries) ServerBinaryFor(p Platform) ([]byte, error) {
	if b == nil {
		return nil, fmt.Errorf("%w：二进制未加载", ErrBinaryMissing)
	}
	pair, ok := b.deliver[p]
	if !ok || len(pair.frps) == 0 {
		return nil, fmt.Errorf("%w：%s 上没有服务端二进制（frps）", ErrPlatformUnsupported, p.Label())
	}
	return pair.frps, nil
}

// HasClient 报告指定平台是否有客户端二进制。
func (b *Binaries) HasClient(p Platform) bool {
	if b == nil {
		return false
	}
	pair, ok := b.deliver[p]
	return ok && len(pair.frpc) > 0
}

// HasServer 报告指定平台是否有服务端二进制。
func (b *Binaries) HasServer(p Platform) bool {
	if b == nil {
		return false
	}
	pair, ok := b.deliver[p]
	return ok && len(pair.frps) > 0
}

// ClientPlatforms 返回可作为客户端包目标的平台列表。
//
// 只列**真的有二进制**的平台：界面下拉里放一个选了必然失败的平台，
// 比不放更糟 —— 管理员会在装到一半时才知道这台机器根本不被支持。
func (b *Binaries) ClientPlatforms() []Platform {
	out := make([]Platform, 0, len(AllPlatforms))
	for _, p := range AllPlatforms {
		if b.HasClient(p) {
			out = append(out, p)
		}
	}
	return out
}

// ServerPlatforms 返回可作为服务器包目标的平台列表。
func (b *Binaries) ServerPlatforms() []Platform {
	out := make([]Platform, 0, len(AllPlatforms))
	for _, p := range AllPlatforms {
		if b.HasServer(p) {
			out = append(out, p)
		}
	}
	return out
}

// ClientSHA256For 返回指定平台客户端二进制的哈希。
func (b *Binaries) ClientSHA256For(p Platform) string {
	if pair, ok := b.deliver[p]; ok {
		return sumHex(pair.frpc)
	}
	return ""
}

// ServerSHA256For 返回指定平台服务端二进制的哈希。
func (b *Binaries) ServerSHA256For(p Platform) string {
	if pair, ok := b.deliver[p]; ok {
		return sumHex(pair.frps)
	}
	return ""
}

// ClientSizeFor 返回指定平台客户端二进制体积。
func (b *Binaries) ClientSizeFor(p Platform) int {
	if pair, ok := b.deliver[p]; ok {
		return len(pair.frpc)
	}
	return 0
}

// ServerSizeFor 返回指定平台服务端二进制体积。
func (b *Binaries) ServerSizeFor(p Platform) int {
	if pair, ok := b.deliver[p]; ok {
		return len(pair.frps)
	}
	return 0
}

// ClientSHA256 返回**默认平台**客户端二进制的哈希，用于 01-prd F15 的记录与交付校验。
func (b *Binaries) ClientSHA256() string { return b.ClientSHA256For(DefaultPlatform) }

// ServerSHA256 返回**默认平台**服务端二进制的哈希。
func (b *Binaries) ServerSHA256() string { return b.ServerSHA256For(DefaultPlatform) }

// ClientSize 返回**默认平台**客户端二进制体积，供界面展示。
func (b *Binaries) ClientSize() int { return b.ClientSizeFor(DefaultPlatform) }

// ServerSize 返回**默认平台**服务端二进制体积。
func (b *Binaries) ServerSize() int { return b.ServerSizeFor(DefaultPlatform) }

// Version 返回缓存的 FRP 版本；尚未探测时为空串。
func (b *Binaries) Version() string { return b.version }

func sumHex(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SumBytes 计算字节内容的 SHA256 十六进制值。
func SumBytes(data []byte) string { return sumHex(data) }

// materialize 把二进制写到磁盘上的可执行文件，供 verify -c 调用。
//
// 目标路径按内容哈希分目录，因此重复调用天然幂等：同一份二进制只会写一次，
// 升级 FRP 版本后哈希变化会自动落到新目录，不会出现新旧混用。
//
// 只用宿主平台的二进制调用：在 Windows 上 exec 一个 Linux ELF 必然失败，
// 而 TOML 校验恰巧与平台无关，所以「拿宿主那份校验所有平台的配置」是
// 既正确又省事的做法 —— 不必为了校验而内嵌各平台的可执行副本。
func (b *Binaries) materialize(name string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("%w：%s 内容为空", ErrBinaryMissing, name)
	}
	h := sumHex(data)
	dir := filepath.Join(os.TempDir(), "sinyefrp-bin", h[:16])
	target := filepath.Join(dir, name)

	if st, err := os.Stat(target); err == nil && st.Size() == int64(len(data)) {
		return target, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建二进制缓存目录失败: %w", err)
	}
	// 先写临时文件再改名，避免并发请求读到写了一半的文件。
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", name, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("落地 %s 失败: %w", name, err)
	}
	return target, nil
}
