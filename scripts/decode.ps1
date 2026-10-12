param([string]$Path, [string]$Pattern = 'FAIL', [int]$Tail = 0)
$bytes = [System.IO.File]::ReadAllBytes($Path)
$enc = $null
if ($bytes.Length -ge 2 -and $bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE) { $enc = [System.Text.Encoding]::Unicode }
elseif ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) { $enc = [System.Text.Encoding]::UTF8 }
else { $enc = [System.Text.Encoding]::UTF8 }
$text = $enc.GetString($bytes)
if ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) { $text = $text.TrimStart([char]0xFEFF) }
$lines = $text -split "`r?`n"
Write-Output ("FILE  : " + $Path)
Write-Output ("ENC   : " + $enc.WebName + "  LINES: " + $lines.Count)
if ($Pattern) {
  Write-Output "--- MATCH ---"
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match $Pattern) { Write-Output ("[" + ($i + 1) + "] " + $lines[$i]) }
  }
}
if ($Tail -gt 0) {
  Write-Output "--- TAIL ---"
  $s = [Math]::Max(0, $lines.Count - $Tail)
  for ($i = $s; $i -lt $lines.Count; $i++) { Write-Output ("[" + ($i + 1) + "] " + $lines[$i]) }
}
