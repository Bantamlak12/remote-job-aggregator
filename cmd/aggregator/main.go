// Command aggregator is the composition root for the remote job
// aggregator. It wires configuration, logging, and the database pool
// together, and dispatches to the subcommand named on the command line.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/observability"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

const usage = "usage: aggregator <run|migrate-up|migrate-down --yes [--all]|migrate-force <version>>"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		os.Exit(1)
	}
}

// bootstrapLogger is used only to report a config load failure, i.e.
// before we know the operator's chosen LOG_FORMAT/LOG_LEVEL. Every other
// error in this file is logged through the properly configured logger
// built right after config.Load succeeds.
func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// installSignalHandling returns a context canceled on the first
// SIGINT/SIGTERM, and forces an immediate os.Exit(1) on a second one.
//
// signal.NotifyContext alone does not give "second signal forces exit"
// behavior: it keeps re-delivering every signal as the same cancellation
// for as long as its stop func hasn't been called, and stop can't be
// called while the caller is itself blocked waiting on something that
// doesn't check ctx (e.g. golang-migrate's own Lock(), which runs its
// query with context.Background() regardless of the context passed to
// Up()/Down()/Steps() — verified live: holding an ACCESS EXCLUSIVE lock
// on schema_migrations and sending SIGTERM to a blocked migrate-down
// leaves it running indefinitely no matter how many SIGTERMs follow).
// This gives every command — including one stuck inside a library call
// we can't interrupt — a way out via a second signal, the same "ask
// nicely, then insist" pattern graceful shutdown implementations
// conventionally offer, without relying on OS default disposition (which
// signal.NotifyContext suppresses for as long as it's installed).
func installSignalHandling(parent context.Context) (ctx context.Context, stop func()) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-done:
			return
		}
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "received a second interrupt signal, forcing immediate exit")
			os.Exit(1)
		case <-done:
		}
	}()

	// Wrapped in sync.Once: this mimics signal.NotifyContext's stop
	// signature, and that stop is safe to call more than once, so callers
	// reasonably expect the same here. Without it, a second call would
	// panic on the already-closed done channel.
	var once sync.Once
	stop = func() {
		once.Do(func() {
			signal.Stop(sigCh)
			cancel()
			close(done)
		})
	}
	return ctx, stop
}

func run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, usage)
		return errors.New(usage)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run", "migrate-up", "migrate-down", "migrate-force":
	default:
		fmt.Fprintln(os.Stderr, usage)
		return fmt.Errorf("unknown command %q: %s", cmd, usage)
	}

	cfg, err := config.Load()
	if err != nil {
		bootstrapLogger().Error("failed to load configuration", "error", err)
		return err
	}

	logger := observability.NewLogger(cfg.Log, os.Stdout)
	slog.SetDefault(logger)

	// Every command gets a context that's canceled on the first
	// Ctrl-C/SIGTERM, with a second signal forcing immediate exit — see
	// installSignalHandling's comment for why that needs its own logic
	// rather than relying on signal.NotifyContext alone. migrate-up and
	// migrate-down additionally use that cancellation to stop gracefully
	// between migration files, via watchGracefulStop, instead of the
	// process dying mid-migration with schema_migrations left dirty.
	// migrate-force takes no context: it's a single fast metadata update
	// with nothing for graceful-stop to do.
	ctx, stop := installSignalHandling(ctx)
	defer stop()

	switch cmd {
	case "migrate-up":
		return runMigrateUp(ctx, cfg, logger)
	case "migrate-down":
		return runMigrateDown(ctx, cfg, logger, rest)
	case "migrate-force":
		return runMigrateForce(cfg, logger, rest)
	case "run":
		return runApp(ctx, cfg, logger)
	}
	return nil // unreachable: cmd was validated above
}

func runMigrateUp(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("applying migrations")
	if err := database.MigrateUp(ctx, cfg.Database.URL, migrations.FS); err != nil {
		logger.Error("applying migrations failed", "error", err)
		return err
	}
	logger.Info("migrations applied")
	return nil
}

// runMigrateDown always requires --yes: rolling back a migration is a
// DROP-TABLE-class operation (this repo currently has exactly one
// migration, so even the "single step" default drops every table), and
// CLAUDE.md's Safety rules require explicit confirmation for that, not a
// bare CLI verb. --all additionally rolls back every applied migration
// instead of just the most recent one.
func runMigrateDown(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	var all, yes bool
	for _, arg := range args {
		switch arg {
		case "--all":
			all = true
		case "--yes":
			yes = true
		default:
			err := fmt.Errorf("migrate-down: unknown flag %q (usage: migrate-down --yes [--all])", arg)
			logger.Error(err.Error())
			return err
		}
	}

	if !yes {
		err := errors.New("migrate-down requires --yes to confirm: rolls back one migration by default, or every migration with --all (usage: migrate-down --yes [--all])")
		logger.Error(err.Error())
		return err
	}

	if all {
		logger.Warn("rolling back ALL migrations, dropping every table")
		if err := database.MigrateDown(ctx, cfg.Database.URL, migrations.FS); err != nil {
			logger.Error("rolling back migrations failed", "error", err)
			return err
		}
		logger.Info("all migrations rolled back")
		return nil
	}

	logger.Info("rolling back one migration")
	if err := database.MigrateDownStep(ctx, cfg.Database.URL, migrations.FS); err != nil {
		logger.Error("rolling back migration failed", "error", err)
		return err
	}
	logger.Info("migration rolled back")
	return nil
}

// runMigrateForce clears a "dirty" schema_migrations state left behind by
// an interrupted migrate-up/migrate-down, without running any migration.
func runMigrateForce(cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) != 1 {
		err := errors.New("migrate-force: usage: migrate-force <version>")
		logger.Error(err.Error())
		return err
	}
	version, err := strconv.Atoi(args[0])
	if err != nil {
		err := fmt.Errorf("migrate-force: version must be an integer, got %q", args[0])
		logger.Error(err.Error())
		return err
	}

	logger.Warn("forcing migration version, no migration will run", "version", version)
	if err := database.MigrateForce(cfg.Database.URL, migrations.FS, version); err != nil {
		logger.Error("forcing migration version failed", "error", err)
		return err
	}
	logger.Info("migration version forced", "version", version)
	return nil
}

// runApp starts the long-running process: connect to the database, then
// block until a shutdown signal arrives, then release resources within
// cfg.Shutdown.Timeout. There is no ingestion pipeline yet (that lands in
// a later phase) — this establishes the connect/shutdown lifecycle the
// rest of the application will run inside.
func runApp(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("starting aggregator", "app_env", cfg.AppEnv, "database", cfg.Database)

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	logger.Info("connected to database")

	<-ctx.Done()
	logger.Info("shutdown signal received, closing resources")

	if err := closeWithTimeout(cfg.Shutdown.Timeout, db.Close); err != nil {
		logger.Error("shutdown did not complete cleanly", "error", err)
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

// closeWithTimeout runs closeFn in the background and waits up to timeout
// for it to finish, returning an error if it doesn't. Extracted from
// runApp so the timeout-vs-clean-exit behavior is testable without a real
// database or OS signals.
func closeWithTimeout(timeout time.Duration, closeFn func()) error {
	done := make(chan struct{})
	go func() {
		closeFn()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("shutdown timed out after %s", timeout)
	}
}
