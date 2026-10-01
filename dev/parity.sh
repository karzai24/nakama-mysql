#!/usr/bin/env bash
# Isolated full-package tests. Never attaches to compose.dev.yml or its databases.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-all}" in
  all) backends=(mysql) ;;
  mysql) backends=("$1") ;;
  *) echo 'Usage: bash dev/parity.sh [all|mysql]' >&2; exit 2 ;;
esac
export PARITY_RACE="${PARITY_RACE:-true}"
case "$PARITY_RACE" in true|false) ;; *) echo 'PARITY_RACE must be true or false' >&2; exit 2 ;; esac
run_id="$(date -u +%Y%m%d%H%M%S)-$$"
reports="$PWD/.parity-results/$run_id"
mkdir -p "$reports"
credentials="$(mktemp)"
chmod 600 "$credentials"
python3 - "$credentials" <<'PY'
import secrets,sys
from pathlib import Path
Path(sys.argv[1]).write_text('PARITY_DB_PASSWORD='+secrets.token_hex(24)+'\nPARITY_ADMIN_PASSWORD='+secrets.token_hex(24)+'\n')
PY
project=""
compose=()
cleanup_project() {
  if [[ -n "$project" ]]; then
    if "${compose[@]}" down --volumes --remove-orphans --rmi local > "$PARITY_REPORT_DIR/cleanup.log" 2>&1; then
      project=""
    else
      echo "Test stack cleanup failed; see $PARITY_REPORT_DIR/cleanup.log" >&2
      return 1
    fi
  fi
}
cleanup() {
  status=$?
  trap - EXIT
  cleanup_project || status=1
  rm -f "$credentials"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker volume create nakama-mysql-build-cache >/dev/null
failed=0
for backend in "${backends[@]}"; do
  export PARITY_BACKEND="$backend"
  export PARITY_REPORT_DIR="$reports/$backend"
  mkdir -p "$PARITY_REPORT_DIR"
  project="nakama-parity-$run_id-$backend"
  compose=(docker compose --env-file "$credentials" --project-name "$project" -f dev/compose.parity.yml --profile "$backend")
  echo "Full package suite: $backend (race=$PARITY_RACE)"
  if ! python3 dev/parity-live.py "$PARITY_REPORT_DIR/start.log" "${compose[@]}" up -d --wait --wait-timeout 180 "$backend"; then
    echo "Database startup failed; see $PARITY_REPORT_DIR/start.log" >&2
    failed=1
  elif ! python3 dev/parity-live.py "$PARITY_REPORT_DIR/build.log" "${compose[@]}" build test; then
    echo "Test image build failed; see $PARITY_REPORT_DIR/build.log" >&2
    failed=1
  else
    status=0
    python3 dev/parity-live.py "$PARITY_REPORT_DIR/runner.log" "${compose[@]}" run --rm --no-deps test || status=$?
    python3 dev/parity-report.py "$backend" "$PARITY_REPORT_DIR" "$status" || failed=1
  fi
  cleanup_project || { failed=1; break; }
done
echo "Reports: $reports"
exit "$failed"
