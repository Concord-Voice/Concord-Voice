package media

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/feedbackscreenshots"
)

func TestUploadFeedbackScreenshotPendingQuotaCapsAtEightWithoutObjectWrites(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "feedback_screenshot_quota")
	imageData := makePNG(t, 200, 200)

	for i := 0; i < 8; i++ {
		body, ct := multipartBody(t, "file", "screenshot.png", imageData, nil)
		w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, http.MethodPost, pathUploadFeedbackScreenshot, userID, body, ct)
		require.Equalf(t, http.StatusCreated, w.Code, "upload %d body: %s", i+1, w.Body.String())
	}
	body, ct := multipartBody(t, "file", "screenshot.png", imageData, nil)
	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, http.MethodPost, pathUploadFeedbackScreenshot, userID, body, ct)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "ninth pending screenshot must be refused; body: %s", w.Body.String())
	assert.Zero(t, ts.store.objectCount(), "staging pending screenshots must not write to remote storage")
	assert.Equal(t, 8, countFeedbackScreenshotMedia(t, ts, userID))
	var pending, bytes int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes),0) FROM feedback_screenshot_uploads WHERE user_id=$1 AND state='pending'`, userID).Scan(&pending, &bytes))
	assert.Equal(t, 8, pending)
	assert.Greater(t, bytes, 0)
}

func TestFeedbackScreenshotMetadataInsertFailureDoesNotWriteObjectOrLedger(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "feedback_screenshot_insert_failure")
	_, err := ts.db.Exec(`CREATE OR REPLACE FUNCTION fail_feedback_screenshot_metadata_insert() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced metadata insert failure'; END $$`)
	require.NoError(t, err)
	_, err = ts.db.Exec(`CREATE TRIGGER fail_feedback_screenshot_metadata_insert BEFORE INSERT ON media_files
		FOR EACH ROW WHEN (NEW.storage_key LIKE 'feedback-screenshots/%') EXECUTE FUNCTION fail_feedback_screenshot_metadata_insert()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.db.Exec(`DROP TRIGGER IF EXISTS fail_feedback_screenshot_metadata_insert ON media_files`)
		_, _ = ts.db.Exec(`DROP FUNCTION IF EXISTS fail_feedback_screenshot_metadata_insert()`)
	})
	body, ct := multipartBody(t, "file", "screenshot.png", makePNG(t, 100, 100), nil)
	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, http.MethodPost, pathUploadFeedbackScreenshot, userID, body, ct)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Zero(t, ts.store.objectCount(), "staging must fail before any object-store write")
	assert.Zero(t, countFeedbackScreenshotMedia(t, ts, userID))
	var ledgerCount int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM feedback_screenshot_uploads`).Scan(&ledgerCount))
	assert.Zero(t, ledgerCount, "failed metadata insertion must roll back the lifecycle charge")
}

func TestUploadFeedbackScreenshotPendingIsPubliclyReadableFromDatabase(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "feedback_screenshot_read")
	url := uploadFeedbackScreenshot(t, ts, userID)
	id := strings.TrimPrefix(url, feedbackScreenshotsPathPrefix)
	w := ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, http.MethodGet, url, gin.Params{{Key: "id", Value: id}})
	assert.Equal(t, http.StatusOK, w.Code, "live pending screenshot should be served from staged bytes: %s", w.Body.String())
	assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
	assert.Zero(t, ts.store.GetObjectCalls(), "staged reads must not contact object storage")
}

func TestUploadFeedbackScreenshotExpiredPendingIsHiddenAndSwept(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "feedback_screenshot_expire")
	url := uploadFeedbackScreenshot(t, ts, userID)
	id := strings.TrimPrefix(url, feedbackScreenshotsPathPrefix)
	_, err := ts.db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 minute' WHERE object_key=$1`, id)
	require.NoError(t, err)
	w := ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, http.MethodGet, url, gin.Params{{Key: "id", Value: id}})
	assert.Equal(t, http.StatusNotFound, w.Code, "expired pending screenshot must be hidden before sweeping")
	deleted, err := feedbackscreenshots.New(ts.db).Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.Equal(t, 0, countFeedbackScreenshotMedia(t, ts, userID), "sweeping must remove stale Tier 1 metadata")
	var state string
	var staged []byte
	require.NoError(t, ts.db.QueryRow(`SELECT state,staged_data FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &staged))
	assert.Equal(t, "deleted", state)
	assert.Nil(t, staged, "sweeping must release the staged bytes")
	assert.Zero(t, ts.store.GetObjectCalls())
}

func TestPinnedPublishedScreenshotSurvivesPendingExpiryAndOffloads(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "feedback_screenshot_pin")
	url := uploadFeedbackScreenshot(t, ts, userID)
	id := strings.TrimPrefix(url, feedbackScreenshotsPathPrefix)
	key := "feedback-screenshots/" + id
	lifecycle := feedbackscreenshots.New(ts.db)
	receipt, err := lifecycle.Pin(context.Background(), userID, []string{url})
	require.NoError(t, err)
	require.NoError(t, lifecycle.ConfirmPublished(context.Background(), receipt, []string{url}))
	_, err = ts.db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 minute' WHERE object_key=$1`, id)
	require.NoError(t, err)
	deleted, err := lifecycle.Sweep(context.Background())
	require.NoError(t, err)
	assert.Zero(t, deleted, "published pins must never be reclaimed by the pending expiry sweep")
	got, err := ts.db.Exec(`UPDATE feedback_screenshot_uploads SET retry_at=NULL WHERE object_key=$1`, id)
	require.NoError(t, err)
	_, _ = got.RowsAffected()
	count, err := lifecycle.Offload(context.Background(), func(*string) (feedbackscreenshots.ObjectPutter, error) { return ts.store, nil }, allowScreenshotOffload{})
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.True(t, ts.store.hasObject(key), "published screenshot should be retained in remote storage")
	w := ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, http.MethodGet, url, gin.Params{{Key: "id", Value: id}})
	assert.Equal(t, http.StatusOK, w.Code, "offloaded screenshot remains publicly readable: %s", w.Body.String())
	assert.Equal(t, 1, ts.store.GetObjectCalls())
}

func TestUnpublishedScreenshotIsNotOffloadedAndPendingQuotaRemainsCharged(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "feedback_screenshot_unpublished")
	url := uploadFeedbackScreenshot(t, ts, userID)
	lifecycle := feedbackscreenshots.New(ts.db)
	_, err := lifecycle.Pin(context.Background(), userID, []string{url})
	require.NoError(t, err)
	count, err := lifecycle.Offload(context.Background(), func(*string) (feedbackscreenshots.ObjectPutter, error) { return ts.store, nil }, allowScreenshotOffload{})
	require.NoError(t, err)
	assert.Zero(t, count, "a pin is not permanent until report publication is confirmed")
	assert.Zero(t, ts.store.objectCount())
	var staged []byte
	var state string
	require.NoError(t, ts.db.QueryRow(`SELECT state,staged_data FROM feedback_screenshot_uploads`).Scan(&state, &staged))
	assert.Equal(t, "pinned", state)
	assert.NotEmpty(t, staged, "unpublished bytes remain durably staged and charged")
}

func TestProxyFeedbackScreenshotLifecycleDatabaseFailureDoesNotReadObject(t *testing.T) {
	ts := setupMediaTest(t)
	id := uuid.NewString()
	key := "feedback-screenshots/" + id
	require.NoError(t, ts.store.PutObject(context.Background(), key, strings.NewReader("image"), 5, mimeImagePNG))
	closedDB, err := sqlOpenClosedForScreenshotTest()
	require.NoError(t, err)
	ts.handler.db = closedDB
	w := ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, http.MethodGet, feedbackScreenshotsPathPrefix+id, gin.Params{{Key: "id", Value: id}})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "lifecycle lookup failure should fail closed")
	assert.Zero(t, ts.store.GetObjectCalls(), "do not fall back to object storage when lifecycle lookup fails")
}

func uploadFeedbackScreenshot(t *testing.T, ts *testSetup, userID string) string {
	t.Helper()
	body, ct := multipartBody(t, "file", "screenshot.png", makePNG(t, 200, 200), nil)
	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, http.MethodPost, pathUploadFeedbackScreenshot, userID, body, ct)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	url, ok := resp["url"].(string)
	require.True(t, ok, "successful screenshot response URL must be a string, got %#v", resp["url"])
	id := strings.TrimPrefix(url, feedbackScreenshotsPathPrefix)
	_, err := uuid.Parse(id)
	require.NoError(t, err)
	assert.Equal(t, id, uuid.MustParse(id).String())
	return url
}

func countFeedbackScreenshotMedia(t *testing.T, ts *testSetup, userID string) int {
	t.Helper()
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE uploader_id=$1 AND storage_key LIKE 'feedback-screenshots/%' AND deleted_at IS NULL`, userID).Scan(&count))
	return count
}

type allowScreenshotOffload struct{}

func (allowScreenshotOffload) Check() error { return nil }

func sqlOpenClosedForScreenshotTest() (*sql.DB, error) {
	db, err := sql.Open("postgres", "postgres://localhost/unused")
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return db, nil
}
