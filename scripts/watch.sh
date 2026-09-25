#!/bin/bash
# Rebuild and test on every .go/go.mod change. Ctrl+C to stop.
set -uo pipefail
cd "$(dirname "$0")/.."

cycle() {
  clear
  printf '\033[1m%s  make check build\033[0m\n' "$(date +%T)"
  out=$(make --no-print-directory check build 2>&1)
  rc=$?
  grep -v -e '^ok ' -e 'no test files' <<<"$out"
  if [ "$rc" -eq 0 ]; then
    printf '\033[32m✓ green\033[0m — run `make dev` in another terminal\n'
  else
    printf '\033[31m✗ failed\033[0m\n'
  fi
}

cycle
while inotifywait -qq -r -e close_write,create,delete,move \
    --include '(\.go|go\.mod|go\.sum)$' cmd internal go.mod go.sum; do
  sleep 0.2 # let editors finish multi-file saves
  cycle
done
