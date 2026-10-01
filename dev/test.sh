#!/usr/bin/env bash
# Local-only database contracts. Requires the selected database service(s) in the dev stack.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${1:-all}" in
  all) backends="mysql" ;;
  mysql) backends="$1" ;;
  *) echo "Usage: bash dev/test.sh [all|mysql]" >&2; exit 2 ;;
esac
for backend in $backends; do
  echo "Testing $backend"
  docker run --rm --network nakama-mysql-dev_default --env-file .env \
    -v "$PWD:/src" -w /src \
    -v nakama-mysql-build-cache:/root/.cache/go-build \
    golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd \
    sh -ec '
      go test -mod=vendor ./console/acl
      export TEST_DB_BACKEND="$1"
      export TEST_DB_URL="nakama:$MYSQL_PASSWORD@mysql:3306/nakama?tls=false"
      export TEST_MYSQL_ADMIN_URL="root:$MYSQL_ROOT_PASSWORD@mysql:3306/mysql?tls=false"
      go test -mod=vendor ./migrate -run TestMySQL -count=1
      go test -mod=vendor ./server -run "^(TestStorage(Write|Fetch|Remove|List|Overrwrite|ReadObjects)|TestNonOCC|TestOCC|TestDualBackend|TestMySQL|TestLocalStorageIndex|TestLocalLeaderboardRankCache|TestTournament)" -count=1
    ' -- "$backend"
done
