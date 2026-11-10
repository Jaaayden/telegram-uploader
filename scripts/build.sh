#!/usr/bin/env bash
# Builds the application for the host system, or for another macOS
# architecture with GOARCH=amd64|arm64. On macOS the result is an ad-hoc signed
# .app bundle; set ARCHIVE=name.zip to also produce the release archive.
#
#   BUILD_VERSION  version string to embed (default: contents of VERSION)
#   BUILD_COMMIT   source revision to embed (default: git HEAD, if available)
#   SKIP_TESTS=1   do not run "go test ./..." first
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

APP_NAME="TelegramVideoUploader"
BUILD_DIR="${BUILD_DIR:-build}"
GO_BIN="${GO:-go}"
HOST_OS="$(uname -s)"
PACKAGE="github.com/Jaaayden/telegram-uploader/internal/buildinfo"

"$ROOT_DIR/scripts/check-version.sh"
VERSION="$("$ROOT_DIR/scripts/version.sh")"
BUILD_VERSION="${BUILD_VERSION:-$VERSION}"
BUILD_COMMIT="${BUILD_COMMIT:-$(git rev-parse HEAD 2>/dev/null || true)}"
LDFLAGS="-s -w -X $PACKAGE.Version=$BUILD_VERSION -X $PACKAGE.Commit=$BUILD_COMMIT"

if [[ "${SKIP_TESTS:-}" != "1" ]]; then
  "$GO_BIN" test ./...
fi
mkdir -p "$BUILD_DIR"

if [[ "$HOST_OS" != "Darwin" ]]; then
  "$GO_BIN" build -trimpath -ldflags="$LDFLAGS" -o "$BUILD_DIR/$APP_NAME" ./cmd/tg-video-uploader
  printf 'Built %s\n' "$BUILD_DIR/$APP_NAME"
  exit 0
fi

# The window is a small Objective-C shim around WKWebView, so macOS builds
# need cgo. Cross-building between arm64 and x86_64 only needs clang's -arch.
export CGO_ENABLED=1
export GOARCH="${GOARCH:-$("$GO_BIN" env GOARCH)}"
case "$GOARCH" in
  arm64) CLANG_ARCH=arm64 ;;
  amd64) CLANG_ARCH=x86_64 ;;
  *) echo "unsupported GOARCH for macOS: $GOARCH" >&2; exit 1 ;;
esac
export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-12.0}"
MIN_VERSION_FLAG="-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
export CC="clang -arch $CLANG_ARCH"
export CGO_CFLAGS="-O2 $MIN_VERSION_FLAG"
export CGO_LDFLAGS="$MIN_VERSION_FLAG"

APP_BUNDLE="$BUILD_DIR/$CLANG_ARCH/$APP_NAME.app"
rm -rf "$APP_BUNDLE"
mkdir -p "$APP_BUNDLE/Contents/MacOS" "$APP_BUNDLE/Contents/Resources"
"$GO_BIN" build -trimpath -ldflags="$LDFLAGS" -o "$APP_BUNDLE/Contents/MacOS/$APP_NAME" ./cmd/tg-video-uploader
sed "s/@VERSION@/$VERSION/g" packaging/macos/Info.plist > "$APP_BUNDLE/Contents/Info.plist"
cp packaging/macos/AppIcon.icns "$APP_BUNDLE/Contents/Resources/AppIcon.icns"
plutil -lint "$APP_BUNDLE/Contents/Info.plist" > /dev/null
codesign --force --deep --sign - "$APP_BUNDLE"
codesign --verify --deep --strict "$APP_BUNDLE"
printf 'Built %s\n' "$APP_BUNDLE"

if [[ -n "${ARCHIVE:-}" ]]; then
  rm -f "$BUILD_DIR/$ARCHIVE"
  ditto -c -k --sequesterRsrc --keepParent "$APP_BUNDLE" "$BUILD_DIR/$ARCHIVE"
  printf 'Built %s\n' "$BUILD_DIR/$ARCHIVE"
fi
