#!/usr/bin/env bash
# Verify the built release has no PostgreSQL dependency or selectable backend.
set -euo pipefail
cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
GOFLAGS=-mod=vendor go list -deps ./... > "$work/dependencies"
if grep -Eq 'github.com/(jackc/|heroiclabs/sql-migrate)' "$work/dependencies"; then
  echo 'PostgreSQL dependency found in MySQL-only release' >&2
  exit 1
fi
go build -trimpath -mod=vendor -o "$work/nakama" .
"$work/nakama" --version
for backend in postgres cockroach unknown; do
  if "$work/nakama" migrate up --database.backend "$backend" > "$work/rejected" 2>&1; then
    echo "Unsupported backend accepted: $backend" >&2
    exit 1
  fi
  if ! grep -q 'supports only database.backend=mysql' "$work/rejected"; then
    echo "Unsupported backend did not fail before connecting: $backend" >&2
    exit 1
  fi
done
echo 'PASS MySQL-only dependency graph, binary build and unsupported-backend rejection'
