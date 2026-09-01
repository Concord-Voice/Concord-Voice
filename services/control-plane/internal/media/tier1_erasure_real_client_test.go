package media_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type realClientFreeTier struct{}

func (realClientFreeTier) GetTier(context.Context, string) string { return entitlements.TierFree }

func realClientMultipartPNG(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var imageBytes bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 0x44, G: 0x88, B: 0xcc, A: 0xff})
		}
	}
	require.NoError(t, png.Encode(&imageBytes, img))
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "avatar.png")
	require.NoError(t, err)
	_, err = part.Write(imageBytes.Bytes())
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return &body, w.FormDataContentType()
}

func waitForRealClientDeletes(t *testing.T, deletes <-chan string, want int) {
	t.Helper()
	for i := 0; i < want; i++ {
		select {
		case <-deletes:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out after %d/%d minio-go DELETEs", i, want)
		}
	}
}

func TestRealClientAccountErasureKeepsLatePutFencedAndReclaimable(t *testing.T) {
	var visible atomic.Bool
	var unexpectedReads atomic.Int32
	putBodyReceived := make(chan struct{})
	remoteSawCancellation := make(chan struct{})
	allowLatePutCommit := make(chan struct{})
	latePutCommitted := make(chan struct{})
	deleteSeen := make(chan string, 8)
	putKey := make(chan string, 1)
	var putOnce, cancellationOnce, commitOnce, releaseOnce sync.Once

	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucketRoot := strings.TrimSuffix(r.URL.Path, "/") == "/concord-media"
		lifecycle := r.URL.Query().Has("lifecycle")
		switch {
		case r.Method == http.MethodHead && bucketRoot:
			w.Header().Set("x-amz-bucket-region", "us-east-1")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && bucketRoot && lifecycle:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
		case r.Method == http.MethodPut && bucketRoot && lifecycle:
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/concord-media/avatars/"):
			putKey <- strings.TrimPrefix(r.URL.Path, "/concord-media/")
			_, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			putOnce.Do(func() { close(putBodyReceived) })
			<-r.Context().Done()
			cancellationOnce.Do(func() { close(remoteSawCancellation) })
			<-allowLatePutCommit
			visible.Store(true)
			commitOnce.Do(func() { close(latePutCommitted) })
			w.Header().Set("ETag", `"late-put"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/concord-media/"):
			if strings.HasPrefix(r.URL.Path, "/concord-media/avatars/") {
				visible.Store(false)
			}
			deleteSeen <- r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		case (r.Method == http.MethodHead || r.Method == http.MethodGet) && strings.HasPrefix(r.URL.Path, "/concord-media/"):
			unexpectedReads.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code></Error>`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s3.Close)

	store, err := storage.New(&config.Config{
		StorageBackend: "minio", StorageEndpoint: strings.TrimPrefix(s3.URL, "http://"),
		StorageRegion: "us-east-1", StorageAccessKey: "test-access", StorageSecretKey: "test-secret",
		StorageBucket: "concord-media",
	}, logger.New("real-client-test"))
	require.NoError(t, err)

	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, $4, true, true)`, userID, "late-account-test@test.local", "lateaccounttest",
		"$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY")
	require.NoError(t, err)

	handler := media.NewHandler(db, store, logger.New("real-client-test"), &config.Config{UploadMaxSize: 25 << 20}, nil, realClientFreeTier{})
	worker := media.NewTier1ErasureReclaimer(db, store, logger.New("real-client-test"))
	workerCtx, stopWorker := context.WithCancel(context.Background())
	waitWorker := worker.Start(workerCtx)
	t.Cleanup(func() {
		stopWorker()
		waitWorker()
	})
	account := users.NewAccountService(db, logger.New("real-client-test"))
	account.SetErasedMediaReclaimer(func(context.Context, []media.BlobRef, []media.BlobRef) { worker.Wake() })

	body, contentType := realClientMultipartPNG(t)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelRequest()
		releaseOnce.Do(func() { close(allowLatePutCommit) })
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/media/upload/avatar", body).WithContext(requestCtx)
	req.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	c.Set("user_id", userID)
	uploadDone := make(chan struct{})
	go func() {
		handler.UploadAvatar(c)
		close(uploadDone)
	}()

	select {
	case <-putBodyReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("minio-go did not deliver the complete PUT body")
	}
	cancelRequest()
	select {
	case <-uploadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("profile request did not return after cancellation")
	}
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	select {
	case <-remoteSawCancellation:
	case <-time.After(2 * time.Second):
		t.Fatal("remote S3 handler did not observe client cancellation")
	}
	var avatarURL *string
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, userID).Scan(&avatarURL))
	assert.Nil(t, avatarURL, "canceled upload must roll back avatar metadata")
	physicalKey := <-putKey
	assert.NotEqual(t, path.Join("avatars", userID), physicalKey,
		"a canceled profile upload must use a fresh physical key, not the reusable canonical slot")

	require.NoError(t, account.DeleteAccount(context.Background(), userID))
	waitForRealClientDeletes(t, deleteSeen, 3)
	legacyAvatarKey := path.Join("avatars", userID)
	bannerKey := path.Join("banners", userID)
	var tombstones, successfulDeletes int
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT COUNT(*), COUNT(last_delete_at)
			FROM tier1_erasure_delete_obligations WHERE storage_key IN ($1, $2, $3)`, physicalKey, legacyAvatarKey, bannerKey).
			Scan(&tombstones, &successfulDeletes)
		return err == nil && tombstones == 3 && successfulDeletes == 3
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, 3, tombstones)
	assert.Equal(t, 3, successfulDeletes)

	releaseOnce.Do(func() { close(allowLatePutCommit) })
	select {
	case <-latePutCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("remote PUT did not finish its late commit")
	}
	assert.True(t, visible.Load(), "the remote can still physically commit after client cancellation")

	getRecorder := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRecorder)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/media/avatars/"+userID, nil)
	getCtx.Params = gin.Params{{Key: "user_id", Value: userID}}
	handler.ProxyAvatar(getCtx)
	assert.Equal(t, http.StatusNotFound, getRecorder.Code)
	assert.Equal(t, "no-store", getRecorder.Header().Get("Cache-Control"))
	assert.Zero(t, unexpectedReads.Load(), "tombstone denial must happen before object storage GET/HEAD")

	_, err = db.Exec(`UPDATE tier1_erasure_delete_obligations SET reconcile_after = clock_timestamp() - interval '1 second'
		WHERE storage_key = $1`, physicalKey)
	require.NoError(t, err)
	worker.Wake()
	waitForRealClientDeletes(t, deleteSeen, 1)
	assert.False(t, visible.Load(), "scheduled permanent-tombstone retry must delete the late object")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, physicalKey).
		Scan(&tombstones))
	assert.Equal(t, 1, tombstones, "successful retry must retain the permanent tombstone")
}
