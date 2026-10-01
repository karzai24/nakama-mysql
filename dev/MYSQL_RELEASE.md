# Standalone MySQL release ledger

Branch: `release/nakama-3.40.0-mysql-26.7.0`. Tag: `nakama-3.40.0-mysql-26.7.0`.
Upstream baseline: Nakama 3.40.0. MySQL 26.7.0 and 8.4.11 are tested.

## Scope

Native MySQL persistence, migrations, checksums and retries replace the upstream
PostgreSQL/CockroachDB implementations. Those drivers, migration SQL and development
database containers are absent. Standard Nakama APIs and local session behavior
remain; no game-specific APIs or application deployment dependencies are included.
Custom runtime SQL must use MySQL syntax and placeholders.

## Validation

Verified 2026-10-01 UTC (2026-09-30 local) against this standalone working tree:

| Target | Top-level passed | Subtests passed | Documented skips | Report |
| --- | ---: | ---: | ---: | --- |
| MySQL 8.4.11 | 367 | 189 | 2 | `.parity-results/20261001024744-36317/mysql` |
| MySQL 26.7.0 | 367 | 189 | 2 | `.parity-results/20261001024745-36332/mysql` |

Both passed migration, dependency/binary/backend-rejection checks, full vet,
all-package race/coverage and all 20 seeded skiplist stress cases. Only inherited
Satori tests requiring external credentials skipped; no failures or unexpected skips.
The standalone runtime-surface and environment tests passed on both targets.
Eight reporting-tool failure-path tests passed.

The source-built standalone image passed startup/health, HTTP account/social/storage,
realtime status/chat/parties/matchmaking/account-deletion disconnect, Lua wallet/ledger/
MultiUpdate, ranking persistence/restart, and console MFA/ACL/audit/import/export/
restart-revocation smoke. Scoped live fixtures were removed. The retained experimental
backend warning points to this adoption contract; no unexplained server errors were
observed. Local development containers were stopped after verification without
removing retained database volumes.

The source validation preceded publication. Public packaging changes only documentation
and workflow routing; the tested server source, schema and dependency tree are unchanged.
The release workflow repeats both database suites and produces a downloadable
candidate image without publishing a container or deploying a service.
External provider credentials, custom runtime modules, managed-service settings
and deployment capacity remain separate adoption gates.

## Updating

Integrate each exact upstream release tag on a separate branch, port new persistence
paths to MySQL, and repeat both database matrices plus live smoke before tagging.
