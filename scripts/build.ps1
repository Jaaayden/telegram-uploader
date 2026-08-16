$ErrorActionPreference = "Stop"

$RootDir = Split-Path -Parent $PSScriptRoot
Set-Location $RootDir

$AppName = if ($env:APP_NAME) { $env:APP_NAME } else { "TelegramVideoUploader.exe" }
$BuildDir = if ($env:BUILD_DIR) { $env:BUILD_DIR } else { "build" }

New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
go test ./...
go build -trimpath -ldflags "-s -w -H=windowsgui" -o (Join-Path $BuildDir $AppName) ./cmd/tg-video-uploader

Write-Host "Built $(Join-Path $BuildDir $AppName)"
