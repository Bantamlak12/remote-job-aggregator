# Project Status

_Last updated: 2026-09-20. Generated after the original development worktree was lost and this
session was recovered — see [Section 10](#10-development-continuation-instructions) before
making changes._

## 1. Project Overview

**remote-job-aggregator** is an autonomous remote-job aggregation platform, built as a Go
modular monolith. Its purpose: discover, collect, normalize, filter, rank, store, and notify
about legitimate remote job postings — with particular attention to correct geographic
eligibility (default target: Ethiopia), reliable deduplication, and data quality over raw
volume — so a software engineer doesn't have to manually check hundreds of company career
pages.

Target scale (architectural, not yet exercised): 10,000+ companies, 100,000+ jobs, without
loading the full dataset into memory. Tech stack: Go (stdlib-first), PostgreSQL, Docker.
Full requirements live in the repo's (gitignored, not committed) `CLAUDE.md`.

Only **Phase 1 — foundation** is implemented. Nothing that touches an actual job posting
exists yet: no discovery, no ATS integration, no ingestion, no filtering, no ranking, no
scheduler, no notifications.

## 2. Completed Work

### Phase 1: Configuration, database pool, migrations, CLI, graceful shutdown

Merged to `main` via PR #1 (commit `7abc04d`, merge commit `beb25bf`, authored/merged
2026-09-17). Verified working against the current `main` checkout on 2026-09-20 (see
Section 7 for the exact commands run).

**Config — `internal/config/config.go`**
- Loads and validates all operational config from environment variables (`config.Load()`).
- Explicit types (`Environment`, `LogFormat`, `DatabaseConfig`, `LogConfig`,
  `ShutdownConfig`), no global mutable state — constructed once in `main` and passed down.
- Every validation failure is collected and returned together (`errors.Join`), not one at a
  time.
- `DATABASE_URL` validation never echoes the raw or even partially-parsed value in an error
  message — only the parsed URL scheme (`schemeOf`) — specifically because an earlier
  version leaked credentials into error text for malformed/scheme-less URLs.
- `DatabaseConfig.LogValue()` redacts credentials in both forms pgx/libpq accept: userinfo
  (`postgres://user:pass@...`) and a `?password=` query parameter.
- Pool size fields (`MaxConns`, `MinConns`) are `int32` at the type level (matching
  pgxpool's own field types) with range validation in `Load()`, not an unchecked cast later.
- Tests: `internal/config/config_test.go` — defaults, overrides, every validation failure
  path (including the two credential-leak shapes above), redaction through `LogValue()`.

**Database & migrations — `internal/database/`**
- `database.go`: wraps `pgxpool.Pool` (`New`, `Health`, `Close`). Pool sizing (max/min
  conns, conn lifetime/idle time) comes entirely from `config.DatabaseConfig`, explicit
  precedence over any `pool_max_conns`-style DSN query params.
- `migrate.go`: migration runner on top of `golang-migrate/migrate/v4` (pgx5 driver,
  embedded `iofs` source). Exposes `MigrateUp`, `MigrateDown` (full rollback),
  `MigrateDownStep` (roll back one migration), `MigrateForce` (clear a dirty
  `schema_migrations` state without running anything).
- `checkInterrupted(ctx)`: every migrate function checks `ctx.Err()` before returning
  success, because golang-migrate's own graceful-stop mechanism returns the same `nil` on a
  stop as on genuine completion — without this check, a SIGTERM mid-migration could report
  "applied"/"rolled back" and exit 0 having done partial or no work. Verified live against a
  real lock-blocked database.
- `watchGracefulStop`: wires `ctx` cancellation to golang-migrate's `GracefulStop` channel so
  Ctrl-C/SIGTERM stops a migration between files, not mid-statement.
- Tests: `internal/database/database_test.go` (20 tests) — pool connectivity, unreachable-host
  handling, migration idempotency (including a golang-migrate inconsistency where `Steps(-1)`
  at nil version returns `os.ErrNotExist` instead of `migrate.ErrNoChange`, disambiguated
  from a genuine "no migration found for version N" failure), context-cancellation handling
  for all three migrate functions, and schema-constraint enforcement (see below). All run
  against a **real PostgreSQL instance** via `docker-compose`, not mocks; skip cleanly
  (`t.Skip`) if `TEST_DATABASE_URL` is unset, and refuse to run at all unless that URL's
  database name ends in `_test` (these tests call `MigrateDown`, which drops every table).

**Schema — `migrations/000001_init_schema.{up,down}.sql`**
- Tables: `companies`, `target_companies`, `jobs`. See [Section 5](#5-current-database-state)
  for full detail.
- DB-level invariants (not just application logic): composite foreign keys tying a job's
  `company_id`/`source` to the target board it actually belongs to; non-blank `CHECK`
  constraints on every identity field; `status`/`closed_at` consistency;
  `last_seen_at >= first_seen_at`; case/whitespace-insensitive uniqueness on company name,
  company website (normalized), and target board id.
- `updated_at` maintained by a trigger that only fires on a genuine change (not a no-op
  `UPDATE`), not application code.
- Deliberately **no** `job_eligibility` table yet — nothing populates it until the filtering
  phase.

**Observability — `internal/observability/logger.go`**
- `slog` construction, JSON or text handler, configurable level.
- Tests: `internal/observability/logger_test.go` — level filtering, handler format, and
  credential redaction verified through a **real `slog.Handler`** (not just the
  `LogValue()` method directly — an earlier version's redaction bug only surfaced when
  tested this way).

**CLI — `cmd/aggregator/main.go`**
- Commands: `run` (connect, block until shutdown signal, close pool), `migrate-up`,
  `migrate-down` (always requires `--yes`; add `--all` for a full rollback instead of one
  step), `migrate-force <version>`.
- Graceful shutdown: first SIGINT/SIGTERM cancels context; a second one forces immediate
  `os.Exit(1)` (`installSignalHandling`) — needed because golang-migrate's own lock
  acquisition ignores the context it's given and runs with `context.Background()`
  regardless, so a migration blocked on another process's lock is not interruptible by a
  first signal at all (verified live).
- Tests: `cmd/aggregator/main_test.go` — CLI dispatch/validation, `closeWithTimeout`
  behavior, double-signal force-exit (via subprocess re-exec, the standard Go idiom for
  testing `os.Exit`), signal-handler idempotency.

**Docker — `docker-compose.yml`, `Dockerfile`, `.dockerignore`, `scripts/postgres-initdb/`**
- `docker-compose.yml`: `db` (Postgres 17, port bound to `127.0.0.1` only) +
  `app` services. `scripts/postgres-initdb/01-create-test-db.sql` auto-provisions a second
  `aggregator_test` database so integration tests never point at the dev database.
- `Dockerfile`: multi-stage build, `-trimpath -ldflags="-s -w"`, `ca-certificates` installed
  in the final image (needed for any future outbound HTTPS/TLS), non-root user.
- Verified live end to end: `docker compose build app`, `docker compose up -d`,
  `docker compose run --rm app migrate-up` all succeed; the containerized app connects to
  the containerized database.

**Review process that produced this state.** This phase went through five rounds of
independent, adversarial critic review (see the conversation history for full transcripts),
each verifying claims by actually running commands — and, for several fixes, deliberately
reverting the fix to confirm the regression test actually fails without it (mutation
testing). That process is why the credential-leak, missing-invariant, unsafe-migrate-down,
and false-success-on-signal issues above don't exist in the merged code — they were found
and closed before merge, not left as known issues.

## 3. Work In Progress

Nothing is mid-implementation in the codebase itself — Phase 1 is complete and merged, and
`git status` in a fresh worktree off `main` is clean.

**However**, the original shared checkout at `/home/bantamlak/my-repos/remote-job-aggregator`
(as distinct from the worktree this document was written in) currently has two files with
**uncommitted local modifications** that predate this recovery and were not made by this
session:

- `.gitignore` — one addition not present in the committed version: a `.claude/` ignore
  entry (with a `# Agent` comment), inserted just before the existing `CLAUDE.md` ignore
  rule.
- `Dockerfile` — purely cosmetic: blank lines added between each instruction. No content
  difference otherwise.

These were **not modified, committed, or discarded** while writing this document (per
explicit instruction). If the `.claude/` gitignore addition is wanted, it should be
committed properly through a worktree, not left dangling in the shared checkout.

There is also a Postgres container (`remote-job-aggregator-db-1`, `docker compose up -d db`
run from the shared checkout, not a worktree) that has been running for approximately 9
hours as of this writing. Its `aggregator` database is at migration version 1, clean, with
zero rows in `companies`/`target_companies`/`jobs` — consistent with a plain
`migrate-up` and no ingestion (which doesn't exist yet). No orphaned or unexplained data.

## 4. Remaining Implementation Roadmap

Ordered by dependency, per the original project requirements.

### Phase 2 — Company & discovery, HTTP infrastructure
- **Objective:** persist companies and their ATS targets; build the reusable HTTP client
  infrastructure every provider integration will sit on.
- **Tasks:** `company` package (repository over the existing `companies` table);
  `discovery` package (seed companies, candidate ATS slug discovery, endpoint validation,
  persist discovered targets to `target_companies`); pooled `http.Client` with timeouts,
  max response size, User-Agent, retry/backoff for 429/5xx.
- **Expected files/modules:** `internal/company/`, `internal/discovery/`, an HTTP client
  builder (likely `internal/httpclient/` or similar).
- **Dependencies:** none beyond Phase 1 (schema and pool already exist).
- **Verification:** unit tests for discovery logic against fake HTTP servers/fixtures;
  repository tests against real Postgres (same pattern as `internal/database`).

### Phase 3 — ATS integration & ingestion
- **Objective:** fetch and normalize jobs from Greenhouse (first provider), run ingestion
  as a bounded worker pool.
- **Tasks:** `ats` package with a `Provider`-scoped client interface and Greenhouse's
  first; provider-specific response models kept separate from the normalized `job.Post`
  type; `ingestion` package (load active targets, bounded worker pool, decode/normalize,
  route through filtering/ranking once those exist); `job` package for normalization and
  lifecycle (new/changed/removed detection, content-hash comparison).
- **Expected files/modules:** `internal/ats/greenhouse/`, `internal/ingestion/`,
  `internal/job/`.
- **Dependencies:** Phase 2 (targets to ingest from, HTTP client).
- **Verification:** fake ATS test servers/fixtures (success, 404, 429, 500, invalid JSON,
  slow/large response, duplicate/changed jobs); `go test -race` for the worker pool.

### Phase 4 — Geographic eligibility & relevance filtering
- **Objective:** deterministic classification of remote-eligibility and role relevance.
- **Tasks:** `filtering` package — geographic classifier (ELIGIBLE/INELIGIBLE/UNCERTAIN
  with confidence, reasons, evidence, detected locations, restrictions) and relevance
  filtering (configurable positive/negative signals, not hard-coded to one job title). New
  migration adding `job_eligibility` (deferred from Phase 1 deliberately — see Section 5).
- **Expected files/modules:** `internal/filtering/`, `migrations/00000X_job_eligibility.*.sql`.
- **Dependencies:** Phase 3 (jobs to classify).
- **Verification:** unit tests are the primary tool (deterministic logic), plus an eval
  suite per CLAUDE.md's rule that classification quality needs more than deterministic
  tests alone.

### Phase 5 — Ranking & upsert/change detection
- **Objective:** deterministic, explainable scoring; wire filtering+ranking into ingestion
  with PostgreSQL upsert and lifecycle (new/changed/removed) detection.
- **Tasks:** `ranking` package (configurable scoring, explainable output); ingestion
  updated to upsert via `(source, source_job_id)` identity, detect changes via
  `content_hash`, mark removed jobs via `status`.
- **Expected files/modules:** `internal/ranking/`; changes to `internal/ingestion/`.
- **Dependencies:** Phase 4 (jobs need eligibility/relevance data to rank against).
- **Verification:** unit tests for scoring; integration tests for upsert/change-detection
  against real Postgres.

### Phase 6 — Scheduling & notifications
- **Objective:** periodic, unattended execution; notify on relevant new jobs.
- **Tasks:** `scheduler` package (periodic discovery/ingestion/cleanup, no distributed
  scheduling infra); `notification` package with a `Notifier` interface, Telegram first.
- **Expected files/modules:** `internal/scheduler/`, `internal/notification/telegram/`.
- **Dependencies:** Phase 5 (ranked jobs to notify about).
- **Verification:** scheduler tests with a fake clock/ticker; notifier tests against a fake
  Telegram endpoint.

### Phase 7 — Additional providers & observability
- **Objective:** Lever and Ashby support; metrics beyond structured logging.
- **Tasks:** `internal/ats/lever/`, `internal/ats/ashby/`; metrics
  (`jobs_fetched_total`, `provider_errors_total`, etc.) per CLAUDE.md §23.
- **Dependencies:** Phase 3's `ats` abstraction must already support a second/third
  provider without rework.
- **Verification:** same fake-server pattern as Greenhouse.

## 5. Current Database State

**Migrations:** one — `000001_init_schema` (up + down), applied and clean
(`schema_migrations`: version 1, dirty=false) in both the local `aggregator` dev database
and verified working against `aggregator_test`.

**Tables:**

| Table | Purpose | Key constraints |
|---|---|---|
| `companies` | Organizations known to the system | PK `id`; `CHECK` non-blank `name`; unique on `lower(btrim(name))`; unique on `normalize_website(website)` where present (case-fold + trailing-slash strip, does **not** normalize http/https or www) |
| `target_companies` | A company's ATS boards (many per company) | PK `id`; FK `company_id → companies(id) ON DELETE CASCADE`; unique on `(ats_provider, lower(btrim(external_board_id)))`; `CHECK` non-blank `ats_provider`/`external_board_id`; two support-only unique constraints `(id, company_id)` and `(id, ats_provider)` used by `jobs`' composite FKs |
| `jobs` | Normalized postings | PK `id`; identity = unique `(source, source_job_id)` — **not** company+title; composite FKs `(target_company_id, company_id) → target_companies(id, company_id)` and `(target_company_id, source) → target_companies(id, ats_provider)` (both `ON DELETE CASCADE`) so a job can't be attributed to a company that doesn't own its board, and `source` can't drift from the board's own provider; `CHECK` non-blank on `source`/`source_job_id`/`title`/`application_url`/`content_hash`; `CHECK` `canonical_url IS NULL OR` non-blank; `remote_type`/`employment_type`/`status` are `CHECK`-constrained enums; `CHECK last_seen_at >= first_seen_at`; `CHECK (status IN ('closed','removed')) = (closed_at IS NOT NULL)`; unique partial index on `canonical_url` where present |

**Relationships:** `companies` 1—N `target_companies` 1—N `jobs`, with `jobs` also holding a
direct (constrained, see above) `company_id`.

**Indexes beyond the uniqueness ones above:** `target_companies(company_id)`,
`target_companies(is_active)` (partial, active only), `jobs(company_id)`,
`jobs(target_company_id)`, `jobs(status)` (partial, `status='open'` only — the dominant
query pattern), `jobs(last_seen_at)`.

**`updated_at` triggers** on all three tables via a shared `set_updated_at()` function,
firing only when a column other than `updated_at` itself actually changed.

**Missing schema components (intentional, not oversights):**
- `job_eligibility` — geographic/relevance classification data. Belongs to Phase 4.
- No indexes/tables for ranking, scheduling state, or notification dedup — belong to
  Phases 5–6.

**Database-related pending work:** none outstanding within Phase 1's scope. The next schema
change is the Phase 4 `job_eligibility` migration.

## 6. Current Architecture

**Implemented today:**

```
cmd/aggregator (composition root)
    │
    ├─ internal/config          (env → validated Config)
    ├─ internal/observability   (slog construction)
    └─ internal/database        (pgxpool + migration runner)
            │
            └─ migrations/      (embedded SQL, go:embed)
```

One deployable binary. `cmd/aggregator/main.go` is the only place that wires concrete
implementations together — `config.Load()` → `observability.NewLogger()` →
`database.New()` → dispatch to the requested command. Nothing outside `internal/database`
touches `pgxpool` directly; nothing outside `internal/config` reads `os.Getenv`.

**Planned, not implemented:** `internal/company`, `internal/discovery`, `internal/ats`,
`internal/ingestion`, `internal/job`, `internal/filtering`, `internal/ranking`,
`internal/notification`, `internal/scheduler` — see Section 4. These packages do not exist
on disk. Do not assume any of their functionality when reasoning about what the system
currently does.

**Concurrency implemented today:** only `pgxpool`'s internal connection management
(bounded by config) and the CLI's own signal-handling goroutines (`installSignalHandling`,
`watchGracefulStop`, `closeWithTimeout`). No worker pools, no channels-based pipeline yet —
those arrive with ingestion in Phase 3.

## 7. Testing and Verification Status

**Run and passing as of this document (2026-09-20, fresh worktree off `main` at
`beb25bf`):**

```bash
gofmt -l .                    # clean
go build ./...                # clean
go vet ./...                  # clean
go test ./...                 # all 4 packages ok, DB tests skip without TEST_DATABASE_URL
go test -race -count=1 ./...  # all 4 packages ok, WITH a live Postgres (TEST_DATABASE_URL
                               #   set to the aggregator_test database on the already-running
                               #   remote-job-aggregator-db-1 container) — 20/20 tests in
                               #   internal/database pass under the race detector
```

**Recommended verification commands for a fresh session:**

```bash
# From a worktree (never the shared checkout — see Section 10):
gofmt -l .
go build ./...
go vet ./...
go test ./...                          # unit tests, no DB required

docker compose up -d db                # or reuse the already-running container if present
export TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test
go test -count=1 ./...
go test -race -count=1 ./...

docker compose build app
docker compose run --rm app migrate-up # confirms the Docker path end to end
```

**Missing tests:** none within Phase 1's own scope — every package has both unit and (where
a real dependency exists) integration coverage, including deliberately adversarial cases
(context cancellation mid-migration, dirty-state recovery, credential redaction through a
real log handler, double-signal force-exit). Coverage will naturally need to grow with each
future phase (fake ATS servers for Phase 3, an eval suite for Phase 4's classifier, etc.) —
none of that exists yet because none of that code exists yet.

**Known failures:** none.

## 8. Known Issues and Risks

- **Uncommitted local changes in the shared checkout** (Section 3) — not part of any
  branch, not backed up anywhere except the live filesystem. If that checkout is lost again,
  the `.claude/` gitignore addition would be lost with it (low cost: it's a one-line,
  easily-redone change).
- **`ats_provider` is an unconstrained `TEXT` column** (no DB-level `CHECK`/enum) by
  deliberate design — new providers shouldn't need a migration — but this means the Go-side
  `ats.Provider` type (which doesn't exist yet) is the only thing that will ever validate it.
  Not a bug today since nothing writes to this column outside tests.
- **No HTTP client infrastructure exists yet.** Every future phase depends on it (Phase 2).
  Building it without reusing a battle-tested pattern (connection pooling, backoff, size
  limits) is the most likely place for a future session to reinvent something poorly if it
  skips the "search before building" step CLAUDE.md requires.
- **Single migration file.** `migrate-down`'s `--all` vs. single-step distinction is
  currently untested in the "more than one migration" case, since there's only ever been
  one. This will get real exercise the moment Phase 4 adds a second migration — worth
  double-checking `MigrateDownStep` behavior then, even though it's already unit-tested
  against the underlying golang-migrate mechanics.
- **This session was recovered after the original worktree was lost.** The cause of that
  loss was not investigated as part of this task (out of scope — see the task that produced
  this document). If it recurs, the recovery pattern in Section 10 should still apply.

## 9. Exact Next Step

**Start Phase 2: build `internal/company` (a thin repository over the existing `companies`
table) and the pooled HTTP client infrastructure `discovery` will need.**

Why this and not `internal/discovery` itself first: `discovery`'s job is to populate
`companies`/`target_companies`, so it needs a `company` repository to write through, and it
needs a properly configured `http.Client` (timeouts, size limits, User-Agent, retry/backoff)
before it can safely make its first outbound request to a candidate ATS endpoint. Building
the HTTP client in isolation first, with its own tests against a fake server, means
`discovery`'s own tests can focus on discovery logic rather than re-deriving HTTP
correctness inline.

Concretely: read CLAUDE.md §13 (HTTP Architecture) and §11 (ATS Integrations) again before
starting, since they specify the exact behaviors required (connection pooling, context
cancellation, max response size, 429/5xx retry with backoff) — building this without that
checklist in hand is how a future session ends up re-doing it.

## 10. Development Continuation Instructions

**Before making any change to this repository:**

1. Read this file (`PROJECT_STATUS.md`) in full.
2. Read `CLAUDE.md` at the repo root (gitignored — present locally, not on GitHub; if it's
   missing, ask Bantamlak for a copy before proceeding, since it carries the authoritative
   project requirements and the mandatory branching/testing/review workflow).
3. Run the verification commands in [Section 7](#7-testing-and-verification-status) to
   confirm the state this document describes still matches reality — this document is a
   snapshot, not a live source of truth. If something doesn't match, trust the repository
   and update this document, not the other way around.
4. Follow CLAUDE.md's "Branching" section exactly: one worktree per session, never write in
   the shared checkout (`/home/bantamlak/my-repos/remote-job-aggregator`). This document was
   itself written from inside a freshly created worktree for exactly this reason, after the
   prior one was lost.

**After completing meaningful work:**

1. Update this document's relevant sections — especially Section 2 (move newly-completed
   work out of Section 4), Section 3, Section 5 if the schema changed, and Section 9 (name
   the new next step).
2. Do not let this document drift from the actual repository state. A stale status document
   is worse than none, since it will be trusted.
3. Follow CLAUDE.md's commit/push/PR ritual for the code changes themselves; update this
   file in the same commit as the work it describes, not a separate one, so the two never
   fall out of sync.
