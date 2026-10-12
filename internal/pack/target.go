package pack

import (
	"fmt"
	"strings"
)

// Platform 是部署包的**目标运行平台**。
//
// 为什么需要一个显式维度：本平台此前一直隐含假设「生成包的机器 = 跑包的机器」，
// 全部 .go 里 runtime.GOOS / GOARCH 零命中，代码里根本没有「目标系统」这个概念。
// 一旦要交付 Linux 客户端包（机器人侧常见的是 Linux 边缘盒子），这条假设就断了：
// 平台自己跑在 Windows Server 上，却要产出一包在 Linux 上装得起来、起得来的东西。
//
// 受它影响的不是一处，而是打包链路上的每一环，且每一环的正确答案都不同：
//
//   - 二进制：Windows 是 frpc.exe，Linux 是 frpc（无扩展名），且要分 amd64/arm64；
//   - 生命周期脚本：Windows 是 .ps1 + 计划任务 + .bat 双击入口，
//     Linux 是 .sh + systemd unit；
//   - 文本编码：Windows 要 BOM + CRLF，Linux 必须 LF 且**绝不能**有 BOM
//     （.sh 带 BOM 会让内核找不到 #!/bin/sh，报错却是「No such file or directory」）；
//   - 服务名：Windows 是计划任务名，Linux 是 systemd 服务名（要求小写）。
//
// 因此这里把全部平台相关决策收敛到一个类型上，避免各处各判一次 ——
// 那种散落写法正是「改漏一处、只有真装一遍才发现」的温床。
type Platform string

const (
	PlatformWindowsAMD64 Platform = "windows-amd64"
	PlatformLinuxAMD64   Platform = "linux-amd64"
	PlatformLinuxARM64   Platform = "linux-arm64"
)

// DefaultPlatform 是未指定时的平台。
//
// 固定为 Windows x64，而不是「跟随宿主」：本平台已签发的全部存量包都是 Windows 包，
// 老客户端（含已下发的脚本、回归用例、界面默认值）都不带这个字段。
// 若默认值跟着宿主走，开发机上（Windows）看起来一切正常，
// 一旦平台哪天跑在 Linux 上，同一份不带字段的请求会突然产出另一种包 ——
// 这种「输入没变、产物变了」的漂移最难查。显式默认可杜绝它。
const DefaultPlatform = PlatformWindowsAMD64

// AllPlatforms 按固定顺序列出全部目标平台，供前端下拉与测试遍历。
//
// 顺序即展示顺序：Windows 排第一（存量默认），其后按 Linux amd64 / arm64。
var AllPlatforms = []Platform{PlatformWindowsAMD64, PlatformLinuxAMD64, PlatformLinuxARM64}

// platformLabels 是各平台的中文名。
//
// 与 web/src/api/types.ts 的 PLATFORM_LABEL 是同构的两份（同 ROBOT_TYPE_LABEL 的既有分工）。
// 新增平台时**两处都要改**，否则界面会出现一个选不出中文名的值，而 vue-tsc 查不出来。
var platformLabels = map[Platform]string{
	PlatformWindowsAMD64: "Windows x64",
	PlatformLinuxAMD64:   "Linux x64",
	PlatformLinuxARM64:   "Linux ARM64",
}

// Valid 判断是否为已知平台。
func (p Platform) Valid() bool {
	for _, x := range AllPlatforms {
		if p == x {
			return true
		}
	}
	return false
}

// ParsePlatform 解析平台标识，空串返回默认值。
//
// 空串合法是刻意的：存量调用方（含全部回归用例与已保存的表单草稿）都不带这个字段，
// 判为错误会让它们一齐失败。默认值在 DefaultPlatform 里定死，不跟宿主走。
func ParsePlatform(s string) (Platform, error) {
	v := Platform(strings.TrimSpace(s))
	if v == "" {
		return DefaultPlatform, nil
	}
	if !v.Valid() {
		return "", fmt.Errorf("未知的目标平台 %q", s)
	}
	return v, nil
}

// PlatformChoices 把全部平台拼成一句人类可读的清单，用于错误提示。
//
// 与 ProxyTypeChoices / RobotTypeChoices 对称：错误文案里给出「能选哪些」，
// 比只说「不支持」更能让管理员一次改对。
func PlatformChoices() string {
	parts := make([]string, 0, len(AllPlatforms))
	for _, p := range AllPlatforms {
		parts = append(parts, string(p))
	}
	return strings.Join(parts, "、")
}

// OS 返回操作系统部分。
func (p Platform) OS() string {
	if i := strings.IndexByte(string(p), '-'); i > 0 {
		return string(p)[:i]
	}
	return string(p)
}

// Arch 返回架构部分。
func (p Platform) Arch() string {
	if i := strings.IndexByte(string(p), '-'); i > 0 && i+1 < len(p) {
		return string(p)[i+1:]
	}
	return ""
}

// IsWindows 报告目标是否为 Windows。
func (p Platform) IsWindows() bool { return p.OS() == "windows" }

// IsLinux 报告目标是否为 Linux。
func (p Platform) IsLinux() bool { return p.OS() == "linux" }

// Label 返回中文平台名；未知值原样返回。
//
// 未知值原样返回而不显示「未知」：真出现这种情况多半是降级部署或库里存了
// 本版本不认识的值，透出原始值比一个笼统的「未知」更容易定位。
func (p Platform) Label() string {
	if l, ok := platformLabels[p]; ok {
		return l
	}
	return string(p)
}

// ClientExeName 是 FRP 客户端二进制在包内的名字。
func (p Platform) ClientExeName() string {
	if p.IsWindows() {
		return "frpc.exe"
	}
	return "frpc"
}

// ServerExeName 是 FRP 服务端二进制在包内的名字。
func (p Platform) ServerExeName() string {
	if p.IsWindows() {
		return "frps.exe"
	}
	return "frps"
}

// AppExeName 是管理平台主程序在包内的名字。
func (p Platform) AppExeName() string {
	if p.IsWindows() {
		return "sinyefrp.exe"
	}
	return "sinyefrp"
}

// ServiceKind 是「开机自启」在该平台上的载体名称，用于脚本与 README 文案。
func (p Platform) ServiceKind() string {
	if p.IsWindows() {
		return "计划任务"
	}
	return "systemd 服务"
}

// ServiceName 构造该平台上本实例的服务名。
//
// 两端都遵循「前缀标归属 + 后缀保唯一」：Windows 计划任务名允许大小写，
// 沿用既有的 SinyeFRP-<role>-<id>；systemd 服务名必须小写（虽然 systemd 不强制，
// 但大写服务名在日志与 systemctl 输出里极易被误认，且不符合惯例），
// 因此 Linux 侧统一小写。
func (p Platform) ServiceName(role, id string) string {
	if p.IsWindows() {
		return taskName(role, id)
	}
	return strings.ToLower(taskName(role, id))
}

// unitFileName 是包内 systemd unit 的固定文件名。
//
// 刻意固定、不带 clientID：包内文件名稳定，README 的文件清单与 ZIP 条目
// 才不会随实例变化；唯一性由**安装后的服务名**承担（见 ServiceName），
// 安装时以服务名为目标复制进 /etc/systemd/system/。
// 同一台机器装两个客户端包时，包内文件名相同、服务名不同，互不覆盖。
const unitFileName = "sinyefrp.service"

// ScriptFileNames 返回该平台下生命周期脚本的文件名清单，顺序固定。
//
// 顺序与生成顺序、README 描述顺序、ZIP 内排列顺序共用同一份来源，
// 运维在命令行里按 tab 看到的补全顺序和文档里写的顺序完全一致。
//
// Windows 多一个「双击入口」且排在首位：解压后目录默认按名称排序，
// 入口排在脚本之前，现场人员一眼能看见「先点这个」。
// Linux 没有等价物（也不该有 —— 服务器上没有「双击」这回事），
// 但多一个 unit 文件，且它是 install.sh 的输入，故排在脚本之后。
func (p Platform) ScriptFileNames() []string {
	if p.IsWindows() {
		return []string{
			launcherName,
			"install.ps1", "uninstall.ps1", "start.ps1", "stop.ps1", "restart.ps1", "status.ps1",
		}
	}
	return []string{
		"install.sh", "uninstall.sh", "start.sh", "stop.sh", "restart.sh", "status.sh",
		unitFileName,
	}
}

// IsScriptName 报告该文件名是否为某平台下的生命周期脚本。
func (p Platform) IsScriptName(name string) bool {
	for _, n := range p.ScriptFileNames() {
		if n == name {
			return true
		}
	}
	return false
}

// WantsBOM 报告该平台下此文件是否必须带 UTF-8 BOM。
//
// 判据不是扩展名偏好，而是「这份文件由谁的解析器读」，两边容忍度恰好相反：
//
//   - Windows：.ps1 由 PowerShell 5.1 读，无 BOM 的 UTF-8 会被按 ANSI 代码页解析，
//     中文整段乱码甚至直接语法报错（本项目实测踩过）；
//   - Windows：.toml 由 FRP 读，其 TOML 解析器遇 BOM 直接报
//     `toml: invalid character at start of key: ï` 并退出 1，frpc 完全起不来；
//   - Linux：一律不要 BOM。shell 脚本带 BOM 会让内核把 `#!/bin/sh` 认成
//     `#!/bin/sh`（前三个字节不可见），报错是「No such file or directory」——
//     一个指向完全错误方向的信息，属典型的静默失败。
func (p Platform) WantsBOM(name string) bool {
	if !p.IsWindows() {
		return false
	}
	switch {
	case name == appConfigName:
		// 平台自己的 config.toml：由 BurntSushi 读（容 BOM），整篇中文注释，
		// 运维会拿记事本打开，必须带 BOM，否则中文乱码。
		return true
	case strings.HasSuffix(name, ".bat"):
		// cmd.exe 会把 BOM 当作命令的一部分，首行直接报
		// 「'ï»¿@echo' 不是内部或外部命令」，双击入口彻底失效且无任何日志。
		return false
	case strings.HasSuffix(name, ".toml"):
		// FRP 的 frpc.toml / frps.toml：遇 BOM 直接退出 1。
		return false
	case isTextName(name):
		return true
	}
	return false
}

// WantsCRLF 报告该平台下文本文件是否使用 CRLF 行尾。
//
// Windows 侧统一 CRLF 是为了记事本：Windows Server 2019 自带的老版记事本
// 另存时会把裸 LF 改写成 CRLF，而包内 config.toml 正是运维唯一会亲手改的文件 ——
// 出厂就是 CRLF，这一步对记事本才是幂等的，不会触发哈希校验提示。
//
// Linux 侧一律 LF：CRLF 的 shell 脚本会把 CR 当成命令的一部分，
// 表现为 `#!/bin/sh^M: bad interpreter`，同样是难查的静默失败。
func (p Platform) WantsCRLF() bool { return p.IsWindows() }

// NeedsExecBit 报告该文件在 ZIP 里是否需要可执行权限位。
//
// ZIP 的格式本身能带权限位（外部属性），但**只有部分解压工具会还原**：
// Windows 上无所谓（扩展名决定可执行性），Linux 上却是能不能跑起来的分界线。
// 因此 Linux 包里的二进制与 .sh 一律置 0755，其余 0644。
//
// 注意这还不够稳妥：Info-ZIP 会还原，某些图形解压工具不会。
// 所以 README 里仍保留一条 chmod +x 兜底说明（见 readme.go）。
func (p Platform) NeedsExecBit(name string) bool {
	if !p.IsLinux() {
		// Windows 包沿用原状：.exe 一直带 Exec（无害，且避免改动既有产物哈希）。
		return false
	}
	if strings.HasSuffix(name, ".sh") {
		return true
	}
	return name == p.ClientExeName() || name == p.ServerExeName() || name == p.AppExeName()
}

// AssetPath 返回该平台 FRP 二进制在内嵌资产目录中的相对路径。
//
// Windows 沿用平铺布局（assets/frpc.exe）—— 那是既有构建脚本与
// 已发布产物的约定，动它会让 build.ps1 与历史包一起失效。
// Linux 按平台分子目录，便于后续继续加架构而不污染根目录。
func (p Platform) AssetPath(kind string) string {
	return "assets/" + strings.Join(p.relPath(kind), "/")
}

// relPath 返回该平台二进制相对于资产根（或磁盘回退根）的路径片段。
//
// 磁盘回退目录（项目根的 frp/）与内嵌资产目录刻意保持同一套布局，
// 这样开发期不复制二进制也能联调，且两边只有一处定义路径规则。
func (p Platform) relPath(kind string) []string {
	if p.IsWindows() {
		return []string{kind + ".exe"}
	}
	return []string{string(p), kind}
}
