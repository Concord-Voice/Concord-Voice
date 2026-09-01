package media

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errDeleter is a minimal ObjectDeleter that returns a configurable error.
type errDeleter struct {
	called bool
	err    error
}

type cleanupExecDB struct {
	RowQuerier
	result sql.Result
	err    error
}

func (d cleanupExecDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return d.result, d.err
}

func (e *errDeleter) DeleteObject(_ context.Context, _ string) error {
	e.called = true
	return e.err
}

// setupTestDB is defined in handlers_test.go (same package) — reused here to
// avoid the import-cycle that testhelpers → api → media would create.

func TestCleanupObjectNilStoreReturnsNilWithoutLiveRow(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// nil store and no live row → nothing to clean up.
	assert.NoError(t, CleanupObject(context.Background(), db, nil, "nonexistent/key"))
}

func TestCleanupObjectStoreErrorPreservesMetadata(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	storageKey := "test-media/" + uuid.New().String()
	userID := uuid.New().String()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID, userID+"@test.concord.chat", "cleanuperr"+userID[:8],
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 1024, $2)`,
		userID, storageKey,
	)
	require.NoError(t, err)

	store := &errDeleter{err: errors.New("storage backend unavailable")}

	assert.Error(t, CleanupObject(ctx, db, store, storageKey))
	assert.True(t, store.called, "DeleteObject should have been called")
	var deletedAt *time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "metadata must remain live when storage deletion fails")
}

func TestCleanupObjectStoreSuccessNilError(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := &errDeleter{err: nil}

	assert.NoError(t, CleanupObject(context.Background(), db, store, "some/key"))
	assert.True(t, store.called)
}

func TestCleanupObjectCancelledContextReturnsError(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.Error(t, CleanupObject(ctx, db, nil, "some/key"))
}

func TestCleanupObjectSuccessSoftDeletesRow(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	storageKey := "test-media/" + uuid.New().String()

	userID := uuid.New().String()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID,
		userID+"@test.concord.chat",
		"cleanupuser"+userID[:8],
	)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 1024, $2)`,
		userID, storageKey,
	)
	require.NoError(t, err)

	store := &errDeleter{err: nil}
	assert.NoError(t, CleanupObject(ctx, db, store, storageKey))

	var deletedAt *time.Time
	err = db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`,
		storageKey,
	).Scan(&deletedAt)
	require.NoError(t, err)
	assert.NotNil(t, deletedAt, "deleted_at should be set after CleanupObject")
}

func TestCleanupObjectZeroRowsPreservesLiveMetadata(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	ctx := context.Background()
	storageKey := "test-media/" + uuid.New().String()
	userID := uuid.New().String()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID, userID+"@test.concord.chat", "cleanupzero"+userID[:8],
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 1024, $2)`,
		userID, storageKey,
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		CREATE FUNCTION test_cleanup_skip_update() RETURNS trigger AS $$
		BEGIN
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_cleanup_skip_update
			BEFORE UPDATE OF deleted_at ON media_files
			FOR EACH ROW EXECUTE FUNCTION test_cleanup_skip_update();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.ExecContext(ctx, `
			DROP TRIGGER IF EXISTS test_cleanup_skip_update ON media_files;
			DROP FUNCTION IF EXISTS test_cleanup_skip_update();
		`)
		require.NoError(t, cleanupErr)
	})

	assert.Error(t, CleanupObject(ctx, db, nil, storageKey))
	var deletedAt *time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "metadata must remain live when the CAS updates zero rows")
}

func TestCleanupObjectAlreadyDeletedIsIdempotent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	storageKey := "test-media/" + uuid.New().String()

	userID := uuid.New().String()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID,
		userID+"@test.concord.chat",
		"cleanupuser2"+userID[:8],
	)
	require.NoError(t, err)

	// Row already soft-deleted — WHERE deleted_at IS NULL won't match it
	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, deleted_at)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 1024, $2, NOW())`,
		userID, storageKey,
	)
	require.NoError(t, err)

	assert.NoError(t, CleanupObject(ctx, db, nil, storageKey))
}

// TestCleanupObjectRefusesNonLegacyBackend is the reason CleanupObject was
// changed at all (ADR-0038 / #2759), and it had no test.
//
// This is the ONE placement decision in the codebase with no resolver behind
// it: CleanupObject is reached from avatar/banner and server-icon replacement,
// takes a bare ObjectDeleter, and uses isLegacyBackend as its sole guard. The
// function's own contract states nothing retries behind it, so deleting from
// the wrong bucket is unrecoverable — and an S3 DELETE of a key absent from the
// target bucket returns SUCCESS, which would then soft-delete the row and
// record an erasure that never happened.
//
// Both assertions are load-bearing: the object must not be deleted AND the row
// must not be marked. Asserting only one leaves the other free to regress.
func TestCleanupObjectRefusesNonLegacyBackend(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	storageKey := "test-media/" + uuid.New().String()

	userID := uuid.New().String()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID, userID+"@test.concord.chat", "cleanupnl"+userID[:8],
	)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, storage_backend)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 1024, $2, 'r2-useast')`,
		userID, storageKey,
	)
	require.NoError(t, err)

	store := &errDeleter{err: nil}
	assert.Error(t, CleanupObject(ctx, db, store, storageKey))

	assert.False(t, store.called,
		"a vendor-resident object must NOT be deleted from the legacy store")

	var deletedAt *time.Time
	err = db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt)
	require.NoError(t, err)
	assert.Nil(t, deletedAt,
		"refusing to delete must also refuse to record the erasure")
}

// TestCleanupObjectProceedsForLegacyBackend is the positive control for the
// test above: it proves the same seeding path CAN reach the delete-and-mark
// outcome, so a "did not delete" assertion means the guard fired rather than
// the fixture never getting that far.
func TestCleanupObjectProceedsForLegacyBackend(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	storageKey := "test-media/" + uuid.New().String()

	userID := uuid.New().String()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID, userID+"@test.concord.chat", "cleanuplg"+userID[:8],
	)
	require.NoError(t, err)

	// storage_backend deliberately left NULL — the permanent legacy spelling.
	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 1024, $2)`,
		userID, storageKey,
	)
	require.NoError(t, err)

	store := &errDeleter{err: nil}
	assert.NoError(t, CleanupObject(ctx, db, store, storageKey))

	assert.True(t, store.called, "a NULL-backend object is legacy and must be deleted")

	var deletedAt *time.Time
	err = db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt)
	require.NoError(t, err)
	assert.NotNil(t, deletedAt)
}

func TestTerminalizeProfileSlotRejectsInvalidSlot(t *testing.T) {
	err := TerminalizeProfileSlot(context.Background(), nil, uuid.NewString(), "not-a-profile-slot")
	assert.ErrorContains(t, err, "invalid slot")
}

func TestTerminalizeProfileSlotReturnsInsertError(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	err = TerminalizeProfileSlot(ctx, tx, uuid.NewString(), ProfileSlotAvatar)
	assert.ErrorContains(t, err, "record deletion obligations")
}

func TestTerminalizeProfileSlotRefusesNonLegacyBackend(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := uuid.NewString()
	storageKey := "avatars/" + userID + "/" + uuid.NewString()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true)`,
		userID, userID+"@test.concord.chat", "terminalvendor"+userID[:8])
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, storage_backend, profile_slot)
		 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 4, $2, 'r2-useast', 'avatar')`, userID, storageKey)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	err = TerminalizeProfileSlot(ctx, tx, userID, ProfileSlotAvatar)
	assert.ErrorContains(t, err, "refused non-legacy backend")
	var deletedAt *time.Time
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt))
	assert.Nil(t, deletedAt)
	var obligations int
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key IN ($1, $2)`,
		storageKey, "avatars/"+userID).Scan(&obligations))
	assert.Zero(t, obligations)
}

func TestCleanupObjectDatabaseFailurePreservesMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result sql.Result
		err    error
	}{
		{name: "exec", err: errors.New("update failed")},
		{name: "rows affected", result: driver.ResultNoRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := setupTestDB(t)
			defer cleanup()

			ctx := context.Background()
			storageKey := "test-media/" + uuid.NewString()
			userID := uuid.NewString()
			_, err := db.ExecContext(ctx,
				`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
				 VALUES ($1, $2, $3, 'hash', true, true)`,
				userID, userID+"@test.concord.chat", "cleanupupdate"+userID[:8])
			require.NoError(t, err)
			_, err = db.ExecContext(ctx,
				`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
				 VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/jpeg', 4, $2)`, userID, storageKey)
			require.NoError(t, err)

			wrapped := cleanupExecDB{RowQuerier: db, result: tc.result, err: tc.err}
			assert.Error(t, CleanupObject(ctx, wrapped, nil, storageKey))
			var deletedAt *time.Time
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt))
			assert.Nil(t, deletedAt)
		})
	}
}
