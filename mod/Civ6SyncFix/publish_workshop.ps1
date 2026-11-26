param(
    [string]$SteamUser = "",
    [string]$PublishedFileId = "",
    [string]$ChangeNote = "",
    [ValidateSet("0", "1", "2")]
    [string]$Visibility = ""
)

$ErrorActionPreference = "Stop"

$modRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Split-Path -Parent (Split-Path -Parent $modRoot)
$contentDir = Join-Path $repoRoot "dist\Civ6SyncFix"
$vdfPath = Join-Path $modRoot "workshop_upload.vdf"
$previewPath = Join-Path $modRoot "workshop_preview.jpg"
$steamCmdDir = Join-Path $modRoot "tools\steamcmd"
$steamCmdExe = Join-Path $steamCmdDir "steamcmd.exe"

if (-not (Test-Path (Join-Path $contentDir "Civ6SyncFix.modinfo"))) {
    Write-Host "[1/4] dist\Civ6SyncFix missing, building Civ6SyncFix first..."
    & (Join-Path $modRoot "build.ps1")
}

if (-not (Test-Path $previewPath)) {
    throw "Missing preview image: $previewPath"
}

$previewSize = (Get-Item $previewPath).Length
if ($previewSize -gt 1048576) {
    throw "Preview image exceeds Steam's 1 MB limit ($previewSize bytes): $previewPath"
}

# Ensure official Valve SteamCMD is present
if (-not (Test-Path $steamCmdExe)) {
    Write-Host "[2/4] Downloading official Valve SteamCMD..."
    New-Item -ItemType Directory -Path $steamCmdDir -Force | Out-Null
    $zipFile = Join-Path $steamCmdDir "steamcmd.zip"
    $url = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"
    if (Get-Command curl.exe -ErrorAction SilentlyContinue) {
        & curl.exe -fSL --connect-timeout 20 --max-time 120 -o $zipFile $url
        if ($LASTEXITCODE -ne 0) { throw "Failed to download SteamCMD from $url (curl exit $LASTEXITCODE)" }
    } else {
        $ProgressPreference = "SilentlyContinue"
        Invoke-WebRequest -Uri $url -OutFile $zipFile
    }
    Expand-Archive -Path $zipFile -DestinationPath $steamCmdDir -Force
    Remove-Item $zipFile -Force
}

# Update optional VDF overrides (UTF-8 without BOM)
$vdfText = Get-Content -LiteralPath $vdfPath -Raw -Encoding UTF8
$escapedContent = ($contentDir -replace '\\', '\\')
$vdfText = [regex]::Replace($vdfText, '("contentfolder"\s+)"[^"]*"', "`$1`"$escapedContent`"")
if ($PublishedFileId -ne "") {
    $vdfText = [regex]::Replace($vdfText, '("publishedfileid"\s+)"[^"]*"', "`$1`"$PublishedFileId`"")
}
if ($Visibility -ne "") {
    $vdfText = [regex]::Replace($vdfText, '("visibility"\s+)"[^"]*"', "`$1`"$Visibility`"")
}
if ($ChangeNote -ne "") {
    $escapedNote = ($ChangeNote -replace '\\', '\\' -replace '"', '\"')
    $vdfText = [regex]::Replace($vdfText, '("changenote"\s+)"[^"]*"', "`$1`"$escapedNote`"")
}
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($vdfPath, $vdfText, $utf8NoBom)

if ([string]::IsNullOrWhiteSpace($SteamUser)) {
    $SteamUser = Read-Host "请输入你的 Steam 登录账号名（不是昵称）"
}
if ([string]::IsNullOrWhiteSpace($SteamUser)) {
    throw "Steam 登录账号名不能为空。"
}

Write-Host "[3/4] Content folder : $contentDir"
Write-Host "      VDF config     : $vdfPath"
Write-Host "      Preview image  : $previewPath (首次发布后请在工坊网页上传封面图)"
Write-Host "[4/4] Launching SteamCMD (请按提示输入密码并通过手机 Steam Guard 确认)..."
Write-Host ""

& $steamCmdExe +login $SteamUser +workshop_build_item $vdfPath +quit
$exitCode = $LASTEXITCODE
if ($exitCode -ne 0) {
    Write-Host "`nSteamCMD 退出码: $exitCode（若首次下载自动更新后退出，重新运行一次本脚本即可）"
    exit $exitCode
}

Write-Host "`n发布完成！"
Write-Host "提示：如果是首次发布，请记下上方输出的 PublishedFileID，并填回 $vdfPath 的 publishedfileid，以便后续直接覆盖更新。"
Write-Host "首次发布还需在浏览器打开创意工坊页面，按页面顶部提示同意《Steam 创意工坊法律协议》。"
