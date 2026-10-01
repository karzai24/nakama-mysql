# Contract sign-off evidence

This inventory identifies tested built-in database contracts and their practical limits.
It does not certify every external service or every possible deployment.
The complete upstream/fork suite, exact skips and execution reports remain in
[MYSQL_PARITY.md](MYSQL_PARITY.md) and [MYSQL_COVERAGE.md](MYSQL_COVERAGE.md).

## Wallet, chat and combined-update review

The `TestDualBackend...` names reflect the original dual-backend development
fixtures. This standalone branch runs those retained assertions against MySQL only. Generated IDs/time values are checked relationally or against persisted
rows. Runtime casing and language number representations remain their existing
public API contracts; Go, Lua and JavaScript do not have identical representations.

| Entry points/surface | Shared evidence | Boundaries still open |
| --- | --- | --- |
| `UpdateWallets`, Go `WalletUpdate`/`WalletsUpdate` | Lifecycle, atomic batch, bounds/snapshots, concurrent debit/credit, missing-user behavior; `TestDualBackendWallet...` | Real OCI version/settings; production contention deferred |
| Go/Lua/JavaScript wallet update, batch, ledger list/update | `TestDualBackendWalletRuntimeContracts`: actual binding calls, repeated-user results, Unicode metadata, original changeset, merged metadata, pagination, invalid input, insufficient funds and zero/invalid limits | Language precision beyond exact integer range is not repaired by a database driver |
| `UpdateWalletLedger`, `ListWalletLedger` | ID/owner/metadata result fields, shallow merge with null, exact integer storage, invalid-merge rollback, time-tied next/previous pages and filter binding; owner-deletion race | Persisted arbitrary JSON numeric spelling/locale boundaries are not exhaustive |
| Console `GetWalletLedger` / `DeleteWalletLedger` | `TestDualBackendWalletLifecycle`: list, wrong-owner no-op, owner-scoped removal, balance unchanged | Broader console auth/ACL coverage remains separate |
| `ChannelMessageSend` / `Update` / `Remove`, history | Lifecycle, permissions, next/previous/cacheable cursors, full channel scope, uppercase/invalid IDs, empty runtime sender, ephemeral/closed-pool behavior, concurrent mutation/removal/account deletion | Direct core and representative Go runtime coverage; not every Lua/JS/realtime adapter error path |
| Console channel list/delete | `TestDualBackendConsoleChatDeletionAtomic` validates malformed IDs/times before changes and forces rollback after cutoff deletion | Offline retention operations may still be expensive |
| `MultiUpdate`, Go runtime combined update | All phase failures roll back; canonical cross-owner locking; final index state; `TestDualBackendMultiUpdate...` | Lua/JS combined-update wrappers, post-commit cache ordering/crash recovery remain open |
| PostgreSQL storage index acknowledgement pairing | `TestDualBackendStorageIndexAcknowledgementOrder`: write order differs from SQL sort; object/value/version matches persisted row on both engines | Does not prove ordering between independent concurrent transactions |
| Transaction connection interruption | `TestDualBackendConnectionLossRollback`: terminate only the fixture connection before commit, verify rollback/no replay/pool recovery | Does not certify server/node failover |
| Ambiguous commit | `TestDualBackendLostCommitReply`: consume real successful COMMIT reply, close before driver sees it, verify one persisted credit/ledger, no success result, next request succeeds | No application-level idempotency key is added; callers must not blindly retry unknown outcomes |

Fault cases are restricted to `TEST_DB_FAULTS=true`, set by the isolated Compose
runner. They do not target retained development databases or OCI. Final verification on MySQL 8.4.11, MySQL 26.7.0 and PostgreSQL 16.15 is recorded
in the parity review, including which runs used race detection and exact skips.

## Identity, social, billing, ranking and console contracts

The entry names below denote public core/console operations; client HTTP/gRPC and
runtime wrappers delegate to them. Actual wrapper fixtures are listed separately,
so a core test is never presented as an independently executed language/API route.

| Public operation family | Same assertions on both engines | Concrete limits |
| --- | --- | --- |
| Authenticate/link/unlink custom, device, email, providers | `TestDualBackend...` identity/provider suites: racing create/passwords, ownership collisions, bans, optional email collisions, rollback/remap and last usable login | Provider cryptographic/HTTP fixtures are local; real credentials/devices remain separate |
| Accounts, lookup, moderation, delete | Account lifecycle/update/delete suites: every group role, counter repair, receipt retention, deletion races, rollback, index/session cleanup | Post-commit process failure and distributed invalidation are shared operational limits |
| Facebook/Steam friendship import/reset | Provider friendship and friend import suites: paging, private lists, duplicate input, blocks, complete rollback, notification effects | Fetch/import remains best effort at its existing callers |
| Friend add/delete/block/metadata/list/FoF | Friend suites: reciprocal races, block/deletion races, counters, ordered cross-referrer cursors, JSONB-compatible merge | No durable outbox or global notification delivery order |
| Group CRUD/join/leave/add/kick/ban/promote/demote | Group suites plus `GroupMembershipRaces`, `GroupLeaveLastAdminRace`, `GroupJoinRollback`, `GroupDisabledMembership`, `GroupRejectMissingOwner` | Join checks capacity before treating an existing edge as a no-op, preserving the shared contract |
| Console group role/request changes | `ConsoleGroupRoleRaces`, `ConsoleGroupAcceptCapacityRace`: last-admin, banned edge and capacity protection | Group notifications keep best-effort delivery |
| Leaderboard/tournament definition, score, join, attempt, delete, haystack, reset/end | Leaderboard/tournament suites: overflow, concurrency, capacity, attempt exhaustion, rollback, persisted definition winner, period isolation, rank disabling, previous-page traversal | Hooks have no exactly-once delivery guarantee; virtual owners are supported |
| Purchase/subscription persistence and lookup | Billing suites: ownership/store conflicts, tombstones, monotonic refunds, atomic batches, duplicate validation, deterministic selection and filter-bound pagination | Real stores and delayed event ordering remain external/shared limits |
| Console admin/authentication/MFA/reset/ACL | Console user/MFA/admin race/root suites: durable revisions, restart cache independence, single-use links/codes, root protection | Individual logout is memory-only; root Basic auth remains upstream behavior |
| Console accounts/import/notes/search | `ConsoleAccountManagement`, `ConsoleFinalIdentityGuard`, `ConsoleReplaceIdentity`, `ConsoleImportValidation`, `ConsoleImportRollback`, `ConsoleAccountSearch` | Account imports do not restore every exported relationship; native text/time limits apply |
| Console audit/notification/billing history | `ConsoleAuditPaging`, `ConsoleNotificationsAndBilling`: zero enum filters, owner/date/cursor binding, ties, next/previous pages | No database-wide snapshot across separate requests |
| Console DeleteStorage/DeleteAllData/DeleteAccounts | `ConsoleDestructiveMaintenance`: fresh dedicated database, forced rollback, system user, cache effects, receipt retention | Quiesced traffic required; deprecated DeleteAccounts commits each account separately |
| Transaction helpers | Retry exhaustion, ambiguous completion, real connection loss and lost-COMMIT-reply fixtures | Unknown outcome is returned to caller, not automatically replayed |
| MySQL migrations | `TestMySQLMigrations`: fresh/upgrade/down/redo, populated baseline, stable key, real DDL failure, dirty/checksum refusal and fixture repair | PostgreSQL uses its separate migration files; tests never roll back retained application databases |

The complete matrix runs every upstream/fork package, not just these test names.
Live `dev/smoke.py`, `dev/realtime.sh`, `dev/wallet.sh`, `dev/rank.sh` and
`dev/console.sh` exercise HTTP, WebSocket, Lua RPC, console ACLs and restart behavior.
Console JWT revocation, single-use resets, audit cursors and exhausted tournament
writes now use the same live assertions on both engines.

## Interpretation

No database-dependent feature family is deliberately excluded from the port.
Raw custom SQL remains engine-specific. The explicit skip inventory contains no
server feature regressions skipped only on PostgreSQL. That is stronger evidence
than the earlier checkpoint, but it is not exhaustive proof for every exported
adapter, Unicode collation, provider response or concurrent crash interleaving.
Use the generated function inventory to locate unexecuted paths without assigning
unearned per-function sign-off from a factory/helper being executed.

Verified local results and the precise remaining OCI, provider, operational and
production-load gates belong in [MYSQL_PARITY.md](MYSQL_PARITY.md). Deployment and managed-service validation are separate gates.
