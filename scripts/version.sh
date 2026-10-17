#!/usr/bin/env bash
# Prints the application version. FyneApp.toml is the single source of truth;
# scripts/check-version.sh verifies the other copies agree with it.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
sed -n 's/^Version *= *"\(.*\)"$/\1/p' "$ROOT_DIR/FyneApp.toml" | head -n1
