<#
.SYNOPSIS
    一条命令跑完全部回归：冒烟 + 部署包 + 客户端台账 + 平台自身包 + 生命周期 + 前端渲染走查。

.DESCRIPTION
    本脚本解决的是一类**环境陷阱**，而不是「少敲几条命令」：

    1. 测试脚本假定被测服务已经在跑，但它们自己不会拉起服务。
       手工启动会踩另一个坑 —— 在本项目的自动化环境里，命令结束时
       子进程会被一起杀掉（浏览器 CDP 走查踩过同一个坑），
       表现为「启动日志说服务已就绪，下一条命令却连不上」。
       因此启动服务与执行测试必须发生在同一个进程生命周期里。
       **走查尤其躲不开这一条**：它要「起应用 → 等就绪 → 登录拿 Cookie →
       起无头浏览器 → 跑 node → 收尾」整串在同一进程里完成，
       否则浏览器会在半途消失，报出来的是 `ECONNREFUSED` —— 看着像浏览器没起，
       其实是被回收了。第 5 段就是为了把这一串封装成一次调用。

    2. 冒烟测试与部署包测试**必须各用一套实例与数据库**。
       冒烟测试最后一步会把本机 IP 打进 15 分钟登录锁定窗口，
       与部署包测试共用实例会让后者后续登录全部吃 429，
       而后者的失败信息看起来像是打包功能坏了 —— 极难定位。
       两者的配置分别在 .workbuddy/test/smoke 与 .workbuddy/test/pkg 下。
       走查用第三套（.workbuddy/test/ui），因为它要真登录一次。
       客户端台账用第四套（.workbuddy/test/registry）：它是有状态的（台账会累积
       记录），必须从空库开始，「台账初始为空」这类断言才与执行顺序无关。

    3. 生命周期测试依赖部署包测试的产物（它自动取 .workbuddy/pkgtest 下最新的一份），
       所以顺序不能颠倒。

    4. 台账套件（F19）自带一个 frps（7100/7600）并在其中真跑一次 frpc，
       用来覆盖「FRP 实时在线端口占用」这条口径。它必须自带而不是复用
       frp\frps.toml 那个：实测确认未配 auth.token 的 frps 会拒绝所有带
       token 的 frpc（"token in login doesn't match token from configuration"），
       而平台生成的客户端包必定带 token。自带也让该套件能脱离本脚本单跑。

.EXAMPLE
    .\scripts\regression.ps1
    .\scripts\regression.ps1 -SkipUI
    .\scripts\regression.ps1 -Only walkthrough
#>
[CmdletBinding()]
param(
    [int]$PkgPort   = 7810,
    [int]$SmokePort = 7820,
    [int]$UiPort    = 7830,
    # 客户端台账与端口占用（F19）用独立实例与独立库：
    # 它是**有状态**的套件（台账会累积记录），与冒烟/打包共用库会让
    # 「台账初始为空」这类断言依赖执行顺序。
    [int]$RegistryPort = 7850,
    [switch]$SkipLifecycle,
    # 跳过前端真实渲染走查（第 5 段）。它在无头浏览器里逐个路由截图，
    # 需要多起一个实例与一个浏览器，比别的套件慢；纯后端排障时可以跳过。
    [switch]$SkipUI,
    [switch]$KeepServers,
    # 只跑其中几个套件，取值：smoke / pkg / registry / platform / lifecycle / walkthrough。
    # 排障时不必把整套重跑一遍（整套约 2 分钟，其中一半花在真装真卸上）。
    [string[]]$Only = @()
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# ---- 展平环境块里的大小写重复项 --------------------------------------
#
# 为什么必须有这一段：
# 本机进程的环境块里同时存在 Path 与 PATH（还有两组代理变量
# HTTP_PROXY/http_proxy、HTTPS_PROXY/https_proxy）。Windows 环境块允许这种
# 只差大小写的物理重复，而 PowerShell 的 GetEnvironmentVariables() 返回的是
# 大小写不敏感的 Hashtable，会把它们合并掉 —— 所以从 PowerShell 侧**完全看不出**
# 有问题。但 Start-Process 内部是拿原始条目逐条往一个大小写不敏感的字典里
# .Add()，撞上第二条立刻抛：
#     Item has already been added. Key in dictionary: 'Path'  Key being added: 'PATH'
# 症状是回归**间歇性**中断在「启动 frps / 启动实例」那一步，报错的键还会变
# （先撞上哪个取决于遍历顺序，所以有时看到 http_proxy，有时看到 Path）——
# 极易被误判成脚本 bug 或环境抖动。
#
# 对策：直接读原始环境块（Win32 GetEnvironmentStringsW）找出重复组，
# 每组只保留一个拼写、其余删掉。只作用于本进程，不写注册表。
Add-Type -Namespace SinyeFrpEnv -Name Raw -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("kernel32.dll", CharSet=System.Runtime.InteropServices.CharSet.Unicode, SetLastError=true)]
public static extern System.IntPtr GetEnvironmentStringsW();
[System.Runtime.InteropServices.DllImport("kernel32.dll", SetLastError=true)]
public static extern bool FreeEnvironmentStringsW(System.IntPtr lpszEnvironmentBlock);
'@ -ErrorAction SilentlyContinue

# 取原始环境块的全部条目（形如 "Path=C:\..."，可含大小写重复的键）。
#
# 两条路径，先准后稳：
#   1) Win32 GetEnvironmentStringsW —— 直读原始块，最准确；
#   2) cmd /c set —— 回退路径。
#
# 为什么需要回退：部分受限环境禁止 Add-Type（运行期编译并加载 .NET 代码），
# 上面那段声明就建不出来，此时第 1 条会抛异常。没有回退的话，整个回归会
# 止步于「启动 frps/实例」并报 Item has already been added —— 一个与本脚本
# 逻辑毫无关系的错误。cmd.exe 继承的是同一个原始环境块，`set` 会把重复项
# 一并打印出来，因此它能起到同样的诊断作用。
function Get-RawEnvEntries {
    try {
        $ptr = [SinyeFrpEnv.Raw]::GetEnvironmentStringsW()
        if ($ptr -ne [IntPtr]::Zero) {
            $entries = New-Object System.Collections.Generic.List[string]
            $off = 0
            while ($true) {
                $s = [System.Runtime.InteropServices.Marshal]::PtrToStringUni([System.IntPtr]::Add($ptr, $off * 2))
                if ([string]::IsNullOrEmpty($s)) { break }
                $entries.Add($s)
                $off += $s.Length + 1
            }
            [void][SinyeFrpEnv.Raw]::FreeEnvironmentStringsW($ptr)
            return $entries
        }
    } catch { }

    try {
        $out = @(& cmd.exe /c set) 2>$null
        $entries = New-Object System.Collections.Generic.List[string]
        foreach ($line in $out) {
            if ($line -is [string] -and $line.Contains('=')) { $entries.Add($line) }
        }
        return $entries
    } catch {
        return $null
    }
}

# 返回展平的条目数；-1 表示读不到原始环境块（此时后面的 Start-Process 可能失败）。
function Repair-ProcessEnvironment {
    try {
        $entries = Get-RawEnvEntries
        if (-not $entries) { return -1 }

        $groups = @{}
        foreach ($e in $entries) {
            $i = $e.IndexOf('=')
            if ($i -le 0) { continue }
            $k = $e.Substring(0, $i)
            # cmd 的 set 会输出形如 "=C:=C:\dir" 的当前目录伪变量，跳过。
            if ($k -like '=*') { continue }
            $lk = $k.ToLowerInvariant()
            if ($groups.ContainsKey($lk)) { $groups[$lk] = $groups[$lk] + '|' + $k } else { $groups[$lk] = $k }
        }
        $fixedCount = 0
        foreach ($lk in @($groups.Keys)) {
            $names = $groups[$lk] -split '\|'
            if ($names.Count -lt 2) { continue }
            # 保留第一个拼写，其余按名字删除（SetEnvironmentVariable 不区分大小写）。
            # 实际上是「按名字删掉一个命中项」：无论删掉哪个拼写，
            # 每组最终只剩一条，这才是目的。
            for ($i = 1; $i -lt $names.Count; $i++) {
                [System.Environment]::SetEnvironmentVariable($names[$i], $null, 'Process')
                $fixedCount++
            }
        }
        return $fixedCount
    } catch {
        return -1
    }
}

$script:EnvFixed = Repair-ProcessEnvironment

$Root   = Split-Path -Parent $PSScriptRoot
$Exe    = Join-Path $Root 'bin\sinyefrp.exe'
$TestDir = Join-Path $Root '.workbuddy\test'
$RunLog = Join-Path $TestDir ('regression-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')

$PS51 = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (-not (Test-Path $PS51)) { $PS51 = 'powershell.exe' }

$script:Lines = New-Object System.Collections.Generic.List[string]
$script:RunLogWriter = $null

# 日志**实时落盘**，不做「最后一次性写文件」。
#
# 原因是一次真实的踩坑：跑全套五个套件约需 2 分钟，而本项目的自动化环境对
# 单条命令有约 120 秒的上限，超时后整个进程被回收。原先日志在收尾时才写，
# 于是超时那一刻**连一个字都没留下** —— 恰恰是最需要日志的时候什么都没有。
# 改成 AutoFlush 的流式写入后，被砍到哪一步，日志就断在哪一步，可直接定位。
function Open-RunLogWriter {
    if ($script:RunLogWriter) { return $script:RunLogWriter }
    try {
        if (-not (Test-Path $TestDir)) { New-Item -ItemType Directory -Path $TestDir -Force | Out-Null }
        # 带 BOM 创建一次即可；之后是纯追加，不会反复写 BOM。
        $script:RunLogWriter = New-Object System.IO.StreamWriter($RunLog, $false, (New-Object System.Text.UTF8Encoding($true)))
        $script:RunLogWriter.AutoFlush = $true
    } catch {
        $script:RunLogWriter = $null
    }
    return $script:RunLogWriter
}

function Say([string]$Text, [string]$Color = 'Gray') {
    $script:Lines.Add($Text)
    try { Write-Host $Text -ForegroundColor $Color } catch { }
    try {
        $w = Open-RunLogWriter
        if ($w) { $w.WriteLine($Text) }
    } catch { }
}

function Save-RunLog {
    try {
        if ($script:RunLogWriter) {
            $script:RunLogWriter.Flush()
            $script:RunLogWriter.Close()
            $script:RunLogWriter = $null
        }
    } catch { }
}

function Test-PortOpen([int]$Port) {
    $c = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $c.BeginConnect('127.0.0.1', $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne(1000, $false)) { return $false }
        $c.EndConnect($iar)
        return $true
    } catch {
        return $false
    } finally {
        try { $c.Close() } catch { }
    }
}

# 在独立实例上跑一个测试脚本，返回退出码与输出。
# 必须走子进程：测试脚本用 exit N 表达结果，直接调用会把宿主一起退出。
#
# 输出同时落到 .workbuddy\test\suite-<名称>-<时间戳>.log：本项目的自动化环境里
# 控制台回显时常拿不到，而「哪个断言失败了」恰恰是唯一有用的信息。
function Invoke-Suite {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Script,
        [string[]]$Arguments = @()
    )
    $path = Join-Path $PSScriptRoot $Script
    $tmpLog = Join-Path $env:TEMP ('suite-' + [guid]::NewGuid().ToString('N') + '.log')
    $keepLog = Join-Path $TestDir ('suite-' + $Name + '-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
    $prev = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $PS51 -NoProfile -ExecutionPolicy Bypass -File $path @Arguments *> $tmpLog
        $code = $LASTEXITCODE
        $text = ''
        if (Test-Path $tmpLog) { $text = [System.IO.File]::ReadAllText($tmpLog) }
        try { [System.IO.File]::WriteAllText($keepLog, $text, (New-Object System.Text.UTF8Encoding($true))) } catch { }
        return [pscustomobject]@{ Name = $Name; Code = $code; Text = $text; LogPath = $keepLog }
    } finally {
        $ErrorActionPreference = $prev
        Remove-Item $tmpLog -Force -ErrorAction SilentlyContinue
    }
}

# 从 frp\frps.toml 读出 Dashboard 的端口与凭据。
#
# 冒烟测试的节点连通性用例需要一份**正确**的 Dashboard 凭据才会通过
# （用错密码那一条另有用例覆盖）。这里从配置文件里读，而不是把口令抄进本脚本：
# 抄一份意味着换口令时要改两处，而且仓库里会多出一份明文副本。
function Read-FrpsToml {
    $result = @{ Port = 7500; User = 'admin'; Password = '' }
    $p = Join-Path $Root 'frp\frps.toml'
    if (-not (Test-Path $p)) { return $result }
    foreach ($line in (Get-Content -LiteralPath $p)) {
        if ($line -match '^\s*webServer\.port\s*=\s*(\d+)') { $result.Port = [int]$matches[1] }
        if ($line -match '^\s*webServer\.user\s*=\s*"([^"]*)"') { $result.User = $matches[1] }
        if ($line -match '^\s*webServer\.password\s*=\s*"([^"]*)"') { $result.Password = $matches[1] }
    }
    return $result
}

# 判断某个套件是否在本次要跑的范围内。
function Test-Selected([string]$Name) {
    if ($Only.Count -eq 0) { return $true }
    foreach ($o in $Only) { if ($o.Trim().ToLowerInvariant() -eq $Name) { return $true } }
    return $false
}

# 找 node。不写死某个用户的安装路径 —— 本仓库的自动化环境把 node 放在
# %USERPROFILE%\.workbuddy\binaries\node\versions\<ver>\node.exe，但换台机器就不同了，
# 所以先看 PATH，再回退到这套托管目录并挑版本号最大的一个。
function Find-Node {
    $cmd = Get-Command node -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $pat = Join-Path $env:USERPROFILE '.workbuddy\binaries\node\versions\*\node.exe'
    $cands = @(Get-ChildItem -Path $pat -ErrorAction SilentlyContinue | Sort-Object FullName -Descending)
    if ($cands.Count -gt 0) { return $cands[0].FullName }
    return $null
}

# 找 Edge。走查的基线是 Edge（Chromium），生产环境也是它。
function Find-Edge {
    $cands = @(
        (Join-Path ${env:ProgramFiles(x86)} 'Microsoft\Edge\Application\msedge.exe'),
        (Join-Path $env:ProgramFiles 'Microsoft\Edge\Application\msedge.exe'),
        (Join-Path $env:LOCALAPPDATA 'Microsoft\Edge\Application\msedge.exe')
    )
    foreach ($c in $cands) {
        if ($c -and (Test-Path $c)) { return $c }
    }
    return $null
}

# 第 5 段：前端真实渲染走查。
#
# 这一段存在的理由：`vue-tsc` 查不出两类缺陷 —— 模板里组件插槽变量是 any
# （字段名写错照样编译通过），以及隐藏面板里的运行时崩溃会连带整页白屏。
# 只有真在浏览器里跑一次才会现形。
#
# 为什么必须封装在这一段里（而不是像别的套件那样独立执行）：
# 本项目的自动化环境中，由「一次工具调用」启动的子进程会在该调用结束时被回收。
# 走查要在一串步骤里同时hold住应用、无头浏览器与 node，任意一步拆到下一次调用，
# 前面的进程就没了 —— 症状是 `ECONNREFUSED 127.0.0.1:9333`，看着像浏览器起不来。
function Invoke-Walkthrough {
    param(
        [Parameter(Mandatory)][int]$Port,
        [int]$CdpPort = 9333
    )

    $node = Find-Node
    if (-not $node) { throw '未找到 node，无法执行前端渲染走查（可加 -SkipUI 跳过）' }
    $edge = Find-Edge
    if (-not $edge) { throw '未找到 msedge.exe，无法执行前端渲染走查（可加 -SkipUI 跳过）' }

    $base = 'http://127.0.0.1:' + $Port
    $lines = New-Object System.Collections.Generic.List[string]
    $lines.Add('node : ' + $node)
    $lines.Add('edge : ' + $edge)

    # ---- 登录：拿一个会话 Cookie ----
    #
    # 走查脚本**不能**自己登录：登录接口有「用户名 + 来源 IP」双键限速，
    # 走查反复登录会吃 429。这里由脚本登一次，把 Cookie 通过环境变量注入。
    #
    # 这个实例每次运行都是全新的库（New-RunConfig），所以先走初始化；
    # 万一库已存在（例如 -KeepServers 后重跑），回退到登录。
    #
    # 【必须用 -WebSession，不能用 -Headers @{ Cookie = ... }】
    # .NET 把 Cookie 列为**受保护请求头**：手工塞进 Headers 会被静默忽略，
    # 表现为后续请求一律 401 —— 而同一个 Cookie 值用 curl 发却是 200，
    # 极易误判成「服务端会话有问题」。交给 WebRequestSession 管理即可。
    $adminUser = 'walkadmin'
    $adminPass = 'WalkThrough_2026!x'
    $body = '{"username":"' + $adminUser + '","password":"' + $adminPass + '"}'
    $web = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $session = $null   # name=value，注入浏览器用
    foreach ($ep in @('/api/setup/admin', '/api/auth/login')) {
        try {
            Invoke-WebRequest -Uri ($base + $ep) -Method POST -Body $body `
                -ContentType 'application/json' -UseBasicParsing -TimeoutSec 10 `
                -WebSession $web | Out-Null
            $lines.Add('会话获取 : ' + $ep)
            break
        } catch {
            $lines.Add('会话获取失败 : ' + $ep + ' -> ' + $_.Exception.Message)
        }
    }
    foreach ($ck in $web.Cookies.GetCookies([uri]$base)) {
        if ($ck.Name -eq 'sinyefrp_session') { $session = $ck.Name + '=' + $ck.Value }
    }
    if (-not $session) { throw '未能取得会话 Cookie，走查无法进行' }

    # 指向本地 frps，让页面渲染的是真实数据而不是降级态。
    if ($frps.Password) {
        $nb = '{"name":"walkthrough","api_url":"http://127.0.0.1:' + $frps.Port + '","username":"' +
              $frps.User + '","password":"' + $frps.Password + '","timeout_seconds":5,"enabled":true}'
        try {
            Invoke-WebRequest -Uri ($base + '/api/node/config') -Method POST -Body $nb `
                -ContentType 'application/json' -UseBasicParsing -TimeoutSec 10 `
                -WebSession $web | Out-Null
            $lines.Add('节点配置 : 已指向 127.0.0.1:' + $frps.Port)
        } catch {
            $lines.Add('节点配置失败（页面会渲染成降级态）: ' + $_.Exception.Message)
        }
    }

    # ---- 起无头 Edge ----
    #
    # user-data-dir 放临时目录并每次重建：残留的 profile 里可能留着上一轮的会话，
    # 而走查要验的恰恰是「注入的这一个 Cookie 能不能用」。
    $profile = Join-Path $env:TEMP 'sinyefrp-walk-profile'
    if (Test-Path $profile) {
        try { [System.IO.Directory]::Delete($profile, $true) } catch { $lines.Add('旧 profile 未清掉（不影响结果）') }
    }

    $edgeProc = $null
    try {
        # --disable-extensions 必须带上，不是可选优化。
        #
        # 实测（2026-09-25）：本机装了 AdBlock Plus（eyeo），它的 content script
        # 会注入到被测页面里并自己抛异常，报出来是
        #   Uncaught (in promise) Cannot read properties of undefined (reading 'url')
        #   @chrome-extension://gmgoamodcdcjnbaobigkjelfplakmdhh/vendor/@eyeo/.../content.js
        # 走查的「全程无控制台异常与告警」把它算成我们页面的缺陷，于是随机红。
        # 这类错误与应用代码无关、且随机器安装的扩展而变，不隔离就无法复现。
        #
        # 注意：换 user-data-dir 挡不住它 —— 扩展可能是按策略强制安装的，
        # 只有显式 --disable-extensions 才生效。
        $edgeProc = Start-Process -FilePath $edge -WindowStyle Hidden -PassThru -ArgumentList @(
            '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
            '--disable-extensions', '--disable-component-extensions-with-background-pages',
            ('--remote-debugging-port=' + $CdpPort),
            ('--user-data-dir=' + $profile),
            ($base + '/')
        )
        $up = $false
        for ($i = 0; $i -lt 20; $i++) {
            if (Test-PortOpen $CdpPort) { $up = $true; break }
            Start-Sleep -Milliseconds 500
        }
        if (-not $up) { throw ('无头 Edge 未能在 10 秒内打开调试端口 ' + $CdpPort) }
        $lines.Add('调试端口 : ' + $CdpPort + '  PID ' + $edgeProc.Id)

        # ---- 跑两个 node 脚本 ----
        #
        # SINYEFRP_SESSION 传的是**整串 `name=value`**（从 Set-Cookie 抠出来的原样），
        # 走查脚本会自己剥掉名字 —— 这也是人手工粘贴时的自然写法，顺带把这条兼容路径测了。
        $prevSession = $env:SINYEFRP_SESSION
        $env:SINYEFRP_SESSION = $session
        $code = 0
        try {
            $outDir = Join-Path $TestDir ('shots\' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
            $nodeArgs = @(
                (Join-Path $PSScriptRoot 'ui-walkthrough.js'), $CdpPort,
                '--base', $base, '--out', $outDir, '--full-page'
            )
            $lines.Add('')
            $lines.Add('== 逐路由走查 ==')
            $prev = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            $wt = & $node @nodeArgs 2>&1 | ForEach-Object { $_.ToString() }
            $code = $LASTEXITCODE
            $ErrorActionPreference = $prev
            foreach ($l in $wt) { $lines.Add($l) }

            # 走查只按路由截图、不点 tab，而 el-tab-pane 是「隐藏而非卸载」：
            # 隐藏面板只保证不崩，**内容渲染错了不报任何错**。所以要真切过去看一眼。
            $lines.Add('')
            $lines.Add('== 「管理平台自身」tab 内容 ==')
            $prev = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            $tc = & $node @(
                (Join-Path $PSScriptRoot 'ui-tab-check.js'), $CdpPort,
                '--base', $base, '--route', '/packages', '--label', '管理平台自身',
                '--out', (Join-Path $outDir 'tab-管理平台自身.png')
            ) 2>&1 | ForEach-Object { $_.ToString() }
            $tcCode = $LASTEXITCODE
            $ErrorActionPreference = $prev
            foreach ($l in $tc) { $lines.Add($l) }
            if ($tcCode -ne 0 -and $code -eq 0) { $code = $tcCode }

            # 敏感字段（token / Dashboard 密码）的遮蔽与取用流程。
            #
            # 这一项没法靠截图：曾经出现「遮蔽串与密码圆点被画两遍」——输入框用
            # type=password 渲染成圆点，prefix 插槽里又画了一个遮蔽串，同一个值画两遍。
            # 控制台零告警、vue-tsc 也过，只有直接读 DOM 才能断言。
            $lines.Add('')
            $lines.Add('== 敏感字段（遮蔽 / 显示明文）==')
            $prev = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            $sc = & $node @(
                (Join-Path $PSScriptRoot 'ui-secret-check.js'), $CdpPort,
                '--base', $base, '--out', (Join-Path $outDir 'secret-服务器部署包.png')
            ) 2>&1 | ForEach-Object { $_.ToString() }
            $scCode = $LASTEXITCODE
            $ErrorActionPreference = $prev
            foreach ($l in $sc) { $lines.Add($l) }
            if ($scCode -ne 0 -and $code -eq 0) { $code = $scCode }

            # 机器人类型分组（F20）。
            #
            # 分组是纯前端视图（排序 + 合并单元格 + 筛选计数），算错的表现全都很温和：
            # 合并块错位、计数变 0、列下标写错导致整列消失 —— 一个异常都不抛，
            # 截图也看不出。所以必须读 DOM 断言。
            #
            # 脚本会自己在浏览器里生成三个带类型的客户端包（两个同类型 + 一个
            # 另一类型）：走查实例是空台账，没有数据时这些断言会全部假绿。
            $lines.Add('')
            $lines.Add('== 机器人类型分组（设备台账 / 客户端）==')
            $prev = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            $grp = & $node @(
                (Join-Path $PSScriptRoot 'ui-robot-group-check.js'), $CdpPort,
                '--base', $base, '--out', (Join-Path $outDir 'robot-group.png')
            ) 2>&1 | ForEach-Object { $_.ToString() }
            $grpCode = $LASTEXITCODE
            $ErrorActionPreference = $prev
            foreach ($l in $grp) { $lines.Add($l) }
            $lines.Add('')
            if ($grpCode -ne 0 -and $code -eq 0) { $code = $grpCode }
        } finally {
            if ($prevSession) { $env:SINYEFRP_SESSION = $prevSession } else { Remove-Item env:SINYEFRP_SESSION -ErrorAction SilentlyContinue }
        }
    } finally {
        if ($edgeProc) { try { Stop-Process -Id $edgeProc.Id -Force -ErrorAction SilentlyContinue } catch { } }
        # Edge 的子进程会继续 hold 住 profile 目录，稍等再删，删不掉也不影响结果。
        Start-Sleep -Milliseconds 800
        try { [System.IO.Directory]::Delete($profile, $true) } catch { }
    }

    $keepLog = Join-Path $TestDir ('suite-walkthrough-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
    $text = ($lines -join [Environment]::NewLine)
    try { [System.IO.File]::WriteAllText($keepLog, $text, (New-Object System.Text.UTF8Encoding($true))) } catch { }
    return [pscustomobject]@{ Name = 'walkthrough'; Code = $code; Text = $text; LogPath = $keepLog }
}

# 生成一份「本次运行专用」的实例配置，指向一个新的 data / logs 子目录。
#
# 为什么不先清掉上一次的数据：冒烟测试**要求空库**，第二次运行沿用旧库会直接判定
# 「已完成初始化」并以退出码 2 失败——那条信息看起来像是产品坏了。
# 而递归删除在本项目的自动化环境里会被批量删除防护拦下（报错只说
# "bulk delete guard blocked deletion"，看不出根因）。
# 改为每次运行换一个数据目录：不删任何东西，空库由构造保证。
# 这也与 pkg-test.ps1「保留时间戳目录便于事后取证」的做法一致。
#
# 目录名写死而不是从端口推导：配置目录名与端口之间没有推导关系。
function New-RunConfig {
    param(
        [Parameter(Mandatory)][int]$Port,
        [Parameter(Mandatory)][string]$Name
    )
    $base = Join-Path $TestDir $Name
    if (-not (Test-Path $base)) { New-Item -ItemType Directory -Path $base -Force | Out-Null }
    $src = Join-Path $base 'config.toml'
    if (-not (Test-Path $src)) { throw ('缺少测试配置: ' + $src) }

    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $text = [System.IO.File]::ReadAllText($src)
    # 用正斜杠：TOML 基本字符串里反斜杠是转义引导符，"data\2026..." 会解析失败。
    $text = $text -replace '(?m)^\s*path\s*=\s*".*"\s*$', ('path = "data/' + $stamp + '/sinyefrp.db"')
    $text = $text -replace '(?m)^\s*dir\s*=\s*"logs"\s*$', ('dir = "logs/' + $stamp + '"')

    $dst = Join-Path $base ('run-' + $stamp + '.toml')
    [System.IO.File]::WriteAllText($dst, $text, (New-Object System.Text.UTF8Encoding($true)))
    return $dst
}

if (-not (Test-Path $Exe)) {
    Say ('未找到 ' + $Exe + '，请先执行 scripts\build.ps1') 'Red'
    exit 1
}

$started = @()
$frpsProc = $null
$frps = Read-FrpsToml
$results = New-Object System.Collections.Generic.List[object]

Say '============================================================'
Say ' SinyeFRP 全量回归'
Say '============================================================'
Say ('被测程序  : ' + $Exe)
Say ('冒烟实例  : 127.0.0.1:' + $SmokePort + '  (.workbuddy\test\smoke)')
Say ('打包实例  : 127.0.0.1:' + $PkgPort   + '  (.workbuddy\test\pkg)')
if (Test-Selected 'registry') {
    Say ('台账实例  : 127.0.0.1:' + $RegistryPort + '  (.workbuddy\test\registry)')
}
if ((-not $SkipUI) -and (Test-Selected 'walkthrough')) {
    Say ('走查实例  : 127.0.0.1:' + $UiPort + '  (.workbuddy\test\ui)')
}
Say ('运行日志  : ' + $RunLog)
if ($script:EnvFixed -gt 0) {
    Say ('环境自检   : 已展平 ' + $script:EnvFixed + ' 处大小写重复的环境变量（否则 Start-Process 会抛异常）')
} elseif ($script:EnvFixed -lt 0) {
    Say '环境自检   : 读不到原始环境块 —— 若中断在「启动 frps/实例」处，先查这一项' 'Yellow'
}
Say ''

try {
    # ---- 本地 frps：冒烟测试要有一条「连通 Dashboard 成功」的用例 ----
    #
    # 若 7500 已在监听（开发者自己起着 frps 调试），就直接复用、结束时也不动它；
    # 只有确实没起时才由本脚本拉起并在结束时停掉——否则会误杀开发者正在用的实例。
    if (Test-PortOpen $frps.Port) {
        Say ('本地 frps 已在运行 :' + $frps.Port + '  直接复用，结束时不做改动')
    } else {
        $frpsExe = Join-Path $Root 'frp\frps.exe'
        if (Test-Path $frpsExe) {
            $frpsOut = Join-Path $TestDir 'frps.log'
            $frpsProc = Start-Process -FilePath $frpsExe `
                -ArgumentList @('-c', (Join-Path $Root 'frp\frps.toml')) `
                -WorkingDirectory (Join-Path $Root 'frp') `
                -RedirectStandardOutput $frpsOut -RedirectStandardError ($frpsOut + '.err') `
                -WindowStyle Hidden -PassThru
            $ok = $false
            for ($i = 0; $i -lt 10; $i++) {
                if (Test-PortOpen $frps.Port) { $ok = $true; break }
                Start-Sleep -Seconds 1
            }
            if ($ok) {
                Say ('已启动本地 frps :' + $frps.Port + '  PID ' + $frpsProc.Id) 'Green'
            } else {
                Say ('本地 frps 未能在 10 秒内就绪，冒烟测试的 4 条节点连通性用例会失败') 'Yellow'
            }
        } else {
            Say ('未找到 ' + $frpsExe + '，冒烟测试的节点连通性用例会失败') 'Yellow'
        }
    }
    Say ''

    # ---- 启动互相隔离的实例（只起本次真正要用的那些）----
    #
    # ui 实例只在跑走查时才需要（走查要真登录一次，不能蹭冒烟那套被限速的库）。
    $wantUi = (-not $SkipUI) -and (Test-Selected 'walkthrough')
    $instances = @()
    if (Test-Selected 'smoke')    { $instances += @{ Port = $SmokePort;    Name = 'smoke' } }
    if (Test-Selected 'pkg')      { $instances += @{ Port = $PkgPort;      Name = 'pkg' } }
    if (Test-Selected 'registry') { $instances += @{ Port = $RegistryPort; Name = 'registry' } }
    if ($wantUi)                  { $instances += @{ Port = $UiPort;       Name = 'ui' } }

    foreach ($item in $instances) {
        if (Test-PortOpen $item.Port) {
            Say ('端口 ' + $item.Port + ' 已被占用 —— 请先停掉占用它的进程，避免两套测试互相污染') 'Red'
            throw ('端口 ' + $item.Port + ' 被占用')
        }
        $conf = New-RunConfig -Port $item.Port -Name $item.Name
        Say ('实例配置   : ' + $conf)
        $out = Join-Path $TestDir ('run-' + $item.Port + '.log')
        $p = Start-Process -FilePath $Exe -ArgumentList @('-config', $conf) `
            -WorkingDirectory $Root -RedirectStandardOutput $out `
            -RedirectStandardError ($out + '.err') -WindowStyle Hidden -PassThru
        $started += $p
        Say ('已启动实例 :' + $item.Port + '  PID ' + $p.Id)
    }

    foreach ($item in $instances) {
        $ok = $false
        for ($i = 0; $i -lt 15; $i++) {
            if (Test-PortOpen $item.Port) { $ok = $true; break }
            Start-Sleep -Seconds 1
        }
        if (-not $ok) { throw ('实例 ' + $item.Port + ' 未能在 15 秒内就绪，请查看 .workbuddy\test\run-' + $item.Port + '.log') }
        Say ('实例就绪   :' + $item.Port) 'Green'
    }
    Say ''

    # ---- 冒烟测试（独立实例）----
    if (Test-Selected 'smoke') {
        Say '-- 1/6 冒烟测试 --' 'Cyan'
        # 显式传节点地址与凭据：脚本自身的默认值是一个「故意错误的密码」，
        # 用于覆盖 unauthorized 分类；不传的话「连通成功」那条会必然失败。
        $smokeArgs = @('-BaseUrl', ('http://127.0.0.1:' + $SmokePort))
        if ($frps.Password) {
            $smokeArgs += @('-NodeUrl', ('http://127.0.0.1:' + $frps.Port), '-NodePassword', $frps.Password)
        }
        $r = Invoke-Suite -Name 'smoke' -Script 'smoke-test.ps1' -Arguments $smokeArgs
        $results.Add($r)
        Say ('  退出码 ' + $r.Code + '  明细: ' + $r.LogPath)
        Say ''
    } else {
        Say '-- 1/6 冒烟测试：已跳过 --' 'Yellow'
    }

    # ---- 部署包测试 ----
    if (Test-Selected 'pkg') {
        Say '-- 2/6 部署包测试 --' 'Cyan'
        $r = Invoke-Suite -Name 'pkg' -Script 'pkg-test.ps1' -Arguments @('-BaseUrl', ('http://127.0.0.1:' + $PkgPort))
        $results.Add($r)
        Say ('  退出码 ' + $r.Code + '  明细: ' + $r.LogPath)
        Say ''
    } else {
        Say '-- 2/6 部署包测试：已跳过 --' 'Yellow'
    }

    # ---- 客户端台账与端口占用（自带 frps + 真跑 frpc）----
    if (Test-Selected 'registry') {
        Say '-- 3/6 客户端台账与端口占用（F19）--' 'Cyan'
        $r = Invoke-Suite -Name 'registry' -Script 'registry-test.ps1' `
            -Arguments @('-BaseUrl', ('http://127.0.0.1:' + $RegistryPort))
        $results.Add($r)
        Say ('  退出码 ' + $r.Code + '  明细: ' + $r.LogPath)
        Say ''
    } else {
        Say '-- 3/6 客户端台账与端口占用：已跳过 --' 'Yellow'
        Say ''
    }

    # ---- 平台自身部署包（真装、真跑、真卸）----
    if (Test-Selected 'platform') {
        Say '-- 4/6 平台自身部署包（F18）--' 'Cyan'
        $r = Invoke-Suite -Name 'platform' -Script 'platform-test.ps1'
        $results.Add($r)
        Say ('  退出码 ' + $r.Code + '  明细: ' + $r.LogPath)
        Say ''
    } else {
        Say '-- 4/6 平台自身部署包：已跳过 --' 'Yellow'
    }

    # ---- 生命周期测试（依赖部署包测试产出的包）----
    if ((-not $SkipLifecycle) -and (Test-Selected 'lifecycle')) {
        Say '-- 5/6 生命周期测试（frpc 部署包真装真卸）--' 'Cyan'
        $r = Invoke-Suite -Name 'lifecycle' -Script 'lifecycle-test.ps1'
        $results.Add($r)
        Say ('  退出码 ' + $r.Code + '  明细: ' + $r.LogPath)
        Say ''
    } else {
        Say '-- 5/6 生命周期测试：已跳过 --' 'Yellow'
        Say ''
    }

    # ---- 前端真实渲染走查（无头 Edge + CDP）----
    if ($wantUi) {
        Say '-- 6/6 前端渲染走查（无头 Edge 逐路由）--' 'Cyan'
        $r = Invoke-Walkthrough -Port $UiPort
        $results.Add($r)
        Say ('  退出码 ' + $r.Code + '  明细: ' + $r.LogPath)
        Say ''
    } else {
        Say '-- 6/6 前端渲染走查：已跳过 --' 'Yellow'
        Say ''
    }

} catch {
    Say ('[中断] ' + $_.Exception.Message) 'Red'
    $results.Add([pscustomobject]@{ Name = '执行中断'; Code = 1; Text = $_.Exception.Message; LogPath = '' })
} finally {
    if (-not $KeepServers) {
        foreach ($p in $started) {
            try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch { }
        }
        Start-Sleep -Seconds 1
        Say '已停止测试实例'
    } else {
        Say ('测试实例保留运行（-KeepServers）：' + (($started | ForEach-Object { $_.Id }) -join ', '))
    }

    # 只停由本脚本拉起的 frps；开发者原本就在跑的那个不动。
    if ($frpsProc) {
        try { Stop-Process -Id $frpsProc.Id -Force -ErrorAction SilentlyContinue } catch { }
        Say '已停止本次拉起的本地 frps'
    }
}

# ---- 汇总：每个套件打印自己的结果块，便于一眼看出是哪个套件退化 ----
Say ''
Say '============================================================'
Say ' 汇总'
Say '============================================================'
$failed = 0
foreach ($r in $results) {
    # 先算出文案再传参：PS 5.1 的内联 if 作为实参容易在复杂表达式里出错，
    # 而这里恰恰是「汇总没打印出来」最容易被当成「全部通过」的地方。
    $color = 'Red'
    $label = '失败（退出码 ' + $r.Code + '）'
    if ($r.Code -eq 0) { $color = 'Green'; $label = '通过' }
    else { $failed++ }
    Say ('  ' + $r.Name.PadRight(12) + ' ' + $label) $color
}

foreach ($r in $results) {
    # 每个套件末尾都有一行计数，但各套件的措辞并不统一：
    #   冒烟     -> 通过 41 项，失败 0 项
    #   部署包   -> 通过 101 / 101
    #   平台自身 -> 结果: 全部通过  71/71
    #   生命周期 -> 通过 37 / 37
    #   走查     -> ... 有问题的页面数: 0
    # 早先只匹配「N / M」与「结果:」，结果冒烟的计数被静默吞掉，
    # 汇总里少一行看着像「这个套件没跑」——这里把这几种写法都纳入，并且
    # 取不到时显式提示，避免再次出现「没打印 = 以为通过」。
    $tail = @($r.Text -split "`r?`n" | Where-Object { $_ -match '通过\s+\d+\s*(/|项)|结果:\s|有问题的页面数:\s*\d+' })
    if ($tail.Count -gt 0) {
        # 一个套件可能有多条计数：走查既报「有问题的页面数」又报敏感字段断言数。
        # 只显示最后一条会让人以为另一项没跑，所以全列出来。
        for ($i = 0; $i -lt $tail.Count; $i++) {
            $tag = $r.Name
            if ($i -gt 0) { $tag = ' ' * $r.Name.Length }
            Say ('    ' + $tag + ': ' + $tail[$i].Trim())
        }
    } else {
        Say ('    ' + $r.Name + ': 未取到计数，请查看明细日志') 'Yellow'
    }
}

Say ''
if ($failed -eq 0) {
    Say ' 全量回归通过' 'Green'
} else {
    Say (' ' + $failed + ' 个套件失败') 'Red'
}
# 必须写在 Save-RunLog 之前：它一旦落在后面，就会在 writer 关闭后又重建一次
# 而把整份日志截断（见 Open-RunLogWriter 的注释）。
Say (' 运行日志: ' + $RunLog)
Save-RunLog

exit $(if ($failed -eq 0) { 0 } else { 1 })
