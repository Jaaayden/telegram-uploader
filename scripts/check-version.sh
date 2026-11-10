#!/usr/bin/env bash
# Fails when VERSION is malformed or, given a tag argument (e.g. v2.0.0), when
# the tag does not match it. The buildinfo tests check the Go default.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$("$ROOT_DIR/scripts/version.sh")"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "VERSION must be MAJOR.MINOR.PATCH, got '$version'" >&2
  exit 1
fi
if [[ $# -ge 1 && "${1#v}" != "$version" ]]; then
  echo "tag $1 != VERSION ($version)" >&2
  exit 1
fi
