# Backend coverage audit

Local evidence: 2026-09-27, Nakama v3.40.0 fork, MySQL 8.4.11 / PostgreSQL 16.15.
This page preserves the historical execution inventory. Current contract results,
including the three-engine matrix, are in [MYSQL_PARITY.md](MYSQL_PARITY.md).
Execution percentages alone never establish feature or per-adapter parity.

## Reproducible inventory

`bash dev/parity.sh all` produces a `test-inventory.json` and a
`function-inventory.csv` per engine under `.parity-results/<run-id>/`. The CSV has
source file, line, function, surface, statement execution percentage and a separate
`contract_signoff` column. All sign-offs currently remain `pending`. `coverage.html`
shows executed/unexecuted lines. The raw coverage profile is retained; only synthetic
yacc source locations are excluded from the readable view, recorded in each summary
and `coverage-excluded.txt`. Parser tests are still executed.

The full race run `20260928014109-94834` inventories 5,556 production functions per
engine. This includes support code, helpers and both database-specific implementations,
so its total and percentages cannot measure progress toward database parity.
The table below counts functions with **any** executed statements; it includes
constructors/helpers and does not mean a public API request exercised every function.

| Source family | Functions inventoried | MySQL: any execution | PostgreSQL: any execution |
| --- | ---: | ---: | ---: |
| Client API | 95 | 14 | 14 |
| Console | 155 | 90 | 61 |
| Realtime pipeline | 30 | 2 | 1 |
| Go runtime module | 147 | 10 | 8 |
| Lua runtime module | 227 | 46 | 46 |
| JavaScript runtime module | 213 | 181 | 181 |
| Scheduler | 12 | 12 | 12 |
| Rank cache | 12 | 11 | 11 |
| Storage index | 12 | 11 | 11 |

JavaScript factories return closures: registration can execute the factory without
invoking the returned function. Its high touched-function count is particularly
unsuitable as a contract-coverage score. Also, earlier external Docker HTTP/console/
WebSocket/Lua smoke runs are separate evidence; this instrumented Go suite does not
include their execution. A zero here is a gap in this suite, not proof a feature
has never worked or lacks a database implementation.

## How to interpret and extend the inventory

1. Map each public HTTP/gRPC, realtime, console and Go/Lua/JavaScript entry point to
   its core operation and exact scenarios. Keep helper/factory execution separate.
   Prioritize database-dependent adapters currently reached only through core tests.
2. Run matching scripts/requests against both engines and compare normalized public
   results, error codes, permissions, cursors and persisted side effects. Cover
   malformed inputs and Unicode/JSON/numeric/time boundaries as well as happy paths.
3. Keep each regression under the same assertions on both engines. The documented
   native-only contracts have now been promoted; see MYSQL_CONTRACTS.md. A new
   engine-specific server skip must not silently reintroduce a parity gap.
4. Complete restart/failure/ambiguous-commit/cache-ordering and migration/backup/TLS
   verification, then isolated OCI checks. Real external-provider/device validation
   remains visibly separate from local fixture evidence.

Production adoption follows backend sign-off. Deployment capacity
remains deferred as requested. HeatWave acceleration stays disabled.

## Sign-off review follow-up

The wallet/chat/combined-update pass adds actual Go/Lua/JavaScript wallet calls,
shared native-regression scenarios and deterministic real-database connection-fault
checks. The former MySQL-only wallet bounds/deletion, chat scope/deletion and
MultiUpdate final-index/owner-lock cases now execute on PostgreSQL as well. Those 38 native-only skip entries belonged to that historical checkpoint. The
current skip inventory retains no PostgreSQL-only server feature exclusions; only
the engine-specific MySQL migration test remains. The added Community MySQL 26.7.0
suite also passes; it matches the OCI endpoint's reported release line, not its
cloud configuration. Exact final results and race scope are in the parity review;
per-surface evidence is in [MYSQL_CONTRACTS.md](MYSQL_CONTRACTS.md).

The older function-count table above is retained as the original inventory baseline.
Use each new run's generated CSV for its actual source execution. No blanket function
sign-off or completion percentage is inferred from these additional passing tests.
