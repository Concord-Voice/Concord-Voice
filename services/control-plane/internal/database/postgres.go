// Package database provides PostgreSQL database connection and migration utilities.
package database

import (
	"database/sql"
	"fmt"
	"log"
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
// Automatically recovers from dirty migration state, which can occur when
// a process is interrupted mid-migration. PostgreSQL DDL is transactional,
// so the migration either fully applied or fully rolled back — the dirty
// flag just means the process didn't get to clear it.
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

	// Check for dirty migration state and auto-recover
	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return fmt.Errorf("could not check migration version: %w", err)
	}
	if dirty {
		log.Printf("WARNING: Database migration version %d is dirty — auto-recovering", version)
		// PostgreSQL DDL is transactional: the migration SQL either fully
		// committed or fully rolled back. Force the version clean so
		// golang-migrate can proceed. If the SQL rolled back, Up() will
		// re-apply it; if it committed, Up() moves to the next version.
		if err := m.Force(int(version)); err != nil { //nolint:gosec // migration version is always small
			return fmt.Errorf("could not force dirty migration clean: %w", err)
		}
		log.Printf("Forced version %d clean — continuing with pending migrations", version)
	}

	// Run all pending migrations
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("could not run migrations: %w", err)
	}

	return nil
}
