<#
.SYNOPSIS
    验证「管理平台自身部署包」（01-prd F18）能否真正安装、拉起、停止与卸载。

.DESCRIPTION
    这是 F18 的终极验证：脚本语法正确不等于它能跑。本测试真的注册 SYSTEM 计划任务、
    真的拉起 sinyefrp.exe 进程，并真的用 HTTP 请求确认管理台接口可用。

    与 lifecycle-test.ps1 一样，本测试始终保留一个「对照组」实例：一个由测试自己
    注册、不由产品脚本管理的平台实例。它在整个过程中必须一直活着 ——
    若它消失了，说明实现退回了 taskkill /im sinyefrp.exe 那套按进程名击杀的做法，
    而那会打断同机上任何别的平台实例。这是 F14「不按进程名误杀」对平台角色的判据。

    对照组刻意不用 Start-Process 拉起，而是由测试自己 Register-ScheduledTask：
    这样「平台如何被拉起」这条路径不引入第三种方式，测试与生产用的是同一个机制。

    需要管理员权限（要创建 SYSTEM 计划任务）。

    哈希校验是两级判定，本测试两侧都要守：主程序被改动 → 退出码 12 中止；
    配置文件（config.toml）被改动 → 只提示不阻断。后者是部署常态（改端口、
    改节点名），若把它也判死，运维就只能靠 -SkipHashCheck 通过，而那个开关会
    连主程序的校验一起关掉。

    两个容易踩的坑，写在这里免得下次又踩：

    1. 所有函数调用外层都套 @()。PowerShell 的 `return @()` 不会返回空数组，
       而是什么都不输出，调用方拿到 $null，`.Count` 随之变成空值 —— 断言会静默失真。

    2. 局部变量禁止叫 $task / $exe / $conf。PowerShell 变量名不区分大小写，
       它们会覆盖脚本头部的同名常量，导致传给 Stop-ScheduledTask 的变成对象而非名字。

.EXAMPLE
    .\scripts\platform-test.ps1
    .\scripts\platform-test.ps1 -SourceZip D:\dist\SinyeFRP-platform-0.1.0.zip
#>
[CmdletBinding()]
param(
    [string]$SourceZip = '',
    [string]$WorkDir   = '',
    [string]$LogPath   = '',
    [switch]$KeepWorkDir
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$Root = Split-Path -Parent $PSScriptRoot

# ---- 日志：结果同时进控制台与文件（自动化环境下控制台常常拿不到）----
$base = Join-Path $Root '.workbuddy\f18'
if (-not $LogPath) {
    $LogPath = Join-Path $base ('logs\' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
}
$logDir = Split-Path -Parent $LogPath
if ($logDir -and -not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }

$script:Log = New-Object System.Collections.Generic.List[string]
function Say([string]$Text, [string]$Color = 'Gray') {
    $script:Log.Add($Text)
    try { Write-Host $Text -ForegroundColor $Color } catch { }
}
function Save-Log {
    try {
        if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }
        [System.IO.File]::WriteAllLines($LogPath, $script:Log, (New-Object System.Text.UTF8Encoding($true)))
    } catch { }
}

# ---- 断言 ----
$script:Pass = 0
$script:Fail = 0
function Assert-True([bool]$Condition, [string]$Desc) {
    if ($Condition) {
        $script:Pass++
        Say ('  [PASS] ' + $Desc) 'Green'
    } else {
        $script:Fail++
        Say ('  [FAIL] ' + $Desc) 'Red'
    }
}

# ---- 被测脚本的宿主：固定用 Windows PowerShell 5.1 ----
$PS51 = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (-not (Test-Path $PS51)) { $PS51 = 'powershell.exe' }

# ---- 端口：刻意避开生产默认的 7800 与 frps/Dashboard 的 7000/7500 ----
$PortDeploy   = 7831
$PortSentinel = 7833

$TaskDeploy   = 'SinyeFRP-platform'
$TaskSentinel = 'SinyeFRP-platform-sentinel'

# ---- 工作目录 ----
if (-not $WorkDir) {
    $WorkDir = Join-Path $base ('run-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
}
$DirDeploy   = Join-Path $WorkDir 'deploy'
$DirSentinel = Join-Path $WorkDir 'sentinel'
# 篡改检测专用副本：改动主程序属于破坏性操作，不能拿被测目录做实验。
$DirTamper   = Join-Path $WorkDir 'tamper'

# ---- 源包 ----
if (-not $SourceZip) {
    $SourceZip = Join-Path $base 'platform.zip'
}
if (-not (Test-Path $SourceZip)) {
    throw ('找不到平台部署包: ' + $SourceZip + '；请先执行 bin\sinyefrp.exe pkg app -o .workbuddy\f18\platform.zip')
}

# ---------- 辅助函数 ----------

# 在本部署目录内运行一个产品脚本，返回退出码与输出。
#
# 数组 splatting 对原生程序是逐元素传参（与 PS 脚本的 [switch] 参数不同，
# 后者必须用哈希表 splatting）—— 这里传的是 powershell.exe，属原生程序。
function Invoke-DeployScript {
    param(
        [Parameter(Mandatory)][string]$Dir,
        [Parameter(Mandatory)][string]$Script,
        [string[]]$Arguments = @()
    )
    $path = Join-Path $Dir $Script
    $tmpLog = Join-Path $env:TEMP ('plat-' + [guid]::NewGuid().ToString('N') + '.log')
    $prev = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $PS51 -NoProfile -ExecutionPolicy Bypass -File $path @Arguments *> $tmpLog
        $code = $LASTEXITCODE
        $text = ''
        if (Test-Path $tmpLog) { $text = [System.IO.File]::ReadAllText($tmpLog) }
        return [pscustomobject]@{ Code = $code; Text = $text }
    } finally {
        $ErrorActionPreference = $prev
        Remove-Item $tmpLog -Force -ErrorAction SilentlyContinue
    }
}

# 认领本部署目录对应的进程。
#
# 与产品脚本使用完全相同的判据（exe 绝对路径优先，取不到时回退 CommandLine）。
# 若这里放宽标准，产品认不出自己拉起的进程这种致命缺陷就会被掩盖。
function Get-InstanceProcess {
    param([Parameter(Mandatory)][string]$Dir)
    $exePath  = Join-Path $Dir 'sinyefrp.exe'
    $confPath = Join-Path $Dir 'config.toml'
    $hits = @()
    foreach ($p in @(Get-CimInstance Win32_Process -Filter "Name='sinyefrp.exe'" -ErrorAction SilentlyContinue)) {
        if ($p.ExecutablePath -and $p.ExecutablePath.Equals($exePath, [StringComparison]::OrdinalIgnoreCase)) {
            $hits += $p
            continue
        }
        if (-not $p.ExecutablePath -and $p.CommandLine) {
            $cmd = $p.CommandLine
            $hasExe  = $cmd.IndexOf($exePath, [StringComparison]::OrdinalIgnoreCase) -ge 0
            $hasConf = $cmd.IndexOf($confPath, [StringComparison]::OrdinalIgnoreCase) -ge 0
            if ($hasExe -and $hasConf) { $hits += $p }
        }
    }
    return $hits
}

# 直接发一个 HTTP/1.1 请求拿回原始响应。
#
# 刻意不用 Invoke-WebRequest：PS 5.1 的它默认走 IE 引擎与系统代理设置，
# 在配置过代理的机器上可能连 127.0.0.1 都被转出去，得到的结论不可信。
function Get-RawHttp([int]$Port, [string]$Path) {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne(2000, $false)) { return '' }
        $client.EndConnect($iar)
        $stream = $client.GetStream()
        $req = 'GET ' + $Path + " HTTP/1.1`r`nHost: 127.0.0.1:" + $Port + "`r`nConnection: close`r`n`r`n"
        $bytes = [System.Text.Encoding]::ASCII.GetBytes($req)
        $stream.Write($bytes, 0, $bytes.Length)
        $stream.Flush()
        $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::UTF8)
        return $reader.ReadToEnd()
    } catch {
        return ''
    } finally {
        try { $client.Close() } catch { }
    }
}

# 从部署包解压出一个实例目录。
function Expand-Deploy {
    param([Parameter(Mandatory)][string]$Dir)
    if (Test-Path $Dir) { Remove-Item $Dir -Recurse -Force }
    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [System.IO.Compression.ZipFile]::ExtractToDirectory($SourceZip, $Dir)
    return $Dir
}

# 改写部署目录内 config.toml 的监听端口。
# 用 UTF8Encoding($true) 写回：配置文件带 BOM，去掉它会让记事本把中文注释读成乱码。
function Set-DeployPort {
    param([Parameter(Mandatory)][string]$Dir, [Parameter(Mandatory)][int]$Port)
    $confPath = Join-Path $Dir 'config.toml'
    $text = [System.IO.File]::ReadAllText($confPath)
    $text = $text -replace '(?m)^port\s*=\s*\d+', ('port = ' + $Port)
    [System.IO.File]::WriteAllText($confPath, $text, (New-Object System.Text.UTF8Encoding($true)))
}

# 清理一个计划任务（不存在也算成功）。
function Remove-TaskQuietly([string]$Name) {
    try {
        $info = Get-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue
        if ($info) {
            Stop-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $Name -Confirm:$false -ErrorAction SilentlyContinue
        }
    } catch { }
}

# ---------- 开始 ----------

Say '============================================================'
Say ' SinyeFRP 管理平台自身部署包 · 生命周期验证（F18）'
Say '============================================================'
Say ('源包      : ' + $SourceZip)
Say ('工作目录  : ' + $WorkDir)
Say ('被测端口  : ' + $PortDeploy + '   对照端口: ' + $PortSentinel)
Say ('日志文件  : ' + $LogPath)
Say ''

$sentinelTask = $null
$exitCode = 0

try {
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    Assert-True $isAdmin '当前进程具备管理员权限（创建 SYSTEM 计划任务的前提）'
    if (-not $isAdmin) { throw '本测试需要管理员权限' }

    # ---- 准备两个实例：被测实例 + 对照组 ----
    Say ''
    Say '-- 准备实例 --'
    [void](Expand-Deploy -Dir $DirDeploy)
    [void](Expand-Deploy -Dir $DirSentinel)
    Set-DeployPort -Dir $DirDeploy   -Port $PortDeploy
    Set-DeployPort -Dir $DirSentinel -Port $PortSentinel

    $files = @(Get-ChildItem $DirDeploy -File | Select-Object -ExpandProperty Name)
    Assert-True ($files -contains 'sinyefrp.exe') '包内含 sinyefrp.exe'
    Assert-True ($files -contains 'config.toml') '包内含 config.toml'
    Assert-True ($files -contains 'SHA256SUMS.txt') '包内含 SHA256SUMS.txt'
    foreach ($s in @('install.ps1', 'uninstall.ps1', 'start.ps1', 'stop.ps1', 'restart.ps1', 'status.ps1')) {
        Assert-True ($files -contains $s) ('包内含 ' + $s)
    }
    Say ('  包内文件: ' + ($files -join ', '))

    # ---- 1. 哈希校验是两级判定：主程序被改必须拦，配置被改只提示 ----
    #
    # 这两条的不对称是刻意的。若配置文件被改也判死，运维改完端口再安装就必须加
    # -SkipHashCheck，而那个开关连主程序的校验一起关掉 —— 等于为了改一个端口
    # 放弃对代码的校验。所以这里同时守住两侧：既不能放过主程序被改，
    # 也不能把正常的配置改动挡在门外。
    Say ''
    Say '-- 1. 哈希校验（两级判定）--'

    # 1a. 主程序被改 → 必须中止
    [void](Expand-Deploy -Dir $DirTamper)
    Set-DeployPort -Dir $DirTamper -Port $PortDeploy
    $tamperExe = Join-Path $DirTamper 'sinyefrp.exe'
    $fs = [System.IO.File]::Open($tamperExe, [System.IO.FileMode]::Append, [System.IO.FileAccess]::Write)
    try { $fs.WriteByte(0) } finally { $fs.Close() }
    $r = Invoke-DeployScript -Dir $DirTamper -Script 'install.ps1'
    Assert-True ($r.Code -eq 12) ('主程序被改动后安装应被拒（退出码 12），实际 ' + $r.Code)
    Assert-True ($r.Text -match '校验失败') '提示中含「校验失败」'
    Assert-True ($r.Text -match 'sinyefrp\.exe') '提示点名了被改动的主程序'

    # 1b. 配置文件被改（部署常态）→ 只提示，不阻断，且不加任何开关
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'install.ps1'
    Assert-True ($r.Code -eq 0) ('config.toml 改过端口后，不加 -SkipHashCheck 也应装成功，实际退出码 ' + $r.Code)
    Assert-True ($r.Text -match '主程序哈希校验通过') '输出含「主程序哈希校验通过」'
    Assert-True ($r.Text -match 'config\.toml 与出厂版本不一致') '输出提示 config.toml 与出厂版本不一致'
    Assert-True (-not ($r.Text -match '校验失败')) '配置改动不会被当作校验失败'

    # ---- 2. 启动对照组（测试自管，不属于产品脚本的管辖范围）----
    Say ''
    Say '-- 2. 启动对照组实例（必须全程存活）--'
    $sentinelAction = New-ScheduledTaskAction -Execute (Join-Path $DirSentinel 'sinyefrp.exe') `
        -Argument ('-config "' + (Join-Path $DirSentinel 'config.toml') + '"') -WorkingDirectory $DirSentinel
    $sentinelTrigger   = New-ScheduledTaskTrigger -AtStartup
    $sentinelPrincipal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    $sentinelSettings  = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero)
    Register-ScheduledTask -TaskName $TaskSentinel -Action $sentinelAction -Trigger $sentinelTrigger `
        -Principal $sentinelPrincipal -Settings $sentinelSettings -Force | Out-Null
    Start-ScheduledTask -TaskName $TaskSentinel
    Start-Sleep -Seconds 4

    $sentinelProcs = @(Get-InstanceProcess -Dir $DirSentinel)
    Assert-True ($sentinelProcs.Count -gt 0) ('对照组进程已拉起（PID ' + (($sentinelProcs | ForEach-Object { $_.ProcessId }) -join ', ') + '）')
    $sentinelHealth = Get-RawHttp -Port $PortSentinel -Path '/api/health'
    Assert-True ($sentinelHealth -match '"status"\s*:\s*"ok"') ('对照组管理台 /api/health 返回 ok（端口 ' + $PortSentinel + '）')

    # ---- 3. 重复安装（幂等）与 -SkipHashCheck 逃生口 ----
    #
    # 第 1b 步已经装过一次，这里再装一次。F13 要求重复安装只更新原任务而不产生
    # 副本，而「卸载后重装」正是运维最常见的动作，必须真的走一遍。
    Say ''
    Say '-- 3. install.ps1 重复安装（幂等）与 -SkipHashCheck --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'install.ps1' -Arguments @('-SkipHashCheck')
    Say $r.Text
    Assert-True ($r.Code -eq 0) ('install.ps1 退出码 0，实际 ' + $r.Code)
    Assert-True ($r.Text -match '已跳过哈希校验') '-SkipHashCheck 时明确提示已跳过校验（含主程序）'
    Assert-True ($r.Text -match '\[1/5\]') '安装输出含步骤 [1/5]（哈希校验）'
    Assert-True ($r.Text -match '\[2/5\]') '安装输出含步骤 [2/5]（数据目录与权限）'
    Assert-True ($r.Text -match '\[5/5\]') '安装输出含步骤 [5/5]（启动）'

    $deployProcs = @(Get-InstanceProcess -Dir $DirDeploy)
    Assert-True ($deployProcs.Count -gt 0) ('被测实例进程存在（PID ' + (($deployProcs | ForEach-Object { $_.ProcessId }) -join ', ') + '）')
    Assert-True ($r.Text -match ('http://127\.0\.0\.1:' + $PortDeploy + '/')) '安装输出给出管理台地址（端口取自 config.toml）'

    $deployHealth = Get-RawHttp -Port $PortDeploy -Path '/api/health'
    Assert-True ($deployHealth -match '"status"\s*:\s*"ok"') ('被测实例 /api/health 返回 ok（端口 ' + $PortDeploy + '）')

    # 数据目录：进程起来后数据库应当已被创建
    Assert-True (Test-Path (Join-Path $DirDeploy 'data')) '安装脚本创建了 data 目录'
    Assert-True (Test-Path (Join-Path $DirDeploy 'logs')) '安装脚本创建了 logs 目录'
    Assert-True (Test-Path (Join-Path $DirDeploy 'data\sinyefrp.db')) '启动后数据库文件已创建（相对路径按配置文件目录解析）'
    Assert-True (Test-Path (Join-Path $DirDeploy 'logs\sinyefrp.log')) '启动后日志文件已创建'

    # 双击入口。平台包与 frp 包共用同一套模板，所以这一项同时守住两件事：
    # 中文文件名在 ZIP 里编码正确（解压后能原样落地），以及入口内容契约完整。
    # 四条断言全部无条件求值：文件缺失时它们一起失败，而不是让后面的
    # ReadAllBytes 抛异常把整段测试掀掉。
    $batPath = Join-Path $DirDeploy '一键安装.bat'
    $batRaw  = @(); $batTxt = ''
    try {
        $batRaw = [System.IO.File]::ReadAllBytes($batPath)
        $batTxt = [System.IO.File]::ReadAllText($batPath)
    } catch { }
    $batOk = ($batRaw.Count -gt 0)
    Assert-True ($batOk -and $batRaw.Count -lt 4096) '部署目录含双击入口（一键安装.bat）'
    $batAscii = $true
    foreach ($b in $batRaw) { if ($b -gt 127) { $batAscii = $false; break } }
    Assert-True ($batOk -and $batAscii) '双击入口为纯 ASCII（cmd 按 CP936 解析 .bat，中文会乱码）'
    $batBom = ($batRaw.Length -ge 3 -and $batRaw[0] -eq 0xEF -and $batRaw[1] -eq 0xBB -and $batRaw[2] -eq 0xBF)
    Assert-True ($batOk -and (-not $batBom)) '双击入口不带 BOM（cmd 会把 BOM 当命令的一部分）'
    Assert-True ($batOk -and $batTxt -match 'install\.ps1' -and $batTxt -match '-Elevate' -and $batTxt -match '-Pause') '双击入口调用 install.ps1 且带 -Elevate -Pause'

    # ACL：不应再有 Users / Everyone
    $aclText = (& icacls (Join-Path $DirDeploy 'data')) -join ' '
    Assert-True ($aclText -notmatch 'BUILTIN\\Users') 'data 目录 ACL 已移除 BUILTIN\Users'
    Assert-True ($aclText -match 'BUILTIN\\Administrators') 'data 目录 ACL 保留 Administrators'
    Assert-True ($aclText -match 'NT AUTHORITY\\SYSTEM') 'data 目录 ACL 保留 SYSTEM'

    # ---- 3b. 自启动契约：回读系统里真实注册的任务对象 ----
    #
    # 上面验的是「现在能跑起来」，这一段验的是「机器重启后还能自己跑起来」。
    # 计划任务是首版唯一的自启动机制（不引 WinSW、不用根目录那份旧 nssm.exe），
    # 所以它的触发条件、运行身份与容错设置本身就是产品契约。
    #
    # 为什么必须回读真实注册的对象、而不是 grep 脚本文本：pkg-test 已经断言过
    # install.ps1 里写了 -AtStartup 与 SYSTEM。但「脚本里写了」与「系统里生效」
    # 是两件事 —— Register-ScheduledTask 的参数一旦拼错，脚本照样跑完并打印成功，
    # 而任务可能压根没建或建成了一个没触发器的空壳。只有回读任务对象才能证实。
    #
    # 注意局部变量不能叫 $taskDeploy：PS 变量名大小写不敏感，那会直接覆盖
    # 头部常量 $TaskDeploy（任务名），后面清理段就会把任务对象当名字传进去。
    Say ''
    Say '-- 3b. 计划任务自启动契约（回读真实注册的任务）--'
    $taskObj = Get-ScheduledTask -TaskName $TaskDeploy -ErrorAction SilentlyContinue
    Assert-True ($null -ne $taskObj) ('计划任务已注册: ' + $TaskDeploy)
    if ($taskObj) {
        Assert-True ($taskObj.Principal.UserId -eq 'SYSTEM') ('以 SYSTEM 身份运行，UserId=' + $taskObj.Principal.UserId)
        Assert-True (([string]$taskObj.Principal.RunLevel) -eq 'Highest') ('提权级别为 Highest，实际 ' + $taskObj.Principal.RunLevel)

        # 触发器类型从 CimClass 取：PS 5.1 里 Triggers 元素的 TypeNames 不总是稳定，
        # 这里与 lifecycle-test.ps1 用同一套判据，避免两个套件对同一件事各判各的。
        $trigNames = @()
        foreach ($t in @($taskObj.Triggers)) {
            if ($t.CimClass -and $t.CimClass.CimClassName) { $trigNames += $t.CimClass.CimClassName }
            elseif ($t.PSObject.TypeNames.Count -gt 0)     { $trigNames += $t.PSObject.TypeNames[0] }
        }
        Assert-True ((@($trigNames) -join ',') -match 'Boot') ('触发器为系统启动 Boot，实际 ' + ($trigNames -join ','))

        Assert-True ($taskObj.Actions[0].Execute -eq (Join-Path $DirDeploy 'sinyefrp.exe')) ('动作指向本部署目录的主程序，实际 ' + $taskObj.Actions[0].Execute)
        Assert-True ($taskObj.Actions[0].WorkingDirectory -eq $DirDeploy) ('工作目录为部署目录（配置相对路径据此解析），实际 ' + $taskObj.Actions[0].WorkingDirectory)
        Assert-True ($taskObj.Settings.RestartCount -ge 1) ('含失败重启策略，RestartCount=' + $taskObj.Settings.RestartCount)
        Assert-True (([string]$taskObj.Settings.ExecutionTimeLimit) -match '^PT0S$|^$') ('无执行时长上限（否则默认 72 小时会被任务计划强杀），实际 ' + $taskObj.Settings.ExecutionTimeLimit)
        Assert-True ([bool]$taskObj.Settings.StartWhenAvailable) '错过启动时间会尽快补跑（StartWhenAvailable）'
        Assert-True ($taskObj.State -ne 'Disabled') ('任务未被禁用，State=' + $taskObj.State)
    }

    # ---- 4. 对照组必须仍然存活（F14 不按进程名误杀）----
    Say ''
    Say '-- 4. 对照组存活检查（安装之后）--'
    $sentinelProcs = @(Get-InstanceProcess -Dir $DirSentinel)
    Assert-True ($sentinelProcs.Count -gt 0) '安装被测实例后，对照组进程未被误杀'
    Assert-True ((Get-RawHttp -Port $PortSentinel -Path '/api/health') -match '"status"\s*:\s*"ok"') '对照组管理台仍然可用'

    # ---- 5. status.ps1 -Json ----
    Say ''
    Say '-- 5. status.ps1 -Json --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'status.ps1' -Arguments @('-Json')
    Say ('  ' + $r.Text.Trim())
    Assert-True ($r.Code -eq 0) ('status.ps1 -Json 退出码 0（运行中），实际 ' + $r.Code)
    $st = $null
    try { $st = $r.Text.Trim() | ConvertFrom-Json } catch { }
    Assert-True ($null -ne $st) 'status.ps1 -Json 输出可被解析为 JSON'
    if ($st) {
        Assert-True ([bool]$st.installed) 'installed = true'
        Assert-True ([bool]$st.running) 'running = true'
        Assert-True ([bool]$st.portListening) 'portListening = true'
        Assert-True ($st.taskName -eq $TaskDeploy) ('taskName = ' + $TaskDeploy)
        Assert-True ([string]$st.platformVersion -eq '0.1.0-dev') ('platformVersion = 0.1.0-dev，实际 ' + $st.platformVersion)
    }

    # ---- 6. stop.ps1 ----
    Say ''
    Say '-- 6. stop.ps1 --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'stop.ps1'
    Assert-True ($r.Code -eq 0) ('stop.ps1 退出码 0，实际 ' + $r.Code)
    Assert-True (@(Get-InstanceProcess -Dir $DirDeploy).Count -eq 0) '被测实例进程已停止'
    Assert-True (-not ((Get-RawHttp -Port $PortDeploy -Path '/api/health') -match '"status"')) ('端口 ' + $PortDeploy + ' 已释放')
    Assert-True (@(Get-InstanceProcess -Dir $DirSentinel).Count -gt 0) 'stop.ps1 没有影响对照组'

    # ---- 7. start.ps1 ----
    Say ''
    Say '-- 7. start.ps1 --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'start.ps1'
    Assert-True ($r.Code -eq 0) ('start.ps1 退出码 0，实际 ' + $r.Code)
    Assert-True (@(Get-InstanceProcess -Dir $DirDeploy).Count -gt 0) '被测实例已被重新拉起'
    Assert-True ((Get-RawHttp -Port $PortDeploy -Path '/api/health') -match '"status"\s*:\s*"ok"') '重新拉起后管理台可用'

    # ---- 8. restart.ps1 ----
    Say ''
    Say '-- 8. restart.ps1 --'
    $before = @(Get-InstanceProcess -Dir $DirDeploy)
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'restart.ps1'
    Assert-True ($r.Code -eq 0) ('restart.ps1 退出码 0，实际 ' + $r.Code)
    $after = @(Get-InstanceProcess -Dir $DirDeploy)
    Assert-True ($after.Count -gt 0) '重启后进程存在'
    $beforeIds = @($before | ForEach-Object { [int]$_.ProcessId })
    $afterIds  = @($after  | ForEach-Object { [int]$_.ProcessId })
    Assert-True ($null -ne (Compare-Object $beforeIds $afterIds)) '重启确实换了进程（PID 变化）'
    Assert-True (@(Get-InstanceProcess -Dir $DirSentinel).Count -gt 0) 'restart.ps1 没有影响对照组'

    # ---- 9. uninstall.ps1（默认保留数据）----
    Say ''
    Say '-- 9. uninstall.ps1 --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'uninstall.ps1'
    Say $r.Text
    Assert-True ($r.Code -eq 0) ('uninstall.ps1 退出码 0，实际 ' + $r.Code)
    Assert-True (@(Get-InstanceProcess -Dir $DirDeploy).Count -eq 0) '卸载后进程已停止'
    Assert-True ($null -eq (Get-ScheduledTask -TaskName $TaskDeploy -ErrorAction SilentlyContinue)) '卸载后计划任务已删除'
    Assert-True (Test-Path (Join-Path $DirDeploy 'data\sinyefrp.db')) '默认卸载保留了数据库'
    Assert-True (Test-Path (Join-Path $DirDeploy 'config.toml')) '默认卸载保留了配置文件'
    Assert-True (@(Get-InstanceProcess -Dir $DirSentinel).Count -gt 0) 'uninstall.ps1 没有影响对照组'

    # ---- 9b. uninstall.ps1 -RemoveData 时**实例仍在运行**的处置 ----
    #
    # 这条用例是补上一个真实事故的缺口：老版本 uninstall.ps1 删 data\ 前不查活进程，
    # 而 Windows 上「删除一个仍被打开的文件」会**成功**（目录项摘掉，进程仍可读写），
    # 于是脚本报成功、平台继续跑、数据已经在磁盘上没了 —— 要等下次重启
    # 才以「记录凭空消失」的形态暴露。
    #
    # ⚠️ 断言口径要说清：**基线不要求「拒绝」，只要求「不许在进程还活着时删数据」**。
    # 产品当前的选择是「先自己把进程优雅停掉，停干净了再删」——这是合理且更好用的行为
    # （运维拿到的是一次成功的卸载，而不是一句「请先跑 stop.ps1」）。所以这里断言的是
    # 那条底线：走到删数据那一刻时，进程必须已经没了。
    #
    # 这条用例刻意**不走 install.ps1**，而是直接拉起 exe 造出「实例在跑」的状态 ——
    # 生产上真正咬人的形态不是「任务还在」，而是**进程还在、任务已经不在**：
    #   · 运维手动 stop 过，或上次卸载把任务删了但进程没退干净；
    #   · 更常见的：进程是上次运行留下的孤儿，任务那侧早已无据可查。
    # 老版 uninstall.ps1 第 1 步就是 Unregister 计划任务，于是第 3 步的进程检查
    # 永远看到空列表 —— 只要任务不在，怎么查都拦不住。所以用例必须绕开任务，
    # 直接验证「有活进程就不许删」这条底线本身。
    Say ''
    Say '-- 9b. uninstall.ps1 -RemoveData（实例运行中、任务已不在）--'

    # 先把第 9 条留下的状态清干净，并造出一个「无任务、有进程」的现场。
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'status.ps1' -Arguments @('-Json')
    Assert-True ($r.Code -eq 2) ('9b 前置：当前应为未安装状态，实际退出码 ' + $r.Code)

    $dbFile = Join-Path $DirDeploy 'data\sinyefrp.db'
    Assert-True (Test-Path $dbFile) '9b 前置：数据库文件仍在（第 9 条保留了它）'

    # 直接以独立进程拉起被测程序。WorkingDirectory 设成部署根，
    # 与计划任务的 Action.WorkingDirectory 一致 —— 相对路径的解析基准相同，
    # 数据库才会落到同一个 data\ 下。
    $probe = Start-Process -FilePath (Join-Path $DirDeploy 'sinyefrp.exe') `
        -ArgumentList @('-config', ('"' + (Join-Path $DirDeploy 'config.toml') + '"')) `
        -WorkingDirectory $DirDeploy -PassThru
    Start-Sleep -Seconds 3
    $alive = @(Get-InstanceProcess -Dir $DirDeploy)
    Assert-True ($alive.Count -gt 0) ('9b 前置：实例已在运行（PID ' + (($alive | ForEach-Object { $_.ProcessId }) -join ', ') + '）')
    # 关键前置：这次**没有**注册计划任务，确保用例走的是「任务不在」那条路。
    Assert-True ($null -eq (Get-ScheduledTask -TaskName $TaskDeploy -ErrorAction SilentlyContinue)) '9b 前置：计划任务确实不存在（无据可查的运行中实例）'
    # 前置：库里必须有可丢的数据，否则「数据还在不在」这条断言是空的。
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'status.ps1' -Arguments @('-Json')
    Assert-True ($r.Text -match '"processCount"\s*:\s*1') '9b 前置：status 报告恰好 1 个进程'

    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'uninstall.ps1' -Arguments @('-RemoveData')
    Assert-True ($r.Code -eq 0) ('9b：-RemoveData 应正常完成，实际退出码 ' + $r.Code)

    # ★ 核心底线：既然脚本报告删成功了，那删之前进程就必须已经不在了。
    #   老版本这里会输出「无运行中的进程」并把 data\ 删掉 —— 就是那次生产事故。
    Assert-True ($r.Text -notmatch '无运行中的进程') ('不允许在实例仍运行时报「无运行中的进程」，实际输出: ' + $r.Text.Substring(0, [Math]::Min(300, $r.Text.Length)))
    Assert-True ($r.Text -match '已停止 1 个进程') '9b：脚本在删数据前自己停下了那个进程'
    # 删成功了，就说明进程确实没了 —— 不能再有活进程。
    Assert-True (@(Get-InstanceProcess -Dir $DirDeploy).Count -eq 0) '9b：-RemoveData 完成后不再有活进程'
    Assert-True (-not (Test-Path $dbFile)) '9b：-RemoveData 确实删掉了数据库（说明它真的走到了删数据那一步）'

    # ---- 10. status.ps1 在未安装时的语义 ----
    Say ''
    Say '-- 10. 未安装状态 --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'status.ps1' -Arguments @('-Json')
    Assert-True ($r.Code -eq 2) ('未安装时 status.ps1 退出码应为 2，实际 ' + $r.Code)
    Assert-True ($r.Text -match '"installed"\s*:\s*false') 'installed = false'

    # ---- 11. start.ps1 在未安装时必须拒绝 ----
    Say ''
    Say '-- 11. 未安装时 start.ps1 的语义 --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'start.ps1'
    Assert-True ($r.Code -eq 2) ('未安装时 start.ps1 退出码应为 2，实际 ' + $r.Code)

    # ---- 12. uninstall.ps1 -RemoveData ----
    Say ''
    Say '-- 12. uninstall.ps1 -RemoveData --'
    $r = Invoke-DeployScript -Dir $DirDeploy -Script 'uninstall.ps1' -Arguments @('-RemoveData')
    Assert-True ($r.Code -eq 0) ('uninstall.ps1 -RemoveData 退出码 0，实际 ' + $r.Code)
    Assert-True (-not (Test-Path (Join-Path $DirDeploy 'data'))) '-RemoveData 删除了 data 目录'
    Assert-True (-not (Test-Path (Join-Path $DirDeploy 'logs'))) '-RemoveData 删除了 logs 目录'
    Assert-True (-not (Test-Path (Join-Path $DirDeploy 'config.toml'))) '-RemoveData 删除了 config.toml'
    Assert-True (@(Get-InstanceProcess -Dir $DirSentinel).Count -gt 0) '-RemoveData 没有影响对照组'

    # ---- 13. 命令行子命令 ----
    Say ''
    Say '-- 13. CLI: pkg app --'
    $exeDeploy = Join-Path $DirDeploy 'sinyefrp.exe'
    # 上一步 -RemoveData 把 exe 也删了，这里从对照组取一份（同一版本）
    $exeDeploy = Join-Path $DirSentinel 'sinyefrp.exe'
    $cliOut = Join-Path $WorkDir 'cli-generated.zip'
    $cliLog = Join-Path $env:TEMP ('plat-cli-' + [guid]::NewGuid().ToString('N') + '.log')
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    & $exeDeploy pkg app -config (Join-Path $DirSentinel 'config.toml') -o $cliOut *> $cliLog
    $cliCode = $LASTEXITCODE
    $ErrorActionPreference = $prevEap
    $cliText = ''
    if (Test-Path $cliLog) { $cliText = [System.IO.File]::ReadAllText($cliLog); Remove-Item $cliLog -Force }
    Say ('  ' + $cliText.Trim())
    Assert-True ($cliCode -eq 0) ('pkg app 退出码 0，实际 ' + $cliCode)
    Assert-True (Test-Path $cliOut) 'CLI 产出了 ZIP 文件'
    if (Test-Path $cliOut) {
        Assert-True ((Get-Item $cliOut).Length -gt 5MB) 'CLI 产出的 ZIP 体积合理（> 5 MB）'
    }
    Assert-True ($cliText -match '已生成平台自身部署包') 'CLI 输出了生成结果'

    # ---- 14. 平台包可重复生成且内容稳定 ----
    Say ''
    Say '-- 14. 生成的可复现性 --'
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zipA = [System.IO.Compression.ZipFile]::OpenRead($SourceZip)
    try {
        $namesA = @($zipA.Entries | ForEach-Object { $_.FullName } | Sort-Object)
    } finally { $zipA.Dispose() }
    $zipB = [System.IO.Compression.ZipFile]::OpenRead($cliOut)
    try {
        $namesB = @($zipB.Entries | ForEach-Object { $_.FullName } | Sort-Object)
    } finally { $zipB.Dispose() }
    Assert-True (($namesA -join ',') -eq ($namesB -join ',')) '两次生成的包内文件清单完全一致'
    Say ('  文件清单: ' + ($namesA -join ', '))

} catch {
    $script:Fail++
    Say ''
    Say ('[异常] ' + $_.Exception.Message) 'Red'
    Say $_.ScriptStackTrace
    $exitCode = 1
} finally {
    # ---- 无论如何都要清理：计划任务与进程一个都不能留 ----
    Say ''
    Say '-- 清理 --'
    Remove-TaskQuietly $TaskDeploy
    Remove-TaskQuietly $TaskSentinel
    foreach ($d in @($DirDeploy, $DirSentinel, $DirTamper)) {
        $procs = @(Get-InstanceProcess -Dir $d)
        foreach ($p in $procs) { Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue }
    }
    Start-Sleep -Seconds 2
    $leftDeploy   = @(Get-InstanceProcess -Dir $DirDeploy).Count
    $leftSentinel = @(Get-InstanceProcess -Dir $DirSentinel).Count
    $leftTamper   = @(Get-InstanceProcess -Dir $DirTamper).Count
    Assert-True ($leftDeploy -eq 0) '被测实例进程已全部退出'
    Assert-True ($leftSentinel -eq 0) '对照组进程已全部退出'
    Assert-True ($leftTamper -eq 0) '篡改副本目录没有残留进程'
    Assert-True ($null -eq (Get-ScheduledTask -TaskName $TaskDeploy -ErrorAction SilentlyContinue)) '被测计划任务已移除'
    Assert-True ($null -eq (Get-ScheduledTask -TaskName $TaskSentinel -ErrorAction SilentlyContinue)) '对照组计划任务已移除'

    if (-not $KeepWorkDir) {
        # 两个实例目录里各有一份 52 MB 的 exe，留着会持续占盘。
        # 需要事后取证时用 -KeepWorkDir 保留。
        Remove-Item $WorkDir -Recurse -Force -ErrorAction SilentlyContinue
        Say ('  已清理工作目录 ' + $WorkDir + '（需要保留请加 -KeepWorkDir）')
    } else {
        Say ('  工作目录保留在 ' + $WorkDir)
    }

    Say ''
    Say '============================================================'
    $total = $script:Pass + $script:Fail
    if ($script:Fail -eq 0) {
        Say (' 结果: 全部通过  ' + $script:Pass + '/' + $total) 'Green'
    } else {
        Say (' 结果: ' + $script:Fail + ' 项失败  ' + $script:Pass + '/' + $total + ' 通过') 'Red'
        if ($exitCode -eq 0) { $exitCode = 1 }
    }
    Say '============================================================'
    Save-Log
}

exit $exitCode
