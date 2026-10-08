// Package database provides PostgreSQL database connection and migration utilities.
package database

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // Register PostgreSQL driver

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
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

// RunMigrations runs the control plane's migrations through the shared
// migrationrunner, which refuses a dirty recorded version (#3587 PR-0).
func RunMigrations(db *sql.DB) error {
	return migrationrunner.Run(db, "file://migrations")
}
