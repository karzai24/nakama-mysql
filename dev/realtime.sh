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
  golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd \
  go run -mod=vendor ./dev/realtime
