package pack

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// 生命周期脚本模板（01-prd F13/F14、§8.3）。
//
// 三条硬约束贯穿全部脚本：
//
//  1. 绝不使用 taskkill /im frpc.exe 这类按进程名批量结束的做法。
//     同一台机器上可能并存多个 frp 部署（包括其它业务遗留的实例），
//     按名击杀会直接打断别人的隧道。本平台一律按「可执行文件绝对路径」
//     精确匹配，只操作本部署目录对应的实例。
//
//  2. 幂等。重复执行 install 只会更新既有计划任务，不产生副本。
//
//  3. 生成的 .ps1 一律以 UTF-8 with BOM 落盘。Windows PowerShell 5.1
//     （Windows Server 2019 的默认版本，无 pwsh 7）会把无 BOM 的 UTF-8
//     按 ANSI 解析，脚本里的中文注释会变成乱码甚至引发语法错误。
//     这是本项目在测试阶段实际踩到的坑，必须在生成端解决。
//
//  4. 局部变量不得命名为 $task（或 $exe / $conf 等与脚本头部常量同名的标识）。
//     PowerShell 变量名不区分大小写，$task 会直接覆盖 $Task（计划任务名），
//     曾导致 status.ps1 把整个 ScheduledTask 对象当成任务名输出、
//     stop/start 传给 Stop-ScheduledTask 的也是对象而非名字。
//     静态检查完全看不出这类问题，只有把包真的装一遍才会暴露。
//
//  5. 包内文本的行尾一律 CRLF，但 BOM 必须按类型分开判定（normalizeText）：
//     .ps1 / .txt 必须有 BOM —— 第 3 条那个原因；
//     .toml 绝对不能有 BOM —— FRP 0.69.0 的 TOML 解析器遇到 BOM 直接报
//     「toml: invalid character at start of key」，退出码 1，frpc/frps 于是
//     完全起不来。这条是真跑出来的（.workbuddy/test/verify-toml-variants.ps1
//     按「行尾 × BOM」四象限各跑一次 frpc verify -c，只有带 BOM 的两种失败）。
//     行尾统一成 CRLF 则是为了记事本：Windows Server 2019 自带的老版记事本
//     在另存时会把裸 LF 改写成 CRLF，运维「打开看了一眼、顺手保存」就足以让
//     config.toml 哈希变化并触发安装脚本的校验提示。
//
// 脚本结构固定为：文件头注释 → param 块（可选）→ 变量 → 函数 → 主体。
// param 必须是第一条可执行语句，因此它必须排在变量赋值之前——
// 注释与空行不算可执行语句，可以出现在 param 之前。

// ScriptExitCodes 是全部脚本共用的退出码约定，同时用于生成 README。
var ScriptExitCodes = []struct {
	Code int
	Mean string
}{
	{0, "成功；对 status.ps1 而言表示实例正在运行"},
	{1, "一般性失败；对 status.ps1 而言表示实例未运行"},
	{2, "尚未安装（仅 status.ps1 / start.ps1）"},
	{10, "缺少管理员权限（含用户取消了提权确认）"},
	{11, "部署目录缺少必需文件（二进制或配置）"},
	{12, "文件哈希校验失败，包可能已损坏或被篡改"},
	{13, "计划任务创建、更新或删除失败"},
}

// scriptVars 是脚本模板的填充数据。
type scriptVars struct {
	Role        string // 「客户端」/「服务器」/「管理平台」，用于提示文案
	ExeName     string // frpc.exe / frps.exe / sinyefrp.exe
	ConfName    string // frpc.toml / frps.toml / config.toml
	TaskName    string // 服务名：Windows 是计划任务名，Linux 是 systemd 服务名，需唯一且稳定
	DisplayName string
	Version     string

	// ServiceKind 是「开机自启」在目标平台上的载体名称，用于提示文案。
	//
	// Windows 是「计划任务」，Linux 是「systemd 服务」。同一个概念在两类系统
	// 上有两个名字，文案里说错一个，运维就会去找一个不存在的东西
	// （例如对着 Linux 机器找「计划任务」）。由调用方按目标平台填入。
	ServiceKind string

	// ConfFlag 是传给主程序的配置参数名：frp 用 -c，管理平台用 -config。
	//
	// 刻意参数化而不是统一成一种：这是两个程序各自既有的命令行契约，
	// 让 frp 接受 -config、或让平台去兼容 -c，都只是为了脚本复用而改动产品行为。
	ConfFlag string

	// HasWebUI 标记「管理平台自身部署包」（01-prd F18）。
	//
	// 平台与 frp 的生命周期动作完全一致（注册 SYSTEM 计划任务、按路径认领进程、
	// 查状态），真正的差异只有三处：配置参数名、需要预建 data 与 logs 并收紧 ACL、
	// 需要探测 HTTP 端口才算「服务可用」。用参数表达这三处差异，
	// 好过为两个角色各维护一套脚本 —— 后者意味着修一处 bug 要改两个地方，
	// 而这两套脚本恰恰是「改漏一处只有装一遍才会发现」的那种代码。
	HasWebUI bool

	// ServicePort 是生成时 config.toml 里的监听端口。
	//
	// 它只是**兜底值**：脚本在运行期会重新从 config.toml 解析生效端口，
	// 只有配置文件缺失或格式异常时才退回这个值。
	ServicePort int

	// DataPaths 是 uninstall -RemoveData 时额外要清理的相对路径。
	// 各角色的运行数据文件名不同，写死会让平台包漏删数据库。
	DataPaths []string

	// Steps 是 install.ps1 的步骤编号，由 RenderScripts 依 HasWebUI 算好。
	//
	// 平台包比 frp 包多一步「准备数据目录」。若在模板里写死 [2/4]，
	// 平台包会打印出与实际步数不符的编号，运维对着日志数步骤时会懵。
	Steps stepNums

	// LauncherName 是包内双击入口的文件名，供脚本模板里的提示文案引用。
	//
	// 由 RenderScripts 统一填写，调用方一概不用管：包内入口名一旦要改，
	// README、脚本提示、ZIP 条目必须同时跟着变，只有一处赋值才不会漏。
	LauncherName string
}

// stepNums 是 install.ps1 的步骤编号。
//
// DataDir 在 frp 包中为 0，模板据 HasWebUI 决定是否渲染该行。
type stepNums struct {
	Hash    int
	DataDir int
	Task    int
	Stop    int
	Start   int
	Total   int
}

// computeSteps 依角色算出 install.ps1 的步骤编号。
func computeSteps(hasWebUI bool) stepNums {
	if hasWebUI {
		return stepNums{Hash: 1, DataDir: 2, Task: 3, Stop: 4, Start: 5, Total: 5}
	}
	return stepNums{Hash: 1, Task: 2, Stop: 3, Start: 4, Total: 4}
}

// headerTpl 是每个脚本共用的文件头，末尾预留 param 块位置。
const headerTpl = `# SinyeFRP 生命周期脚本 — {{.Vars.Role}}（{{.Action}}）
# {{if .Vars.HasWebUI}}生成器: SinyeFRP 管理平台自身    版本: {{.Vars.Version}}{{else}}生成器: SinyeFRP 管理平台    目标 FRP 版本: {{.Vars.Version}}{{end}}
# 编码: UTF-8 with BOM（Windows PowerShell 5.1 正确识别中文所必需）
#
# 本脚本只操作位于其所在目录的实例，不会影响机器上其它已部署的实例。
# 退出码: 0 成功 / 1 一般失败 / 2 未安装 / 10 缺权限 / 11 缺文件 / 12 哈希不符 / 13 任务失败
{{.Params}}
$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
$Exe  = Join-Path $Root '{{.Vars.ExeName}}'
$Conf = Join-Path $Root '{{.Vars.ConfName}}'
$Task = '{{.Vars.TaskName}}'
`

// findProcTpl 是「精确查找本目录实例」的共用实现。
const findProcTpl = `
# 返回本部署目录对应的进程列表。
# 先按可执行文件绝对路径匹配；个别系统上 SYSTEM 进程取不到 ExecutablePath，
# 此时退回命令行匹配，并要求命令行里同时出现本目录的 exe 与配置文件，
# 以免误伤恰好同名的其他实例。
function Get-InstanceProcess {
    $hits = @()
    foreach ($p in @(Get-CimInstance Win32_Process -Filter "Name='{{.Vars.ExeName}}'" -ErrorAction SilentlyContinue)) {
        if ($p.ExecutablePath -and $p.ExecutablePath.Equals($Exe, [StringComparison]::OrdinalIgnoreCase)) {
            $hits += $p
            continue
        }
        if (-not $p.ExecutablePath -and $p.CommandLine) {
            $cmd = $p.CommandLine
            $a = $cmd.IndexOf($Exe, [StringComparison]::OrdinalIgnoreCase) -ge 0
            $b = $cmd.IndexOf($Conf, [StringComparison]::OrdinalIgnoreCase) -ge 0
            if ($a -and $b) { $hits += $p }
        }
    }
    return $hits
}

function Test-Admin {
    $pr = New-Object Security.Principal.WindowsPrincipal(
        [Security.Principal.WindowsIdentity]::GetCurrent())
    return $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# 优雅停止本目录实例，返回本次被停止的进程数。
#
# 为什么不能直接用 Stop-Process -Force：平台的数据库是 SQLite + WAL 模式，
# 新提交的数据先进 .db-wal，要等进程正常退出（defer db.Close()）时才 checkpoint
# 回主库。Stop-Process -Force 是操作系统级强杀，进程收不到任何信号、没有机会
# 执行清理，WAL 就一直胖着不回写；此时若碰上硬重启（掉电、按电源键、reset），
# 未落盘的提交会直接蒸发，表现为「重启之后新增的记录不见了」。
#
# 为什么不用 taskkill /PID（不带 /F）或 Stop-Process（不带 -Force）：
# 这两个办法在**本平台的实际部署形态下都是无效的**，实测结论如下
# （.workbuddy/test/sig-check.ps1，2026-09-24 实测记录）：
#
#   前提：进程由计划任务以 SYSTEM 身份、隐藏窗口方式拉起，没有控制台、没有主窗口。
#   手段 A  taskkill /PID（无 /F）      → 进程纹丝不动，WAL 大小完全不变。
#   手段 B  Stop-Process（无 -Force）   → 进程退出了，但 WAL 同样一个字没少 ——
#                                        那只是 CloseMainWindow 对无窗口进程
#                                        退化成硬终止，并没有走正常关闭路径。
#
# 所以「先请求关闭、再等它自己退」这条路在脚本层根本走不通，硬等 20 秒
# 只会白白拖长停机时间，最后仍落回强杀。真正可靠的做法是让**进程自己**
# 提供入口：平台监听数据目录下的 sinyefrp.stop 文件，脚本创建该文件即可
# 触发与 Ctrl+C 完全相同的关闭路径（srv.Shutdown → store.Close → WAL checkpoint）。
#
# 因此顺序是：写信号文件 → 等它自己退（给 SQLite 留出 checkpoint 时间）
# → 超时才降级为强杀并明确告警。强杀永远是最后手段，不能是默认手段。
#
# 注意：信号文件只对**管理平台**有意义（它才有 SQLite + WAL）。
# frp 客户端/服务端没有数据库，写不写文件都不影响，等到超时再强杀即可 ——
# 那条路径与改动前的行为一致，因此本函数可安全复用于两类包。
function Stop-InstanceGracefully {
    param(
        [object[]]$Processes,
        [int]$TimeoutSeconds = 20
    )
    if (-not $Processes -or $Processes.Count -eq 0) { return 0 }

    $ids = @($Processes | ForEach-Object { $_.ProcessId })

    # 第一轮：创建关停信号文件，请求目标实例优雅退出。
    # 路径固定在部署根下的 data\ —— 与平台的数据目录解析基准一致
    # （config.toml 里 database.path 默认为 "data/sinyefrp.db"）。
    # 不用 $dataDir 那个局部变量：它只在 install 主体里赋值，本函数调用点更早。
    #
    # 这段在生成期按 HasWebUI 决定是否渲染：仅管理平台包包含它。
    # frp 客户端/服务端的包里 $stopFile 恒为空串，后面「删除残留」一步自动跳过。
{{if .Vars.HasWebUI}}    $stopFile = Join-Path (Join-Path $Root 'data') 'sinyefrp.stop'
    try {
        $stopDir = Split-Path -Parent $stopFile
        if (-not (Test-Path $stopDir)) { New-Item -ItemType Directory -Path $stopDir -Force | Out-Null }
        New-Item -ItemType File -Path $stopFile -Force | Out-Null
    } catch {
        Write-Host ('  [提示] 无法创建关停信号文件 ' + $stopFile + '，将直接转入等待与强制结束流程。')
    }
{{else}}    $stopFile = ''
{{end}}

    # 第二轮：轮询等待，给 SQLite 足够时间完成 WAL checkpoint。
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $exited = @()
    while ((Get-Date) -lt $deadline) {
        $alive = @()
        foreach ($id in $ids) {
            if (Get-Process -Id $id -ErrorAction SilentlyContinue) { $alive += $id }
        }
        if ($alive.Count -eq 0) { $exited = $ids; break }
        Start-Sleep -Milliseconds 500
    }

    # 正常关停时进程会自己消费掉信号文件；这里兜住「进程没起来收信号」
    # 的情况，避免残留文件让下一次启动立刻退出。
    if ($stopFile -and (Test-Path $stopFile)) {
        try { Remove-Item -LiteralPath $stopFile -Force -ErrorAction SilentlyContinue } catch { }
    }

    $killed = @()
    foreach ($id in $ids) {
        $p = Get-Process -Id $id -ErrorAction SilentlyContinue
        if ($p) {
            # 超时仍未退出：降级强杀。这一步会丢 WAL，必须让运维看见。
            Write-Host ('  [警告] PID ' + $id + ' 在 ' + $TimeoutSeconds + ' 秒内未正常退出，已强制结束。') -ForegroundColor Yellow
            Write-Host '         强制结束来不及把 WAL 回写主库；若随后异常掉电，最新写入可能丢失。' -ForegroundColor Yellow
            Stop-Process -Id $id -Force -ErrorAction SilentlyContinue
            $killed += $id
        }
    }
    if ($killed.Count -gt 0) { Start-Sleep -Seconds 2 }
    return $ids.Count
}

# 结束时是否停屏等一次回车。
#
# 默认 $false：运维在命令行调用、监控采集、自动化流水线都走这条路，
# 多停一下都是干扰。只有包内的双击入口（{{.Vars.LauncherName}}）会显式传
# -Pause 把它打开 —— 双击的人看到的是「黑窗口一闪就没了」，
# 装成功还是报错根本来不及看，必须停住。
$script:WaitOnExit = $false

# 停屏。两条必须跳过的分支，缺一条都会咬人：
#   · 没要求停屏 —— 见上；
#   · 标准输入被重定向 —— 此时 Read-Host 等的是一个永远不会到来的回车，
#     整个流水线会挂死在这里。自动化测试即使传了 -Pause 也能安全跑完，
#     靠的就是这一条。
function Wait-Exit {
    if (-not $script:WaitOnExit) { return }
    if ([Console]::IsInputRedirected) { return }
    Write-Host ''
    [void](Read-Host '请按回车键关闭本窗口')
}

function Fail([int]$Code, [string]$Message) {
    Write-Host ('[错误] ' + $Message) -ForegroundColor Red
    Wait-Exit
    exit $Code
}
{{if .Vars.HasWebUI}}
# 从 config.toml 的 [server] 段读出生效的监听端口。
#
# 刻意在运行期读文件，而不是把生成时的端口写死在脚本里：改端口是运维就地调整
# 配置时最常见的一项，若脚本留的是旧值，status.ps1 会一直报「端口未监听」，
# 而服务其实是好的。这种「监控喊坏了、实际是好的」比不监控更糟 ——
# 它会训练运维忽略告警，等到真的坏了就没人看了。
#
# 只做够用的解析：TOML 的 [server] 段里第一个 port = <数字>。
# 注释行以 # 开头，不会匹配到（正则要求 port 是行首第一个非空白记号）。
function Get-ServicePort {
    $fallback = {{.Vars.ServicePort}}
    if (-not (Test-Path $Conf)) { return $fallback }
    $inServer = $false
    foreach ($line in @(Get-Content -LiteralPath $Conf -ErrorAction SilentlyContinue)) {
        if ($line -match '^\s*\[(.+?)\]\s*$') {
            $inServer = ($matches[1].Trim() -eq 'server')
            continue
        }
        if ($inServer -and $line -match '^\s*port\s*=\s*(\d+)') {
            return [int]$matches[1]
        }
    }
    return $fallback
}

# 从 config.toml 的 [server] 段读出生效的监听地址。
#
# 同样在运行期读，理由与端口一致：运维可能把 listen_addr 改成 0.0.0.0
# （公网直接访问管理台）。脚本若不跟着变，打印出来的访问地址会一直是
# 127.0.0.1 —— 那是个只有本机能用的地址，照着它去浏览器里试必然失败。
function Get-ServiceAddr {
    if (-not (Test-Path $Conf)) { return '127.0.0.1' }
    $inServer = $false
    foreach ($line in @(Get-Content -LiteralPath $Conf -ErrorAction SilentlyContinue)) {
        if ($line -match '^\s*\[(.+?)\]\s*$') {
            $inServer = ($matches[1].Trim() -eq 'server')
            continue
        }
        if ($inServer -and $line -match '^\s*listen_addr\s*=\s*"([^"]*)"') {
            $addr = $matches[1].Trim()
            if ($addr) { return $addr }
        }
    }
    return '127.0.0.1'
}

# 把监听地址翻译成一个「能在浏览器里打开的地址」。
#
# 0.0.0.0 / :: 是绑定用的通配地址，不是能连的地址，直接拼进 URL 必然打不开。
# 这种情况改用本机第一个 IPv4 地址，同局域网的机器可以直接点开。
function Get-AccessURL {
    $addr = Get-ServiceAddr
    if (-not ($addr -eq '0.0.0.0' -or $addr -eq '::' -or $addr -eq '*')) {
        return ('http://' + $addr + ':' + $Port + '/')
    }
    $ip = ''
    try {
        $found = @([System.Net.Dns]::GetHostAddresses([System.Net.Dns]::GetHostName()) |
            Where-Object { $_.AddressFamily -eq 'InterNetwork' })
        if ($found.Count -gt 0) { $ip = $found[0].IPAddressToString }
    } catch {
        $ip = ''
    }
    if (-not $ip) { $ip = '<本机IP>' }
    return ('http://' + $ip + ':' + $Port + '/')
}

# 监听地址是否已经超出环回（即对网络开放）。
#
# 脚本不阻止这种配置 —— 生产上确实可能配合安全组或来源限制使用；
# 但必须让运维看见代价，所以调用方会据此多打两行提示。
function Test-PublicBind {
    $addr = Get-ServiceAddr
    if ($addr -eq '127.0.0.1' -or $addr -eq 'localhost' -or $addr -eq '::1') { return $false }
    return $true
}

# 探测 HTTP 端口是否已在监听。
#
# 进程活着不等于服务可用：端口没监听说明它还在启动，或者已经崩溃但进程尚未退出。
# 刻意不用 Test-NetConnection —— 本机若把 DNS 解析优先给 IPv6，它会长时间挂住，
# 这正是「npm install 无输出且不报错」的同一个坑。直接对 127.0.0.1 发起 TCP 连接
# 并自带超时，结果确定、耗时可预期。
function Test-PortListening {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne(1500, $false)) { return $false }
        $client.EndConnect($iar)
        return $true
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

# 端口在函数定义之后解析：这样脚本里所有引用 $Port 的地方（消息文案与探活）
# 用的都是当前 config.toml 的值，而不是生成时的快照。
$Port = Get-ServicePort
{{end}}`

// installBodyTpl 实现安装：校验 → 数据目录 → 创建/更新计划任务 → 启动。
//
// 步骤编号取自 Vars.Steps 而不是写死：管理平台包比 frp 包多一步「准备数据目录」，
// 写死 [2/4] 会让平台包打印出与实际步数不符的编号，运维对着日志数步骤时会懵。
const installBodyTpl = `
Write-Host '=== SinyeFRP {{.Vars.Role}}安装 ===' -ForegroundColor Cyan
Write-Host ('部署目录: ' + $Root)

# 停屏开关由双击入口传 -Pause 打开；Fail 与成功收尾都经过 Wait-Exit。
$script:WaitOnExit = [bool]$Pause

# 管理员权限 —— 创建 SYSTEM 计划任务必须提权。
#
# 非管理员时分两档，这个分档是刻意的：
#   · 走双击入口（-Elevate）：自动弹出 UAC，以管理员身份把本脚本重跑一遍。
#     现场人员只要会双击就行，不需要知道「以管理员身份运行」这回事。
#   · 其它一切调用方式：维持原样，直接以退出码 10 收场。运维脚本、自动化
#     流水线、监控采集都依赖这个契约，不能为了照顾双击路径让他们一起变成
#     弹框等待（那会把无人值守的安装直接挂死）。
#
# -Elevated 是内部标记，只由提权后的新进程带上。它的作用是保证「已经提过一次」：
# 万一提权后权限仍未生效（受限令牌、策略拦截等边缘情形），第二次也不会再弹一轮
# UAC，否则会变成无限弹框，用户只能强杀进程。
if (-not (Test-Admin)) {
    if ($Elevate -and -not $Elevated) {
        Write-Host '正在请求管理员权限，请在弹出窗口中确认……' -ForegroundColor Cyan
        $argLine = '-NoProfile -ExecutionPolicy Bypass -File "' + $PSCommandPath + '" -Elevate -Elevated'
        if ($Pause) { $argLine = $argLine + ' -Pause' }
        try {
            Start-Process -FilePath 'powershell.exe' -Verb RunAs -ArgumentList $argLine -ErrorAction Stop
        } catch {
            Fail 10 ('未能取得管理员权限，安装未执行。原因: ' + $_.Exception.Message + '。请确认你在这台机器上有管理员权限，或右键点击{{.Vars.LauncherName}}选择「以管理员身份运行」。')
        }
        # 提权后的新窗口会独立完成安装并停屏（-Pause 已一并传下去）。
        # 本进程立刻退出：否则会同时留下两个窗口，用户不知道该看哪个。
        exit 0
    }
    Fail 10 '需要管理员权限。请双击包内的「{{.Vars.LauncherName}}」，它会自动请求提权；或右键点击 PowerShell 选择「以管理员身份运行」后重试。'
}

# 必需文件
foreach ($f in @($Exe, $Conf)) {
    if (-not (Test-Path $f)) { Fail 11 ('缺少必需文件: ' + $f) }
}

# 哈希校验 —— 确认包未被损坏或篡改。
#
# 判定刻意分两级、不对称：
#   · 主程序不一致 = 致命。它是这个包的代码，出厂后不该被人动过，
#     一旦变了就说明包不可信。
#   · 配置文件不一致 = 只警告。监听端口、节点名、Dashboard 地址本来就
#     要按目标环境改，改完再装是标准流程。若把它也判死，运维唯一的出路
#     就是加 -SkipHashCheck —— 而那会把主程序的校验一起关掉。为了一处
#     正常的配置改动而放弃对代码的校验，是笔亏本买卖。
$sumFile = Join-Path $Root 'SHA256SUMS.txt'
if ((Test-Path $sumFile) -and (-not $SkipHashCheck)) {
    $expected = @{}
    # 必须显式 -Encoding UTF8：SHA256SUMS.txt 是**无 BOM** 的（它是机器读的清单，
    # 加 BOM 会让 sha256sum -c 之类的标准工具读不了）。而 PS 5.1 的 Get-Content
    # 在无 BOM 时按 ANSI 代码页解码 —— 包内现在有一个中文文件名
    # （{{.Vars.LauncherName}}），那一行会变成乱码。
    #
    # 乱码本身不会让安装失败：下面只查 exe / 配置这两个 ASCII 名字。
    # 但那是**巧合**，不是设计 —— 哪天主程序换了非 ASCII 名字，这个字典里就找不到
    # 对应键，哈希校验会静默跳过（正是它该拦住的那种情况）。所以这里写死编码。
    foreach ($line in (Get-Content -LiteralPath $sumFile -Encoding UTF8)) {
        if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$') {
            $expected[$matches[2]] = $matches[1].ToLowerInvariant()
        }
    }
    if ($expected.ContainsKey('{{.Vars.ExeName}}')) {
        $actual = (Get-FileHash (Join-Path $Root '{{.Vars.ExeName}}') -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected['{{.Vars.ExeName}}']) {
            Fail 12 ('{{.Vars.ExeName}} 校验失败: 主程序与出厂版本不一致，包不可信。请重新解压一份完整的部署包，不要在这个目录里继续安装。')
        }
    }
    Write-Host '[{{.Vars.Steps.Hash}}/{{.Vars.Steps.Total}}] 主程序哈希校验通过' -ForegroundColor Green
    if ($expected.ContainsKey('{{.Vars.ConfName}}')) {
        $actual = (Get-FileHash (Join-Path $Root '{{.Vars.ConfName}}') -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected['{{.Vars.ConfName}}']) {
            Write-Host ('[提示] {{.Vars.ConfName}} 与出厂版本不一致（按目标环境改过配置属正常），不阻断安装。') -ForegroundColor Yellow
            Write-Host '       若不是你改的，请先核对内容再决定是否继续。' -ForegroundColor Yellow
        }
    }
} else {
    Write-Host '[{{.Vars.Steps.Hash}}/{{.Vars.Steps.Total}}] 已跳过哈希校验（主程序与配置都未校验）' -ForegroundColor Yellow
}
{{if .Vars.HasWebUI}}
# 运行目录与访问权限
#
# 平台的数据库与日志都落在部署目录下：config.toml 里的相对路径以
# 「配置文件所在目录」为基准解析，与进程工作目录无关，所以计划任务以 SYSTEM
# 身份拉起时同样正确（若按工作目录解析，库会被写进 C:\Windows\System32）。
#
# 这里必须收紧 NTFS 权限：节点密码与接入点 token 用 DPAPI 的**机器范围**加密
# （选用户范围会导致「自启动能跑、管理员手工跑报错」），代价是本机任何账户都能
# 解开它，只能靠目录权限挡住非管理员（01-prd §8.5）。
$dataDir = Join-Path $Root 'data'
$logDir  = Join-Path $Root 'logs'
foreach ($d in @($dataDir, $logDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
}
try {
    foreach ($d in @($dataDir, $logDir)) {
        # /inheritance:r 断开继承（去掉 Users 与 Everyone），
        # 再显式授予 Administrators(S-1-5-32-544) 与 SYSTEM(S-1-5-18)。
        # 用 SID 而不是账户名：账户名随系统语言变化，SID 不会。
        & icacls $d /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' /grant:r '*S-1-5-18:(OI)(CI)F' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw ('icacls 返回退出码 ' + $LASTEXITCODE) }
    }
    Write-Host '[{{.Vars.Steps.DataDir}}/{{.Vars.Steps.Total}}] 数据与日志目录已就绪，访问权限已收紧' -ForegroundColor Green
} catch {
    Write-Host ('[{{.Vars.Steps.DataDir}}/{{.Vars.Steps.Total}}] 目录已创建，但收紧权限失败: ' + $_.Exception.Message) -ForegroundColor Yellow
    Write-Host '      数据库内含加密凭据，建议手工确认 data\ 未对普通用户开放。' -ForegroundColor Yellow
}
{{end}}
# 创建或更新计划任务
#    -AtStartup + SYSTEM：开机即起，无需用户登录
#    -Force：同名任务直接覆盖，保证重复安装不会产生副本
#    RestartCount/RestartInterval：进程异常退出时由任务计划自动拉起
try {
    $action = New-ScheduledTaskAction -Execute $Exe -Argument ('{{.Vars.ConfFlag}} "' + $Conf + '"') -WorkingDirectory $Root
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)

    Register-ScheduledTask -TaskName $Task -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
    Write-Host ('[{{.Vars.Steps.Task}}/{{.Vars.Steps.Total}}] 计划任务已创建或更新: ' + $Task) -ForegroundColor Green
} catch {
    Fail 13 ('创建计划任务失败: ' + $_.Exception.Message)
}

# 若已有旧进程在跑，先停掉，避免同目录出现两个实例。
# 走优雅停止：旧进程的 WAL 里可能还有没回写主库的数据（比如刚生成的部署包记录），
# 强杀会让这些写入悬在 WAL 中，一旦随后异常掉电就再也找不回来。
$running = @(Get-InstanceProcess)
if ($running.Count -gt 0) {
    Stop-InstanceGracefully -Processes $running | Out-Null
    Write-Host ('[{{.Vars.Steps.Stop}}/{{.Vars.Steps.Total}}] 已停止 ' + $running.Count + ' 个旧进程') -ForegroundColor Yellow
} else {
    Write-Host '[{{.Vars.Steps.Stop}}/{{.Vars.Steps.Total}}] 无旧进程需要停止' -ForegroundColor Green
}

# 立即启动
try {
    Start-ScheduledTask -TaskName $Task
    Start-Sleep -Seconds 3
    Write-Host '[{{.Vars.Steps.Start}}/{{.Vars.Steps.Total}}] 任务已启动' -ForegroundColor Green
} catch {
    Fail 13 ('启动任务失败: ' + $_.Exception.Message)
}

$now = @(Get-InstanceProcess)
if ($now.Count -gt 0) {
    Write-Host ''
    Write-Host ('安装完成，进程 PID: ' + (($now | ForEach-Object { $_.ProcessId }) -join ', ')) -ForegroundColor Green
    Write-Host ('配置文件: ' + $Conf)
{{if .Vars.HasWebUI}}
    # 进程起来不等于服务可用，还要确认 HTTP 端口已监听。
    # 首次启动要建库并展开内嵌资源，稍慢属正常，因此重试到 10 秒再判定。
    $listening = $false
    for ($i = 0; $i -lt 10; $i++) {
        if (Test-PortListening) { $listening = $true; break }
        Start-Sleep -Seconds 1
    }
    if ($listening) {
        Write-Host ('管理台地址: ' + (Get-AccessURL)) -ForegroundColor Green
        if (Test-PublicBind) {
            Write-Host ('[注意] config.toml 的 listen_addr 是 ' + (Get-ServiceAddr) + '，管理台正在对网络监听。') -ForegroundColor Yellow
            Write-Host '       本界面是纯 HTTP，管理员密码与会话 Cookie 会明文经过网络。' -ForegroundColor Yellow
            Write-Host '       请确认防火墙入站规则的来源已限制到固定 IP，或改用隧道/反向代理。' -ForegroundColor Yellow
        }
        Write-Host '若这是全新部署，请在浏览器打开该地址完成初始化向导。'
    } else {
        Write-Host ('进程已启动，但 ' + $Port + ' 端口尚未监听。') -ForegroundColor Yellow
        Write-Host '首次启动会创建数据库；请稍候用 .\status.ps1 复查。' -ForegroundColor Yellow
    }
{{end}}    Write-Host '查看运行状态请执行 .\status.ps1'
    Wait-Exit
    exit 0
}

Write-Host ''
Write-Host '任务已注册，但进程尚未出现。请稍候用 .\status.ps1 复查；' -ForegroundColor Yellow
Write-Host '若持续未启动，请检查同目录下的日志文件。' -ForegroundColor Yellow
Wait-Exit
exit 0
`

// uninstallBodyTpl 实现卸载：停进程 → 删任务 → 保留数据。
const uninstallBodyTpl = `
Write-Host '=== SinyeFRP {{.Vars.Role}}卸载 ===' -ForegroundColor Cyan

if (-not (Test-Admin)) {
    Fail 10 '需要管理员权限。请右键点击 PowerShell 选择「以管理员身份运行」后重试。'
}

# 0) 先停进程，再删计划任务 —— 顺序不能反。
#
# 为什么「加 -RemoveData 时必须先停」反而要先停：计划任务一旦被 Unregister，
# 「进程还在跑」这个事实就再也拦不住了。真正的生产方式是这样丢数据的：
# 运维直接双击/调用 uninstall.ps1 -RemoveData（平台正被任务计划拉着跑），
# 此时若先删任务，第 1 步就顺手把唯一的进程依据抹掉了，第 3 步的
# 「仍在运行」检查看到的永远是空列表，于是删 data\ 「成功」——
# 而 Windows 允许删除仍被打开的文件（目录项摘掉、进程照常读写），
# 平台继续跑、脚本报成功，直到下次重启新建空库才暴露数据已丢。
#
# 所以第 0 步就要看清楚：**这次卸载必须由我们自己来收场**。
# 有活进程 → 优雅停；停不掉 → 立刻中止，绝不往下走删数据的分支。
$running = @(Get-InstanceProcess)
if ($running.Count -gt 0) {
    # 优雅停止：让进程把 WAL 回写主库。强杀会让未 checkpoint 的数据悬在
    # .db-wal 里 —— 所以这里不能图省事直接 Stop-Process -Force。
    Stop-InstanceGracefully -Processes $running | Out-Null
    $left = @(Get-InstanceProcess)
    if ($left.Count -gt 0) {
        if ($RemoveData) {
            Fail 1 ('检测到 ' + $left.Count + ' 个进程仍在运行，已中止删除数据。' +
                    '请先执行 .\stop.ps1，确认进程全部退出后再重试；' +
                    '否则会在数据库仍被占用时删除 data\，导致数据静默丢失。')
        }
        Fail 1 ('仍有 ' + $left.Count + ' 个进程未能停止，请手动检查。')
    }
    Write-Host ('[1/3] 已停止 ' + $running.Count + ' 个进程') -ForegroundColor Green
} else {
    Write-Host '[1/3] 无运行中的进程' -ForegroundColor Green
}

# 1) 删除计划任务（不存在也算成功，保证可重复执行）
#    放在停进程之后：任务还在的这段时间里，它是「服务应当开着」的唯一凭据，
#    先留着，等确认进程收干净了再撤掉自启动。
$taskInfo = Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
if ($taskInfo) {
    try {
        Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $Task -Confirm:$false
        Write-Host ('[2/3] 计划任务已删除: ' + $Task) -ForegroundColor Green
    } catch {
        Fail 13 ('删除计划任务失败: ' + $_.Exception.Message)
    }
} else {
    Write-Host '[2/3] 计划任务不存在，跳过' -ForegroundColor Yellow
}

# 3) 数据保留策略：默认只移除自启动，保留配置与日志
if ($RemoveData) {
    # 待清理清单由生成端给出：frp 包是配置文件与日志，
    # 平台包是 data\ 与 logs\。写死会导致平台包漏删数据库。
    foreach ($name in @('{{.Vars.ConfName}}', '{{.Vars.ExeName}}'{{range .Vars.DataPaths}}, '{{.}}'{{end}})) {
        $p = Join-Path $Root $name
        if (Test-Path $p) { Remove-Item $p -Recurse -Force -ErrorAction SilentlyContinue }
    }
    Write-Host '[3/3] 已按 -RemoveData 删除配置与日志' -ForegroundColor Yellow
} else {
    Write-Host '[3/3] 已保留配置与日志；如需一并删除请加 -RemoveData' -ForegroundColor Green
}

Write-Host ''
Write-Host '卸载完成。' -ForegroundColor Green
exit 0
`

// startBodyTpl 启动实例。
const startBodyTpl = `
Write-Host '=== SinyeFRP {{.Vars.Role}}启动 ===' -ForegroundColor Cyan

if (-not (Test-Admin)) {
    Fail 10 '需要管理员权限。请右键点击 PowerShell 选择「以管理员身份运行」后重试。'
}

$running = @(Get-InstanceProcess)
if ($running.Count -gt 0) {
    Write-Host ('实例已在运行，PID: ' + (($running | ForEach-Object { $_.ProcessId }) -join ', ')) -ForegroundColor Yellow
    exit 0
}

$taskInfo = Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
if (-not $taskInfo) {
    Fail 2 '尚未安装，请先执行 .\install.ps1'
}

try {
    Start-ScheduledTask -TaskName $Task
    Start-Sleep -Seconds 3
} catch {
    Fail 13 ('启动任务失败: ' + $_.Exception.Message)
}

$now = @(Get-InstanceProcess)
if ($now.Count -eq 0) {
    Fail 1 '任务已下发，但进程未出现。请检查同目录日志。'
}
Write-Host ('启动成功，PID: ' + (($now | ForEach-Object { $_.ProcessId }) -join ', ')) -ForegroundColor Green
exit 0
`

// stopBodyTpl 停止实例。
const stopBodyTpl = `
Write-Host '=== SinyeFRP {{.Vars.Role}}停止 ===' -ForegroundColor Cyan

if (-not (Test-Admin)) {
    Fail 10 '需要管理员权限。请右键点击 PowerShell 选择「以管理员身份运行」后重试。'
}

# 先停任务，防止它按「失败重启」策略把进程再拉起来
$taskInfo = Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
if ($taskInfo) { Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue }

$running = @(Get-InstanceProcess)
if ($running.Count -eq 0) {
    Write-Host '实例未在运行' -ForegroundColor Yellow
    exit 0
}

# 优雅停止：等进程自己退出，让它把 WAL 里未回写的数据 checkpoint 进主库。
# 直接 -Force 强杀会让这部分数据悬在 WAL 中，一旦随后硬重启就会丢失。
Stop-InstanceGracefully -Processes $running | Out-Null

$left = @(Get-InstanceProcess)
if ($left.Count -gt 0) {
    Fail 1 ('仍有 ' + $left.Count + ' 个进程未能停止，请手动检查。')
}
Write-Host ('已停止 ' + $running.Count + ' 个进程') -ForegroundColor Green
exit 0
`

// restartBodyTpl 重启实例。
const restartBodyTpl = `
Write-Host '=== SinyeFRP {{.Vars.Role}}重启 ===' -ForegroundColor Cyan

if (-not (Test-Admin)) {
    Fail 10 '需要管理员权限。请右键点击 PowerShell 选择「以管理员身份运行」后重试。'
}

$taskInfo = Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
if (-not $taskInfo) {
    Fail 2 '尚未安装，请先执行 .\install.ps1'
}

Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
$running = @(Get-InstanceProcess)
# 重启同样要走优雅停止：不然每次重启都在制造一个「胖 WAL」，
# 把数据暴露在「掉电即丢」的窗口里。
if ($running.Count -gt 0) { Stop-InstanceGracefully -Processes $running | Out-Null }

try {
    Start-ScheduledTask -TaskName $Task
    Start-Sleep -Seconds 3
} catch {
    Fail 13 ('启动任务失败: ' + $_.Exception.Message)
}

$now = @(Get-InstanceProcess)
if ($now.Count -eq 0) {
    Fail 1 '重启后进程未出现。请检查同目录日志。'
}
Write-Host ('重启完成，PID: ' + (($now | ForEach-Object { $_.ProcessId }) -join ', ')) -ForegroundColor Green
exit 0
`

// statusBodyTpl 查询状态，供运维巡检与监控采集调用。
//
// 输出刻意保持「人可读 + 机器可解析」两用：默认输出键值对便于肉眼查看，
// -Json 输出单行 JSON 便于被监控系统直接采集。
const statusBodyTpl = `
$taskInfo = Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
$taskState = if ($taskInfo) { [string]$taskInfo.State } else { 'NotInstalled' }
$running = @(Get-InstanceProcess)
{{if .Vars.HasWebUI}}
# 端口探活只在进程存在时才做：否则每次巡检都要白等一次 1.5 秒超时。
# 平台包独有 —— 对 frp 而言「进程在跑」就是全部结论，不需要再探端口。
$listening = $false
if ($running.Count -gt 0) { $listening = Test-PortListening }
{{end}}
$pids = @($running | ForEach-Object { [int]$_.ProcessId })
$installed = [bool]$taskInfo
$isRunning = $running.Count -gt 0

if ($Json) {
    [ordered]@{
        role         = '{{.Vars.Role}}'
        displayName  = '{{.Vars.DisplayName}}'
        taskName     = $Task
        installed    = $installed
        taskState    = $taskState
        running      = $isRunning
        processCount = $running.Count
        pids         = $pids
{{if .Vars.HasWebUI}}        portListening = $listening
{{end}}        root         = $Root
        {{if .Vars.HasWebUI}}platformVersion{{else}}frpVersion{{end}} = '{{.Vars.Version}}'
    } | ConvertTo-Json -Compress
} else {
    Write-Host '=== SinyeFRP {{.Vars.Role}}状态 ===' -ForegroundColor Cyan
    Write-Host ('名称     : {{.Vars.DisplayName}}')
    Write-Host ('部署目录 : ' + $Root)
    Write-Host ('{{if .Vars.HasWebUI}}平台版本{{else}}FRP 版本{{end}} : {{.Vars.Version}}')
    Write-Host ('计划任务 : ' + $Task)
    Write-Host ('任务状态 : ' + $taskState)
    if ($isRunning) {
        Write-Host ('运行状态 : 运行中 (PID ' + ($pids -join ', ') + ')') -ForegroundColor Green
    } else {
        Write-Host '运行状态 : 未运行' -ForegroundColor Yellow
    }
{{if .Vars.HasWebUI}}    if ($listening) {
        Write-Host ('管理台   : ' + (Get-AccessURL) + ' 已就绪') -ForegroundColor Green
        if (Test-PublicBind) {
            Write-Host ('           listen_addr = ' + (Get-ServiceAddr) + '，已对网络监听；本界面为纯 HTTP') -ForegroundColor Yellow
        }
    } elseif ($isRunning) {
        Write-Host ('管理台   : ' + $Port + ' 端口未监听 —— 进程在跑但服务不可用') -ForegroundColor Yellow
    }
{{end}}}

if (-not $installed) { exit 2 }
if ($isRunning) { exit 0 }
exit 1
`

// launcherName 是包内「双击安装」入口的文件名。
//
// 刻意用中文名：这份包最终交到现场人员手里，而不是交给运维。他们不会先读
// README 再决定点哪个文件，文件名本身就是唯一的路标。
//
// 中文名在 ZIP 里的编码由 archive/zip 负责：非纯 ASCII 的条目名会自动带上
// UTF-8 标志位（general purpose bit 11），资源管理器与 7-Zip 都能正确显示。
// 个别老解压工具仍可能解出乱码，所以 README 里专门写了一条「文件名乱码怎么办」，
// 兜住这种情况 —— 文件名选中文换来的是辨识度，代价要一并交代给用户。
const launcherName = "一键安装.bat"

// launcherBat 是双击入口的内容。
//
// 三条硬约束，每一条都对应一个会真实炸掉的地方：
//
//  1. **纯 ASCII，一个中文字符都不许有。** cmd.exe 用控制台当前代码页解析
//     .bat（简体中文 Windows 上是 CP936），而 UTF-8 保存的中文在 CP936 下
//     是乱码。用 chcp 65001 让 cmd 换码页能治，但那会让 cmd 按新码页重新
//     读取脚本自身，行为随系统版本漂移。所以：.bat 里只留 ASCII，
//     所有面向人的中文提示都交给它调起的 PowerShell ——
//     .ps1 是 UTF-8 with BOM，中文一定正确，这是本项目已验证的组合。
//
//  2. **绝对不能带 BOM，与 .ps1 恰好相反。** cmd 会把 BOM 当成命令的一部分，
//     首行直接报「'ï»¿@echo' 不是内部或外部命令」，双击什么都不会发生。
//     normalizeText 为此给 .bat 单开一档「去 BOM + CRLF」，并且刻意不把 .bat
//     加进 isTextName —— 那条分支是加 BOM 的。改这里之前先看 normalizeText。
//
//  3. **只做提权与转发，不实现任何生命周期逻辑。** 安装逻辑只有 install.ps1
//     一份实现。入口里再抄一份「创建计划任务」的代码，等于主动把
//     「改漏一处只有真装一遍才会发现」的隐患请进门 —— 而两处脚本正是最容易
//     改漏的地方。
//
// 两个开关的分工：-Elevate 让 install.ps1 在非管理员时弹 UAC 自己重跑；
// -Pause 让装完的窗口停住。没有 -Pause 的话，双击的人看到的只是黑窗口一闪。
const launcherBat = `@echo off
rem SinyeFRP installer entry. Double-click this file to install.
rem All install logic lives in install.ps1; this file only forwards to it.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" -Elevate -Pause
`

// scriptDef 描述一个待渲染的脚本。
//
// Params 为空字符串，或形如 "`nparam(...)`n" 的完整 param 块。
// 由于它是第一条可执行语句，必须紧跟在文件头注释之后。
type scriptDef struct {
	File   string
	Action string
	Params string
	Body   string
}

var scriptDefs = []scriptDef{
	{
		File:   "install.ps1",
		Action: "安装并注册开机自启",
		Params: "\nparam(\n" +
			"    # 跳过 SHA256SUMS.txt 校验。会同时跳过主程序校验，仅在包经人工改动、\n" +
			"    # 确认来源可信且必须继续时使用；改配置不需要它。\n" +
			"    [switch]$SkipHashCheck,\n" +
			"\n" +
			"    # 非管理员时自动弹出 UAC 提权，再以管理员身份重跑本脚本。\n" +
			"    # 包内的双击入口会带上它 —— 现场人员不必知道「以管理员身份运行」这件事。\n" +
			"    # 不传时行为与从前完全一致（直接以退出码 10 收场），无人值守的调用不受影响。\n" +
			"    [switch]$Elevate,\n" +
			"\n" +
			"    # 内部标记，由提权后的新进程带上，用于防止反复弹 UAC。请勿手工使用。\n" +
			"    [switch]$Elevated,\n" +
			"\n" +
			"    # 结束时等一次回车再关窗口，供双击入口使用。\n" +
			"    # 标准输入被重定向时自动跳过，不会挂住自动化调用。\n" +
			"    [switch]$Pause\n" +
			")\n",
		Body: installBodyTpl,
	},
	{
		File:   "uninstall.ps1",
		Action: "卸载并移除开机自启",
		Params: "\nparam(\n    # 同时删除配置、日志与运行数据。默认保留，便于事后排查或重装。\n    [switch]$RemoveData\n)\n",
		Body:   uninstallBodyTpl,
	},
	{File: "start.ps1", Action: "启动", Body: startBodyTpl},
	{File: "stop.ps1", Action: "停止", Body: stopBodyTpl},
	{File: "restart.ps1", Action: "重启", Body: restartBodyTpl},
	{
		File:   "status.ps1",
		Action: "状态查询",
		Params: "\nparam(\n    # 以单行 JSON 输出，便于被监控系统采集。\n    [switch]$Json\n)\n",
		Body:   statusBodyTpl,
	},
}

// tmplData 是渲染脚本时的顶层数据。
type tmplData struct {
	Vars   scriptVars
	Action string
	Params string
}

// renderScripts 按目标平台渲染生命周期脚本，是两套模板的唯一分发点。
//
// 放在这里而不是让三个调用点各自判断：客户端/服务器/平台包都要走这一步，
// 分散判断迟早会有一个漏掉 —— 漏掉的那个不会报错，只会安静地产出
// 一包在目标机器上根本跑不起来的 .ps1（或反过来）。
func renderScripts(p Platform, v scriptVars) (map[string][]byte, error) {
	// 载体名称由平台决定，调用方不必关心：文案里把 Linux 的 systemd 服务
	// 写成「计划任务」，运维就会去找一个不存在的东西。
	v.ServiceKind = p.ServiceKind()
	if p.IsWindows() {
		return RenderScripts(v)
	}
	return RenderLinuxScripts(v)
}

// RenderScripts 生成全部生命周期脚本与双击入口，返回「文件名 → 内容」。
//
// 返回内容已按 UTF-8 with BOM 编码，可直接写入 ZIP —— 唯一的例外是双击入口
// 那个 .bat，它必须**没有** BOM（见 launcherBat 第 2 条）。
func RenderScripts(v scriptVars) (map[string][]byte, error) {
	v.Steps = computeSteps(v.HasWebUI)

	// 入口名在这里统一赋值，而不是让每个调用方（客户端/服务器/平台三处）各传一次：
	// 包内入口名一旦要改，README、脚本提示、ZIP 条目必须同时跟着变。
	// 三个调用点各填一次，迟早会漏掉一个，而漏掉的那个症状是
	// 「脚本让用户双击一个不存在的文件名」—— 不影响任何断言，只会让现场卡住。
	v.LauncherName = launcherName

	out := make(map[string][]byte, len(scriptDefs)+1)
	for _, d := range scriptDefs {
		var buf bytes.Buffer
		t, err := template.New(d.File).Parse(headerTpl + findProcTpl + d.Body)
		if err != nil {
			return nil, fmt.Errorf("解析 %s 模板失败: %w", d.File, err)
		}
		if err := t.Execute(&buf, tmplData{Vars: v, Action: d.Action, Params: d.Params}); err != nil {
			return nil, fmt.Errorf("渲染 %s 失败: %w", d.File, err)
		}
		out[d.File] = withBOM(buf.Bytes())
	}

	// 双击入口刻意不走上面的模板渲染：它是纯静态内容，且**不能带 BOM**。
	// 这里只做行尾规范化 —— 与 .ps1 模板共用 withBOM 的代价是整个入口失效，
	// 而症状只是「双击没反应」，不会报任何错。
	out[launcherName] = withCRLF([]byte(launcherBat))
	return out, nil
}

// withBOM 在内容前加 UTF-8 BOM。
//
// 生成的脚本含中文提示，而 Windows Server 2019 只有 Windows PowerShell 5.1：
// 无 BOM 的 UTF-8 会被按 ANSI 代码页解析，导致中文乱码并可能直接语法报错。
func withBOM(b []byte) []byte {
	return append([]byte{0xEF, 0xBB, 0xBF}, b...)
}

// isTextName 判断包内文件是否属于「需要按 Windows 原生形态规范的文本文件」。
//
// 只认扩展名，不嗅探内容：误判一次就会改掉 frpc.exe 的字节，而这份清单紧接着
// 要被算成 SHA256 写进 SHA256SUMS.txt —— 一字节之差，包就再也装不上。
func isTextName(name string) bool {
	switch {
	case strings.HasSuffix(name, ".ps1"),
		strings.HasSuffix(name, ".toml"),
		strings.HasSuffix(name, ".txt"),
		strings.HasSuffix(name, ".md"),
		strings.HasSuffix(name, ".json"):
		return true
	}
	return false
}

// stripBOM 去掉开头的 UTF-8 BOM。
func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// withCRLF 只规范行尾为 CRLF，不碰 BOM。
//
// 先统一成 LF 再统一成 CRLF，而不是直接把 \n 替换成 \r\n：
// 后者会把已经存在的 CRLF 变成 CRCRLF。
func withCRLF(b []byte) []byte {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return []byte(s)
}

// withCRLFBOM 把文本文件规范为「UTF-8 BOM + CRLF」。
//
// 给 .ps1 / .txt 用：脚本含中文提示，而无 BOM 的 UTF-8 会被 Windows
// PowerShell 5.1 按 ANSI 代码页解析，中文整段乱码甚至直接语法报错。
//
// 为什么连行尾也要在生成端就管住：
//
// 包内的 config.toml 是运维拿到手后唯一会亲手打开编辑的东西（改监听端口、
// 改节点名、改 Dashboard 地址都是标准动作）。而 Windows Server 2019 自带的
// 老版记事本在保存时会把裸 LF 全部改写成 CRLF —— 运维「打开看了一眼、顺手
// 保存」，文件哈希就已经变了，随后 install.ps1 会提示「与出厂版本不一致」，
// 把他引向一个根本不存在的故障。出厂就是 CRLF 的话，这一步对记事本是幂等的。
func withCRLFBOM(b []byte) []byte {
	out := withCRLF(b)
	if len(out) >= 3 && out[0] == 0xEF && out[1] == 0xBB && out[2] == 0xBF {
		return out
	}
	return append([]byte{0xEF, 0xBB, 0xBF}, out...)
}

// normalizeText 按文件类型施加 Windows 原生形态，二进制原样返回。
//
// 判据不是「扩展名」，而是**这份文件由谁的解析器读** —— 两边对 BOM 的容忍度
// 恰好相反，一刀切必然错一边：
//
//   - config.toml（管理平台自己的配置）由 internal/config 读，BurntSushi 容 BOM。
//     它整篇是中文注释，运维会拿记事本打开，所以必须带 BOM，否则中文注释
//     在简体中文 Windows 上会整段乱码。行尾也统一 CRLF（见 withCRLFBOM）。
//   - frpc.toml / frps.toml 由 FRP 自己读，而 FRP 0.69.0 的 TOML 解析器遇到
//     BOM 直接报 `toml: line 1, column 1: toml: invalid character at start of
//     key: ï` 并退出 1 —— frpc/frps 于是完全起不来。
//
// 这条结论是真跑出来的，不是读文档得到的：同一份配置按「行尾 × BOM」四种
// 组合各跑一次 frpc verify -c，只有带 BOM 的两种失败
// （见 .workbuddy/test/verify-toml-variants.ps1）。顺手"统一加 BOM"的代价是
// 整个包都跑不起来，所以这里刻意写成显式分支而不是一个布尔开关。
func normalizeText(p Platform, name string, data []byte) []byte {
	// Linux 目标：一律 LF、一律无 BOM。
	//
	// 这一档必须整块前置，不能落进下面那串 switch —— 那串是 Windows 的
	// 「按扩展名分三档」，对 Linux 而言三档答案全一样（无 BOM + LF），
	// 走进去只会增加「某一档将来被误改成带 BOM」的风险。
	//
	// .sh 的 BOM 禁忌见 Platform.WantsBOM 的注释：内核找不到解释器时
	// 报的是「No such file or directory」，指向完全错误的方向。
	// unit 文件同样不能有 BOM：systemd 会把首行 [
	// 前面那三个不可见字节当成语法错误。
	if !p.IsWindows() {
		if isTextName(name) || strings.HasSuffix(name, ".sh") || name == unitFileName {
			return withLF(stripBOM(data))
		}
		return data
	}
	switch {
	case name == appConfigName:
		return withCRLFBOM(data)
	// .bat 的 BOM 要求与其它文本**恰好相反**，这是本项目第二份「一刀切必然错一边」
	// 的清单（第一份是 frpc.toml 那份）：cmd.exe 会把 BOM 当作命令的一部分，
	// 双击后首行直接报「'ï»¿@echo' 不是内部或外部命令」，安装入口彻底失效，
	// 且不产生任何日志。所以这里显式去 BOM。
	//
	// 顺序也重要：这一档排在 isTextName 之前。即便将来有人「顺手」把 .bat
	// 加进 isTextName（那条分支是加 BOM 的），这里仍然先命中，不会走错。
	case strings.HasSuffix(name, ".bat"):
		return withCRLF(stripBOM(data))
	case strings.HasSuffix(name, ".toml"):
		return withCRLF(stripBOM(data))
	case isTextName(name):
		return withCRLFBOM(data)
	}
	return data
}
