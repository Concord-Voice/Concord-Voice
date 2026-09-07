// Package database provides PostgreSQL database connection and migration utilities.
package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Register file source driver for migrations
	_ "github.com/lib/pq"                                // Register PostgreSQL driver
)

// New creates a new PostgreSQL database connection
func New(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	return db, nil
}

// RunMigrations runs database migrations using golang-migrate.
func RunMigrations(db *sql.DB) error {
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("could not create migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		"file://migrations",
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

	// Run all pending migrations
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("could not run migrations: %w", err)
	}

	return nil
}
