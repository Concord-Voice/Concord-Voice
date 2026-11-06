package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const urlSuffix = "/url"

// insertDirectFixture stores an attachment of `size` bytes on `backend` (nil =
// legacy) in a channel the owner can read, and returns its id.
func insertDirectFixture(t *testing.T, ts *testSetup, store *mockStore, owner, channelID string, backend *string, size int64, keyVersion *int) string {
	t.Helper()
	if keyVersion == nil {
		one := 1 // tier 2 rows require an epoch (valid_media_context)
		keyVersion = &one
	}
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("x")), 1, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id, storage_backend)
		 VALUES ($1, $2, 'video', 2, 'video/mp4', $3, $4, $5, $6, $7)`,
		fileID, owner, size, storageKey, keyVersion, channelID, backend)
	require.NoError(t, err)
	return fileID
}

func directSetup(t *testing.T) (ts *testSetup, vendor *mockStore, owner, channelID string) {
	t.Helper()
	ts = setupMediaTest(t)
	ts.handler.cfg.StoragePresignMinBytes = 1_048_576
	vendor = newMockStore()
	ts.handler.SetStoreResolver(stubDeleteStoreResolver{store: vendor})
	owner = ts.createTestUser(t, "directowner"+uuid.NewString()[:8])
	serverID := ts.createTestServer(t, owner, "Direct Server")
	channelID = ts.createTestChannel(t, serverID, "direct")
	return ts, vendor, owner, channelID
}

func getURL(ts *testSetup, fileID, userID string) *httptest.ResponseRecorder {
	return ts.doJSON(ts.handler.AttachmentDownloadURL, "GET", pathAttachmentsPrefix+fileID+urlSuffix, userID, gin.Params{{Key: "file_id", Value: fileID}})
}

func TestAttachmentDownloadURL_VendorObjectGetsPresignedURL(t *testing.T) {
	ts, vendor, owner, channelID := directSetup(t)
	backend := "r2-useast"
	epoch := 7
	fileID := insertDirectFixture(t, ts, vendor, owner, channelID, &backend, 1<<30, &epoch)

	before := time.Now()
	w := getURL(ts, fileID, owner)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
	var body attachmentDownloadURLResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(t, strings.Contains(body.URL, fileID), "presigned for this row's key")
	assert.Equal(t, "video/mp4", body.MimeType)
	require.NotNil(t, body.KeyVersion, "the epoch must ride in the body, since a presigned URL cannot carry the header")
	assert.Equal(t, 7, *body.KeyVersion)
	assert.Equal(t, int64(1<<30), body.Size)
	assert.WithinDuration(t, before.Add(directDownloadTTL), body.ExpiresAt, 5*time.Second)
	assert.LessOrEqual(t, directDownloadTTL, 120*time.Second, "ADR-0038 caps the TTL at 120 s")
	assert.GreaterOrEqual(t, directDownloadTTL, 60*time.Second)
}

func TestAttachmentDownloadURL_ProxyOnlyCases(t *testing.T) {
	ts, vendor, owner, channelID := directSetup(t)
	backend := "r2-useast"
	small := insertDirectFixture(t, ts, vendor, owner, channelID, &backend, 1_048_575, nil)
	legacy := insertDirectFixture(t, ts, ts.store, owner, channelID, nil, 1<<30, nil)

	for name, fileID := range map[string]string{"below the floor": small, "legacy backend": legacy} {
		w := getURL(ts, fileID, owner)
		assert.Equal(t, http.StatusConflict, w.Code, name)
		assert.Contains(t, w.Body.String(), errCodeProxyOnly, name)
	}
}

func TestAttachmentDownloadURL_OutsiderGets403NotProxyOnly(t *testing.T) {
	ts, vendor, owner, channelID := directSetup(t)
	outsider := ts.createTestUser(t, "directoutsider"+uuid.NewString()[:8])
	// Legacy + tiny: if authorization ran after the placement check, the
	// outsider would learn the file exists from a proxy_only answer.
	legacy := insertDirectFixture(t, ts, ts.store, owner, channelID, nil, 10, nil)
	backend := "r2-useast"
	vendorFile := insertDirectFixture(t, ts, vendor, owner, channelID, &backend, 1<<30, nil)

	for _, fileID := range []string{legacy, vendorFile} {
		w := getURL(ts, fileID, outsider)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.NotContains(t, w.Body.String(), "url")
	}
}

func TestAttachmentDownloadURL_BadAndMissingIDs(t *testing.T) {
	ts, _, owner, _ := directSetup(t)
	assert.Equal(t, http.StatusBadRequest, getURL(ts, "not-a-uuid", owner).Code)
	assert.Equal(t, http.StatusNotFound, getURL(ts, uuid.NewString(), owner).Code)
}

func TestAttachmentDownloadURL_PresignFailureIs500(t *testing.T) {
	ts, vendor, owner, channelID := directSetup(t)
	backend := "r2-useast"
	fileID := insertDirectFixture(t, ts, vendor, owner, channelID, &backend, 1<<30, nil)
	vendor.mu.Lock()
	vendor.presignErr = errors.New("signing failed")
	vendor.mu.Unlock()

	assert.Equal(t, http.StatusInternalServerError, getURL(ts, fileID, owner).Code)
}

func TestAttachmentDownloadURL_UnresolvableBackendIs503(t *testing.T) {
	ts, vendor, owner, channelID := directSetup(t)
	backend := "r2-useast"
	fileID := insertDirectFixture(t, ts, vendor, owner, channelID, &backend, 1<<30, nil)
	ts.handler.SetStoreResolver(stubDeleteStoreResolver{err: fmt.Errorf("gone")})

	assert.Equal(t, http.StatusServiceUnavailable, getURL(ts, fileID, owner).Code)
}
