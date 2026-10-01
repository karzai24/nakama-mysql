#!/usr/bin/env bash
# The selected local database must already be running. No volumes are removed.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-mysql}" in
  mysql) export NAKAMA_DATABASE_BACKEND="${1:-mysql}" ;;
  *) echo "Usage: bash dev/console.sh [mysql]" >&2; exit 2 ;;
esac
docker compose -f compose.dev.yml up -d --no-deps --wait --wait-timeout 120 nakama
python3 dev/console-smoke.py "$NAKAMA_DATABASE_BACKEND"
