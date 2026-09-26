//go:build integration

package purge_test

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// TestMessagePurgeClearReasonMigrationRoundTrip pins 000155/000156 (#3462):
// the widened reason CHECK admits 'clear', the down refuses while clear
// evidence exists, and a clean down restores pg_constraint exactly — the
// definition and convalidated both.
func TestMessagePurgeClearReasonMigrationRoundTrip(t *testing.T) {
	db, m := newDisposableClearReasonMigrationDB(t)
	// Reach the version just below 000155 by migrating UP from empty. Stepping
	// back from 156 would run 156's own down, and the baseline would then
	// compare the down against itself. The prior version is read from the
	// migrations directory so no neighbour's number is hard-coded.
	require.NoError(t, m.Migrate(priorMigrationVersion(t, clearReasonUp)), "migrate to the pre-clear-reason schema")
	before := clearReasonConstraint(t, db)

	require.NoError(t, m.Migrate(clearReasonValidate), "000155-000156 must apply")
	require.Contains(t, clearReasonConstraint(t, db), "validated=true")
	insertPurge := func(reason string) (string, error) {
		var id string
		err := db.QueryRow(`INSERT INTO message_purges (context_type, context_id, reason, status)
			VALUES ('dm', $1, $2, 'completed') RETURNING id`, uuid.NewString(), reason).Scan(&id)
		return id, err
	}
	clearID, err := insertPurge("clear")
	require.NoError(t, err)
	_, err = insertPurge("expiry")
	require.NoError(t, err, "a four-value writer still works")

	require.NoError(t, m.Steps(-1), "000156 down")
	require.Contains(t, clearReasonConstraint(t, db), "validated=false")
	err = m.Steps(-1)
	require.ErrorContains(t, err, "refusing to drop clear purge audit evidence")
	var evidence int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_purges WHERE id = $1`, clearID).Scan(&evidence))
	require.Equal(t, 1, evidence, "a refused downgrade must leave clear evidence intact")
	// Reset the dirty marker only after asserting preservation, as the 000130
	// round trip does (internal/expiration/migration_integration_test.go).
	_, err = db.Exec(`UPDATE schema_migrations SET version = $1, dirty = FALSE`, clearReasonUp)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM message_purges WHERE id = $1`, clearID)
	require.NoError(t, err)

	require.NoError(t, m.Steps(-1), "000155 down succeeds once clear evidence is gone")
	require.Equal(t, before, clearReasonConstraint(t, db), "down restores pg_constraint exactly")
	_, err = insertPurge("clear")
	require.Error(t, err, "after down, 'clear' is rejected again")
}

// The two migrations under test. Renumber here if they move at merge time.
const (
	clearReasonUp       uint = 155
	clearReasonValidate uint = 156
)

// priorMigrationVersion returns the highest migration version below v.
func priorMigrationVersion(t *testing.T, v uint) uint {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	ups, err := filepath.Glob(filepath.Join(filepath.Dir(filename), "..", "..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	var prior uint
	for _, path := range ups {
		n, convErr := strconv.ParseUint(strings.SplitN(filepath.Base(path), "_", 2)[0], 10, 64)
		require.NoError(t, convErr)
		if uint(n) < v && uint(n) > prior {
			prior = uint(n)
		}
	}
	require.NotZero(t, prior)
	return prior
}

// clearReasonConstraint returns the constraint definition plus convalidated,
// so a down that leaves the constraint NOT VALID is caught.
func clearReasonConstraint(t *testing.T, db *sql.DB) string {
	t.Helper()
	var def string
	require.NoError(t, db.QueryRow(`
		SELECT pg_get_constraintdef(oid) || ' validated=' || convalidated
		FROM pg_constraint
		WHERE conrelid = 'message_purges'::regclass AND conname = 'message_purges_reason_check'`).Scan(&def))
	return def
}

// newDisposableClearReasonMigrationDB creates a throwaway database so the
// round trip never moves the shared test database's schema version. Copied
// from runIsolatedMigrationRoundTrip in internal/expiration.
func newDisposableClearReasonMigrationDB(t *testing.T) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	baseURL := testdb.DatabaseURL()
	parsed, err := url.Parse(baseURL)
	require.NoError(t, err)
	databaseName := "concord_clear_reason_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pq.QuoteIdentifier(databaseName)
	adminDB, err := sql.Open("postgres", baseURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
		if _, cleanupErr := adminDB.Exec(`DROP DATABASE IF EXISTS ` + quotedName + ` WITH (FORCE)`); cleanupErr != nil {
			t.Errorf("drop disposable database: %v", cleanupErr)
		}
		if cleanupErr := adminDB.Close(); cleanupErr != nil {
			t.Errorf("close disposable database admin connection: %v", cleanupErr)
		}
	})
	require.NoError(t, adminDB.Ping())
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
	_, err = adminDB.Exec(`CREATE DATABASE ` + quotedName)
	require.NoError(t, err)

	parsed.Path = "/" + databaseName
	db, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		if cleanupErr := db.Close(); cleanupErr != nil {
			t.Errorf("close disposable migration database: %v", cleanupErr)
		}
	})
	require.NoError(t, db.Ping())

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	migrationDir := filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationDir, "postgres", driver)
	require.NoError(t, err)
	t.Cleanup(func() {
		sourceErr, databaseErr := m.Close()
		if sourceErr != nil {
			t.Errorf("close disposable migration source: %v", sourceErr)
		}
		if databaseErr != nil {
			t.Errorf("close disposable migration database driver: %v", databaseErr)
		}
	})
	return db, m
}
