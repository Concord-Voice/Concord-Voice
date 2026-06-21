package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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
