package media

import (
	"bytes"
	"net/http"
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
