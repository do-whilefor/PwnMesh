#!/usr/bin/env bash
set -euo pipefail
source "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"
require_linux
[[ $# -le 1 ]] || fail "Usage: bash scripts/build.sh [output]"
output=${1:-bin/pwnmesh}
build_web
mkdir -p -- "$(dirname -- "$output")"
CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o "$output" ./cmd/pwnmesh
