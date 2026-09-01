package users_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type profileMediaTierResolver struct{}

func (profileMediaTierResolver) GetTier(context.Context, string) string { return entitlements.TierFree }

type signalingProfileMediaDeleter struct {
	deleted chan<- string
	present bool
}

func (d *signalingProfileMediaDeleter) DeleteObject(_ context.Context, key string) error {
	d.present = false
	d.deleted <- key
	return nil
}

func invokeProfilePatch(t *testing.T, handler *users.Handler, userID string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPatch, urlUsersMe, bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	handler.UpdateMe(c)
	return recorder
}

func seedProfileAvatar(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	key := "avatars/" + userID
	_, err := db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
		VALUES ($1, $2, 'photo', 1, 'image/png', 4, $3, 'avatar')`, uuid.New().String(), userID, key)
	require.NoError(t, err)
	return key
}

func newProfileMediaHandler(db *sql.DB, wake func()) *users.Handler {
	h := users.NewHandler(db, logger.New("test"), nil, nil, profileMediaTierResolver{}, nil, nil)
	h.SetTier1ErasureWake(wake)
	return h
}

func countPermanentObligations(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&count))
	return count
}

func assertNoProfileClearWake(t *testing.T, wake <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-wake:
		t.Fatal(message)
	default:
	}
}

func TestUpdateMeClearRollsBackMediaCleanupWhenCommitFails(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "profilecleanuprollback")
	key := seedProfileAvatar(t, ts.DB, user.ID)
	originalURL := fmt.Sprintf("/api/v1/media/%s", key)
	_, err := ts.DB.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`, originalURL, user.ID)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_profile_cleanup_commit_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'simulated profile cleanup commit failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_profile_cleanup_commit_failure
			AFTER UPDATE ON users
			DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION test_profile_cleanup_commit_failure();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_profile_cleanup_commit_failure ON users;
			DROP FUNCTION IF EXISTS test_profile_cleanup_commit_failure();
		`)
		require.NoError(t, cleanupErr)
	})

	wake := make(chan struct{}, 1)
	h := newProfileMediaHandler(ts.DB, func() { wake <- struct{}{} })
	response := invokeProfilePatch(t, h, user.ID, map[string]string{"avatar_url": ""})

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	var avatarURL *string
	require.NoError(t, ts.DB.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, user.ID).Scan(&avatarURL))
	require.NotNil(t, avatarURL)
	assert.Equal(t, originalURL, *avatarURL)
	var deletedAt *time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT deleted_at FROM media_files WHERE storage_key = $1`, key).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "media metadata must remain live after transaction rollback")
	assert.Zero(t, countPermanentObligations(t, ts.DB, key), "rollback must not persist a deletion obligation")
	assertNoProfileClearWake(t, wake, "rollback must not wake the reclaimer")
}

func TestUpdateMeClearReturns500WhenMediaCleanupFails(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "profilecleanupmediafailure")
	key := seedProfileAvatar(t, ts.DB, user.ID)
	originalURL := fmt.Sprintf("/api/v1/media/%s", key)
	_, err := ts.DB.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`, originalURL, user.ID)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_profile_cleanup_media_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'simulated media cleanup failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_profile_cleanup_media_failure
			BEFORE UPDATE OF deleted_at ON media_files
			FOR EACH ROW EXECUTE FUNCTION test_profile_cleanup_media_failure();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_profile_cleanup_media_failure ON media_files;
			DROP FUNCTION IF EXISTS test_profile_cleanup_media_failure();
		`)
		require.NoError(t, cleanupErr)
	})

	wake := make(chan struct{}, 1)
	h := newProfileMediaHandler(ts.DB, func() { wake <- struct{}{} })
	response := invokeProfilePatch(t, h, user.ID, map[string]string{"avatar_url": ""})

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	var avatarURL *string
	require.NoError(t, ts.DB.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, user.ID).Scan(&avatarURL))
	require.NotNil(t, avatarURL)
	assert.Equal(t, originalURL, *avatarURL)
	var deletedAt *time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT deleted_at FROM media_files WHERE storage_key = $1`, key).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "media metadata must remain live after cleanup failure")
	assert.Zero(t, countPermanentObligations(t, ts.DB, key), "failed cleanup must not persist a deletion obligation")
	assertNoProfileClearWake(t, wake, "failed cleanup must not wake the reclaimer")
}

func TestUpdateMeClearStorageFailureRetainsDurableRetrySignal(t *testing.T) {
	t.Run("ordinary clear recovers after restart", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "profileclearorphan")
		key := seedProfileAvatar(t, ts.DB, user.ID)
		originalURL := fmt.Sprintf("/api/v1/media/%s", key)
		_, err := ts.DB.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`, originalURL, user.ID)
		require.NoError(t, err)

		wake := make(chan struct{}, 1)
		response := invokeProfilePatch(t, newProfileMediaHandler(ts.DB, func() { wake <- struct{}{} }), user.ID, map[string]string{"avatar_url": ""})
		require.Equal(t, http.StatusOK, response.Code, "the API reports the clear succeeded")

		var avatarURL *string
		require.NoError(t, ts.DB.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, user.ID).Scan(&avatarURL))
		assert.Nil(t, avatarURL, "the canonical reference is cleared")
		var deletedAt *time.Time
		require.NoError(t, ts.DB.QueryRow(`SELECT deleted_at FROM media_files WHERE storage_key = $1`, key).Scan(&deletedAt))
		assert.NotNil(t, deletedAt, "the media row is retired")
		assert.Equal(t, 1, countPermanentObligations(t, ts.DB, key), "ordinary profile clear must persist a permanent retry obligation")
		select {
		case <-wake:
		default:
			t.Fatal("committed clear must wake the reclaimer")
		}
		var tombstones int
		require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&tombstones))
		assert.Equal(t, 1, tombstones, "ordinary profile clear must terminalize the retired physical key")

		deleted := make(chan string, 1)
		store := &signalingProfileMediaDeleter{deleted: deleted, present: true}
		ctx, cancel := context.WithCancel(context.Background())
		wait := media.NewTier1ErasureReclaimer(ts.DB, store, logger.New("test")).Start(ctx)
		select {
		case got := <-deleted:
			assert.Equal(t, key, got)
			assert.False(t, store.present, "a later reclaimer with restored storage must delete retained plaintext from durable state")
		case <-time.After(2 * time.Second):
			cancel()
			wait()
			t.Fatal("retained plaintext after a failed profile clear must remain represented by durable retryable database state")
		}
		cancel()
		wait()
	})

	t.Run("existing obligation reaches restored storage", func(t *testing.T) {
		ts := setupTS(t)
		key := "avatars/profile-clear-reclaimer-control"
		_, err := ts.DB.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
		require.NoError(t, err)
		deleted := make(chan string, 1)
		store := &signalingProfileMediaDeleter{deleted: deleted, present: true}
		ctx, cancel := context.WithCancel(context.Background())
		wait := media.NewTier1ErasureReclaimer(ts.DB, store, logger.New("test")).Start(ctx)
		defer func() { cancel(); wait() }()
		select {
		case got := <-deleted:
			assert.Equal(t, key, got, "positive control: reclaimer must reach restored storage")
		case <-time.After(2 * time.Second):
			t.Fatal("positive control reclaimer did not reach restored storage")
		}
	})
}

func TestUpdateMeClearTerminalizesStaleProfileUploadIntent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "profileintentclear")
	key := "avatars/" + user.ID + "/" + uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO tier1_profile_upload_intents (user_id, profile_slot, storage_key) VALUES ($1, 'avatar', $2)`, user.ID, key)
	require.NoError(t, err)

	response := invokeProfilePatch(t, newProfileMediaHandler(ts.DB, nil), user.ID, map[string]string{"avatar_url": ""})
	assert.Equal(t, http.StatusOK, response.Code)

	var intents, tombstones int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM tier1_profile_upload_intents WHERE user_id = $1 AND storage_key = $2`, user.ID, key).Scan(&intents))
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&tombstones))
	assert.Zero(t, intents, "clearing a profile must consume the stale upload intent")
	assert.Equal(t, 1, tombstones, "the stale exact key must become permanently reclaimable")
}
