// Package migrationrunner applies golang-migrate migrations and refuses a dirty
// recorded version (#3587 PR-0). The control plane's database.RunMigrations
// calls it.
package migrationrunner

import (
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Register the file:// migration source.
)

// Run applies every pending migration from sourceURL to db. It refuses a dirty
// recorded version and leaves it untouched for an operator to repair.
//
// Run does not close db or the migrate instance (whose Close would close db),
// so each call pins one pooled connection for the life of the process. Only
// file:// is registered, and sourceURL must be a trusted constant.
//
// postgres.WithInstance waits on golang-migrate's advisory lock with no
// timeout, so a process that starts while another runner is migrating blocks,
// with no log line, until that runner finishes, then boots or refuses on the
// version it left. The dirty check reads without the lock, so a runner that
// starts between WithInstance and that read makes this process refuse even if
// the runner then succeeds. The version table and the lock key come from the
// connection's current schema, so each caller needs its own database or schema.
func Run(db *sql.DB, sourceURL string) error {
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("could not create migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		sourceURL,
		"postgres",
		driver,
	)
	if err != nil {
		return fmt.Errorf("could not create migrate instance: %w", err)
	}

	// A dirty migration may have committed non-transactional or concurrent
	// objects before the runner failed. Refuse to guess whether the recorded
	// version is safe to replay; an operator must inspect and repair it first.
	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return fmt.Errorf("could not check migration version: %w", err)
	}
	if dirty {
		return fmt.Errorf("database migration version %d is dirty; inspect and repair migration %d before clearing its state, then rerun migrations (use migrate -command=force -force-version=<verified version> only after verification)", version, version)
	}

	// Run all pending migrations. The != is deliberate: golang-migrate joins an
	// unlock failure onto ErrNoChange, and errors.Is would swallow it.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("could not run migrations: %w", err)
	}

	return nil
}
