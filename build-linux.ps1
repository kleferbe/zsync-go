# Cross-compile zsync for linux/amd64 into zsync-go/zsync.
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$module = Join-Path $root "zsync-go"
$out = Join-Path $module "zsync"

$saved = @{
    GOOS        = $env:GOOS
    GOARCH      = $env:GOARCH
    CGO_ENABLED = $env:CGO_ENABLED
}

$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
Push-Location $module
try {
    go build -o $out ./cmd/zsync
}
finally {
    Pop-Location
    foreach ($name in $saved.Keys) {
        Set-Item -Path "Env:$name" -Value $saved[$name]
    }
}

Write-Host "built $out"
