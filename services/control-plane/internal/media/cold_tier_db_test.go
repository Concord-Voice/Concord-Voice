package media

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// coldDownloadSetup serves a channel attachment from a store that reports
// Infrequent Access, with Redis wired in.
func coldDownloadSetup(t *testing.T, class string) (*testSetup, *coldStore, *miniredis.Miniredis, string, string, string) {
	t.Helper()
	ts := setupMediaTest(t)
	store := &coldStore{mockStore: ts.store, class: class, done: make(chan string, 4)}
	ts.handler.store = store
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ts.handler.SetSessionRedis(rdb)

	owner := ts.createTestUser(t, "cold"+uuid.NewString()[:8])
	channelID := ts.createTestChannel(t, ts.createTestServer(t, owner, "Cold"), "files")
	fileID := uuid.New().String()
	key := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), key, bytes.NewReader([]byte("ciphertext")), 10, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 10, $3, 1, $4)`,
		fileID, owner, key, channelID)
	require.NoError(t, err)
	return ts, store, mr, owner, fileID, key
}

func TestDownloadAttachment_SecondColdReadPromotes(t *testing.T) {
	ts, store, _, owner, fileID, key := coldDownloadSetup(t, "STANDARD_IA")
	params := gin.Params{{Key: "file_id", Value: fileID}}

	for i := 0; i < 2; i++ {
		w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, params)
		require.Equal(t, http.StatusOK, w.Code)
	}
	select {
	case got := <-store.done:
		require.Equal(t, key, got)
	case <-time.After(5 * time.Second):
		t.Fatal("second cold download did not promote")
	}
	require.True(t, ts.store.hasObject(key), "a live attachment keeps its object")
}

func TestDownloadAttachment_StandardReadNotCounted(t *testing.T) {
	ts, _, mr, owner, fileID, _ := coldDownloadSetup(t, "STANDARD")
	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})
	require.Equal(t, http.StatusOK, w.Code)
	require.Never(t, func() bool { return mr.Exists(coldReadKey(fileID)) }, 200*time.Millisecond, 20*time.Millisecond)
}

func TestNoteColdRead_DeletedDuringCopyRemovesObject(t *testing.T) {
	ts, store, _, _, fileID, key := coldDownloadSetup(t, "STANDARD_IA")
	_, err := ts.db.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE id = $1`, fileID)
	require.NoError(t, err)

	ts.handler.noteColdRead(store, fileID, key)
	ts.handler.noteColdRead(store, fileID, key)
	require.Equal(t, []string{key}, store.promoted)
	require.False(t, ts.store.hasObject(key), "a copy that outlived its row is deleted again")
}
