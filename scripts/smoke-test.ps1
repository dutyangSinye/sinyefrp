# SinyeFRP 本机冒烟测试
#
# 覆盖范围：初始化闭环、登录与会话、登录限速、节点配置保存、凭据加密、
#           审计记录、同源与内容类型护栏、节点不可达时的状态语义。
#
# 使用前提：数据库为空（首次初始化路径）。脚本会拒绝在已初始化的实例上运行，
#           以免误改真实数据。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -BaseUrl http://127.0.0.1:7800

[CmdletBinding()]
param(
    [string]$BaseUrl = "http://127.0.0.1:7800",
    [string]$AdminUser = "smokeadmin",
    [string]$AdminPass = "SmokeTest_2026!x",
    [string]$NodePassword = "ProbeOnly_1a2b3c",
    [string]$NodeUrl = ""
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
# 控制台按 UTF-8 解码 curl 的输出，否则接口返回的中文提示会变成乱码，
# 排障时无法判断到底是服务端出错还是显示问题。
try {
    [Console]::OutputEncoding = [System.Text.Encoding]::UTF8
    $OutputEncoding = [System.Text.Encoding]::UTF8
}
catch { }

$script:pass = 0
$script:fail = 0
$script:jar = Join-Path $env:TEMP ("sinyefrp-smoke-" + [guid]::NewGuid().ToString("N") + ".cookies")

function Result($name, $ok, $detail) {
    if ($ok) {
        $script:pass++
        Write-Host ("  [PASS] " + $name) -ForegroundColor Green
    }
    else {
        $script:fail++
        Write-Host ("  [FAIL] " + $name + "  -> " + $detail) -ForegroundColor Red
    }
}

# Http 发起请求，返回 @{ Code; Body; Json }
function Http {
    param(
        [string]$Method,
        [string]$Path,
        [string]$Payload = $null,
        [switch]$NoAuth,
        [switch]$FreshLogin,
        [switch]$RawBody,
        [string]$ContentType = "application/json"
    )
    # 注意：不能用 $args，它是 PowerShell 的自动变量。
    # --noproxy "*"：本机冒烟测试必须直连。若开发机装了系统级代理，
    # curl 会把 127.0.0.1 也交给代理，失败时返回的是代理的 502 而不是真实结果，
    # 会把「服务没起来」误判成「接口返回 502」。
    $cargs = @("-s", "-S", "--noproxy", "*", "-o", "-", "-w", "`n%{http_code}", "-X", $Method)

    # Cookie 处理有三种模式，必须严格区分：
    #   默认        —— 读出并写回（普通已登录请求）
    #   -FreshLogin —— 只写回、不读出（登录/初始化：此时还没有会话，若同时传 -b
    #                  会读到空 jar；而只传 -c 才是「拿新会话」的正确姿势）
    #   -NoAuth     —— 完全不碰 jar。不能只省 -b 而保留 -c：
    #                  没有 -b 时 curl 认为「不存在任何 Cookie」，写回会清空 jar，
    #                  把之前登录的会话悄悄抹掉。
    if (-not $NoAuth) {
        if (-not $FreshLogin) { $cargs += @("-b", $script:jar) }
        $cargs += @("-c", $script:jar)
    }

    # 请求体写入临时文件后用 --data-binary @file 传递。
    # 直接把 JSON 当命令行参数传给 curl.exe，在 Windows PowerShell 5.1 下
    # 会因控制台代码页转换而损坏非 ASCII 内容（中文用户名等），因此必须走文件。
    #
    # 变量命名注意：PowerShell 变量名不区分大小写，且 [string] 参数会形成
    # 类型约束并持续生效。若这里把参数命名为 $Json，后面 `$json = ... | ConvertFrom-Json`
    # 会被强制转成字符串，解析结果静默作废（症状是调用方永远拿到空对象）。
    $bodyFile = $null
    if ($null -ne $Payload) {
        $bodyFile = Join-Path $env:TEMP ("sinyefrp-body-" + [guid]::NewGuid().ToString("N") + ".json")
        [System.IO.File]::WriteAllText($bodyFile, $Payload, [System.Text.UTF8Encoding]::new($false))
        $cargs += @("-H", ("Content-Type: " + $ContentType), "--data-binary", ("@" + $bodyFile))
    }
    elseif ($RawBody) {
        $bodyFile = Join-Path $env:TEMP ("sinyefrp-body-" + [guid]::NewGuid().ToString("N") + ".txt")
        [System.IO.File]::WriteAllText($bodyFile, "x=1", [System.Text.UTF8Encoding]::new($false))
        $cargs += @("-H", ("Content-Type: " + $ContentType), "--data-binary", ("@" + $bodyFile))
    }
    $cargs += @($BaseUrl + $Path)

    try {
        $out = & curl.exe @cargs 2>&1
    }
    finally {
        if ($bodyFile -and (Test-Path $bodyFile)) { Remove-Item $bodyFile -Force -ErrorAction SilentlyContinue }
    }

    # Out-String 会追加换行，而 json.Encoder 的响应体本身也以换行结尾，
    # 因此必须先 TrimEnd，否则 LastIndexOf 取到的是那个空行、状态码恒为 0。
    $text = ($out | Out-String).TrimEnd("`r", "`n")
    $idx = $text.LastIndexOf("`n")
    $code = 0
    $respBody = $text
    if ($idx -ge 0) {
        $codeStr = $text.Substring($idx + 1).Trim()
        [void][int]::TryParse($codeStr, [ref]$code)
        $respBody = $text.Substring(0, $idx)
    }
    $parsed = $null
    if ($respBody.Trim()) { try { $parsed = $respBody | ConvertFrom-Json } catch { } }
    return @{ Code = $code; Body = $respBody.Trim(); Json = $parsed }
}

function ClearJar { if (Test-Path $script:jar) { Remove-Item $script:jar -Force } }

Write-Host ""
Write-Host "SinyeFRP 冒烟测试  ->  $BaseUrl" -ForegroundColor Cyan
Write-Host ("=" * 60)

# ---------- 0. 前置检查 ----------
Write-Host "`n[0] 前置检查"
$r = Http GET "/api/health" -NoAuth
Result "健康检查可达" ($r.Code -eq 200) ("HTTP " + $r.Code)

$r = Http GET "/api/setup/status" -NoAuth
Result "初始化状态可读且未初始化" ($r.Code -eq 200 -and -not $r.Json.initialized) `
    ("HTTP " + $r.Code + " " + $r.Body)
if ($r.Json.initialized) {
    Write-Host "`n该实例已完成初始化，冒烟测试需要空库。请先删除 data\sinyefrp.db 后重试。" -ForegroundColor Yellow
    exit 2
}
$minLen = $r.Json.policy.min_password_len

# ---------- 1. 未初始化时的访问控制 ----------
Write-Host "`n[1] 未初始化时的访问控制（01-prd F01）"
$r = Http GET "/api/overview"
Result "总览在未初始化时被拒（409 not_initialized）" `
    ($r.Code -eq 409 -and $r.Json.error.kind -eq "not_initialized") ("HTTP " + $r.Code + " " + $r.Body)

$r = Http GET "/api/clients"
Result "客户端列表在未初始化时被拒" ($r.Code -eq 409) ("HTTP " + $r.Code)

$r = Http POST "/api/node/config" '{"name":"x","api_url":"http://127.0.0.1:7500"}'
Result "写接口在未初始化时被拒" ($r.Code -eq 409) ("HTTP " + $r.Code)

# ---------- 2. 口令策略 ----------
Write-Host "`n[2] 口令与账号策略"
$r = Http POST "/api/setup/admin" (ConvertTo-Json @{ username = $AdminUser; password = "short" } -Compress)
Result ("过短密码被拒（< " + $minLen + " 字节）") ($r.Code -eq 400) ("HTTP " + $r.Code + " " + $r.Body)

$r = Http POST "/api/setup/admin" (ConvertTo-Json @{ username = $AdminUser; password = "abcdefghijkl" } -Compress)
Result "单一字符种类密码被拒" ($r.Code -eq 400) ("HTTP " + $r.Code + " " + $r.Body)

$r = Http POST "/api/setup/admin" (ConvertTo-Json @{ username = "ab"; password = $AdminPass } -Compress)
Result "过短用户名被拒" ($r.Code -eq 400) ("HTTP " + $r.Code + " " + $r.Body)

$r = Http POST "/api/setup/admin" (ConvertTo-Json @{ username = "中文用户"; password = $AdminPass } -Compress)
Result "非 ASCII 用户名被拒" ($r.Code -eq 400) ("HTTP " + $r.Code + " " + $r.Body)

# ---------- 3. 初始化并建立会话 ----------
Write-Host "`n[3] 初始化与管理会话（01-prd F01/F02）"
ClearJar
$r = Http POST "/api/setup/admin" (ConvertTo-Json @{ username = $AdminUser; password = $AdminPass } -Compress) -FreshLogin
Result "创建管理员成功" ($r.Code -eq 200) ("HTTP " + $r.Code + " " + $r.Body)
Result "初始化后直接下发会话" ((Test-Path $script:jar) -and ((Get-Content $script:jar -Raw) -match "sinyefrp_session")) "cookie jar 中没有会话"

$r = Http GET "/api/setup/status" -NoAuth
Result "初始化状态已翻转为 true" ($r.Json.initialized -eq $true) ("initialized=" + $r.Json.initialized)

$r = Http POST "/api/setup/admin" (ConvertTo-Json @{ username = "hijack"; password = $AdminPass } -Compress)
Result "初始化接口不可重入（409）" ($r.Code -eq 409) ("HTTP " + $r.Code + " " + $r.Body)

$r = Http GET "/api/auth/me"
Result "会话可读取当前身份" ($r.Code -eq 200 -and $r.Json.user.username -eq $AdminUser) `
    ("HTTP " + $r.Code + " " + $r.Body)

# ---------- 4. 节点配置与连通性 ----------
Write-Host "`n[4] 节点配置与连通性测试（01-prd F03）"
$target = $NodeUrl
if (-not $target) {
    $cur = Http GET "/api/node/config"
    $target = $cur.Json.node.api_url
}
Write-Host ("  目标节点: " + $target) -ForegroundColor DarkGray

$r = Http POST "/api/node/config" '{"name":"bad","api_url":"not-a-url"}'
Result "非法地址被拒" ($r.Code -eq 400) ("HTTP " + $r.Code + " " + $r.Body)

$testBody = ConvertTo-Json @{
    name = "smoke"; api_url = $target; username = "admin"
    password = $NodePassword; timeout_seconds = 5; enabled = $true
} -Compress
$r = Http POST "/api/node/test" $testBody
$testOk = ($r.Code -eq 200 -and $r.Json.ok -eq $true)
Result "连通性测试区分出成功" $testOk ("HTTP " + $r.Code + " " + $r.Body)
if ($testOk) {
    Write-Host ("        版本=" + $r.Json.frp_version + "  兼容=" + $r.Json.compatible +
        "  客户端=" + $r.Json.client_count + "  代理=" + $r.Json.proxy_count +
        "  耗时=" + $r.Json.latency_ms + "ms") -ForegroundColor DarkGray
}
Result "测试结果不回传密码" (-not ($r.Body -match [regex]::Escape($NodePassword))) "响应体中出现明文密码"

$badBody = ConvertTo-Json @{
    name = "smoke"; api_url = $target; username = "admin"
    password = "definitely-wrong-password"; timeout_seconds = 5; enabled = $true
} -Compress
$r2 = Http POST "/api/node/test" $badBody
$authFail = ($r2.Code -eq 200 -and -not $r2.Json.ok -and $r2.Json.kind -eq "unauthorized")
Result "错误密码被分类为 unauthorized，而非笼统失败" $authFail ("HTTP " + $r2.Code + " " + $r2.Body)

# 用一个确定不可达的端口验证 unreachable 分类
$deadBody = ConvertTo-Json @{
    name = "smoke"; api_url = "http://127.0.0.1:17999"; username = "admin"
    password = "x"; timeout_seconds = 2; enabled = $true
} -Compress
$r3 = Http POST "/api/node/test" $deadBody
$deadOk = ($r3.Code -eq 200 -and -not $r3.Json.ok -and ($r3.Json.kind -eq "unreachable" -or $r3.Json.kind -eq "timeout"))
Result "不可达端口被分类为 unreachable/timeout" $deadOk ("kind=" + $r3.Json.kind)

$r = Http POST "/api/node/config" $testBody
Result "保存节点配置成功" ($r.Code -eq 200) ("HTTP " + $r.Code + " " + $r.Body)
Result "保存后立即生效（无需重启）" ($r.Json.needs_restart -eq $false) ("needs_restart=" + $r.Json.needs_restart)

$r = Http GET "/api/node/config"
Result "配置回读为已持久化" ($r.Json.persisted -eq $true -and $r.Json.node.password_configured -eq $true) `
    ("persisted=" + $r.Json.persisted + " configured=" + $r.Json.node.password_configured)
Result "配置回读不含明文密码" (-not ($r.Body -match [regex]::Escape($NodePassword))) "响应体中出现明文密码"

# ---------- 5. 监控数据 ----------
Write-Host "`n[5] 监控数据通路（01-prd F04/F05/F08）"
$r = Http GET "/api/overview"
Result "总览返回 200 且节点已知" ($r.Code -eq 200 -and $r.Json.health.known -eq $true) `
    ("HTTP " + $r.Code + " kind=" + $r.Json.health.error_kind)
if ($r.Code -eq 200) {
    Write-Host ("        FRP=" + $r.Json.server.version + "  客户端=" + $r.Json.clients.total +
        "  代理=" + $r.Json.proxy_count) -ForegroundColor DarkGray
}

$r = Http GET "/api/clients"
Result "客户端列表可用" ($r.Code -eq 200) ("HTTP " + $r.Code)

$r = Http POST "/api/refresh"
Result "手动刷新返回" ($r.Code -eq 200) ("HTTP " + $r.Code)

# ---------- 6. 前端与护栏 ----------
Write-Host "`n[6] 前端资源与请求护栏"
$r = Http GET "/" -NoAuth
Result "入口页可访问" ($r.Code -eq 200 -and $r.Body -match "<title>SinyeFRP") ("HTTP " + $r.Code)

$r = Http GET "/settings" -NoAuth
Result "SPA 路由回退到入口页" ($r.Code -eq 200 -and $r.Body -match "<title>SinyeFRP") ("HTTP " + $r.Code)

$r = Http POST "/api/node/config" -RawBody -ContentType "application/x-www-form-urlencoded"
Result "非 JSON 正文被拒（415）" ($r.Code -eq 415) ("HTTP " + $r.Code + " " + $r.Body)

$r = Http POST "/api/node/config" '{"name":"x","api_url":"http://127.0.0.1:7500","unknown_field":1}'
Result "未知字段被拒" ($r.Code -eq 400) ("HTTP " + $r.Code)

ClearJar
$r = Http GET "/api/overview" -NoAuth
Result "无凭据访问业务接口返回 401" ($r.Code -eq 401) ("HTTP " + $r.Code)

# ---------- 7. 审计 ----------
Write-Host "`n[7] 审计（01-prd F16/F17）"
# 第 6 步清过 cookie，这里重新登录以取得会话。
$r = Http POST "/api/auth/login" (ConvertTo-Json @{ username = $AdminUser; password = $AdminPass } -Compress) -FreshLogin
Result "重新登录成功" ($r.Code -eq 200) ("HTTP " + $r.Code + " " + $r.Body)

$r = Http GET "/api/audit"
$auditOk = ($r.Code -eq 200 -and $r.Json.items.Count -gt 0)
Result "审计接口返回记录" $auditOk ("HTTP " + $r.Code + " count=" + $(if ($r.Json) { $r.Json.items.Count } else { 0 }))
if ($auditOk) {
    $kinds = ($r.Json.items | Select-Object -ExpandProperty action -Unique) -join ", "
    Write-Host ("        动作类型: " + $kinds) -ForegroundColor DarkGray
    $leak = $r.Body -match [regex]::Escape($NodePassword) -or $r.Body -match [regex]::Escape($AdminPass)
    Result "审计记录不含任何明文口令" (-not $leak) "审计内容中出现明文凭据"
}

# ---------- 8. 退出与限速（放最后，会锁定账号） ----------
Write-Host "`n[8] 退出登录与登录限速（01-prd F02）"
$r = Http POST "/api/auth/logout" '{}'
Result "退出登录成功" ($r.Code -eq 200) ("HTTP " + $r.Code)

$r = Http GET "/api/auth/me"
Result "退出后会话立即失效" ($r.Code -eq 401) ("HTTP " + $r.Code)

ClearJar

# 先测「账号不存在」的话术一致性，必须在触发限速之前：
# 限速按「用户名 + 来源 IP」双键计数，一旦本机 IP 被锁定，
# 后续任何登录都会先返回 429，就测不出话术差异了。
$r = Http POST "/api/auth/login" (ConvertTo-Json @{ username = "nosuchuser"; password = "whatever" } -Compress) -NoAuth
Result "不存在的账号与错误密码返回同一话术" `
    ($r.Code -eq 401 -and $r.Json.error.message -eq "用户名或密码不正确") ("HTTP " + $r.Code + " " + $r.Body)

# 连续失败直到进入锁定。
#
# 断言的是不变量而非具体次数：上面的 nosuchuser 已消耗 1 次本机 IP 的失败额度，
# 而 IP 额度与用户额度共享同一个阈值，写死「第 6 次才 429」会变成脆弱断言。
$codes = @()
for ($i = 1; $i -le 8; $i++) {
    $r = Http POST "/api/auth/login" (ConvertTo-Json @{ username = $AdminUser; password = "wrong-password-$i" } -Compress) -NoAuth
    $codes += $r.Code
}
$firstLock = [array]::IndexOf($codes, 429)
$beforeLock = if ($firstLock -lt 0) { $codes } else { $codes[0..($firstLock - 1)] }
$afterLock = if ($firstLock -lt 0) { @() } else { $codes[$firstLock..($codes.Count - 1)] }

Result "锁定前一律返回 401（不泄露账号是否存在）" `
    (($beforeLock.Count -gt 0) -and (($beforeLock | Where-Object { $_ -ne 401 }).Count -eq 0)) `
    ("实际状态码: " + ($codes -join ","))

Result "失败累计到阈值后返回 429" ($firstLock -ge 0) ("实际状态码: " + ($codes -join ","))

Result "一旦锁定不再回退为 401" `
    (($afterLock.Count -gt 0) -and (($afterLock | Where-Object { $_ -ne 429 }).Count -eq 0)) `
    ("锁定后状态码: " + ($afterLock -join ","))

$r = Http POST "/api/auth/login" (ConvertTo-Json @{ username = $AdminUser; password = $AdminPass } -Compress) -NoAuth
Result "正确密码在锁定期内同样被拒（限速先于校验）" ($r.Code -eq 429) ("HTTP " + $r.Code + " " + $r.Body)

# ---------- 汇总 ----------
if (Test-Path $script:jar) { Remove-Item $script:jar -Force }
Write-Host ""
Write-Host ("=" * 60)
Write-Host ("通过 $script:pass 项，失败 $script:fail 项") -ForegroundColor $(if ($script:fail -eq 0) { "Green" } else { "Red" })
Write-Host ""
if ($script:fail -gt 0) { exit 1 }
exit 0
