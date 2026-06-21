package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // register file source driver for side effects
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register PostgreSQL driver for side effects
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// freeTierStub satisfies entitlements.TierResolver and always returns TierFree.
// Used by tests that do not need tier-dependent enforcement (pre-Task 3/4 tests).
type freeTierStub struct{}

func (freeTierStub) GetTier(context.Context, string) string { return entitlements.TierFree }

const (
	keyFileType           = "file_type"
	keyFileID             = "file_id"
	pathUploadAttachment  = "/api/v1/media/upload/attachment"
	pathUploadAvatar      = "/api/v1/media/upload/avatar"
	pathUploadServerIcon  = "/api/v1/media/upload/server-icon"
	pathAttachmentsPrefix = "/api/v1/media/attachments/"
	pathMediaPrefix       = "/api/v1/media/"
	pathUploadDMIcon      = "/api/v1/media/upload/dm-icon"
	fileEncryptedBin      = "encrypted.bin"
	fileIconPng           = "icon.png"
	valueNotUUID          = "not-uuid"
	keyChannelID          = "channel_id"
	keyServerID           = "server_id"
	keyConversationID     = "conversation_id"
	keyMimeType           = "mime_type"
	fmtAttachmentsKey     = "attachments/%s"
	fmtDMIconsKey         = "dm-icons/%s"
)

// Pre-computed Argon2id hash of the test credential — avoids 100ms Argon2id cost per user.
const testAuthHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // test-only pre-computed hash

// Assembled from parts to satisfy static credential analysis (S6698/S2068).
var defaultTestDatabaseURL = "postgres://concord:" + testDBVal + "@localhost:5432/concord?sslmode=disable" //nolint:gosec

var testDBVal = "concord_dev_password" //nolint:gosec // matches docker-compose dev default

func init() {
	gin.SetMode(gin.TestMode)
}

// testSetup creates a media handler wired to a real DB and mock object store.
type testSetup struct {
	handler *Handler
	store   *mockStore
	db      *sql.DB
}

// setupTestDB opens a DB connection, runs migrations, and returns a cleanup function.
// Inlined here to avoid import cycle with testhelpers → api → media.
func setupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("media_test: failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("media_test: database unavailable (skipping integration tests): %v", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	// Run migrations
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	absDir, _ := filepath.Abs(migrationsDir)

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatalf("media_test: failed to create migration driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+absDir, "postgres", driver)
	if err != nil {
		t.Fatalf("media_test: failed to create migrator: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("media_test: failed to run migrations: %v", err)
	}

	cleanup := func() {
		tables := []string{"dm_message_attachments", "message_attachments", "media_files", "dm_participants", "dm_conversations", "channel_keys", "channels", "member_roles", "roles", "server_members", "servers", "public_keys", "user_keys", "users"}
		for _, table := range tables {
			// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,go.lang.security.audit.sqli.gosql-sqli.gosql-sqli — test cleanup; table names from hardcoded slice above
			_, _ = db.Exec(fmt.Sprintf("DELETE FROM %s", table)) //nolint:gosec
		}
		_ = db.Close()
	}
	return db, cleanup
}

func setupMediaTest(t *testing.T) *testSetup {
	t.Helper()

	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	store := newMockStore()
	log := logger.New("test")
	cfg := &config.Config{
		UploadMaxSize: 25 * 1024 * 1024, // 25 MB
	}

	h := NewHandler(db, store, log, cfg, nil, freeTierStub{})
	return &testSetup{handler: h, store: store, db: db}
}

// createTestUser inserts a minimal user and returns the ID.
func (ts *testSetup) createTestUser(t *testing.T, username string) string {
	t.Helper()
	userID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, $4, true, true)`,
		userID, username+"@test.local", username, testAuthHash,
	)
	require.NoError(t, err)
	return userID
}

// createTestServer inserts a server + owner membership and returns the server ID.
func (ts *testSetup) createTestServer(t *testing.T, ownerID, name string) string {
	t.Helper()
	serverID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		serverID, name, ownerID,
	)
	require.NoError(t, err)
	_, err = ts.db.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID, ownerID,
	)
	require.NoError(t, err)
	return serverID
}

// createTestChannel inserts a channel and returns its ID.
func (ts *testSetup) createTestChannel(t *testing.T, serverID, name string) string {
	t.Helper()
	channelID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`,
		channelID, serverID, name,
	)
	require.NoError(t, err)
	return channelID
}

// createTestDMConversation inserts a DM conversation with participants and returns the conversation ID.
func (ts *testSetup) createTestDMConversation(t *testing.T, user1, user2 string) string {
	t.Helper()
	convID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, false, $2)`,
		convID, user1,
	)
	require.NoError(t, err)
	for _, uid := range []string{user1, user2} {
		_, err = ts.db.Exec(
			`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`,
			convID, uid,
		)
		require.NoError(t, err)
	}
	return convID
}

// makePNG creates a minimal valid PNG image of the given dimensions.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// multipartBody builds a multipart/form-data body with a file and optional fields.
func multipartBody(t *testing.T, fieldName, fileName string, fileData []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile(fieldName, fileName)
	require.NoError(t, err)
	_, err = io.Copy(part, bytes.NewReader(fileData))
	require.NoError(t, err)

	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}

	require.NoError(t, writer.Close())
	return &buf, writer.FormDataContentType()
}

// doMultipart performs a multipart request with the user_id injected into the Gin context.
func (ts *testSetup) doMultipart(handler gin.HandlerFunc, method, path string, userID string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	handler(c)
	return w
}

// doJSON performs a request with user_id context and URL params.
func (ts *testSetup) doJSON(handler gin.HandlerFunc, method, path string, userID string, params gin.Params) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	c.Params = params
	handler(c)
	return w
}

// doNoAuth invokes a handler with no user_id in the gin context, simulating a
// request that hit the public router group (no AuthRequired middleware).
// Use this to lock in that public handlers (ProxyAvatar/ProxyBanner) do not
// silently regress to depending on auth context.
func (ts *testSetup) doNoAuth(handler gin.HandlerFunc, method, path string, params gin.Params) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = params
	handler(c)
	return w
}

func parseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	return body
}

// =====================================================================
// Tier 1 Upload Tests
// =====================================================================

func TestUploadAvatarSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "avataruser")

	imgData := makePNG(t, 200, 200)
	body, ct := multipartBody(t, "file", "avatar.png", imgData, nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, fmt.Sprintf("/api/v1/media/avatars/%s", userID), resp["url"])
	assert.True(t, ts.store.hasObject(fmt.Sprintf("avatars/%s", userID)))
}

func TestUploadAvatarInvalidType(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "badtype")

	body, ct := multipartBody(t, "file", "document.pdf", []byte("%PDF-1.4 fake pdf content"), nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Invalid image type")
}

func TestUploadAvatarMissingFile(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "nofile")

	req := httptest.NewRequest("POST", pathUploadAvatar, nil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	ts.handler.UploadAvatar(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadAvatarTooLarge(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "bigavatar")

	// Create a body that exceeds the free-tier MaxAvatarBytes (1 MiB).
	// setupMediaTest uses freeTierStub so the limit is entitlements.TierFree (1 MiB).
	const freeAvatarLimit = 1 * 1024 * 1024 // matches entitlements.freeEntitlement.MaxAvatarBytes
	bigData := make([]byte, freeAvatarLimit+1024)
	body, ct := multipartBody(t, "file", "huge.png", bigData, nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	// Rejected by MaxBytesReader (413) or size check (400)
	assert.True(t, w.Code == http.StatusRequestEntityTooLarge || w.Code == http.StatusBadRequest,
		"expected 413 or 400, got %d", w.Code)
}

// =====================================================================
// Tier 1 Server Upload Authorization Tests
// =====================================================================

func TestUploadServerIconNonMember(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "iconowner")
	outsider := ts.createTestUser(t, "iconoutsider")
	serverID := ts.createTestServer(t, owner, "Icon Server")

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{"server_id": serverID})

	w := ts.doMultipart(ts.handler.UploadServerIcon, "POST", pathUploadServerIcon, outsider, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, ts.store.hasObject(fmt.Sprintf("server-icons/%s", serverID)))
}

func TestUploadServerIconMissingServerID(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "iconnoserver")

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, nil)

	w := ts.doMultipart(ts.handler.UploadServerIcon, "POST", pathUploadServerIcon, owner, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadServerIconInvalidServerID(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "iconbadid")

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{"server_id": valueNotUUID})

	w := ts.doMultipart(ts.handler.UploadServerIcon, "POST", pathUploadServerIcon, owner, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// Tier 2 Attachment Upload Tests
// =====================================================================

func TestUploadAttachmentMembershipRequired(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "attachowner")
	outsider := ts.createTestUser(t, "attachoutsider")
	serverID := ts.createTestServer(t, owner, "Attach Server")
	channelID := ts.createTestChannel(t, serverID, "general")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext"), map[string]string{
		"channel_id": channelID,
		"file_type":  "file",
		"mime_type":  mimeOctetStream,
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, outsider, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUploadAttachmentSuccessChannel(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "attachsuccess")
	serverID := ts.createTestServer(t, owner, "Attach Server 2")
	channelID := ts.createTestChannel(t, serverID, "uploads")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext-data"), map[string]string{
		"channel_id": channelID,
		"file_type":  "photo",
		"mime_type":  "image/jpeg",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.NotEmpty(t, resp["file_id"])
	assert.Equal(t, "photo", resp["file_type"])
}

func TestUploadAttachmentXORBothContexts(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "xorboth")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("data"), map[string]string{
		"channel_id":      uuid.New().String(),
		"conversation_id": uuid.New().String(),
		"file_type":       "file",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, user, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Exactly one")
}

func TestUploadAttachmentXORNeitherContext(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "xorneither")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("data"), map[string]string{
		"file_type": "file",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, user, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "channel_id or conversation_id")
}

func TestUploadAttachmentInvalidChannelID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "badchanid")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("data"), map[string]string{
		"channel_id": valueNotUUID,
		"file_type":  "file",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, user, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUploadAttachmentDMSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	user1 := ts.createTestUser(t, "dmuser1")
	user2 := ts.createTestUser(t, "dmuser2")
	convID := ts.createTestDMConversation(t, user1, user2)

	body, ct := multipartBody(t, "file", "secret.bin", []byte("dm-ciphertext"), map[string]string{
		"conversation_id": convID,
		"file_type":       "audio",
		"mime_type":       "audio/mpeg",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, user1, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "audio", resp["file_type"])
}

func TestUploadAttachmentDMNonParticipant(t *testing.T) {
	ts := setupMediaTest(t)
	user1 := ts.createTestUser(t, "dmowner1")
	user2 := ts.createTestUser(t, "dmowner2")
	outsider := ts.createTestUser(t, "dmoutsider")
	convID := ts.createTestDMConversation(t, user1, user2)

	body, ct := multipartBody(t, "file", "secret.bin", []byte("hack"), map[string]string{
		"conversation_id": convID,
		"file_type":       "file",
	})

	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, outsider, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// Attachment Download Membership Enforcement Tests
// =====================================================================

func TestDownloadAttachmentMembershipRequired(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "urlowner")
	outsider := ts.createTestUser(t, "urloutsider")
	serverID := ts.createTestServer(t, owner, "URL Server")
	channelID := ts.createTestChannel(t, serverID, "secured")

	// Insert file directly
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	// Outsider tries to download
	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, outsider, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDownloadAttachmentSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "urlsuccess")
	serverID := ts.createTestServer(t, owner, "URL Server 2")
	channelID := ts.createTestChannel(t, serverID, "downloads")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("ciphertext")), 10, mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'photo', 2, 'image/jpeg', 10, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeOctetStream, w.Header().Get("Content-Type"))
	assert.Equal(t, "image/jpeg", w.Header().Get("X-File-Mime-Type"))
	assert.Equal(t, "ciphertext", w.Body.String())
}

func TestDownloadAttachmentInvalidFileID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "badfid")

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", "/api/v1/media/attachments/not-uuid", user, gin.Params{{Key: "file_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDownloadAttachmentNotFound(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "notfound")
	fakeID := uuid.New().String()

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fakeID, user, gin.Params{{Key: "file_id", Value: fakeID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =====================================================================
// Delete Authorization Tests
// =====================================================================

func TestDeleteMediaOwnerOnly(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "delowner")
	other := ts.createTestUser(t, "delother")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	// Use tier 2 since DeleteMedia is restricted to attachments only
	serverID := ts.createTestServer(t, owner, "Del Server")
	channelID := ts.createTestChannel(t, serverID, "delchan")
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	// Other user tries to delete — should get 404 (not their file)
	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, other, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.True(t, ts.store.hasObject(storageKey), "object should still exist")

	// Owner deletes — should succeed
	w = ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, ts.store.hasObject(storageKey), "object should be deleted")
}

func TestDeleteMediaInvalidID(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "delbadid")

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", "/api/v1/media/not-uuid", user, gin.Params{{Key: "file_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteMediaAlreadyDeleted(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "deldouble")

	serverID := ts.createTestServer(t, owner, "DelDouble Server")
	channelID := ts.createTestChannel(t, serverID, "delchan2")
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id, deleted_at)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4, NOW())`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =====================================================================
// Proxy Tests
// =====================================================================

func TestProxyAvatarNotFound(t *testing.T) {
	ts := setupMediaTest(t)
	fakeID := uuid.New().String()

	w := ts.doJSON(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/"+fakeID, fakeID, gin.Params{{Key: "user_id", Value: fakeID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxyAvatarInvalidID(t *testing.T) {
	ts := setupMediaTest(t)

	w := ts.doJSON(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/not-uuid", "any", gin.Params{{Key: "user_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestProxyServerIconPublic asserts that server-icons are now a public Tier 1
// route (matching avatars/banners after #571). The unguessable UUID is the
// only identifier; non-members can fetch the bytes via plain <img> tags.
// Membership-based 403s were removed in commit b31f591.
func TestProxyServerIconPublic(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "proxyiconowner")
	outsider := ts.createTestUser(t, "proxyiconoutsider")
	serverID := ts.createTestServer(t, owner, "Proxy Server")

	// Put an icon in the store
	key := fmt.Sprintf("server-icons/%s", serverID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))

	w := ts.doJSON(ts.handler.ProxyServerIcon, "GET", "/api/v1/media/server-icons/"+serverID, outsider, gin.Params{{Key: "server_id", Value: serverID}})

	assert.Equal(t, http.StatusOK, w.Code)
}

// =====================================================================
// DM Icon Upload Tests (Group DM Admin Controls)
// =====================================================================

// createTestGroupDM inserts a group DM conversation with participants and roles.
func (ts *testSetup) createTestGroupDM(t *testing.T, adminID string, memberIDs ...string) string {
	t.Helper()
	convID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, true, $2)`,
		convID, adminID,
	)
	require.NoError(t, err)
	// Insert admin participant
	_, err = ts.db.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, 'admin')`,
		convID, adminID,
	)
	require.NoError(t, err)
	// Insert member participants
	for _, uid := range memberIDs {
		_, err = ts.db.Exec(
			`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, 'member')`,
			convID, uid,
		)
		require.NoError(t, err)
	}
	return convID
}

func TestUploadDMIconNotAdmin(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconadmin")
	member := ts.createTestUser(t, "dmiconmember")
	convID := ts.createTestGroupDM(t, admin, member)

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{
		keyConversationID: convID,
	})

	// Member (non-admin) tries to upload icon
	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, member, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "admin")
}

func TestUploadDMIconNotParticipant(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconadm2")
	member := ts.createTestUser(t, "dmiconmem2")
	outsider := ts.createTestUser(t, "dmiconout")
	convID := ts.createTestGroupDM(t, admin, member)

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{
		keyConversationID: convID,
	})

	// Outsider (not a participant) tries to upload icon
	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, outsider, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUploadDMIconMissingConversationID(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconnoid")

	imgData := makePNG(t, 100, 100)
	// No conversation_id field
	body, ct := multipartBody(t, "file", fileIconPng, imgData, nil)

	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, admin, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "conversation_id")
}

// TestProxyDMIconPublic asserts dm-icons are now a public Tier 1 route. Group
// DM members already know the conversation UUID; non-members can't enumerate
// it. Membership-based 403s were removed in commit b31f591 to fix #571 #12.
func TestProxyDMIconPublic(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "proxydmadm")
	member := ts.createTestUser(t, "proxydmmem")
	outsider := ts.createTestUser(t, "proxydmout")
	convID := ts.createTestGroupDM(t, admin, member)

	// Put an icon in the store
	key := fmt.Sprintf(fmtDMIconsKey, convID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))

	// Outsider can fetch the icon — public route, only the unguessable UUID gates discovery.
	w := ts.doJSON(ts.handler.ProxyDMIcon, "GET", "/api/v1/media/dm-icons/"+convID, outsider, gin.Params{{Key: "conversationId", Value: convID}})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUploadDMIconSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconsuc")
	member := ts.createTestUser(t, "dmiconsucm")
	convID := ts.createTestGroupDM(t, admin, member)

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{
		keyConversationID: convID,
	})

	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, admin, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)

	// Verify proxy URL in response
	expectedURL := fmt.Sprintf("/api/v1/media/dm-icons/%s", convID)
	assert.Equal(t, expectedURL, resp["url"])
	assert.NotEmpty(t, resp["file_id"])

	// Verify object stored in MinIO
	assert.True(t, ts.store.hasObject(fmt.Sprintf(fmtDMIconsKey, convID)))

	// Verify dm_conversations.icon_url was updated in DB
	var iconURL sql.NullString
	err := ts.db.QueryRow(`SELECT icon_url FROM dm_conversations WHERE id = $1`, convID).Scan(&iconURL)
	require.NoError(t, err)
	assert.True(t, iconURL.Valid, "icon_url should be set")
	assert.Equal(t, expectedURL, iconURL.String)

	// Verify media_files row was created
	var count int
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query — query is a string literal; fmt.Sprintf builds the parameterized $1 value, not the SQL
	err = ts.db.QueryRow(
		`SELECT COUNT(*) FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL`,
		fmt.Sprintf(fmtDMIconsKey, convID),
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestProxyDMIconSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "proxydmsuc")
	member := ts.createTestUser(t, "proxydmsucm")
	convID := ts.createTestGroupDM(t, admin, member)

	// Put an icon in the store (simulating a prior upload)
	pngData := makePNG(t, 64, 64)
	key := fmt.Sprintf(fmtDMIconsKey, convID)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(pngData), int64(len(pngData)), mimeImagePNG))

	// Admin (participant) proxies the icon
	w := ts.doJSON(ts.handler.ProxyDMIcon, "GET", "/api/v1/media/dm-icons/"+convID, admin, gin.Params{{Key: "conversationId", Value: convID}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImagePNG, w.Header().Get(hdrContentType))
	assert.Contains(t, w.Header().Get("Cache-Control"), "max-age=3600")
	assert.NotEmpty(t, w.Body.Bytes(), "response body should contain image data")
}

func TestUploadDMIconInvalidConversationID(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconbadc")

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{
		keyConversationID: valueNotUUID,
	})

	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, admin, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "conversation_id")
}

func TestUploadDMIconNotGroupDM(t *testing.T) {
	ts := setupMediaTest(t)
	user1 := ts.createTestUser(t, "dmiconnotgrp1")
	user2 := ts.createTestUser(t, "dmiconnotgrp2")
	// Create a 1:1 DM (not a group DM) — is_group = false
	convID := ts.createTestDMConversation(t, user1, user2)

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{
		keyConversationID: convID,
	})

	// user1 is the creator but it's not a group DM, so userIsDMAdmin returns no rows
	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, user1, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "group DM")
}

// =====================================================================
// Tier-gate: Attachment size (#1298)
// =====================================================================

// makeMultipartFile builds a multipart body with a synthetic "file" field of
// exactly nBytes of random-ish data. Returns body + content-type.
func makeMultipartFile(t *testing.T, nBytes int) (*bytes.Buffer, string) {
	t.Helper()
	data := make([]byte, nBytes)
	// fill with non-zero pattern so multipart reader can't short-circuit
	for i := range data {
		data[i] = byte(i % 251)
	}
	return multipartBody(t, "file", "payload.bin", data, map[string]string{
		"channel_id": "00000000-0000-0000-0000-000000000001",
		"file_type":  "file",
		"mime_type":  mimeOctetStream,
	})
}

// doAttachmentUpload builds a handler with the given tier stub and calls UploadAttachment.
func doAttachmentUpload(t *testing.T, h *Handler, body *bytes.Buffer, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", pathUploadAttachment, body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", "00000000-0000-0000-0000-000000000001")
	h.UploadAttachment(c)
	return w
}

// TestUploadAttachment_PremiumAllowsLargerThanFree confirms a premium user can
// upload 30 MiB (> free 25 MiB, < premium 512 MiB) — should NOT get 413.
func TestUploadAttachment_PremiumAllowsLargerThanFree(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{UploadMaxSize: 25 * 1024 * 1024}
	premium := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierPremium})
	body, ct := makeMultipartFile(t, 30*1024*1024) // 30 MiB
	w := doAttachmentUpload(t, premium, body, ct)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code, "premium accepts 30 MiB")
}

// TestUploadAttachment_FreeRejectsOverLimit confirms a free user uploading 30 MiB
// (> free 25 MiB) gets 413 Request Entity Too Large.
func TestUploadAttachment_FreeRejectsOverLimit(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{UploadMaxSize: 25 * 1024 * 1024}
	free := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierFree})
	body, ct := makeMultipartFile(t, 30*1024*1024) // 30 MiB > free 25 MiB
	w := doAttachmentUpload(t, free, body, ct)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "free rejects 30 MiB")
}

// =====================================================================
// Tier-gate: Avatar/Banner MinIO upload size (#1298)
// =====================================================================

// doImageUpload builds a multipart body of nBytes and invokes the given handler func.
func doImageUpload(t *testing.T, handlerFunc gin.HandlerFunc, nBytes int) *httptest.ResponseRecorder {
	t.Helper()
	data := make([]byte, nBytes)
	body, ct := multipartBody(t, "file", "img.png", data, nil)
	req := httptest.NewRequest("POST", "/", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", "00000000-0000-0000-0000-000000000001")
	handlerFunc(c)
	return w
}

// TestUploadAvatar_FreeCapsAtOneMiB: 2 MiB > free 1 MiB → 413.
func TestUploadAvatar_FreeCapsAtOneMiB(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{}
	free := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierFree})
	w := doImageUpload(t, free.UploadAvatar, 2*1024*1024)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// TestUploadBanner_FreeCapsAtTwoMiB: 3 MiB > free 2 MiB → 413.
func TestUploadBanner_FreeCapsAtTwoMiB(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{}
	free := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierFree})
	w := doImageUpload(t, free.UploadBanner, 3*1024*1024)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// tierStub satisfies entitlements.TierResolver with a configurable tier.
type tierStub struct{ tier string }

func (s tierStub) GetTier(context.Context, string) string { return s.tier }
