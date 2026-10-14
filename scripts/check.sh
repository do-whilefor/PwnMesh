#!/usr/bin/env bash
set -euo pipefail
source "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"
require_linux
[[ $# -le 1 ]] || fail "Usage: bash scripts/check.sh [go|web|python|all]"
suite=${1:-all}
case "$suite" in go|web|python|all) ;; *) fail "Unknown check suite: $suite" ;; esac
clear_acceptance_env
clear_model_env

if [[ $suite == go || $suite == all ]]; then
    go test -mod=readonly -race -count=1 ./...
    go vet -mod=readonly ./...
fi
if [[ $suite == web || $suite == all ]]; then
    for script in web/static/*.js; do node --check "$script"; done
    node --test web/*_test.cjs
fi
if [[ $suite == python || $suite == all ]]; then
    python3 -m unittest discover -s container -p 'test_*.py'
    python3 -m unittest discover -s tests/scripts -p 'test_*.py'
fi
