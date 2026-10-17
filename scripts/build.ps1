$ErrorActionPreference = "Stop"

$RootDir = Split-Path -Parent $PSScriptRoot
Set-Location $RootDir

$AppName = if ($env:APP_NAME) { $env:APP_NAME } else { "TelegramVideoUploader.exe" }
$BuildDir = if ($env:BUILD_DIR) { $env:BUILD_DIR } else { "build" }

$Version = if ($env:VERSION) { $env:VERSION } else {
    (Select-String -Path (Join-Path $RootDir "FyneApp.toml") -Pattern '^Version\s*=\s*"(.*)"$').Matches[0].Groups[1].Value
}
$VersionFlag = "-X github.com/Jaaayden/telegram-uploader/internal/buildinfo.Version=$Version"

New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
go test ./...
go build -trimpath -ldflags "-s -w -H=windowsgui $VersionFlag" -o (Join-Path $BuildDir $AppName) ./cmd/tg-video-uploader

Write-Host "Built $(Join-Path $BuildDir $AppName)"
