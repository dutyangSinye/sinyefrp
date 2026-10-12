package pack

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"sinyefrp/internal/config"
)

// 本文件实现「管理平台自身部署包」的生成（01-prd F18）。
//
// 它和客户端/服务端包共享同一套生命周期脚本（internal/pack/scripts.go），
// 但有自己的配置模板与说明文件：平台要交付的是「一个 exe 加一份配置」，
// 没有代理、没有 token、没有要放行的端口，注意事项几乎不相交。
//
// 有一条容易被忽略的连锁关系值得写在这里：
// 平台包的 install.ps1 会预建 data\ 与 logs\ 并收紧 ACL，而 config.toml 里的
// 相对路径按「配置文件所在目录」解析（internal/config 的 resolvePaths）。
// 两者必须同时成立 —— 若路径按进程工作目录解析，计划任务以 SYSTEM 拉起时
// 工作目录是 C:\Windows\System32，数据库会被写进系统目录，而 ACL 却加在了
// 部署目录上，等于既丢数据又没保护到。

// appExeName 是管理平台主程序在包内的固定名字。
//
// 与 frp 的 clientExeName / serverExeName 并列但性质不同：那两个是内嵌资源，
// 这个是「运行中的自己」的副本（见 Self）。
const appExeName = "sinyefrp.exe"

// appConfigName 是管理平台配置文件在包内的固定名字。
//
// 单独立成常量，是因为它被赋予了一项与 frp 包相反的性质：这份 TOML 由
// internal/config 读（BurntSushi，容 BOM），而 frpc.toml / frps.toml 由 FRP 读
// （其解析器遇 BOM 直接报错）。normalizeText 靠这个常量把两者分开 ——
// 名字写死成字面量时，将来改名就会悄悄改变这条判定。
const appConfigName = "config.toml"

// platformTaskName 是平台自身计划任务名。
//
// 不含变量后缀：一台机器上只应有一个管理台实例，
// 多实例会同时写同一个 FRP Dashboard 并让采集结果互相覆盖。
const platformTaskName = "SinyeFRP-platform"

// ErrSelfMissing 表示读不到当前进程的可执行文件，无法生成平台自身部署包。
var ErrSelfMissing = errors.New("无法读取管理平台自身可执行文件")

// Self 持有「管理平台自己」的可执行文件内容。
//
// 为什么需要它：平台包要交付一份能重新部署的自身副本，而这份副本只存在于
// 当前正在运行的这个文件里 —— 它不像 frpc.exe / frps.exe 那样能在编译期内嵌
// （自己内嵌自己是不可能的）。
//
// 与 Binaries 分开的理由：两者的来源与失效条件完全不同。frp 二进制在编译期
// 焊死，永远可用；自身副本依赖启动方式（开发期是 go run 的临时文件，
// 交付期是 bin\sinyefrp.exe），可能在文件被占用或已删除时读不到。
type Self struct {
	data []byte
	path string
	err  error
}

// LoadSelf 读取当前进程的可执行文件。
//
// 刻意不返回 error：读不到自己不该让整个平台起不来 —— 那只是「生成平台自身
// 部署包」这一个附加能力不可用，监控与生成 frp 部署包都不受影响。
// 调用方用 Available() 判断，并把 Reason() 作为接口层的错误信息。
func LoadSelf() *Self {
	p, err := os.Executable()
	if err != nil {
		return &Self{err: fmt.Errorf("无法定位当前可执行文件: %w", err)}
	}
	// 解析符号链接：部分部署方式下 os.Executable 返回链接路径，
	// 直接读会拿到链接本身而不是目标文件。
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		p = resolved
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return &Self{path: p, err: fmt.Errorf("读取 %s 失败: %w", p, err)}
	}
	return &Self{path: p, data: data}
}

// Available 报告自身上传内容是否可用。
func (s *Self) Available() bool { return s != nil && s.err == nil && len(s.data) > 0 }

// Reason 返回不可用原因；可用时返回空串。
func (s *Self) Reason() string {
	if s == nil {
		return "未加载当前可执行文件"
	}
	if s.err != nil {
		return s.err.Error()
	}
	return ""
}

// Path 返回自身可执行文件的绝对路径，仅用于界面展示。
func (s *Self) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Size 返回自身可执行文件的字节数。
func (s *Self) Size() int {
	if s == nil {
		return 0
	}
	return len(s.data)
}

// SHA256 返回自身可执行文件的哈希，写入包内 SHA256SUMS.txt 与生成记录。
func (s *Self) SHA256() string {
	if s == nil {
		return ""
	}
	return sumHex(s.data)
}

// PlatformSpec 是管理平台自身部署包的生成参数。
//
// 与 ClientSpec / ServerSpec 不同，它没有「管理员填的字段」：平台自身的监听
// 地址与节点地址都来自当前进程的生效配置，由调用方注入。这仍然是必要的
// —— 若把端口写死成默认的 7800，那么一个把端口改成 7801 的部署生成出来的
// 新包会退回 7800，管理员要自己发现并改配置。
type PlatformSpec struct {
	// Version 是管理平台版本号，写进 README、脚本头与文件清单。
	Version string
	// FRPVersion 是内嵌的 FRP 版本，仅用于 README 说明，可为空。
	FRPVersion string
	// ListenAddr / Port 写入包内 config.toml 的 [server] 段。
	ListenAddr string
	Port       int
	// NodeName / NodeAPIURL 写入 [node] 段，作为「数据库里还没有节点配置时」的兜底。
	NodeName   string
	NodeAPIURL string
	// LogLevel 写入 [log] 段。
	LogLevel string
}

// Normalize 填充缺省值。
//
// 默认值一律取自 internal/config 而不是在本文件里重新写一遍：
// 「平台默认监听 7800」这条事实只应该有一个定义处，否则改了一边、
// 另一边生成的包就会静默地用旧默认值，而它自己的配置校验还会通过。
func (s *PlatformSpec) Normalize() {
	d := config.Default()

	if strings.TrimSpace(s.Version) == "" {
		s.Version = "unknown"
	}
	if strings.TrimSpace(s.ListenAddr) == "" {
		s.ListenAddr = d.Server.ListenAddr
	}
	if s.Port == 0 {
		s.Port = d.Server.Port
	}
	if strings.TrimSpace(s.NodeName) == "" {
		s.NodeName = d.Node.Name
	}
	if strings.TrimSpace(s.NodeAPIURL) == "" {
		s.NodeAPIURL = d.Node.APIURL
	}
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		s.LogLevel = d.Log.Level
	}
}

// Validate 校验生成参数。
//
// 复用 config 自身的 Validate：端口是否与 frps/Dashboard 冲突、URL 是否合法、
// 日志级别是否受支持，这些规则已经在启动路径上定义过一次了。
// 在这里重写一遍既会漂移，又会给出与启动时报错不一致的文案。
func (s *PlatformSpec) Validate() []error {
	c := config.Default()
	c.Server.ListenAddr = s.ListenAddr
	c.Server.Port = s.Port
	c.Node.APIURL = s.NodeAPIURL
	c.Log.Level = s.LogLevel
	if err := c.Validate(); err != nil {
		return []error{err}
	}
	return nil
}

// platformConfigTpl 是包内 config.toml 的模板。
//
// 刻意手写而不是用 TOML 编码器序列化 config.Config：编码器给不出注释，
// 而这份文件是运维拿到手后唯一要读的东西 —— 「为什么不能改成 0.0.0.0」
// 「相对路径按哪解析」这类结论必须写在它旁边，而不是另找文档。
// 关键取值仍由 PlatformSpec 注入，保证与当前部署一致。
const platformConfigTpl = `# SinyeFRP 管理平台配置
#
# 生成时间 : {{.Now}}
# 生成工具 : SinyeFRP 管理平台（版本 {{.Version}}）
#
# 开发环境与生产环境共用同一份代码，差异只体现在本文件。
# 优先级：本文件 < SINYEFRP_* 环境变量 < 命令行参数。
#
# 本文件可以按目标环境自由修改（端口、节点名、Dashboard 地址都是常见改动）：
# install.ps1 对它的校验只提示、不阻断，改完直接装即可，不需要任何额外开关；
# 只有主程序 sinyefrp.exe 与出厂版本不一致时安装才会中止。
#
# 【重要】本文件中的相对路径一律按「本文件所在目录」解析，与谁拉起进程无关。
# 计划任务以 SYSTEM 身份在系统启动时拉起时，进程工作目录是 C:\Windows\System32；
# 若改成按工作目录解析，数据库会被写进系统目录，整个部署目录搬走也会错位。

[server]
# 仅监听环回地址。这台机器可能带有公网 IP，改成 0.0.0.0 会把管理控制台
# 直接暴露到公网。需要远程访问请用隧道或前置反向代理。
listen_addr = "{{.ListenAddr}}"
port = {{.Port}}

[database]
path = "data/sinyefrp.db"

[log]
dir = "logs"
level = "{{.LogLevel}}"

[security]
# 会话有效期（小时）。每次请求滑动续期。
session_hours = 12
# 连续失败多少次后锁定；锁定窗口与时长见下两项。
max_login_failures = 5
fail_window_minutes = 10
lock_minutes = 15

[node]
name = "{{.NodeName}}"
# 被管理 frps 的 Dashboard 地址。与 frps 同机部署时保持环回即可。
#
# 注意这与「客户端接入点」是两件事：这里是管理台采集用的地址（走环回或内网），
# 不是 frpc 要连的对外地址。后者在管理台的「设置 → 客户端接入点」里单独配置。
#
# 这里只是「数据库里还没有节点配置时」的兜底：一旦在初始化向导或设置页
# 保存过节点，就以数据库为准。
api_url = "{{.NodeAPIURL}}"
username = "admin"
# 不在此处存放明文凭据，改为在界面上填写，之后由平台用 DPAPI 加密后存入 SQLite。
# 调试旁路 SINYEFRP_NODE_PASSWORD 只用于开发期，生产环境不要设置。
password = ""
timeout_seconds = 5

[pack]
# 正式交付件已把 frpc.exe / frps.exe 编译进 exe 本体（解压即用、不装运行时），
# 运行时不读这个目录；它只服务于开发期联调。保留默认值即可。
fallback_dir = "frp"
`

// platformConfigData 是配置模板的填充数据。
type platformConfigData struct {
	Now        string
	Version    string
	ListenAddr string
	Port       int
	LogLevel   string
	NodeName   string
	NodeAPIURL string
}

// RenderPlatformConfig 生成包内 config.toml 内容（含 BOM）。
//
// 加 BOM 与脚本同理：运维大概率会用记事本打开它，而简体中文 Windows 的记事本
// 在无 BOM 时会按 GBK 解释，文件里的中文注释会整段乱码。
func RenderPlatformConfig(spec *PlatformSpec) ([]byte, error) {
	cp := *spec
	cp.Normalize()

	t, err := template.New("platform-config").Parse(platformConfigTpl)
	if err != nil {
		return nil, fmt.Errorf("解析平台配置模板失败: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, platformConfigData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		Version:    cp.Version,
		ListenAddr: cp.ListenAddr,
		Port:       cp.Port,
		LogLevel:   cp.LogLevel,
		NodeName:   cp.NodeName,
		NodeAPIURL: cp.NodeAPIURL,
	}); err != nil {
		return nil, fmt.Errorf("渲染平台配置失败: %w", err)
	}
	return withBOM(buf.Bytes()), nil
}

// platformReadmeTpl 是管理平台自身部署包的说明文件模板。
//
// 与 frp 包的 readmeTpl 分开而不是共用骨架加分支：两者的注意事项几乎不相交
// （frp 讲「一台设备一个包」、要放行的端口、出站网络要求；平台讲数据目录权限、
// 数据迁移、管理台访问方式）。塞进同一个模板只会让两边的读者都被无关内容干扰。
const platformReadmeTpl = `SinyeFRP 管理平台部署包
======================

版本       : {{.Version}}
内嵌 FRP   : {{.FRPVersion}}
生成时间   : {{.Now}}
生成工具   : SinyeFRP 管理平台
管理台地址 : http://{{.ListenAddr}}:{{.Port}}/（仅本机可访问）
计划任务   : {{.TaskName}}

本包部署的是管理平台自身
------------------------
它只读调用被管理 frps 的 Dashboard API 来展示客户端、代理与流量，不参与 FRP
数据转发。因此停掉管理台不会影响任何已建立的隧道：frps.exe / frpc.exe 仍会继续工作。

它读取的 Dashboard 地址是 {{.NodeAPIURL}}，建议与 frps 部署在同一台机器上。

本包不含任何运行时依赖，解压即用：前端资源与 FRP 二进制都已编译进
sinyefrp.exe 本体，不需要 Node、Go、Python。

包内文件
--------
{{.FileList}}
SHA256SUMS.txt   各文件的 SHA256 校验值
README.txt       本说明文件

安装
----
1. 把整个目录复制到目标机器（路径可以包含空格），例如：
       C:\SinyeFRP\
   这个目录会长期存放数据库与日志，请放在不会被随意清理、磁盘空间充足的位置。

2. 【推荐】双击包内的「{{.LauncherName}}」。
   它会自动请求管理员权限（弹出「用户账户控制」时点「是」），随后完成下面第 3 步
   的全部动作；装完窗口会停住，看到「安装完成」再按回车关闭即可。

   这条路不需要你打开 PowerShell、不需要知道 .\ 前缀、也不受脚本执行策略影响。

   也可以沿用命令行方式（效果完全一样：入口调用的就是同一个脚本）：
   以管理员身份打开 PowerShell，进入该目录后执行 .\install.ps1。

3. 安装会依次完成：校验文件哈希 → 创建 data\ 与 logs\ 并收紧其访问权限 →
   注册「SYSTEM 身份、系统启动时运行」的计划任务 → 立即启动并确认端口已监听。

   如果你按目标环境改过 config.toml（改端口、改节点名都属于正常操作），
   安装会提示「与出厂版本不一致」，但会继续安装，不需要任何额外开关。
   只有主程序 sinyefrp.exe 与出厂版本不一致时，安装才会中止 —— 那说明包本身
   已不可信，应当重新解压一份完整包，而不是绕过校验。

4. 确认服务状态：
       .\status.ps1

5. 在浏览器打开管理台。首次使用会进入初始化向导：
       http://{{.ListenAddr}}:{{.Port}}/

关于数据目录与权限
------------------
安装脚本会把 data\ 与 logs\ 的访问权限收紧到 Administrators 与 SYSTEM，这是必要的：
节点密码与接入点 token 用 Windows DPAPI 的「机器范围」加密（换成用户范围会导致
「自启动能跑、管理员手工跑报错」这类难查问题），代价是本机上任何账户都能解开它，
只能靠 NTFS 权限挡住非管理员。

请不要把 data\ 开放给普通用户，也不要把它放在共享盘上。

迁移已有数据
------------
如果这台机器上已经跑过一版管理台，请在安装前把旧目录下的 data\ 整个复制到本目录。
否则新实例会从一个空数据库开始，需要重新走一遍初始化向导，历史记录也不会出现。
install.ps1 不会删除 data\ 里已有的文件，覆盖安装是安全的。

备份时注意：数据库处于 WAL 模式，sinyefrp.db、sinyefrp.db-wal、sinyefrp.db-shm
三个文件要一起复制，只拷主库文件可能丢掉最近的写入。

卸载
----
    .\uninstall.ps1

默认只移除开机自启并停止进程，config.toml、数据库与日志都会保留，便于事后排查或重装。
如需连数据一并删除，请使用：
    .\uninstall.ps1 -RemoveData

（-RemoveData 会删除 data\ 与 logs\。数据库里保存着全部采集记录与加密凭据，
  执行前请先备份。若实例仍在运行，这一步会被拒绝并提示你先执行 .\stop.ps1 ——
  这是刻意的：Windows 允许删除仍被进程打开的文件，删掉之后平台会继续正常跑，
  直到下次重启才新建一个空库，那时才发现记录已经没了。）

停止与重启
----------
    .\stop.ps1       停止实例（会等待数据落盘后再结束进程）
    .\restart.ps1    重启实例

这两个脚本会先请求「优雅退出」：平台监听到部署目录下的 data\sinyefrp.stop 文件后，
会走与正常关闭完全一致的流程，把预写日志（WAL）里的最新写入回写到主库，然后退出。

为什么不能直接强制结束进程：本平台的数据库是 SQLite + WAL 模式，新提交的数据
先落在 sinyefrp.db-wal 里，只有正常退出时才回写主库。强行结束进程（或直接按电源键
重启）会让这些尚未回写的数据失去唯一副本；此时若再遇到掉电，记录就会永久消失，
而登录账号仍在（它在更早的回写里），表现为「能登录，但最近的记录不见了」。

因此请注意：
  - 停实例请用 .\stop.ps1 或 .\restart.ps1，不要用任务管理器「结束任务」；
  - 停实例后不要立刻断电，脚本会自己等待；等待超时它会明确告警；
  - 备份数据库时，sinyefrp.db、sinyefrp.db-wal、sinyefrp.db-shm 三个文件要一起复制。

想确认当前有没有尚未回写的数据：在管理台的「设置」页最下方「数据存储」一块，
会显示数据库文件的绝对路径、主库大小与 WAL 大小。WAL 明显偏大时那里会给出提示。

命令行
------
同一个 exe 也带命令行能力，可用于无界面场景或发布流水线：
    sinyefrp.exe -version                    打印版本
    sinyefrp.exe pkg app -o 输出路径.zip      生成一份平台自身部署包
    sinyefrp.exe -h                          查看全部参数

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
A: 这是 PowerShell 执行策略限制，不是包的问题。以管理员身份执行下面这条命令后
   重试（只对当前窗口生效，不会修改你机器上的长期设置）：
       Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
   （双击「{{.LauncherName}}」不会遇到这个问题：入口自带 -ExecutionPolicy Bypass，
     无需你手工改任何设置。）

Q: 安装完成但浏览器打不开管理台
A: 先执行 .\status.ps1，它除了进程状态之外还会报告 HTTP 端口是否已监听：
   - 端口未监听：查看本目录 logs\sinyefrp.log，通常是端口被占用或配置有误；
   - 端口已监听但仍打不开：确认地址里的端口与 config.toml 的 server.port 一致，
     并确认用的是 127.0.0.1 而不是本机公网 IP —— 管理台只监听环回，这是刻意设计。

Q: 想从别的机器访问管理台
A: 不要把 config.toml 的 listen_addr 改成 0.0.0.0。这台机器可能带有公网 IP，
   那会把管理台直接暴露到公网。请使用 SSH 隧道、内网反向代理，或让管理台自己
   作为被代理对象经 FRP 隧道访问。

Q: 开机没有自动启动
A: 任务以 SYSTEM 身份在「系统启动时」运行，不依赖用户登录。执行 .\status.ps1，
   若任务状态异常请重新执行 .\install.ps1（可重复执行，只会更新原任务）。

Q: 日志与数据库在哪里
A: 日志 logs\sinyefrp.log，数据库 data\sinyefrp.db。
   要确认平台此刻正在读写哪个数据库文件，看管理台「设置」页最下方「数据存储」一块：
   那里直接给出绝对路径与文件大小。配置里写的是相对路径，它按 config.toml 所在目录
   解析（不是启动时的工作目录），搬动整个部署目录不会错位。

Q: 重启机器之后，之前签发的部署包记录不见了
A: 先按上一条确认「数据存储」里的数据库路径，确认看的是同一个库。
   若确实同库，则多半是强制结束或直接断电导致的：数据还在预写日志（WAL）里没回写主库。
   请改用 .\stop.ps1 / .\restart.ps1 停实例，它们会等待数据落盘。
   已经丢失的记录无法自动恢复，但可以补救：管理台「客户端台账」页右上角有
   「手工补录台账」按钮，用于设备仍在运行、只是台账记录丢失的情况。

退出码
------
{{.ExitCodes}}
`

// platformReadmeData 是平台 README 的填充数据。
type platformReadmeData struct {
	Version      string
	FRPVersion   string
	Now          string
	TaskName     string
	ListenAddr   string
	Port         int
	NodeAPIURL   string
	FileList     string
	ExitCodes    string
	LauncherName string
}

// renderPlatformReadme 生成 README.txt 内容（UTF-8 with BOM）。
func renderPlatformReadme(d platformReadmeData) ([]byte, error) {
	if d.Now == "" {
		d.Now = time.Now().Format("2006-01-02 15:04:05")
	}
	if d.LauncherName == "" {
		d.LauncherName = launcherName
	}
	if d.TaskName == "" {
		d.TaskName = platformTaskName
	}
	if d.ExitCodes == "" {
		var b strings.Builder
		for i, c := range ScriptExitCodes {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%-4d %s", c.Code, c.Mean)
		}
		d.ExitCodes = b.String()
	}

	t, err := template.New("platform-readme").Parse(platformReadmeTpl)
	if err != nil {
		return nil, fmt.Errorf("解析平台 README 模板失败: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("渲染平台 README 失败: %w", err)
	}
	return withBOM(buf.Bytes()), nil
}

// BuildPlatformPackage 生成管理平台自身部署包（01-prd F18）。
//
// 与另外两个包的关键差异：这里没有 FRP 配置可校验，所以不调用 verify。
// 平台自身的配置权威是 internal/config 的 Validate，已在 spec.Validate 中执行过，
// 而它启动时还会再校一遍 —— 不存在「生成了一份跑不起来的配置却没人知道」的空档。
func BuildPlatformPackage(self *Self, spec *PlatformSpec) (*Package, error) {
	// 先判自身二进制，再判参数：读不到自己时无论参数多正确都产不出包，
	// 而这条错误更需要被优先看到（它意味着交付流程本身有问题）。
	if !self.Available() {
		return nil, fmt.Errorf("%w：%s", ErrSelfMissing, self.Reason())
	}

	spec.Normalize()
	if errs := spec.Validate(); len(errs) > 0 {
		return nil, joinErrs(errs)
	}

	confContent, err := RenderPlatformConfig(spec)
	if err != nil {
		return nil, err
	}

	scripts, err := RenderScripts(scriptVars{
		Role:        "管理平台",
		ExeName:     appExeName,
		ConfName:    "config.toml",
		TaskName:    platformTaskName,
		DisplayName: "SinyeFRP 管理平台",
		Version:     spec.Version,
		// frp 的配置参数是 -c，平台的是 -config：这是两个程序各自的契约，
		// 不能为了脚本复用去统一。
		ConfFlag:    "-config",
		HasWebUI:    true,
		ServicePort: spec.Port,
		// -RemoveData 要删的是数据库与日志，而不是 frp 的 .toml 之外的东西。
		DataPaths: []string{"data", "logs"},
	})
	if err != nil {
		return nil, err
	}

	// 平台自身包固定 Windows：它的自启动依赖计划任务与 NTFS ACL，
	// 凭据加密依赖 DPAPI，三者都没有 Linux 对应物（见 PlatformFileNames 的注释）。
	fileList := buildFileList(PlatformWindowsAMD64, []fileDesc{
		{appExeName, "管理平台主程序（已内嵌前端与 FRP 二进制，无需其它运行时）"},
		{"config.toml", "监听地址、数据库、日志与节点兜底配置"},
	}, scripts)

	frpVersion := spec.FRPVersion
	if strings.TrimSpace(frpVersion) == "" {
		frpVersion = "未知（未探测）"
	}

	readme, err := renderPlatformReadme(platformReadmeData{
		Version:    spec.Version,
		FRPVersion: frpVersion,
		TaskName:   platformTaskName,
		ListenAddr: spec.ListenAddr,
		Port:       spec.Port,
		NodeAPIURL: spec.NodeAPIURL,
		FileList:   fileList,
	})
	if err != nil {
		return nil, err
	}

	files := assemble(PlatformWindowsAMD64,
		[]PkgFile{
			{Name: appExeName, Data: self.data, Exec: true},
			{Name: "config.toml", Data: confContent},
		},
		scripts,
		readme,
	)

	summary := fmt.Sprintf("平台自身包 · 监听 %s:%d · 版本 %s · 主程序 %s",
		spec.ListenAddr, spec.Port, spec.Version, shortHash(self.SHA256()))

	return &Package{
		FileName:  PlatformFileName(spec.Version),
		Kind:      "platform",
		Version:   spec.Version,
		ExeSHA256: self.SHA256(),
		Summary:   summary,
		Files:     files,
	}, nil
}

// PlatformFileName 返回平台自身部署包的下载文件名。
//
// 导出是为了让「预览接口给出的名字」与「真正下载时的名字」来自同一处：
// 两边各拼一次字符串，迟早会出现预览说 A、下载得到 B 的情况。
func PlatformFileName(version string) string {
	return "SinyeFRP-platform-" + safeFileToken(version) + ".zip"
}

// safeFileToken 把版本号收敛成可安全用于文件名的形式。
//
// 刻意不复用 sanitizeForTask：后者把 '.' 也换成 '-'（计划任务名里点号虽允许，
// 但统一成连字符便于人眼比对），用在版本号上会把 0.1.0-dev 变成 0-1-0-dev。
func safeFileToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "unknown"
	}
	return out
}

// shortHash 把 64 位十六进制哈希截断成便于放在摘要里人眼比对的短串。
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
