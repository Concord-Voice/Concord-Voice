package migrationrunner_test

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// controlPlaneMigrations returns the control plane's migrations directory as a
// file:// URL, resolved from this file so the test needs no t.Chdir.
func controlPlaneMigrations(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return "file://" + filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

// freshSchemaDB returns a handle whose search_path is a throwaway schema, so the
// schema_migrations table and anything a migration creates land there and the
// shared test database (and a developer's dev database behind it) is untouched.
// The schema is dropped when the test ends.
func freshSchemaDB(t *testing.T) *sql.DB {
	t.Helper()

	// SetupTestDB first: it takes the shared test-database lock for this test.
	db, _ := testhelpers.SetupTestDB(t)

	schema := "migrationrunner_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test schema is quoted with pq.QuoteIdentifier
	_, err := db.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- fixed test schema is quoted with pq.QuoteIdentifier
		_, dropErr := db.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(schema) + ` CASCADE`)
		require.NoError(t, dropErr)
	})

	// A second handle whose search_path is the new schema.
	u, err := url.Parse(testdb.DatabaseURL())
	require.NoError(t, err)
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	fresh, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	// Registered after the drop above, so t.Cleanup (LIFO) closes it first.
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })

	// Guard the isolation itself: every unqualified statement a caller runs on
	// this handle (UPDATE, ALTER TABLE) must resolve inside the throwaway schema.
	var current string
	require.NoError(t, fresh.QueryRow(`SELECT current_schema()`).Scan(&current))
	require.Equal(t, schema, current, "the fresh handle must resolve to its own schema")

	return fresh
}

// freshSource writes a one-migration source (version 1, creating fresh_marker)
// and returns its file:// URL.
func freshSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1_fresh.up.sql"), []byte("CREATE TABLE fresh_marker (id integer);\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1_fresh.down.sql"), []byte("DROP TABLE fresh_marker;\n"), 0o600))
	return "file://" + dir
}

func TestRun_DirtyDatabaseFailsClosed(t *testing.T) {
	// A throwaway schema: marking the recorded version dirty here must never
	// touch the shared test database.
	db := freshSchemaDB(t)
	source := freshSource(t)
	require.NoError(t, migrationrunner.Run(db, source))

	result, err := db.Exec(`UPDATE schema_migrations SET dirty = TRUE`)
	require.NoError(t, err)
	rowsAffected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	err = migrationrunner.Run(db, source)
	// Pinned byte for byte: the control plane surfaces this text to operators,
	// and #3587 PR-0 requires the move to be behaviour-preserving.
	require.EqualError(t, err, "database migration version 1 is dirty; inspect and repair migration 1 before clearing its state, then rerun migrations (use migrate -command=force -force-version=<verified version> only after verification)")

	var observedVersion int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&observedVersion, &dirty))
	require.Equal(t, 1, observedVersion)
	require.True(t, dirty, "Run must refuse without touching the dirty row")
}

func TestRun_CleanMigratedDatabaseIsNoChange(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)

	var before int
	require.NoError(t, db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&before))

	require.NoError(t, migrationrunner.Run(db, controlPlaneMigrations(t)))

	var after int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&after, &dirty))
	require.Equal(t, before, after)
	require.False(t, dirty)
}

func TestRun_UnreadableSourceFails(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)

	err := migrationrunner.Run(db, "file://"+filepath.Join(t.TempDir(), "does-not-exist"))
	require.ErrorContains(t, err, "could not create migrate instance")
}

func TestRun_ClosedDatabaseHandleFails(t *testing.T) {
	// sql.Open is lazy, so this never dials; closing the handle makes the
	// driver's initial ping fail without needing a reachable server.
	db, err := sql.Open("postgres", "postgres://127.0.0.1:1/unreachable?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	err = migrationrunner.Run(db, controlPlaneMigrations(t))
	require.ErrorContains(t, err, "could not create migration driver")
}

// TestRun_UnreadableVersionFails covers the version check that is neither a
// fresh schema nor a dirty one: golang-migrate cannot read the recorded
// version, so Run must stop there with its own error rather than guess.
func TestRun_UnreadableVersionFails(t *testing.T) {
	db := freshSchemaDB(t)
	source := freshSource(t)
	require.NoError(t, migrationrunner.Run(db, source))

	// The driver's version read selects the dirty column, so dropping it makes
	// m.Version() fail while the table itself still exists.
	_, err := db.Exec(`ALTER TABLE schema_migrations DROP COLUMN dirty`)
	require.NoError(t, err)

	err = migrationrunner.Run(db, source)
	require.ErrorContains(t, err, "could not check migration version")
}

// TestRun_FreshSchemaAppliesSource covers the first-boot path: a schema with no
// schema_migrations row makes golang-migrate report ErrNilVersion, which Run
// must treat as "nothing applied yet" and not as a failed version check.
func TestRun_FreshSchemaAppliesSource(t *testing.T) {
	fresh := freshSchemaDB(t)

	require.NoError(t, migrationrunner.Run(fresh, freshSource(t)))

	var version int
	var dirty bool
	require.NoError(t, fresh.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	require.Equal(t, 1, version)
	require.False(t, dirty)

	var markerExists bool
	require.NoError(t, fresh.QueryRow(`SELECT to_regclass(quote_ident(current_schema()) || '.fresh_marker') IS NOT NULL`).Scan(&markerExists))
	require.True(t, markerExists, "the first migration must have created fresh_marker in the new schema")
}

func TestRun_UpFailureIsReported(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)

	var versionBefore int
	require.NoError(t, db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&versionBefore))

	// A source that does not contain the database's recorded version makes
	// m.Up() fail before it applies anything (golang-migrate readUp looks the
	// current version up in the source), so this writes nothing.
	dir := t.TempDir()
	for _, name := range []string{"1_only.up.sql", "1_only.down.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;\n"), 0o600))
	}

	err := migrationrunner.Run(db, "file://"+dir)
	require.ErrorContains(t, err, "could not run migrations")

	var versionAfter int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&versionAfter, &dirty))
	require.Equal(t, versionBefore, versionAfter, "a refused Up must not move the recorded version")
	require.False(t, dirty, "a refused Up must not mark the database dirty")
}
