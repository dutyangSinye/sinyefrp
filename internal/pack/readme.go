package pack

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// README 模板。两个角色共用同一骨架，差异部分用条件分支表达。
//
// README 与脚本一样以 UTF-8 with BOM 落盘：运维会用记事本直接打开它，
// 而简体中文 Windows 的记事本在无 BOM 时会按 GBK 解释，中文会全部乱码。
const readmeTpl = `SinyeFRP {{.Role}}部署包
{{.Rule}}

显示名称   : {{.DisplayName}}
{{if .ClientID}}客户端标识 : {{.ClientID}}
{{end}}{{if .RobotType}}机器人类型 : {{.RobotType}}
{{end}}FRP 版本   : {{.Version}}
生成时间   : {{.Now}}
生成工具   : SinyeFRP 管理平台

{{if .IsClient}}【重要】一台设备一个包
--------------------------------
本包内含唯一的客户端标识（clientID）与认证 token，用于在服务器侧识别
这一台设备。请勿把同一个包复制到多台机器——那会让多台设备共用同一身份，
表现为客户端列表里反复上下线、代理互相顶掉。

需要部署多台设备时，请在管理平台为每台设备分别生成部署包。
{{else}}【重要】本包内含服务器凭据
--------------------------------
本包内的 frps.toml 含有认证 token 与 Dashboard 密码，属于敏感文件。
请勿通过即时通讯、邮件等明文渠道传递，也不要把解压后的目录放在共享盘上。
Dashboard 默认只监听 127.0.0.1，仅供本机管理平台读取。
{{end}}
包内文件
--------
{{.FileList}}
SHA256SUMS.txt   各文件的 SHA256 校验值
README.txt       本说明文件

安装步骤
--------
1. 将整个目录复制到目标机器（路径可以包含空格），例如：
       {{.ExampleDir}}

2. 【推荐】双击包内的「{{.LauncherName}}」。
   它会自动请求管理员权限（弹出「用户账户控制」时点「是」），随后完成安装、
   注册开机自启并立即启动。装完窗口会停住，看到「安装完成」再按回车关闭即可。

   这条路不需要你打开 PowerShell、不需要知道 .\ 前缀、也不受脚本执行策略影响 ——
   这三个是现场装包最常见的失败原因，入口文件已经全部替你绕过。

   也可以沿用命令行方式（效果完全一样：入口调用的就是同一个脚本，
   两者只有提权方式不同）。以管理员身份打开 PowerShell，进入该目录后执行：
       cd {{.ExampleDir}}
       .\install.ps1

3. 确认运行状态：
       .\status.ps1

   若你按目标环境改过配置文件，安装会提示「与出厂版本不一致」但会继续，
   不需要额外开关；只有主程序与出厂版本不一致时，安装才会中止。

卸载
----
    .\uninstall.ps1

默认只移除开机自启并停止进程，配置文件与日志会保留，便于事后排查或重装。
如需连配置和日志一并删除，请使用：
    .\uninstall.ps1 -RemoveData

常用问答
--------
Q: 双击「{{.LauncherName}}」没反应，或者文件名显示成一堆乱码
A: 文件名乱码说明解压工具没有按 UTF-8 解析它（部分老版本压缩软件会这样）。
   请删掉已解压的目录，改用资源管理器自带的「全部解压缩」或 7-Zip 重新解压。
   文件名正常但双击无反应时，右键点它选「以管理员身份运行」——
   这样窗口不会一闪而过，能看到具体报错。

Q: 弹出「用户账户控制」时点了「否」
A: 安装不会执行，窗口会提示「已取消管理员授权」。安装要在机器上注册开机自启
   任务，这一步必须有管理员权限。如果你的账户不是本机管理员，
   需要请管理员在这一步输入管理员密码。

Q: 提示「无法将 install.ps1 项识别为 cmdlet、函数、脚本文件或可运行程序的名称」
A: 这是 PowerShell 的默认安全行为：它不会从当前目录加载命令，必须显式加 .\ 前缀。
   正确写法是 .\install.ps1；只写 install.ps1 就会报这个错。其余脚本同理。
   （双击「{{.LauncherName}}」不会遇到这个问题。）

Q: 提示「禁止运行脚本」「无法加载文件，因为在此系统上禁止运行脚本」
A: 这是 PowerShell 执行策略限制，不是包的问题。以管理员身份执行下面这条
   命令后重试（只对当前窗口生效，不会修改你机器上的长期设置）：
       Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
   （双击「{{.LauncherName}}」不会遇到这个问题：入口自带 -ExecutionPolicy Bypass，
     无需你手工改任何设置。）

Q: 安装后进程没有起来
A: 执行 .\status.ps1 查看任务状态；若任务已注册但进程未出现，请查看本目录
   下的 {{.LogName}}，里面通常是连接失败或认证失败的具体原因。

Q: 开机没有自动启动
A: 任务以 SYSTEM 身份在「系统启动时」运行，不依赖用户登录。
   执行 .\status.ps1，若任务状态异常请重新执行 .\install.ps1（可重复执行，
   只会更新原任务，不会产生重复任务）。

Q: 日志在哪里
A: 本目录下的 {{.LogName}}，按天滚动，保留 {{.LogDays}} 天。
{{if .IsServer}}
需要放行的端口
--------------
本包不会自动修改防火墙。请由管理员按需放行：

  {{.BindPort}}   FRP 服务端口，需要允许来自公网的入站连接（必须）
  {{.DashPort}}   Dashboard 端口，默认只监听 {{.DashAddr}}，无需对公网开放
{{range .ExtraPorts}}{{.}}
{{end}}
其余端口按代理配置的实际需要另行放行。请勿一次性放行全部端口。
{{else}}
网络要求
--------
本包不需要任何入站端口。目标机器需要能够主动访问：
    {{.ServerTarget}}
请确认该机器的出站策略与安全组允许这条连接。
{{end}}
退出码
------
{{.ExitCodes}}
`

// readmeLinuxTpl 是 Linux 目标平台的 README 模板。
//
// 与 Windows 那份**并列而非共用**：两者除了「显示名称 / 包内文件 / 退出码」
// 这几段之外几乎没有重合 —— Windows 那份有一半篇幅在讲 PowerShell 执行策略、
// UAC 提权与 .\ 前缀，这些在 Linux 上全都不存在；Linux 该讲的则是 sudo、
// systemd 与执行位。硬塞进一个模板只会让两边都塞满条件分支，
// 而分支正是「改一处漏一处」的入口。
const readmeLinuxTpl = `SinyeFRP {{.Role}}部署包
{{.Rule}}

显示名称   : {{.DisplayName}}
{{if .ClientID}}客户端标识 : {{.ClientID}}
{{end}}{{if .RobotType}}机器人类型 : {{.RobotType}}
{{end}}FRP 版本   : {{.Version}}
目标平台   : {{.PlatformLabel}}
生成时间   : {{.Now}}
生成工具   : SinyeFRP 管理平台

【重要】一台设备一个包
--------------------------------
本包内含唯一的客户端标识（clientID）与认证 token，用于在服务器侧识别
这一台设备。请勿把同一个包复制到多台机器——那会让多台设备共用同一身份，
表现为客户端列表里反复上下线、代理互相顶掉。

需要部署多台设备时，请在管理平台为每台设备分别生成部署包。

包内文件
--------
{{.FileList}}
SHA256SUMS.txt   各文件的 SHA256 校验值
README.txt       本说明文件

安装步骤
--------
1. 将整个目录复制到目标机器，例如：
       {{.ExampleDir}}

2. 执行安装（需要 root 权限）：
       cd {{.ExampleDir}}
       sudo ./install.sh

   安装会做四件事：校验文件完整性、把 systemd 服务注册为开机自启、
   停止同目录的旧进程、立即启动。可重复执行，只会更新原服务。

3. 确认运行状态：
       sudo ./status.sh

   若你按目标环境改过配置文件，安装会提示「与出厂版本不一致」但会继续，
   不需要额外开关；只有主程序与出厂版本不一致时，安装才会中止。

卸载
----
    sudo ./uninstall.sh

默认只移除开机自启并停止进程，配置文件与日志会保留，便于事后排查或重装。
如需连配置和日志一并删除，请使用：
    sudo ./uninstall.sh --remove-data

常用问答
--------
Q: 提示「Permission denied」
A: 注册开机自启需要 root 权限，请在命令前加 sudo。
   若脚本本身没有执行权限，先执行一次：
       chmod +x *.sh ./{{.ExeName}}
   部分解压工具不会还原 ZIP 里记录的执行位，这是已知现象，不是包损坏。

Q: 提示「未找到 systemctl」
A: 本包只支持 systemd。请确认目标系统是带 systemd 的发行版
   （Ubuntu 20.04+ / Debian 10+ / CentOS 7+ 等）。不带 systemd 的环境
   只能手工启动，且不会有开机自启：
       ./{{.ExeName}} -c ./{{.ConfName}}

Q: 安装后进程没有起来
A: 执行 sudo ./status.sh 查看服务状态；若服务已注册但进程未出现，
   请查看本目录下的 {{.LogName}}，或执行：
       journalctl -u {{.ServiceName}} -n 100

Q: 开机没有自动启动
A: 服务以 multi-user.target 注册，开机即起，不依赖用户登录。
   执行 sudo ./status.sh 查看「开机自启」一行的状态；
   若为 disabled，重新执行 sudo ./install.sh（可重复执行，只会更新原服务）。

Q: 会不会影响机器上已有的 frp
A: 不会。所有脚本只按「可执行文件的绝对路径」认领本目录的实例，
   不按进程名批量结束，因此同机并存的其他 frp 部署不受影响。

Q: 日志在哪里
A: 本目录下的 {{.LogName}}，按天滚动，保留 {{.LogDays}} 天。

网络要求
--------
本包不需要任何入站端口。目标机器需要能够主动访问：
    {{.ServerTarget}}
请确认该机器的出站策略与安全组允许这条连接。

退出码
------
{{.ExitCodes}}

服务与进程
----------
服务名     : {{.ServiceName}}
unit 文件  : /etc/systemd/system/{{.ServiceName}}.service
查看服务   : systemctl status {{.ServiceName}}
查看日志   : journalctl -u {{.ServiceName}} -f
`

// ReadmeData 是渲染 README 的数据。
type ReadmeData struct {
	Role      string
	Rule      string
	Platform  Platform // 决定用哪套模板与退出码文案；空值时取 DefaultPlatform
	PlatformLabel string
	ServiceName   string // Linux 专有：systemd 服务名，写进排查指引
	ExeName       string // 二进制文件名，Linux README 的手工启动示例要用
	ConfName      string // 配置文件名，同上
	DisplayName string
	ClientID    string
	Version     string
	Now         string
	IsClient    bool
	IsServer    bool
	FileList    string
	ExampleDir  string
	LogName     string
	LogDays     int
	ExitCodes   string

	// 服务器专有
	BindPort   int
	DashPort   int
	DashAddr   string
	ExtraPorts []string

	// 客户端专有
	ServerTarget string
	// RobotType 是这台设备的机器人类型中文名（已由调用方翻译），
	// 为空表示未分类，此时 README 不出现这一行。只有客户端包会填。
	RobotType string

	// LauncherName 是包内「双击安装」入口的文件名。
	//
	// 由 renderReadme 兜底填 launcherName，调用方不必逐个传 ——
	// README 里要让用户「双击这个文件」，名字写错一个字这份说明就废了，
	// 所以它必须与生成 ZIP 时用的是同一个常量。
	LauncherName string
}

// renderReadme 生成 README.txt 内容（UTF-8 with BOM）。
func renderReadme(d ReadmeData) ([]byte, error) {
	if d.Now == "" {
		d.Now = time.Now().Format("2006-01-02 15:04:05")
	}
	if d.LauncherName == "" {
		d.LauncherName = launcherName
	}
	if d.Platform == "" {
		d.Platform = DefaultPlatform
	}
	if d.PlatformLabel == "" {
		d.PlatformLabel = d.Platform.Label()
	}
	if d.ExitCodes == "" {
		// 退出码**数值**跨平台一致（监控用同一套判据），
		// 但文案必须跟着平台走：Linux 上没有「计划任务」，权限也不叫「管理员」。
		codes := ScriptExitCodes
		if !d.Platform.IsWindows() {
			codes = LinuxScriptExitCodes
		}
		var b strings.Builder
		for i, c := range codes {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%-4d %s", c.Code, c.Mean)
		}
		d.ExitCodes = b.String()
	}

	tplName := "readme-windows"
	tplBody := readmeTpl
	if !d.Platform.IsWindows() {
		tplName = "readme-linux"
		tplBody = readmeLinuxTpl
	}

	t, err := template.New(tplName).Parse(tplBody)
	if err != nil {
		return nil, fmt.Errorf("解析 README 模板失败: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("渲染 README 失败: %w", err)
	}
	return withBOM(buf.Bytes()), nil
}
