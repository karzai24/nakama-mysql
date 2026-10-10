#!/usr/bin/env bash
# Two-client smoke against the current local Compose Nakama service.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-all}" in
  all) realtime_restart_probe=false ;;
  restart) realtime_restart_probe=true ;;
  *) echo "Usage: bash dev/realtime.sh [all|restart]" >&2; exit 2 ;;
esac
docker run --rm --network nakama-mysql-dev_default --env-file .env \
  -e "NAKAMA_CHAT_RESTART_PROBE=$realtime_restart_probe" \
  -v "$PWD:/src:ro" -w /src \
  -v nakama-mysql-build-cache:/root/.cache/go-build \
  golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 \
  go run -mod=vendor ./dev/realtime
