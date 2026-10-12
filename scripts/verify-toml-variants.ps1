<#
.SYNOPSIS
    用四象限实验定位：FRP 0.69.0 到底拒绝 BOM 还是拒绝 CRLF。

.DESCRIPTION
    本项目的包内 config.toml 长期是「UTF-8 BOM + 裸 LF」，frpc/frps 都能用。
    当把文本文件统一改成「BOM + CRLF」后，frpc verify -c 开始失败 ——
    但 BOM 与 CRLF 是同时变的，光看结果分不清是谁的锅。

    于是把同一份配置按两个维度各拆一次，四种组合都真跑一遍 frpc verify：

        行尾 \ BOM    无 BOM        有 BOM
        裸 LF         A             B
        CRLF          C             D

    只有真跑才能定论 —— 这类「某个字节格式让解析器不认」的问题，
    靠读文档或猜版本行为都不可靠。

.INPUTS
    配置母本取最近一次 pkg-test 生成的客户端包里的 frpc.toml（真实产物，不是手写的）。

.EXAMPLE
    .\verify-toml-variants.ps1
#>
[CmdletBinding()]
param(
    [string]$Frpc     = '',
    [string]$SourceZip = '',
    [string]$OutFile  = ''
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
if (-not $Frpc)     { $Frpc     = Join-Path $Root 'frp\frpc.exe' }
if (-not $OutFile)  { $OutFile  = Join-Path $Root '.workbuddy\_toml-variants.txt' }

if (-not $SourceZip) {
    $cand = Get-ChildItem (Join-Path $Root '.workbuddy\pkgtest') -Recurse -Filter '*.zip' -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending
    foreach ($c in $cand) {
        $names = @()
        try {
            Add-Type -AssemblyName System.IO.Compression.FileSystem -ErrorAction Stop
            $z = [System.IO.Compression.ZipFile]::OpenRead($c.FullName)
            try { $names = @($z.Entries | ForEach-Object { $_.FullName }) } finally { $z.Dispose() }
        } catch { $names = @() }
        if ($names -contains 'frpc.toml') { $SourceZip = $c.FullName; break }
    }
}

$lines = New-Object System.Collections.Generic.List[string]
function Add-Line([string]$t) { $lines.Add($t) }

Add-Line ('frpc     : ' + $Frpc)
Add-Line ('配置母本 : ' + $SourceZip)
if (-not $Frpc -or -not (Test-Path $Frpc)) { Add-Line '[FATAL] 找不到 frpc.exe'; [System.IO.File]::WriteAllText($OutFile, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($true))); exit 2 }
if (-not $SourceZip -or -not (Test-Path $SourceZip)) { Add-Line '[FATAL] 找不到含 frpc.toml 的包'; [System.IO.File]::WriteAllText($OutFile, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($true))); exit 2 }

$work = Join-Path $Root ('.workbuddy\_tomlvar-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Force -Path $work | Out-Null
Expand-Archive -LiteralPath $SourceZip -DestinationPath $work -Force

$orig = [System.IO.File]::ReadAllBytes((Join-Path $work 'frpc.toml'))
$origHasBom = ($orig.Length -ge 3 -and $orig[0] -eq 0xEF -and $orig[1] -eq 0xBB -and $orig[2] -eq 0xBF)
$body = [System.IO.File]::ReadAllText((Join-Path $work 'frpc.toml'))   # 自动跳 BOM
Add-Line ('母本是否有 BOM : ' + $origHasBom + '   字节数 ' + $orig.Length)
$lfOnly = $body.Replace("`r`n", "`n")
Add-Line ('母本行尾 CRLF 数 : ' + ([regex]::Matches($body, "`r`n").Count) + '   裸 LF 数 ' + ([regex]::Matches($lfOnly, "`n").Count))

$bom = New-Object System.Text.UTF8Encoding($true)
$noBom = New-Object System.Text.UTF8Encoding($false)

$cases = @(
    [pscustomobject]@{ ID = 'A'; Name = '裸 LF + 无 BOM';  Text = $lfOnly;                       Enc = $noBom },
    [pscustomobject]@{ ID = 'B'; Name = '裸 LF + 有 BOM';  Text = $lfOnly;                       Enc = $bom },
    [pscustomobject]@{ ID = 'C'; Name = 'CRLF  + 无 BOM';  Text = $lfOnly.Replace("`n", "`r`n"); Enc = $noBom },
    [pscustomobject]@{ ID = 'D'; Name = 'CRLF  + 有 BOM';  Text = $lfOnly.Replace("`n", "`r`n"); Enc = $bom }
)

Add-Line ''
Add-Line '--- frpc verify -c 结果 ---'
foreach ($c in $cases) {
    $f = Join-Path $work ($c.ID + '.toml')
    [System.IO.File]::WriteAllText($f, $c.Text, $c.Enc)
    $log = Join-Path $work ($c.ID + '.out')
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    & $Frpc verify -c $f *> $log
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prevEap
    $txt = ''
    if (Test-Path $log) { $txt = ([System.IO.File]::ReadAllText($log) -replace '\x1b\[[0-9;]*[A-Za-z]', '').Trim() }
    $verdict = '通过'
    if ($code -ne 0) { $verdict = '失败' }
    Add-Line ('  [{0}] {1,-16} 退出码={2}  {3}' -f $c.ID, $c.Name, $code, $verdict)
    if ($txt) {
        $first = ($txt -split "`r?`n") | Select-Object -First 3
        foreach ($l in $first) { Add-Line ('        ' + $l) }
    }
}

[System.IO.File]::WriteAllText($OutFile, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($true)))
Write-Output 'done'
