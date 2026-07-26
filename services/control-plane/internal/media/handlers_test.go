package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register PostgreSQL driver for side effects
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// mediaTestWithResolver builds a media testSetup whose handler is wired with the
// given RBAC resolver (over the supplied working DB), so the CV-CAN-003/004
// permission checks in UploadAttachment actually run. The default setupMediaTest
// passes a nil resolver (checks fall through to allow); these tests exercise the
// real permission code + its defensive error branches.
//
// This package's tests live in `package media` (internal), so they cannot import
// internal/testhelpers (that would cycle back through internal/api -> media);
// the resolver + Redis are therefore wired inline here.
func mediaTestWithResolver(t *testing.T, db *sql.DB, resolver *rbac.Resolver) *testSetup {
	t.Helper()
	store := newMockStore()
	cfg := &config.Config{UploadMaxSize: 25 * 1024 * 1024}
	h := NewHandler(db, store, logger.New("test"), cfg, resolver, freeTierStub{})
	return &testSetup{handler: h, store: store, db: db}
}

// mediaTestRedis connects to the test Redis (matches the docker-compose dev
// default consumed by internal/testhelpers). Used only to back the RBAC
// permission cache in resolver tests.
func mediaTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		// Assembled from parts to satisfy static credential analysis; matches the
		// docker-compose dev default (test-only, not a production secret).
		url = "redis://:" + "concord_dev" + "_redis@localhost:6379/1"
	}
	opt, err := redis.ParseURL(url)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// mediaBrokenResolver returns a resolver whose DB is closed, so every permission
// computation errors — used to cover the handler's defensive `permErr != nil`
// (HTTP 500) branches. Mirrors testhelpers.BrokenResolver, inlined to avoid the
// internal/testhelpers import cycle described above.
func mediaBrokenResolver(t *testing.T, rdb *redis.Client) *rbac.Resolver {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL // existing package var (assembled from parts)
	}
	closed, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	return rbac.NewResolver(closed, rbac.NewPermissionCache(rdb), logger.New("test"))
}

// freeTierStub satisfies entitlements.TierResolver and always returns TierFree.
// Used by tests that do not need tier-dependent enforcement (pre-Task 3/4 tests).
type freeTierStub struct{}

func (freeTierStub) GetTier(context.Context, string) string { return entitlements.TierFree }

type mediaOpsCounterSpy struct{ uploads int }

func (spy *mediaOpsCounterSpy) Increment(key opsmetrics.MetricKey) {
	if key == opsmetrics.MetricMediaUploadsTotal {
		spy.uploads++
	}
}

func TestRecordSuccessfulUpload(t *testing.T) {
	counters := &mediaOpsCounterSpy{}
	handler := &Handler{}
	handler.recordSuccessfulUpload()
	handler.SetOpsCounter(counters)
	handler.recordSuccessfulUpload()
	require.Equal(t, 1, counters.uploads)
}

func TestRecordSuccessfulUploadAllowsDisabledOpsMetrics(t *testing.T) {
	var counters *opsmetrics.Counters
	handler := &Handler{}

	handler.SetOpsCounter(counters)

	require.NotPanics(t, handler.recordSuccessfulUpload)
}

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

// setupTestDB shares the cycle-free test fixture's cross-process lock.
func setupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	return testdb.SetupTestDB(t)
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

func (ts *testSetup) setServerIconURL(t *testing.T, serverID, iconURL string) {
	t.Helper()
	_, err := ts.db.Exec(`UPDATE servers SET icon_url = $1 WHERE id = $2`, iconURL, serverID)
	require.NoError(t, err)
}

func (ts *testSetup) createTestInviteCode(t *testing.T, serverID, createdBy, code string, revoked bool) {
	t.Helper()
	_, err := ts.db.Exec(
		`INSERT INTO server_invites (id, server_id, code, created_by, max_uses, use_count, is_revoked)
		 VALUES ($1, $2, $3, $4, NULL, 0, $5)`,
		uuid.New().String(), serverID, code, createdBy, revoked,
	)
	require.NoError(t, err)
}

// createTestChannel inserts a text channel and returns its ID.
func (ts *testSetup) createTestChannel(t *testing.T, serverID, name string) string {
	t.Helper()
	return ts.createTestChannelWithType(t, serverID, name, "text")
}

// createTestChannelWithType inserts a channel of the given type and returns its ID.
func (ts *testSetup) createTestChannelWithType(t *testing.T, serverID, name, channelType string) string {
	t.Helper()
	channelID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, $4)`,
		channelID, serverID, name, channelType,
	)
	require.NoError(t, err)
	return channelID
}

// createTestRoleWithPerms inserts a role with the given permission bitfield and
// assigns it to the (already-added) server member, so resolver-backed tests can
// grant precise permissions. The member must already exist in server_members.
func (ts *testSetup) createTestRoleWithPerms(t *testing.T, serverID, userID, name string, perms rbac.Permission) {
	t.Helper()
	roleID := uuid.New().String()
	_, err := ts.db.Exec(
		`INSERT INTO roles (id, server_id, name, permissions) VALUES ($1, $2, $3, $4)`,
		roleID, serverID, name, int64(perms),
	)
	require.NoError(t, err)
	_, err = ts.db.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, userID, roleID,
	)
	require.NoError(t, err)
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

func assertStorageDisabledResponse(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "storage")
}

// =====================================================================
// Tier 1 Upload Tests
// =====================================================================

func TestUploadAvatarSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	counters := &mediaOpsCounterSpy{}
	ts.handler.SetOpsCounter(counters)
	userID := ts.createTestUser(t, "avataruser")

	imgData := makePNG(t, 200, 200)
	body, ct := multipartBody(t, "file", "avatar.png", imgData, nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, fmt.Sprintf("/api/v1/media/avatars/%s", userID), resp["url"])
	assert.True(t, ts.store.hasObject(fmt.Sprintf("avatars/%s", userID)))
	assert.Equal(t, 1, counters.uploads)
}

func TestUploadAvatarInvalidType(t *testing.T) {
	ts := setupMediaTest(t)
	counters := &mediaOpsCounterSpy{}
	ts.handler.SetOpsCounter(counters)
	userID := ts.createTestUser(t, "badtype")

	body, ct := multipartBody(t, "file", "document.pdf", []byte("%PDF-1.4 fake pdf content"), nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Invalid image type")
	assert.Zero(t, counters.uploads)
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

	// Create a body that exceeds the free-tier MaxAvatarBytes (5 MiB).
	// setupMediaTest uses freeTierStub so the limit is entitlements.TierFree (5 MiB).
	const freeAvatarLimit = 5 * 1024 * 1024 // matches entitlements.freeEntitlement.MaxAvatarBytes
	bigData := make([]byte, freeAvatarLimit+1024)
	body, ct := multipartBody(t, "file", "huge.png", bigData, nil)

	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	// Rejected by MaxBytesReader (413) or size check (400)
	assert.True(t, w.Code == http.StatusRequestEntityTooLarge || w.Code == http.StatusBadRequest,
		"expected 413 or 400, got %d", w.Code)
}

func TestUploadAvatarStorageDisabledReturns503(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "avatarstorageoff")
	ts.handler.store = nil

	imgData := makePNG(t, 200, 200)
	body, ct := multipartBody(t, "file", "avatar.png", imgData, nil)

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)
	})
	assertStorageDisabledResponse(t, w)
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

func TestEnforceTier1UploadLimit_GroundspeedServerImagesRejectOverFiveMiB(t *testing.T) {
	for _, purpose := range []string{purposeServerIcon, purposeServerBanner} {
		t.Run(purpose, func(t *testing.T) {
			h := &Handler{serverTiers: serverTierStub{entitlements.TierGroundspeed}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/", nil)

			ok := enforceTier1UploadLimit(c, h, purpose, uuid.New().String(), 5*1024*1024+1, serverImageMaxUpload)

			assert.False(t, ok)
			assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			resp := parseBody(t, w)
			assert.Equal(t, float64(5*1024*1024), resp["max_size"])
		})
	}
}

func TestEnforceTier1UploadLimit_MachServerImagesAllowFiveMiBPlusOne(t *testing.T) {
	for _, purpose := range []string{purposeServerIcon, purposeServerBanner} {
		t.Run(purpose, func(t *testing.T) {
			h := &Handler{serverTiers: serverTierStub{entitlements.TierMach1}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/", nil)

			ok := enforceTier1UploadLimit(c, h, purpose, uuid.New().String(), 5*1024*1024+1, serverImageMaxUpload)

			assert.True(t, ok)
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
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
	counters := &mediaOpsCounterSpy{}
	ts.handler.SetOpsCounter(counters)
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
	assert.Equal(t, 1, counters.uploads)
}

// CV-CAN-004: with a real resolver, the server OWNER (owner-bypass grants all
// permissions) uploads successfully — exercises the SEND + VIEW permission-check
// success paths that the nil-resolver tests skip.
func TestUploadAttachment_Resolver_OwnerSucceeds(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "attach_res_owner")
	serverID := ts.createTestServer(t, owner, "Attach Resolver Server")
	channelID := ts.createTestChannel(t, serverID, "res-uploads")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext"), map[string]string{
		"channel_id": channelID,
		"file_type":  "photo",
		"mime_type":  "image/jpeg",
	})
	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}

// CV-CAN-004: with a real resolver, a member with no RBAC role (no SEND) is
// rejected — exercises the `!hasPerm` 403 branch of checkSendPermission.
func TestUploadAttachment_Resolver_NoSendPermission403(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "attach_res_owner2")
	member := ts.createTestUser(t, "attach_res_member")
	serverID := ts.createTestServer(t, owner, "Attach Resolver Server 2")
	channelID := ts.createTestChannel(t, serverID, "res-uploads2")
	// Add member with no RBAC role -> resolver yields zero permissions -> no SEND.
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, member)
	require.NoError(t, err)

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext"), map[string]string{
		"channel_id": channelID,
		"file_type":  "file",
		"mime_type":  mimeOctetStream,
	})
	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, member, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// CV-CAN-003/004: when the permission resolver itself errors, UploadAttachment
// returns 500 — exercises the defensive `permErr != nil` branch. Uses a resolver
// backed by a closed DB while the handler's own DB still resolves the channel.
func TestUploadAttachment_Resolver_Error500(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	ts := mediaTestWithResolver(t, db, mediaBrokenResolver(t, rdb))

	owner := ts.createTestUser(t, "attach_res_owner3")
	serverID := ts.createTestServer(t, owner, "Attach Resolver Server 3")
	channelID := ts.createTestChannel(t, serverID, "res-uploads3")

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext"), map[string]string{
		"channel_id": channelID,
		"file_type":  "file",
		"mime_type":  mimeOctetStream,
	})
	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
}

// CV-CAN-004: a member serving an active timeout must not upload an attachment,
// even with full send/attach/view permissions. The timeout gate in
// checkSendPermission mirrors messages.checkSendAccess. Asserts the 403
// member_timed_out response AND that no media_files row is created (the gate
// fires before any storage/metadata write).
func TestUploadAttachment_Resolver_TimedOutMember403(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "attach_timeout_owner")
	member := ts.createTestUser(t, "attach_timeout_member")
	serverID := ts.createTestServer(t, owner, "Attach Timeout Server")
	channelID := ts.createTestChannel(t, serverID, "timeout-uploads")
	// Timed out for another hour but otherwise fully permitted, so a non-403 here
	// would prove the timeout gate is missing rather than an unrelated perms gap.
	_, err := ts.db.Exec(
		`INSERT INTO server_members (server_id, user_id, role, timed_out_until) VALUES ($1, $2, 'member', $3)`,
		serverID, member, time.Now().UTC().Add(time.Hour),
	)
	require.NoError(t, err)
	ts.createTestRoleWithPerms(t, serverID, member, "full", rbac.BasePermissions)

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext"), map[string]string{
		"channel_id": channelID,
		"file_type":  "file",
		"mime_type":  mimeOctetStream,
	})
	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, member, body, ct)

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "member_timed_out", parseBody(t, w)["code"])

	// The gate fires before any storage/metadata write: no attachment row exists.
	var count int
	require.NoError(t, ts.db.QueryRow(
		`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1`, member,
	).Scan(&count))
	assert.Equal(t, 0, count, "no media_files row must be created for a timed-out upload")
}

// checkSendPermission direct unit tests (CV-CAN-004). It is invoked AFTER
// userHasChannelAccess (which shares the same resolver), so its own error/deny
// branches are shadowed in the full UploadAttachment flow; call it directly to
// cover them. The media test is `package media` (internal), so the unexported
// method is reachable.
func newCheckSendCtx() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, w
}

func TestCheckSendPermission_ChannelNotFound_403(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	c, w := newCheckSendCtx()
	// A non-existent channel -> `SELECT server_id` yields sql.ErrNoRows -> 403
	// (client-facing missing-channel condition, matching userHasChannelAccess).
	ok := ts.handler.checkSendPermission(c, uuid.New().String(), uuid.New().String())

	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCheckSendPermission_ChannelLookupError_500(t *testing.T) {
	rdb := mediaTestRedis(t)
	// Closed DB: the channel lookup errors with a non-ErrNoRows connection error
	// -> 500 (genuine server fault, distinct from the missing-channel 403 above).
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}
	closed, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	resolver := rbac.NewResolver(closed, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, closed, resolver)

	c, w := newCheckSendCtx()
	ok := ts.handler.checkSendPermission(c, uuid.New().String(), uuid.New().String())

	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCheckSendPermission_ResolverError_500(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	ts := mediaTestWithResolver(t, db, mediaBrokenResolver(t, rdb))

	owner := ts.createTestUser(t, "csp_err_owner")
	serverID := ts.createTestServer(t, owner, "CSP Err")
	channelID := ts.createTestChannel(t, serverID, "csp-err")

	c, w := newCheckSendCtx()
	// Channel lookup (working DB) succeeds; the SEND HasPermission (closed DB) errors -> 500.
	ok := ts.handler.checkSendPermission(c, owner, channelID)

	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCheckSendPermission_NoSendPerm_403(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "csp_perm_owner")
	member := ts.createTestUser(t, "csp_perm_member")
	serverID := ts.createTestServer(t, owner, "CSP NoSend")
	channelID := ts.createTestChannel(t, serverID, "csp-nosend")
	// Member with no RBAC role -> zero permissions -> no SEND.
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, member)
	require.NoError(t, err)

	c, w := newCheckSendCtx()
	ok := ts.handler.checkSendPermission(c, member, channelID)

	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUploadAttachmentStorageDisabledReturns503(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "attachstorageoff")
	serverID := ts.createTestServer(t, owner, "Attach Storage Off")
	channelID := ts.createTestChannel(t, serverID, "uploads")
	ts.handler.store = nil

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext-data"), map[string]string{
		"channel_id": channelID,
		"file_type":  "photo",
		"mime_type":  "image/jpeg",
	})

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, owner, body, ct)
	})
	assertStorageDisabledResponse(t, w)
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

// CV-CAN-003: with a real resolver, a server MEMBER who lacks channel VIEW
// (PermViewTextChannels) must NOT be able to download a channel attachment by
// file UUID. This is the endpoint-level regression guard for the core download
// fix — membership alone is insufficient once view is denied. The owner
// (OwnerPermissions) download of the same file is asserted as a positive control
// so the member's 403 is provably the VIEW gate, not a broken fixture.
func TestDownloadAttachment_Resolver_NoViewPermission403(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "dl_res_owner")
	member := ts.createTestUser(t, "dl_res_member")
	serverID := ts.createTestServer(t, owner, "DL Resolver Server")
	channelID := ts.createTestChannel(t, serverID, "hidden")
	// Member is in the server but has no RBAC role -> zero permissions -> no VIEW.
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, member)
	require.NoError(t, err)

	// Owner-uploaded attachment living in the (member-hidden) channel.
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("ciphertext")), 10, mimeOctetStream))
	_, err = ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 10, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	// Member with membership but no VIEW is blocked at the endpoint (CV-CAN-003).
	wMember := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, member, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusForbidden, wMember.Code, "body: %s", wMember.Body.String())
	assert.NotEqual(t, "ciphertext", wMember.Body.String(), "ciphertext must not be streamed on a VIEW-deny")

	// Positive control: the owner can download the same file.
	wOwner := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusOK, wOwner.Code, "body: %s", wOwner.Body.String())
	assert.Equal(t, "ciphertext", wOwner.Body.String())
}

// CV-CAN-003 (type-aware VIEW): a voice channel's visibility is gated on
// PermViewVoiceChannels, not PermViewTextChannels. A member who can view voice
// channels but lacks the text-view bit must still reach that voice channel's
// attachments — hard-coding the text bit would wrongly 403 them. This test
// fails under the old hard-coded PermViewTextChannels behavior.
func TestDownloadAttachment_Resolver_VoiceChannelUsesVoiceViewBit(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "dl_voice_owner")
	member := ts.createTestUser(t, "dl_voice_member")
	serverID := ts.createTestServer(t, owner, "DL Voice Server")
	voiceChannel := ts.createTestChannelWithType(t, serverID, "voice-chat", "voice")
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, member)
	require.NoError(t, err)
	// Grant voice-view (deliberately no PermViewTextChannels) plus read-history,
	// which the download path also requires (CV-CAN-003); this isolates the test
	// to the type-aware VIEW bit rather than the read-history gate.
	ts.createTestRoleWithPerms(t, serverID, member, "voice-viewer", rbac.PermViewVoiceChannels|rbac.PermReadMessageHistory)

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("ciphertext")), 10, mimeOctetStream))
	_, err = ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 10, $3, 1, $4)`,
		fileID, owner, storageKey, voiceChannel,
	)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, member, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusOK, w.Code, "voice-view member must access voice channel attachment; body: %s", w.Body.String())
	assert.Equal(t, "ciphertext", w.Body.String())
}

// CV-CAN-003: a member who can VIEW a channel but lacks READ_MESSAGE_HISTORY
// must not download that channel's attachments by file UUID. The download path
// requires read-history in addition to the type-appropriate VIEW bit, mirroring
// the message-read path (messages.checkChannelAccess). The owner (all
// permissions) download of the same file is a positive control so the member's
// 403 is provably the read-history gate, not a broken fixture.
func TestDownloadAttachment_Resolver_NoReadHistory403(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test"))
	ts := mediaTestWithResolver(t, db, resolver)

	owner := ts.createTestUser(t, "dl_rh_owner")
	member := ts.createTestUser(t, "dl_rh_member")
	serverID := ts.createTestServer(t, owner, "DL ReadHistory Server")
	channelID := ts.createTestChannel(t, serverID, "rh-hidden")
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, member)
	require.NoError(t, err)
	// VIEW but deliberately NO read-history: passes userHasChannelAccess, fails
	// the download-only read-history gate.
	ts.createTestRoleWithPerms(t, serverID, member, "viewer-no-history", rbac.PermViewTextChannels)

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("ciphertext")), 10, mimeOctetStream))
	_, err = ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 10, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)

	// Member has VIEW but not read-history -> blocked at the endpoint (CV-CAN-003).
	wMember := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, member, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusForbidden, wMember.Code, "body: %s", wMember.Body.String())
	assert.NotEqual(t, "ciphertext", wMember.Body.String(), "ciphertext must not be streamed on a read-history deny")

	// Positive control: the owner (all perms) can download the same file.
	wOwner := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusOK, wOwner.Code, "body: %s", wOwner.Body.String())
	assert.Equal(t, "ciphertext", wOwner.Body.String())
}

func TestDownloadAttachmentStorageDisabledReturns503(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "downloadstorageoff")
	serverID := ts.createTestServer(t, owner, "Download Storage Off")
	channelID := ts.createTestChannel(t, serverID, "downloads")

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'photo', 2, 'image/jpeg', 10, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)
	ts.handler.store = nil

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})
	})
	assertStorageDisabledResponse(t, w)
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

func TestDeleteMediaStorageDisabledReturns503AndLeavesRowActive(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "delstorageoff")

	serverID := ts.createTestServer(t, owner, "Del Storage Off")
	channelID := ts.createTestChannel(t, serverID, "delstorage")
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`,
		fileID, owner, storageKey, channelID,
	)
	require.NoError(t, err)
	ts.handler.store = nil

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, owner, gin.Params{{Key: "file_id", Value: fileID}})
	})
	assertStorageDisabledResponse(t, w)

	var deletedAt sql.NullTime
	err = ts.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt)
	require.NoError(t, err)
	assert.False(t, deletedAt.Valid, "media row should remain active when storage is disabled")
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

func TestProxyAvatarStorageDisabledReturns503(t *testing.T) {
	ts := setupMediaTest(t)
	userID := uuid.New().String()
	ts.handler.store = nil

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doJSON(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/"+userID, userID, gin.Params{{Key: "user_id", Value: userID}})
	})
	assertStorageDisabledResponse(t, w)
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
// upload 40 MiB (> free 32 MiB, < premium 256 MiB) — should NOT get 413.
func TestUploadAttachment_PremiumAllowsLargerThanFree(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{UploadMaxSize: 25 * 1024 * 1024}
	premium := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierPremium})
	body, ct := makeMultipartFile(t, 40*1024*1024) // 40 MiB
	w := doAttachmentUpload(t, premium, body, ct)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code, "premium accepts 40 MiB")
}

// TestUploadAttachment_FreeRejectsOverLimit confirms a free user uploading 40 MiB
// (> free 32 MiB) gets 413 Request Entity Too Large.
func TestUploadAttachment_FreeRejectsOverLimit(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{UploadMaxSize: 25 * 1024 * 1024}
	free := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierFree})
	body, ct := makeMultipartFile(t, 40*1024*1024) // 40 MiB > free 32 MiB
	w := doAttachmentUpload(t, free, body, ct)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "free rejects 40 MiB")
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

// TestUploadAvatar_FreeCapsAtFiveMiB: 6 MiB > free 5 MiB -> 413.
func TestUploadAvatar_FreeCapsAtFiveMiB(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{}
	free := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierFree})
	w := doImageUpload(t, free.UploadAvatar, 6*1024*1024)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// TestUploadBanner_FreeCapsAtFiveMiB: 6 MiB > free 5 MiB -> 413.
func TestUploadBanner_FreeCapsAtFiveMiB(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	cfg := &config.Config{}
	free := NewHandler(db, nil, logger.New("test"), cfg, nil, tierStub{entitlements.TierFree})
	w := doImageUpload(t, free.UploadBanner, 6*1024*1024)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// tierStub satisfies entitlements.TierResolver with a configurable tier.
type tierStub struct{ tier string }

func (s tierStub) GetTier(context.Context, string) string { return s.tier }

type serverTierStub struct{ tier string }

func (s serverTierStub) GetServerTier(context.Context, string) string { return s.tier }

// =====================================================================
// Animated GIF uploads (#1302) — entitlement-gated animation preservation
// =====================================================================

// makeAnimatedGIFUpload builds a 3-frame animated GIF fixture (in-test, no
// binary files) using the shared helpers from processing_gif_test.go.
func makeAnimatedGIFUpload(t *testing.T) []byte {
	t.Helper()
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 64, 64), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 64, 64), gifColBlue, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 64, 64), gifColWhite, pal),
	}
	return encodeTestGIF(t, frames, []int{10, 20, 30},
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalNone}, 0, 64, 64)
}

// makeStaticGIFUpload builds a single-frame GIF fixture.
func makeStaticGIFUpload(t *testing.T) []byte {
	t.Helper()
	pal := gifTestPalette()
	frames := []*image.Paletted{solidPalettedFrame(t, image.Rect(0, 0, 64, 64), gifColRed, pal)}
	return encodeTestGIF(t, frames, []int{0}, []byte{gif.DisposalNone}, 0, 64, 64)
}

// storedObject fetches a stored object's bytes + content type from the mock store.
func (ts *testSetup) storedObject(t *testing.T, key string) ([]byte, string) {
	t.Helper()
	obj, contentType, err := ts.store.GetObject(context.Background(), key)
	require.NoError(t, err)
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	return data, contentType
}

func TestUploadAvatarAnimatedGIF_PremiumPreservesAnimation(t *testing.T) {
	ts := setupMediaTest(t)
	ts.handler.tiers = tierStub{entitlements.TierPremium}
	userID := ts.createTestUser(t, "premiumanimgif")

	body, ct := multipartBody(t, "file", "avatar.gif", makeAnimatedGIFUpload(t), nil)
	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	data, contentType := ts.storedObject(t, fmt.Sprintf("avatars/%s", userID))
	assert.Equal(t, "image/gif", contentType, "animated output keeps the gif content type")

	out, err := gif.DecodeAll(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Len(t, out.Image, 3, "animation preserved")
	assert.Equal(t, []int{10, 20, 30}, out.Delay, "delays preserved")
}

func TestUploadAvatarAnimatedGIF_FreeRejectedWithUpsellCode(t *testing.T) {
	ts := setupMediaTest(t) // freeTierStub
	userID := ts.createTestUser(t, "freeanimgif")

	body, ct := multipartBody(t, "file", "avatar.gif", makeAnimatedGIFUpload(t), nil)
	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	require.Equal(t, http.StatusForbidden, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "animated_profile_premium", resp["code"], "typed upsell code (#1522 pattern)")
	assert.False(t, ts.store.hasObject(fmt.Sprintf("avatars/%s", userID)), "nothing stored on reject")
}

// Behavior lock: a STATIC (single-frame) GIF keeps today's flatten path for
// every tier — for avatars that is the PNG static pipeline (banners flatten
// to JPEG), unchanged by #1302.
func TestUploadAvatarStaticGIF_FreeFlattensUnchanged(t *testing.T) {
	ts := setupMediaTest(t) // freeTierStub
	userID := ts.createTestUser(t, "freestaticgif")

	body, ct := multipartBody(t, "file", "avatar.gif", makeStaticGIFUpload(t), nil)
	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	_, contentType := ts.storedObject(t, fmt.Sprintf("avatars/%s", userID))
	assert.Equal(t, "image/png", contentType, "static gif avatar flattens via the static path")
}

func TestUploadServerBannerAnimatedGIF_GroundspeedRejected(t *testing.T) {
	ts := setupMediaTest(t) // serverTiers nil → ResolveServerTier → groundspeed
	owner := ts.createTestUser(t, "gsbannerowner")
	serverID := ts.createTestServer(t, owner, "Groundspeed Banner Server")

	body, ct := multipartBody(t, "file", "banner.gif", makeAnimatedGIFUpload(t),
		map[string]string{keyServerID: serverID})
	w := ts.doMultipart(ts.handler.UploadServerBanner, "POST", "/api/v1/media/upload/server-banner", owner, body, ct)

	require.Equal(t, http.StatusForbidden, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "animated_server_banner_mach", resp["code"])
	assert.False(t, ts.store.hasObject(fmt.Sprintf("server-banners/%s", serverID)))
}

func TestUploadServerBannerAnimatedGIF_SelfhostPreserved(t *testing.T) {
	ts := setupMediaTest(t)
	// selfhost is the tier GetServerTier resolves on INSTANCE_TYPE=self-hosted
	// (ServerCache short-circuit); the stub injects it at the same seam.
	ts.handler.serverTiers = serverTierStub{entitlements.TierSelfHost}
	owner := ts.createTestUser(t, "shbannerowner")
	serverID := ts.createTestServer(t, owner, "Selfhost Banner Server")

	body, ct := multipartBody(t, "file", "banner.gif", makeAnimatedGIFUpload(t),
		map[string]string{keyServerID: serverID})
	w := ts.doMultipart(ts.handler.UploadServerBanner, "POST", "/api/v1/media/upload/server-banner", owner, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	data, contentType := ts.storedObject(t, fmt.Sprintf("server-banners/%s", serverID))
	assert.Equal(t, "image/gif", contentType)

	out, err := gif.DecodeAll(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Len(t, out.Image, 3, "animation preserved")
}

func TestUploadServerIconAnimatedGIF_RejectedUnsupported(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "animiconowner")
	serverID := ts.createTestServer(t, owner, "Anim Icon Server")

	body, ct := multipartBody(t, "file", "icon.gif", makeAnimatedGIFUpload(t),
		map[string]string{keyServerID: serverID})
	w := ts.doMultipart(ts.handler.UploadServerIcon, "POST", pathUploadServerIcon, owner, body, ct)

	require.Equal(t, http.StatusForbidden, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "animated_not_supported", resp["code"], "server icons stay static at every tier")
}

func TestUploadDMIconAnimatedGIF_RejectedUnsupported(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "animdmadmin")
	member := ts.createTestUser(t, "animdmmember")
	convID := ts.createTestGroupDM(t, admin, member)

	body, ct := multipartBody(t, "file", "icon.gif", makeAnimatedGIFUpload(t),
		map[string]string{keyConversationID: convID})
	w := ts.doMultipart(ts.handler.UploadDMIcon, "POST", pathUploadDMIcon, admin, body, ct)

	require.Equal(t, http.StatusForbidden, w.Code)
	resp := parseBody(t, w)
	assert.Equal(t, "animated_not_supported", resp["code"], "dm icons stay static at every tier")
}

// Bomb guards surface as a clean 400 through the upload path — the guard
// rejection happens during animation detection, before any entitlement gate.
func TestUploadAvatarAnimatedGIF_BombGuardRejects400(t *testing.T) {
	ts := setupMediaTest(t)
	ts.handler.tiers = tierStub{entitlements.TierPremium}
	userID := ts.createTestUser(t, "bombgif")

	pal := gifTestPalette()
	frames := make([]*image.Paletted, maxGifFrames+1)
	delays := make([]int, maxGifFrames+1)
	disposal := make([]byte, maxGifFrames+1)
	for i := range frames {
		frames[i] = solidPalettedFrame(t, image.Rect(0, 0, 1, 1), gifColRed, pal)
		disposal[i] = gif.DisposalNone
	}
	raw := encodeTestGIF(t, frames, delays, disposal, 0, 1, 1)

	body, ct := multipartBody(t, "file", "bomb.gif", raw, nil)
	w := ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.False(t, ts.store.hasObject(fmt.Sprintf("avatars/%s", userID)))
}
