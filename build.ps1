param(
    [string]$Version = "2.2.0",
    [string]$OutputDir = "dist"
)

$ErrorActionPreference = "Stop"
$previousGoOS = $env:GOOS
$previousGoArch = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
Push-Location $PSScriptRoot
try {
    $env:GOOS = $null
    $env:GOARCH = $null
    Write-Host "==> go test ./..."
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "tests failed" }

    New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
    $client = Join-Path $OutputDir 'Civ6RelayClient.exe'
    $server = Join-Path $OutputDir 'civ6relay-server'
    $check = Join-Path $OutputDir 'civ6relay-tunnelcheck'

    Write-Host "==> client (windows/amd64)"
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version" -o $client ./cmd/client
    if ($LASTEXITCODE -ne 0) { throw "client build failed" }

    Write-Host "==> server (linux/amd64)"
    $env:GOOS = "linux"
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $server ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "server build failed" }
    go build -trimpath -ldflags "-s -w" -o $check ./cmd/tunnelcheck
    if ($LASTEXITCODE -ne 0) { throw "tunnelcheck build failed" }

    Write-Host ""
    Get-Item -LiteralPath $client, $server, $check | Select-Object Name, Length | Format-Table -AutoSize
    Get-FileHash -LiteralPath $client, $server, $check -Algorithm SHA256 | Select-Object Path, Hash | Format-Table -AutoSize
} finally {
    $env:GOOS = $previousGoOS
    $env:GOARCH = $previousGoArch
    $env:CGO_ENABLED = $previousCGO
    Pop-Location
}
