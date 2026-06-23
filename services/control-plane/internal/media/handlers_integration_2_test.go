package media

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fileBannerPng          = "banner.png"
	fileAvatarPng          = "avatar.png"
	pathUploadBanner       = "/api/v1/media/upload/banner"
	pathUploadServerBanner = "/api/v1/media/upload/server-banner"
	pathServerBanners      = "/api/v1/media/server-banners/"
	fmtServerBannersKey    = "server-banners/%s"
	fmtAvatarsKey          = "avatars/%s"
	hdrContentType         = "Content-Type"
	hdrCacheControl        = "Cache-Control"
	mimeImagePNG           = "image/png"
	mimeImageJPEG          = "image/jpeg"
	testCiphertextData     = "ciphertext-data"
)

// =====================================================================
// Tier 1 Upload: Banner
// =====================================================================

func TestUploadBannerSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "banneruser")

	imgData := makePNG(t, 400, 200)
	body, ct := multipartBody(t, "file", fileBannerPng, imgData, nil)

	w := ts.doMultipart(ts.handler.UploadBanner, "POST", pathUploadBanner, userID, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, fmt.Sprintf("/api/v1/media/banners/%s", userID), resp["url"])
	assert.True(t, ts.store.hasObject(fmt.Sprintf("banners/%s", userID)))
}

func TestUploadBannerMissingFile(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "bannernof")

	req := httptest.NewRequest("POST", pathUploadBanner, nil)
	req.Header.Set(hdrContentType, "multipart/form-data; boundary=xxx")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	ts.handler.UploadBanner(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadBannerTooLarge(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "bigbanner")

	bigData := make([]byte, bannerMaxUpload+1024)
	body, ct := multipartBody(t, "file", "huge.png", bigData, nil)

	w := ts.doMultipart(ts.handler.UploadBanner, "POST", pathUploadBanner, userID, body, ct)

	assert.True(t, w.Code == http.StatusRequestEntityTooLarge || w.Code == http.StatusBadRequest,
		"expected 413 or 400, got %d", w.Code)
}

func TestUploadBannerInvalidImageType(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "bannerbadtype")

	body, ct := multipartBody(t, "file", "doc.txt", []byte("This is not an image at all, just plain text"), nil)

	w := ts.doMultipart(ts.handler.UploadBanner, "POST", pathUploadBanner, userID, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// Tier 1 Upload: Server Banner
// =====================================================================

func TestUploadServerBannerSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "sbowner")
	serverID := ts.createTestServer(t, owner, "Banner Server")

	imgData := makePNG(t, 400, 200)
	body, ct := multipartBody(t, "file", fileBannerPng, imgData, map[string]string{keyServerID: serverID})

	w := ts.doMultipart(ts.handler.UploadServerBanner, "POST", pathUploadServerBanner, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.True(t, ts.store.hasObject(fmt.Sprintf(fmtServerBannersKey, serverID)))
	assert.NotEmpty(t, resp[keyFileID])
}

func TestUploadServerBannerNonMember(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "sbbowner")
	outsider := ts.createTestUser(t, "sbboutsider")
	serverID := ts.createTestServer(t, owner, "SB Non-Member")

	imgData := makePNG(t, 200, 100)
	body, ct := multipartBody(t, "file", fileBannerPng, imgData, map[string]string{keyServerID: serverID})

	w := ts.doMultipart(ts.handler.UploadServerBanner, "POST", pathUploadServerBanner, outsider, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUploadServerBannerMissingServerID(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "sbnoid")

	imgData := makePNG(t, 200, 100)
	body, ct := multipartBody(t, "file", fileBannerPng, imgData, nil)

	w := ts.doMultipart(ts.handler.UploadServerBanner, "POST", pathUploadServerBanner, owner, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadServerBannerInvalidServerID(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "sbbadid")

	imgData := makePNG(t, 200, 100)
	body, ct := multipartBody(t, "file", fileBannerPng, imgData, map[string]string{keyServerID: "not-a-uuid"})

	w := ts.doMultipart(ts.handler.UploadServerBanner, "POST", pathUploadServerBanner, owner, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// Tier 1 Upload: Server Icon Success
// =====================================================================

func TestUploadServerIconSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "iconsuccess")
	serverID := ts.createTestServer(t, owner, "Icon Success Server")

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{keyServerID: serverID})

	w := ts.doMultipart(ts.handler.UploadServerIcon, "POST", pathUploadServerIcon, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.True(t, ts.store.hasObject(fmt.Sprintf("server-icons/%s", serverID)))
	assert.NotEmpty(t, resp[keyFileID])
}

// =====================================================================
// Tier 2 Attachment: Edge Cases
// =====================================================================

func TestUploadAttachmentMissingFile(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "attachnof")

	req := httptest.NewRequest("POST", pathUploadAttachment, nil)
	req.Header.Set(hdrContentType, "multipart/form-data; boundary=xxx")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", user)
	ts.handler.UploadAttachment(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadAttachmentInvalidConversationID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "badconvid")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("data"), map[string]string{
		keyConversationID: "not-a-uuid",
		keyFileType:       "file",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, user, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Invalid conversation_id")
}

func TestUploadAttachmentInvalidKeyVersion(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "badkeyver")
	serverID := ts.createTestServer(t, owner, "KeyVer Server")
	channelID := ts.createTestChannel(t, serverID, "keyver")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("data"), map[string]string{
		keyChannelID:  channelID,
		keyFileType:   "file",
		"key_version": "abc",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "key_version")
}

func TestUploadAttachmentNegativeKeyVersion(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "negkeyver")
	serverID := ts.createTestServer(t, owner, "NegKeyVer Server")
	channelID := ts.createTestChannel(t, serverID, "negkeyver")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("data"), map[string]string{
		keyChannelID:  channelID,
		keyFileType:   "file",
		"key_version": "-1",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadAttachmentDefaultFileType(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "defaultft")
	serverID := ts.createTestServer(t, owner, "DefaultFT Server")
	channelID := ts.createTestChannel(t, serverID, "defaultft")

	// Send an invalid file_type; should default to "file"
	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte(testCiphertextData), map[string]string{
		keyChannelID: channelID,
		keyFileType:  "invalid_type",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "file", resp[keyFileType], "invalid file_type should default to 'file'")
}

func TestUploadAttachmentDefaultMimeType(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "defaultmime")
	serverID := ts.createTestServer(t, owner, "DefaultMime Server")
	channelID := ts.createTestChannel(t, serverID, "defaultmime")

	// Do not send mime_type; should default to application/octet-stream
	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte(testCiphertextData), map[string]string{
		keyChannelID: channelID,
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestUploadAttachmentWithValidKeyVersion(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "validkeyver")
	serverID := ts.createTestServer(t, owner, "ValidKeyVer Server")
	channelID := ts.createTestChannel(t, serverID, "validkv")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("cipher"), map[string]string{
		keyChannelID:  channelID,
		keyFileType:   "file",
		"key_version": "3",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestUploadAttachmentVideoFileType(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "videoft")
	serverID := ts.createTestServer(t, owner, "VideoFT Server")
	channelID := ts.createTestChannel(t, serverID, "videoft")

	body, ct := multipartBody(t, "file", "video.mp4", []byte("video-ciphertext"), map[string]string{
		keyChannelID: channelID,
		keyFileType:  "video",
		keyMimeType:  "video/mp4",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "video", resp[keyFileType])
}

func TestUploadAttachmentAnimatedFileType(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "animft")
	serverID := ts.createTestServer(t, owner, "AnimFT Server")
	channelID := ts.createTestChannel(t, serverID, "animft")

	body, ct := multipartBody(t, "file", "anim.gif", []byte("animated-ciphertext"), map[string]string{
		keyChannelID: channelID,
		keyFileType:  "animated",
		keyMimeType:  "image/gif",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "animated", resp[keyFileType])
}

// =====================================================================
// Download Attachment: Additional Edge Cases
// =====================================================================

func TestDownloadAttachmentDMSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	user1 := ts.createTestUser(t, "dluser1")
	user2 := ts.createTestUser(t, "dluser2")
	convID := ts.createTestDMConversation(t, user1, user2)

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("dm-cipher")), 9, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		 VALUES ($1, $2, 'file', 2, 'text/plain', 9, $3, 1, $4)`,
		fileID, user1, storageKey, convID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, user1, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "dm-cipher", w.Body.String())
	assert.Equal(t, "text/plain", w.Header().Get("X-File-Mime-Type"))
	assert.Equal(t, "private, no-store", w.Header().Get(hdrCacheControl))
}

func TestDownloadAttachmentDMNonParticipant(t *testing.T) {
	ts := setupMediaTest(t)
	user1 := ts.createTestUser(t, "dldmowner1")
	user2 := ts.createTestUser(t, "dldmowner2")
	outsider := ts.createTestUser(t, "dldmout")
	convID := ts.createTestDMConversation(t, user1, user2)

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("dm-secret")), 9, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 9, $3, 1, $4)`,
		fileID, user1, storageKey, convID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, outsider, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDownloadAttachmentStorageMissing(t *testing.T) {
	// File metadata exists but the blob is missing from the store
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "dlmissing")
	serverID := ts.createTestServer(t, owner, "DL Missing Server")
	channelID := ts.createTestChannel(t, serverID, "dlmissing")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	// Do NOT put the object in the store — only insert metadata
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 6, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "not found in storage")
}

func TestDownloadAttachmentSoftDeleted(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "dldeleted")
	serverID := ts.createTestServer(t, owner, "DL Deleted Server")
	channelID := ts.createTestChannel(t, serverID, "dldeleted")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id, deleted_at)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4, NOW())`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =====================================================================
// Proxy: Additional Paths
// =====================================================================

func TestProxyAvatarSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "proxyavatar")

	key := fmt.Sprintf(fmtAvatarsKey, userID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))

	// Public route — invoke without setting user_id in the gin context to
	// lock in that ProxyAvatar does not depend on auth middleware state.
	w := ts.doNoAuth(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/"+userID, gin.Params{{Key: "user_id", Value: userID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImagePNG, w.Header().Get(hdrContentType))
	// Cloudflare/shared caches must be allowed.
	assert.Contains(t, w.Header().Get(hdrCacheControl), "public")
	assert.Contains(t, w.Header().Get(hdrCacheControl), "max-age=3600")
}

func TestProxyBannerSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "proxybanner")

	key := fmt.Sprintf("banners/%s", userID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 200, 50)), 200, mimeImageJPEG))

	// Public route — no user_id in context (see TestProxyAvatarSuccess).
	w := ts.doNoAuth(ts.handler.ProxyBanner, "GET", "/api/v1/media/banners/"+userID, gin.Params{{Key: "user_id", Value: userID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImageJPEG, w.Header().Get(hdrContentType))
	assert.Contains(t, w.Header().Get(hdrCacheControl), "public")
	assert.Contains(t, w.Header().Get(hdrCacheControl), "max-age=3600")
}

func TestProxyBannerInvalidID(t *testing.T) {
	ts := setupMediaTest(t)

	w := ts.doJSON(ts.handler.ProxyBanner, "GET", "/api/v1/media/banners/not-uuid", "any", gin.Params{{Key: "user_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxyBannerNotFound(t *testing.T) {
	ts := setupMediaTest(t)
	fakeID := uuid.New().String()

	w := ts.doJSON(ts.handler.ProxyBanner, "GET", "/api/v1/media/banners/"+fakeID, fakeID, gin.Params{{Key: "user_id", Value: fakeID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxyServerIconSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyiconsuc")
	serverID := ts.createTestServer(t, owner, "ProxyIcon Server")

	key := fmt.Sprintf("server-icons/%s", serverID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))

	w := ts.doJSON(ts.handler.ProxyServerIcon, "GET", "/api/v1/media/server-icons/"+serverID, owner, gin.Params{{Key: "server_id", Value: serverID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImagePNG, w.Header().Get(hdrContentType))
	// Public Tier 1 (post-#571 #12): shared-cacheable so CDNs can serve it.
	assert.Contains(t, w.Header().Get(hdrCacheControl), "public")
}

func TestProxyServerIconInvalidID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "proxyiconbad")
	_ = user // needed to setup DB connection

	w := ts.doJSON(ts.handler.ProxyServerIcon, "GET", "/api/v1/media/server-icons/not-uuid", user, gin.Params{{Key: "server_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxyInviteServerIconInvalidCodeFallback(t *testing.T) {
	ts := setupMediaTest(t)

	w := ts.doNoAuth(ts.handler.ProxyInviteServerIcon, "GET", "/api/v1/invites/not-a-code/icon", gin.Params{{Key: "code", Value: "not-a-code"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get(hdrContentType), "image/svg+xml")
	assert.Contains(t, w.Header().Get(hdrCacheControl), "max-age=60")
}

func TestProxyInviteServerIconMissingInviteFallback(t *testing.T) {
	ts := setupMediaTest(t)

	w := ts.doNoAuth(ts.handler.ProxyInviteServerIcon, "GET", "/api/v1/invites/GHJKMNPQ/icon", gin.Params{{Key: "code", Value: "GHJKMNPQ"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get(hdrContentType), "image/svg+xml")
}

func TestProxyInviteServerIconSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyinviteicon")
	serverID := ts.createTestServer(t, owner, "Proxy Invite Icon")
	code := "HJKLMNPQ"
	ts.setServerIconURL(t, serverID, "server-icons/"+serverID)
	ts.createTestInviteCode(t, serverID, owner, code, false)

	key := fmt.Sprintf("server-icons/%s", serverID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))

	w := ts.doNoAuth(ts.handler.ProxyInviteServerIcon, "GET", "/api/v1/invites/"+code+"/icon", gin.Params{{Key: "code", Value: code}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImagePNG, w.Header().Get(hdrContentType))
	assert.Contains(t, w.Header().Get(hdrCacheControl), "public")
	assert.Contains(t, w.Header().Get(hdrCacheControl), "max-age=3600")
}

func TestProxyInviteServerIconRevokedInviteFallback(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyinviteiconrev")
	serverID := ts.createTestServer(t, owner, "Proxy Invite Revoked")
	code := "JKLMNPQR"
	ts.setServerIconURL(t, serverID, "server-icons/"+serverID)
	ts.createTestInviteCode(t, serverID, owner, code, true)

	key := fmt.Sprintf("server-icons/%s", serverID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))

	w := ts.doNoAuth(ts.handler.ProxyInviteServerIcon, "GET", "/api/v1/invites/"+code+"/icon", gin.Params{{Key: "code", Value: code}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get(hdrContentType), "image/svg+xml")
}

func TestProxyInviteServerIconMissingStorageFallback(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyinviteiconnf")
	serverID := ts.createTestServer(t, owner, "Proxy Invite Missing Storage")
	code := "KLMNPQRS"
	ts.setServerIconURL(t, serverID, "server-icons/"+serverID)
	ts.createTestInviteCode(t, serverID, owner, code, false)

	w := ts.doNoAuth(ts.handler.ProxyInviteServerIcon, "GET", "/api/v1/invites/"+code+"/icon", gin.Params{{Key: "code", Value: code}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get(hdrContentType), "image/svg+xml")
}

func TestProxyInviteServerIconStorageDisabledReturns503(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyinviteiconoff")
	serverID := ts.createTestServer(t, owner, "Proxy Invite Storage Off")
	code := "LMNPQRST"
	ts.setServerIconURL(t, serverID, "server-icons/"+serverID)
	ts.createTestInviteCode(t, serverID, owner, code, false)
	ts.handler.store = nil

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doNoAuth(ts.handler.ProxyInviteServerIcon, "GET", "/api/v1/invites/"+code+"/icon", gin.Params{{Key: "code", Value: code}})
	})
	assertStorageDisabledResponse(t, w)
}

func TestProxyServerBannerSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxysbsuc")
	serverID := ts.createTestServer(t, owner, "ProxySB Server")

	key := fmt.Sprintf(fmtServerBannersKey, serverID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 200, 50)), 200, mimeImageJPEG))

	w := ts.doJSON(ts.handler.ProxyServerBanner, "GET", pathServerBanners+serverID, owner, gin.Params{{Key: "server_id", Value: serverID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImageJPEG, w.Header().Get(hdrContentType))
}

func TestProxyServerBannerInvalidID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "proxysbbad")
	_ = user

	w := ts.doJSON(ts.handler.ProxyServerBanner, "GET", "/api/v1/media/server-banners/not-uuid", user, gin.Params{{Key: "server_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestProxyServerBannerPublic asserts server-banners are now a public Tier 1
// route. See TestProxyServerIconPublic in handlers_test.go for the rationale —
// the unguessable UUID is the only identifier and they need to render via
// plain <img> tags. Membership-based 403 was removed in commit b31f591.
func TestProxyServerBannerPublic(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxysbown")
	outsider := ts.createTestUser(t, "proxysbout")
	serverID := ts.createTestServer(t, owner, "ProxySB NM Server")

	key := fmt.Sprintf(fmtServerBannersKey, serverID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 200, 50)), 200, mimeImageJPEG))

	w := ts.doJSON(ts.handler.ProxyServerBanner, "GET", pathServerBanners+serverID, outsider, gin.Params{{Key: "server_id", Value: serverID}})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestProxyServerBannerNotFound(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxysbnf")
	serverID := ts.createTestServer(t, owner, "ProxySB NF Server")

	w := ts.doJSON(ts.handler.ProxyServerBanner, "GET", pathServerBanners+serverID, owner, gin.Params{{Key: "server_id", Value: serverID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxyServerIconNotFound(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyiconnf")
	serverID := ts.createTestServer(t, owner, "ProxyIcon NF")

	w := ts.doJSON(ts.handler.ProxyServerIcon, "GET", "/api/v1/media/server-icons/"+serverID, owner, gin.Params{{Key: "server_id", Value: serverID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =====================================================================
// Delete: Additional Edge Cases
// =====================================================================

func TestDeleteMediaNonExistentFile(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "delnofile")
	fakeID := uuid.New().String()

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fakeID, user, gin.Params{{Key: "file_id", Value: fakeID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteMediaTier1Rejected(t *testing.T) {
	// DeleteMedia only allows tier 2 (attachments). A tier 1 file should return 404.
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "deltier1")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAvatarsKey, owner)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		 VALUES ($1, $2, 'photo', 1, 'image/png', 100, $3)`,
		fileID, owner, storageKey,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	// tier 1 doesn't match the WHERE media_tier = 2, so 404
	assert.Equal(t, http.StatusNotFound, w.Code)
	// Object should still exist
	assert.True(t, ts.store.hasObject(storageKey))
}

// =====================================================================
// Upload: Avatar re-upload (upsert)
// =====================================================================

func TestUploadAvatarReuploadOverwrites(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "reupavatar")

	// First upload
	imgData1 := makePNG(t, 100, 100)
	body1, ct1 := multipartBody(t, "file", fileAvatarPng, imgData1, nil)
	w1 := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body1, ct1)
	assert.Equal(t, http.StatusCreated, w1.Code)

	// Second upload overwrites the same key
	imgData2 := makePNG(t, 200, 200)
	body2, ct2 := multipartBody(t, "file", fileAvatarPng, imgData2, nil)
	w2 := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body2, ct2)
	assert.Equal(t, http.StatusCreated, w2.Code)

	// The storage key should still exist (overwritten, not duplicated)
	assert.True(t, ts.store.hasObject(fmt.Sprintf(fmtAvatarsKey, userID)))
}

// =====================================================================
// Upload: Empty file detection
// =====================================================================

func TestUploadAvatarEmptyFile(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "emptyavatar")

	body, ct := multipartBody(t, "file", fileAvatarPng, []byte{}, nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// isValidFileType
// =====================================================================

func TestIsValidFileType(t *testing.T) {
	assert.True(t, isValidFileType(FileTypePhoto))
	assert.True(t, isValidFileType(FileTypeAnimated))
	assert.True(t, isValidFileType(FileTypeVideo))
	assert.True(t, isValidFileType(FileTypeAudio))
	assert.True(t, isValidFileType(FileTypeFile))
	assert.False(t, isValidFileType("unknown"))
	assert.False(t, isValidFileType(""))
}

// =====================================================================
// Delete: Successful deletion
// =====================================================================

func TestDeleteMediaSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "delsuccess")
	serverID := ts.createTestServer(t, owner, "Del Success Server")
	channelID := ts.createTestChannel(t, serverID, "delsuccess")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusOK, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, true, resp["deleted"])
	assert.False(t, ts.store.hasObject(storageKey))
}

func TestDeleteMediaWrongOwner(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "delowner")
	other := ts.createTestUser(t, "delother")
	serverID := ts.createTestServer(t, owner, "Del Owner Server")
	channelID := ts.createTestChannel(t, serverID, "delowner")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, other, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.True(t, ts.store.hasObject(storageKey))
}

// =====================================================================
// Proxy: DM Icon
// =====================================================================

func TestProxyDMIconInvalidID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "proxydmbad")

	w := ts.doJSON(ts.handler.ProxyDMIcon, "GET", "/api/v1/media/dm-icons/not-uuid", user, gin.Params{{Key: "conversationId", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// Tier 1 Upload: Tier1StorageKey variants
// =====================================================================

func TestTier1StorageKeyAvatar(t *testing.T) {
	assert.Equal(t, "avatars/u1", tier1StorageKey("avatar", "u1", "", ""))
}

func TestTier1StorageKeyBanner(t *testing.T) {
	assert.Equal(t, "banners/u1", tier1StorageKey("banner", "u1", "", ""))
}

func TestTier1StorageKeyServerIcon(t *testing.T) {
	assert.Equal(t, "server-icons/s1", tier1StorageKey(purposeServerIcon, "", "s1", ""))
}

func TestTier1StorageKeyServerBanner(t *testing.T) {
	assert.Equal(t, "server-banners/s1", tier1StorageKey(purposeServerBanner, "", "s1", ""))
}

func TestTier1StorageKeyDMIcon(t *testing.T) {
	assert.Equal(t, "dm-icons/c1", tier1StorageKey(purposeDMIcon, "", "", "c1"))
}

// =====================================================================
// Download: Channel success path (exercises streaming)
// =====================================================================

func TestDownloadAttachmentChannelSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "dlchansuc")
	serverID := ts.createTestServer(t, owner, "DL Chan Server")
	channelID := ts.createTestChannel(t, serverID, "dlchansuc")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	payload := []byte("channel-cipher-data")
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader(payload), int64(len(payload)), mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'image/jpeg', $3, $4, 1, $5)`,
		fileID, owner, len(payload), storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(payload), w.Body.String())
	assert.Equal(t, "image/jpeg", w.Header().Get("X-File-Mime-Type"))
	assert.Equal(t, mimeOctetStream, w.Header().Get(hdrContentType))
}

// =====================================================================
// Upload: Store failure paths
// =====================================================================

func TestUploadAttachmentStoreFailure(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "attachstorefail")
	serverID := ts.createTestServer(t, owner, "StoreFail Server")
	channelID := ts.createTestChannel(t, serverID, "storefail")

	ts.store.putErr = fmt.Errorf("storage unavailable")
	defer func() { ts.store.putErr = nil }()

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte(testCiphertextData), map[string]string{
		keyChannelID: channelID,
		keyFileType:  "file",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Failed to store file")
}

func TestUploadAvatarStoreFailure(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "avatarstorefail")

	ts.store.putErr = fmt.Errorf("storage unavailable")
	defer func() { ts.store.putErr = nil }()

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", "avatar.png", imgData, nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Failed to store image")
}

// =====================================================================
// Tier1StorageKey: fallback branch
// =====================================================================

func TestTier1StorageKeyFallback(t *testing.T) {
	key := tier1StorageKey("unknown-purpose", "user123", "", "")
	assert.Equal(t, "media/unknown-purpose/user123", key)
}
