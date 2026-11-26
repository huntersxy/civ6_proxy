param(
    [string]$OutDir = "dist\Civ6SyncFix"
)

$ErrorActionPreference = "Stop"
$env:PATH = "C:\msys64\mingw64\bin;$env:PATH"
$gpp = "C:\msys64\mingw64\bin\g++.exe"
$objdump = "C:\msys64\mingw64\bin\objdump.exe"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$src = Join-Path $root "src"
$pkg = Join-Path $root "package"
$build = Join-Path $root "build"
New-Item -ItemType Directory -Path $build -Force | Out-Null

$commonFlags = @("-std=c++17", "-O2", "-Wall", "-Wextra", "-Wno-unused-parameter", "-static-libgcc", "-static-libstdc++", "-static")

Write-Host "[1/4] Compiling C6SE plugin DLL (Civ6SyncFix.dll)..."
& $gpp @commonFlags -shared -o (Join-Path $build "Civ6SyncFix.dll") `
    (Join-Path $src "syncfix_core.cpp") `
    (Join-Path $src "c6se_entry.cpp")
if ($LASTEXITCODE -ne 0) { throw "Failed to build Civ6SyncFix.dll" }

Write-Host "[2/4] Compiling standalone GameCore proxy DLL (GameCore_XP2_Civ6SyncFix_FinalRelease.dll)..."
& $gpp @commonFlags -shared -o (Join-Path $build "GameCore_XP2_Civ6SyncFix_FinalRelease.dll") `
    (Join-Path $src "syncfix_core.cpp") `
    (Join-Path $src "gamecore_proxy.cpp") `
    (Join-Path $src "gamecore_exports.def") `
    "-Wl,--enable-stdcall-fixup"
if ($LASTEXITCODE -ne 0) { throw "Failed to build GameCore_XP2_Civ6SyncFix_FinalRelease.dll" }

Write-Host "[3/4] Compiling & running offline verifier against DX11 & DX12 binaries + unit tests..."
& $gpp @commonFlags -o (Join-Path $build "verify_patterns.exe") `
    (Join-Path $src "syncfix_core.cpp") `
    (Join-Path $src "verify_patterns.cpp")
if ($LASTEXITCODE -ne 0) { throw "Failed to build verify_patterns.exe" }

& (Join-Path $build "verify_patterns.exe")
if ($LASTEXITCODE -ne 0) { throw "Verification FAILED!" }

Write-Host "[4/4] Syncing complete standard Mod tree -> package\ and $OutDir ..."
# Keep mod\Civ6SyncFix\package\ itself a complete, ready-to-upload Civ 6 Mod tree
New-Item -ItemType Directory -Path (Join-Path $pkg "Binaries\Win64\c6se") -Force | Out-Null
Copy-Item (Join-Path $build "Civ6SyncFix.dll") (Join-Path $pkg "Binaries\Win64\c6se\Civ6SyncFix.dll") -Force
Copy-Item (Join-Path $build "GameCore_XP2_Civ6SyncFix_FinalRelease.dll") (Join-Path $pkg "Binaries\Win64\GameCore_XP2_Civ6SyncFix_FinalRelease.dll") -Force

$repoRoot = Split-Path -Parent (Split-Path -Parent $root)
$dest = Join-Path $repoRoot $OutDir
if (Test-Path $dest) { Remove-Item $dest -Recurse -Force }
Copy-Item -Path $pkg -Destination $dest -Recurse -Force

$zipPath = Join-Path (Split-Path -Parent $dest) "Civ6SyncFix.zip"
if (Test-Path $zipPath) { Remove-Item $zipPath -Force }
Compress-Archive -Path $dest -DestinationPath $zipPath -Force

Write-Host "`n=== DLL Dependency & Export Check ==="
& $objdump -p (Join-Path $dest "Binaries\Win64\c6se\Civ6SyncFix.dll") | Select-String "DLL Name:|C6SEPlugin_Info|GetPlugin|DestroyPlugin"
& $objdump -p (Join-Path $dest "Binaries\Win64\GameCore_XP2_Civ6SyncFix_FinalRelease.dll") | Select-String "DLL Name:|DllCreateGameContext|DllDestroyGameContext" | Select-Object -First 15

Write-Host "`nPackaged Mod folder: $dest"
Write-Host "Packaged Mod zip   : $zipPath"
