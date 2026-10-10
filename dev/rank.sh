#!/usr/bin/env bash
# The chosen database must already be running; no volumes are removed.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-mysql}" in
  mysql) export NAKAMA_DATABASE_BACKEND="${1:-mysql}" ;;
  *) echo "Usage: bash dev/rank.sh [mysql]" >&2; exit 2 ;;
esac
restore() {
  docker compose -f compose.dev.yml up -d --no-deps --wait --wait-timeout 120 nakama
}
trap restore EXIT
docker compose -f compose.dev.yml -f dev/compose.rank-test.yml up -d --no-deps --wait --wait-timeout 120 nakama
python3 dev/rank-smoke.py
