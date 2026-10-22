// Package store is the account authority's database and commit core (#3587
// PR-B1, DoR §3.2). It has no caller until PR-B2.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
)

// MigrationFiles embeds the authority's migrations (B1-D1). A test pins it to
// the directory; B2's binary feeds it to the runner through MigrateEmbedded.
//
//go:embed migrations/*.sql
var MigrationFiles embed.FS

// SchemaVersion is the newest migration this code knows.
const SchemaVersion uint = 1

// The refusals CheckSchema wraps.
var (
	ErrSchemaDirty = errors.New("authority store: schema is dirty")
	ErrSchemaSkew  = errors.New("authority store: schema version differs from the code")
)

// Migrate applies the authority's migrations through the shared runner, which
// refuses a dirty recorded version (D-209: no copy, no parity test).
func Migrate(db *sql.DB, sourceURL string) error {
	return migrationrunner.Run(db, sourceURL)
}

// MigrateEmbedded applies MigrationFiles through the shared runner: the
// binary carries its own migrations (B1-D1, B2-D1).
func MigrateEmbedded(db *sql.DB) error {
	src, err := iofs.New(MigrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("authority store: embedded migrations: %w", err)
	}
	return migrationrunner.RunSource(db, src)
}

// CheckSchema refuses a dirty, missing or skewed schema in either direction
// (D-33). B2's `serve` calls it at startup.
func CheckSchema(ctx context.Context, db *sql.DB) error {
	var version int64
	var dirty bool
	err := db.QueryRowContext(ctx, `SELECT version, dirty FROM authority.schema_migrations`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no recorded version, code is at %d", ErrSchemaSkew, SchemaVersion)
	}
	if err != nil {
		return fmt.Errorf("authority store: read schema version: %w", err)
	}
	if dirty {
		return fmt.Errorf("%w at version %d", ErrSchemaDirty, version)
	}
	if version != int64(SchemaVersion) {
		return fmt.Errorf("%w: database %d, code %d", ErrSchemaSkew, version, SchemaVersion)
	}
	return nil
}
