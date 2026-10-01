#!/usr/bin/env bash
# Keep the raw profile. The generated yacc parser maps unrelated Go blocks to
# synthetic yaccpar/yacctab/parser.go.y locations, which go tool cover cannot
# merge (conflicting NumStmt) or open. Exclude only those locations from the
# human-readable view; no tests or executable source are excluded from the run.
set -euo pipefail
reports="${1:?Usage: bash dev/coverage.sh REPORT_DIRECTORY}"
pattern='^github[.]com/heroiclabs/nakama/v3/internal/gopher-lua/parse/(yaccpar|yacctab|parser[.]go[.]y):'
awk -v pattern="$pattern" 'NR == 1 || $1 !~ pattern' "$reports/coverage.out" > "$reports/coverage-view.out"
awk -v pattern="$pattern" '$1 ~ pattern { sub(/:.*/, "", $1); print $1 }' "$reports/coverage.out" | sort -u > "$reports/coverage-excluded.txt"
go tool cover -func="$reports/coverage-view.out" > "$reports/functions.txt"
go tool cover -html="$reports/coverage-view.out" -o "$reports/coverage.html"
