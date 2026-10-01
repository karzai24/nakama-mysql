# Nakama 3.41.0 MySQL integration

- Prior verified edition: `nakama-3.40.0-mysql-26.7.0`.
- Upstream input: exact tag `v3.41.0`, commit `cab8af584947ff0d7ebee91d6c33e67d87fd3e47`.
- Integration branch: `release/nakama-3.41.0-mysql-26.7.0`; separate worktree. No changes to production or application release branches.
- Upstream master has additional commits after this tag; they are not included in this release integration.

## Integration decisions

Import upstream runtime/API/protobuf/common-library, console UI/ACL, Google-audience,
FleetManager namespace, WebSocket headers, tournament scheduling and paging changes.
Port all new SQL to native MySQL. Formatting/refactoring of PostgreSQL-only paths
is not reintroduced. Use Go 1.27.1 and regenerated pinned vendored dependencies;
retain the existing Bluge document-reset fix. No PostgreSQL driver or SQL migration
is reintroduced. The upstream docs-dispatch workflow is omitted: a fork release
must not dispatch to Heroic Labs documentation infrastructure.

New registered providers are stored in the existing user_device table with an
additive provider column. Preserve the upstream globally unique identity ID key,
keep device identities in the empty namespace, and prevent provider IDs from
being accepted as device logins. Native account reads/imports/exports split device
and provider identities; provider friend import reuses transactional locks and
post-commit notifications. Account linking/unlinking uses the existing account
lock boundary and refuses removal of the last usable login identifier.

Applied migrations 0001 and 0002 are unchanged. 0003 adds only the provider column;
existing devices survive with provider=''. Migration tests cover populated upgrade,
checksums, dirty-state/DDL failure, redo and down paths. Old binaries reject the
new schema. A down migration removes provider information and is not an operational
rollback plan after adopting the feature.

Storage version rejection adopts code 9 FAILED_PRECONDITION; permission rejection
remains code 3 INVALID_ARGUMENT. The retained native console reset implementation
now authorizes caller versus target ACL under its existing row lock while retaining
revision-based token revocation and configured-root protection. Adapted upstream
mock-driver regressions exercise MySQL isolation and deadlock retries.

## Validation

The initial full run exposed five Google-provider fixture failures on each engine.
The synthetic token used an audience different from the configured OAuth client;
3.41 correctly rejected it under the new audience validation. The fixture now
uses the configured client ID for both aud and azp. Focused provider lifecycle,
proof rejection, profile import, race and email-collision cases pass with the
corrected fixture, alongside actual Go/Lua/JavaScript provider adapters on 26.7.
No authentication validation was weakened.

The source-built 3.41 image migrated the retained local 3.40 development database
and passed HTTP, realtime, wallet, ranking and console smoke checks. Production
was not accessed or modified. The retained local development volume is now at
migration 0003; the old local binary cannot reuse it without an explicit downgrade.

The local image has no Google OAuth client configured, so its new upstream
warning about skipped audience restrictions is expected in this test setup.
Configure google_auth.client_ids or credentials_json before offering Google
authentication in a deployment. Signed external-provider fixtures exercise the
configured-client path; these tests do not claim real external-provider credentials.

Clean full-package race/coverage suites, vet, migrations and release-contract checks
passed on MySQL 8.4.11 and 26.7.0. Both include the new-provider regressions and
actual Go/Lua/JavaScript adapters. All 20 seeded skiplist stress cases and final coverage/report aggregation also
passed. Each engine reports 382 top-level tests and 218 subtests, zero failures,
zero unexpected skips, and only the two documented external Satori credential
skips. Both complete runners exited 0.

Final report directories:
- MySQL 8.4: `.parity-results/20261001055948-40195/mysql`.
- MySQL 26.7: `.parity-results/20261001060009-40251/mysql`.

This is local integration evidence, not CI, public publication or production
deployment evidence. The public snapshot preserves the tested code and removes private development history.

Local development containers were stopped after verification, retaining their
upgraded database volume. Source publication uses the release branch and tag; no registry publication or
production deployment is included.
