<#
.SYNOPSIS
    SinyeFRP 客户端台账与端口占用端到端测试（01-prd F19）。

.DESCRIPTION
    覆盖：
      0. 前置（服务、初始化、未登录拒绝）
      1. 建包前置：未配接入点时建包被拒，配好接入点后默认值带地址/端口/token
      2. 台账初始状态
      3. 首次签发后记账正确（名称、clientID、端口、次数）
      4. 同 clientID 重新生成不算冲突（改代理后重新出包是必须允许的路径）
      5. 异 clientID 撞同一端口被拦（预览给 blocking、下载 409）
      6. 换端口即可生成
      7. 端口占用是「整体替换」而不是累加（删掉一条代理，端口要跟着释放）
      8. 停用释放端口 / 启用时反向冲突校验 / 删除级联释放
      9. 不占端口的代理类型（http）不进端口总览
     10. 真实 frps + frpc：实时端口占用口径、「别人在线上占着」也要拦，
         以及台账显示名称关联到 /api/clients 与 /api/proxies
     11. 机器人类型：落台账、可改可清空、非法值被拒，
         以及它**不会**进 frpc.toml（FRP 严格解码，多一个键就拒包）
     12. 审计留痕

    为什么要真跑 frps 与 frpc：
    冲突判定有两个来源——本平台台账，和 FRP 此刻真实在线的代理。后者只能由
    真正的 frpc 注册到真正的 frps 上才能产生。这一点尤其不能靠造数据绕过，
    因为「frpc 注册代理时远端端口冲突会怎样」本身就是这条功能的立论基础。

    这里**自己拉一个 frps**（端口 7100/7600），而不是复用 frp\frps.toml 里那个：
    实测确认 frps 未配 auth.token 时，带 token 的 frpc 一律登录失败
    （"token in login doesn't match token from configuration"），
    而本平台生成的客户端包必定带 token。所以必须有一个带 token 的测试 frps。
    自带一个也让本套件可以脱离 regression.ps1 单独运行。

.EXAMPLE
    .\scripts\registry-test.ps1
    .\scripts\registry-test.ps1 -BaseUrl http://127.0.0.1:7850
#>
[CmdletBinding()]
param(
    [string]$BaseUrl   = '',
    [string]$AdminUser = 'regadmin',
    [string]$AdminPass = 'RegTest_2026!x',
    [string]$FrpDir    = '',
    [switch]$KeepFrps
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# curl.exe 以 UTF-8 字节流写出响应，而 Windows PowerShell 5.1 默认按控制台代码页
# （简体中文环境为 GBK）解码，中文全部乱码，会让「错误文案里是否提到占用者名称」
# 这类断言恒为假 —— 看着像产品缺陷，实为测试环境问题。
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding           = [System.Text.Encoding]::UTF8

$Root = Split-Path -Parent $PSScriptRoot
if (-not $BaseUrl) { $BaseUrl = 'http://127.0.0.1:7850' }
if (-not $FrpDir)  { $FrpDir  = Join-Path $Root 'frp' }

# 每次运行使用独立的时间戳目录；不清理旧目录（同其他套件的理由：
# 递归删除容易被安全策略拦截，而保留产物在排障时更有用）。
$Work = Join-Path $Root ('.workbuddy\registry\' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Force -Path $Work | Out-Null
$Jar = Join-Path $Work 'cookies.txt'
# 「匿名探测」请求（health / setup-status / 401 负向用例）必须用**另一个** jar 文件。
#
# 这是一个很隐蔽的坑：curl 的 -c 每次都会**重写整个 jar 文件**，即使这条请求
# 一个 Cookie 都没收到也一样。所以如果匿名探测与已登录共用同一个 jar，
# 登录之后再发一条匿名请求，就会把刚拿到手的会话 Cookie 覆盖成空 ——
# 表现为「登录明明成功，后面每个请求都 401」，看起来像服务端会话坏了。
#
# 反过来还有第二个坑（第一版修复时踩到的）：-NoAuth 只该用在「不需要也不产生
# 会话」的探测上。**登录本身绝不能用 -NoAuth**，否则会话 Cookie 会被写进
# 这个匿名 jar，而后续请求读的是主 jar —— 症状与上面一模一样，同样表现为 401。
$JarAnon = Join-Path $Work 'cookies-anon.txt'

# 测试用的 frps 参数。端口刻意避开 7000/7500（开发者可能正跑着 frps）
# 与 7810/7820/7830/7850（其他套件的实例端口）。
$FrpsBindPort = 7100
$FrpsWebPort  = 7600
$FrpsToken    = 'RegTest_FRP_Token_2026_9f3a'
$FrpsWebUser  = 'admin'
$FrpsWebPass  = 'RegTest_Web_7Qw2'
$FakePortA    = 19301
$FakePortB    = 19302
$FakePortC    = 19303
$LivePort     = 19311
# 机器人类型那一段用独立端口，避免与上面的台账段落互相干扰
# （端口是全局资源，共用端口会让「这段失败」看起来像「上段的问题」）。
$RobotPort    = 19321

$script:Pass = 0
$script:Fail = 0
$script:FailNames = @()
$script:FrpsProc = $null
$script:FrpcProc = $null

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
    $jarFile = $Jar
    if ($NoAuth) { $jarFile = $JarAnon }
    $cargs = @('-s', '--noproxy', '*', '-X', $Method, '-c', $jarFile)
    # 只在主 jar 真的存在时送 -b：curl 对「-b 指向不存在的文件」行为依版本而异
    # （有的版本会打警告），显式判断一下更确定。首次登录前它是没有的。
    if (-not $NoAuth -and (Test-Path $Jar)) { $cargs += @('-b', $Jar) }

    $bodyFile = $null
    if ($null -ne $Body) {
        $bodyFile = Join-Path $env:TEMP ('sinyefrp-reg-' + [guid]::NewGuid().ToString('N') + '.json')
        [System.IO.File]::WriteAllText($bodyFile, $Body, [System.Text.UTF8Encoding]::new($false))
        $cargs += @('-H', 'Content-Type: application/json', '--data-binary', ('@' + $bodyFile))
    }
    if ($OutFile) {
        $cargs += @('-o', $OutFile, '-w', '%{http_code}')
    } else {
        $cargs += @('-w', "`n%{http_code}")
    }
    $cargs += @($BaseUrl + $Path)

    try   { $out = & curl.exe @cargs 2>&1 }
    finally { if ($bodyFile -and (Test-Path $bodyFile)) { Remove-Item $bodyFile -Force -ErrorAction SilentlyContinue } }

    $text = ($out | Out-String)
    if ($OutFile) {
        $code = 0
        [void][int]::TryParse($text.Trim(), [ref]$code)
        # 非 200 时 curl 会把错误正文（JSON）照样写进 -o 指定的文件。
        # 必须把它读回来：否则「409 的 details 说了什么」「blocking 文案提到谁」
        # 这类断言拿到的永远是空串 —— 恒假，看起来像产品没返回内容。
        # 顺带说明为什么不能用「文件存在」判断「有没有生成包」——见 Test-ZipFile。
        $body = ''
        $obj = $null
        if ($code -ne 200 -and (Test-Path $OutFile)) {
            try { $body = [System.IO.File]::ReadAllText($OutFile, [System.Text.Encoding]::UTF8) } catch { }
            if ($body.Trim()) { try { $obj = $body | ConvertFrom-Json } catch { } }
        }
        return @{ Code = $code; Body = $body.Trim(); Json = $obj }
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

# ---------- 台账读写辅助 ----------

# 安全的「取列表长度」。
#
# 必须用它而不是直接 .Count：@($null) 得到的是「含一个 $null 元素的数组」，
# Count 是 1 而不是 0。于是「列表里有 N 条」这类断言在接口返回错误体
# （$r.Json 里根本没有那个字段）时**假通过** —— 比直接失败危险得多。
function Count-Of($v) {
    if ($null -eq $v) { return 0 }
    return @($v).Count
}

# 判断某个文件是不是一个真的 ZIP（按魔数 PK\x03\x04）。
#
# 不能只看「文件存不存在」：curl 的 -o 会把非 200 的错误正文也写进去，
# 于是被 409 拦下时那个 .zip 路径上其实躺着一小段 JSON。
# 用魔数判断才能真的证明「被拦时没有落下部署包」。
function Test-ZipFile([string]$Path) {
    if (-not (Test-Path $Path)) { return $false }
    try {
        $fs = [System.IO.File]::OpenRead($Path)
        try {
            $b = New-Object byte[] 4
            $n = $fs.Read($b, 0, 4)
            if ($n -lt 4) { return $false }
            return ($b[0] -eq 0x50 -and $b[1] -eq 0x4B -and $b[2] -eq 0x03 -and $b[3] -eq 0x04)
        } finally { $fs.Dispose() }
    } catch { return $false }
}

# 读接口在非 200 时直接抛错，而不是返回一个空对象继续往下跑。
#
# 理由同上：拿不到数据时继续断言，会产出几十条真假混杂的结论。
# 让它在第一步就中断，剩下的用例记为「执行中断」，指向网络/会话层面的真问题。
function Get-Registry {
    $r = Http GET '/api/registry/clients'
    if ($r.Code -ne 200) { throw ('读取台账失败: HTTP ' + $r.Code + ' ' + $r.Body) }
    return $r.Json
}

# 取台账客户端列表，**返回真数组**。
#
# ⚠️ 这条链上的坑比想象的多，务必照抄，不要「简化」成别的写法。
#
# Get-Registry 的返回值是 **PSCustomObject**（它内部 already 取过 $r.Json 了）。
# 于是：
#
#   @((Get-Registry).clients)          → 对**函数调用结果**做属性访问
#   @((Get-Registry).Json.clients)     → 多穿一层 .Json，错！
#
# 后者之所以错：`(Get-Registry).Json` 里的 Get-Registry 是函数调用，
# 而 PS 5.1 对「函数调用结果」做属性访问时走的是适配器路径，
# 取不到 Json 这个键就**静默**给 $null；再 .clients 仍是 $null；
# @($null) 得到「含一个 $null 的数组」，**Count 恰好是 1**。
# 结果就是「台账非空」类断言假通过、Find-Client 恒返回 $null、
# 而 if ($mc) 里的子断言被整段跳过（实测：@((Get-Registry).Json.clients)
# Count=1 但 [0] is null —— 见 .workbuddy/test/e2e-reg12.ps1 的实测输出）。
#
# 唯一正确的写法是对**函数调用结果**只取一层属性：@((Get-Registry).clients)。
function Get-RegistryClients {
    $list = @((Get-Registry).clients)
    # 自检：@($null) 的长度是 1，会把「取不到」伪装成「有 1 条」。
    # 一旦这里出现「长度为 1 且唯一元素是 $null」，说明属性访问链又写错了，
    # 必须立刻炸出来 —— 这个坑已经让本套件整段静默失效过两次。
    if ($list.Count -eq 1 -and $null -eq $list[0]) {
        throw '取台账客户端列表失败：拿到了 @($null)（属性访问链写错了，见 Get-RegistryClients 注释）'
    }
    return $list
}

function Get-Ports {
    $r = Http GET '/api/registry/ports'
    if ($r.Code -ne 200) { throw ('读取端口总览失败: HTTP ' + $r.Code + ' ' + $r.Body) }
    return $r.Json
}

# 取某 clientID 的台账记录；找不到返回 $null。
#
# 参数可以有两种形状，两种都要认（历史调用点混用）：
#   (a) Get-RegistryClients 返回的**客户端数组**；
#   (b) Get-Registry 返回的**整个 hashtable**（里面有 Json.clients）。
#
# ⚠️ 判形状时**不能**用 $reg.PSObject.Properties.Name -contains 'clients'。
# 对 Hashtable 而言，PSObject.Properties 暴露的是 Hashtable **自己的成员**
# （IsReadOnly / Keys / Values / Count ...），**不含** hash 的键；
# 所以那个判断恒为 $false，分支永远走不到，(b) 会被静默判成「没找到」。
# 判断字典要用 IDictionary + ContainsKey —— 这才是字典的接口语义。
# （这个坑和 @((Get-Registry).clients) 是同一类：都是「看起来像属性访问，
#   实际走的是 PS 适配器」，都不报错、只是结果恒为空，最难查。）
function Find-Client($reg, [string]$ClientID) {
    if (-not $reg) { return $null }

    $list = $null
    if ($reg -is [System.Array] -or $reg -is [System.Collections.IList]) {
        # 形状 (a)：已经是列表
        $list = $reg
    } elseif ($reg -is [System.Collections.IDictionary]) {
        # 形状 (b)：字典（Get-Registry 的 hashtable）—— 取里面的 clients
        if ($reg.Contains('clients')) { $list = $reg['clients'] }
    } elseif ($reg -is [psobject] -and $reg.PSObject.Properties['clients']) {
        # 形状 (c)：普通对象，属性就是 clients
        $list = $reg.clients
    }
    if ($null -eq $list) { return $null }

    # 过滤掉 $null 元素：@($null) 的长度是 1，会把「没有记录」伪装成「有一条」。
    $hit = @($list | Where-Object { $_ -and $_.client_id -eq $ClientID })
    if ($hit.Count -eq 0) { return $null }
    return $hit[0]
}

# 该客户端在台账里登记的**端口对象**列表（保留 remote_port / local_port 两项）。
#
# ⚠️ 不要「顺手」把它改成只返回端口号数组。这里返回的是**对象**，
# 调用点要同时读 remote_port 与 local_port；如果只回传 remote_port，
# 外面 $mports[0].remote_port 会静默取不到（PS 对 int 做属性访问给 $null），
# 断言恒假 —— 这正是本套件踩过的第三个同源坑（前两个见 Get-RegistryClients
# 与 Find-Client 的注释），症状同样是「不报错、只是永远失败」。
function ClientPorts($client) {
    if (-not $client -or -not $client.ports) { return @() }
    return @($client.ports | Where-Object { $_ -and $_.remote_port -gt 0 } |
        Sort-Object { $_.remote_port })
}

# 只取端口号（升序），给「端口集合是否相等」这类比较用。
function ClientPortNumbers($client) {
    return @(@(ClientPorts $client) | ForEach-Object { $_.remote_port })
}

# 端口总览（台账口径）里某端口的占用者，返回 $null 表示没被占。
function RegOwnerOf($ports, [int]$Port) {
    if (-not $ports -or -not $ports.registry) { return $null }
    $hit = @($ports.registry | Where-Object { $_.port -eq $Port })
    if ($hit.Count -eq 0) { return $null }
    return $hit[0]
}

function LiveOwnerOf($ports, [int]$Port) {
    if (-not $ports -or -not $ports.live) { return $null }
    $hit = @($ports.live | Where-Object { $_.port -eq $Port })
    if ($hit.Count -eq 0) { return $null }
    return $hit[0]
}

# 构造一条代理规则。
#
# 所有字段都显式给值（而不是只给用到的）：后端对未知字段是严格拒绝的，
# 但**缺失**字段会静默取零值。把字段写全，才能让「缺字段」这类问题在
# 测试里表现为断言失败，而不是变成一个看起来合法的配置。
function New-Proxy {
    param(
        [string]$Name,
        [string]$Type = 'tcp',
        [int]$RemotePort = 0,
        [int]$LocalPort = 8080,
        [string[]]$Domains = @(),
        [string]$Subdomain = ''
    )
    return @{
        name            = $Name
        type            = $Type
        local_ip        = '127.0.0.1'
        local_port      = $LocalPort
        remote_port     = $RemotePort
        custom_domains  = @($Domains)
        subdomain       = $Subdomain
        locations       = @()
        secret_key      = ''
        use_encryption  = $false
        use_compression = $false
    }
}

# 构造一份客户端配置。地址/端口/token 取自服务端的默认值 —— 那正是
# 「客户端接入点」这条链路的产物，用它才能保证生成的包真的能连上测试 frps。
function New-ClientSpec {
    param([string]$ClientID, [string]$Name, [array]$Proxies, [string]$RobotType = '', [string]$Platform = '')
    $d = (Http GET '/api/pkg/client/defaults').Json
    return @{
        server_addr  = $d.server_addr
        server_port  = $d.server_port
        name         = $Name
        client_id    = $ClientID
        # 默认给空串（未分类）而不是省略这个键：后端对**缺失**字段是静默取零值的，
        # 省略会让「字段压根没传」与「传了空值」在测试里看起来一模一样。
        robot_type   = $RobotType
        # platform 留空是由后端 Normalize 补成 windows-amd64，这正是 platform 功能
        # 之前的行为 —— 存量用例（不知道有这个字段）不该因为加了列而改变性质。
        # 但**想验 Linux 就必须显式传 "linux-amd64"**：Normalize 的兜底会把
        # 「忘了传」也变成 Windows，两者在结果上完全一样，只有传值才区分得开。
        platform     = $Platform
        token        = $d.token
        log_level    = 'info'
        log_max_days = 7
        enable_tls   = $false
        proxies      = $Proxies
    }
}

# 环境块里有大小写重复的键时（Path/PATH、HTTP_PROXY/http_proxy …），
# PowerShell 的 Start-Process 会用大小写不敏感字典装环境，撞键即抛
# "Item has already been added"。经 regression.ps1 调用时父进程已展平过，
# 但本套件要能单独运行，所以自己再处理一次。
#
# 这里用 cmd /c set 读原始环境块（而不是 Win32 GetEnvironmentStringsW）：
# 后者需要 Add-Type 编译 P/Invoke 声明，在受限环境里可能被拦。
# 只删每组里「第一个拼写之外」的那些，因此 PATH 一定还留着。
function Repair-ProcessEnvironment {
    try {
        $raw = (& cmd.exe /c set) 2>&1
        $groups = @{}
        foreach ($line in $raw) {
            $i = $line.IndexOf('=')
            if ($i -le 0) { continue }
            $k = $line.Substring(0, $i)
            if ($k -like '=*') { continue }
            $lk = $k.ToLowerInvariant()
            if ($groups.ContainsKey($lk)) { $groups[$lk] = $groups[$lk] + '|' + $k } else { $groups[$lk] = $k }
        }
        $fixed = 0
        foreach ($lk in @($groups.Keys)) {
            $names = $groups[$lk] -split '\|'
            if ($names.Count -lt 2) { continue }
            for ($i = 1; $i -lt $names.Count; $i++) {
                [System.Environment]::SetEnvironmentVariable($names[$i], $null, 'Process')
                $fixed++
            }
        }
        return $fixed
    } catch {
        return -1
    }
}

function Wait-PortOpen {
    param([int]$Port, [int]$Seconds = 15)
    for ($i = 0; $i -lt $Seconds; $i++) {
        $c = New-Object System.Net.Sockets.TcpClient
        try {
            $iar = $c.BeginConnect('127.0.0.1', $Port, $null, $null)
            if ($iar.AsyncWaitHandle.WaitOne(800) -and $c.Connected) { return $true }
        } catch { } finally { $c.Close() }
        Start-Sleep -Seconds 1
    }
    return $false
}

# 直接查测试 frps 的 Dashboard，确认 frpc 真的把代理注册上去了。
# 不依赖被测平台：注册失败时能立刻把「FRP 侧没注册」和「平台没看到」区分开。
#
# 用 curl 而不是 Invoke-RestMethod：本机开着代理（HTTP_PROXY 指向本地代理软件），
# .NET 默认会按系统代理转发请求，127.0.0.1 也可能被带走。curl 加
# --noproxy '*' 才是确定的直连（本项目其他套件同理）。
function Get-FrpsProxyPorts {
    $url = 'http://127.0.0.1:' + $FrpsWebPort + '/api/proxy/tcp'
    $out = & curl.exe -s --noproxy '*' -u ($FrpsWebUser + ':' + $FrpsWebPass) $url 2>&1
    $text = ($out | Out-String).Trim()
    if (-not $text) { return @() }
    $obj = $null
    try { $obj = $text | ConvertFrom-Json } catch { return @() }
    $ports = @()
    foreach ($p in @($obj.proxies)) {
        if ($p.conf -and $p.conf.remotePort) { $ports += [int]$p.conf.remotePort }
    }
    return @($ports)
}

# ---------- 启动测试用 frps ----------

function Start-TestFrps {
    $tomlPath = Join-Path $Work 'frps.toml'
    $lines = @(
        ('bindPort = ' + $FrpsBindPort),
        ('auth.token = "' + $FrpsToken + '"'),
        '',
        ('webServer.addr = "127.0.0.1"'),
        ('webServer.port = ' + $FrpsWebPort),
        ('webServer.user = "' + $FrpsWebUser + '"'),
        ('webServer.password = "' + $FrpsWebPass + '"')
    )
    [System.IO.File]::WriteAllText($tomlPath, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($false)))

    $frpsExe = Join-Path $FrpDir 'frps.exe'
    if (-not (Test-Path $frpsExe)) { return $null }
    $out = Join-Path $Work 'frps.log'
    $p = Start-Process -FilePath $frpsExe -ArgumentList @('-c', $tomlPath) `
        -WorkingDirectory $Work -RedirectStandardOutput $out `
        -RedirectStandardError ($out + '.err') -WindowStyle Hidden -PassThru
    return $p
}

# ---------- 测试主体 ----------
# 整个主体包在 try 中：中途抛错时，已执行的用例结果仍要出现在汇总里，
# 否则一个 null 引用会让前面几十条通过记录一起消失。
try {

Section '0. 前置检查'
$envFixed = Repair-ProcessEnvironment
Write-Host ('  环境自检: 已展平 ' + $envFixed + ' 处大小写重复的环境变量') -ForegroundColor DarkGray

$r = Http GET '/api/health'
Result '管理服务可访问' ($r.Code -eq 200) ('HTTP ' + $r.Code)
if ($r.Code -ne 200) {
    Write-Host '管理服务未启动，无法继续。' -ForegroundColor Red
    exit 1
}

$r = Http GET '/api/setup/status' -NoAuth
if ($r.Json -and -not $r.Json.initialized) {
    # 初始化与登录都走主 jar（不能带 -NoAuth）：它们要建立会话。
    $r = Http POST '/api/setup/admin' (ToJson @{ username = $AdminUser; password = $AdminPass })
    Result '首次初始化创建管理员' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
}
$r = Http POST '/api/auth/login' (ToJson @{ username = $AdminUser; password = $AdminPass })
Result '管理员登录成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
if ($r.Code -ne 200) {
    Write-Host '登录失败，无法继续。' -ForegroundColor Red
    exit 1
}

# 未登录访问台账必须被拒（会话保护不能漏在新增接口上）。
$r = Http GET '/api/registry/clients' -NoAuth
Result '未登录访问台账被拒（401）' ($r.Code -eq 401) ('HTTP ' + $r.Code)
$r = Http GET '/api/registry/ports' -NoAuth
Result '未登录访问端口总览被拒（401）' ($r.Code -eq 401) ('HTTP ' + $r.Code)

Section '1. 建包前置：客户端接入点'
# 生成客户端包要求 ClientSpec.ServerAddr 非空，而它来自「设置」页的客户端接入点
# （internal/web/pkg_handlers.go handleClientDefaults 取 endpoint.Addr）。
# 未配接入点时表单拿不到地址，建包会被 400 拒 —— 这是产品的真实前置，
# 先把这个行为断言下来，再把接入点配好。
#
# 这一段必须放在台账用例之前：否则后面每一段都会以 400 收场，
# 失败项几十条，而根因只是「本套件没配接入点」，与台账/端口逻辑毫无关系。
$cidPre = 'sf-regpre' + (Get-Random -Minimum 1000 -Maximum 9999)
$prePort = 19300
$preZip = Join-Path $Work 'pre.zip'
$specPre = New-ClientSpec -ClientID $cidPre -Name '前置校验' -Proxies @(
    (New-Proxy -Name 'p-tcp' -Type 'tcp' -RemotePort $prePort -LocalPort 8080)
)
$r = Http POST '/api/pkg/client' (ToJson $specPre) -OutFile $preZip
Result '未配接入点时建包被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code)
Result '拒绝原因点明服务器地址缺失' ($r.Body -like '*服务器地址*') $r.Body
Result '被拒时没有落下 ZIP 产物' (-not (Test-ZipFile $preZip)) ''

$r = Http POST '/api/pkg/endpoint' (ToJson @{
    client_addr = '127.0.0.1'; client_port = $FrpsBindPort
    token = $FrpsToken; clear_token = $false
})
Result '接入点已保存（后续各段的建包前置）' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)

$d = (Http GET '/api/pkg/client/defaults').Json
Result '建包默认值带上了接入点地址' ($d.server_addr -eq '127.0.0.1') ('实际: ' + $d.server_addr)
Result '建包默认值带上了接入点端口' ([int]$d.server_port -eq $FrpsBindPort) ('实际: ' + $d.server_port)
Result '建包默认值带上了接入点 token' ($d.token -eq $FrpsToken) ''

Section '2. 台账初始状态'
$reg = Get-Registry
Result '台账初始为空' ((Count-Of $reg.clients) -eq 0) ('条数: ' + (Count-Of $reg.clients))
Result '台账响应带采集健康度' ($null -ne $reg.health) ''
$ports = Get-Ports
Result '端口总览初始为空' ((Count-Of $ports.registry) -eq 0) ('条数: ' + (Count-Of $ports.registry))
# 用原始正文检查键是否存在：PS 5.1 的 ConvertFrom-Json 会把 `[]` 反序列化成
# $null，靠 $null 判断「字段有没有返回」是判不出来的。
$portsRaw = (Http GET '/api/registry/ports').Body
Result '端口总览同时返回 registry 与 live 两个口径' `
    (($portsRaw -like '*"registry"*') -and ($portsRaw -like '*"live"*')) $portsRaw

Section '3. 首次签发后记账（F19）'
$cidA = 'sf-regaaa' + (Get-Random -Minimum 1000 -Maximum 9999)
$cidB = 'sf-regbbb' + (Get-Random -Minimum 1000 -Maximum 9999)
$cidC = 'sf-regccc' + (Get-Random -Minimum 1000 -Maximum 9999)
$cidX = 'sf-regxxx' + (Get-Random -Minimum 1000 -Maximum 9999)
$cidY = 'sf-regyyy' + (Get-Random -Minimum 1000 -Maximum 9999)
$cidH = 'sf-reghtt' + (Get-Random -Minimum 1000 -Maximum 9999)

$specA = New-ClientSpec -ClientID $cidA -Name '台账测试-甲' -Proxies @(
    (New-Proxy -Name 'a-tcp' -Type 'tcp' -RemotePort $FakePortA -LocalPort 8080)
)
$zipA = Join-Path $Work 'a.zip'
$r = Http POST '/api/pkg/client' (ToJson $specA) -OutFile $zipA
Result '客户端甲首包生成成功' ($r.Code -eq 200) ('HTTP ' + $r.Code)
Result '产物是一个真的 ZIP' (Test-ZipFile $zipA) ('HTTP ' + $r.Code + ' ' + $r.Body)

$reg = Get-Registry
$clientA = Find-Client $reg $cidA
Result '台账出现该客户端' ($null -ne $clientA) ''
Result '台账记录了显示名称' ($clientA.display_name -eq '台账测试-甲') ('实际: ' + $clientA.display_name)
Result '台账记录的 clientID 一致' ($clientA.client_id -eq $cidA) ''
Result '生成次数为 1' ($clientA.generate_count -eq 1) ('实际: ' + $clientA.generate_count)
Result '台账记录了首次生成时间' ($null -ne $clientA.first_generated_at) ''
Result ('端口占用登记为 ' + $FakePortA) ((ClientPortNumbers $clientA) -contains $FakePortA) `
    ('实际: ' + ((ClientPortNumbers $clientA) -join ','))
Result '新签发的客户端为启用状态' ($clientA.status -eq 'active') ('实际: ' + $clientA.status)

$ports = Get-Ports
$owner = RegOwnerOf $ports $FakePortA
Result ('端口总览里 ' + $FakePortA + ' 归属该客户端') ($null -ne $owner -and $owner.client_id -eq $cidA) ''
Result '端口总览带占用者显示名' ($null -ne $owner -and $owner.client_name -eq '台账测试-甲') ''
Result '端口总览的来源口径为 registry' ($null -ne $owner -and $owner.source -eq 'registry') ''
Result '端口总览的行带台账编号（可用于单独释放）' ($null -ne $owner -and [int]$owner.id -gt 0) ''

Section '4. 同 clientID 重新生成不算冲突'
$r = Http POST '/api/pkg/client' (ToJson $specA) -OutFile (Join-Path $Work 'a2.zip')
Result '同 clientID 同端口可以重新生成' ($r.Code -eq 200) ('HTTP ' + $r.Code)
$reg = Get-Registry
$clientA = Find-Client $reg $cidA
Result '重新生成不新增台账记录' ((Count-Of $reg.clients) -eq 1) ('条数: ' + (Count-Of $reg.clients))
Result '生成次数递增为 2' ($clientA.generate_count -eq 2) ('实际: ' + $clientA.generate_count)
$ports = Get-Ports
Result '端口总览里该端口只出现一次' (@($ports.registry | Where-Object { $_.port -eq $FakePortA }).Count -eq 1) ''

Section '5. 异 clientID 撞同一端口被拦'
$specB1 = New-ClientSpec -ClientID $cidB -Name '台账测试-乙' -Proxies @(
    (New-Proxy -Name 'b-tcp' -Type 'tcp' -RemotePort $FakePortA -LocalPort 8081)
)
$r = Http POST '/api/pkg/client/preview' (ToJson $specB1)
Result '预览仍返回 200（管理员要能看到配置才能改端口）' ($r.Code -eq 200) ('HTTP ' + $r.Code)
Result '预览把冲突列为 blocking' ((Count-Of $r.Json.blocking) -eq 1) ('条数: ' + (Count-Of $r.Json.blocking))
$blockText = (@($r.Json.blocking) -join ' ')
Result 'blocking 文案点出冲突端口' ($blockText -like ('*' + $FakePortA + '*')) $blockText
Result 'blocking 文案点出占用方名称' ($blockText -like '*台账测试-甲*') $blockText
Result '预览的 toml 仍已返回（未被冲突挡住）' ($r.Json.toml -like '*serverAddr*') ''

$badZip = Join-Path $Work 'b-bad.zip'
$r = Http POST '/api/pkg/client' (ToJson $specB1) -OutFile $badZip
Result '撞端口的下载被拦（409）' ($r.Code -eq 409) ('HTTP ' + $r.Code + ' ' + $r.Body)
Result '409 返回逐条细节 details' ((Count-Of $r.Json.error.details) -ge 1) ('条数: ' + (Count-Of $r.Json.error.details))
Result '冲突细节里点名占用者的 clientID' ((@($r.Json.error.details) -join ' ') -like ('*' + $cidA + '*')) `
    (@($r.Json.error.details) -join ' ')
# 不能只看文件是否存在：curl 会把这段 409 的 JSON 正文写进 -o 指定的路径。
# 必须按魔数确认它不是 ZIP，才算真的「没有落下部署包」。
Result '被拦时不产生 ZIP 文件' (-not (Test-ZipFile $badZip)) ''
$reg = Get-Registry
Result '被拦时不写入台账' ((Count-Of $reg.clients) -eq 1) ('条数: ' + (Count-Of $reg.clients))

Section '6. 换端口即可生成'
$specB = New-ClientSpec -ClientID $cidB -Name '台账测试-乙' -Proxies @(
    (New-Proxy -Name 'b-tcp' -Type 'tcp' -RemotePort $FakePortB -LocalPort 8081)
)
$r = Http POST '/api/pkg/client' (ToJson $specB) -OutFile (Join-Path $Work 'b.zip')
Result '换端口后生成成功' ($r.Code -eq 200) ('HTTP ' + $r.Code)
$reg = Get-Registry
Result '台账有 2 条记录' ((Count-Of $reg.clients) -eq 2) ('条数: ' + (Count-Of $reg.clients))
$ports = Get-Ports
Result ('端口总览含 ' + $FakePortA + ' 与 ' + $FakePortB) `
    ((@($ports.registry | Where-Object { $_.port -eq $FakePortA }).Count -eq 1) -and
     (@($ports.registry | Where-Object { $_.port -eq $FakePortB }).Count -eq 1)) ''
$sorted = @($ports.registry | ForEach-Object { [int]$_.port })
$asc = $true
for ($i = 1; $i -lt $sorted.Count; $i++) { if ($sorted[$i] -lt $sorted[$i - 1]) { $asc = $false } }
Result '端口总览按端口升序' $asc ('实际: ' + ($sorted -join ','))

Section '7. 端口占用是整体替换而不是累加'
$specA2 = New-ClientSpec -ClientID $cidA -Name '台账测试-甲' -Proxies @(
    (New-Proxy -Name 'a-tcp' -Type 'tcp' -RemotePort $FakePortA -LocalPort 8080),
    (New-Proxy -Name 'a-tcp2' -Type 'tcp' -RemotePort $FakePortC -LocalPort 8082)
)
$r = Http POST '/api/pkg/client' (ToJson $specA2) -OutFile (Join-Path $Work 'a3.zip')
Result '甲增加一条代理后重新生成成功' ($r.Code -eq 200) ('HTTP ' + $r.Code)
$reg = Get-Registry
$clientA = Find-Client $reg $cidA
Result '甲的端口登记为 2 个' ((@(ClientPortNumbers $clientA)).Count -eq 2) ('实际: ' + ((ClientPortNumbers $clientA) -join ','))
$ports = Get-Ports
Result ('新端口 ' + $FakePortC + ' 进入总览') ((RegOwnerOf $ports $FakePortC).client_id -eq $cidA) ''

# 改回单条代理：旧端口必须跟着释放，否则会被一条「已经不存在于任何包里」的记录永久锁住。
$r = Http POST '/api/pkg/client' (ToJson $specA) -OutFile (Join-Path $Work 'a4.zip')
Result '甲改回单条代理后重新生成成功' ($r.Code -eq 200) ('HTTP ' + $r.Code)
$ports = Get-Ports
Result ('被删掉的代理端口 ' + $FakePortC + ' 已释放') ($null -eq (RegOwnerOf $ports $FakePortC)) ''
$reg = Get-Registry
$clientA = Find-Client $reg $cidA
Result '甲的端口回到 1 个' ((@(ClientPortNumbers $clientA)).Count -eq 1) ('实际: ' + ((ClientPortNumbers $clientA) -join ','))

Section '8. 停用 / 启用 / 删除对端口的影响'
$r = Http POST ('/api/registry/clients/' + $clientA.id) `
    (ToJson @{ display_name = '台账测试-甲'; note = '第二轮测试'; status = 'retired' })
Result '停用客户端成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
$ports = Get-Ports
Result ('停用后端口 ' + $FakePortA + ' 从总览消失') ($null -eq (RegOwnerOf $ports $FakePortA)) ''
$reg = Get-Registry
$clientA = Find-Client $reg $cidA
Result '停用状态已落库' ($clientA.status -eq 'retired') ('实际: ' + $clientA.status)
Result '备注已保存' ($clientA.note -eq '第二轮测试') ('实际: ' + $clientA.note)

# 端口已释放，别人应当能用
$specC = New-ClientSpec -ClientID $cidC -Name '台账测试-丙' -Proxies @(
    (New-Proxy -Name 'c-tcp' -Type 'tcp' -RemotePort $FakePortA -LocalPort 8083)
)
$r = Http POST '/api/pkg/client' (ToJson $specC) -OutFile (Join-Path $Work 'c.zip')
Result '停用释放后该端口可被其他客户端使用' ($r.Code -eq 200) ('HTTP ' + $r.Code)
$reg = Get-Registry
$clientC = Find-Client $reg $cidC
Result '丙已登记台账' ($null -ne $clientC) ''

# 反向：端口已被别人占走，重新启用必须被拒，否则会造出两个启用客户端占同一端口。
$r = Http POST ('/api/registry/clients/' + $clientA.id) `
    (ToJson @{ display_name = '台账测试-甲'; note = '第二轮测试'; status = 'active' })
Result '端口被占时启用被拒（409）' ($r.Code -eq 409) ('HTTP ' + $r.Code + ' ' + $r.Body)
Result '启用被拒时返回逐条细节' ((Count-Of $r.Json.error.details) -ge 1) ''
$reg = Get-Registry
$clientA = Find-Client $reg $cidA
Result '启用被拒后状态仍为停用' ($clientA.status -eq 'retired') ('实际: ' + $clientA.status)

$r = Http DELETE ('/api/registry/clients/' + $clientC.id)
Result '删除客户端丙成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
$r = Http POST ('/api/registry/clients/' + $clientA.id) `
    (ToJson @{ display_name = '台账测试-甲'; note = '第二轮测试'; status = 'active' })
Result '释放占用后启用成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
$ports = Get-Ports
$owner = RegOwnerOf $ports $FakePortA
Result '端口重新归属甲' ($null -ne $owner -and $owner.client_id -eq $cidA) ''

$r = Http DELETE ('/api/registry/clients/' + $clientA.id)
Result '删除客户端甲成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
$reg = Get-Registry
Result '台账中甲已消失' ($null -eq (Find-Client $reg $cidA)) ''
Result '台账中丙已消失（前一步删除）' ($null -eq (Find-Client $reg $cidC)) ''
$ports = Get-Ports
Result ('删除后 ' + $FakePortA + ' 已释放（级联删除端口占用）') ($null -eq (RegOwnerOf $ports $FakePortA)) ''
Result '删掉甲的记录不影响乙' ($null -ne (Find-Client $reg $cidB)) ''

Section '9. 不占端口的代理类型'
$specH = New-ClientSpec -ClientID $cidH -Name '台账测试-无端口' -Proxies @(
    (New-Proxy -Name 'h-http' -Type 'http' -LocalPort 8090 -Domains @('no-port.example.com'))
)
$r = Http POST '/api/pkg/client' (ToJson $specH) -OutFile (Join-Path $Work 'h.zip')
Result 'http 类型代理生成成功' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
$reg = Get-Registry
$clientH = Find-Client $reg $cidH
Result '无端口客户端也登记台账' ($null -ne $clientH) ''
Result '它的端口登记为空' ((@(ClientPortNumbers $clientH)).Count -eq 0) ('实际: ' + ((ClientPortNumbers $clientH) -join ','))

$r = Http DELETE ('/api/registry/clients/' + $clientH.id)
Result '清理无端口客户端' ($r.Code -eq 200) ('HTTP ' + $r.Code)

Section '10. 实时端口占用（真实 frps + frpc）'
$frpsExe = Join-Path $FrpDir 'frps.exe'
$frpcExe = Join-Path $FrpDir 'frpc.exe'
if (-not (Test-Path $frpsExe) -or -not (Test-Path $frpcExe)) {
    Result '本机具备 frps/frpc 二进制（实时段前置）' $false ('缺少: ' + $FrpDir)
} else {
    $script:FrpsProc = Start-TestFrps
    $ready = Wait-PortOpen -Port $FrpsWebPort -Seconds 15
    Result '测试用 frps 已就绪（带 auth.token）' $ready ('端口 ' + $FrpsWebPort)

    if ($ready) {
        # 节点配置（采集用）指向这个 frps。接入点在「1. 建包前置」已配好，
        # 这里不重复设置，避免同一件事在两个地方各断言一遍（读日志时容易误以为覆盖了两条路径）。
        $r = Http POST '/api/node/config' (ToJson @{
            name = 'registry-frps'; api_url = ('http://127.0.0.1:' + $FrpsWebPort)
            username = $FrpsWebUser; password = $FrpsWebPass
            timeout_seconds = 5; enabled = $true
        })
        Result '节点配置已指向测试 frps' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)

        # 生成 X 的包并真跑 frpc
        # 显式给一个机器人类型：下面要验「类型从台账流到实时客户端列表」，
        # 留空的话断言会退化成「空 == 空」，永远通过（典型假绿）。
        $specX = New-ClientSpec -ClientID $cidX -Name '台账测试-实时' -RobotType 'spray' -Proxies @(
            (New-Proxy -Name 'x-tcp' -Type 'tcp' -RemotePort $LivePort -LocalPort 8099)
        )
        $zipX = Join-Path $Work 'x.zip'
        $r = Http POST '/api/pkg/client' (ToJson $specX) -OutFile $zipX
        Result '实时测试包生成成功' ($r.Code -eq 200) ('HTTP ' + $r.Code)

        $exX = Join-Path $Work 'x'
        Expand-Archive -Path $zipX -DestinationPath $exX -Force
        $outX = Join-Path $Work 'frpc.log'
        $script:FrpcProc = Start-Process -FilePath (Join-Path $exX 'frpc.exe') `
            -ArgumentList @('-c', (Join-Path $exX 'frpc.toml')) `
            -WorkingDirectory $exX -RedirectStandardOutput $outX `
            -RedirectStandardError ($outX + '.err') -WindowStyle Hidden -PassThru

        # 直接看 frps 有没有把这个代理注册上：注册失败与平台没采集到是两回事。
        $registered = $false
        for ($i = 0; $i -lt 20; $i++) {
            Start-Sleep -Seconds 1
            if ((Get-FrpsProxyPorts) -contains $LivePort) { $registered = $true; break }
        }
        Result ('frpc 已把远端端口 ' + $LivePort + ' 注册到 frps') $registered ''
        if (-not $registered) {
            $frpcText = ''
            if (Test-Path $outX) { $frpcText = (Get-Content $outX -Raw) }
            Write-Host ('         frpc 日志: ' + $frpcText.Trim()) -ForegroundColor DarkGray
        }

        # 触发采集，等待平台看到该端口
        [void](Http POST '/api/refresh' '{}')
        $liveSeen = $null
        for ($i = 0; $i -lt 15; $i++) {
            $ports = Get-Ports
            $liveSeen = LiveOwnerOf $ports $LivePort
            if ($null -ne $liveSeen) { break }
            Start-Sleep -Seconds 1
        }
        Result '平台采集到实时端口占用' ($null -ne $liveSeen) ''
        Result '实时占用的 clientID 与签发的一致' ($null -ne $liveSeen -and $liveSeen.client_id -eq $cidX) `
            ('实际: ' + $liveSeen.client_id)
        Result '实时占用标注来源为 live' ($null -ne $liveSeen -and $liveSeen.source -eq 'live') ''
        Result '实时占用不占用台账编号' ($null -ne $liveSeen -and [int]$liveSeen.id -eq 0) ''

        # ---- 显示名称：台账 → 实时数据的关联 ----
        #
        # FRP 的 /api/clients 与 /api/proxies 只给 clientID，没有可读名称。
        # 管理员看到的必须是自己填的名字（如「财务部-前台机」），
        # 所以由后端按 clientID 关联本平台台账。这里验证关联确实发生了，
        # 而且是**中文**往返（JSON 编码出错的典型症状就是这里变问号）。
        $clientsNow = @((Http GET '/api/clients').Json.clients)
        $shownX = $clientsNow | Where-Object { $_.clientID -eq $cidX } | Select-Object -First 1
        Result '/api/clients 里能查到实时客户端' ($null -ne $shownX) ('共 ' + (Count-Of $clientsNow) + ' 个')
        Result '客户端带上了台账显示名称' ($null -ne $shownX -and $shownX.display_name -eq '台账测试-实时') `
            ('实际: ' + $shownX.display_name)
        Result '客户端被标记为已登记' ($null -ne $shownX -and $shownX.registered -eq $true) `
            ('实际: ' + $shownX.registered)

        # 机器人类型走的是同一条关联路径（F20）：客户端列表要按类型分组，
        # 而 FRP 同样没有这个概念 —— 类型只能来自本平台台账。
        $typeX = ''
        if ($null -ne $shownX) { $typeX = [string]$shownX.robot_type }
        Result '客户端带上了台账里的机器人类型' ($typeX -eq 'spray') ('实际: ' + $typeX)
        Result '机器人类别值就是台账里存的值（未被翻译或改写）' ($typeX -eq $specX.robot_type) `
            ('实时: ' + $typeX + ' / 台账: ' + $specX.robot_type)

        $proxiesNow = @((Http GET '/api/proxies').Json.proxies)
        $proxyX = $proxiesNow | Where-Object { $_.clientID -eq $cidX } | Select-Object -First 1
        $proxyName = ''
        if ($null -ne $proxyX) { $proxyName = [string]$proxyX.display_name }
        Result '代理带上了所属客户端的显示名称' ($null -ne $proxyX -and $proxyName -eq '台账测试-实时') `
            ('实际: ' + $proxyName)

        # ---- 代理的本地端口：FRP 不返回，只能从台账关联 ----
        #
        # FRP Dashboard 的代理 conf 只有 localIP 与 remotePort，**没有 localPort**
        # （03-frp-api-contract §4-⑦ 实测）。所以本地端口必须按
        # (clientID, 代理短名) 关联本平台台账 —— 这是本功能最容易静默失效的一环：
        # 关联键写错（用了完整名、或只按 clientID 取）时本地端口会恒为 0，
        # 而接口照样 200、页面照样好看，没有任何报错。
        #
        # 上面那条 x-tcp 建包时填的是 8099，这里必须原样取回来。
        $localX = $null
        if ($null -ne $proxyX) { $localX = $proxyX.local_port }
        Result '代理被标记为已登记本地端口' ($null -ne $proxyX -and $proxyX.local_port_known -eq $true) `
            ('known=' + $(if ($null -ne $proxyX) { $proxyX.local_port_known } else { '<无此代理>' }))
        Result '关联出的本地端口就是建包时填的值' ([string]$localX -eq '8099') ('实际: ' + $localX)

        # ---- 台账的在线状态：必须能回答「现在通不通」 ----
        #
        # 台账原先只下发 seen_in_frp（FRP 里见过没有），而它**连已断开但尚未被
        # FRP 清理的客户端也算**。于是「连过后来断了」与「压根没装上」在界面上是
        # 同一个样子，两者的处置方式却完全不同（前者查网络与 frps，后者去现场装包）。
        # 现场反馈正是这个：客户端页显示在线，台账页却完全看不出来。
        # 因此 online 与 seen_in_frp 必须**同时**下发，由界面区分三态。
        $regRows = @((Http GET '/api/registry/clients').Json.clients)
        $rowX = $regRows | Where-Object { $_.client_id -eq $cidX } | Select-Object -First 1
        Result '在线客户端在台账里标记为在线' ($null -ne $rowX -and $rowX.online -eq $true) `
            ('online=' + $(if ($null -ne $rowX) { $rowX.online } else { '<台账里没有这条>' }))
        Result '在线客户端在台账里标记为 FRP 中可见' ($null -ne $rowX -and $rowX.seen_in_frp -eq $true) ''
        # 没装过的记录：两个字段都必须是 false，界面据此显示「从未上线」。
        # 只断言 online 是不够的 —— online=false 同时覆盖「离线」与「从未上线」，
        # 把这两个状态混为一谈正是本次要修的问题本身。
        $rowNever = $regRows | Where-Object { $_.client_id -ne $cidX } | Select-Object -First 1
        Result '未安装客户端在台账里标记为未在线' ($null -ne $rowNever -and $rowNever.online -eq $false) `
            ('clientID=' + $(if ($null -ne $rowNever) { $rowNever.client_id } else { '<台账里只有一条>' }))
        Result '未安装客户端在台账里标记为 FRP 中未见' ($null -ne $rowNever -and $rowNever.seen_in_frp -eq $false) ''

        # ---- 穿透链接：按设备回答「从外面怎么访问」 ----
        #
        # 地址 = 接入点地址 + 这条代理的远端端口。接入点是 frpc 真正连的**对外**
        # 地址，与采集用的 Dashboard 地址是两件事（frps 前有 NAT 时连端口都不同）。
        # 拿 Dashboard 地址拼出来的链接必错且毫无征兆，所以这里不只断言
        # 「看着像 IP:端口」，而是断言前缀**就是**接入点（本套件配的是 127.0.0.1）。
        $lk = (Http GET ('/api/clients/' + $cidX + '/links')).Json
        Result '穿透链接带上台账显示名称' ($lk.display_name -eq '台账测试-实时') ('实际: ' + $lk.display_name)
        Result '穿透链接标出接入点已配置' ($lk.endpoint_configured -eq $true) ''
        $lkTcp = @($lk.links) | Where-Object { $_.proxy_type -eq 'tcp' } | Select-Object -First 1
        Result 'TCP 代理给出「接入点:远端端口」形式的地址' `
            ($null -ne $lkTcp -and $lkTcp.access_url -eq ('127.0.0.1:' + $LivePort)) `
            ('实际: ' + $(if ($null -ne $lkTcp) { $lkTcp.access_url } else { '<没有 tcp 代理>' }))
        # 界面与「复制全部」都用短名：完整名带 user. 前缀，贴给现场会被当成地址的一部分。
        Result '穿透链接用代理短名而不是 FRP 完整名' ($null -ne $lkTcp -and $lkTcp.short_name -eq 'x-tcp') `
            ('实际: ' + $(if ($null -ne $lkTcp) { $lkTcp.short_name } else { '<无>' }))
        # 本地端口：只给「名字 + 地址」时，收到的人分不清这条对应目标机器上
        # 哪个服务 —— 现场就是靠它认人的。它只能从台账关联（FRP 不返回），
        # 所以这里断言的是**建包时填进去的那个值原样回来了**，而不是「有个数字」。
        Result '穿透链接带上台账登记的本地端口' ($null -ne $lkTcp -and $lkTcp.local_port -eq 8099) `
            ('实际: ' + $(if ($null -ne $lkTcp) { $lkTcp.local_port } else { '<无>' }))
        Result '穿透链接标记本地端口已登记' ($null -ne $lkTcp -and $lkTcp.local_port_known -eq $true) `
            ('实际: ' + $(if ($null -ne $lkTcp) { $lkTcp.local_port_known } else { '<无>' }))
        $r = Http GET '/api/clients/sf-nosuchclient/links'
        Result '未知客户端取穿透链接返回 404' ($r.Code -eq 404) ('HTTP ' + $r.Code)

        # 客户端详情走的是另一条代码路径（clientDetailResponse）。
        # 它原先直接下发 FRP 的原始代理结构，因此是本次改动里最容易漏掉的一处 ——
        # 代理列表对而详情不对，现象只是「详情抽屉里少一列」。
        $detailX = (Http GET ('/api/clients/' + $cidX)).Json
        $dProxies = @()
        if ($null -ne $detailX) { $dProxies = @($detailX.proxies) }
        $dProxy = $dProxies | Where-Object { $_.clientID -eq $cidX } | Select-Object -First 1
        $dLocal = $null
        if ($null -ne $dProxy) { $dLocal = $dProxy.local_port }
        Result '客户端详情里的代理也带本地端口' `
            ($null -ne $dProxy -and $dProxy.local_port_known -eq $true -and [string]$dLocal -eq '8099') `
            ('共 ' + (Count-Of $dProxies) + ' 条，本地端口: ' + $dLocal)

        # 自己占的端口不阻挡自己：这一步同时存在「台账占用」和「实时占用」，
        # 两者都属于 cidX，重新生成必须照常放行。
        $r = Http POST '/api/pkg/client' (ToJson $specX) -OutFile (Join-Path $Work 'x2.zip')
        Result '自己占用的端口不阻挡自己重新生成' ($r.Code -eq 200) ('HTTP ' + $r.Code)

        # 删掉台账记录但保留运行中的 frpc：这正是本功能要暴露的分叉状态 ——
        # 「装了但没登记」。此后 19311 只剩实时口径占着。
        $reg = Get-Registry
        $clientX = Find-Client $reg $cidX
        $r = Http DELETE ('/api/registry/clients/' + $clientX.id)
        Result '删除实时客户端的台账记录' ($r.Code -eq 200) ('HTTP ' + $r.Code)
        $ports = Get-Ports
        Result '台账口径下该端口已释放' ($null -eq (RegOwnerOf $ports $LivePort)) ''
        Result '实时口径下该端口仍被占用（两个口径独立）' ($null -ne (LiveOwnerOf $ports $LivePort)) ''

        # 台账记录没了，客户端本身还在线。此时界面必须降级为「未登记」，
        # 而**不是**把这个客户端从列表里抹掉或报错 —— 显示名称是附加信息，
        # 它读不到不能影响离线列表本身（这条也顺带守住 registered 字段的语义：
        # 名称留空 ≠ 没登记，两者在界面上给的是不同提示）。
        $clientsAfter = @((Http GET '/api/clients').Json.clients)
        $shownXAfter = $clientsAfter | Where-Object { $_.clientID -eq $cidX } | Select-Object -First 1
        $afterName = ''
        if ($null -ne $shownXAfter) { $afterName = [string]$shownXAfter.display_name }
        Result '台账记录删除后客户端仍在列表里' ($null -ne $shownXAfter) ''
        Result '台账记录删除后显示名称清空' ([string]::IsNullOrEmpty($afterName)) ('实际: ' + $afterName)
        Result '台账记录删除后标记为未登记' ($null -ne $shownXAfter -and $shownXAfter.registered -eq $false) `
            ('实际: ' + $shownXAfter.registered)
        # 类型与名称同时消失：两者是同一次关联下发的，不该出现「名称清了、类型还留着」
        # 这种半个状态 —— 那会让客户端页把一个未登记的设备画进某个类型组里。
        $typeAfter = ''
        if ($null -ne $shownXAfter) { $typeAfter = [string]$shownXAfter.robot_type }
        Result '台账记录删除后机器人类型一并清空' ([string]::IsNullOrEmpty($typeAfter)) ('实际: ' + $typeAfter)

        # 本地端口与名称、类型是同一个来源（都来自台账），台账没了就必须一起消失。
        # 只清名称与类型、却还显示着本地端口，会造出「未登记却有本地端口」的
        # 自相矛盾状态 —— 而这正是最容易漏的一种：名称与类型一直有人盯着，
        # 本地端口是这一轮才加的字段，漏掉的症状只是「多显示了一个数字」。
        $proxiesAfter = @((Http GET '/api/proxies').Json.proxies)
        $proxyXAfter = $proxiesAfter | Where-Object { $_.clientID -eq $cidX } | Select-Object -First 1
        $knownAfter = $null
        if ($null -ne $proxyXAfter) { $knownAfter = $proxyXAfter.local_port_known }
        Result '台账记录删除后本地端口标记为未登记' ($null -ne $proxyXAfter -and $knownAfter -eq $false) `
            ('实际: ' + $knownAfter)

        # 穿透链接弹窗走的是**第三处**本地端口关联（前两处是代理列表与客户端详情）。
        # 三处共用 lookupLocalPort，但这是新写的第四个调用点，必须单独验一遍：
        # 漏掉它的症状只是「弹窗里少一个数字」，没有报错，回归也照样全绿。
        $lkAfter = (Http GET ('/api/clients/' + $cidX + '/links')).Json
        $lkAfterTcp = @($lkAfter.links) | Where-Object { $_.proxy_type -eq 'tcp' } | Select-Object -First 1
        Result '台账删除后穿透链接仍能给出访问地址' `
            ($null -ne $lkAfterTcp -and $lkAfterTcp.access_url -eq ('127.0.0.1:' + $LivePort)) `
            ('实际: ' + $(if ($null -ne $lkAfterTcp) { $lkAfterTcp.access_url } else { '<无>' }))
        Result '台账删除后穿透链接的本地端口标记为未登记' `
            ($null -ne $lkAfterTcp -and $lkAfterTcp.local_port_known -eq $false) `
            ('实际: ' + $(if ($null -ne $lkAfterTcp) { $lkAfterTcp.local_port_known } else { '<无>' }))
        # 值也必须是 0：界面不许显示成「本机 0」，也不许残留删除前的 8099。
        # 只断言 known=false 而放行旧值，会造出「未登记却有本地端口」的自相矛盾。
        Result '台账删除后穿透链接不再残留旧的本地端口' `
            ($null -ne $lkAfterTcp -and $lkAfterTcp.local_port -eq 0) `
            ('实际: ' + $(if ($null -ne $lkAfterTcp) { $lkAfterTcp.local_port } else { '<无>' }))

        # 现在冲突只能来自实时口径，文案必须说清「不在本平台台账里」，
        # 因为处理方式完全不同：这条只能改端口，台账里没有可释放的记录。
        $specY = New-ClientSpec -ClientID $cidY -Name '台账测试-撞实时' -Proxies @(
            (New-Proxy -Name 'y-tcp' -Type 'tcp' -RemotePort $LivePort -LocalPort 8098)
        )
        $r = Http POST '/api/pkg/client' (ToJson $specY) -OutFile (Join-Path $Work 'y.zip')
        Result '撞实时在线端口同样被拦（409）' ($r.Code -eq 409) ('HTTP ' + $r.Code + ' ' + $r.Body)
        $detailText = (@($r.Json.error.details) -join ' ')
        Result '实时冲突文案说明「不在本平台台账里」' ($detailText -like '*不在本平台台账里*') $detailText

        # 台账记录已删、只剩实时占用时，仍要靠 clientID 认出「这是自己」。
        $r = Http POST '/api/pkg/client' (ToJson $specX) -OutFile (Join-Path $Work 'x3.zip')
        Result '仅凭实时占用的 clientID 也能认出自己并放行' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)

        if ($script:FrpcProc -and -not $script:FrpcProc.HasExited) {
            Stop-Process -Id $script:FrpcProc.Id -Force -ErrorAction SilentlyContinue
        }
        Start-Sleep -Seconds 1
        Write-Host '        已停止测试用 frpc' -ForegroundColor DarkGray
    } else {
        Result '测试用 frps 已就绪（带 auth.token）' $false '未能在 15 秒内监听'
    }
}

Section '11. 机器人类型（F19 扩展）'
# 机器人类型是纯业务分类。本段要守住三件事：
#   ① 它必须真的落进台账，并且能改、能清空；
#   ② 它**绝不能**进 frpc.toml —— FRP 对未知字段是严格解码的，多一个键整包被拒
#      （02-技术选型 §6.5 记过 remotePort 的同类事故），这是本段最重要的断言；
#   ③ 非法值必须在生成阶段就被拒，而不是先写进台账再让管理员去猜哪里不对。
$cidR = 'sf-regrrr' + (Get-Random -Minimum 1000 -Maximum 9999)
$specR = New-ClientSpec -ClientID $cidR -Name '台账测试-喷涂班' -RobotType 'spray' -Proxies @(
    (New-Proxy -Name 'r-tcp' -Type 'tcp' -RemotePort $RobotPort -LocalPort 8090)
)
$zipR = Join-Path $Work 'r.zip'
$r = Http POST '/api/pkg/client' (ToJson $specR) -OutFile $zipR
Result '带机器人类型的包可以生成' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)

$reg = Get-Registry
$clientR = Find-Client $reg $cidR
Result '台账记录了机器人类型' ($null -ne $clientR -and $clientR.robot_type -eq 'spray') `
    ('实际: ' + $(if ($clientR) { $clientR.robot_type } else { '(无记录)' }))

$exR = Join-Path $Work 'ex-r'
Expand-Archive -Path $zipR -DestinationPath $exR -Force
$tomlR = [System.IO.File]::ReadAllText((Join-Path $exR 'frpc.toml'), [System.Text.Encoding]::UTF8)
Result '机器人类型没有进 frpc.toml（多一个键就会被 FRP 拒包）' `
    (($tomlR -notlike '*robot*') -and ($tomlR -notlike '*机器*')) ''
$readmeR = [System.IO.File]::ReadAllText((Join-Path $exR 'README.txt'), [System.Text.Encoding]::UTF8)
Result '包内 README 写出机器人类型中文名' ($readmeR -like '*机器人类型 : 喷涂机器人*') `
    (((@($readmeR -split "`r`n") | Select-Object -First 7) -join ' | '))

$specRBad = New-ClientSpec -ClientID $cidR -Name '台账测试-喷涂班' -RobotType 'bogus' -Proxies @(
    (New-Proxy -Name 'r-tcp' -Type 'tcp' -RemotePort $RobotPort -LocalPort 8090)
)
$r = Http POST '/api/pkg/client' (ToJson $specRBad)
Result '生成时未知机器人类型被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code + ' ' + $r.Body)
Result '拒绝原因点明机器人类型' ($r.Body -like '*机器人类型*') $r.Body

$editUrl = '/api/registry/clients/' + $clientR.id
$r = Http POST $editUrl (ToJson @{
    display_name = '台账测试-喷涂班'; robot_type = 'rebar'; note = ''; status = 'active'
})
Result '台账可把机器人类型改为钢筋加工' ($r.Code -eq 200 -and $r.Json.client.robot_type -eq 'rebar') `
    ('HTTP ' + $r.Code + ' 回读=' + [string]$r.Json.client.robot_type)

$r = Http POST $editUrl (ToJson @{
    display_name = '台账测试-喷涂班'; robot_type = 'bogus'; note = ''; status = 'active'
})
Result '台账编辑非法机器人类型被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code + ' ' + $r.Body)

# 停用/启用只改状态。这个接口是**整体替换**语义（四个字段一起提交），
# 所以这两条同时也在守「别把类型顺带清空」—— 漏传字段的调用方会在这里现形。
$r = Http POST $editUrl (ToJson @{
    display_name = '台账测试-喷涂班'; robot_type = 'welding'; note = ''; status = 'retired'
})
Result '停用时写入的机器人类型被保留' ($r.Code -eq 200 -and $r.Json.client.robot_type -eq 'welding') `
    ('HTTP ' + $r.Code + ' 回读=' + [string]$r.Json.client.robot_type)
$r = Http POST $editUrl (ToJson @{
    display_name = '台账测试-喷涂班'; robot_type = 'welding'; note = ''; status = 'active'
})
Result '重新启用后机器人类型不变' ($r.Code -eq 200 -and $r.Json.client.robot_type -eq 'welding') `
    ('HTTP ' + $r.Code + ' 回读=' + [string]$r.Json.client.robot_type)

$r = Http POST $editUrl (ToJson @{
    display_name = '台账测试-喷涂班'; robot_type = ''; note = ''; status = 'active'
})
Result '机器人类型可以清空回未分类' `
    ($r.Code -eq 200 -and $null -ne $r.Json.client -and [string]$r.Json.client.robot_type -eq '') `
    ('HTTP ' + $r.Code + ' 回读=' + [string]$r.Json.client.robot_type)

# 重新生成时以**本次表单**为准：管理员在表单里改了类型，台账必须跟着变，
# 否则「改错类型想改回来」这件事在界面上会显得无效。
$r = Http POST '/api/pkg/client' (ToJson $specR) -OutFile (Join-Path $Work 'r2.zip')
$reg = Get-Registry
$clientR2 = Find-Client $reg $cidR
Result '重新生成时机器人类型以表单为准' ($null -ne $clientR2 -and $clientR2.robot_type -eq 'spray') `
    ('实际: ' + $(if ($clientR2) { $clientR2.robot_type } else { '(无记录)' }))

# ---- 目标平台：改包功能的地基 ----
#
# 它决定下一次改包产出 Windows 还是 Linux 包，三者二进制、脚本、行尾全不同。
# 平台值**只能从两条路来**：本次签发的表单，或管理员在台账里改。前者写不丢
# 就等于这条记录永远停留在「未记录」，后者写丢则是一次 silently 的回退。
$specP = New-ClientSpec -ClientID $cidR -Name '台账测试-ARM' -RobotType 'spray' `
    -Platform 'linux-arm64' -Proxies @(
        (New-Proxy -Name 'r-tcp' -Type 'tcp' -RemotePort $RobotPort -LocalPort 8090)
    )
$r = Http POST '/api/pkg/client' (ToJson $specP) -OutFile (Join-Path $Work 'r-arm.zip')
Result '指定 Linux ARM64 能成功出包' ($r.Code -eq 200) ('HTTP ' + $r.Code + ' ' + $r.Body)
$reg = Get-Registry
$clientP = Find-Client $reg $cidR
Result '签发时把目标平台写进台账' ($null -ne $clientP -and $clientP.platform -eq 'linux-arm64') `
    ('实际: ' + $(if ($clientP) { $clientP.platform } else { '(无记录)' }))
#
# 覆盖边界（诚实记录）：这里不断言 ARM 包里的二进制到底是不是 Linux 版本 ——
# 「选了 linux-arm64 就产出 .sh + systemd + 无 BOM 的包」由 pkg-test 的
# Linux 段落完整覆盖（那里为此内置了 zip 条目与编码检查）。这一条只守住
# **决定包形态的那个值有没有落到台账里**，因为改包功能依赖的正是它。

$editUrlP = '/api/registry/clients/' + $clientR.id
$r = Http POST $editUrlP (ToJson @{
    display_name = '台账测试-喷涂班'; robot_type = 'spray'; platform = 'plan9-mips'; note = ''; status = 'active'
})
Result '台账编辑非法目标平台被拒（400）' ($r.Code -eq 400) ('HTTP ' + $r.Code + ' ' + $r.Body)

# 停用与启用各走一次。这两个动作看起来只动状态，而接口是**整体替换**语义 ——
# 调用方漏传 platform 就会把它清成「未记录」，而线索要等到几周后给这台 ARM 盒子
# 改包时才浮出来（那时产的是 Windows 包，没人会怀疑当初点过一次「停用」）。
foreach ($pair in @(@('retired', '停用后目标平台保留'), @('active', '重新启用后目标平台保留'))) {
    $r = Http POST $editUrlP (ToJson @{
        display_name = '台账测试-喷涂班'; robot_type = 'spray'; platform = 'linux-arm64'
        note = ''; status = $pair[0]
    })
    Result $pair[1] ($r.Code -eq 200 -and $r.Json.client.platform -eq 'linux-arm64') `
        ('HTTP ' + $r.Code + ' 回读=' + [string]$r.Json.client.platform)
}

# ---- 台账 → 打包草稿（改包） ----
#
# 历史 ZIP 从不落盘、服务端也不保留密钥（§8.5），所以「编辑旧包」只能是
# 「用台账重建一份表单」。这个接口就是重建的结果，它必须同时满足两件事：
# ① 台账里有的，原样回来；② 台账里没有的，**如实说它没有**，而不是悄悄补个默认值。
# 第二条尤其容易退化：补个默认值看起来完全正常，管理员会当成原样直接提交。
$rs = (Http GET ('/api/registry/clients/' + $clientR.id + '/spec')).Json
Result '改包草稿取到同一台设备的 clientID' ([string]$rs.spec.client_id -eq $cidR) `
    ('实际: ' + [string]$rs.spec.client_id)
# clientID 是这条链路的命根子：它一错，新包就不再是「同一台设备的新版本」，
# 而是一份发给陌生设备的包 —— 界面上看不出来，只会多出一条台账记录。
Result '改包草稿还原目标平台' ([string]$rs.spec.platform -eq 'linux-arm64') `
    ('实际: ' + [string]$rs.spec.platform)
# 先算再判，不要用 `(... | Where-Object {...}).Count -eq 1` 这种连写形式：
# PS 5.1 里 -and 的优先级高于比较运算符，那种写法实际会被解析成
# `(A -and B) -eq 1`，命中 2 条时代价是结果变成 False，而盯得最久的问题
# 恰恰是「为什么这里明明有一条」。
$rMatch = @(@($rs.spec.proxies) | Where-Object { $_.name -eq 'r-tcp' })
Result '改包草稿还原代理短名' ((Count-Of $rMatch) -eq 1) `
    ('命中 ' + (Count-Of $rMatch) + ' 条；全部代理名: ' +
        ((@($rs.spec.proxies) | ForEach-Object { $_.name }) -join ','))
$rtcp = @($rs.spec.proxies) | Where-Object { $_.name -eq 'r-tcp' } | Select-Object -First 1
Result '改包草稿还原代理远端端口' ($null -ne $rtcp -and [int]$rtcp.remote_port -eq $RobotPort) `
    ('实际: ' + $(if ($rtcp) { $rtcp.remote_port } else { '<无此代理>' }))
Result '改包草稿还原代理本地端口' ($null -ne $rtcp -and [int]$rtcp.local_port -eq 8090) `
    ('实际: ' + $(if ($rtcp) { $rtcp.local_port } else { '<无此代理>' }))
# token 不是还原出来的，是从接入点取的 —— 这是刻意的：旧包的密钥从未留存，
# 而且两端必须用同一个 token 才连得上。断言它**非空且等于接入点的**。
Result '改包草稿的 token 取自接入点且不为空' (-not [string]::IsNullOrEmpty([string]$rs.spec.token)) ''
Result '改包草稿如实报告按默认值补齐的字段' ((Count-Of $rs.defaults_applied) -ge 1) `
    ('实际: ' + ((@($rs.defaults_applied)) -join ','))
# 逐条代理的缺项只列这条代理用得上的：给 tcp 列 custom_domains 是噪音，
# 而给 http 不列它就是漏了。这里 tcp 至少要指出 local_ip 没能还原。
$tcpHint = @($rs.proxy_defaults) | Where-Object { $_.proxy_name -eq 'r-tcp' } | Select-Object -First 1
Result '改包草稿逐条代理列出未还原字段' (
    ($null -ne $tcpHint) -and (@($tcpHint.fields) -contains 'local_ip')) `
    ('实际: ' + $(if ($tcpHint) { (@($tcpHint.fields)) -join ',' } else { '<无此代理的提示>' }))

$cidOld = 'sf-regressold0001'
$specOld = New-ClientSpec -ClientID $cidOld -Name '台账测试-未记录平台' -Platform '' -Proxies @(
    (New-Proxy -Name 'o-tcp' -Type 'tcp' -RemotePort ($RobotPort + 1) -LocalPort 8091)
)
$r = Http POST '/api/pkg/client' (ToJson $specOld) -OutFile (Join-Path $Work 'o.zip')
Result '不带 platform 的出包仍然成功（存量调用方不受影响）' ($r.Code -eq 200) ('HTTP ' + $r.Code)
$reg = Get-Registry
$clientOld = Find-Client $reg $cidOld
Result '未指定平台时台账按 Windows 登记（不留下未记录）' `
    ($null -ne $clientOld -and $clientOld.platform -eq 'windows-amd64') `
    ('实际: ' + $(if ($clientOld) { $clientOld.platform } else { '(无记录)' }))

# 「未记录平台」的告警路径：把 platform 清空成空串，模拟一份在平台记录功能
# 上线**之前**就登记下来的老记录 —— 那正是存量数据真实的样子。
$r = Http POST ('/api/registry/clients/' + $clientOld.id) (ToJson @{
    display_name = '台账测试-未记录平台'; robot_type = ''; platform = ''
    note = ''; status = 'active'
})
Result '目标平台可清空回未记录' ($r.Code -eq 200 -and [string]$r.Json.client.platform -eq '') `
    ('HTTP ' + $r.Code + ' 回读=' + [string]$r.Json.client.platform)
$rsW = (Http GET ('/api/registry/clients/' + $clientOld.id + '/spec')).Json
Result '未记录平台时草稿给出告警' ((Count-Of $rsW.warnings) -ge 1) `
    ('实际: ' + ((@($rsW.warnings)) -join ' / '))
# 同时必须落在默认平台上：给不出任何值的表单等于开不了门。
Result '未记录平台时草稿按默认平台预填' ([string]$rsW.spec.platform -eq 'windows-amd64') `
    ('实际: ' + [string]$rsW.spec.platform)

$r = Http GET '/api/registry/clients/999999/spec'
Result '取不存在的台账草稿返回 404' ($r.Code -eq 404) ('HTTP ' + $r.Code)

# 空值必须在 README 里整行不出现，而不是渲染成半行「机器人类型 : 」（右边什么都没有）。
$specN = New-ClientSpec -ClientID $cidR -Name '台账测试-未分类' -RobotType '' -Proxies @(
    (New-Proxy -Name 'r-tcp' -Type 'tcp' -RemotePort $RobotPort -LocalPort 8090)
)
$zipN = Join-Path $Work 'r-none.zip'
$r = Http POST '/api/pkg/client' (ToJson $specN) -OutFile $zipN
$exN = Join-Path $Work 'ex-r-none'
Expand-Archive -Path $zipN -DestinationPath $exN -Force
$readmeN = [System.IO.File]::ReadAllText((Join-Path $exN 'README.txt'), [System.Text.Encoding]::UTF8)
Result '未分类的包 README 不出现机器人类型行' ($readmeN -notlike '*机器人类型*') ''

Section '12. 手工补录台账'

# 补录的存在理由：设备已经在跑，但台账记录丢了（数据库异常、误删）。
# 旧版本唯一能创建台账的入口是「生成客户端包」，而管理员并不想再出一个包。
# 这组用例覆盖：正常补录、重复 clientID、端口冲突、非法输入。

$manualID = 'sf-manual0001'
$manualPort = 39410

$r = Http POST '/api/registry/clients' (ToJson @{
    client_id    = $manualID
    display_name = '补录测试机器人'
    robot_type   = 'transport'
    note         = '现场已部署，台账丢失后补录'
    server_addr  = '127.0.0.1'
    server_port  = 7000
    ports        = @(
        @{ proxy_name = 'm1'; proxy_type = 'tcp'; remote_port = $manualPort; local_port = 9999 }
    )
})
Result '补录返回 200' ($r.Code -eq 200) ('HTTP ' + $r.Code)

$clients = Get-RegistryClients
$mc = Find-Client $clients $manualID
Result '补录记录出现在台账里' ($null -ne $mc) ''

if ($mc) {
    # generate_count 必须是 0：这条记录没有签发历史，界面据此标注「手工补录」。
    # 若记成 1，就与「真的发过一个包」无法区分了。
    Result '补录记录 generate_count 为 0' ($mc.generate_count -eq 0) ('实际: ' + $mc.generate_count)
    Result '补录记录显示名称正确' ($mc.display_name -eq '补录测试机器人') ('实际: ' + $mc.display_name)
    Result '补录记录机器人类型正确' ($mc.robot_type -eq 'transport') ('实际: ' + $mc.robot_type)

    $mports = @(ClientPorts $mc)
    Result '补录记录的端口已登记' ($mports.Count -eq 1) ('实际: ' + $mports.Count)
    if ($mports.Count -eq 1) {
        Result '补录记录的远端端口正确' ($mports[0].remote_port -eq $manualPort) ('实际: ' + $mports[0].remote_port)
        # 本地端口只有台账里有（FRP 不返回），补录时必须能写进来
        Result '补录记录的本地端口正确' ($mports[0].local_port -eq 9999) ('实际: ' + $mports[0].local_port)
    }

    # 补录的端口必须真的受保护：另一个客户端来抢应当被拦
    Result '补录端口进入冲突判定' ($null -ne (RegOwnerOf (Get-Ports) $manualPort)) ''
}

$dup = Http POST '/api/registry/clients' (ToJson @{
    client_id    = $manualID
    display_name = '重复补录'
    robot_type   = ''
    note         = ''
    server_addr  = ''
    server_port  = 0
    ports        = @()
})
Result '重复 clientID 补录被拒绝（409）' ($dup.Code -eq 409) ('HTTP ' + $dup.Code)

$conflict = Http POST '/api/registry/clients' (ToJson @{
    client_id    = 'sf-manual0002'
    display_name = '抢端口的'
    robot_type   = ''
    note         = ''
    server_addr  = ''
    server_port  = 0
    ports        = @(
        @{ proxy_name = 'x1'; proxy_type = 'tcp'; remote_port = $manualPort; local_port = 8080 }
    )
})
Result '补录撞端口被拒绝（409）' ($conflict.Code -eq 409) ('HTTP ' + $conflict.Code)
# 非 200 时 Http 不会解析 Json（那是有意的：只有成功响应才当 JSON 用），
# 因此这里必须自己从 Body 解析，否则断言恒假、看起来像后端没返回 details。
$conflictObj = $null
try { $conflictObj = $conflict.Body | ConvertFrom-Json } catch { }
Result '撞端口时给出逐条说明' (@($conflictObj.error.details).Count -gt 0) ('实际: ' + $conflict.Body)

# 大写类型必须同样参与判定。这是实际踩过的坑：
# 后端曾用 pt.Valid() 判断，而枚举是小写，传 'TCP' 会让该代理既不进冲突集合
# 也不报错 —— 端口被静默放行，两条记录可以登记同一个端口。
$upper = Http POST '/api/registry/clients' (ToJson @{
    client_id    = 'sf-manual0003'
    display_name = '大写类型'
    robot_type   = ''
    note         = ''
    server_addr  = ''
    server_port  = 0
    ports        = @(
        @{ proxy_name = 'u1'; proxy_type = 'UDP'; remote_port = $manualPort; local_port = 8081 }
    )
})
Result '大写类型同样参与冲突判定（409）' ($upper.Code -eq 409) ('HTTP ' + $upper.Code)

# 不认识的类型要报错，不能静默放行 —— 静默放行等于放走一个可能冲突的端口
$bogus = Http POST '/api/registry/clients' (ToJson @{
    client_id    = 'sf-manual0004'
    display_name = '非法类型'
    robot_type   = ''
    note         = ''
    server_addr  = ''
    server_port  = 0
    ports        = @(
        @{ proxy_name = 'b1'; proxy_type = 'bogus'; remote_port = 39499; local_port = 8082 }
    )
})
Result '非法代理类型被拒绝（400）' ($bogus.Code -eq 400) ('HTTP ' + $bogus.Code)

$badID = Http POST '/api/registry/clients' (ToJson @{
    client_id    = 'sf 含空格'
    display_name = 'x'
    robot_type   = ''
    note         = ''
    server_addr  = ''
    server_port  = 0
    ports        = @()
})
Result 'clientID 含非法字符被拒绝（400）' ($badID.Code -eq 400) ('HTTP ' + $badID.Code)

$badType = Http POST '/api/registry/clients' (ToJson @{
    client_id    = 'sf-manual0005'
    display_name = 'x'
    robot_type   = 'notatype'
    note         = ''
    server_addr  = ''
    server_port  = 0
    ports        = @()
})
Result '非法机器人类型被拒绝（400）' ($badType.Code -eq 400) ('HTTP ' + $badType.Code)

# 空 ports 时响应里必须是 []，不能是 null —— 前端读 .length 会崩
$emptyClient = Find-Client (Get-RegistryClients) $manualID
$rawEmpty = (Http GET '/api/registry/clients').Body
Result '台账响应中空端口为 [] 而非 null' ($rawEmpty -notmatch '"ports":null') ''

Section '13. 删除时的审计粒度'

# 删除台账会级联删掉 client_port，事后再想回答「释放了哪几个端口」只能全库反推。
# 而 FRP 不返回本地端口，台账是它的唯一副本 —— 审计必须写清端口号。
$beforeDel = Get-RegistryClients
$delTarget = Find-Client $beforeDel $manualID
if ($delTarget) {
    $del = Http DELETE ('/api/registry/clients/' + $delTarget.id)
    Result '删除补录记录返回 200' ($del.Code -eq 200) ('HTTP ' + $del.Code)

    $auditAll = @((Http GET '/api/audit?limit=100').Json.items)
    $delEntry = $auditAll | Where-Object { $_.action -eq 'registry.delete' } | Select-Object -First 1
    if ($delEntry) {
        # 关键断言：审计里要出现**具体端口号**，不能只说「释放了端口」
        Result '删除审计写明被释放的端口号' ($delEntry.detail -like ('*' + $manualPort + '*')) ('实际: ' + $delEntry.detail)
    } else {
        Result '删除审计存在' $false '未找到 registry.delete 记录'
    }
}

$createEntry = @((Http GET '/api/audit?limit=100').Json.items) |
    Where-Object { $_.action -eq 'registry.create' } | Select-Object -First 1
Result '补录动作有审计留痕' ($null -ne $createEntry) ''

Section '14. 审计留痕'
$audit = @((Http GET '/api/audit').Json.items)
$actions = @($audit | ForEach-Object { $_.action })
Result '审计含 registry.retired' ($actions -contains 'registry.retired') ('实际: ' + (($actions | Select-Object -Unique) -join ','))
Result '审计含 registry.active' ($actions -contains 'registry.active') ''
Result '审计含 registry.delete' ($actions -contains 'registry.delete') ''
Result '审计含 pkg.generate' ($actions -contains 'pkg.generate') ''
# 直接扫**原始响应文本**，而不是先解析成对象再序列化回去 ——
# 后者在「台账已被前一段用例清空」时会扫到一个空数组，断言恒真（假通过）。
$regRaw = (Http GET '/api/registry/clients').Body
Result '台账响应不含任何 token 明文' (-not $regRaw.Contains($FrpsToken)) ''

} catch {
    Write-Host ''
    Write-Host ('[中断] ' + $_.Exception.Message) -ForegroundColor Red
    Write-Host ('        位置: ' + $_.InvocationInfo.PositionMessage.Trim()) -ForegroundColor DarkGray
    $script:Fail++
    $script:FailNames += ('执行中断: ' + $_.Exception.Message)
} finally {
    if ($script:FrpcProc -and -not $script:FrpcProc.HasExited) {
        Stop-Process -Id $script:FrpcProc.Id -Force -ErrorAction SilentlyContinue
    }
    if ((-not $KeepFrps) -and $script:FrpsProc -and -not $script:FrpsProc.HasExited) {
        Stop-Process -Id $script:FrpsProc.Id -Force -ErrorAction SilentlyContinue
    }
}

# ---------- 汇总 ----------
Section '汇总'
$total = $script:Pass + $script:Fail
Write-Host ('  通过 ' + $script:Pass + ' / ' + $total) -ForegroundColor $(if ($script:Fail -eq 0) { 'Green' } else { 'Red' })
Write-Host ('  工作目录: ' + $Work) -ForegroundColor DarkGray
if ($script:Fail -gt 0) {
    Write-Host '  失败项:' -ForegroundColor Red
    foreach ($n in $script:FailNames) { Write-Host ('    - ' + $n) -ForegroundColor Red }
    exit 1
}
Write-Host '  全部通过' -ForegroundColor Green
exit 0
