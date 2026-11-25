param(
    [string]$Version = "1.0.0"
)

$ErrorActionPreference = "Stop"
Push-Location $PSScriptRoot
try {
    Write-Host "==> go test ./..."
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "tests failed" }

    New-Item -ItemType Directory -Force dist | Out-Null

    Write-Host "==> client (windows/amd64)"
    go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version" -o dist/Civ6RelayClient.exe ./cmd/client
    if ($LASTEXITCODE -ne 0) { throw "client build failed" }

    Write-Host "==> server (linux/amd64)"
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o dist/civ6relay-server ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "server build failed" }
    go build -trimpath -ldflags "-s -w" -o dist/civ6relay-tunnelcheck ./cmd/tunnelcheck
    if ($LASTEXITCODE -ne 0) { throw "tunnelcheck build failed" }

    Write-Host ""
    Get-ChildItem dist | Select-Object Name, Length | Format-Table -AutoSize
    Get-FileHash dist/* -Algorithm SHA256 | Select-Object Path, Hash | Format-Table -AutoSize
} finally {
    Pop-Location
}
