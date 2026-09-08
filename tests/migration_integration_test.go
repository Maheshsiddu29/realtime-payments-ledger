//go:build integration

package tests

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
)

// Migrations must be reversible. This runs up, down to nothing, and up again
// against a database of its own, so a broken down-migration is caught here
// rather than during an incident.
func TestMigrationsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg, err := testConfig()
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	// A dedicated database: the shared test schema must not be torn down
	// underneath the other tests.
	cfg.Postgres.Database = cfg.Postgres.Database + "_roundtrip"

	if err := ensureDatabase(ctx, cfg); err != nil {
		t.Fatalf("creating the round-trip database: %v", err)
	}

	m, closeFn, err := openMigrator(cfg)
	if err != nil {
		t.Fatalf("opening the migrator: %v", err)
	}
	defer closeFn()

	// Start from a known-empty schema.
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("initial down: %v", err)
	}

	assertVersion := func(step string, want uint) {
		t.Helper()
		version, dirty, err := m.Version()
		if errors.Is(err, migrate.ErrNilVersion) {
			version = 0
			err = nil
		}
		if err != nil {
			t.Fatalf("%s: reading version: %v", step, err)
		}
		if dirty {
			t.Fatalf("%s: schema is dirty at version %d", step, version)
		}
		if version != want {
			t.Errorf("%s: version = %d, want %d", step, version, want)
		}
	}

	assertVersion("after initial down", 0)

	// Derived from the directory rather than hard-coded, so adding a migration
	// does not require editing this test.
	latest := latestMigrationVersion(t)

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertVersion("after up", latest)
	assertTables(t, ctx, cfg.Postgres.DSN(), true)

	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertVersion("after down", 0)
	assertTables(t, ctx, cfg.Postgres.DSN(), false)

	if err := m.Up(); err != nil {
		t.Fatalf("second up: %v", err)
	}
	assertVersion("after second up", latest)
	assertTables(t, ctx, cfg.Postgres.DSN(), true)
}

// latestMigrationVersion returns the highest version present in the migrations
// directory, so this test tracks the schema instead of a hard-coded number.
func latestMigrationVersion(t *testing.T) uint {
	t.Helper()

	entries, err := filepath.Glob(filepath.Join(migrationsPath, "*.up.sql"))
	if err != nil {
		t.Fatalf("listing migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no migrations found in %s", migrationsPath)
	}

	var latest uint
	for _, entry := range entries {
		name := filepath.Base(entry)
		digits, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %q does not start with a version prefix", name)
		}
		version, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has a non-numeric version prefix: %v", name, err)
		}
		latest = max(latest, uint(version))
	}
	return latest
}

// assertTables checks that the ledger tables are present or absent, and that
// down migrations also clean up the functions they created — a leftover
// function would make the next up migration fail.
func assertTables(t *testing.T, ctx context.Context, dsn string, want bool) {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	for _, table := range []string{"accounts", "transfers", "ledger_entries"} {
		var exists bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = $1)`,
			table).Scan(&exists)
		if err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if exists != want {
			t.Errorf("table %s exists = %t, want %t", table, exists, want)
		}
	}

	var functions int
	err = conn.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'
		  AND p.proname IN ('set_updated_at', 'forbid_completed_transfer_mutation',
		                    'forbid_ledger_entry_mutation', 'assert_transfer_balanced',
		                    'ledger_entries_assert_balanced', 'transfers_assert_balanced')`).Scan(&functions)
	if err != nil {
		t.Fatalf("counting functions: %v", err)
	}

	wantFunctions := 0
	if want {
		wantFunctions = 6
	}
	if functions != wantFunctions {
		t.Errorf("%d ledger functions present, want %d", functions, wantFunctions)
	}
}
