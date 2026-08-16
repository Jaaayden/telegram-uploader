#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

APP_NAME="${APP_NAME:-TelegramVideoUploader}"
BUILD_DIR="${BUILD_DIR:-build}"
GO_BIN="${GO:-go}"
HOST_OS="$(uname -s)"

if [[ "$HOST_OS" == "Darwin" ]]; then
  export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-11.0}"
  MIN_VERSION_FLAG="-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
  export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} $MIN_VERSION_FLAG"
  export CGO_CXXFLAGS="${CGO_CXXFLAGS:--O2 -g} $MIN_VERSION_FLAG"
  export CGO_LDFLAGS="${CGO_LDFLAGS:+${CGO_LDFLAGS} }$MIN_VERSION_FLAG"
fi

mkdir -p "$BUILD_DIR"
"$GO_BIN" test ./...
"$GO_BIN" build -trimpath -ldflags="-s -w" -o "$BUILD_DIR/$APP_NAME" ./cmd/tg-video-uploader

printf 'Built %s\n' "$BUILD_DIR/$APP_NAME"

if [[ "$HOST_OS" == "Darwin" ]]; then
  APP_BUNDLE="$BUILD_DIR/$APP_NAME.app"
  mkdir -p "$APP_BUNDLE/Contents/MacOS" "$APP_BUNDLE/Contents/Resources"
  cp "$BUILD_DIR/$APP_NAME" "$APP_BUNDLE/Contents/MacOS/TelegramVideoUploader"
  cp "$ROOT_DIR/packaging/macos/Info.plist" "$APP_BUNDLE/Contents/Info.plist"
  codesign --force --deep --sign - "$APP_BUNDLE"
  printf 'Built %s\n' "$APP_BUNDLE"
fi
