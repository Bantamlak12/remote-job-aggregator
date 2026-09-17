# remote-job-aggregator

Autonomous remote-job aggregation platform, built as a Go modular monolith.
Discovers, normalizes, filters, ranks, and stores remote job postings from
ATS providers, prioritizing geographic-eligibility correctness (with a
default focus on Ethiopia) and data quality over raw volume.

**Current status: Phase 1 — foundation.** Config, structured logging, the
PostgreSQL connection pool, and the migration system exist. There is no
discovery, ingestion, ATS integration, filtering, ranking, scheduling, or
notification code yet — those land in later phases.

## Requirements

- Go 1.27+
- Docker + Docker Compose (for PostgreSQL)

## Running locally

```bash
cp .env.example .env   # edit if needed
docker compose up -d db
export $(grep -v '^#' .env | xargs)   # or use a tool like direnv
make migrate-up
make run
```

`aggregator run` connects to the database and blocks until it receives
SIGINT/SIGTERM, at which point it closes the pool and exits. There is no
ingestion loop yet, so this currently does nothing beyond holding the
connection open.

## Commands

```
aggregator run                        # connect, then block until shutdown signal
aggregator migrate-up                 # apply pending migrations
aggregator migrate-down --yes         # roll back one migration (requires confirmation)
aggregator migrate-down --yes --all   # roll back EVERY migration, dropping all tables
aggregator migrate-force <version>    # clear a "dirty" schema_migrations state
                                       # after an interrupted migrate-up/-down, without
                                       # running any migration
```

`migrate-down` always requires `--yes`: rolling back even one migration is
a `DROP TABLE`-class operation (this repo currently has exactly one
migration, so a single step already drops every table — `--all` only
matters once there's more than one).

All commands respond to a first Ctrl-C/SIGTERM by cancelling their
context; a second one forces an immediate exit regardless of what the
process is doing. `migrate-force` is a single near-instant metadata
update, so that context has nothing to do for it. `migrate-up` and
`migrate-down` use it to stop gracefully between migration files instead
of dying mid-migration — and, critically, they treat a canceled context
as a failure even if golang-migrate itself reports success: a graceful
stop makes the underlying library call return the same nil it would
return on genuine completion, so without this check a signal landing
mid-migration would log "migrations applied" (or "rolled back") and
exit 0 having done partial or no work — found live, on the one command
gated behind `--yes` specifically because it's destructive. That first
signal also isn't guaranteed to land promptly: golang-migrate's own lock
acquisition ignores the context it's given and runs with
`context.Background()` regardless, so a migration blocked waiting on
another process's lock is not interruptible by a first signal at all.
Verified live: holding an `ACCESS EXCLUSIVE` lock on `schema_migrations`
and sending SIGTERM to a blocked `migrate-down` left it running; a second
SIGTERM forced it to exit before it had touched the table. Either way —
blocked on a lock, or interrupted mid-run — the command exits non-zero
and is safe to simply re-run; `migrate-up`/`migrate-down` are idempotent.
If a migration is ever left dirty anyway (a hard kill, `kill -9`, a crash
mid-statement), `migrate-force` recovers it without a manual `psql`
session.

## Environment variables

See [.env.example](.env.example) for the full list with defaults. All
validation happens in `internal/config`; an invalid or missing required
value fails fast with a specific error message (multiple problems are
reported together, not one at a time), and that message never includes a
raw credential even when the value itself was the problem.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `APP_ENV` | no | `development` | one of `development`, `staging`, `production` |
| `DATABASE_URL` | **yes** | — | `postgres://` or `postgresql://` |
| `DB_MAX_OPEN_CONNS` | no | `10` | 1 – 2147483647 |
| `DB_MIN_CONNS` | no | `2` | a floor pgxpool tries to keep warm, not a cap — pgxpool has no idle-connection ceiling; must not exceed `DB_MAX_OPEN_CONNS` |
| `DB_CONN_MAX_LIFETIME` | no | `30m` | must be positive |
| `DB_CONN_MAX_IDLE_TIME` | no | `5m` | must be positive |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | no | `json` | `json`, `text` |
| `SHUTDOWN_TIMEOUT` | no | `15s` | max wait during graceful shutdown |
| `TEST_DATABASE_URL` | no | — | integration tests only; database name must end in `_test` |

Config values passed via `DATABASE_URL`'s own query string (e.g.
`pool_max_conns`) are intentionally overridden by the `DB_*` variables
above — `internal/config` is the single source of truth for pool sizing,
not the DSN.

## Database

PostgreSQL is the source of truth. Schema is managed with versioned SQL
migrations under [migrations/](migrations/), embedded into the binary and
applied via `golang-migrate` (never automatically at process startup).

Phase 1 schema:

- **companies** — organizations known to the system. Unique on
  `lower(name)` and, when present, on a normalized form of `website`
  (case-folded, trailing slash stripped — it does not normalize `http`
  vs `https` or `www` vs bare host, since that needs real URL parsing).
- **target_companies** — a company's ATS boards. A company may have
  several. Unique on `(ats_provider, external_board_id)`.
- **jobs** — normalized postings. Identity is `(source, source_job_id)`
  per the provider's own stable ID, with a `canonical_url` fallback —
  never company name + title (see CLAUDE.md §9). Composite foreign keys
  tie `(target_company_id, company_id)` and `(target_company_id, source)`
  back to `target_companies`, so a job can't be attributed to a company
  that doesn't own its board, and its `source` can't drift from the
  board's own `ats_provider` — both would otherwise silently corrupt the
  identity constraint above. `remote_type`, `employment_type`, and
  `status` are DB-level `CHECK` constrained, as are non-blank identity
  fields, `status`/`closed_at` consistency, and `last_seen_at >=
  first_seen_at`. `updated_at` on every table is maintained by a trigger
  (only on real changes, not a no-op `UPDATE`), not application code.

There is deliberately no `job_eligibility` table yet — nothing populates
it until the geographic-classification phase lands.

## Testing

```bash
make test         # unit tests only — no database required
docker compose up -d db
export TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test
go test ./...      # now also runs internal/database's integration tests
make test-race
```

Database and migration tests run against a real PostgreSQL instance
(skipped automatically if `TEST_DATABASE_URL` is unset) — they are not
mocked, per project testing policy. These tests call `migrate-down` and
drop tables, so `TEST_DATABASE_URL`'s database name **must** end in
`_test`; the test suite refuses to run otherwise. `docker compose up -d
db` provisions `aggregator_test` alongside the main `aggregator` database
automatically (see `scripts/postgres-initdb/`) — never point
`TEST_DATABASE_URL` at the same database `DATABASE_URL` uses.

## Docker

```bash
docker compose up -d db      # PostgreSQL only, for local development/tests
docker compose up --build    # PostgreSQL + the aggregator binary
```

Postgres's port is published on `127.0.0.1` only. The `app` service waits
for the database's healthcheck before starting. It does not run
migrations automatically; run `docker compose run --rm app migrate-up`
once against a fresh database first.

## Architecture

Modular monolith, one deployable binary. Package boundaries so far:

- `internal/config` — env parsing and validation. No I/O beyond
  `os.Getenv`.
- `internal/observability` — `slog` construction.
- `internal/database` — the `pgxpool` connection pool and the migration
  runner. Nothing else in the codebase touches `pgxpool` directly.
- `migrations/` — versioned SQL, embedded via `go:embed`.
- `cmd/aggregator` — composition root: wires config → logger → pool,
  dispatches `run`/`migrate-up`/`migrate-down`/`migrate-force`, handles
  graceful shutdown.

`company`, `discovery`, `ats`, `ingestion`, `job`, `filtering`, `ranking`,
`notification`, and `scheduler` do not exist yet — they're created when
the phase that needs them lands, not before.
