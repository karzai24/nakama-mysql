#!/usr/bin/env bash
# Temporarily mount the RPC fixture into the already-built local source image.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-mysql}" in
  mysql) export NAKAMA_DATABASE_BACKEND="${1:-mysql}" ;;
  *) echo "Usage: bash dev/wallet.sh [mysql]" >&2; exit 2 ;;
esac
restore() {
  docker compose -f compose.dev.yml up -d --no-deps --wait --wait-timeout 120 nakama
}
trap restore EXIT
docker compose -f compose.dev.yml -f dev/compose.wallet-test.yml up -d --no-deps --wait --wait-timeout 120 nakama
python3 dev/wallet-smoke.py
