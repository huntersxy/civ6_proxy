@echo off
chcp 65001 >nul
set "BAT_SELF=%~f0"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$lines = Get-Content -LiteralPath $env:BAT_SELF -Encoding UTF8 | Select-Object -Skip 7; Invoke-Expression ($lines -join [Environment]::NewLine)"
echo.
pause
exit /b
$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

function Copy-SharedFile {
    param(
        [Parameter(Mandatory)][string]$Source,
        [Parameter(Mandatory)][string]$DestDir
    )
    if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) {
        return $false
    }
    New-Item -ItemType Directory -Path $DestDir -Force | Out-Null
    $name = Split-Path -Path $Source -Leaf
    $dest = Join-Path $DestDir $name
    try {
        $inStream = [System.IO.File]::Open($Source, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        try {
            $outStream = [System.IO.File]::Create($dest)
            try {
                $inStream.CopyTo($outStream)
            } finally {
                $outStream.Dispose()
            }
        } finally {
            $inStream.Dispose()
        }
        return $true
    } catch {
        try {
            Copy-Item -LiteralPath $Source -Destination $dest -Force -ErrorAction Stop
            return $true
        } catch {
            Write-Host ('[ERR] Copy failed: ' + $name + ' -> ' + $_.Exception.Message)
            return $false
        }
    }
}

function Add-DirListing {
    param(
        [System.Collections.Generic.List[string]]$Report,
        [string]$Title,
        [string]$Dir
    )
    $Report.Add('')
    $Report.Add("=== $Title ($Dir) ===")
    if (-not (Test-Path -LiteralPath $Dir)) {
        $Report.Add('(Not found)')
        return
    }
    $items = Get-ChildItem -LiteralPath $Dir -Force -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending
    if (-not $items) {
        $Report.Add('(Empty)')
        return
    }
    foreach ($it in $items) {
        $size = if ($it.PSIsContainer) { '<DIR>' } else { '{0,10}' -f $it.Length }
        $Report.Add(('{0:yyyy-MM-dd HH:mm:ss}  {1}  {2}' -f $it.LastWriteTime, $size, $it.Name))
    }
}

function Get-SteamLibraryRoots {
    $candidates = New-Object System.Collections.Generic.List[string]
    $regPaths = @(
        @{ Path = 'HKCU:\Software\Valve\Steam'; Key = 'SteamPath' },
        @{ Path = 'HKLM:\SOFTWARE\WOW6432Node\Valve\Steam'; Key = 'InstallPath' },
        @{ Path = 'HKLM:\SOFTWARE\Valve\Steam'; Key = 'InstallPath' }
    )
    foreach ($rp in $regPaths) {
        try {
            $val = (Get-ItemProperty -LiteralPath $rp.Path -Name $rp.Key -ErrorAction Stop).($rp.Key)
            if ($val) { $candidates.Add($val) }
        } catch {}
    }
    foreach ($envName in @('ProgramFiles(x86)', 'ProgramFiles')) {
        $base = [Environment]::GetEnvironmentVariable($envName)
        if ($base) { $candidates.Add((Join-Path $base 'Steam')) }
    }

    $seen = @{}
    $roots = New-Object System.Collections.Generic.List[string]
    foreach ($c in $candidates) {
        $clean = $c.Trim()
        if (-not $clean) { continue }
        $key = $clean.ToLowerInvariant()
        if (-not $seen.ContainsKey($key)) {
            $seen[$key] = $true
            $roots.Add($clean)
        }
        foreach ($vdfRel in @('steamapps\libraryfolders.vdf', 'config\libraryfolders.vdf')) {
            $vdf = Join-Path $clean $vdfRel
            if (Test-Path -LiteralPath $vdf) {
                $content = Get-Content -LiteralPath $vdf -Raw -ErrorAction SilentlyContinue
                foreach ($m in [regex]::Matches($content, '"path"\s+"([^"]+)"')) {
                    $lib = $m.Groups[1].Value -replace '\\\\', '\'
                    $lkey = $lib.ToLowerInvariant()
                    if ($lib -and -not $seen.ContainsKey($lkey)) {
                        $seen[$lkey] = $true
                        $roots.Add($lib)
                    }
                }
            }
        }
    }
    return $roots
}

$civDir = Join-Path $env:LOCALAPPDATA "Firaxis Games\Sid Meier's Civilization VI"
$civLogs = Join-Path $civDir 'Logs'
$relayLocal = Join-Path $env:LOCALAPPDATA 'Civ6Relay'
$relayLogs = Join-Path $relayLocal 'logs'
$relayCaptures = Join-Path $relayLocal 'captures'
$relayRoaming = Join-Path $env:APPDATA 'Civ6Relay'
$diagReport = Join-Path $env:TEMP 'Civ6RelayDiag.txt'
$docsDir = [Environment]::GetFolderPath('MyDocuments')
$localModsDir = Join-Path $docsDir "My Games\Sid Meier's Civilization VI\Mods"
$syncFixLocalDir = Join-Path $localModsDir 'Civ6SyncFix'

$tmp = Join-Path $env:TEMP ('civ6_collect_' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

$report = New-Object System.Collections.Generic.List[string]
$report.Add('CollectedAt: ' + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss zzz'))
$report.Add('Computer: ' + $env:COMPUTERNAME)
$report.Add('User: ' + $env:USERNAME)
try {
    $os = Get-CimInstance Win32_OperatingSystem -ErrorAction Stop
    $report.Add(('OS: {0} ({1})' -f $os.Caption, $os.Version))
} catch {
    $report.Add('OS: ' + [Environment]::OSVersion.VersionString)
}
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$report.Add('IsAdmin: ' + $isAdmin)

$report.Add('')
$report.Add('=== Running Processes ===')
$procs = Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -match 'Civilization|Civ6Relay' }
if ($procs) {
    foreach ($p in $procs) { $report.Add(('{0} (PID={1})' -f $p.ProcessName, $p.Id)) }
} else {
    $report.Add('(None)')
}

$report.Add('')
$report.Add('=== Civ6SyncFix Mod Status ===')
$report.Add('LocalModsDir: ' + $localModsDir + ' (Exists=' + (Test-Path -LiteralPath $localModsDir) + ')')
$report.Add('LocalCiv6SyncFix: ' + $syncFixLocalDir + ' (Exists=' + (Test-Path -LiteralPath $syncFixLocalDir) + ')')
if (Test-Path -LiteralPath $syncFixLocalDir) {
    Get-ChildItem -LiteralPath $syncFixLocalDir -Recurse -File -ErrorAction SilentlyContinue | ForEach-Object {
        $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256 -ErrorAction SilentlyContinue).Hash
        $report.Add(('  LocalFile: {0} ({1} bytes, SHA256={2})' -f $_.FullName.Substring($syncFixLocalDir.Length + 1), $_.Length, $hash))
    }
}
$steamRoots = Get-SteamLibraryRoots
foreach ($sr in $steamRoots) {
    foreach ($ws in @(
        @{ Id = '3846151010'; Label = 'Civ6SyncFix(Workshop)' },
        @{ Id = '3823991616'; Label = 'Legacy-DX12-SyncFix(3823991616)' }
    )) {
        $wsDir = Join-Path $sr ("steamapps\workshop\content\289070\" + $ws.Id)
        if (Test-Path -LiteralPath $wsDir) {
            $report.Add(('  Found {0}: {1}' -f $ws.Label, $wsDir))
        }
    }
}

$report.Add('')
$report.Add('=== WinDivert Driver & Service ===')
foreach ($base in @($env:ProgramData, $env:LOCALAPPDATA)) {
    if (-not $base) { continue }
    $drv = Join-Path $base 'Civ6Relay\windivert-2.2.2'
    if (Test-Path -LiteralPath $drv) {
        $report.Add('DriverDir: ' + $drv)
        Get-ChildItem -LiteralPath $drv -File -ErrorAction SilentlyContinue | ForEach-Object {
            $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256 -ErrorAction SilentlyContinue).Hash
            $report.Add(('  {0} ({1} bytes, SHA256={2})' -f $_.Name, $_.Length, $hash))
        }
    }
}
$report.Add('--- sc.exe qc WinDivert ---')
$report.Add(((& sc.exe qc WinDivert 2>&1) -join [Environment]::NewLine))
$report.Add('--- sc.exe query WinDivert ---')
$report.Add(((& sc.exe query WinDivert 2>&1) -join [Environment]::NewLine))

$report.Add('')
$report.Add('=== Firewall Rules ===')
foreach ($ruleName in @('Civ6Relay-Game-Inbound', 'Civ6Relay-P2P-Inbound', 'Civ6Relay-P2P-Outbound')) {
    $out = (& netsh advfirewall firewall show rule name="$ruleName" 2>&1) -join [Environment]::NewLine
    $report.Add("--- $ruleName ---")
    $report.Add($out.Trim())
}

Add-DirListing -Report $report -Title 'Civ6 Logs' -Dir $civLogs
Add-DirListing -Report $report -Title 'Civ6Relay Logs' -Dir $relayLogs
Add-DirListing -Report $report -Title 'Civ6Relay Captures' -Dir $relayCaptures

# 1. Civ6 logs
$civTargets = New-Object System.Collections.Generic.List[string]
foreach ($name in @(
    'Civ6SyncFix.log',
    'FastSync.log',
    'net_message_debug.log',
    'net_connection_debug.log',
    'Modding.log',
    'Lua.log',
    'Database.log',
    'Renderer.log',
    'App.log'
)) {
    $civTargets.Add((Join-Path $civLogs $name))
}
if (Test-Path -LiteralPath $civLogs) {
    Get-ChildItem -LiteralPath $civLogs -File -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match 'Sync|Desync|C6SE' -and $_.Length -lt 50MB } |
        ForEach-Object { $civTargets.Add($_.FullName) }
}

# 2. Civ6Relay logs & config & diag
$relayTargets = New-Object System.Collections.Generic.List[string]
if (Test-Path -LiteralPath $relayLogs) {
    Get-ChildItem -LiteralPath $relayLogs -Filter 'client-*.log*' -File -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending |
        Select-Object -First 4 |
        ForEach-Object { $relayTargets.Add($_.FullName) }
}
$relayTargets.Add($diagReport)
$relayTargets.Add((Join-Path $relayRoaming 'config.json'))

# 3. Recent small PCAP captures (up to 2 files, <= 30MB total)
$captureTargets = New-Object System.Collections.Generic.List[string]
if (Test-Path -LiteralPath $relayCaptures) {
    $pcapTotal = [int64]0
    Get-ChildItem -LiteralPath $relayCaptures -Filter '*.pcap' -File -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending |
        Select-Object -First 2 |
        ForEach-Object {
            if (($pcapTotal + $_.Length) -le 30MB) {
                $captureTargets.Add($_.FullName)
                $pcapTotal += $_.Length
            } else {
                $report.Add("Skipped large PCAP: $($_.Name) ($($_.Length) bytes)")
            }
        }
}

$copied = 0
$groups = @(
    @{ SubDir = 'civ6_logs'; Paths = ($civTargets | Select-Object -Unique) },
    @{ SubDir = 'relay_logs'; Paths = ($relayTargets | Select-Object -Unique) },
    @{ SubDir = 'relay_captures'; Paths = ($captureTargets | Select-Object -Unique) }
)

foreach ($g in $groups) {
    $destDir = Join-Path $tmp $g.SubDir
    foreach ($p in $g.Paths) {
        if ($p -and (Copy-SharedFile -Source $p -DestDir $destDir)) {
            $fi = Get-Item -LiteralPath $p
            Write-Host ('[OK] ' + $g.SubDir + '\' + $fi.Name + ' (' + $fi.Length + ' bytes, ' + $fi.LastWriteTime.ToString('MM-dd HH:mm:ss') + ')')
            $copied++
        } elseif ($p -and $g.SubDir -eq 'civ6_logs' -and (Split-Path $p -Leaf) -in @('Civ6SyncFix.log', 'net_message_debug.log', 'net_connection_debug.log', 'Modding.log', 'Lua.log')) {
            Write-Host ('[--] Missing: ' + (Split-Path $p -Leaf))
        }
    }
}

$report | Set-Content -LiteralPath (Join-Path $tmp 'env_check.txt') -Encoding UTF8

$desktop = [Environment]::GetFolderPath('Desktop')
$zipName = ('Civ6_Logs_{0}_{1}.zip' -f $env:COMPUTERNAME, (Get-Date -Format 'yyyyMMdd_HHmmss'))
$out = Join-Path $desktop $zipName
if (Test-Path -LiteralPath $out) { Remove-Item -LiteralPath $out -Force }
Compress-Archive -Path (Join-Path $tmp '*') -DestinationPath $out -Force
Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue

Write-Host ''
Write-Host ("Done! Copied $copied file(s) -> $out")
Start-Process explorer.exe -ArgumentList ('/select,"' + $out + '"')
