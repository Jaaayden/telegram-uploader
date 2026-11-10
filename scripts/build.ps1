# Builds the Windows application. No C compiler is needed: the window uses the
# system WebView2 runtime through pure Go.
#
#   BUILD_VERSION  version string to embed (default: contents of VERSION)
#   BUILD_COMMIT   source revision to embed (default: git HEAD, if available)
#   SKIP_TESTS=1   do not run "go test ./..." first
$ErrorActionPreference = "Stop"

$RootDir = Split-Path -Parent $PSScriptRoot
Set-Location $RootDir

$Output = if ($env:OUTPUT) { $env:OUTPUT } else { "build/TelegramVideoUploader.exe" }
$Version = (Get-Content (Join-Path $RootDir "VERSION") -Raw).Trim()
if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "VERSION must be MAJOR.MINOR.PATCH, got '$Version'" }
$BuildVersion = if ($env:BUILD_VERSION) { $env:BUILD_VERSION } else { $Version }
$Commit = if ($env:BUILD_COMMIT) { $env:BUILD_COMMIT } else { (git rev-parse HEAD 2>$null) }
$Package = "github.com/Jaaayden/telegram-uploader/internal/buildinfo"

$env:CGO_ENABLED = "0"
if ($env:SKIP_TESTS -ne "1") {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "tests failed" }
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Output) | Out-Null
go build -trimpath -ldflags "-s -w -H=windowsgui -X $Package.Version=$BuildVersion -X $Package.Commit=$Commit" -o $Output ./cmd/tg-video-uploader
if ($LASTEXITCODE -ne 0) { throw "build failed" }

Write-Host "Built $Output"
