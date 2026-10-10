#!/usr/bin/env bash
set -euo pipefail
case "${TEST_DB_BACKEND:-}" in
  mysql)
    address="nakama:$PARITY_DB_PASSWORD@mysql:3306/nakama?tls=false"
    export TEST_DB_URL="$address"
    export TEST_MYSQL_ADMIN_URL="root:$PARITY_ADMIN_PASSWORD@mysql:3306/mysql?tls=false"
    ;;
  *) echo 'Unsupported test backend' >&2; exit 2 ;;
esac
stage() {
  local label="$1" started=$SECONDS status=0
  shift
  echo "[stage] START $label"
  "$@" || status=$?
  echo "[stage] END $label elapsed=$((SECONDS-started))s exit=$status"
  printf '%s\t%s\t%s\n' "$label" "$((SECONDS-started))" "$status" >> /reports/stages.tsv
  return "$status"
}
log_to() {
  local logfile="$1"
  shift
  "$@" > "$logfile" 2>&1
}
# Fresh disposable database only: some upstream fixtures delete whole tables.
# Migration output stays in the artifact; never echo the credential-bearing command.
stage migrate log_to /reports/migration.log go run . migrate up --database.backend "$TEST_DB_BACKEND" --database.address "$address"
stage release-contract log_to /reports/release-contract.log bash dev/mysql-release-check.sh
stage vet log_to /reports/vet.log go vet ./...
mkdir -p internal/gopher-lua/_lua5.1-tests/libs/P1
race_args=()
case "${PARITY_RACE:-true}" in
  true) race_args=(-race) ;;
  false) ;;
  *) echo 'PARITY_RACE must be true or false' >&2; exit 2 ;;
esac
stage package-inventory log_to /reports/expected-packages.txt go list ./...
# Run every package exactly once. Isolate the CPU-heavy upstream stress package
# from server/Lua contention, with a separate 30m allowance; all others keep 15m.
# No test-name filters, disabled vet, skipped stress cases or test-result caching.
stress_package="github.com/heroiclabs/nakama/v3/internal/skiplist"
packages=()
stress_found=false
while IFS= read -r package; do
  if [[ "$package" == "$stress_package" ]]; then
    stress_found=true
  else
    packages+=("$package")
  fi
done < /reports/expected-packages.txt
if [[ "$stress_found" != true || "${#packages[@]}" -eq 0 ]]; then
  echo 'Incomplete package inventory' >&2
  exit 1
fi
run_tests() {
  local profile="$1" timeout="$2"
  shift 2
  go test "${race_args[@]}" -covermode=atomic -coverprofile="$profile" -json -count=1 -timeout="$timeout" "$@" 2>&1 | tee -a /reports/tests.jsonl
}
: > /reports/tests.jsonl
status=0
stage package-suite run_tests /reports/coverage-suite.out 15m "${packages[@]}" || status=1
stage skiplist-stress run_tests /reports/coverage-stress.out 30m "$stress_package" || status=1
# Disjoint package sets: retain both raw profiles and combine their statement rows.
if [[ -s /reports/coverage-suite.out && -s /reports/coverage-stress.out ]]; then
  { head -n 1 /reports/coverage-suite.out; tail -n +2 /reports/coverage-suite.out; tail -n +2 /reports/coverage-stress.out; } > /reports/coverage.out
  stage coverage bash dev/coverage.sh /reports || status=1
else
  echo 'Coverage profile missing' >&2
  status=1
fi
exit "$status"
