package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	// Registers the "pgx5" database driver via its init(); not referenced
	// directly, we only ever address it by URL scheme.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// pgx5URL rewrites a postgres:// (or postgresql://) DATABASE_URL to the
// pgx5:// scheme golang-migrate's driver registry expects. The driver
// itself rewrites the scheme back to postgres before opening the
// connection; this is just how it picks which registered driver to use.
func pgx5URL(dbURL string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil {
		return "", fmt.Errorf("database: parsing DATABASE_URL: %w", err)
	}
	u.Scheme = "pgx5"
	return u.String(), nil
}

func newMigrator(dbURL string, migrations fs.FS) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("database: loading embedded migrations: %w", err)
	}

	target, err := pgx5URL(dbURL)
	if err != nil {
		return nil, err
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return nil, fmt.Errorf("database: initializing migrator: %w", err)
	}
	return m, nil
}

// watchGracefulStop wires ctx's cancellation to m.GracefulStop, so a
// migration interrupted by Ctrl-C or SIGTERM stops between migration
// files instead of the process dying mid-migration and leaving
// schema_migrations marked dirty. The returned stop func must be called
// once the migration call (Up/Down/Steps) has returned, to release the
// watcher goroutine.
//
// A graceful stop makes Up/Down/Steps return nil — the same value they
// return on genuine completion — so every caller here must check
// checkInterrupted(ctx) before treating that nil as success. See its
// doc comment for why: this is not hypothetical, it was found live.
func watchGracefulStop(ctx context.Context, m *migrate.Migrate) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			m.GracefulStop <- true
		case <-done:
		}
	}()
	return func() { close(done) }
}

// checkInterrupted reports whether ctx was canceled, for callers that
// just ran Up/Down/Steps and got back a nil error.
//
// Found live during development: golang-migrate's runMigrations starts
// its loop with "if m.stop() { return nil }", so a graceful stop
// (triggered by watchGracefulStop above) makes Up/Down/Steps return nil
// exactly as they would on genuine completion — with fewer migrations
// actually applied or rolled back than intended. Without this check, a
// single SIGTERM during a blocked or in-flight migrate-up/migrate-down
// logs "migrations applied" / "migration(s) rolled back" and exits 0
// having done partial or no work — on migrate-down specifically, the one
// command gated behind --yes because it's destructive, that is a false
// success on exactly the operation --yes exists to protect.
//
// Deliberately imprecise in the safe direction: a signal landing after
// the real work finished but before this check runs still reports an
// error for a migration that actually completed. That's fine —
// Up/Down/Steps are idempotent, so the caller just re-runs — and far
// better than the alternative of occasionally reporting success for
// work that didn't happen.
func checkInterrupted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("database: migration interrupted before completion, schema may be partially applied: %w", err)
	}
	return nil
}

// MigrateUp applies every pending migration in migrations, in order.
// It is idempotent: running it again with nothing pending is a no-op
// (migrate.ErrNoChange), not an error.
func MigrateUp(ctx context.Context, dbURL string, migrations fs.FS) error {
	m, err := newMigrator(dbURL, migrations)
	if err != nil {
		return err
	}
	defer m.Close()
	defer watchGracefulStop(ctx, m)()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database: applying migrations: %w", err)
	}
	return checkInterrupted(ctx)
}

// MigrateDown rolls back every applied migration, in reverse order. This
// is a full teardown — callers exposing it on a CLI must gate it behind
// an explicit, hard-to-fat-finger confirmation (CLAUDE.md Safety: no
// destructive op without explicit confirmation); see cmd/aggregator's
// "migrate-down --all --yes".
func MigrateDown(ctx context.Context, dbURL string, migrations fs.FS) error {
	m, err := newMigrator(dbURL, migrations)
	if err != nil {
		return err
	}
	defer m.Close()
	defer watchGracefulStop(ctx, m)()

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database: rolling back migrations: %w", err)
	}
	return checkInterrupted(ctx)
}

// MigrateDownStep rolls back exactly the most recently applied migration.
// This is the safe default for an unqualified "migrate-down": one
// reversible step, not an unconfirmed full teardown of every table.
//
// Idempotent like MigrateUp/MigrateDown, but golang-migrate reports "no
// migration left to roll back" differently for a stepped call than for
// Down(): Down() at nil version returns migrate.ErrNoChange, while
// Steps(-1) at nil version returns os.ErrNotExist (see migrate.go's
// readDown: the from==-1 && limit==-1 case is special-cased to
// ErrNoChange, the from==-1 && limit>0 case is not).
//
// os.ErrNotExist is NOT always that harmless case, though: the same
// sentinel wraps "no migration found for version N" when
// schema_migrations.version points at a version this binary has no
// migration file for (e.g. after `migrate-force` was pointed at a bogus
// version, or an older binary with fewer migrations stepped back on a
// newer database) — versionExists's fs.ErrNotExist from source/iofs
// propagates through unchanged. Swallowing os.ErrNotExist
// unconditionally would report success for that real failure while
// having done nothing, which is worse than the bug this is fixing:
// migrate-down is the one command gated behind --yes specifically
// because it's destructive, so it must never lie about what happened.
// Disambiguate by checking the actual version after Steps returns: nil
// version means genuinely nothing left to roll back, any other version
// means the ErrNotExist was real.
func MigrateDownStep(ctx context.Context, dbURL string, migrations fs.FS) error {
	m, err := newMigrator(dbURL, migrations)
	if err != nil {
		return err
	}
	defer m.Close()
	defer watchGracefulStop(ctx, m)()

	err = m.Steps(-1)
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		return checkInterrupted(ctx)
	}
	if errors.Is(err, os.ErrNotExist) {
		if _, _, verErr := m.Version(); errors.Is(verErr, migrate.ErrNilVersion) {
			return checkInterrupted(ctx)
		}
	}
	return fmt.Errorf("database: rolling back one migration: %w", err)
}

// MigrateForce sets schema_migrations to version without running any
// migration. It exists to clear a "dirty" state left behind when a
// migrate-up/migrate-down was interrupted after golang-migrate marked the
// database dirty but before it finished — without it, the only recovery
// path is hand-editing schema_migrations with psql.
func MigrateForce(dbURL string, migrations fs.FS, version int) error {
	m, err := newMigrator(dbURL, migrations)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Force(version); err != nil {
		return fmt.Errorf("database: forcing migration version %d: %w", version, err)
	}
	return nil
}
