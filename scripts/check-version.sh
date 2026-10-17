#!/usr/bin/env bash
# Fails when the version copies disagree. With a tag argument (e.g. v1.2.0) the
# tag must also match FyneApp.toml.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$("$ROOT_DIR/scripts/version.sh")"
build="$(sed -n 's/^Build *= *\([0-9][0-9]*\)$/\1/p' "$ROOT_DIR/FyneApp.toml" | head -n1)"
plist="$ROOT_DIR/packaging/macos/Info.plist"
plist_value() {
  awk -v key="$1" '$0 ~ "<key>" key "</key>" { getline; gsub(/<[^>]*>|[ \t]/, ""); print; exit }' "$plist"
}
status=0
[[ -n "$version" ]] || { echo "FyneApp.toml has no Version" >&2; exit 1; }
if [[ "$(plist_value CFBundleShortVersionString)" != "$version" ]]; then
  echo "Info.plist CFBundleShortVersionString != FyneApp.toml Version ($version)" >&2; status=1
fi
if [[ "$(plist_value CFBundleVersion)" != "$build" ]]; then
  echo "Info.plist CFBundleVersion != FyneApp.toml Build ($build)" >&2; status=1
fi
if [[ $# -ge 1 && "${1#v}" != "$version" ]]; then
  echo "tag $1 != FyneApp.toml Version ($version)" >&2; status=1
fi
exit $status
