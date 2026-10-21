package media

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// =====================================================================
// SetDiskWatermark wiring (#2759 unit A1)
// =====================================================================

// TestSetDiskWatermark mirrors the existing TestRecordSuccessfulUpload shape
// for SetOpsCounter: an optional dependency, unset by default, wired by the
// setter.
func TestSetDiskWatermark(t *testing.T) {
	handler := &Handler{}
	assert.Nil(t, handler.diskWatermark, "diskWatermark must be unset until SetDiskWatermark is called")

	watermark := NewDiskWatermark(false, logger.New("test"))
	handler.SetDiskWatermark(watermark)

	assert.Same(t, watermark, handler.diskWatermark)
}

// refusingWatermark returns a *DiskWatermark permanently at 100% occupancy for
// the given self-hosted posture, using the injectable seam constructor so no
// real filesystem is touched.
func refusingWatermark(selfHosted bool) *DiskWatermark {
	return newDiskWatermark("/", selfHosted, fixedDiskStatFS(100, 0), logger.NewWithWriter(&bytes.Buffer{}))
}

// TestUploadFeedbackScreenshot_DiskWatermarkGate proves that feedback
// screenshots obey the SaaS shared-disk refusal and remain writable for
// self-hosted deployments, below-threshold occupancy, and an unset watermark.
// Each case checks the externally visible object and media_files effects, so
// the refusal cannot pass merely because the response status happens to match.
func TestUploadFeedbackScreenshot_DiskWatermarkGate(t *testing.T) {
	tests := []struct {
		name       string
		watermark  *DiskWatermark
		wantStatus int
	}{
		{
			name:       "SaaS refuses at capacity",
			watermark:  refusingWatermark(false),
			wantStatus: http.StatusInsufficientStorage,
		},
		{
			name:       "SaaS refuses at 75 percent boundary",
			watermark:  newDiskWatermark("/", false, fixedDiskStatFS(100, 25), logger.NewWithWriter(&bytes.Buffer{})),
			wantStatus: http.StatusInsufficientStorage,
		},
		{
			name:       "SaaS refuses at 80 percent",
			watermark:  newDiskWatermark("/", false, fixedDiskStatFS(100, 20), logger.NewWithWriter(&bytes.Buffer{})),
			wantStatus: http.StatusInsufficientStorage,
		},
		{
			name:       "self-hosted allows at capacity",
			watermark:  refusingWatermark(true),
			wantStatus: http.StatusCreated,
		},
		{
			name:       "SaaS allows below refusal threshold",
			watermark:  newDiskWatermark("/", false, fixedDiskStatFS(100, 26), logger.NewWithWriter(&bytes.Buffer{})),
			wantStatus: http.StatusCreated,
		},
		{
			name:       "unset watermark allows upload",
			wantStatus: http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			if tt.watermark != nil {
				ts.handler.SetDiskWatermark(tt.watermark)
			}
			userID := ts.createTestUser(t, "screenshot_watermark")
			body, ct := multipartBody(t, "file", "screenshot.png", makePNG(t, 200, 200), nil)

			w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)

			require.Equal(t, tt.wantStatus, w.Code, "body: %s", w.Body.String())
			var metadataCount int
			require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1 AND storage_key LIKE 'feedback-screenshots/%' AND deleted_at IS NULL`, userID).Scan(&metadataCount))
			if tt.wantStatus == http.StatusInsufficientStorage {
				resp := parseBody(t, w)
				assert.Equal(t, errMsgAttachmentStorageAtCapacity, resp["error"])
				assert.Empty(t, ts.store.objects, "a refused screenshot must never reach the object store")
				assert.Zero(t, metadataCount, "a refused screenshot must not create media metadata")
				return
			}

			resp := parseBody(t, w)
			url, ok := resp["url"].(string)
			require.True(t, ok, "successful screenshot response URL must be a string, got %#v", resp["url"])
			storageKey, ok := resp["storage_key"].(string)
			require.True(t, ok, "successful screenshot response storage_key must be a string, got %#v", resp["storage_key"])
			assert.Equal(t, "/api/v1/media/"+storageKey, url)
			assert.True(t, strings.HasPrefix(storageKey, "feedback-screenshots/"), "unexpected storage key %q", storageKey)
			assert.Zero(t, ts.store.objectCount(), "pending screenshots must remain in PostgreSQL until publication")
			assert.Equal(t, 1, metadataCount, "successful screenshot must create one live media row")
			var storedKey string
			require.NoError(t, ts.db.QueryRow(`SELECT storage_key FROM media_files WHERE uploader_id = $1 AND storage_key LIKE 'feedback-screenshots/%' AND deleted_at IS NULL`, userID).Scan(&storedKey))
			assert.Equal(t, storageKey, storedKey, "metadata must point at the uploaded object")
			var state string
			var staged []byte
			require.NoError(t, ts.db.QueryRow(`SELECT state,staged_data FROM feedback_screenshot_uploads WHERE object_key=$1`, strings.TrimPrefix(storageKey, "feedback-screenshots/")).Scan(&state, &staged))
			assert.Equal(t, "pending", state)
			assert.NotEmpty(t, staged, "accepted screenshot bytes must be durably staged")
		})
	}
}

func TestUploadAvatar_DiskWatermarkDoesNotBlockOtherTier1Media(t *testing.T) {
	ts := setupMediaTest(t)
	ts.handler.SetDiskWatermark(refusingWatermark(false))
	userID := ts.createTestUser(t, "avatar_watermark_control")
	body, ct := multipartBody(t, "file", fileAvatarPng, makePNG(t, 100, 100), nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, "the feedback screenshot gate must not block avatar uploads; body: %s", w.Body.String())
	assert.Equal(t, 1, ts.store.objectCount())
	resp := parseBody(t, w)
	storageKey, ok := resp["storage_key"].(string)
	require.True(t, ok, "successful avatar response storage_key must be a string, got %#v", resp["storage_key"])
	assert.True(t, ts.store.hasObject(storageKey), "successful avatar object must exist at the returned key")
	var metadataCount int
	var storedKey string
	require.NoError(t, ts.db.QueryRow(`SELECT storage_key FROM media_files WHERE uploader_id = $1 AND profile_slot = 'avatar' AND deleted_at IS NULL`, userID).Scan(&storedKey))
	assert.Equal(t, storageKey, storedKey, "avatar metadata must point at the uploaded object")
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1 AND profile_slot = 'avatar' AND deleted_at IS NULL`, userID).Scan(&metadataCount))
	assert.Equal(t, 1, metadataCount, "successful avatar upload must create one live media row")
}

// =====================================================================
// UploadAttachment (single-shot path)
// =====================================================================

func TestUploadAttachment_507WhenDiskWatermarkRefuses(t *testing.T) {
	ts := setupMediaTest(t)
	ts.handler.SetDiskWatermark(refusingWatermark(false))

	owner := ts.createTestUser(t, "attach_watermark_saas")
	serverID := ts.createTestServer(t, owner, "Attach Watermark Server")
	channelID := ts.createTestChannel(t, serverID, "uploads")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext-data"), map[string]string{
		"channel_id":  channelID,
		"file_type":   "photo",
		"mime_type":   "image/jpeg",
		"key_version": "1",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	require.Equal(t, http.StatusInsufficientStorage, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	assert.Equal(t, errMsgAttachmentStorageAtCapacity, resp["error"])
	assert.Empty(t, ts.store.objects, "a refused write must never reach the object store")
}

func TestUploadAttachment_SelfHostedNeverRefusesOnDiskWatermark(t *testing.T) {
	ts := setupMediaTest(t)
	ts.handler.SetDiskWatermark(refusingWatermark(true))

	owner := ts.createTestUser(t, "attach_watermark_selfhost")
	serverID := ts.createTestServer(t, owner, "Attach Watermark Self-Host Server")
	channelID := ts.createTestChannel(t, serverID, "uploads")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext-data"), map[string]string{
		"channel_id":  channelID,
		"file_type":   "photo",
		"mime_type":   "image/jpeg",
		"key_version": "1",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code, "self-hosted must upload successfully even at 100%% occupancy; body: %s", w.Body.String())
}

func TestUploadAttachment_NilDiskWatermarkAllowsUpload(t *testing.T) {
	// setupMediaTest never calls SetDiskWatermark -- this locks in that the
	// zero-value nil field keeps every pre-existing upload path working.
	ts := setupMediaTest(t)

	owner := ts.createTestUser(t, "attach_watermark_unset")
	serverID := ts.createTestServer(t, owner, "Attach Watermark Unset Server")
	channelID := ts.createTestChannel(t, serverID, "uploads")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext-data"), map[string]string{
		"channel_id":  channelID,
		"file_type":   "photo",
		"mime_type":   "image/jpeg",
		"key_version": "1",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}

// =====================================================================
// InitUploadSession (chunked path)
// =====================================================================

func TestUploadSession_507WhenDiskWatermarkRefuses(t *testing.T) {
	ss := setupSessionTest(t)
	ss.handler.SetDiskWatermark(refusingWatermark(false))
	userID, _, channelID := ss.channelContext(t, "sess_watermark_saas")

	w := ss.doInit(userID, initBody(channelID, 2048))

	require.Equal(t, http.StatusInsufficientStorage, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	assert.Equal(t, errMsgAttachmentStorageAtCapacity, resp["error"])
	assert.Zero(t, ss.fake.openUploadCount(),
		"a refused init must never start a multipart upload")

	exists, err := ss.rdb.Exists(t.Context(), attachBudgetKey(userID)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "a refused init must never reserve byte budget")
}

// TestUploadSession_DiskWatermarkGateRunsBeforeMembershipCheck locks in the
// InitUploadSession comment's ordering claim: the watermark gate runs before
// validateSessionContext, so a caller with NO access at all to the named
// channel still gets 507 (not 403/404) when the disk is at capacity. If the
// gate were ever reordered to run after the membership check, this test
// would start observing 403/404 instead and fail.
func TestUploadSession_DiskWatermarkGateRunsBeforeMembershipCheck(t *testing.T) {
	ss := setupSessionTest(t)
	ss.handler.SetDiskWatermark(refusingWatermark(false))

	// A user with no server, no channel, no membership of any kind.
	strangerID := ss.createTestUser(t, "sess_watermark_stranger")
	// A channel that belongs to an entirely different server the stranger
	// never joined.
	owner := ss.createTestUser(t, "sess_watermark_owner")
	serverID := ss.createTestServer(t, owner, "sess-watermark-owner-server")
	channelID := ss.createTestChannel(t, serverID, "owner-only")

	w := ss.doInit(strangerID, initBody(channelID, 2048))

	assert.Equal(t, http.StatusInsufficientStorage, w.Code,
		"the disk watermark gate must be reached before the membership check; body: %s", w.Body.String())
}

func TestUploadSession_SelfHostedNeverRefusesOnDiskWatermark(t *testing.T) {
	ss := setupSessionTest(t)
	ss.handler.SetDiskWatermark(refusingWatermark(true))
	userID, _, channelID := ss.channelContext(t, "sess_watermark_selfhost")

	w := ss.doInit(userID, initBody(channelID, 2048))

	assert.Equal(t, http.StatusCreated, w.Code,
		"self-hosted must open a session even at 100%% occupancy; body: %s", w.Body.String())
}

func TestUploadSession_NilDiskWatermarkAllowsInit(t *testing.T) {
	// setupSessionTest never calls SetDiskWatermark -- locks in that the
	// zero-value nil field keeps the existing chunked-upload suite working.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_watermark_unset")

	w := ss.doInit(userID, initBody(channelID, 2048))

	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}
