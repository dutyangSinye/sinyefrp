<#
.SYNOPSIS
    验证生成的部署包能否真正安装、拉起、停止与卸载。

.DESCRIPTION
    这是 01-prd F13/F14 的终极验证：脚本语法正确不等于它能跑。

    本测试会真的注册计划任务、真的拉起 frpc 进程，并在每一步之后确认
    「机器上其它 frp 实例没有被动过」——后者才是 F14「不按进程名误杀」
    的真正判据。测试期间机器上如果本来就跑着别的 frpc，它会作为对照组
    被持续观察；如果它消失了，说明实现退回了 taskkill /im 那套做法。

    需要管理员权限（要创建 SYSTEM 计划任务）。

    三个刻意为之的实现选择：

    1. 被测脚本一律在独立的 Windows PowerShell 5.1 子进程中执行。
       生产目标是 Windows Server 2019，只有 5.1；在 pwsh 7 里跑通不能说明
       问题，而且脚本若弹确认框会直接失败而非挂死，比同进程调用更安全。

    2. 测试与产品使用完全相同的「按路径认领进程」逻辑（含 CommandLine
       回退）。这不是为了迁就测试——如果产品脚本认不出自己拉起的进程，
       那本身就是产品的致命缺陷，测试必须把它暴露出来。

    3. 所有函数调用外层都套 @()。PowerShell 的 `return @()` 并不会返回一个
       空数组，而是什么都不输出，调用方拿到 $null，`.Count` 随之变成空值——
       断言会静默失真。这是本脚本第一版翻车的直接原因，勿删。

.EXAMPLE
    .\scripts\lifecycle-test.ps1
#>
[CmdletBinding()]
param(
    [string]$SourcePkg = '',
    [string]$DeployDir = '',
    [string]$LogPath   = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$Root = Split-Path -Parent $PSScriptRoot

# ---- 日志：结果同时进控制台与文件（自动化环境下控制台常常拿不到）----
# 日志与部署目录分开存放：两者都按时间戳命名，同目录下极易混淆。
$base = Join-Path $Root '.workbuddy\lifecycle'
if (-not $LogPath) {
    $LogPath = Join-Path (Join-Path $base 'logs') ((Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
}
$logDir = Split-Path -Parent $LogPath
if ($logDir -and -not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }

$script:Log = New-Object System.Collections.Generic.List[string]
function Say([string]$Text, [string]$Color = 'Gray') {
    $script:Log.Add($Text)
    try { Write-Host $Text -ForegroundColor $Color } catch { }
}

# 无条件落盘：即使后面抛异常，也要留下一份可读的证据。
function Save-Log {
    try {
        if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }
        [System.IO.File]::WriteAllLines($LogPath, $script:Log, (New-Object System.Text.UTF8Encoding($true)))
    } catch { }
}

# ---- 被测脚本的宿主：固定用 Windows PowerShell 5.1 ----
$PS51 = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (-not (Test-Path $PS51)) { $PS51 = 'powershell.exe' }

# ---- 源包与部署目录 ----
if (-not $SourcePkg) {
    $latest = Get-ChildItem (Join-Path $Root '.workbuddy\pkgtest') -Directory -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending | Select-Object -First 1
    if (-not $latest) { throw '未找到测试产物，请先运行 scripts\pkg-test.ps1' }
    $SourcePkg = Join-Path $latest.FullName 'client'
}
if (-not (Test-Path (Join-Path $SourcePkg 'install.ps1'))) {
    throw ('源包目录无效: ' + $SourcePkg)
}
if (-not $DeployDir) {
    $DeployDir = Join-Path $base (Get-Date -Format 'yyyyMMdd-HHmmss')
}

$script:Pass = 0
$script:Fail = 0
$script:FailNames = New-Object System.Collections.Generic.List[string]
$script:Results   = New-Object System.Collections.Generic.List[object]

function Result([string]$Name, [bool]$Ok, [string]$Detail = '') {
    $script:Results.Add([pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail })
    if ($Ok) {
        $script:Pass++
        Say ('  [通过] ' + $Name) 'Green'
    } else {
        $script:Fail++
        $script:FailNames.Add($Name)
        Say ('  [失败] ' + $Name) 'Red'
        if ($Detail) { Say ('         ' + $Detail) 'DarkGray' }
    }
}

function Section([string]$Title) {
    Say ''
    Say ('=== ' + $Title + ' ===') 'Cyan'
}

# ---- 在 PS 5.1 子进程中执行被测脚本 ----
# 注意：switch 参数绝不能用数组 splatting（@('-Json')）传，PS 会静默忽略，
# 表现为「脚本跑成功但输出的是人读格式」。必须走哈希表 splatting。
function RunScript([string]$Name, [hashtable]$Params = @{}) {
    $p = Join-Path $DeployDir $Name
    $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $p)
    foreach ($k in @($Params.Keys)) {
        $v = $Params[$k]
        if ($v -is [bool] -or $v -is [switch]) {
            if ($v) { $argList += ('-' + $k) }
        } else {
            $argList += ('-' + $k)
            $argList += [string]$v
        }
    }
    $code = -1
    $out  = ''
    try {
        $out  = (@(& $PS51 @argList 2>&1) | Out-String).Trim()
        $code = $LASTEXITCODE
        if ($null -eq $code) { $code = 0 }
    } catch {
        $out  = 'EXCEPTION: ' + $_.Exception.Message
        $code = -1
    }
    return @{ Code = $code; Out = $out }
}

# ---- 进程认领：与产品脚本同一套判据 ----
$script:DeployRoot = $DeployDir.TrimEnd('\')

function Test-InDeploy([object]$Proc) {
    if ($null -eq $Proc) { return $false }
    if ($Proc.ExecutablePath -and
        $Proc.ExecutablePath.StartsWith($script:DeployRoot, [StringComparison]::OrdinalIgnoreCase)) {
        return $true
    }
    # SYSTEM 进程常常取不到 ExecutablePath，退回命令行匹配
    if ($Proc.CommandLine -and
        $Proc.CommandLine.IndexOf($script:DeployRoot, [StringComparison]::OrdinalIgnoreCase) -ge 0) {
        return $true
    }
    return $false
}

function Get-AllFrpc {
    return @(Get-CimInstance Win32_Process -Filter "Name='frpc.exe'" -ErrorAction SilentlyContinue)
}
function Get-DeployedProc {
    return @(Get-AllFrpc | Where-Object { Test-InDeploy $_ })
}
function Get-BystanderProc {
    return @(Get-AllFrpc | Where-Object { -not (Test-InDeploy $_) })
}
function Get-ProcIds([object[]]$Procs) {
    return @(@($Procs) | Where-Object { $_ } | ForEach-Object { [int]$_.ProcessId })
}
function Describe-Pids([object[]]$Ids) {
    $a = @($Ids)
    if ($a.Count -eq 0) { return '无' }
    return ($a -join ', ')
}

$taskName = ''
$bystanderIds = @()
$interrupted = $false

# 主体包在 try 中：任一步骤抛错时，已执行的用例结果仍要出现在汇总里。
try {

Section '0. 准备'
$pr = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Say '本测试需要管理员权限（要创建 SYSTEM 计划任务）。' 'Red'
    throw 'not-admin'
}
Result '当前会话具备管理员权限' $true ''
Say ('  被测脚本宿主: ' + $PS51) 'DarkGray'

Copy-Item $SourcePkg $DeployDir -Recurse -Force
Result '部署包已解压到独立目录' (Test-Path (Join-Path $DeployDir 'frpc.exe')) ('' + $DeployDir)

# 双击入口与提权开关的静态契约。它们错了不会报任何错，只会让「双击安装」
# 这条路哑掉 —— 现场反馈是一句「双击了没反应」，从现象根本查不回来。
Result '部署目录含双击入口（一键安装.bat）' (Test-Path (Join-Path $DeployDir '一键安装.bat')) ''
$instText = [System.IO.File]::ReadAllText((Join-Path $DeployDir 'install.ps1'))
Result 'install.ps1 声明了 -Elevate / -Elevated / -Pause' ($instText -match '\$Elevate' -and $instText -match '\$Elevated' -and $instText -match '\$Pause') ''
# 守的是「自动化不挂死」：-Pause 若不做输入重定向判断，双击场景是舒服了，
# 但任何把标准输入接走的调用方都会永久卡在 Read-Host 上。
Result 'install.ps1 用 IsInputRedirected 保护 -Pause（自动化调用不挂死）' ($instText -match 'IsInputRedirected') ''

$bystanders   = @(Get-BystanderProc)
$bystanderIds = @(Get-ProcIds $bystanders)
Say ('  对照组：机器上其它 frpc 实例 ' + $bystanders.Count + ' 个（PID ' + (Describe-Pids $bystanderIds) + '）') 'DarkGray'

# 测试前就存在的同名任务（理论上是残留）
$preTask = @(Get-ScheduledTask -TaskName 'SinyeFRP-frpc-*' -ErrorAction SilentlyContinue)
if ($preTask.Count) {
    Say ('  警告：测试前已存在同名任务 ' + ($preTask.TaskName -join ', ') + '，将在测试结束时清理') 'Yellow'
}

Section '1. 安装（F13）'
$r = RunScript 'install.ps1'
Result 'install.ps1 退出码为 0' ($r.Code -eq 0) ('退出码 ' + $r.Code + ' | ' + $r.Out)
Say ('  ↳ ' + (($r.Out -split "`r?`n") -join ' / ')) 'DarkGray'
Start-Sleep -Seconds 3

$st = RunScript 'status.ps1' @{ Json = $true }
$status = $null
try { $status = $st.Out | ConvertFrom-Json } catch { }
Result 'status.ps1 -Json 输出可被解析' ($null -ne $status) ('退出码 ' + $st.Code + ' | ' + $st.Out)

if ($status) {
    $taskName = [string]$status.taskName
    Result '实例已被安装'      ($status.installed -eq $true) ('installed=' + $status.installed)
    Result '实例处于运行中'    ($status.running -eq $true)   ('running=' + $status.running + ' pid=' + ($status.pids -join ','))
    Result 'status 退出码为 0' ($st.Code -eq 0)             ('退出码 ' + $st.Code)

    if ($taskName) {
        $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Result '计划任务确实存在' ($null -ne $task) ('taskName=' + $taskName)
        if ($task) {
            Result '计划任务以 SYSTEM 身份运行' ($task.Principal.UserId -eq 'SYSTEM') ('UserId=' + $task.Principal.UserId)

            $trigNames = @()
            foreach ($t in @($task.Triggers)) {
                if ($t.CimClass -and $t.CimClass.CimClassName) { $trigNames += $t.CimClass.CimClassName }
                elseif ($t.PSObject.TypeNames.Count -gt 0)     { $trigNames += $t.PSObject.TypeNames[0] }
            }
            Result '计划任务触发器为开机启动' ((@($trigNames) -join ',') -match 'Boot') ('触发器: ' + ($trigNames -join ','))

            Result '计划任务工作目录为部署目录' ($task.Actions[0].WorkingDirectory -eq $DeployDir) ('WorkingDirectory=' + $task.Actions[0].WorkingDirectory)
            Result '计划任务可执行文件为本目录 frpc.exe' `
                ($task.Actions[0].Execute -eq (Join-Path $DeployDir 'frpc.exe')) ('Execute=' + $task.Actions[0].Execute)
            Result '计划任务含失败重启策略' ($task.Settings.RestartCount -ge 1) ('RestartCount=' + $task.Settings.RestartCount)
            Result '计划任务无执行时长上限' (([string]$task.Settings.ExecutionTimeLimit) -match '^PT0S$|^$') ('ExecutionTimeLimit=' + $task.Settings.ExecutionTimeLimit)
        }
    } else {
        Result 'status 返回了非空 taskName' $false 'taskName 为空'
    }
}

$proc = @(Get-DeployedProc)
Result 'frpc 进程实际已拉起' ($proc.Count -gt 0) ('进程数 ' + $proc.Count + '，PID ' + (Describe-Pids (Get-ProcIds $proc)))
if ($proc.Count -gt 0) {
    Say ('  ↳ 部署目录实例 PID: ' + (Describe-Pids (Get-ProcIds $proc))) 'DarkGray'
}

Section '2. F14 核心：不误杀其它实例'
$afterIds = @(Get-ProcIds @(Get-BystanderProc))
$lost = @($bystanderIds | Where-Object { $afterIds -notcontains $_ })
Result '安装后其它 frp 实例仍存活' ($lost.Count -eq 0) ('被动到的 PID: ' + (Describe-Pids $lost))

Section '3. 幂等：重复安装不产生副本'
$r2 = RunScript 'install.ps1'
Result '重复安装退出码为 0' ($r2.Code -eq 0) ('退出码 ' + $r2.Code)
$tasks = @(Get-ScheduledTask -TaskName 'SinyeFRP-frpc-*' -ErrorAction SilentlyContinue)
Result '计划任务仍只有一个（无副本）' ($tasks.Count -eq 1) ('任务数 ' + $tasks.Count + ': ' + ($tasks.TaskName -join ', '))
$proc2 = @(Get-DeployedProc)
Result '重复安装后仍只有本目录一个实例' ($proc2.Count -eq 1) ('进程数 ' + $proc2.Count + '，PID ' + (Describe-Pids (Get-ProcIds $proc2)))
$idemIds = @(Get-ProcIds @(Get-BystanderProc))
$lostIdem = @($bystanderIds | Where-Object { $idemIds -notcontains $_ })
Result '重复安装后其它 frp 实例仍存活' ($lostIdem.Count -eq 0) ('被动到的 PID: ' + (Describe-Pids $lostIdem))

Section '3b. 双击入口与 -Elevate（已提权时不得再弹 UAC）'
# 双击入口传给 install.ps1 的就是 -Elevate。这里在**已经提权**的进程里再跑一遍，
# 验的是两件事：
#   · -Elevate 绝不能因为「想提权」而再弹一次 UAC。本进程已是管理员，
#     它必须直接干活。判据写反的话真实症状是「双击后弹一次 UAC，
#     装完又弹一次」，而测试环境里则会直接挂死等一个没人点的弹框。
#   · 多了这个开关，幂等性不许破：任务与进程依旧各只有一个。
$r3 = RunScript 'install.ps1' @{ Elevate = $true }
Result 'install.ps1 -Elevate 退出码为 0（已提权时应直接安装，不弹 UAC）' ($r3.Code -eq 0) ('退出码 ' + $r3.Code + ' | ' + $r3.Out)
$tasks3 = @(Get-ScheduledTask -TaskName 'SinyeFRP-frpc-*' -ErrorAction SilentlyContinue)
Result '-Elevate 重复安装后计划任务仍只有一个' ($tasks3.Count -eq 1) ('任务数 ' + $tasks3.Count + ': ' + ($tasks3.TaskName -join ', '))
$proc3 = @(Get-DeployedProc)
Result '-Elevate 重复安装后仍只有本目录一个实例' ($proc3.Count -eq 1) ('进程数 ' + $proc3.Count + '，PID ' + (Describe-Pids (Get-ProcIds $proc3)))

Section '4. 停止与启动（F14）'
$r = RunScript 'stop.ps1'
Result 'stop.ps1 退出码为 0' ($r.Code -eq 0) ('退出码 ' + $r.Code + ' | ' + $r.Out)
Start-Sleep -Seconds 2
$procStop = @(Get-DeployedProc)
Result '进程已停止' ($procStop.Count -eq 0) ('残留进程数 ' + $procStop.Count + '，PID ' + (Describe-Pids (Get-ProcIds $procStop)))
$afterStopIds = @(Get-ProcIds @(Get-BystanderProc))
$lostAfterStop = @($bystanderIds | Where-Object { $afterStopIds -notcontains $_ })
Result '停止后其它 frp 实例仍存活' ($lostAfterStop.Count -eq 0) ('被动到的 PID: ' + (Describe-Pids $lostAfterStop))

$st2 = RunScript 'status.ps1' @{ Json = $true }
$s2 = $null; try { $s2 = $st2.Out | ConvertFrom-Json } catch { }
Result '未运行时 status 退出码为 1' ($st2.Code -eq 1) ('退出码 ' + $st2.Code)
if ($s2) { Result 'status 报告已安装但未运行' (($s2.installed -eq $true) -and ($s2.running -eq $false)) ('installed=' + $s2.installed + ' running=' + $s2.running) }

$r = RunScript 'start.ps1'
Result 'start.ps1 退出码为 0' ($r.Code -eq 0) ('退出码 ' + $r.Code + ' | ' + $r.Out)
Start-Sleep -Seconds 3
$procStart = @(Get-DeployedProc)
Result '进程已重新启动' ($procStart.Count -gt 0) ('进程数 ' + $procStart.Count + '，PID ' + (Describe-Pids (Get-ProcIds $procStart)))

$r = RunScript 'restart.ps1'
Result 'restart.ps1 退出码为 0' ($r.Code -eq 0) ('退出码 ' + $r.Code + ' | ' + $r.Out)
Start-Sleep -Seconds 3
$procRestart = @(Get-DeployedProc)
Result '重启后进程存在' ($procRestart.Count -gt 0) ('进程数 ' + $procRestart.Count + '，PID ' + (Describe-Pids (Get-ProcIds $procRestart)))

Section '5. 卸载（默认保留数据）'
$r = RunScript 'uninstall.ps1'
Result 'uninstall.ps1 退出码为 0' ($r.Code -eq 0) ('退出码 ' + $r.Code + ' | ' + $r.Out)
Start-Sleep -Seconds 2
$procUn = @(Get-DeployedProc)
Result '进程已停止' ($procUn.Count -eq 0) ('残留进程数 ' + $procUn.Count + '，PID ' + (Describe-Pids (Get-ProcIds $procUn)))

if ($taskName) { $gone = -not (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) }
else { $gone = (@(Get-ScheduledTask -TaskName 'SinyeFRP-frpc-*' -ErrorAction SilentlyContinue)).Count -eq 0 }
Result '计划任务已删除' $gone ''

Result '默认保留配置文件' (Test-Path (Join-Path $DeployDir 'frpc.toml')) ''
Result '默认保留主程序'   (Test-Path (Join-Path $DeployDir 'frpc.exe')) ''

$st3 = RunScript 'status.ps1' @{ Json = $true }
$s3 = $null; try { $s3 = $st3.Out | ConvertFrom-Json } catch { }
Result '未安装时 status 退出码为 2' ($st3.Code -eq 2) ('退出码 ' + $st3.Code)
if ($s3) { Result 'status 正确报告未安装' ($s3.installed -eq $false) ('installed=' + $s3.installed) }

Section '6. 对照收尾'
$finalIds = @(Get-ProcIds @(Get-BystanderProc))
$lostFinal = @($bystanderIds | Where-Object { $finalIds -notcontains $_ })
Result '全流程结束后其它 frp 实例仍存活' ($lostFinal.Count -eq 0) ('被动到的 PID: ' + (Describe-Pids $lostFinal))

} catch {
    if ($_.Exception.Message -ne 'not-admin') {
        $interrupted = $true
        Say ''
        Say ('[中断] ' + $_.Exception.Message) 'Red'
        if ($_.InvocationInfo) { Say ('        ' + [string]$_.InvocationInfo.PositionMessage) 'DarkGray' }
        $script:Fail++
        $script:FailNames.Add('执行中断: ' + $_.Exception.Message)
        $script:Results.Add([pscustomobject]@{ name = '执行中断'; ok = $false; detail = $_.Exception.Message })
    }
} finally {
    # 兜底清理：测试中断时绝不在机器上留下 SYSTEM 计划任务
    try {
        foreach ($t in @(Get-ScheduledTask -TaskName 'SinyeFRP-frpc-*' -ErrorAction SilentlyContinue)) {
            Stop-ScheduledTask -TaskName $t.TaskName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $t.TaskName -Confirm:$false -ErrorAction SilentlyContinue
            Say ('  [清理] 已兜底移除残留计划任务 ' + $t.TaskName) 'Yellow'
        }
        Start-Sleep -Seconds 1
        foreach ($p in @(Get-DeployedProc)) {
            Stop-Process -Id ([int]$p.ProcessId) -Force -ErrorAction SilentlyContinue
            Say ('  [清理] 已兜底停止残留进程 PID ' + $p.ProcessId) 'Yellow'
        }
    } catch { }
}

Section '汇总'
$total = $script:Pass + $script:Fail
Say ('  通过 ' + $script:Pass + ' / ' + $total) $(if ($script:Fail -eq 0) { 'Green' } else { 'Red' })
Say ('  部署目录: ' + $DeployDir) 'DarkGray'
if ($script:Fail -gt 0) {
    Say '  失败项:' 'Red'
    foreach ($n in @($script:FailNames)) { Say ('    - ' + $n) 'Red' }
}

# ---- 结构化结果落盘 ----
# 注意：List[T] 必须用 .ToArray() 展开后再放进 [ordered]@{}。
# PS 5.1 中 [ordered]@{ x = @($list) } 会直接抛 "Argument types do not match"，
# 且该异常发生在本节，会让整个日志文件写不出来——只有 .ToArray() 是安全的。
$jsonPath = [System.IO.Path]::ChangeExtension($LogPath, '.json')
try {
    $summary = [ordered]@{
        pass        = $script:Pass
        fail        = $script:Fail
        total       = $total
        deployDir   = $DeployDir
        taskName    = $taskName
        bystanders  = $bystanderIds
        interrupted = $interrupted
        failures    = $script:FailNames.ToArray()
        results     = $script:Results.ToArray()
    }
    ($summary | ConvertTo-Json -Depth 6) | Set-Content -Path $jsonPath -Encoding UTF8
    Say ('  结果 JSON: ' + $jsonPath) 'DarkGray'
} catch {
    Say ('  [警告] 结果 JSON 写入失败: ' + $_.Exception.Message) 'Red'
}

Say ('  日志文件 : ' + $LogPath) 'DarkGray'
Save-Log

if ($script:Fail -gt 0) { exit 1 }
exit 0
