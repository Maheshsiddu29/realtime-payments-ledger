// Package database owns the PostgreSQL connection pool.
//
// It is the only place that turns configuration into a live pool, and the only
// place that closes one. Repository types in other packages receive the pool;
// they never construct their own.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
)

// DB wraps a pgx connection pool together with the logger used for lifecycle
// events.
type DB struct {
	pool           *pgxpool.Pool
	log            *slog.Logger
	connectTimeout time.Duration
	redactedDSN    string
}

// New builds a connection pool from configuration.
//
// It returns an error only for problems that no amount of waiting will fix —
// a malformed DSN or an unusable pool setting. It does not contact the server:
// pgxpool connects lazily, and whether the database is reachable right now is
// a readiness question, not a start-up question. Call Verify for that.
func New(cfg config.Config, log *slog.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.DSN())
	if err != nil {
		// Report the redacted form: the raw DSN contains the password.
		return nil, fmt.Errorf("invalid postgres configuration %s: %w", cfg.Postgres.RedactedDSN(), err)
	}

	// MaxConns is the pool ceiling. POSTGRES_MAX_IDLE_CONNS has no pgxpool
	// equivalent and is applied by the migration runner instead, which uses
	// database/sql. See docs/DATABASE.md.
	poolCfg.MaxConns = int32(cfg.Postgres.MaxOpenConns)
	poolCfg.MaxConnLifetime = cfg.Postgres.ConnMaxLifetime
	poolCfg.ConnConfig.ConnectTimeout = cfg.Postgres.ConnectTimeout

	// Tags every backend in pg_stat_activity, so a runaway query can be traced
	// back to this service.
	poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.App.Name

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	log.Info("postgres pool created",
		slog.String("dsn", cfg.Postgres.RedactedDSN()),
		slog.Int("max_conns", cfg.Postgres.MaxOpenConns),
		slog.Duration("conn_max_lifetime", cfg.Postgres.ConnMaxLifetime),
		slog.Duration("connect_timeout", cfg.Postgres.ConnectTimeout),
	)

	return &DB{
		pool:           pool,
		log:            log,
		connectTimeout: cfg.Postgres.ConnectTimeout,
		redactedDSN:    cfg.Postgres.RedactedDSN(),
	}, nil
}

// Verify opens a connection and confirms the server answers, bounded by
// POSTGRES_CONNECT_TIMEOUT so an unreachable database cannot hang start-up.
func (db *DB) Verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, db.connectTimeout)
	defer cancel()

	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres at %s did not respond within %s: %w",
			db.redactedDSN, db.connectTimeout, err)
	}
	return nil
}

// Pool returns the underlying pool for repository construction.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping is the readiness check registered with the health registry.
//
// It is deliberately a real round trip rather than an inspection of pool
// counters: a pool holding idle connections to a database that has stopped
// answering is not ready. The caller's context bounds it, so a wedged server
// fails the probe instead of stalling it.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres unreachable: %w", err)
	}
	return nil
}

// Close releases every pooled connection. It blocks until connections still in
// use are returned, so it runs after the HTTP server has drained.
func (db *DB) Close() {
	start := time.Now()
	db.pool.Close()
	db.log.Info("postgres pool closed", slog.Duration("duration", time.Since(start)))
}
