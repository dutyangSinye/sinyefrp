<#
.SYNOPSIS
    构建 SinyeFRP 管理平台单文件可执行程序。

.DESCRIPTION
    1. 构建前端（web/ → internal/web/dist/）；
    2. 把 frpc.exe / frps.exe 复制到 internal/pack/assets/，使其被 go:embed 内嵌；
    3. 编译出单个 sinyefrp.exe；
    4. 执行 -version 自检。

    为什么必须内嵌而不是运行时读目录：
      - 交付件只有一个文件，满足 01-prd F18「解压即用、不装运行时」；
      - 「平台认知的 FRP 版本」与「生成进部署包里的二进制」在编译期焊死，
        不可能出现版本错配；文件哈希来源固定，正好满足 F15 的记录要求；
      - 少一个 assets/ 目录，就少一个「目录不完整导致功能失效」的现场。

    前端同理：internal/web/dist 是构建产物，不随源码一起维护。

.PARAMETER FrpDir
    FRP 二进制所在目录，默认 <仓库根>\frp。

.PARAMETER Output
    输出文件路径，默认 <仓库根>\bin\sinyefrp.exe。

.PARAMETER SkipWeb
    跳过前端构建，直接使用 internal/web/dist 里已有的产物。
    仅在只改了后端代码、且前端产物是最新的时候使用。

.PARAMETER SkipVerify
    跳过构建后的自检（不执行 -version）。

.EXAMPLE
    .\scripts\build.ps1
    .\scripts\build.ps1 -SkipWeb
    .\scripts\build.ps1 -FrpDir D:\downloads\frp -Output D:\dist\sinyefrp.exe
#>
[CmdletBinding()]
param(
    [string]$FrpDir = '',
    [string]$Output = '',
    [switch]$SkipWeb,
    [switch]$SkipVerify
)

$ErrorActionPreference = 'Stop'

$Root    = Split-Path -Parent $PSScriptRoot
$goenv   = Join-Path $PSScriptRoot 'goenv.ps1'
$assets  = Join-Path $Root 'internal\pack\assets'
$webDir  = Join-Path $Root 'web'

if (-not $FrpDir) { $FrpDir = Join-Path $Root 'frp' }
if (-not $Output) { $Output = Join-Path $Root 'bin\sinyefrp.exe' }

function Fail([int]$Code, [string]$Message) {
    Write-Host ('[错误] ' + $Message) -ForegroundColor Red
    exit $Code
}

# 运行原生程序并把 stdout 与 stderr 一起收成字符串。
#
# 这不是为了少写几行 —— PowerShell 5.1 有一个必须绕开的语义陷阱：
# 原生程序往 stderr 写内容时，如果同时用了 2>&1 重定向且
# $ErrorActionPreference = 'Stop'，PowerShell 会把那些 stderr 文本包装成
# NativeCommandError 当作**终止错误**抛出。结果是脚本在调用处直接中断，
# 自定义的 '编译失败: ...' 提示和 $LASTEXITCODE 判断永远走不到，
# 而 vite 每次构建都会往 stderr 打 chunk 体积告警，正好踩中这个坑。
# 因此这里把 ErrorActionPreference 临时降级，并把全部输出落到临时文件再读回。
function Invoke-Native {
    param(
        [Parameter(Mandatory)][string]$File,
        [string[]]$Arguments = @(),
        [string]$WorkDir = ''
    )
    $prevEap = $ErrorActionPreference
    $prevDir = (Get-Location).ProviderPath
    $log = Join-Path $env:TEMP ('sinyefrp-native-' + [guid]::NewGuid().ToString('N') + '.log')
    try {
        $ErrorActionPreference = 'Continue'
        if ($WorkDir) { Set-Location -LiteralPath $WorkDir }
        & $File @Arguments *> $log
        $code = $LASTEXITCODE
        $text = ''
        if (Test-Path $log) {
            $text = [System.IO.File]::ReadAllText($log)
            # 去掉 ANSI 颜色转义，否则错误信息里夹杂控制序列，很难读
            $text = $text -replace '\x1b\[[0-9;]*[A-Za-z]', ''
        }
        return [pscustomobject]@{ Code = $code; Text = $text.Trim() }
    } finally {
        $ErrorActionPreference = $prevEap
        if ($prevDir) { Set-Location -LiteralPath $prevDir }
        Remove-Item $log -Force -ErrorAction SilentlyContinue
    }
}

Write-Host '=== SinyeFRP 构建 ===' -ForegroundColor Cyan
Write-Host ('仓库根目录: ' + $Root)

# 1) 前端构建
#    必须在 go build 之前完成：dist 的内容会被 go:embed 编进 exe，
#    顺序颠倒会打包出上一版前端。
if ($SkipWeb) {
    Write-Host '[1/4] 已跳过前端构建（-SkipWeb）' -ForegroundColor Yellow
} elseif (-not (Test-Path (Join-Path $webDir 'package.json'))) {
    Write-Host '[1/4] 未找到 web\package.json，跳过前端构建' -ForegroundColor Yellow
} elseif (-not (Test-Path (Join-Path $webDir 'node_modules'))) {
    Write-Host '[1/4] 前端依赖未安装，请在 web\ 下先执行 npm install' -ForegroundColor Yellow
    Write-Host '      本次将沿用 internal\web\dist 中已有的产物' -ForegroundColor Yellow
} else {
    Write-Host '[1/4] 正在构建前端…' -ForegroundColor Cyan
    # 只用 vite build，不跑 vue-tsc：类型检查由交付前的独立步骤执行，
    # 挂在构建路径上会让「改一行样式」也背上全量类型检查的代价。
    $web = Invoke-Native -File 'npm.cmd' -Arguments @('run', 'build:only') -WorkDir $webDir
    if ($web.Code -ne 0) {
        Fail 14 ('前端构建失败（退出码 ' + $web.Code + '）:' + [Environment]::NewLine + $web.Text)
    }
    $distIndex = Join-Path $Root 'internal\web\dist\index.html'
    if (-not (Test-Path $distIndex)) {
        Fail 14 '前端构建未产出 internal\web\dist\index.html'
    }
    Write-Host ('[1/4] 前端构建完成，入口文件时间: ' + (Get-Item $distIndex).LastWriteTime) -ForegroundColor Green
}

# 2) 准备 Go 环境
if (Test-Path $goenv) {
    . $goenv
} else {
    Write-Host '未找到 scripts\goenv.ps1，改用 PATH 中的 go' -ForegroundColor Yellow
}
$goVer = Invoke-Native -File 'go' -Arguments @('version')
if ($goVer.Code -ne 0) {
    Fail 11 ('未找到可用的 Go 工具链: ' + $goVer.Text)
}
Write-Host ('Go: ' + $goVer.Text)

# 3) 校验并复制 FRP 二进制
$frpc = Join-Path $FrpDir 'frpc.exe'
$frps = Join-Path $FrpDir 'frps.exe'
foreach ($f in @($frpc, $frps)) {
    if (-not (Test-Path $f)) { Fail 11 ('缺少 FRP 二进制: ' + $f) }
}

$frpVer = Invoke-Native -File $frpc -Arguments @('--version')
Write-Host ('FRP 版本: ' + $frpVer.Text)

if (-not (Test-Path $assets)) { New-Item -ItemType Directory -Force -Path $assets | Out-Null }
foreach ($pair in @(@($frpc, 'frpc.exe'), @($frps, 'frps.exe'))) {
    $src = $pair[0]
    $dst = Join-Path $assets $pair[1]
    $needCopy = $true
    if (Test-Path $dst) {
        $a = (Get-FileHash $src -Algorithm SHA256).Hash
        $b = (Get-FileHash $dst -Algorithm SHA256).Hash
        if ($a -eq $b) { $needCopy = $false }
    }
    if ($needCopy) {
        Copy-Item $src $dst -Force
        Write-Host ('[2/4] 已复制 ' + $pair[1]) -ForegroundColor Green
    } else {
        Write-Host ('[2/4] ' + $pair[1] + ' 未变化，跳过复制') -ForegroundColor DarkGray
    }
}

# Linux 目标平台的二进制：可选。
#
# 缺了**不阻断构建**，只警告：那意味着「出不了 Linux 客户端包」，
# 而 Windows 包照常能出。判死会让没备齐 Linux 二进制的开发机直接构建失败，
# 换来的只是少一个可选能力，不划算 —— 而且失败时看不出该去哪里补文件。
foreach ($lp in @(@('linux-amd64', 'frpc'), @('linux-arm64', 'frpc'))) {
    $sub  = $lp[0]
    $name = $lp[1]
    $src  = Join-Path $FrpDir (Join-Path $sub $name)
    if (-not (Test-Path $src)) {
        Write-Host ('[2/4] 未找到 ' + $sub + '\' + $name + '，本次构建不提供该目标平台') -ForegroundColor Yellow
        continue
    }
    $dstDir = Join-Path $assets $sub
    if (-not (Test-Path $dstDir)) { New-Item -ItemType Directory -Force -Path $dstDir | Out-Null }
    $dst = Join-Path $dstDir $name
    $needCopy = $true
    if (Test-Path $dst) {
        $a = (Get-FileHash $src -Algorithm SHA256).Hash
        $b = (Get-FileHash $dst -Algorithm SHA256).Hash
        if ($a -eq $b) { $needCopy = $false }
    }
    if ($needCopy) {
        Copy-Item $src $dst -Force
        Write-Host ('[2/4] 已复制 ' + $sub + '\' + $name) -ForegroundColor Green
    } else {
        Write-Host ('[2/4] ' + $sub + '\' + $name + ' 未变化，跳过复制') -ForegroundColor DarkGray
    }
}

# 4) 编译
$outDir = Split-Path -Parent $Output
if ($outDir -and -not (Test-Path $outDir)) { New-Item -ItemType Directory -Force -Path $outDir | Out-Null }

Write-Host '[3/4] 正在编译…' -ForegroundColor Cyan
$build = Invoke-Native -File 'go' -Arguments @('build', '-trimpath', '-ldflags', '-s -w', '-o', $Output, './cmd/sinyefrp') -WorkDir $Root
if ($build.Code -ne 0) {
    Fail 13 ('编译失败（退出码 ' + $build.Code + '）:' + [Environment]::NewLine + $build.Text)
}

if (-not (Test-Path $Output)) { Fail 13 '编译未产出可执行文件' }

$item = Get-Item $Output
Write-Host ('[4/4] 构建完成: ' + $item.FullName) -ForegroundColor Green
Write-Host ('体积    : ' + [math]::Round($item.Length / 1MB, 2) + ' MB')
Write-Host ('SHA256  : ' + (Get-FileHash $Output -Algorithm SHA256).Hash)

if (-not $SkipVerify) {
    $v = Invoke-Native -File $Output -Arguments @('-version')
    Write-Host ('自检    : ' + $v.Text)
}

exit 0
