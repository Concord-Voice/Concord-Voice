package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errDeleter is a minimal ObjectDeleter that returns a configurable error.
type errDeleter struct {
	called bool
	err    error
}

func (e *errDeleter) DeleteObject(_ context.Context, _ string) error {
	e.called = true
	return e.err
}

// setupTestDB is defined in handlers_test.go (same package) — reused here to
// avoid the import-cycle that testhelpers → api → media would create.

func TestCleanupObjectNilStoreDoesNotPanic(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	log := logger.New("test")

	// nil store → skip storage step; DB UPDATE matches 0 rows which is not an error
	assert.NotPanics(t, func() {
		CleanupObject(context.Background(), db, nil, log, "nonexistent/key")
	})
}

func TestCleanupObjectStoreErrorContinuesToDB(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	log := logger.New("test")
	store := &errDeleter{err: errors.New("storage backend unavailable")}

	// Store error must not stop the DB step or panic
	assert.NotPanics(t, func() {
		CleanupObject(context.Background(), db, store, log, "nonexistent/key")
	})
	assert.True(t, store.called, "DeleteObject should have been called")
}

func TestCleanupObjectStoreSuccessNilError(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	log := logger.New("test")
	store := &errDeleter{err: nil}

	assert.NotPanics(t, func() {
		CleanupObject(context.Background(), db, store, log, "some/key")
	})
	assert.True(t, store.called)
}

func TestCleanupObjectCancelledContextDoesNotPanic(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	log := logger.New("test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.NotPanics(t, func() {
		CleanupObject(ctx, db, nil, log, "some/key")
	})
}

func TestCleanupObjectSuccessSoftDeletesRow(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	log := logger.New("test")
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
	CleanupObject(ctx, db, store, log, storageKey)

	var deletedAt *time.Time
	err = db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`,
		storageKey,
	).Scan(&deletedAt)
	require.NoError(t, err)
	assert.NotNil(t, deletedAt, "deleted_at should be set after CleanupObject")
}

func TestCleanupObjectAlreadyDeletedIsIdempotent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	log := logger.New("test")
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

	assert.NotPanics(t, func() {
		CleanupObject(ctx, db, nil, log, storageKey)
	})
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

	log := logger.New("test")
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
	CleanupObject(ctx, db, store, log, storageKey)

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

	log := logger.New("test")
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
	CleanupObject(ctx, db, store, log, storageKey)

	assert.True(t, store.called, "a NULL-backend object is legacy and must be deleted")

	var deletedAt *time.Time
	err = db.QueryRowContext(ctx,
		`SELECT deleted_at FROM media_files WHERE storage_key = $1`, storageKey).Scan(&deletedAt)
	require.NoError(t, err)
	assert.NotNil(t, deletedAt)
}
