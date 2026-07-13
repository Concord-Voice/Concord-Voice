// Package testhelpers provides test utilities for integration tests.
package testhelpers

import (
	"database/sql"
	"testing"

	dbtest "github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
)

// SetupTestDB preserves the shared testhelpers API while delegating database
// setup to the cycle-free fixture package used by domain-package tests.
func SetupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	return dbtest.SetupTestDB(t)
}

// TruncateAllTables preserves the shared testhelpers API.
func TruncateAllTables(db *sql.DB) error {
	return dbtest.TruncateAllTables(db)
}
