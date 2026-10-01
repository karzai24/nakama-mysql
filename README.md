# nakama-mysql — standalone MySQL edition

An **unofficial MySQL port of Heroic Labs Nakama 3.41.0**, intended for developers
who can build, configure, test and operate their own game backend.

**Release candidate, self-managed, no support commitment.** Read the compatibility
and migration sections before adopting it. This is **not a drop-in replacement**
for an existing Nakama/PostgreSQL deployment, and it does not convert your database.

- Standalone source: **`release/nakama-3.41.0-mysql-26.7.0`**. Clone that branch explicitly.
- Database: **MySQL only**; `database.backend=mysql` is the default and only accepted value.
- Verified engines: **MySQL 8.4.11 and Community MySQL 26.7.0**.
- Source toolchain: **Go 1.27.1**; dependencies are vendored.
- License: **Apache 2.0**, with upstream and dependency notices retained.
- No paid support, installation assistance, deployment service, or response-time guarantee.

## What this edition provides

Native MySQL persistence for built-in Nakama identity/accounts, storage, notifications,
friends, groups, chat history, wallets/ledgers, leaderboards/tournaments,
purchase/subscription records, and console permissions/MFA/audit operations.
Standard HTTP/gRPC, realtime sockets, and Go/Lua/JavaScript runtime interfaces remain.

It includes no application game modules, custom game physics, shared database-backed
session cache, application process-generation injection, reserved application
collections, or custom create-once match API. Sessions, presence and active matches
use standard local Nakama behavior. **Sharing MySQL or adding a load balancer does
not provide cross-node coordination, Enterprise clustering, or recovery of active
matches after a process fails.** Design and validate multi-instance routing separately.

The port originated from a hosting constraint: a managed MySQL service allowed a
small deployment to avoid maintaining a separate database VM. It is usable without
Oracle Cloud. This does not imply PostgreSQL is unreliable or that free hosting is
permanent, undeletable, or covered by a service guarantee.

Published source tag: `nakama-3.41.0-mysql-26.7.0`. The MySQL suffix identifies the tested 26.7.0 target; MySQL 8.4.11 is also tested. It is not an embedded database or a MySQL-only compatibility restriction.

## Compatibility and adoption boundaries

| Surface | Contract |
| --- | --- |
| Nakama baseline | Derived from 3.41.0; do not assume compatibility with a different upstream release. |
| MySQL | The two versions above are tested. MariaDB, MySQL 5.7, other versions, proxy services and managed-service configurations are not certified by those results. |
| PostgreSQL / CockroachDB | Drivers, backend implementations and migration SQL are removed from this edition. |
| Client SDKs | Standard Nakama interfaces remain; validate your SDK and application flows against the chosen revision. No all-SDK compatibility claim. |
| Custom runtime SQL | Must use MySQL syntax and `?` placeholders. PostgreSQL casts, operators and `$1` placeholders are not translated. |
| Existing data | No automatic PostgreSQL-to-MySQL converter or migration tool is supplied. |
| Third-party integrations | Local contract fixtures do not certify real social providers, payment stores, credentials or devices. |
| Production readiness | Requires your workload, recovery, backup/restore, security and capacity validation. Passing this suite is not a production SLA. |

Use [the contract inventory](dev/MYSQL_CONTRACTS.md) for evidence and remaining
boundaries, [verification scope](dev/MYSQL_PARITY.md) for test interpretation, and
[the removal ledger](dev/MYSQL_RELEASE.md) for the standalone edition's exact scope.

## Quick start: isolated local development

Prerequisites: Git, Docker Engine with **Docker Compose v2**, Python 3, sufficient
Docker memory/disk, and network access to fetch base images. Native host Go is not
needed for the container workflow. The examples assume a POSIX shell.
This starts a source-built server and a local database; it is not a production setup.

```sh
git clone --branch release/nakama-3.41.0-mysql-26.7.0 https://github.com/karzai24/nakama-mysql.git
cd nakama-mysql
python3 dev/setup.py
docker compose -f compose.dev.yml up --build -d --wait --wait-timeout 300
docker compose -f compose.dev.yml ps
```

`dev/setup.py` creates a private, ignored `.env` with random development credentials
and preserves an existing file. Do not publish that file, copy it into a client,
or reuse its credentials in production. A first build can take several minutes.

| Interface | Local address / credentials |
| --- | --- |
| HTTP / WebSocket API | `http://127.0.0.1:7350`; API server key is `NAKAMA_SERVER_KEY` in `.env`. |
| gRPC | `127.0.0.1:7349` |
| Admin console | `http://127.0.0.1:7351`; username **`localadmin`**, password is `NAKAMA_CONSOLE_PASSWORD` in `.env`. |
| MySQL | `127.0.0.1:3307`; database/user **`nakama`**, password is `MYSQL_PASSWORD` in `.env`. |

All published development ports bind to loopback. Compose runs `migrate up` before
starting Nakama and retains database data in a volume. Changing `.env` does not
change passwords already initialized inside an existing database volume.

Check the API and inspect local logs:

```sh
python3 dev/smoke.py
docker compose -f compose.dev.yml logs --tail=100 nakama
```

Review logs privately before sharing them; configurations and diagnostics may
contain sensitive information. The smoke script creates and removes scoped test
accounts. Additional realtime, wallet, ranking and console probes are listed in
[the development guide](dev/README.md).

Stop the stack while retaining its database:

```sh
docker compose -f compose.dev.yml down
```

**`down -v` deletes the local database volume.** Use it only when you intend to
reset disposable development data. Never copy that command into a production runbook.

## Build a binary or container

With Go 1.27.1 and a working native build toolchain:

```sh
go build -trimpath -mod=vendor -o nakama .
./nakama --version
```

Build the standalone image from the checked-out source revision:

```sh
docker build \
  --build-arg VERSION=3.41.0-mysql \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -f build/Dockerfile -t nakama-mysql:local .
docker run --rm nakama-mysql:local --version
```

The container expects configuration and database access; this build command does
not deploy a service. Do not substitute a stock `heroiclabs/nakama` image for this
binary and expect MySQL support. The build uses CGO; cross-platform Go plugins and
multi-architecture images need their own matching toolchain and verification.

CI runs both database suites before building a **downloadable candidate image**.
The `nakama-mysql-candidate` artifact contains `image.tar.gz`, `image-id.txt` and
`source-commit.txt`; artifacts expire after seven days. After extracting the artifact:

```sh
gzip -dc image.tar.gz | docker load
```

The image tag is `nakama-mysql:<source-commit>`. Check the workflow's source commit
and image architecture before using it. This workflow **does not publish to a
container registry or deploy production**. No public registry image is promised.

## Database configuration and migrations

Exactly **one** database address is supported. The accepted address is a URL:

```text
mysql://USER:URL_ENCODED_PASSWORD@DB_HOST:3306/nakama?tls=true
```

The `mysql://` prefix is optional. Percent-encode reserved characters in URL
credentials. This is not the Go driver's `user:password@tcp(host:port)/database`
DSN format. Only one `tls` query parameter is allowed, with value `true` or `false`.
Other driver parameters are rejected. UTC, strict SQL mode, UTF-8 and other
correctness-critical session settings are configured by the server.

TLS is **verified by default** when the parameter is omitted. `tls=true` requires
a trusted certificate chain and a certificate valid for the database hostname.
Ensure the binary/container's trust store contains the necessary CA. This edition
has no connection-URL switch for skipping verification or supplying a custom CA
file. `tls=false` is used only by the isolated local-development examples.
Database TLS and public API TLS are separate configurations.

Store a real configuration outside source control, protect its permissions, and
use distinct production credentials. An illustrative YAML configuration is:

```yaml
database:
  backend: mysql
  address:
    - "mysql://USER:URL_ENCODED_PASSWORD@DB_HOST:3306/nakama?tls=true"
```

Replace the placeholders. This fragment covers the database only; configure your
server/session/refresh/console keys and the rest of Nakama for your deployment.
Use a private configuration file rather than putting real passwords in shell history:

```sh
./nakama migrate status --config /absolute/path/to/private-config.yml
./nakama migrate up --config /absolute/path/to/private-config.yml
./nakama --config /absolute/path/to/private-config.yml
```

Migration requirements:

- The initial migration requires an **empty dedicated database**; it does not adopt
  an existing PostgreSQL dump or unrelated MySQL schema.
- When binary logging is enabled, `log_bin_trust_function_creators=ON` is required.
  Managed-service administrators must configure that setting where supported.
- Migration credentials need the schema/trigger privileges required by the SQL in
  `migrate/mysql/`. Separate deployment-time migration access from runtime access
  according to your environment; no universal production grant recipe is supplied.
- Startup checks migration versions, checksums and dirty state. Run the matching
  migration before startup; do not edit applied migration files or clear dirty flags
  to bypass a failed migration. MySQL DDL can commit independently of transactions.
- Back up and test restoration before schema updates. Investigate partial migrations
  and recover deliberately; `migrate down` is not a general rollback strategy.

## Moving an existing PostgreSQL deployment

There is no safe one-command cutover. You own schema/data conversion and validation:

1. Inventory built-in data, custom storage, runtime SQL and external integrations.
2. Prepare an isolated MySQL database using this revision's migrations.
3. Convert data with explicit handling of UUIDs, JSON, timestamps, ownership,
   versions, relationships, collation and ordering. PostgreSQL dumps cannot be
   imported directly into this schema.
4. Test application flows and compare the converted data before accepting traffic.
5. Plan write quiescence/final synchronization, backup/restore, cutover and rollback.

Do not point a production service at a fresh database and expect old accounts or
storage to appear. A working local demo does not verify your migration.

## Tests and release evidence

Run the complete suite against disposable databases:

```sh
# Pinned MySQL 8.4 target.
bash dev/parity.sh mysql

# Pinned MySQL 26.7 target.
PARITY_MYSQL_IMAGE=mysql:26.7.0@sha256:ade067ae2fb15eeb6143e81185afe22573e3b3addc58aaf90c41999fa6883991 bash dev/parity.sh mysql
```

The runner applies migrations, checks the binary/dependencies/backend rejection,
runs full `go vet`, every package with race detection and coverage, and all 20
seeded skiplist stress cases. It fails on unexpected skips, missing/incomplete
packages, failed tests and timeouts. These are substantial suites, not instant
health checks. Reports stay in ignored `.parity-results/` and CI artifacts.
Test stacks use isolated credentials and disposable databases.

The earlier standalone 3.40 candidate passed both engines and source-built live
smoke tests. The 3.41 integration is tracked separately in [the upgrade report](dev/UPGRADE_341.md);
The 3.41 edition passed 382 top-level tests, 218 subtests and all 20 seeded stress
cases on each engine, plus source-built live smoke checks. These results are local
validation; the public branch workflow runs the matrix independently in CI.

### Changes when upgrading from the 3.40 edition

- Run additive MySQL migration `0003_nakama_341_provider` before startup. Existing
  device identities remain in the empty provider namespace; provider identities
  appear separately on accounts. Applied 3.40 migration checksums are unchanged.
- Storage write version conflicts now return gRPC `FAILED_PRECONDITION` (code 9);
  permission rejection remains `INVALID_ARGUMENT`. HTTP mapping remains 400.
- Rebuild Go runtime plugins using Go 1.27.1 and the matching dependencies.
- Configure `google_auth.client_ids` or appropriate Google credentials to restrict
  accepted OAuth audiences; upstream now warns when audience checks are unconfigured.
- Console ACL password-reset checks and storage-index APIs follow the upstream
  3.41 contracts. Active games still remain process-local.

A 3.40 binary rejects the newer migration history. Plan restoration/rollback before
upgrading; dropping the provider column is not a safe general-purpose downgrade
once provider identities have been stored.

## Troubleshooting

| Symptom | Check / action |
| --- | --- |
| Clone cannot access the repository | Repository visibility and your GitHub access. This public fork is karzai24/nakama-mysql. |
| Compose reports missing variables | Run `python3 dev/setup.py` in the repository root, then use `compose.dev.yml`. |
| Database access denied after editing `.env` | Existing volumes retain old database passwords. Restore matching credentials or deliberately reset disposable data; do not delete real data. |
| Host port already allocated | Inspect local port users and adjust only the Compose host-side mapping. Smoke tools use the documented default ports. |
| Unsupported backend | Use this branch and `database.backend=mysql`; PostgreSQL/CockroachDB are intentionally unavailable. |
| Invalid database address or query parameter | Use the URL format above, percent-encoded credentials and only `tls=true`/`tls=false`. |
| TLS certificate/hostname error | Verify the trust store, certificate chain and database hostname. Do not bypass verification for production. |
| Initial migration refuses nonempty database | Create a dedicated empty database or perform a reviewed conversion. Existing tables are not imported automatically. |
| Migration requires function-creator setting | Configure `log_bin_trust_function_creators=ON` when binary logging is enabled; contact your service administrator. |
| Schema mismatch/checksum/dirty error | Use matching source/schema, inspect the failed migration and recover from validated backups. Do not alter checksums to force startup. |
| Experimental MySQL warning | Expected candidate-stage notice; read the release ledger and validate your deployment. It does not by itself mean startup failed. |
| Game SQL fails despite successful login | Audit custom SQL, placeholders, collation and assumptions inherited from PostgreSQL. |
| Games disappear after a server restart | Active matches are process-local; this edition does not persist or recover their state. |
| CI is green but an integration fails | Check whether that provider/device/module/load scenario is actually covered; database tests do not certify all integrations. |

## Support

**This repository is self-service.** Maintainers do not provide setup tutorials,
private-message/email support, hosting/account troubleshooting, database conversions,
or help debugging custom game code. Do not contact individual maintainers for those
requests. No response or fix schedule is guaranteed.

Use upstream Nakama documentation for standard server/runtime concepts, your database
provider for service configuration, and the files above for this fork's differences.
Upstream Heroic Labs support should not be expected to troubleshoot this unofficial port.

If you find a defect in this fork, [open an issue on our GitHub issue board](https://github.com/karzai24/nakama-mysql/issues).
Do not include credentials, private keys, tokens, player records or raw production
logs. Issues are not a support channel for installation or custom game code, and
no response or fix schedule is guaranteed.

## Upstream, updates and license

[Nakama](https://github.com/heroiclabs/nakama) is developed by **Heroic Labs**;
its [documentation](https://heroiclabs.com/docs/nakama/) describes the upstream product.
This port is independently maintained and is not endorsed or officially supported
by Heroic Labs. Updates are not automatic: new upstream releases require reviewed
changes and fresh verification before being adopted here.

The [Apache License 2.0](LICENSE), upstream notices and vendored dependency licenses
remain in the repository. Redistribution must preserve applicable notices.
