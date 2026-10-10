# MySQL verification scope

This standalone branch supports MySQL 8.4 and 26.7. The native port was developed
against shared PostgreSQL/MySQL contract fixtures; PostgreSQL implementation and
application extensions are removed here. Historical results on earlier branches
must not be presented as results for this edition.

The complete runner executes schema migration, full vet, every package with race
and coverage, and all seeded skiplist stress cases. Only two inherited live Satori
checks are allowed to skip. Failures, unexpected skips, missing packages, incomplete
tests and timeouts fail the run. Reports live in `.parity-results/`.

See [release and removal ledger](MYSQL_RELEASE.md), [contract inventory](MYSQL_CONTRACTS.md)
and [coverage interpretation](MYSQL_COVERAGE.md). Source-built smoke scripts exercise
HTTP identity/social/storage/account flows, realtime chat/parties/matchmaking,
Lua wallet/ranking and console MFA/ACL/import/restart behavior.

Passing local tests does not certify live identity providers, store SDKs, all custom
runtime modules, managed-service settings, production load or crash interleavings.
Use verified database TLS, least-privilege credentials, backups and a reviewed data
migration when adopting the fork. MySQL collation behavior and custom raw SQL need
application-specific review. Active matches and presence remain process-local.
