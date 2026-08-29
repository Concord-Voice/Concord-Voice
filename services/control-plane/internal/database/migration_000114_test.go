package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000114 (ADR-0038 / #2759) adds media_files.storage_backend: a
// nullable TEXT column with no DEFAULT, no CHECK, and no index. NULL means
// "the configured legacy backend" (MinIO) permanently, not a not-yet-backfilled
// gap. These tests pin that exact shape against a real database — not the SQL
// text — so a later "helpful" NOT NULL, DEFAULT, CHECK, or index reopens the
// mixed-version-deploy hazard the migration's header comment documents and
// fails loudly here instead of silently.

// TestMigration000114_ColumnIsNullableTextNoDefault locks the column's catalog
// shape. If 000114 had never run, this query returns zero rows and Scan fails
// with sql.ErrNoRows — the test cannot pass against a database missing the
// migration.
func TestMigration000114_ColumnIsNullableTextNoDefault(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	var dataType, isNullable string
	var columnDefault sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'media_files' AND column_name = 'storage_backend'`,
	).Scan(&dataType, &isNullable, &columnDefault))

	assert.Equal(t, "text", dataType)
	assert.Equal(t, "YES", isNullable,
		"storage_backend must stay nullable: NULL is the permanent legacy-backend sentinel, not a gap to backfill")
	assert.False(t, columnDefault.Valid,
		"no DEFAULT: a default would silently redefine what an omitted column means for new rows written by an old replica")
}

// TestMigration000114_NoIndexOnStorageBackend locks the documented decision to
// omit an index: the only anticipated read rides the existing primary-key
// lookup on media_files.id. If a later change adds one without naming a query
// that needs it, this test is the tripwire.
func TestMigration000114_NoIndexOnStorageBackend(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM pg_indexes
		WHERE tablename = 'media_files' AND indexdef ILIKE '%storage_backend%'`,
	).Scan(&count))
	assert.Zero(t, count,
		"no identified query scans/filters media_files by storage_backend alone; add an index only when one appears, and name it then")
}

// TestMigration000114_InsertWithoutStorageBackendReadsBackNull models an old
// replica's INSERT (one that has never heard of this column) and the row this
// column applies to for every pre-cutover object. The query fails outright
// with "column does not exist" if the migration is missing, so a passing
// result is direct evidence the column exists AND defaults to NULL.
func TestMigration000114_InsertWithoutStorageBackendReadsBackNull(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	uploaderID := testhelpers.CreateUser(t, db)

	var storageBackend sql.NullString
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		VALUES ($1, 'photo', 1, 'image/png', 1, 'avatars/' || gen_random_uuid()::text)
		RETURNING storage_backend`, uploaderID).Scan(&storageBackend))

	assert.False(t, storageBackend.Valid,
		"a row inserted without naming storage_backend must read back NULL, meaning 'the configured legacy backend'")
}

// TestMigration000114_AcceptsArbitraryBackendIdentifier proves there is no
// CHECK constraint enumerating backend identifiers. ADR-0038 puts the valid
// set in an application-side boot-time registry precisely so a future backend
// (the ADR names EU / Indo-Pacific candidates) needs no migration; a CHECK
// added later to "tidy this up" would fail this test on the very first
// identifier it does not yet know about.
func TestMigration000114_AcceptsArbitraryBackendIdentifier(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	uploaderID := testhelpers.CreateUser(t, db)

	for _, backend := range []string{"r2", "minio", "eu-r2-candidate", "not-a-real-backend-yet"} {
		t.Run(backend, func(t *testing.T) {
			var got sql.NullString
			require.NoError(t, db.QueryRow(`
				INSERT INTO media_files (uploader_id, file_type, media_tier, mime_type, file_size, storage_key, storage_backend)
				VALUES ($1, 'photo', 1, 'image/png', 1, 'avatars/' || gen_random_uuid()::text || '/' || $2, $2)
				RETURNING storage_backend`, uploaderID, backend).Scan(&got))
			require.True(t, got.Valid)
			assert.Equal(t, backend, got.String,
				"the backend registry lives in application code, not a DB enum: no identifier should be rejected here")
		})
	}
}

// TestMigration000114_UpdateChangesPlacementPerObject exercises the column's
// actual purpose: placement is set per object, independent of every other row.
func TestMigration000114_UpdateChangesPlacementPerObject(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	uploaderID := testhelpers.CreateUser(t, db)

	var id string
	require.NoError(t, db.QueryRow(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		VALUES ($1, 'photo', 1, 'image/png', 1, 'avatars/' || gen_random_uuid()::text)
		RETURNING id`, uploaderID).Scan(&id))

	_, err := db.Exec(`UPDATE media_files SET storage_backend = 'r2' WHERE id = $1`, id)
	require.NoError(t, err)

	var backend sql.NullString
	require.NoError(t, db.QueryRow(`SELECT storage_backend FROM media_files WHERE id = $1`, id).Scan(&backend))
	require.True(t, backend.Valid)
	assert.Equal(t, "r2", backend.String, "placement must be settable on one object without touching any other row")
}

// TestMigration000114_DownDropsColumnUpRestoresIt proves the down migration
// actually reverses the up migration against a real database (not merely a
// read of the SQL text), and that re-applying up is idempotent, matching the
// IF NOT EXISTS in the up migration. Run inside a rolled-back transaction so
// it never leaves the shared test schema downgraded for the rest of the
// package.
func TestMigration000114_DownDropsColumnUpRestoresIt(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	downSQL := migrationReadFile(t, "../../migrations/000114_media_files_storage_backend.down.sql")
	upSQL := migrationReadFile(t, "../../migrations/000114_media_files_storage_backend.up.sql")

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	columnExists := func(t *testing.T, tx *sql.Tx) bool {
		t.Helper()
		var exists bool
		require.NoError(t, tx.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'media_files' AND column_name = 'storage_backend'
			)`).Scan(&exists))
		return exists
	}

	require.True(t, columnExists(t, tx), "precondition: 000114's up migration must already be applied by SetupTestDB")

	_, err = tx.Exec(downSQL)
	require.NoError(t, err)
	assert.False(t, columnExists(t, tx), "the down migration is the entire reversal — there is no CHECK or index to also drop")

	_, err = tx.Exec(upSQL)
	require.NoError(t, err)
	assert.True(t, columnExists(t, tx), "re-applying the up migration (ADD COLUMN IF NOT EXISTS) must be idempotent")
}
