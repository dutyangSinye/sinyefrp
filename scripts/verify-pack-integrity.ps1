<#
.SYNOPSIS
    重建产物后的包完整性自检：PS 语法、行尾/BOM、SHA256SUMS 自洽、关键段内容。

.DESCRIPTION
    三件事一次做完，避免在长回归里才发现模板改错了：

    1. 用 PowerShell 5.1 的 Parser 对包内每个 .ps1 做语法自检。
       PS 5.1 与 7.x 的可解析集不同（本项目的模板刻意只用 5.1 语法），
       必须用 5.1 的解析器判，用错版本会得出反向结论。
    2. 核对每个文本文件的行尾与 BOM。包内文本必须是 UTF-8 BOM + CRLF：
       缺 BOM 会让 PS 5.1 按 ANSI 读中文；裸 LF 会在记事本另存时被改写，
       使运维「只是打开看了一眼」就触发哈希校验失败。
    3. 复算 SHA256SUMS.txt 里的每一项，确认包是自洽的 ——
       这一步失败说明生成端自己就把清单算错了，装到哪台机器都装不上。

    刻意不用 Add-Type：本环境的沙箱会拦截它（连同依赖它的
    System.IO.Compression.ZipFile 一起），改用 Expand-Archive，
    并把 $ProgressPreference 关掉，否则 52 MB 的解压进度条会把输出淹掉。

.EXAMPLE
    .\verify-pack-integrity.ps1
    .\verify-pack-integrity.ps1 -Zip D:\dist\platform.zip
#>
[CmdletBinding()]
param(
    [string]$Zip     = '',
    [string]$OutFile = ''
)

$ErrorActionPreference = 'Continue'
$ProgressPreference    = 'SilentlyContinue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

# 从脚本位置向上找仓库根（以 go.mod 为标记）：放在 scripts\ 还是 .workbuddy\test\ 下都能定位。
$Root = $PSScriptRoot
while ($Root -and -not (Test-Path (Join-Path $Root 'go.mod'))) {
    $parent = Split-Path -Parent $Root
    if (-not $parent -or $parent -eq $Root) { break }
    $Root = $parent
}
if (-not $Zip)     { $Zip     = Join-Path $Root '.workbuddy\f18\platform.zip' }
if (-not $OutFile) { $OutFile = Join-Path $Root '.workbuddy\_pack-integrity.txt' }

$lines = New-Object System.Collections.Generic.List[string]
function Add-Line([string]$t) { $lines.Add($t) }

Add-Line ('包文件   : ' + $Zip)
if (-not (Test-Path $Zip)) {
    Add-Line '[FATAL] 找不到包'
    [System.IO.File]::WriteAllText($OutFile, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($true)))
    exit 2
}
Add-Line ('包大小   : ' + (Get-Item $Zip).Length)

$work = Join-Path $Root ('.workbuddy\f18\_integ-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
if (Test-Path $work) { Remove-Item $work -Recurse -Force }
Expand-Archive -LiteralPath $Zip -DestinationPath $work -Force
Add-Line ('解压到   : ' + $work)

# ---- 1. PS 5.1 语法自检 ----
$script:bad = 0
Add-Line ''
Add-Line '--- 1. PowerShell 5.1 语法自检 ---'
foreach ($f in @(Get-ChildItem $work -Filter '*.ps1' | Sort-Object Name)) {
    $tokens = $null
    $errs   = $null
    [void][System.Management.Automation.Language.Parser]::ParseFile($f.FullName, [ref]$tokens, [ref]$errs)
    if ($errs.Count -gt 0) {
        $script:bad++
        Add-Line ('  [FAIL] ' + $f.Name)
        foreach ($e in $errs) {
            Add-Line ('         line ' + $e.Extent.StartLineNumber + ': ' + $e.Message)
        }
    } else {
        Add-Line ('  [ OK ] ' + $f.Name)
    }
}

# ---- 2. 行尾与 BOM ----
#
# BOM 的期望值按类型分三档，不能一刀切：
#   .ps1 / .txt（与平台 config.toml）必须有 BOM —— PS 5.1 会把无 BOM 的 UTF-8
#       当 ANSI 解码，中文提示整段乱码；
#   frpc.toml / frps.toml 绝对不能有 BOM —— FRP 的 TOML 解析器遇到 BOM 直接
#       报 invalid character at start of key，退出码 1，服务根本起不来；
#   .bat 也绝对不能有 BOM，且原因与上面两条都不同：cmd.exe 会把 BOM 当成命令
#       的一部分，双击后首行报「'ï»¿@echo' 不是内部或外部命令」，入口彻底失效
#       且不留任何日志。它与 .ps1 的要求恰好相反，所以必须单列一档。
# 行尾则一律 CRLF，只有 SHA256SUMS.txt 例外（它是被解析的，不给人改）。
Add-Line ''
Add-Line '--- 2. 行尾与编码 ---'
$expectBom = @{
    'config.toml'    = $true
    'frpc.toml'      = $false
    'frps.toml'      = $false
    'install.ps1'    = $true
    'uninstall.ps1'  = $true
    'start.ps1'      = $true
    'stop.ps1'       = $true
    'restart.ps1'    = $true
    'status.ps1'     = $true
    'README.txt'     = $true
    'SHA256SUMS.txt' = $false
    '一键安装.bat'   = $false
}
foreach ($n in @($expectBom.Keys | Sort-Object)) {
    $p = Join-Path $work $n
    if (-not (Test-Path $p)) { continue }
    $b = [System.IO.File]::ReadAllBytes($p)
    $hasBom = ($b.Length -ge 3 -and $b[0] -eq 0xEF -and $b[1] -eq 0xBB -and $b[2] -eq 0xBF)
    $crlf = 0; $lf = 0
    for ($i = 0; $i -lt $b.Length; $i++) {
        if ($b[$i] -eq 10) {
            if ($i -gt 0 -and $b[$i - 1] -eq 13) { $crlf++ } else { $lf++ }
        }
    }
    $ok = ($hasBom -eq $expectBom[$n])
    if ($n -ne 'SHA256SUMS.txt' -and $lf -gt 0) { $ok = $false }
    $bomText = '无BOM'; if ($hasBom) { $bomText = 'BOM' }
    $mark = '[ OK ]'
    if (-not $ok) { $script:bad++; $mark = '[FAIL]' }
    Add-Line ('  {0} {1,-16} {2,-6} CRLF={3} 裸LF={4}' -f $mark, $n, $bomText, $crlf, $lf)
}
if (-not (Test-Path (Join-Path $work 'frpc.toml'))) {
    Add-Line '  (本包是平台包，不含 frpc.toml / frps.toml；那两项由 pkg-test 覆盖)'
}

# ---- 3. SHA256SUMS 自洽 ----
Add-Line ''
Add-Line '--- 3. SHA256SUMS.txt 自洽性（生成端必须自己算对）---'
$expected = @{}
# -Encoding UTF8 不能省：清单是无 BOM 的 UTF-8，而包内有一个中文文件名
# （一键安装.bat）。PS 5.1 的 Get-Content 在无 BOM 时会按 ANSI 解码，
# 那一行随即变成乱码，接着「文件不存在」判假 —— 一个纯读取侧的编码问题，
# 却表现为「生成端漏了文件」。
foreach ($line in (Get-Content -LiteralPath (Join-Path $work 'SHA256SUMS.txt') -Encoding UTF8)) {
    if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$') { $expected[$matches[2]] = $matches[1].ToLowerInvariant() }
}
if ($expected.Count -eq 0) {
    $script:bad++
    Add-Line '  [FAIL] 清单为空，解析失败'
} else {
    foreach ($k in @($expected.Keys | Sort-Object)) {
        $p = Join-Path $work $k
        if (-not (Test-Path $p)) { $script:bad++; Add-Line ('  [FAIL] ' + $k + ' 文件不存在'); continue }
        $a = (Get-FileHash $p -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($a -eq $expected[$k]) {
            Add-Line ('  [ OK ] {0,-18} {1}' -f $k, $a.Substring(0, 16))
        } else {
            $script:bad++
            Add-Line ('  [FAIL] {0,-18} 期望 {1} 实际 {2}' -f $k, $expected[$k].Substring(0, 16), $a.Substring(0, 16))
        }
    }
}

# ---- 4. install.ps1 的两级判定与监听地址解析是否真的落进去了 ----
Add-Line ''
Add-Line '--- 4. 新行为是否进入包内脚本 ---'
$inst = [System.IO.File]::ReadAllText((Join-Path $work 'install.ps1'))
# 用 PSCustomObject 而不是嵌套数组：PowerShell 的 @( @('a', $true) ) 会被拍平成
# 两个元素，$c[0]/$c[1] 随即取到字符串的字符，断言会静默失真成「永远为真」。
$checks = @(
    [pscustomobject]@{ Name = '主程序单独判死（Fail 12）';        Ok = ($inst -match 'Fail 12') },
    [pscustomobject]@{ Name = '配置只警告不阻断';                 Ok = ($inst -match '不阻断安装') },
    [pscustomobject]@{ Name = '能解析 listen_addr';               Ok = ($inst -match 'function Get-ServiceAddr') },
    [pscustomobject]@{ Name = '通配地址翻译成可访问地址';         Ok = ($inst -match 'function Get-AccessURL') },
    [pscustomobject]@{ Name = '对外监听时给出安全提示';           Ok = ($inst -match 'function Test-PublicBind') },
    [pscustomobject]@{ Name = '不再出现旧的「文件可能已损坏」文案'; Ok = (-not ($inst -match '文件可能已损坏')) }
)
foreach ($c in $checks) {
    if ($c.Ok) { Add-Line ('  [ OK ] ' + $c.Name) } else { $script:bad++; Add-Line ('  [FAIL] ' + $c.Name) }
}

# ---- 5. 包内 config.toml 是否写明了可自由修改 ----
Add-Line ''
Add-Line '--- 5. 包内 config.toml 的自述 ---'
$cfg = Join-Path $work 'config.toml'
if (Test-Path $cfg) {
    $ctext = [System.IO.File]::ReadAllText($cfg)
    if ($ctext -match '只提示、不阻断') { Add-Line '  [ OK ] 已写明「config.toml 的校验只提示不阻断」' }
    else { $script:bad++; Add-Line '  [FAIL] 未写明配置文件可自由修改' }
    $inServer = $false
    foreach ($l in ($ctext -split "`r?`n")) {
        if ($l -match '^\s*\[(.+?)\]\s*$') { $inServer = ($matches[1].Trim() -eq 'server') }
        if ($inServer) { Add-Line ('    ' + $l) }
    }
}

# ---- 6. README 与 status.ps1 是否跟着一起变 ----
Add-Line ''
Add-Line '--- 6. 说明与状态输出的配套性 ---'
$rd = [System.IO.File]::ReadAllText((Join-Path $work 'README.txt'))
if ($rd -match '与出厂版本不一致') { Add-Line '  [ OK ] README 说明了「改配置不阻断安装」' }
else { $script:bad++; Add-Line '  [FAIL] README 未说明改配置的处理方式' }
$st = Join-Path $work 'status.ps1'
if (Test-Path $st) {
    $stext = [System.IO.File]::ReadAllText($st)
    if ($stext -match 'Get-AccessURL') { Add-Line '  [ OK ] status.ps1 用 Get-AccessURL 打印访问地址' }
    else { $script:bad++; Add-Line '  [FAIL] status.ps1 仍写死 127.0.0.1' }
}
if ($inst -match 'Get-AccessURL') { Add-Line '  [ OK ] install.ps1 用 Get-AccessURL 打印访问地址' }
else { $script:bad++; Add-Line '  [FAIL] install.ps1 仍写死 127.0.0.1' }

# ---- 7. 双击入口：当成一份会咬人的契约来验 ----
#
# BOM 与行尾在上一节已经验过，这里验的是「内容是否真的能干活」：
# 一旦它不再调用 install.ps1、或者漏传了 -Elevate / -Pause，双击这条路就废了 ——
# 而废掉的表现是「双击没反应」或「窗口一闪而过」，不会有任何报错、不留任何日志。
# 这类静默失效只能靠断言拦住。
Add-Line ''
Add-Line '--- 7. 双击安装入口 ---'
$batName = '一键安装.bat'
$batPath = Join-Path $work $batName
if (-not (Test-Path $batPath)) {
    $script:bad++
    Add-Line ('  [FAIL] 包内没有 ' + $batName)
} else {
    $batBytes = [System.IO.File]::ReadAllBytes($batPath)
    $batText  = [System.IO.File]::ReadAllText($batPath)
    $okAscii  = $true
    foreach ($b in $batBytes) { if ($b -gt 127) { $okAscii = $false; break } }
    $batChecks = @(
        [pscustomobject]@{ Name = '纯 ASCII（cmd 按 CP936 解析 .bat，中文会乱码）'; Ok = $okAscii },
        [pscustomobject]@{ Name = '调用 install.ps1';                              Ok = ($batText -match 'install\.ps1') },
        [pscustomobject]@{ Name = '带 -Elevate（非管理员时自动提权）';              Ok = ($batText -match '-Elevate') },
        [pscustomobject]@{ Name = '带 -Pause（装完停屏，窗口不闪退）';              Ok = ($batText -match '-Pause') },
        [pscustomobject]@{ Name = '用 %~dp0 定位（不依赖当前工作目录）';            Ok = ($batText -match '%~dp0') },
        [pscustomobject]@{ Name = '带 -ExecutionPolicy Bypass（绕过执行策略）';      Ok = ($batText -match 'ExecutionPolicy') },
        [pscustomobject]@{ Name = '只转发不实现逻辑（入口里不许出现 Register-ScheduledTask）'; Ok = (-not ($batText -match 'Register-ScheduledTask')) }
    )
    foreach ($c in $batChecks) {
        if ($c.Ok) { Add-Line ('  [ OK ] ' + $c.Name) } else { $script:bad++; Add-Line ('  [FAIL] ' + $c.Name) }
    }
}
if ($rd -match '一键安装') { Add-Line '  [ OK ] README 已把双击入口写成首选安装方式' }
else { $script:bad++; Add-Line '  [FAIL] README 未提及双击入口（现场人员就找不到它）' }
if ($inst -match '\$Elevate' -and $inst -match '\$Pause') {
    Add-Line '  [ OK ] install.ps1 已声明 -Elevate / -Pause'
} else {
    $script:bad++
    Add-Line '  [FAIL] install.ps1 缺少 -Elevate 或 -Pause'
}
if ($rd -match '乱码') { Add-Line '  [ OK ] README 已给出「文件名解出乱码」的处置办法' }
else { $script:bad++; Add-Line '  [FAIL] README 未交代中文入口名可能被解压工具解乱码' }

Add-Line ''
if ($script:bad -eq 0) {
    Add-Line '结论: 全部通过'
} else {
    Add-Line ('结论: ' + $script:bad + ' 项异常')
}
[System.IO.File]::WriteAllText($OutFile, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($true)))
Write-Output ('RESULT_BAD=' + $script:bad)
