# Standalone MySQL development

Use Docker Engine with Compose and Python 3; direct builds require Go 1.27.1.

```sh
python3 dev/setup.py
docker compose -f compose.dev.yml up --build -d --wait --wait-timeout 300
python3 dev/smoke.py
bash dev/realtime.sh
bash dev/wallet.sh mysql
bash dev/rank.sh mysql
bash dev/console.sh mysql
```

Development credentials live in ignored `.env`; ports bind to loopback. Stop with
`docker compose -f compose.dev.yml down`; adding `-v` destroys the local database.

`bash dev/parity.sh mysql` creates a separate disposable database and runs schema
migration, dependency/binary checks, full vet, all packages with race detection,
coverage and all 20 seeded skiplist stress cases. It never uses production data.
`PARITY_MYSQL_IMAGE` selects the pinned 26.7 target in the CI workflow.
Reports are retained in `.parity-results/`; unexpected skips and incomplete suites fail.
The two documented Satori skips require real external credentials.

See [release ledger](MYSQL_RELEASE.md), [contracts](MYSQL_CONTRACTS.md),
[verification boundaries](MYSQL_PARITY.md), and [coverage interpretation](MYSQL_COVERAGE.md).
