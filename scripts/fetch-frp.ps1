<#
.SYNOPSIS
从上游 Release 拉取 FRP 二进制到 frp\ 目录。

.DESCRIPTION
仓库不含 FRP 二进制（见 .gitignore 第 2 节），clone 之后必须先执行本脚本，
再执行 scripts\build.ps1 —— 后者只负责把 frp\ 下的文件复制进
internal\pack\assets\ 供 go:embed 内嵌，不会自己下载。

下载三个平台：
    windows_amd64   frpc.exe + frps.exe    构建必需
    linux_amd64     frpc                   可选
    linux_arm64     frpc                   可选

必需与可选的划分与 build.ps1 保持一致：Linux 两个缺了只警告，
意味着「出不了 Linux 客户端包」，Windows 包照常能出。判死会让
网络不通的机器连 Windows 包都出不来，不划算。

.EXAMPLE
.\scripts\fetch-frp.ps1
.\scripts\fetch-frp.ps1 -Version 0.69.5 -Force
#>
param(
    # 要与 internal\frpapi\SupportedVersionPrefixes 允许的范围一致（当前 0.69. / 0.71.）。
    [string]$Version = '0.71.0',
    # 已存在时也重新下载覆盖。
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Root   = Split-Path -Parent $PSScriptRoot
$FrpDir = Join-Path $Root 'frp'

function Fail([int]$Code, [string]$Msg) {
    Write-Host ('[FAIL] ' + $Msg) -ForegroundColor Red
    exit $Code
}

# TLS 1.2：PS 5.1 默认不启用，连 GitHub 会被服务端直接断掉，
# 而报错信息只会说「基础连接已关闭」，看不出是这里的问题。
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {
    Fail 2 ('无法启用 TLS 1.2: ' + $_.Exception.Message)
}

$targets = @(
    @{ Plat = 'windows_amd64'; Archive = 'zip';    Files = @('frpc.exe', 'frps.exe'); Sub = '';            Required = $true  },
    @{ Plat = 'linux_amd64';   Archive = 'tar.gz'; Files = @('frpc');                 Sub = 'linux-amd64'; Required = $false },
    @{ Plat = 'linux_arm64';   Archive = 'tar.gz'; Files = @('frpc');                 Sub = 'linux-arm64'; Required = $false }
)

if (-not (Test-Path $FrpDir)) { New-Item -ItemType Directory -Force -Path $FrpDir | Out-Null }

$tmp = Join-Path $env:TEMP ('sinyefrp-fetch-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

$warn = 0

foreach ($t in $targets) {
    $label = $t.Plat
    $file  = 'frp_' + $Version + '_' + $t.Plat + '.' + $t.Archive
    $url   = 'https://github.com/fatedier/frp/releases/download/v' + $Version + '/' + $file
    $dest  = Join-Path $tmp $file

    if ($t.Sub) { $dstDir = Join-Path $FrpDir $t.Sub } else { $dstDir = $FrpDir }
    if (-not (Test-Path $dstDir)) { New-Item -ItemType Directory -Force -Path $dstDir | Out-Null }

    # 已存在且未指定 -Force：跳过下载。
    $missing = @()
    foreach ($f in $t.Files) {
        if (-not (Test-Path (Join-Path $dstDir $f))) { $missing += $f }
    }
    if ((-not $Force) -and $missing.Count -eq 0) {
        Write-Host ('[skip] ' + $label + ' 已就绪') -ForegroundColor DarkGray
        continue
    }

    Write-Host ('[get ] ' + $label + ' <- ' + $file) -ForegroundColor Cyan
    try {
        Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing
    } catch {
        if ($t.Required) { Fail 3 ('下载失败: ' + $url + ' | ' + $_.Exception.Message) }
        Write-Host ('[warn] ' + $label + ' 下载失败，跳过: ' + $_.Exception.Message) -ForegroundColor Yellow
        $warn++
        continue
    }

    $exDir = Join-Path $tmp ('x-' + $t.Plat)
    New-Item -ItemType Directory -Force -Path $exDir | Out-Null

    if ($t.Archive -eq 'zip') {
        Expand-Archive -LiteralPath $dest -DestinationPath $exDir -Force
    } else {
        & tar -xzf $dest -C $exDir
        if ($LASTEXITCODE -ne 0) {
            if ($t.Required) { Fail 4 ('解压失败: ' + $file) }
            Write-Host ('[warn] ' + $label + ' 解压失败，跳过') -ForegroundColor Yellow
            $warn++
            continue
        }
    }

    # 压缩包内是一层以文件名命名的目录；布局变了就直接退到解压根，
    # 免得因为上游改了目录结构而报出一个看不懂的「找不到文件」。
    $inner = Join-Path $exDir ('frp_' + $Version + '_' + $t.Plat)
    if (-not (Test-Path $inner)) { $inner = $exDir }

    foreach ($f in $t.Files) {
        $src = Join-Path $inner $f
        if (-not (Test-Path $src)) {
            $msg = '压缩包内缺少 ' + $f
            if ($t.Required) { Fail 5 ($label + ': ' + $msg) }
            Write-Host ('[warn] ' + $label + ': ' + $msg) -ForegroundColor Yellow
            $warn++
            continue
        }
        Copy-Item $src (Join-Path $dstDir $f) -Force
        if ($t.Sub) { $shown = $t.Sub + '\' + $f } else { $shown = 'frp\' + $f }
        Write-Host ('[ ok ] ' + $shown) -ForegroundColor Green
    }
}

Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue

Write-Host ''
Write-Host ('FRP ' + $Version + ' 就绪：' + $FrpDir) -ForegroundColor Green
if ($warn -gt 0) {
    Write-Host ('有 ' + $warn + ' 个可选平台未获取到（不影响 Windows 构建）') -ForegroundColor Yellow
}
Write-Host '下一步：scripts\build.ps1'
exit 0
