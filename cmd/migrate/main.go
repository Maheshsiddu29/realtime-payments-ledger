// Command migrate applies and reverses database schema migrations.
//
// It is a separate binary on purpose. The API process never migrates on
// start-up: doing so makes several replicas race to change the schema, ties
// the schema version to whichever build happens to boot first, and hides a
// destructive operation inside a routine restart. Migrations are an explicit,
// operator-run step.
//
// Usage:
//
//	migrate up             apply all pending migrations
//	migrate down           roll back exactly one migration
//	migrate down-all       roll back every migration (destroys all data)
//	migrate version        print the current schema version
//	migrate force <ver>    mark the schema as being at <ver> without running it
//
// The database connection comes from the same POSTGRES_* environment
// variables the API uses; see docs/configuration.md.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
)

func main() {
	path := flag.String("path", "migrations", "directory containing the migration files")
	flag.Usage = usage
	flag.Parse()

	if err := run(*path, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: migrate [-path dir] <command>

Commands:
  up             apply all pending migrations
  down           roll back exactly one migration
  down-all       roll back every migration (destroys all data)
  version        print the current schema version
  force <ver>    mark the schema as being at <ver> without running it

`)
	flag.PrintDefaults()
}

func run(path string, args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	m, closeFn, err := open(cfg, path)
	if err != nil {
		return err
	}
	defer closeFn()

	switch cmd := args[0]; cmd {
	case "up":
		return report("up", m.Up(), m)

	case "down":
		// One step, not everything: an unqualified "down" that destroys the
		// entire schema is too easy to run by accident.
		return report("down 1", m.Steps(-1), m)

	case "down-all":
		return report("down-all", m.Down(), m)

	case "version":
		version, dirty, err := m.Version()
		if errors.Is(err, migrate.ErrNilVersion) {
			fmt.Println("no migrations applied")
			return nil
		}
		if err != nil {
			return fmt.Errorf("read version: %w", err)
		}
		fmt.Printf("version %d (dirty: %t)\n", version, dirty)
		return nil

	case "force":
		if len(args) < 2 {
			return errors.New("force requires a version, e.g. 'migrate force 3'")
		}
		version, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("force: %q is not a version number", args[1])
		}
		if err := m.Force(version); err != nil {
			return fmt.Errorf("force %d: %w", version, err)
		}
		fmt.Printf("schema forced to version %d\n", version)
		return nil

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// open builds a migrator over a database/sql handle.
//
// The migration runner uses database/sql rather than pgxpool because that is
// what golang-migrate's driver expects. It is also the one place where
// POSTGRES_MAX_IDLE_CONNS applies: pgxpool has no equivalent setting.
func open(cfg config.Config, path string) (*migrate.Migrate, func(), error) {
	db, err := sql.Open("pgx/v5", cfg.Postgres.DSN())
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", cfg.Postgres.RedactedDSN(), err)
	}

	db.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("connect to %s: %w", cfg.Postgres.RedactedDSN(), err)
	}

	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("build migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance("file://"+path, "pgx5", driver)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("load migrations from %s: %w", path, err)
	}

	return m, func() { _ = db.Close() }, nil
}

// report turns a migrate result into human output. ErrNoChange is a normal,
// successful outcome: it means the schema was already where it should be.
func report(action string, err error, m *migrate.Migrate) error {
	switch {
	case errors.Is(err, migrate.ErrNoChange):
		fmt.Printf("%s: no change\n", action)
	case err != nil:
		return fmt.Errorf("%s: %w", action, err)
	default:
		fmt.Printf("%s: applied\n", action)
	}

	version, dirty, verr := m.Version()
	if errors.Is(verr, migrate.ErrNilVersion) {
		fmt.Println("schema version: none")
		return nil
	}
	if verr != nil {
		return fmt.Errorf("read version after %s: %w", action, verr)
	}
	fmt.Printf("schema version: %d (dirty: %t)\n", version, dirty)
	return nil
}
