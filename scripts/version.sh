#!/usr/bin/env bash
# Prints the application version. The VERSION file is the single source of
# truth: the build scripts inject it into the binary and the macOS Info.plist.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tr -d '[:space:]' < "$ROOT_DIR/VERSION"
