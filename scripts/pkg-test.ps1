<#
.SYNOPSIS
    SinyeFRP 部署包生成端到端测试。

.DESCRIPTION
    覆盖 01-prd F09-F17：
      1. 打包元信息（FRP 版本 / 二进制哈希）
      2. 客户端与服务器表单默认值
      3. 配置预览（含密钥遮蔽校验）
      4. ZIP 生成与下载
      5. ZIP 内容清单完整性
      6. 生成的 TOML 能否被对应 FRP 二进制 verify 通过
      7. SHA256SUMS.txt 逐项校验
      8. 生成的生命周期脚本编码与语法
      9. 脚本是否真正避免「按进程名误杀」
     10. 审计记录

    为什么测试脚本要自己处理这些：
    部署包是本平台唯一的交付物，它的正确性无法靠单元测试覆盖——
    真正要回答的问题是「把这个 ZIP 解压到一台 Windows Server 上，
     它能不能装起来并跑起来」。所以这里坚持用真实的 ZIP + 真实的
     FRP 二进制做校验，而不是断言结构体字段。

.EXAMPLE
    .\scripts\pkg-test.ps1
#>
[CmdletBinding()]
param(
    [string]$BaseUrl   = '',
    [string]$AdminUser = 'pkgadmin',
    [string]$AdminPass = 'PkgTest_2026!x',
    [string]$FrpDir    = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# curl.exe 把响应按 UTF-8 字节流写到标准输出，而 Windows PowerShell 5.1 默认按
# 控制台代码页（简体中文环境为 GBK）解码这些字节，导致响应里的中文全部变成乱码，
# 使「warnings 是否含某句中文提示」这类断言恒为假——看起来像产品缺陷，实为测试环境问题。
# 这里显式指定 UTF-8 解码。
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding           = [System.Text.Encoding]::UTF8

$Root = Split-Path -Parent $PSScriptRoot
if (-not $BaseUrl) { $BaseUrl = 'http://127.0.0.1:7810' }
if (-not $FrpDir)  { $FrpDir  = Join-Path $Root 'frp' }

# 每次运行使用独立的时间戳子目录。
#
# 不清理旧目录是刻意的：递归删除容易被安全策略拦截而导致整轮测试开不了头，
# 而保留产物反而更有用——测试失败时可以直接翻看那次生成的 ZIP 与脚本。
$Work = Join-Path $Root ('.workbuddy\pkgtest\' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Force -Path $Work | Out-Null
$Jar = Join-Path $Work 'cookies.txt'

$script:Pass = 0
$script:Fail = 0
$script:FailNames = @()

function Result([string]$Name, [bool]$Ok, [string]$Detail = '') {
    if ($Ok) {
        $script:Pass++
        Write-Host ('  [通过] ' + $Name) -ForegroundColor Green
    } else {
        $script:Fail++
        $script:FailNames += $Name
        Write-Host ('  [失败] ' + $Name) -ForegroundColor Red
        if ($Detail) { Write-Host ('         ' + $Detail) -ForegroundColor DarkGray }
    }
}

function Section([string]$Title) {
    Write-Host ''
    Write-Host ('=== ' + $Title + ' ===') -ForegroundColor Cyan
}

# 发送请求：返回 @{ Code; Body; Json }
function Http {
    param(
        [string]$Method,
        [string]$Path,
        [string]$Body = $null,
        [switch]$NoAuth,
        [string]$OutFile = $null
    )
    $cargs = @('-s', '--noproxy', '*', '-X', $Method, '-c', $Jar)
    if (-not $NoAuth) { $cargs += @('-b', $Jar) }

    $bodyFile = $null
    if ($null -ne $Body) {
        $bodyFile = Join-Path $env:TEMP ('sinyefrp-pkg-' + [guid]::NewGuid().ToString('N') + '.json')
        [System.IO.File]::WriteAllText($bodyFile, $Body, [System.Text.UTF8Encoding]::new($false))
        $cargs += @('-H', 'Content-Type: application/json', '--data-binary', ('@' + $bodyFile))
    }
    $hdrFile = $null
    if ($OutFile) {
        $cargs += @('-o', $OutFile, '-w', '%{http_code}')
        # 下载类断言要看 Content-Disposition 里的文件名（Linux 包与 Windows 包若同名会互相覆盖）。
        # -o 之后响应体已落盘，只能把响应头另存一份再解析。
        $hdrFile = Join-Path $env:TEMP ('sinyefrp-pkg-h-' + [guid]::NewGuid().ToString('N') + '.txt')
        $cargs += @('-D', $hdrFile)
    } else {
        $cargs += @('-w', "`n%{http_code}")
    }
    $cargs += @($BaseUrl + $Path)

    $hdrText = $null
    try {
        $out = & curl.exe @cargs 2>&1
        if ($hdrFile -and (Test-Path $hdrFile)) {
            $hdrText = @(Get-Content -LiteralPath $hdrFile -Encoding UTF8 -ErrorAction SilentlyContinue)
        }
    }
    finally {
        if ($bodyFile -and (Test-Path $bodyFile)) { Remove-Item $bodyFile -Force -ErrorAction SilentlyContinue }
        if ($hdrFile  -and (Test-Path $hdrFile))  { Remove-Item $hdrFile  -Force -ErrorAction SilentlyContinue }
    }

    $text = ($out | Out-String)
    if ($OutFile) {
        $code = 0
        [void][int]::TryParse($text.Trim(), [ref]$code)
        $fname = ''
        foreach ($l in @($hdrText)) {
            # RFC 5987 形式优先（filename*=UTF-8''xxx），否则取普通 filename="xxx"
            if ($l -match "filename\*\s*=\s*UTF-8''([^;`"\r\n]+)") { $fname = [Uri]::UnescapeDataString($matches[1]) }
            elseif ($fname -eq '' -and $l -match 'filename\s*=\s*"?([^";\r\n]+)"?') { $fname = $matches[1] }
        }
        return @{ Code = $code; Body = ''; Json = $null; FileName = $fname }
    }

    $text = $text.TrimEnd("`r", "`n")
    $idx = $text.LastIndexOf("`n")
    $code = 0
    $payload = $text
    if ($idx -ge 0) {
        [void][int]::TryParse($text.Substring($idx + 1).Trim(), [ref]$code)
        $payload = $text.Substring(0, $idx)
    }
    $obj = $null
    if ($payload.Trim()) { try { $obj = $payload | ConvertFrom-Json } catch { } }
    return @{ Code = $code; Body = $payload.Trim(); Json = $obj }
}

function ToJson($obj) { return ($obj | ConvertTo-Json -Depth 12 -Compress) }

# 整个测试主体包在 try 中：任一步骤抛错时，已执行过的用例结果仍要在汇总里体现。
# 否则一个 null 引用就会让前面几十条通过记录一起消失，反而看不清真实进度。
try {

# ---------- 0. 前置：服务可用 ----------
Section '0. 前置检查'
$r = Http GET '/api/health'
Result '管理服务可访问' ($r.Code -eq 200) ('HTTP ' + $r.Code)
if ($r.Code -ne 200) {
    Write-Host '管理服务未启动，无法继续。' -ForegroundColor Red
    exit 1
}

# 初始化（若未初始化）
$r = Http GET '/api/setup/status' -NoAuth
if ($r.Json -and -not $r.Json.initialized) {
    $r = Http POST '/api/setup/admin' (ToJson @{ username = $AdminUser; password = $AdminPass }) -NoAuth
    Result '首次初始化创建管理员' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
    $r = Http POST '/api/auth/login' (ToJson @{ username = $AdminUser; password = $AdminPass }) -NoAuth
} else {
    $r = Http POST '/api/auth/login' (ToJson @{ username = $AdminUser; password = $AdminPass }) -NoAuth
}
Result '管理员登录成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)

# ---------- 1. 打包元信息 ----------
Section '1. 打包元信息（F15）'
$meta = (Http GET '/api/pkg/meta').Json
# 版本随内嵌二进制走：升级 FRP 时这一行必须同步改，否则「二进制换了、
# 断言还写着旧版本」会在这里红一次，正好提醒你闸门文案也要跟着改。
Result '返回 FRP 版本' ($meta.frp_version -eq '0.71.0') ('实际: ' + $meta.frp_version)
Result '客户端二进制哈希非空' ($meta.client_exe_sha256.Length -eq 64) ('实际: ' + $meta.client_exe_sha256)
Result '服务端二进制哈希非空' ($meta.server_exe_sha256.Length -eq 64)
# 6 种而不是 7 种：tcpmux 于 2026-09-25 移除（平台从未渲染它必需的
# multiplexer 字段，选它打包必被 frpc 拒）。见 internal/pack/spec.go。
Result '支持的代理类型为 6 种' ($meta.proxy_types.Count -eq 6) ('实际: ' + $meta.proxy_types.Count)
Result '代理类型清单不含 tcpmux' (-not ($meta.proxy_types -contains 'tcpmux')) ('实际: ' + ($meta.proxy_types -join ','))
Result '版本在正式支持范围内（无警告）' ($meta.version_warning -eq '') ('警告: ' + $meta.version_warning)
Result '支持范围下发为集合且含当前版本线' (($meta.supported_prefixes.Count -ge 2) -and ($meta.frp_version.StartsWith($meta.supported_prefixes[0]) -or $meta.frp_version.StartsWith($meta.supported_prefixes[1]))) ('实际: ' + ($meta.supported_prefixes -join ','))

# ---------- 2. 默认值 ----------
Section '2. 表单默认值（F09/F11）'
$cd = (Http GET '/api/pkg/client/defaults').Json
Result '客户端默认值返回随机 clientID' ($cd.client_id -like 'sf-*') ('实际: ' + $cd.client_id)
Result '客户端默认值返回随机 token' ($cd.token.Length -ge 40) ('长度: ' + $cd.token.Length)
Result '两次请求的 clientID 不同（未被缓存）' `
    ((Http GET '/api/pkg/client/defaults').Json.client_id -ne $cd.client_id)

$sd = (Http GET '/api/pkg/server/defaults').Json
Result '服务器默认 Dashboard 绑定环回' ($sd.dashboard_addr -eq '127.0.0.1') ('实际: ' + $sd.dashboard_addr)
Result '服务器默认生成强密码（>=20 位）' ($sd.dashboard_password.Length -ge 20) ('长度: ' + $sd.dashboard_password.Length)

# ---------- 3. 客户端配置预览 ----------
Section '3. 客户端配置预览与校验（F09/F15/F17）'
$clientSpec = @{
    server_addr   = '127.0.0.1'
    server_port   = 7000
    name          = '测试设备-甲'
    client_id     = $cd.client_id
    token         = $cd.token
    log_level     = 'info'
    log_max_days  = 7
    enable_tls    = $false
    proxies       = @(
        @{ name = 'web-tcp'; type = 'tcp'; local_ip = '127.0.0.1'; local_port = 8080; remote_port = 18080 },
        @{ name = 'rdp-tcp'; type = 'tcp'; local_ip = '127.0.0.1'; local_port = 3389; remote_port = 18081;
           use_encryption = $true; use_compression = $true }
    )
}
$pv = (Http POST '/api/pkg/client/preview' (ToJson $clientSpec)).Json
Result '预览返回 frpc.toml 内容' ($pv.toml -like '*serverAddr*') ''
Result '预览通过 FRP 校验（frpc verify -c 接受）' ($pv.frp_version -eq '0.71.0') ('版本: ' + $pv.frp_version)
Result '预览中的 token 已遮蔽' (-not $pv.toml.Contains($cd.token)) ''
Result '预览中的遮蔽值可见' ($pv.toml -match 'token\s*=\s*"[^"]*\*+') ''
Result '预览给出未启用 TLS 的提醒' (($pv.warnings -join ' ') -like '*传输加密*') ''
Result '预览列出包内文件 11 项' ($pv.files.Count -eq 11) ('实际: ' + $pv.files.Count)

# 非法配置必须被拒
$badSpec = $clientSpec.Clone()
$badSpec.server_port = 70000
$r = Http POST '/api/pkg/client/preview' (ToJson $badSpec)
Result '端口越界被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code)

$badSpec2 = $clientSpec.Clone()
$badSpec2.client_id = '有中文的标识'
$r = Http POST '/api/pkg/client/preview' (ToJson $badSpec2)
Result '非法 clientID 被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code)

# ---------- 4. 客户端包生成 ----------
Section '4. 客户端部署包（F10）'
$zipClient = Join-Path $Work 'client.zip'
$r = Http POST '/api/pkg/client' (ToJson $clientSpec) -OutFile $zipClient
Result '下载返回 200' ($r.Code -eq 200) ('HTTP ' + $r.Code)
Result 'ZIP 文件已生成' (Test-Path $zipClient) ''
$zipSize = (Get-Item $zipClient).Length
Write-Host ('         ZIP 体积: ' + [math]::Round($zipSize / 1MB, 2) + ' MB') -ForegroundColor DarkGray

$exClient = Join-Path $Work 'client'
Expand-Archive -Path $zipClient -DestinationPath $exClient -Force

# '一键安装.bat' 这一项顺带验了另一件事：它是包内唯一一个非 ASCII 文件名。
# Expand-Archive 走 .NET 的 ZipArchive，只有条目的 UTF-8 标志位（general purpose
# bit 11）被正确写出时，名字才会解成这个样子；写错的话它会解成乱码，
# 于是这一项失败 —— 而不是变成「少一个文件」那种含糊症状。
$expected = @('frpc.exe','frpc.toml','一键安装.bat','install.ps1','uninstall.ps1','start.ps1','stop.ps1',
              'restart.ps1','status.ps1','README.txt','SHA256SUMS.txt')
$missing = @()
foreach ($f in $expected) { if (-not (Test-Path (Join-Path $exClient $f))) { $missing += $f } }
Result 'ZIP 含全部 11 个必需文件' ($missing.Count -eq 0) ('缺少: ' + ($missing -join ', '))

# 双击入口的内容契约。少了任何一项，这条路就是哑的：不报错、不生效，
# 现场只会反馈「双击了没反应」。断言必须落在内容上，不能只看文件在不在。
$batPath = Join-Path $exClient '一键安装.bat'
$batText = [System.IO.File]::ReadAllText($batPath)
$batRaw  = [System.IO.File]::ReadAllBytes($batPath)
$batAscii = $true
foreach ($b in $batRaw) { if ($b -gt 127) { $batAscii = $false; break } }
Result '双击入口为纯 ASCII（cmd 按 CP936 解析 .bat，中文会乱码）' $batAscii ''
$batBom = ($batRaw.Length -ge 3 -and $batRaw[0] -eq 0xEF -and $batRaw[1] -eq 0xBB -and $batRaw[2] -eq 0xBF)
Result '双击入口不带 BOM（cmd 会把 BOM 当命令的一部分）' (-not $batBom) ('前 3 字节: ' + (($batRaw[0..2] | ForEach-Object { $_.ToString('X2') }) -join ' '))
Result '双击入口调用 install.ps1 且带 -Elevate -Pause' ($batText -match 'install\.ps1' -and $batText -match '-Elevate' -and $batText -match '-Pause') ''

# 生成的 TOML 必须能被真实 frpc 校验
$frpc = Join-Path $FrpDir 'frpc.exe'
$verifyOut = (& $frpc verify -c (Join-Path $exClient 'frpc.toml') 2>&1) | Out-String
Result '生成的 frpc.toml 通过 frpc verify -c' ($LASTEXITCODE -eq 0) $verifyOut.Trim()

# 最关键的一条：预览接口遮蔽密钥是为了防肩窥，但 ZIP 里必须是明文，
# 否则客户端装上去根本连不上。这两件事很容易在实现里被一起做错。
$deployedToml = Get-Content (Join-Path $exClient 'frpc.toml') -Raw
Result 'ZIP 内的 frpc.toml 含明文 token（能真正连上服务器）' ($deployedToml.Contains($cd.token)) ''
Result 'ZIP 内的 frpc.toml 含 clientID' ($deployedToml.Contains($cd.client_id)) ''

# SHA256SUMS 逐项校验
#
# 必须显式 -Encoding UTF8：这个清单是**无 BOM** 的 UTF-8（加 BOM 会让
# sha256sum -c 这类标准工具读不了），而包内现在有一个中文文件名
# （一键安装.bat）。PS 5.1 的 Get-Content 在无 BOM 时按 ANSI 代码页解码，
# 会把那一行读成乱码，随后「文件是否存在」必然为假 —— 症状是
# 「SHA256SUMS.txt 全部匹配」失败并报 `ä¸€é”®å®‰è£….bat(缺失)`，
# 看起来像生成端漏了一个文件，实际是读的人解错了编码。
$sumLines = Get-Content -LiteralPath (Join-Path $exClient 'SHA256SUMS.txt') -Encoding UTF8
$sumBad = @()
foreach ($line in $sumLines) {
    if ($line -match '^([0-9a-f]{64})\s+(.+)$') {
        $want = $matches[1]
        $fname = $matches[2]
        $fp = Join-Path $exClient $fname
        if (-not (Test-Path $fp)) { $sumBad += ($fname + '(缺失)'); continue }
        $got = (Get-FileHash $fp -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($got -ne $want) { $sumBad += $fname }
    }
}
Result 'SHA256SUMS.txt 全部匹配' ($sumBad.Count -eq 0) ('不匹配: ' + ($sumBad -join ', '))
Result 'SHA256SUMS 覆盖 10 个文件（不含自身）' ($sumLines.Count -eq 10) ('实际: ' + $sumLines.Count)
# 上面那条「全部匹配」有个盲区：正则只认「64 位小写十六进制 + 文件名」的行，
# 不匹配的行会被**静默跳过**。所以中文名那一行必须单独确认真的被解析到了 ——
# 否则「全部匹配」可能只是「压根没读到」。
$batSum = @($sumLines | Where-Object { $_ -match '一键安装\.bat' })
Result 'SHA256SUMS 覆盖双击入口（中文文件名未被静默跳过）' ($batSum.Count -eq 1) ('含 .bat 的行: ' + (($sumLines | Where-Object { $_ -match '\.bat' }) -join ' | '))

# ---------- 5. 生命周期脚本质量 ----------
Section '5. 生命周期脚本（F13/F14/§8.3）'
$synBad = @()
foreach ($name in @('install.ps1','uninstall.ps1','start.ps1','stop.ps1','restart.ps1','status.ps1')) {
    $p = Join-Path $exClient $name
    $errs = $null
    [void][System.Management.Automation.Language.Parser]::ParseFile($p, [ref]$null, [ref]$errs)
    if ($errs.Count) { $synBad += ($name + ': ' + ($errs[0].Message)) }
}
Result '六个脚本语法全部合法' ($synBad.Count -eq 0) ($synBad -join '; ')

$bomBad = @()
foreach ($name in @('install.ps1','status.ps1')) {
    $b = [System.IO.File]::ReadAllBytes((Join-Path $exClient $name))
    if (-not ($b.Length -ge 3 -and $b[0] -eq 0xEF -and $b[1] -eq 0xBB -and $b[2] -eq 0xBF)) { $bomBad += $name }
}
Result '脚本保留 UTF-8 BOM（PS 5.1 中文不乱码）' ($bomBad.Count -eq 0) ($bomBad -join ', ')

$installTxt = Get-Content (Join-Path $exClient 'install.ps1') -Raw
$uninstallTxt = Get-Content (Join-Path $exClient 'uninstall.ps1') -Raw
$statusTxt = Get-Content (Join-Path $exClient 'status.ps1') -Raw
Result 'install.ps1 注册 SYSTEM 开机自启' ($installTxt -match 'New-ScheduledTaskPrincipal' -and $installTxt -match 'SYSTEM' -and $installTxt -match '-AtStartup') ''
Result 'install.ps1 使用 -Force 保证幂等' ($installTxt -match 'Register-ScheduledTask' -and $installTxt -match '-Force') ''
Result 'install.ps1 含计划任务失败重启策略' ($installTxt -match 'RestartCount') ''
Result '脚本按可执行文件路径精确匹配进程' ($installTxt -match 'ExecutablePath' -and $statusTxt -match 'ExecutablePath') ''
Result '所有脚本均不含 taskkill 按名击杀' `
    (-not (($installTxt + $uninstallTxt + $statusTxt) -match 'taskkill\s+/[fF]\s*/im')) ''
Result 'uninstall 默认保留数据、需显式 -RemoveData' ($uninstallTxt -match 'RemoveData') ''
Result 'status.ps1 支持 -Json 供监控采集' ($statusTxt -match 'ConvertTo-Json') ''

# 脚本里写入的配置绝对路径必须与当前解压目录一致
Result 'install.ps1 使用 $PSScriptRoot 定位部署目录' ($installTxt -match '\$PSScriptRoot') ''

$readme = Get-Content (Join-Path $exClient 'README.txt') -Raw
Result 'README 含「一台设备一个包」警示' ($readme -match '一台设备一个包') ''
Result 'README 含执行策略解决办法' ($readme -match 'Set-ExecutionPolicy') ''
Result 'README 含退出码说明' ($readme -match '退出码') ''

# ---------- 6. 服务器包 ----------
Section '6. 服务器部署包（F11/F12）'
$serverSpec = @{
    bind_addr          = '0.0.0.0'
    bind_port          = 7000
    token              = $sd.token
    dashboard_addr     = '127.0.0.1'
    dashboard_port     = 7500
    dashboard_user     = 'admin'
    dashboard_password = $sd.dashboard_password
    log_level          = 'info'
    log_max_days       = 7
    allow_ports        = @(@{ start = 10000; end = 20000 })
}
$spv = (Http POST '/api/pkg/server/preview' (ToJson $serverSpec)).Json
Result '服务器预览通过 FRP 校验' ($spv.frp_version -eq '0.71.0') ('版本: ' + $spv.frp_version)
Result '服务器预览遮蔽 Dashboard 密码' (-not $spv.toml.Contains($sd.dashboard_password)) ''
Result '服务器预览遮蔽 token' (-not $spv.toml.Contains($sd.token)) ''

$zipServer = Join-Path $Work 'server.zip'
$r = Http POST '/api/pkg/server' (ToJson $serverSpec) -OutFile $zipServer
Result '服务器包下载返回 200' ($r.Code -eq 200) ('HTTP ' + $r.Code)

$exServer = Join-Path $Work 'server'
Expand-Archive -Path $zipServer -DestinationPath $exServer -Force
$missingS = @()
foreach ($f in @('frps.exe','frps.toml','一键安装.bat','install.ps1','uninstall.ps1','start.ps1','stop.ps1',
                 'restart.ps1','status.ps1','README.txt','SHA256SUMS.txt')) {
    if (-not (Test-Path (Join-Path $exServer $f))) { $missingS += $f }
}
Result '服务器 ZIP 含全部 11 个必需文件' ($missingS.Count -eq 0) ('缺少: ' + ($missingS -join ', '))

$frps = Join-Path $FrpDir 'frps.exe'
$verifyOutS = (& $frps verify -c (Join-Path $exServer 'frps.toml') 2>&1) | Out-String
Result '生成的 frps.toml 通过 frps verify -c' ($LASTEXITCODE -eq 0) $verifyOutS.Trim()

# 服务器包同理：ZIP 内必须是明文，管理台才拿得到凭据去回填节点配置。
$serverToml = Get-Content (Join-Path $exServer 'frps.toml') -Raw
Result 'ZIP 内的 frps.toml 含明文 token' ($serverToml.Contains($sd.token)) ''
Result 'ZIP 内的 frps.toml 含明文 Dashboard 密码' ($serverToml.Contains($sd.dashboard_password)) ''

$serverReadme = Get-Content (Join-Path $exServer 'README.txt') -Raw
Result '服务器 README 含端口放行指引' ($serverReadme -match '需要放行的端口') ''
Result '服务器 README 提示不会自动改防火墙' ($serverReadme -match '不会自动修改防火墙') ''

# ---------- 7. 校验失败必须阻止下载 ----------
Section '7. 校验失败阻断（F15）'
$brokenServer = $serverSpec.Clone()
$brokenServer.bind_port = 7500   # 与 Dashboard 端口冲突
$r = Http POST '/api/pkg/server' (ToJson $brokenServer) -OutFile (Join-Path $Work 'should-not-exist.zip')
Result '端口冲突的服务器包被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code)

# ---------- 7.5 管理平台自身部署包 ----------
Section '7.5 管理平台自身部署包（F18）'

# 平台包与另外两个包的本质区别：它没有表单参数，内容与**当前这副部署**绑定。
# 因此这里的断言重点不是字段校验（那在另外两包里已覆盖），而是三方对得上：
# 元信息说的哈希 == 包里 exe 的哈希，预览给的清单 == 解压后的清单。
Result '元信息标记平台主程序可用' ($meta.platform_available -eq $true) ('原因: ' + $meta.platform_unavailable_reason)
Result '元信息含平台版本' ($meta.platform_version.Length -gt 0) ('实际: ' + $meta.platform_version)
Result '元信息含平台主程序哈希' ($meta.platform_exe_sha256.Length -eq 64) ('实际: ' + $meta.platform_exe_sha256)
Result '元信息给出平台主程序体积' ($meta.platform_exe_size -gt 1MB) ('实际: ' + $meta.platform_exe_size)

$ppv = (Http GET '/api/pkg/platform/preview').Json
Result '平台包预览可用' ($null -ne $ppv) ''
Result '平台包文件名形如 SinyeFRP-platform-<版本>.zip' ($ppv.file_name -like 'SinyeFRP-platform-*.zip') ('实际: ' + $ppv.file_name)
Result '平台包清单共 11 个文件（十个标准件 + 双击入口）' ($ppv.files.Count -eq 11) ('实际: ' + $ppv.files.Count)
Result '平台包清单含双击入口' (@($ppv.files) -contains '一键安装.bat') (($ppv.files) -join ', ')
foreach ($f in @('sinyefrp.exe', 'config.toml', 'install.ps1', 'uninstall.ps1',
                 'start.ps1', 'stop.ps1', 'restart.ps1', 'status.ps1',
                 'README.txt', 'SHA256SUMS.txt')) {
    Result ('平台包清单含 ' + $f) ($ppv.files -contains $f) ''
}
Result '平台包预览给出至少 3 条提醒' ($ppv.warnings.Count -ge 3) ('实际: ' + $ppv.warnings.Count)

# 平台配置里本来就没有密钥（节点密码走 DPAPI 存 SQLite），因此预览给的是全文。
# 但仍然要确认它确实不含任何密钥，否则「不遮蔽」这个决定就不成立了。
Result '预览的 config.toml 含 [server] 段' ($ppv.toml -match '\[server\]') ''
Result '预览的 config.toml 不含 token 赋值' (-not ($ppv.toml -match '(?m)^\s*token\s*=')) ''
Result '预览的 config.toml 的节点密码为空' ($ppv.toml -match '(?m)^password\s*=\s*""') ''

$zipPlatform = Join-Path $Work 'SinyeFRP-platform.zip'
$r = Http POST '/api/pkg/platform' $null -OutFile $zipPlatform
Result '平台包下载返回 200' ($r.Code -eq 200) ('HTTP ' + $r.Code)
Result '平台包文件已落盘' (Test-Path $zipPlatform) ''
if (Test-Path $zipPlatform) {
    Result '平台包体积合理（> 5 MB）' ((Get-Item $zipPlatform).Length -gt 5MB) ('实际: ' + (Get-Item $zipPlatform).Length)
}

if (Test-Path $zipPlatform) {
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $exPlatform = Join-Path $Work 'platform'
    if (Test-Path $exPlatform) { Remove-Item $exPlatform -Recurse -Force }
    [System.IO.Compression.ZipFile]::ExtractToDirectory($zipPlatform, $exPlatform)

    $pfiles = @(Get-ChildItem $exPlatform -File | Select-Object -ExpandProperty Name)
    Result '解压后文件数与预览一致' ($pfiles.Count -eq $ppv.files.Count) ('实际: ' + $pfiles.Count)
    Result '平台包不含 frpc.exe / frps.exe（它们内嵌在主程序里）' `
        ((-not ($pfiles -contains 'frpc.exe')) -and (-not ($pfiles -contains 'frps.exe'))) ''

    # 包内主程序哈希必须等于元信息报的那个：这正是「包里的就是正在跑的那个」这句话的含义。
    $exeHash = (Get-FileHash (Join-Path $exPlatform 'sinyefrp.exe') -Algorithm SHA256).Hash.ToLowerInvariant()
    Result '包内主程序哈希与元信息一致' ($exeHash -eq $meta.platform_exe_sha256.ToLowerInvariant()) ('实际: ' + $exeHash)

    # SHA256SUMS.txt 逐项校验：任何一项对不上都说明包在生成或落盘环节出了问题。
    #
    # 两处防呆，都是这一轮踩出来的：
    #   · `-Encoding UTF8` 不能省 —— 清单无 BOM，PS 5.1 的 Get-Content 会按 ANSI
    #     解码，中文文件名变乱码，拼出来的路径自然不存在；
    #   · Get-FileHash 遇到不存在的路径是**抛异常**而不是返回错误，异常会把整个
    #     套件掀掉。本轮就是这样静默少跑了 16 条断言，汇总只显示「通过 83/85」，
    #     失败项里只有一条，极易被当成小问题放过去。逐项捕获后，
    #     「文件缺失」表现为一条失败断言，测试不会提前死掉。
    $bad = @()
    foreach ($line in (Get-Content -LiteralPath (Join-Path $exPlatform 'SHA256SUMS.txt') -Encoding UTF8)) {
        if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$') {
            $want = $matches[1].ToLowerInvariant()
            $name = $matches[2]
            try {
                $got = (Get-FileHash -LiteralPath (Join-Path $exPlatform $name) -Algorithm SHA256).Hash.ToLowerInvariant()
                if ($want -ne $got) { $bad += $name }
            } catch {
                $bad += ($name + '(读不到)')
            }
        }
    }
    Result '平台包 SHA256SUMS.txt 逐项校验通过' ($bad.Count -eq 0) ($bad -join ', ')

    # 平台包与 frp 包共用同一套脚本模板，差异必须体现在参数化上，而不是漏改。
    $pInstall = Get-Content (Join-Path $exPlatform 'install.ps1') -Raw
    $pUninst  = Get-Content (Join-Path $exPlatform 'uninstall.ps1') -Raw
    $pStatus  = Get-Content (Join-Path $exPlatform 'status.ps1') -Raw
    $pReadme  = Get-Content (Join-Path $exPlatform 'README.txt') -Raw

    $pArgLine = @($pInstall -split "`n" | Where-Object { $_ -match 'New-ScheduledTaskAction' })
    Result '平台 install.ps1 以 -config 传配置参数' (($pArgLine -join '') -match "'-config `"") ('实际: ' + ($pArgLine -join '').Trim())
    $frpArgLine = @($installTxt -split "`n" | Where-Object { $_ -match 'New-ScheduledTaskAction' })
    Result 'frp install.ps1 仍以 -c 传配置参数（未被连带改坏）' (($frpArgLine -join '') -match "'-c `"") ''

    Result '平台 install.ps1 收紧 data/logs 目录 ACL' `
        (($pInstall -match 'icacls') -and ($pInstall -match '/inheritance:r') -and ($pInstall -match 'S-1-5-18')) ''
    Result '平台脚本运行期从 config.toml 解析端口' `
        (($pInstall -match 'Get-ServicePort') -and ($pStatus -match 'Get-ServicePort')) ''
    Result '平台 status.ps1 报告端口监听状态' ($pStatus -match 'portListening') ''
    Result '平台 uninstall.ps1 -RemoveData 覆盖 data 与 logs' `
        (($pUninst -match "'data'") -and ($pUninst -match "'logs'")) ''
    Result '平台脚本按可执行文件路径精确匹配进程（无 taskkill 按名击杀）' `
        (($pInstall -match 'ExecutablePath') -and (-not (($pInstall + $pUninst + $pStatus) -match 'taskkill\s+/[fF]\s*/im'))) ''
    Result '平台 status.ps1 支持 -Json 供监控采集' ($pStatus -match 'ConvertTo-Json') ''

    Result '平台 README 说明数据目录权限的必要性' ($pReadme -match 'NTFS') ''
    Result '平台 README 说明如何迁移已有数据' ($pReadme -match '迁移已有数据') ''
    Result '平台 README 警告不要监听 0.0.0.0' ($pReadme -match '0\.0\.0\.0') ''
    Result '平台 README 含 CLI 用法' ($pReadme -match 'pkg app') ''
    Result '平台 config.toml 监听环回地址' `
        ((Get-Content (Join-Path $exPlatform 'config.toml') -Raw) -match '(?m)^listen_addr\s*=\s*"127\.0\.0\.1"') ''
}

# ---------- 7b. Linux 客户端部署包 ----------
Section '7b. Linux 客户端部署包（目标平台）'

# 元信息必须下发可选平台：界面下拉据此生成，而「能选什么」只有后端知道
# （取决于这份平台 exe 里内嵌了哪些二进制）。
$metaLx = (Http GET '/api/pkg/meta').Json
$lxPlats = @($metaLx.client_platforms)
Result '元信息下发客户端可选平台' ($lxPlats.Count -ge 3) ($lxPlats -join ', ')
Result '可选平台含 linux-amd64 与 linux-arm64' `
    (($lxPlats -contains 'linux-amd64') -and ($lxPlats -contains 'linux-arm64')) ($lxPlats -join ', ')
Result '可选平台仍含 windows-amd64（存量默认未被挤掉）' ($lxPlats -contains 'windows-amd64') ''

$lxSpec = $clientSpec.Clone()
$lxSpec.platform  = 'linux-amd64'
$lxSpec.client_id = 'linuxtest01'
$lxSpec.proxies   = @(
    @{ name = 'web-tcp'; type = 'tcp'; local_ip = '127.0.0.1'; local_port = 8080; remote_port = 18090 }
)
$pvx = (Http POST '/api/pkg/client/preview' (ToJson $lxSpec)).Json
Result 'Linux 预览回显目标平台' ($pvx.platform -eq 'linux-amd64') ('实际: ' + $pvx.platform)
Result 'Linux 预览清单含 .sh 脚本' (($pvx.files -join ' ') -match 'install\.sh') ($pvx.files -join ', ')
Result 'Linux 预览清单不含 .ps1' (-not (($pvx.files -join ' ') -match '\.ps1')) ''
Result 'Linux 预览清单含 systemd unit' (@($pvx.files) -contains 'sinyefrp.service') ''
Result 'Linux 预览体积取自 Linux 二进制（与 Windows 不同）' ($pvx.size_bytes -ne $pv.size_bytes) ('Linux: ' + $pvx.size_bytes + ' / Windows: ' + $pv.size_bytes)

$zipLx = Join-Path $Work 'client-linux.zip'
$r = Http POST '/api/pkg/client' (ToJson $lxSpec) -OutFile $zipLx
Result 'Linux 包下载返回 200' ($r.Code -eq 200) ('HTTP ' + $r.Code)
Result 'Linux 包文件名带平台后缀（避免与 Windows 包同名覆盖）' ($r.FileName -match 'linux-amd64') ('实际: ' + $r.FileName)

$exLx = Join-Path $Work 'client-linux'
Expand-Archive -Path $zipLx -DestinationPath $exLx -Force

$lxExpected = @('frpc','frpc.toml','install.sh','uninstall.sh','start.sh','stop.sh','restart.sh',
                'status.sh','sinyefrp.service','README.txt','SHA256SUMS.txt')
$lxMissing = @()
foreach ($f in $lxExpected) { if (-not (Test-Path (Join-Path $exLx $f))) { $lxMissing += $f } }
Result 'Linux ZIP 含全部 11 个必需文件' ($lxMissing.Count -eq 0) ('缺少: ' + ($lxMissing -join ', '))
Result 'Linux 包不含任何 .ps1 或 .bat' `
    (@(Get-ChildItem -Path $exLx -Recurse -File | Where-Object { $_.Name -match '\.(ps1|bat)$' }).Count -eq 0) ''

# 二进制必须是 ELF。给 Linux 机器发一个 Windows 的 .exe 是最彻底的错误，
# 但它一路上不会有任何报错 —— 要到现场执行 ./frpc 才炸，且报错毫无指向性。
$frpcRaw = [System.IO.File]::ReadAllBytes((Join-Path $exLx 'frpc'))
$lxElf = ($frpcRaw.Length -ge 4 -and $frpcRaw[0] -eq 0x7F -and $frpcRaw[1] -eq 0x45 -and $frpcRaw[2] -eq 0x4C -and $frpcRaw[3] -eq 0x46)
Result 'Linux 包内的 frpc 是 ELF 可执行文件' $lxElf ('前 4 字节: ' + (($frpcRaw[0..3] | ForEach-Object { $_.ToString('X2') }) -join ' '))

# BOM 与行尾是 Linux 包最容易静默失败的两处：
#   .sh 带 BOM → 内核找不到 #!/bin/sh，报「No such file or directory」；
#   CRLF 行尾 → bad interpreter。两者都不指向真正的原因。
foreach ($sh in @('install.sh','uninstall.sh','start.sh','stop.sh','restart.sh','status.sh')) {
    $raw = [System.IO.File]::ReadAllBytes((Join-Path $exLx $sh))
    $hasBom = ($raw.Length -ge 3 -and $raw[0] -eq 0xEF -and $raw[1] -eq 0xBB -and $raw[2] -eq 0xBF)
    $hasCR  = ($raw -contains 13)
    Result ('Linux 脚本 ' + $sh + ' 无 BOM 且行尾为 LF') ((-not $hasBom) -and (-not $hasCR)) ''
}

$unitRaw = [System.IO.File]::ReadAllBytes((Join-Path $exLx 'sinyefrp.service'))
$unitBom = ($unitRaw.Length -ge 3 -and $unitRaw[0] -eq 0xEF -and $unitRaw[1] -eq 0xBB -and $unitRaw[2] -eq 0xBF)
Result 'unit 文件无 BOM（systemd 会把 BOM 当语法错误）' (-not $unitBom) ''
Result 'unit 文件行尾为 LF' (-not ($unitRaw -contains 13)) ''

$unitText = [System.IO.File]::ReadAllText((Join-Path $exLx 'sinyefrp.service'))
Result 'unit 的部署目录用占位符（生成时无从得知解压位置）' ($unitText -match '@ROOT@') ''
Result 'unit 显式限制重启次数（对齐「兜底不是守护」）' ($unitText -match 'StartLimitBurst\s*=\s*3') ''
Result 'unit 以 -c 传配置参数' ($unitText -match 'frpc\s+-c\s+@ROOT@/frpc\.toml') ''

# 脚本语义必须与 Windows 版对齐：只认领本目录的实例。
$lxInstall = [System.IO.File]::ReadAllText((Join-Path $exLx 'install.sh'))
$lxStop    = [System.IO.File]::ReadAllText((Join-Path $exLx 'stop.sh'))
$lxAll     = $lxInstall + $lxStop
# 语义断言必须只看**代码**，不能全文匹配：脚本注释里为了解释「为什么不用 pkill -f」，
# 必然会写出 pkill -f 这个字符串，全文匹配会把一条正确的脚本判成错误（本条实测假红过）。
$lxCode    = ((($lxAll -split "`n") | Where-Object { -not ($_ -match '^\s*#') }) -join "`n")
Result 'Linux 脚本按 /proc/<pid>/exe 精确匹配进程' ($lxCode -match '/proc/') ''
Result 'Linux 脚本未使用 pkill/pgrep -f 按名匹配（否则会误杀同机其它 frp）' `
    (-not ($lxCode -match 'pkill\s+-f') -and -not ($lxCode -match 'pgrep\s+-f')) ''
Result 'Linux 停止先发 SIGTERM 而不是直接 SIGKILL' ($lxCode -match 'kill\s+-TERM') ''
Result 'Linux 安装注册 systemd 开机自启' ($lxInstall -match 'systemctl\s+enable') ''
Result 'Linux 安装前做哈希校验' ($lxInstall -match 'SHA256SUMS') ''
Result 'Linux 脚本要求 root（不静默调 sudo 挂住无人值守安装）' ($lxCode -match 'id -u') ''

# README 必须是 Linux 版：Windows 那份讲 PowerShell 执行策略与 UAC，
# 照着它在 Linux 上操作每一步都是错的。
$lxReadme = [System.IO.File]::ReadAllText((Join-Path $exLx 'README.txt'))
Result 'Linux README 提示使用 sudo 安装' ($lxReadme -match 'sudo \./install\.sh') ''
Result 'Linux README 标出目标平台' ($lxReadme -match 'Linux x64') ''
Result 'Linux README 不含 PowerShell 执行策略说明' (-not ($lxReadme -match 'Set-ExecutionPolicy')) ''
Result 'Linux README 不含「计划任务」说法' (-not ($lxReadme -match '计划任务')) ''
Result 'Linux README 给出服务名' ($lxReadme -match 'sinyefrp-frpc-linuxtest01') ''

# 哈希清单必须与实际文件一致：Linux 包换了一套二进制，
# 若还在用 Windows 那份哈希，装到第一步就会以「包不可信」中止。
$lxSums = Get-Content (Join-Path $exLx 'SHA256SUMS.txt') -Encoding UTF8
$lxSumOk = $true
foreach ($line in $lxSums) {
    if ($line -notmatch '^([0-9a-fA-F]{64})\s+(.+?)\s*$') { continue }
    $want = $matches[1].ToLowerInvariant()
    $p = Join-Path $exLx $matches[2]
    if (-not (Test-Path $p)) { $lxSumOk = $false; continue }
    $got = (Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($got -ne $want) { $lxSumOk = $false }
}
Result 'Linux 包 SHA256SUMS.txt 逐项一致' $lxSumOk ''

# 未知平台必须被拒，且错误要给出可选项
$badPlat = $clientSpec.Clone()
$badPlat.platform = 'solaris-sparc'
$r = Http POST '/api/pkg/client/preview' (ToJson $badPlat)
Result '未知目标平台被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code)

# ---------- 8. 审计 ----------
Section '8. 审计记录（F16）'
# 注意：ConvertFrom-Json 在结果为空时会给出 $null，直接 .Count 会抛错，
# 因此这里统一用 @() 强制成数组再做判断。
$recs = @((Http GET '/api/pkg/records').Json.records)
Result '记录了打包行为' ($recs.Count -ge 2) ('条数: ' + $recs.Count)
# pkg_record 只登记成功的生成（失败尝试只进审计流），因此这里没有 result 字段可判。
$hasClient = @($recs | Where-Object { $_.kind -eq 'client' }).Count -gt 0
Result '记录了客户端包成功生成' $hasClient ''
Result '记录了服务器包成功生成' (@($recs | Where-Object { $_.kind -eq 'server' }).Count -gt 0) ''
Result '记录含二进制哈希（F15）' (@($recs | Where-Object { $_.exe_sha256.Length -eq 64 }).Count -gt 0) ''
$recText = if ($recs.Count -gt 0) { ($recs | ConvertTo-Json -Depth 6) } else { '' }
Result '打包记录不含明文 token' (-not $recText.Contains($cd.token)) ''
Result '打包记录不含明文 Dashboard 密码' (-not $recText.Contains($sd.dashboard_password)) ''

# /api/audit 的响应字段名是 items（见 internal/web/settings_handlers.go）。
$audit = @((Http GET '/api/audit').Json.items)
Result '审计流中出现 pkg.generate' (@($audit | Where-Object { $_.action -eq 'pkg.generate' }).Count -gt 0) ''

} catch {
    Write-Host ''
    Write-Host ('[中断] ' + $_.Exception.Message) -ForegroundColor Red
    Write-Host ('        位置: ' + $_.InvocationInfo.PositionMessage.Trim()) -ForegroundColor DarkGray
    $script:Fail++
    $script:FailNames += ('执行中断: ' + $_.Exception.Message)
}

# ---------- 汇总 ----------
Section '汇总'
$total = $script:Pass + $script:Fail
Write-Host ('  通过 ' + $script:Pass + ' / ' + $total) -ForegroundColor $(if ($script:Fail -eq 0) { 'Green' } else { 'Red' })
if ($script:Fail -gt 0) {
    Write-Host '  失败项:' -ForegroundColor Red
    foreach ($n in $script:FailNames) { Write-Host ('    - ' + $n) -ForegroundColor Red }
    exit 1
}
Write-Host '  全部通过' -ForegroundColor Green
exit 0
