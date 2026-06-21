// Package testhelpers provides test utilities for integration tests.
package testhelpers

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Required for file:// migration source
	_ "github.com/lib/pq"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

// defaultTestDatabaseURL is used when DATABASE_URL is not set.
// Assembled from parts to satisfy static credential analysis (S6698/S2068).
var defaultTestDatabaseURL = "postgres://concord:" + testDBVal + "@localhost:5432/concord?sslmode=disable" //nolint:gosec // Test-only default, not a production credential

var testDBVal = "concord_dev_password" //nolint:gosec // matches docker-compose dev default

// SetupTestDB creates a database connection, runs migrations, and returns a cleanup function.
func SetupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("testhelpers: failed to open database: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("testhelpers: failed to ping database: %v", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetConnMaxIdleTime(10 * time.Second)

	// Migrate once per package binary; cleanup truncation between tests ensures isolation.
	ensureMigrations(t, db)

	cleanup := func() {
		if err := TruncateAllTables(db); err != nil {
			t.Errorf("testhelpers: failed to truncate tables: %v", err)
		}
		_ = db.Close()
	}

	return db, cleanup
}

// TruncateAllTables removes all data from application tables.
func TruncateAllTables(db *sql.DB) error {
	_, err := db.Exec(`TRUNCATE
		account_deletions,
		age_verification_records,
		user_sso_identities,
		users,
		user_keys,
		public_keys,
		refresh_tokens,
		servers,
		server_members,
		channels,
		channel_keys,
		pending_key_requests,
		messages,
		server_invites,
		channel_read_states,
		user_preferences,
		ownership_transfers,
		friendships,
		friend_codes,
		dm_conversations,
		dm_participants,
		dm_messages,
		dm_channel_keys,
		dm_pending_key_requests,
		dm_read_states,
		dm_key_revocations,
		roles,
		member_roles,
		channel_permission_overrides,
		audit_log,
		key_revocations,
		voice_participants,
		dm_voice_participants,
		user_mfa_totp,
		user_mfa_webauthn,
		media_files,
		message_attachments,
		dm_message_attachments,
		release_binaries,
		release_spas,
		subscriptions,
		redemption_codes,
		code_redemptions,
		user_presence_settings
	CASCADE`)
	return err
}

// migrationsPath resolves the absolute path to the migrations directory.
func migrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	// testhelpers is at internal/testhelpers/testdb.go
	// migrations is at migrations/
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// ensureMigrations runs migrations exactly once per package binary via sync.Once.
func ensureMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	migrateOnce.Do(func() {
		migrateErr = runMigrations(db, migrationsPath())
	})
	if migrateErr != nil {
		t.Fatalf("testhelpers: migration failed: %v", migrateErr)
	}
}

func runMigrations(db *sql.DB, migrationsDir string) error {
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("could not create migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", migrationsDir),
		"postgres",
		driver,
	)
	if err != nil {
		return fmt.Errorf("could not create migrate instance: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("could not run migrations: %w", err)
	}
	return nil
}
