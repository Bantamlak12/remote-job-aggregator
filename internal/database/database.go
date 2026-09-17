// Package database owns the PostgreSQL connection pool and the migration
// runner. Nothing outside this package should construct a pgxpool.Pool
// directly.
package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
)

// DB wraps a pgxpool.Pool. It exists as a named type (rather than a type
// alias) so this package can attach behavior — Health, Close — without
// leaking pgxpool as part of every caller's import surface.
type DB struct {
	Pool *pgxpool.Pool
}

// New parses cfg into a pgxpool configuration, applies the pool-size and
// lifetime settings explicitly (CLAUDE.md: pool size must be a deliberate
// choice, not a default), and verifies connectivity with a ping before
// returning.
//
// cfg's fields always win over any pool_max_conns/pool_min_conns query
// parameters present in cfg.URL itself: config.Load is the single source
// of truth for pool sizing, so a DSN that also tunes the pool is
// overridden silently rather than the two disagreeing at runtime.
func New(ctx context.Context, cfg config.DatabaseConfig) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("database: parsing DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.ConnMaxLifetime
	poolCfg.MaxConnIdleTime = cfg.ConnMaxIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: creating connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: pinging database: %w", err)
	}

	return &DB{Pool: pool}, nil
}

// Health reports whether the pool can currently reach the database.
func (d *DB) Health(ctx context.Context) error {
	if err := d.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("database: health check failed: %w", err)
	}
	return nil
}

// Close releases every connection in the pool. Safe to call once during
// graceful shutdown.
func (d *DB) Close() {
	d.Pool.Close()
}
