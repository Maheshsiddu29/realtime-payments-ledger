//go:build integration

// Integration tests run against a real PostgreSQL server. There are no mocks
// here: the behaviour under test — transaction atomicity, deferred constraint
// triggers, CHECK constraints — belongs to the database, and a mock would only
// assert that the test author understood PostgreSQL correctly.
//
// Run them with:
//
//	make test-integration
//
// which is `go test -tags=integration ./...`. Without the tag these files are
// not compiled at all, so the default `go test ./...` stays fast and needs no
// infrastructure.
//
// Connection settings come from the same POSTGRES_* environment variables the
// application uses, except for the database name, which is taken from
// POSTGRES_TEST_DB (default "ledger_test") so that a stray test run can never
// touch a development database.

package tests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"database/sql"
	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/ledger"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/reconcile"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// migrationsPath is relative to this package's directory, which is the working
// directory for `go test`.
const migrationsPath = "../migrations"

// USD is the currency used by tests that do not care which currency it is.
const USD = money.Currency("USD")

// sharedPool is the pool every integration test uses. It is opened once in
// TestMain against a database whose schema has already been migrated.
var sharedPool *pgxpool.Pool

// sharedRedis is the Redis client every integration test uses. It points at a
// dedicated logical database (REDIS_TEST_DB, default 15) that the harness
// flushes between tests, so a test run can never disturb development data.
var sharedRedis *redis.Client

// sharedConfig is the configuration the harness resolved, reused by tests that
// need to build their own components.
var sharedConfig config.Config

// testConfig returns the application configuration with the database name
// redirected to the dedicated test database.
func testConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}

	name := os.Getenv("POSTGRES_TEST_DB")
	if name == "" {
		name = "ledger_test"
	}
	cfg.Postgres.Database = name

	// A dedicated Redis logical database, for the same reason.
	cfg.Redis.DB = testRedisDB()

	return cfg, nil
}

// testRedisDB returns the logical Redis database integration tests may flush.
func testRedisDB() int {
	if raw := os.Getenv("REDIS_TEST_DB"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	return 15
}

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		// Fail loudly rather than skipping. An integration test that silently
		// skips when the database is missing is a test that never runs.
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n\n"+
			"These tests need a running PostgreSQL. Start one with:\n"+
			"    make infra-up\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg, err := testConfig()
	if err != nil {
		return 0, err
	}

	if err := ensureDatabase(ctx, cfg); err != nil {
		return 0, err
	}
	if err := migrateUp(cfg); err != nil {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.Postgres.DSN())
	if err != nil {
		return 0, fmt.Errorf("open test pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return 0, fmt.Errorf("ping %s: %w", cfg.Postgres.RedactedDSN(), err)
	}
	sharedPool = pool

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.CommandTimeout,
		WriteTimeout: cfg.Redis.CommandTimeout,
	})
	defer rdb.Close()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return 0, fmt.Errorf("ping redis at %s (db %d): %w", cfg.Redis.Addr, cfg.Redis.DB, err)
	}
	sharedRedis = rdb
	sharedConfig = cfg

	return m.Run(), nil
}

// ensureDatabase creates the test database if it does not exist yet, by
// connecting to the maintenance database first. CREATE DATABASE has no
// IF NOT EXISTS form, so existence is checked separately.
func ensureDatabase(ctx context.Context, cfg config.Config) error {
	admin := cfg
	admin.Postgres.Database = "postgres"

	conn, err := pgx.Connect(ctx, admin.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("connect to %s: %w", admin.Postgres.RedactedDSN(), err)
	}
	defer conn.Close(ctx)

	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
		cfg.Postgres.Database).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check for database %q: %w", cfg.Postgres.Database, err)
	}
	if exists {
		return nil
	}

	// The database name cannot be a bind parameter in DDL. It comes from
	// POSTGRES_TEST_DB, so quote it rather than interpolating it raw.
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{cfg.Postgres.Database}.Sanitize())); err != nil {
		return fmt.Errorf("create database %q: %w", cfg.Postgres.Database, err)
	}
	return nil
}

// migrateUp applies every migration to the test database, exactly as an
// operator would with `make migrate-up`.
func migrateUp(cfg config.Config) error {
	m, closeFn, err := openMigrator(cfg)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

func openMigrator(cfg config.Config) (*migrate.Migrate, func(), error) {
	db, err := sql.Open("pgx/v5", cfg.Postgres.DSN())
	if err != nil {
		return nil, nil, err
	}
	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationsPath, "pgx5", driver)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return m, func() { _ = db.Close() }, nil
}

// env bundles the components under test.
type env struct {
	pool      *pgxpool.Pool
	accounts  *account.Repository
	transfers *transfer.Service
	entries   *ledger.Repository

	// redis and store back the idempotency coordination path.
	redis *redis.Client
	store *idempotency.Store

	// baseline records money placed into accounts by the test fixture rather
	// than by a transfer, so reconciliation can account for it explicitly.
	// See reconcile.Baseline for why this exception exists.
	baseline reconcile.Baseline
}

// newEnv returns a clean environment: every table is emptied first, so tests
// cannot see each other's rows.
//
// Integration tests are not run in parallel — they share one database — so
// truncating at the start of each test is safe.
func newEnv(t *testing.T) *env {
	t.Helper()

	truncateAll(t)

	flushRedis(t)

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := idempotency.NewStore(sharedRedis, sharedConfig)

	return &env{
		pool:      sharedPool,
		accounts:  account.NewRepository(sharedPool),
		transfers: transfer.NewService(sharedPool, log).WithIdempotency(store),
		entries:   ledger.NewRepository(sharedPool),
		redis:     sharedRedis,
		store:     store,
		baseline:  reconcile.Baseline{},
	}
}

// flushRedis empties the dedicated test database so idempotency records cannot
// leak between tests.
func flushRedis(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sharedRedis.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing the redis test database: %v", err)
	}
}

// truncateAll empties every table.
//
// TRUNCATE is used rather than DELETE because ledger_entries and completed
// transfers are protected by row-level triggers that reject DELETE. Those
// triggers do not fire for TRUNCATE, which is a table-level operation
// requiring table ownership — a real and documented limit of the append-only
// guarantee, and the reason this helper exists only in test code.
func truncateAll(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := sharedPool.Exec(ctx, `TRUNCATE ledger_entries, transfers, accounts CASCADE`)
	if err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test-only fixtures
// ---------------------------------------------------------------------------

// fund puts money into an account by writing the balance directly.
//
// THIS IS NOT PRODUCTION FUNCTIONALITY, and deliberately has no equivalent in
// any non-test package. It creates money from nothing: no ledger entry records
// where the balance came from, so it breaks the conservation property that the
// rest of the system maintains. It exists only so that a test can reach a
// funded starting state.
//
// Production funding is a transfer from a funding account, which a later phase
// models as an explicit deposit with its own ledger entries. Until then there
// is no application code path that puts money into the system at all.
func fund(t *testing.T, e *env, id uuid.UUID, minor int64) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tag, err := e.pool.Exec(ctx,
		`UPDATE accounts SET balance_minor = balance_minor + $2 WHERE id = $1`, id, minor)
	if err != nil {
		t.Fatalf("funding account %s: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("funding account %s affected %d rows, want 1", id, tag.RowsAffected())
	}

	// Record the unexplained money so reconciliation can subtract it.
	e.baseline[id] += minor
}

// assertReconciled runs the full reconciliation and fails the test with every
// problem it found.
//
// This is the strongest assertion available: it queries committed rows
// directly, so it holds regardless of what the Go code believed happened.
func assertReconciled(t *testing.T, e *env) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := reconcile.Check(ctx, e.pool, e.baseline)
	if err != nil {
		t.Fatalf("reconciliation failed to run: %v", err)
	}
	if !report.OK() {
		for _, problem := range report.Problems() {
			t.Errorf("reconciliation: %s", problem)
		}
		t.Fatalf("reconciliation found %d violations across %d accounts and %d completed transfers",
			report.Violations(), report.Accounts, report.CompletedTransfers)
	}
}

// newFundedAccount creates an account and funds it in one step.
func newFundedAccount(t *testing.T, e *env, currency money.Currency, minor int64) account.Account {
	t.Helper()

	acct := newAccount(t, e, currency)
	if minor > 0 {
		fund(t, e, acct.ID, minor)
	}
	acct.BalanceMinor = minor
	return acct
}

func newAccount(t *testing.T, e *env, currency money.Currency) account.Account {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	acct, err := e.accounts.Create(ctx, currency)
	if err != nil {
		t.Fatalf("creating %s account: %v", currency, err)
	}
	return acct
}

// balanceOf reads an account's current balance straight from the database.
func balanceOf(t *testing.T, e *env, id uuid.UUID) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	acct, err := e.accounts.Get(ctx, id)
	if err != nil {
		t.Fatalf("reading account %s: %v", id, err)
	}
	return acct.BalanceMinor
}

// totalBalances sums every account balance, which is what the
// conservation-of-money test measures.
func totalBalances(t *testing.T, e *env) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var total int64
	err := e.pool.QueryRow(ctx, `SELECT COALESCE(SUM(balance_minor), 0) FROM accounts`).Scan(&total)
	if err != nil {
		t.Fatalf("summing balances: %v", err)
	}
	return total
}

// countRows returns the number of rows in a table, for asserting that a failed
// operation left nothing behind.
func countRows(t *testing.T, e *env, table string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var n int
	// table is a constant supplied by test code, never external input.
	if err := e.pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
