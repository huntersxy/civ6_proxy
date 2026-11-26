@echo off
set "BAT_SELF=%~f0"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$code = (Get-Content -LiteralPath $env:BAT_SELF | Select-Object -Skip 6) -join [Environment]::NewLine; Invoke-Expression $code"
echo.
pause
exit /b
$ErrorActionPreference = 'Continue'
$civDir = Join-Path $env:LOCALAPPDATA "Firaxis Games\Sid Meier's Civilization VI"
$civLogs = Join-Path $civDir 'Logs'
$relayLogs = Join-Path $env:LOCALAPPDATA 'Civ6Relay\logs'
$tmp = Join-Path $env:TEMP ('civ6_collect_' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

$report = New-Object System.Collections.Generic.List[string]
$report.Add('CollectedAt: ' + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss zzz'))
$report.Add('Computer: ' + $env:COMPUTERNAME)
$report.Add('CivLogsDir: ' + $civLogs + ' (Exists=' + (Test-Path $civLogs) + ')')
$report.Add('FastSync.log Exists: ' + (Test-Path (Join-Path $civLogs 'FastSync.log')))

$targets = @(
  (Join-Path $civLogs 'FastSync.log'),
  (Join-Path $civLogs 'net_message_debug.log'),
  (Join-Path $civLogs 'net_connection_debug.log'),
  (Join-Path $civLogs 'Modding.log'),
  (Join-Path $civLogs 'Lua.log'),
  (Join-Path $civLogs 'Database.log'),
  (Join-Path $civLogs 'Renderer.log')
)

if (Test-Path $civLogs) {
  $extra = Get-ChildItem $civLogs -File -ErrorAction SilentlyContinue | Where-Object { $_.Name -match 'C6SE|FastSync|Desync|Sync' }
  foreach ($f in $extra) { $targets += $f.FullName }
  $report.Add('--- Civ6 Logs Directory ---')
  Get-ChildItem $civLogs -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | ForEach-Object {
    $report.Add(('{0:yyyy-MM-dd HH:mm:ss}  {1,10}  {2}' -f $_.LastWriteTime, $_.Length, $_.Name))
  }
}

if (Test-Path $relayLogs) {
  $rfiles = Get-ChildItem $relayLogs -Filter 'client-*.log' -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | Select-Object -First 2
  foreach ($f in $rfiles) { $targets += $f.FullName }
  $report.Add('--- Civ6Relay Logs Directory ---')
  Get-ChildItem $relayLogs -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | ForEach-Object {
    $report.Add(('{0:yyyy-MM-dd HH:mm:ss}  {1,10}  {2}' -f $_.LastWriteTime, $_.Length, $_.Name))
  }
}

$copied = 0
foreach ($p in ($targets | Select-Object -Unique)) {
  if ($p -and (Test-Path -LiteralPath $p)) {
    Copy-Item -LiteralPath $p -Destination $tmp -Force -ErrorAction SilentlyContinue
    $fi = Get-Item -LiteralPath $p
    Write-Host ('[OK] ' + $fi.Name + ' (' + $fi.Length + ' bytes, ' + $fi.LastWriteTime.ToString('MM-dd HH:mm:ss') + ')')
    $copied++
  } else {
    Write-Host ('[--] Missing: ' + (Split-Path $p -Leaf))
  }
}

$report | Set-Content -Path (Join-Path $tmp 'env_check.txt') -Encoding UTF8
$out = Join-Path ([Environment]::GetFolderPath('Desktop')) 'Civ6_Teammate_Logs.zip'
if (Test-Path -LiteralPath $out) { Remove-Item -LiteralPath $out -Force }
Compress-Archive -Path (Join-Path $tmp '*') -DestinationPath $out -Force
Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue

Write-Host ''
Write-Host ('Done! Copied ' + $copied + ' log file(s) -> ' + $out)
Start-Process explorer.exe -ArgumentList ('/select,"' + $out + '"')
