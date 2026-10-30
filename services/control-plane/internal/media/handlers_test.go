package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/feedbackscreenshots"
	invitecodes "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/lib/pq/pqerror"
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

// mediaTestRedis connects to this process's own allocated Redis logical database
// (#2680). Used only to back the RBAC permission cache in resolver tests. The
// hand-rolled DB-1 URL it replaced was shared by every other package binary in a
// run, so a sibling's flush could empty this cache mid-test. redistest is a LEAF
// package, so importing it does not reintroduce the cycle described above.
func mediaTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	return redistest.Client(t)
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
// Used by tests that do not need tier-dependent enforcement.
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
	testdb.RememberActor(t, ts.db, userID)
	return userID
}

func (ts *testSetup) createTestTier1Media(t *testing.T, userID, storageKey string) {
	t.Helper()
	var profileSlot *string
	if storageKey == fmt.Sprintf("avatars/%s", userID) {
		slot := "avatar"
		profileSlot = &slot
	} else if storageKey == fmt.Sprintf("banners/%s", userID) {
		slot := "banner"
		profileSlot = &slot
	}
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
		 VALUES ($1, $2, 'photo', 1, $3, $4, $5, $6)`,
		uuid.New().String(), userID, mimeImagePNG, 100, storageKey, profileSlot,
	)
	require.NoError(t, err)
}

func (ts *testSetup) liveProfileStorageKey(t *testing.T, userID, slot string) string {
	t.Helper()
	var key string
	require.NoError(t, ts.db.QueryRow(`SELECT storage_key FROM media_files WHERE uploader_id = $1 AND profile_slot = $2 AND deleted_at IS NULL`, userID, slot).Scan(&key))
	return key
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
	testdb.AttachRememberedActor(c, userID)
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
	testdb.AttachRememberedActor(c, userID)
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
	key := ts.liveProfileStorageKey(t, userID, "avatar")
	assert.True(t, ts.store.hasObject(key))
	assert.Equal(t, 1, counters.uploads)
}

func TestUploadAvatarRejectsUnresolvedErasureEvidenceCapBeforePut(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "avatar-evidence-cap")

	for i := 0; i < maxUnresolvedProfileUploadEvidence; i++ {
		key := fmt.Sprintf("avatars/%s/%s", userID, uuid.NewString())
		_, err := ts.db.Exec(`
			INSERT INTO media_files (
				id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot, deleted_at
			) VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/png', 4, $2, 'avatar', NOW())`, userID, key)
		require.NoError(t, err)
		_, err = ts.db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
		require.NoError(t, err)
	}

	body, contentType := multipartBody(t, "file", "avatar.png", makePNG(t, 200, 200), nil)
	w := ts.doMultipart(ts.handler.UploadAvatar, http.MethodPost, pathUploadAvatar, userID, body, contentType)

	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Zero(t, ts.store.objectCount(), "the cap must reject before PutObject")
	var intentCount int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM tier1_profile_upload_intents WHERE user_id = $1`, userID).Scan(&intentCount))
	assert.Zero(t, intentCount, "the cap must reject before creating an upload intent")
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
	testdb.AttachRememberedActor(c, userID)
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

// tier1RaceStore exposes only the ordering needed by the profile-upload race
// tests. It deliberately embeds the normal fake so unrelated ObjectStore
// methods retain their existing behavior.
type tier1RaceStore struct {
	*mockStore
	putStarted     chan struct{}
	putRelease     chan struct{}
	putErr         error
	waitForCancel  bool
	writeThenErr   error
	putDeadline    time.Time
	putStartedAt   time.Time
	putHasDeadline bool
	mu             sync.Mutex
	puts           int
	gets           int
	deletes        int
}

type lateWriteRaceStore struct {
	*mockStore
	putStarted chan struct{}
	lateWrite  chan struct{}
	lateResult chan error
	key        string
}

type blockingGetStore struct {
	*mockStore
	started     chan struct{}
	release     chan struct{}
	mu          sync.Mutex
	gets        int
	releaseOnce sync.Once
}

func (s *blockingGetStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return s.mockStore.GetObject(ctx, key)
}

func (s *blockingGetStore) releaseGet() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func (s *blockingGetStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func (s *lateWriteRaceStore) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	s.key = key
	select {
	case s.putStarted <- struct{}{}:
	default:
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	<-ctx.Done()
	lateCtx := context.WithoutCancel(ctx)
	go func() {
		<-s.lateWrite
		s.lateResult <- s.mockStore.PutObject(lateCtx, key, bytes.NewReader(data), size, contentType)
	}()
	return ctx.Err()
}

func (s *lateWriteRaceStore) DeleteObject(ctx context.Context, key string) error {
	return s.mockStore.DeleteObject(ctx, key)
}

func (s *tier1RaceStore) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	s.mu.Lock()
	s.puts++
	s.putStartedAt = time.Now()
	s.putDeadline, s.putHasDeadline = ctx.Deadline()
	s.mu.Unlock()
	if s.putStarted != nil {
		select {
		case s.putStarted <- struct{}{}:
		default:
		}
	}
	if s.putRelease != nil {
		select {
		case <-s.putRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.waitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.putErr != nil {
		return s.putErr
	}
	if err := s.mockStore.PutObject(ctx, key, reader, size, contentType); err != nil {
		return err
	}
	return s.writeThenErr
}

func (s *tier1RaceStore) DeleteObject(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deletes++
	s.mu.Unlock()
	return s.mockStore.DeleteObject(ctx, key)
}

func (s *tier1RaceStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.mockStore.GetObject(ctx, key)
}

func (s *tier1RaceStore) counts() (puts, deletes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.deletes
}

func (s *tier1RaceStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func (s *tier1RaceStore) putDeadlineInfo() (deadline, startedAt time.Time, hasDeadline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putDeadline, s.putStartedAt, s.putHasDeadline
}

// eraseUserForTier1Race mirrors the account-erasure lock/obligation ordering
// without importing internal/users (which would create a package cycle).
func eraseUserForTier1Race(db *sql.DB, userID string, locked chan<- int64, release <-chan struct{}) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&id); err != nil {
		return err
	}
	if locked != nil {
		var txID int64
		if err := tx.QueryRow(`SELECT txid_current()`).Scan(&txID); err != nil {
			return err
		}
		locked <- txID
	}
	if release != nil {
		<-release
	}
	_, err = tx.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key)
		SELECT storage_key FROM media_files WHERE uploader_id = $1 AND media_tier = 1
		UNION
		SELECT storage_key FROM tier1_profile_upload_intents WHERE user_id = $1
		UNION
		SELECT 'avatars/' || $1
		UNION
		SELECT 'banners/' || $1
		ON CONFLICT (storage_key) DO NOTHING`, userID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM tier1_profile_upload_intents WHERE user_id = $1`, userID); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func rowLockWaiterExists(t *testing.T, probe *sql.DB, txID int64) bool {
	t.Helper()
	var waiting bool
	err := probe.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text::bigint = $1)`, testdb.TransactionIDForLockProbe(txID)).Scan(&waiting)
	require.NoError(t, err)
	return waiting
}

func TestUploadFirstErasureWaitsForProfileMetadataCommit(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "uploadfirst")
	store := &tier1RaceStore{mockStore: newMockStore(), putStarted: make(chan struct{}, 1), putRelease: make(chan struct{})}
	ts.handler.store = store
	body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	uploadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		uploadDone <- ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("profile upload did not reach object storage")
	}
	erasureDone := make(chan error, 1)
	go func() { erasureDone <- eraseUserForTier1Race(ts.db, userID, nil, nil) }()
	select {
	case err := <-erasureDone:
		close(store.putRelease)
		<-uploadDone
		assert.NoError(t, err)
		t.Fatal("account erasure committed before the profile upload metadata transaction")
	default:
	}
	probeTx, err := ts.db.Begin()
	require.NoError(t, err)
	var lockedID string
	lockErr := probeTx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR UPDATE NOWAIT`, userID).Scan(&lockedID)
	assert.Error(t, lockErr, "upload-first ordering must hold the conflicting users-row lock")
	if pgErr, ok := lockErr.(*pq.Error); ok {
		assert.Equal(t, pqerror.Code("55P03"), pgErr.Code, "lock probe must fail because the row is busy")
	}
	_ = probeTx.Rollback()
	close(store.putRelease)
	assert.Equal(t, http.StatusCreated, (<-uploadDone).Code)
	physicalKey := ts.liveProfileStorageKey(t, userID, "avatar")
	assert.NoError(t, <-erasureDone)
	worker := NewTier1ErasureReclaimer(ts.db, store, logger.New("test"))
	_, err = worker.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.False(t, store.hasObject(physicalKey), "reclaimer must delete the upload-first avatar object")
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key IN ($1, $2, $3)`, physicalKey, tier1StorageKey(purposeAvatar, userID, "", ""), tier1StorageKey(purposeBanner, userID, "", "")).Scan(&count))
	assert.Equal(t, 3, count, "successful reclaimer pass must retain all permanent tombstones")
}

func TestErasureFirstProfileUploadPerformsNoPut(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "erasurefirst")
	store := &tier1RaceStore{mockStore: newMockStore(), putStarted: make(chan struct{}, 1)}
	ts.handler.store = store
	locked, release := make(chan int64, 1), make(chan struct{})
	erasureDone := make(chan error, 1)
	go func() { erasureDone <- eraseUserForTier1Race(ts.db, userID, locked, release) }()
	txID := <-locked
	body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	uploadDone := make(chan *httptest.ResponseRecorder, 1)
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })
	go func() {
		uploadDone <- ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct)
	}()
	waiting := false
	for i := 0; i < 1000 && !waiting; i++ {
		waiting = rowLockWaiterExists(t, probe, txID)
	}
	if !waiting {
		close(release)
		assert.NoError(t, <-erasureDone)
		<-uploadDone
		t.Fatal("profile upload did not wait on the erasure transaction's users-row lock")
	}
	testdb.WaitForRowLockWaiter(t, probe, txID)
	close(release)
	assert.NoError(t, <-erasureDone)
	assert.NotEqual(t, http.StatusCreated, (<-uploadDone).Code)
	puts, _ := store.counts()
	assert.Zero(t, puts, "an upload after erasure must not mutate object storage")
}

func TestTier1TombstoneSurvivesLateObjectAndReclaimsItAgain(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "lateobject")
	store := &lateWriteRaceStore{mockStore: newMockStore(), putStarted: make(chan struct{}, 1), lateWrite: make(chan struct{}), lateResult: make(chan error, 1)}
	ts.handler.store = store
	body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	req := httptest.NewRequest(http.MethodPost, pathUploadAvatar, body)
	req.Header.Set("Content-Type", ct)
	putDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req
		c.Set("user_id", userID)
		testdb.AttachRememberedActor(c, userID)
		ts.handler.UploadAvatar(c)
		putDone <- w
	}()
	select {
	case <-store.putStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("late-write upload did not reach object storage")
	}
	var w *httptest.ResponseRecorder
	select {
	case w = <-putDone:
	case <-time.After(12 * time.Second):
		t.Fatal("profile upload did not honor its object-store deadline")
	}
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	require.NoError(t, eraseUserForTier1Race(ts.db, userID, nil, nil))
	key := store.key
	assert.NotEqual(t, tier1StorageKey(purposeAvatar, userID, "", ""), key)

	worker := NewTier1ErasureReclaimer(ts.db, store, logger.New("test"))
	_, err := worker.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	close(store.lateWrite)
	require.NoError(t, <-store.lateResult)
	assert.True(t, store.hasObject(key), "the delayed backend write must recreate the physical object")
	w = ts.doJSON(ts.handler.ProxyAvatar, http.MethodGet, "/api/v1/media/avatars/"+userID, userID,
		gin.Params{{Key: "user_id", Value: userID}})
	assert.Equal(t, http.StatusNotFound, w.Code, "a tombstoned object must never be served")

	_, err = ts.db.Exec(`UPDATE tier1_erasure_delete_obligations SET reconcile_after = now() - interval '1 minute' WHERE storage_key = $1`, key)
	require.NoError(t, err)
	_, err = worker.reclaimDue(context.Background(), 10)
	require.NoError(t, err)
	assert.False(t, store.hasObject(key), "a later reclaim must delete a late object")
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&count))
	assert.Equal(t, 1, count, "the permanent tombstone must survive successful deletion")
}

func TestProfileUploadWithLiveTombstonePerformsNoPut(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "livetombstone")
	legacyKey := tier1StorageKey(purposeAvatar, userID, "", "")
	_, err := ts.db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, legacyKey)
	require.NoError(t, err)
	store := &tier1RaceStore{mockStore: newMockStore()}
	ts.handler.store = store
	body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	w := ts.doMultipart(ts.handler.UploadAvatar, http.MethodPost, pathUploadAvatar, userID, body, ct)
	assert.Equal(t, http.StatusCreated, w.Code)
	puts, _ := store.counts()
	assert.Equal(t, 1, puts, "a legacy canonical tombstone must not block a fresh immutable key")
	physicalKey := ts.liveProfileStorageKey(t, userID, "avatar")
	assert.NotEqual(t, legacyKey, physicalKey)
	assert.True(t, store.hasObject(physicalKey))
	assert.Equal(t, 1, obligationCountForKey(t, ts.db, legacyKey))
}

func installTier1MetadataFailure(t *testing.T, db *sql.DB) func() {
	t.Helper()
	_, err := db.Exec(`CREATE OR REPLACE FUNCTION test_tier1_metadata_failure() RETURNS trigger AS $$
	BEGIN RAISE EXCEPTION 'forced metadata failure'; END; $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER test_tier1_metadata_failure_trigger BEFORE INSERT OR UPDATE ON media_files FOR EACH ROW EXECUTE FUNCTION test_tier1_metadata_failure()`)
	require.NoError(t, err)
	return func() {
		if _, dropErr := db.Exec(`DROP TRIGGER IF EXISTS test_tier1_metadata_failure_trigger ON media_files`); dropErr != nil {
			t.Errorf("drop test trigger: %v", dropErr)
		}
		if _, dropErr := db.Exec(`DROP FUNCTION IF EXISTS test_tier1_metadata_failure()`); dropErr != nil {
			t.Errorf("drop test function: %v", dropErr)
		}
	}
}

func TestDefiniteMetadataFailureDoesNotPutOrAlterPriorProfileObject(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "metadatafailure")
	store := &tier1RaceStore{mockStore: newMockStore()}
	ts.handler.store = store
	firstBody, firstCT := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	assert.Equal(t, http.StatusCreated, ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, firstBody, firstCT).Code)
	key := ts.liveProfileStorageKey(t, userID, "avatar")
	first, _, err := store.GetObject(context.Background(), key)
	require.NoError(t, err)
	firstBytes, err := io.ReadAll(first)
	require.NoError(t, err)
	var beforeSize int
	var beforeMIME string
	require.NoError(t, ts.db.QueryRow(`SELECT file_size, mime_type FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL`, key).Scan(&beforeSize, &beforeMIME))
	cleanup := installTier1MetadataFailure(t, ts.db)
	defer cleanup()
	secondBody, secondCT := multipartBody(t, "file", "avatar.png", makePNG(t, 128, 128), nil)
	assert.Equal(t, http.StatusInternalServerError, ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, secondBody, secondCT).Code)
	puts, deletes := store.counts()
	assert.Equal(t, 1, puts, "definite metadata failure must occur before the second writer's PUT")
	assert.Zero(t, deletes, "metadata failure must not destructively compensate the deterministic key")
	var afterSize int
	var afterMIME string
	require.NoError(t, ts.db.QueryRow(`SELECT file_size, mime_type FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL`, key).Scan(&afterSize, &afterMIME))
	assert.Equal(t, beforeSize, afterSize, "failed writer must not alter prior metadata size")
	assert.Equal(t, beforeMIME, afterMIME, "failed writer must not alter prior metadata content type")
	obj, _, err := store.GetObject(context.Background(), key)
	require.NoError(t, err)
	after, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, firstBytes, after, "failed writer must not alter the successful writer's object")
}

func TestDefiniteMetadataFailureBeforeSuccessCannotPoisonLaterWriter(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "failurefirst")
	store := &tier1RaceStore{mockStore: newMockStore()}
	ts.handler.store = store
	cleanup := installTier1MetadataFailure(t, ts.db)
	t.Cleanup(cleanup)
	body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	assert.Equal(t, http.StatusInternalServerError, ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct).Code)
	var metadataCount int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1 AND profile_slot = 'avatar' AND deleted_at IS NULL`, userID).Scan(&metadataCount))
	assert.Zero(t, metadataCount, "definite metadata failure must leave no live metadata")
	puts, deletes := store.counts()
	assert.Zero(t, puts, "definite metadata failure must occur before any PUT")
	assert.Zero(t, deletes, "definite metadata failure must not issue destructive compensation")
	cleanup()
	secondBody, secondCT := multipartBody(t, "file", "avatar.png", makePNG(t, 128, 128), nil)
	assert.Equal(t, http.StatusCreated, ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, secondBody, secondCT).Code)
	key := ts.liveProfileStorageKey(t, userID, "avatar")
	obj, _, err := store.GetObject(context.Background(), key)
	require.NoError(t, err)
	secondBytes, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.NotEmpty(t, secondBytes, "a later successful writer's immutable object must remain intact")
}

func TestProfilePutErrorsNeverDeleteDeterministicObject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeThen bool
	}{
		{name: "honest put error"},
		{name: "write then error", writeThen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			userID := ts.createTestUser(t, "puterror"+strings.ReplaceAll(tc.name, " ", ""))
			store := &tier1RaceStore{mockStore: newMockStore(), putErr: errors.New("put failed")}
			if tc.writeThen {
				store.putErr = nil
				store.writeThenErr = errors.New("ambiguous put failure")
			}
			ts.handler.store = store
			body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
			assert.Equal(t, http.StatusInternalServerError, ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct).Code)
			_, deletes := store.counts()
			assert.Zero(t, deletes, "PUT failure must not issue destructive compensation")
			var metadataCount int
			require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1 AND profile_slot = 'avatar' AND deleted_at IS NULL`, userID).Scan(&metadataCount))
			assert.Zero(t, metadataCount, "PUT failure must leave transactional metadata absent")
		})
	}
}

func TestProfilePutTimeoutBoundsObjectStoreContextAndReleasesTransaction(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "puttimeout")
	store := &tier1RaceStore{mockStore: newMockStore(), putStarted: make(chan struct{}, 1), waitForCancel: true}
	ts.handler.store = store
	probeDB, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	probeDB.SetMaxOpenConns(1)
	probeDB.SetMaxIdleConns(1)
	probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, probeDB.PingContext(probeCtx))
	cancel()
	t.Cleanup(func() {
		if err := probeDB.Close(); err != nil {
			t.Errorf("close profile upload probe database: %v", err)
		}
	})
	ts.handler.db = probeDB

	body, ct := multipartBody(t, "file", "avatar.png", makePNG(t, 64, 64), nil)
	req := httptest.NewRequest(http.MethodPost, pathUploadAvatar, body)
	req.Header.Set("Content-Type", ct)
	putDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req
		c.Set("user_id", userID)
		testdb.AttachRememberedActor(c, userID)
		ts.handler.UploadAvatar(c)
		putDone <- w
	}()
	select {
	case <-store.putStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("profile upload did not reach object storage")
	}
	var w *httptest.ResponseRecorder
	select {
	case w = <-putDone:
	case <-time.After(12 * time.Second):
		t.Fatal("profile upload did not stop at its ten-second transaction deadline")
	}

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	deadline, putStartedAt, hasDeadline := store.putDeadlineInfo()
	require.True(t, hasDeadline, "profile object-store PUT must have a deadline")
	assert.False(t, deadline.After(putStartedAt.Add(10*time.Second+100*time.Millisecond)),
		"PUT deadline must be bounded to ten seconds from the PUT")

	var metadataCount int
	probeCtx, cancel = context.WithTimeout(context.Background(), time.Second)
	queryErr := probeDB.QueryRowContext(probeCtx,
		`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1 AND profile_slot = 'avatar' AND deleted_at IS NULL`, userID,
	).Scan(&metadataCount)
	cancel()
	require.NoError(t, queryErr, "failed profile upload must release its transaction connection")
	assert.Zero(t, metadataCount, "failed profile upload must leave no live metadata")
}

func TestAmbiguousTier1MetadataCommitNeverDeletesObject(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "ambiguouscommit")
	store := &tier1RaceStore{mockStore: newMockStore()}
	ts.handler.store = store
	ts.handler.tier1UploadCommit = func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("commit outcome unknown")
	}
	expected := makePNG(t, 64, 64)
	body, ct := multipartBody(t, "file", "avatar.png", expected, nil)
	assert.Equal(t, http.StatusInternalServerError, ts.doMultipart(ts.handler.UploadAvatar, "POST", pathUploadAvatar, userID, body, ct).Code)
	key := ts.liveProfileStorageKey(t, userID, "avatar")
	var metadataCount int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL`, key).Scan(&metadataCount))
	assert.Equal(t, 1, metadataCount, "ambiguous commit must retain committed metadata")
	obj, _, err := store.GetObject(context.Background(), key)
	require.NoError(t, err)
	actual, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, expected, actual, "ambiguous commit must retain the committed immutable object")
	puts, deletes := store.counts()
	assert.Equal(t, 1, puts, "ambiguous commit must perform exactly one PUT")
	assert.Zero(t, deletes, "ambiguous commit must not destructively compensate the immutable key")
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

func TestUploadServerIconStoreFailureReturns500WithoutObject(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "iconstorefailure")
	serverID := ts.createTestServer(t, owner, "Icon Store Failure")
	store := &tier1RaceStore{mockStore: ts.store, putErr: errors.New("put failed")}
	ts.handler.store = store
	counters := &mediaOpsCounterSpy{}
	ts.handler.SetOpsCounter(counters)

	imgData := makePNG(t, 100, 100)
	body, ct := multipartBody(t, "file", fileIconPng, imgData, map[string]string{"server_id": serverID})
	w := ts.doMultipart(ts.handler.UploadServerIcon, "POST", pathUploadServerIcon, owner, body, ct)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, errMsgFailedStoreImage, parseBody(t, w)["error"])
	assert.False(t, ts.store.hasObject(fmt.Sprintf("server-icons/%s", serverID)))
	assert.Zero(t, counters.uploads)
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
		"channel_id":  channelID,
		"file_type":   "file",
		"mime_type":   mimeOctetStream,
		"key_version": "1",
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
		"channel_id":  channelID,
		"file_type":   "photo",
		"mime_type":   "image/jpeg",
		"key_version": "1",
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
		"channel_id":  channelID,
		"file_type":   "photo",
		"mime_type":   "image/jpeg",
		"key_version": "1",
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
		"channel_id":  channelID,
		"file_type":   "file",
		"mime_type":   mimeOctetStream,
		"key_version": "1",
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
		"channel_id":  channelID,
		"file_type":   "file",
		"mime_type":   mimeOctetStream,
		"key_version": "1",
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
		"channel_id":  channelID,
		"file_type":   "file",
		"mime_type":   mimeOctetStream,
		"key_version": "1",
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
		"channel_id":  channelID,
		"file_type":   "photo",
		"mime_type":   "image/jpeg",
		"key_version": "1",
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
		"key_version":     "1",
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
		"file_type":   "file",
		"key_version": "1",
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
		"channel_id":  valueNotUUID,
		"file_type":   "file",
		"key_version": "1",
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
		"key_version":     "1",
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
		"key_version":     "1",
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

func insertDMDownloadFixture(t *testing.T, ts *testSetup, uploader, conversationID string, payload []byte) (fileID, messageID string) {
	t.Helper()
	fileID = uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader(payload), int64(len(payload)), mimeOctetStream))
	_, err := ts.db.Exec(`
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', $3, $4, 1, $5)`,
		fileID, uploader, len(payload), storageKey, conversationID)
	require.NoError(t, err)
	messageID = uuid.New().String()
	_, err = ts.db.Exec(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, type, key_version)
		VALUES ($1, $2, $3, 'encrypted attachment', 'text', 1)`, messageID, conversationID, uploader)
	require.NoError(t, err)
	_, err = ts.db.Exec(`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, messageID, fileID)
	require.NoError(t, err)
	return fileID, messageID
}

func hideDMMessageForDownload(t *testing.T, ts *testSetup, viewer, conversationID string, from, to time.Time, includesOwn bool) {
	t.Helper()
	_, err := ts.db.Exec(`
		INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, $3, $4, $5)`, viewer, conversationID, from, to, includesOwn)
	require.NoError(t, err)
}

// A pinned message is never hidden (#3458 §18.1), so its attachment is served
// to a participant whose Clear range covers it, and refused once it is
// unpinned. The peer uploads, so the uploader-preview fallback cannot serve it.
func TestDownloadAttachmentDMVisibility_PinnedMessageUnderClearRange(t *testing.T) {
	ts := setupMediaTest(t)
	actor := ts.createTestUser(t, "mediapinnedactor")
	peer := ts.createTestUser(t, "mediapinnedpeer")
	convID := ts.createTestDMConversation(t, actor, peer)
	fileID, messageID := insertDMDownloadFixture(t, ts, peer, convID, []byte("pinned-bytes"))
	_, err := ts.db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = $1`, messageID, peer)
	require.NoError(t, err)
	hideDMMessageForDownload(t, ts, actor, convID, time.Unix(0, 0), time.Now().Add(time.Minute), true)
	download := func() *httptest.ResponseRecorder {
		return ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, actor, gin.Params{{Key: "file_id", Value: fileID}})
	}

	pinned := download()
	assert.Equal(t, http.StatusOK, pinned.Code)
	assert.Equal(t, []byte("pinned-bytes"), pinned.Body.Bytes())

	_, err = ts.db.Exec(`UPDATE dm_messages SET pinned_at = NULL, pinned_by = NULL WHERE id = $1`, messageID)
	require.NoError(t, err)
	unpinned := download()
	assert.Equal(t, http.StatusForbidden, unpinned.Code)
	assert.False(t, bytes.Contains(unpinned.Body.Bytes(), []byte("pinned-bytes")))
}

func TestDownloadAttachmentDMVisibility(t *testing.T) {
	t.Run("hidden linked attachment is denied to actor but readable by peer", func(t *testing.T) {
		ts := setupMediaTest(t)
		actor := ts.createTestUser(t, "mediahiddenactor")
		peer := ts.createTestUser(t, "mediahiddenpeer")
		convID := ts.createTestDMConversation(t, actor, peer)
		fileID, _ := insertDMDownloadFixture(t, ts, actor, convID, []byte("dm-secret"))
		hideDMMessageForDownload(t, ts, actor, convID, time.Unix(0, 0), time.Now().Add(time.Minute), true)

		actorResponse := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, actor, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusForbidden, actorResponse.Code)
		assert.False(t, bytes.Contains(actorResponse.Body.Bytes(), []byte("dm-secret")))
		peerResponse := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, peer, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusOK, peerResponse.Code)
		assert.Equal(t, []byte("dm-secret"), peerResponse.Body.Bytes())
	})

	t.Run("visible later link keeps a shared file readable", func(t *testing.T) {
		ts := setupMediaTest(t)
		actor := ts.createTestUser(t, "medialateractor")
		peer := ts.createTestUser(t, "medialaterpeer")
		convID := ts.createTestDMConversation(t, actor, peer)
		fileID, oldMessageID := insertDMDownloadFixture(t, ts, actor, convID, []byte("shared-bytes"))
		laterMessageID := uuid.New().String()
		_, err := ts.db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, type, key_version, created_at) VALUES ($1, $2, $3, 'later', 'text', 1, NOW() + INTERVAL '1 minute')`, laterMessageID, convID, peer)
		require.NoError(t, err)
		var oldCreated time.Time
		require.NoError(t, ts.db.QueryRow(`SELECT created_at FROM dm_messages WHERE id = $1`, oldMessageID).Scan(&oldCreated))
		hideDMMessageForDownload(t, ts, actor, convID, oldCreated.Add(-time.Second), time.Now().Add(30*time.Second), true)

		beforeLaterLink := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, actor, gin.Params{{Key: "file_id", Value: fileID}})
		require.Equal(t, http.StatusForbidden, beforeLaterLink.Code)
		_, err = ts.db.Exec(`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, 1)`, laterMessageID, fileID)
		require.NoError(t, err)
		response := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, actor, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, []byte("shared-bytes"), response.Body.Bytes())
	})

	t.Run("unlinked uploader preview survives clear while participant", func(t *testing.T) {
		ts := setupMediaTest(t)
		uploader := ts.createTestUser(t, "mediapreview")
		peer := ts.createTestUser(t, "mediapreviewpeer")
		convID := ts.createTestDMConversation(t, uploader, peer)
		fileID := uuid.New().String()
		storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
		require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("preview")), 7, mimeOctetStream))
		_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id) VALUES ($1, $2, 'file', 2, 'application/octet-stream', 7, $3, 1, $4)`, fileID, uploader, storageKey, convID)
		require.NoError(t, err)
		before := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		require.Equal(t, http.StatusOK, before.Code)
		_, err = ts.db.Exec(`UPDATE dm_participants SET hidden_at = NOW() WHERE conversation_id = $1 AND user_id = $2`, convID, uploader)
		require.NoError(t, err)
		_, err = ts.db.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own) VALUES ($1, $2, '-infinity', NOW(), true)`, uploader, convID)
		require.NoError(t, err)
		after := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusOK, after.Code)
		assert.Equal(t, []byte("preview"), after.Body.Bytes())
		peerResponse := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, peer, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusForbidden, peerResponse.Code)
		_, err = ts.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, uploader)
		require.NoError(t, err)
		removedUploader := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusForbidden, removedUploader.Code)
	})

	t.Run("unlinked group DM uploader preview requires current membership", func(t *testing.T) {
		ts := setupMediaTest(t)
		uploader := ts.createTestUser(t, "mediagrouppreview")
		peer := ts.createTestUser(t, "mediagrouppreviewpeer")
		convID := ts.createTestGroupDM(t, uploader, peer)
		fileID := uuid.New().String()
		storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
		require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("group-preview")), 13, mimeOctetStream))
		_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id) VALUES ($1, $2, 'file', 2, 'application/octet-stream', 13, $3, 1, $4)`, fileID, uploader, storageKey, convID)
		require.NoError(t, err)

		visible := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		require.Equal(t, http.StatusOK, visible.Code)
		assert.Equal(t, []byte("group-preview"), visible.Body.Bytes())

		_, err = ts.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, uploader)
		require.NoError(t, err)
		removed := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusForbidden, removed.Code)
	})

	t.Run("bridge outside conversation blocks preview fallback", func(t *testing.T) {
		ts := setupMediaTest(t)
		uploader := ts.createTestUser(t, "mediaotherbridge")
		peer := ts.createTestUser(t, "mediaotherbridgepeer")
		other := ts.createTestUser(t, "mediaotherbridgeother")
		convID := ts.createTestDMConversation(t, uploader, peer)
		otherConvID := ts.createTestDMConversation(t, uploader, other)
		fileID := uuid.New().String()
		storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
		require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("other")), 5, mimeOctetStream))
		_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id) VALUES ($1, $2, 'file', 2, 'application/octet-stream', 5, $3, 1, $4)`, fileID, uploader, storageKey, convID)
		require.NoError(t, err)
		otherMessageID := uuid.New().String()
		_, err = ts.db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, type, key_version) VALUES ($1, $2, $3, 'other conversation', 'text', 1)`, otherMessageID, otherConvID, uploader)
		require.NoError(t, err)
		_, err = ts.db.Exec(`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, otherMessageID, fileID)
		require.NoError(t, err)
		response := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("channel-only bridge blocks DM preview fallback", func(t *testing.T) {
		ts := setupMediaTest(t)
		uploader := ts.createTestUser(t, "mediachannelbridge")
		peer := ts.createTestUser(t, "mediachannelbridgepeer")
		convID := ts.createTestDMConversation(t, uploader, peer)
		serverID := ts.createTestServer(t, uploader, "media bridge server")
		channelID := ts.createTestChannel(t, serverID, "media bridge channel")
		fileID := uuid.New().String()
		storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
		require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("channel-only")), 12, mimeOctetStream))
		_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id) VALUES ($1, $2, 'file', 2, 'application/octet-stream', 12, $3, 1, $4)`, fileID, uploader, storageKey, convID)
		require.NoError(t, err)
		messageID := uuid.New().String()
		_, err = ts.db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version) VALUES ($1, $2, $3, 'channel attachment', 1)`, messageID, channelID, uploader)
		require.NoError(t, err)
		_, err = ts.db.Exec(`INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, 0)`, messageID, fileID)
		require.NoError(t, err)
		response := ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
		assert.Equal(t, http.StatusForbidden, response.Code)
	})
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

func TestDeleteMediaRejectsFormerChannelMember(t *testing.T) {
	ts := setupMediaTest(t)
	serverOwner := ts.createTestUser(t, "deletechannelowner")
	uploader := ts.createTestUser(t, "deletechannelleaver")
	serverID := ts.createTestServer(t, serverOwner, "Delete Membership Server")
	channelID := ts.createTestChannel(t, serverID, "delete-membership")
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, uploader)
	require.NoError(t, err)
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err = ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, channelID)
	require.NoError(t, err)
	_, err = ts.db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, uploader)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.True(t, ts.store.hasObject(storageKey))
	var deletedAt sql.NullTime
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
}

func TestDeleteMediaRejectsFormerDMParticipant(t *testing.T) {
	ts := setupMediaTest(t)
	uploader := ts.createTestUser(t, "deletedmleaver")
	peer := ts.createTestUser(t, "deletedmpeer")
	conversationID := ts.createTestDMConversation(t, uploader, peer)
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, conversationID)
	require.NoError(t, err)
	_, err = ts.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, uploader)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.True(t, ts.store.hasObject(storageKey))
	var deletedAt sql.NullTime
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
}

func TestDeleteMediaAllowsCurrentDMParticipant(t *testing.T) {
	ts := setupMediaTest(t)
	uploader := ts.createTestUser(t, "deletedmcurrent")
	peer := ts.createTestUser(t, "deletedmcurrentpeer")
	conversationID := ts.createTestDMConversation(t, uploader, peer)
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, conversationID)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, "DELETE", pathMediaPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, ts.store.hasObject(storageKey))
}

func TestDeleteMediaWaitsForConcurrentMembershipRevocation(t *testing.T) {
	ts := setupMediaTest(t)
	serverOwner := ts.createTestUser(t, "deleteraceowner")
	uploader := ts.createTestUser(t, "deleteraceuploader")
	serverID := ts.createTestServer(t, serverOwner, "Delete Race Server")
	channelID := ts.createTestChannel(t, serverID, "delete-race")
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, uploader)
	require.NoError(t, err)
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err = ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, channelID)
	require.NoError(t, err)

	revocation, err := ts.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := revocation.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("roll back revocation: %v", rollbackErr)
		}
	}()
	_, err = revocation.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, uploader)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodDelete, pathMediaPrefix+fileID, nil)
	ctx, cancel := context.WithCancel(req.Context())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(ctx)
	c.Set("user_id", uploader)
	testdb.AttachRememberedActor(c, uploader)
	c.Params = gin.Params{{Key: "file_id", Value: fileID}}
	done := make(chan struct{})
	go func() {
		ts.handler.DeleteMedia(c)
		close(done)
	}()
	// The deferred revocation rollback runs before this cleanup joins the handler.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("deletion did not stop after request cancellation")
		}
	})
	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		require.NoError(t, ts.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR KEY SHARE OF sm%')`).Scan(&waiting))
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, revocation.Commit())
	require.True(t, waiting, "deletion should wait for the membership row lock")
	select {
	case <-done:
		assert.Equal(t, http.StatusForbidden, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not finish after revocation")
	}
	assert.True(t, ts.store.hasObject(storageKey))
	var deletedAt sql.NullTime
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
}

func TestDeleteMediaWaitsForUploaderLockBeforeDeleting(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "deletelockowner")
	uploader := ts.createTestUser(t, "deletelockuploader")
	serverID := ts.createTestServer(t, owner, "Delete Lock Server")
	channelID := ts.createTestChannel(t, serverID, "delete-lock")
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, uploader)
	require.NoError(t, err)
	fileID := uuid.NewString()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err = ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, channelID)
	require.NoError(t, err)
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, probe.Close()) })

	lockTx, err := ts.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := lockTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("roll back uploader lock: %v", rollbackErr)
		}
	}()
	var lockedID string
	require.NoError(t, lockTx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR UPDATE`, uploader).Scan(&lockedID))
	var txID int64
	require.NoError(t, lockTx.QueryRow(`SELECT txid_current()`).Scan(&txID))

	req := httptest.NewRequest(http.MethodDelete, pathMediaPrefix+fileID, nil)
	ctx, cancel := context.WithCancel(req.Context())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(ctx)
	c.Set("user_id", uploader)
	testdb.AttachRememberedActor(c, uploader)
	c.Params = gin.Params{{Key: "file_id", Value: fileID}}
	done := make(chan struct{})
	go func() {
		ts.handler.DeleteMedia(c)
		close(done)
	}()
	defer func() {
		if rollbackErr := lockTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("release uploader lock: %v", rollbackErr)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("deletion did not stop after request cancellation")
		}
	}()

	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !waiting; {
		waiting = rowLockWaiterExists(t, probe, txID)
		time.Sleep(10 * time.Millisecond)
	}
	objectBeforeRelease := ts.store.hasObject(storageKey)
	require.NoError(t, lockTx.Rollback())
	select {
	case <-done:
		assert.Equal(t, http.StatusOK, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not finish after uploader lock released")
	}
	assert.True(t, waiting, "deletion must acquire the uploader lock before the media row")
	assert.True(t, objectBeforeRelease, "storage must not change while account erasure holds the uploader")
	assert.False(t, ts.store.hasObject(storageKey))
}

func TestDeleteMediaWaitsForDMRetirementBeforeLockingMedia(t *testing.T) {
	ts := setupMediaTest(t)
	uploader := ts.createTestUser(t, "deletedmretirement")
	peer := ts.createTestUser(t, "deletedmretirementpeer")
	conversationID := ts.createTestDMConversation(t, uploader, peer)
	_, err := ts.db.Exec(`UPDATE dm_conversations SET is_group=TRUE WHERE id=$1`, conversationID)
	require.NoError(t, err)
	_, err = ts.db.Exec(`UPDATE dm_participants SET hidden_at=NOW() WHERE conversation_id=$1`, conversationID)
	require.NoError(t, err)
	fileID := uuid.NewString()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err = ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, conversationID)
	require.NoError(t, err)
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, probe.Close()) })

	retirement, err := ts.db.Begin()
	require.NoError(t, err)
	var lockedID string
	require.NoError(t, retirement.QueryRow(`SELECT id FROM dm_conversations WHERE id=$1 FOR UPDATE`, conversationID).Scan(&lockedID))
	_, err = retirement.Exec(`UPDATE dm_participants SET hidden_at=hidden_at WHERE conversation_id=$1`, conversationID)
	require.NoError(t, err)
	var txID int64
	require.NoError(t, retirement.QueryRow(`SELECT txid_current()`).Scan(&txID))

	req := httptest.NewRequest(http.MethodDelete, pathMediaPrefix+fileID, nil)
	ctx, cancel := context.WithCancel(req.Context())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(ctx)
	c.Set("user_id", uploader)
	testdb.AttachRememberedActor(c, uploader)
	c.Params = gin.Params{{Key: "file_id", Value: fileID}}
	done := make(chan struct{})
	go func() {
		ts.handler.DeleteMedia(c)
		close(done)
	}()
	t.Cleanup(func() {
		if err := retirement.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("retirement rollback: %v", err)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("deletion did not stop after request cancellation")
		}
	})
	require.Eventually(t, func() bool { return rowLockWaiterExists(t, probe, txID) }, 5*time.Second, 10*time.Millisecond)
	mediaProbe, err := probe.Begin()
	require.NoError(t, err)
	var available int
	err = mediaProbe.QueryRow(`SELECT 1 FROM media_files WHERE id=$1 FOR UPDATE NOWAIT`, fileID).Scan(&available)
	require.NoError(t, mediaProbe.Rollback())
	require.NoError(t, err, "the deletion must wait for the conversation before taking the media row")

	_, err = retirement.Exec(`DELETE FROM dm_conversations WHERE id=$1`, conversationID)
	require.NoError(t, err)
	require.NoError(t, retirement.Commit())
	select {
	case <-done:
		assert.Equal(t, http.StatusNotFound, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not finish after DM retirement")
	}
}

func TestDeleteMediaBeginFailureDoesNotTouchStorage(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "deletebeginfailure")
	closedDB, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, closedDB.Close())
	ts.handler.db = closedDB

	fileID := uuid.NewString()
	w := ts.doJSON(ts.handler.DeleteMedia, http.MethodDelete, pathMediaPrefix+fileID, user, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, ts.store.objectCount())
}

func TestDeleteMediaMissingUploaderDoesNotTouchStorage(t *testing.T) {
	ts := setupMediaTest(t)
	fileID := uuid.NewString()
	w := ts.doJSON(ts.handler.DeleteMedia, http.MethodDelete, pathMediaPrefix+fileID,
		uuid.NewString(), gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Zero(t, ts.store.objectCount())
}

func TestDeleteMediaDatabaseReadFailureDoesNotTouchStorage(t *testing.T) {
	for _, tc := range []struct {
		name, lockSQL, response string
	}{
		{"uploader", "LOCK TABLE users IN ACCESS EXCLUSIVE MODE", errMsgFailedDeleteFile},
		{"file row", "LOCK TABLE media_files IN ACCESS EXCLUSIVE MODE", errMsgFailedDeleteFile},
		{"conversation", "LOCK TABLE dm_conversations IN ACCESS EXCLUSIVE MODE", errMsgFailedDeleteFile},
		{"membership", "LOCK TABLE dm_participants IN ACCESS EXCLUSIVE MODE", errMsgFailedVerifyAccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			uploader := ts.createTestUser(t, "deletereadfailure")
			peer := ts.createTestUser(t, "deletereadpeer")
			conversationID := ts.createTestDMConversation(t, uploader, peer)
			fileID := uuid.NewString()
			storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
			require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
			_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
				VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, conversationID)
			require.NoError(t, err)

			lockTx, err := ts.db.Begin()
			require.NoError(t, err)
			defer func() { assert.NoError(t, lockTx.Rollback()) }()
			_, err = lockTx.Exec(tc.lockSQL)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodDelete, pathMediaPrefix+fileID, nil).WithContext(ctx)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Set("user_id", uploader)
			testdb.AttachRememberedActor(c, uploader)
			c.Params = gin.Params{{Key: "file_id", Value: fileID}}
			ts.handler.DeleteMedia(c)
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.Contains(t, w.Body.String(), tc.response)
			assert.True(t, ts.store.hasObject(storageKey))
		})
	}
}

func TestDeleteMediaSkippedUpdateKeepsRowAndObject(t *testing.T) {
	ts := setupMediaTest(t)
	uploader := ts.createTestUser(t, "deleteskippedupdate")
	serverID := ts.createTestServer(t, uploader, "Skipped Delete Update")
	channelID := ts.createTestChannel(t, serverID, "skipped-delete-update")
	fileID := uuid.NewString()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, channelID)
	require.NoError(t, err)
	_, err = ts.db.Exec(`CREATE FUNCTION skip_media_soft_delete_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropTriggerErr := ts.db.Exec(`DROP TRIGGER IF EXISTS skip_media_soft_delete_test ON media_files`)
		assert.NoError(t, dropTriggerErr)
		_, dropFunctionErr := ts.db.Exec(`DROP FUNCTION IF EXISTS skip_media_soft_delete_test()`)
		assert.NoError(t, dropFunctionErr)
	})
	_, err = ts.db.Exec(`CREATE TRIGGER skip_media_soft_delete_test BEFORE UPDATE OF deleted_at ON media_files FOR EACH ROW EXECUTE FUNCTION skip_media_soft_delete_test()`)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, http.MethodDelete, pathMediaPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.True(t, ts.store.hasObject(storageKey))
	var deletedAt sql.NullTime
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
}

func TestDeleteMediaUpdateFailureKeepsRowAndObject(t *testing.T) {
	ts := setupMediaTest(t)
	uploader := ts.createTestUser(t, "deleteupdatefailure")
	serverID := ts.createTestServer(t, uploader, "Delete Update Failure")
	channelID := ts.createTestChannel(t, serverID, "delete-update-failure")
	fileID := uuid.NewString()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, channelID)
	require.NoError(t, err)
	_, err = ts.db.Exec(`CREATE FUNCTION reject_media_soft_delete_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic media update refusal'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropTriggerErr := ts.db.Exec(`DROP TRIGGER IF EXISTS reject_media_soft_delete_test ON media_files`)
		assert.NoError(t, dropTriggerErr)
		_, dropFunctionErr := ts.db.Exec(`DROP FUNCTION IF EXISTS reject_media_soft_delete_test()`)
		assert.NoError(t, dropFunctionErr)
	})
	_, err = ts.db.Exec(`CREATE TRIGGER reject_media_soft_delete_test BEFORE UPDATE OF deleted_at ON media_files FOR EACH ROW EXECUTE FUNCTION reject_media_soft_delete_test()`)
	require.NoError(t, err)

	w := ts.doJSON(ts.handler.DeleteMedia, http.MethodDelete, pathMediaPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.True(t, ts.store.hasObject(storageKey))
	var deletedAt sql.NullTime
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
}

type failedAttachmentDeleteStore struct{ *mockStore }

func (s *failedAttachmentDeleteStore) DeleteObject(context.Context, string) error {
	return errors.New("synthetic storage delete failure")
}

func TestDeleteMediaStorageFailureLeavesDurableSweepCandidate(t *testing.T) {
	ts := setupMediaTest(t)
	uploader := ts.createTestUser(t, "deletestoragetry")
	serverID := ts.createTestServer(t, uploader, "Delete Storage Retry")
	channelID := ts.createTestChannel(t, serverID, "delete-storage-retry")
	fileID := uuid.NewString()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.Background(), storageKey, bytes.NewReader([]byte("data")), 4, mimeOctetStream))
	_, err := ts.db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		VALUES ($1, $2, 'file', 2, 'application/octet-stream', 4, $3, 1, $4)`, fileID, uploader, storageKey, channelID)
	require.NoError(t, err)
	ts.handler.store = &failedAttachmentDeleteStore{mockStore: ts.store}

	w := ts.doJSON(ts.handler.DeleteMedia, http.MethodDelete, pathMediaPrefix+fileID, uploader, gin.Params{{Key: "file_id", Value: fileID}})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, ts.store.hasObject(storageKey))
	var deletedAt, reapedAt sql.NullTime
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at, blob_reaped_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt, &reapedAt))
	assert.True(t, deletedAt.Valid)
	assert.False(t, reapedAt.Valid)
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

func TestProxyAvatarAbsentProfileMediaReturnsNoStoreWithoutStorageRead(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "avatarabsent")
	store := &tier1RaceStore{mockStore: newMockStore()}
	ts.handler.store = store

	w := ts.doJSON(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/"+userID, userID, gin.Params{{Key: "user_id", Value: userID}})

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, cacheControlNoStore, w.Header().Get(hdrCacheControl))
	assert.Zero(t, store.getCount(), "absent profile media must be denied before object storage")
}

func TestProxyAvatarInvalidID(t *testing.T) {
	ts := setupMediaTest(t)

	w := ts.doJSON(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/not-uuid", "any", gin.Params{{Key: "user_id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxyAvatarStorageDisabledReturns503(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "avatarstoragedisabled")
	ts.createTestTier1Media(t, userID, tier1StorageKey(purposeAvatar, userID, "", ""))
	ts.handler.store = nil

	var w *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		w = ts.doJSON(ts.handler.ProxyAvatar, "GET", "/api/v1/media/avatars/"+userID, userID, gin.Params{{Key: "user_id", Value: userID}})
	})
	assertStorageDisabledResponse(t, w)
}

func TestProxyProfileMediaSuccessIsNoStore(t *testing.T) {
	for _, tc := range []struct {
		name, path, key string
		handler         func(*Handler, *gin.Context)
	}{
		{"avatar", "/api/v1/media/avatars/", "avatars/", (*Handler).ProxyAvatar},
		{"banner", "/api/v1/media/banners/", "banners/", (*Handler).ProxyBanner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			userID := ts.createTestUser(t, "nostore"+tc.name)
			key := tc.key + userID
			ts.createTestTier1Media(t, userID, key)
			require.NoError(t, ts.store.PutObject(context.Background(), key, bytes.NewReader(makePNG(t, 32, 32)), 100, mimeImagePNG))
			w := ts.doJSON(func(c *gin.Context) { tc.handler(ts.handler, c) }, http.MethodGet, tc.path+userID, userID,
				gin.Params{{Key: "user_id", Value: userID}})
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, cacheControlNoStore, w.Header().Get(hdrCacheControl))
		})
	}
}

func seedProxyLoggingProfile(t *testing.T, ts *testSetup, userID, slot, key string) {
	t.Helper()
	_, err := ts.db.Exec(`
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
		VALUES ($1, $2, 'photo', 1, 'image/png', 100, $3, $4)`,
		uuid.New().String(), userID, key, slot)
	require.NoError(t, err)
}

func TestProfileProxyStorageFailuresDoNotLogSensitiveDetails(t *testing.T) {
	const providerDetail = "storage-backend-detail"
	for _, tc := range []struct {
		name        string
		slot        string
		path        string
		partial     bool
		status      int
		errorDetail string
		message     string
	}{
		{name: "avatar_fetch", slot: ProfileSlotAvatar, path: "/api/v1/media/avatars/", status: http.StatusInternalServerError, errorDetail: providerDetail, message: "Failed to fetch profile media from storage"},
		{name: "banner_fetch", slot: ProfileSlotBanner, path: "/api/v1/media/banners/", status: http.StatusInternalServerError, errorDetail: providerDetail, message: "Failed to fetch profile media from storage"},
		{name: "avatar_stream", slot: ProfileSlotAvatar, path: "/api/v1/media/avatars/", partial: true, status: http.StatusOK, errorDetail: "connection reset mid-transfer", message: "Failed to stream profile media to client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			userID := ts.createTestUser(t, "proxylog"+tc.name)
			generationID := uuid.New().String()
			prefix := "avatars"
			if tc.slot == ProfileSlotBanner {
				prefix = "banners"
			}
			key := prefix + "/" + userID + "/" + generationID
			seedProxyLoggingProfile(t, ts, userID, tc.slot, key)

			if tc.partial {
				ts.store.getPartial = []byte("partial profile media")
			} else {
				ts.store.getErr = errors.New(providerDetail)
			}
			var output bytes.Buffer
			ts.handler.log = logger.NewWithWriter(&output)

			w := ts.doNoAuth(func(c *gin.Context) {
				if tc.slot == ProfileSlotAvatar {
					ts.handler.ProxyAvatar(c)
				} else {
					ts.handler.ProxyBanner(c)
				}
			}, http.MethodGet, tc.path+userID, gin.Params{{Key: "user_id", Value: userID}})

			require.Equal(t, tc.status, w.Code)
			log := output.String()
			require.Contains(t, log, tc.message)
			for _, sensitiveDetail := range []string{key, userID, generationID, tc.errorDetail} {
				require.NotContains(t, log, sensitiveDetail, "proxy failure log leaked sensitive detail")
			}
		})
	}
}

func TestFriendCodeAvatarStorageFailuresDoNotLogSensitiveDetails(t *testing.T) {
	const providerDetail = "storage-backend-detail"
	for _, tc := range []struct {
		name        string
		partial     bool
		errorDetail string
		message     string
	}{
		{name: "fetch", errorDetail: providerDetail, message: "Failed to fetch friend-code avatar from storage"},
		{name: "read", partial: true, errorDetail: "connection reset mid-transfer", message: "Failed to read friend-code avatar from storage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			ownerID := ts.createTestUser(t, "friendproxylog"+tc.name)
			generationID := uuid.New().String()
			key := "avatars/" + ownerID + "/" + generationID
			ts.setUserAvatarURL(t, ownerID, "/api/v1/media/avatars/"+ownerID)
			ts.createTestFriendCode(t, ownerID, "FAVBLZE2", false, nil, 0, 0)
			seedProxyLoggingProfile(t, ts, ownerID, ProfileSlotAvatar, key)

			if tc.partial {
				ts.store.getPartial = []byte("partial avatar")
			} else {
				ts.store.getErr = errors.New(providerDetail)
			}
			var output bytes.Buffer
			ts.handler.log = logger.NewWithWriter(&output)

			w := ts.getFriendCodeAvatar(friendCodeAvatarPath("FAVBLZE2"), "FAVBLZE2")
			require.Equal(t, http.StatusOK, w.Code)
			log := output.String()
			require.Contains(t, log, tc.message)
			for _, sensitiveDetail := range []string{key, ownerID, generationID, tc.errorDetail} {
				require.NotContains(t, log, sensitiveDetail, "friend-code avatar failure log leaked sensitive detail")
			}
		})
	}
}

func TestProxyAvatarTombstoneLookupErrorFailsClosedBeforeGet(t *testing.T) {
	closed, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	store := &tier1RaceStore{mockStore: newMockStore()}
	h := NewHandler(closed, store, logger.New("test"), &config.Config{}, nil, freeTierStub{})
	userID := uuid.New().String()
	w := (&testSetup{handler: h, store: store.mockStore, db: closed}).doJSON(
		h.ProxyAvatar, http.MethodGet, "/api/v1/media/avatars/"+userID, userID,
		gin.Params{{Key: "user_id", Value: userID}},
	)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Zero(t, store.getCount(), "tombstone lookup errors must not reach object storage")
}

func TestProfileMediaAdmissionAllowsInFlightReadAfterErasure(t *testing.T) {
	for _, tc := range []struct {
		name, keyPrefix string
		handle          func(*Handler, *gin.Context)
		path            func(string) string
		params          func(string) gin.Params
		friendCode      bool
		secondStatus    int
	}{
		{"avatar", "avatars/", (*Handler).ProxyAvatar, func(id string) string { return "/api/v1/media/avatars/" + id }, func(id string) gin.Params { return gin.Params{{Key: "user_id", Value: id}} }, false, http.StatusNotFound},
		{"banner", "banners/", (*Handler).ProxyBanner, func(id string) string { return "/api/v1/media/banners/" + id }, func(id string) gin.Params { return gin.Params{{Key: "user_id", Value: id}} }, false, http.StatusNotFound},
		{"friend-code avatar", "avatars/", (*Handler).ProxyFriendCodeAvatar, func(string) string { return friendCodeAvatarPath("ABCDEFGH") }, func(string) gin.Params { return gin.Params{{Key: "code", Value: "ABCDEFGH"}} }, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupMediaTest(t)
			owner := ts.createTestUser(t, "admission"+strings.ReplaceAll(tc.name, "-", ""))
			if tc.friendCode {
				ts.setUserAvatarURL(t, owner, "/api/v1/media/avatars/"+owner)
				ts.createTestFriendCode(t, owner, "ABCDEFGH", false, nil, 0, 0)
			}
			key := tc.keyPrefix + owner
			ts.createTestTier1Media(t, owner, key)
			plaintext := makePNG(t, 32, 32)
			store := &blockingGetStore{mockStore: newMockStore(), started: make(chan struct{}, 1), release: make(chan struct{})}
			require.NoError(t, store.PutObject(context.Background(), key, bytes.NewReader(plaintext), int64(len(plaintext)), mimeImagePNG))
			hA := NewHandler(ts.db, store, logger.New("test"), &config.Config{}, nil, freeTierStub{})
			hB := NewHandler(ts.db, store, logger.New("test"), &config.Config{}, nil, freeTierStub{})
			t.Cleanup(store.releaseGet)
			request := func(h *Handler) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodGet, tc.path(owner), nil)
				c.Params = tc.params(owner)
				handle := tc.handle
				handle(h, c)
				return w
			}
			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { firstDone <- request(hA) }()
			select {
			case <-store.started:
			case <-time.After(2 * time.Second):
				t.Fatal("first reader did not reach object storage")
			}
			require.NoError(t, eraseUserForTier1Race(ts.db, owner, nil, nil))
			second := request(hB)
			assert.Equal(t, tc.secondStatus, second.Code)
			assert.Equal(t, cacheControlNoStore, second.Header().Get(hdrCacheControl))
			assert.Equal(t, 1, store.getCount(), "post-erasure reader must be denied before storage")
			store.releaseGet()
			var first *httptest.ResponseRecorder
			select {
			case first = <-firstDone:
			case <-time.After(2 * time.Second):
				t.Fatal("admitted reader did not finish after release")
			}
			assert.Equal(t, http.StatusOK, first.Code)
			assert.Equal(t, cacheControlNoStore, first.Header().Get(hdrCacheControl))
			assert.Equal(t, plaintext, first.Body.Bytes(), "the pre-erasure admitted read may finish with the object it fetched")
		})
	}
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

func TestUploadDMIconRejectsRevokedAdminBeforeObjectWrite(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconrevokedadmin")
	member := ts.createTestUser(t, "dmiconrevokedmember")
	convID := ts.createTestGroupDM(t, admin, member)
	body, ct := multipartBody(t, "file", fileIconPng, makePNG(t, 100, 100), map[string]string{
		keyConversationID: convID,
	})

	revocation, err := ts.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := revocation.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("roll back icon revocation: %v", rollbackErr)
		}
	}()
	_, err = revocation.Exec(`UPDATE dm_participants SET role = 'member' WHERE conversation_id = $1 AND user_id = $2`, convID, admin)
	require.NoError(t, err)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.doMultipart(ts.handler.UploadDMIcon, http.MethodPost, pathUploadDMIcon, admin, body, ct)
	}()
	waiting := false
	var early *httptest.ResponseRecorder
waitForLock:
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		select {
		case early = <-done:
			break waitForLock
		default:
		}
		require.NoError(t, ts.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FROM dm_participants%' AND query LIKE '%FOR SHARE%')`).Scan(&waiting))
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	objectWrittenBeforeRevocation := ts.store.objectCount() != 0
	require.NoError(t, revocation.Commit())
	if early != nil {
		t.Fatalf("icon upload completed before role revocation committed: HTTP %d, object written: %t", early.Code, objectWrittenBeforeRevocation)
	}
	require.True(t, waiting, "icon upload should wait for the admin row lock")
	select {
	case w := <-done:
		assert.Equal(t, http.StatusForbidden, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("icon upload did not finish after role revocation")
	}
	assert.False(t, objectWrittenBeforeRevocation)
	assert.Zero(t, ts.store.objectCount())
}

func TestUploadDMIconWaitsForUploaderErasureBeforeObjectWrite(t *testing.T) {
	ts := setupMediaTest(t)
	creator := ts.createTestUser(t, "dmiconerasurecreator")
	uploader := ts.createTestUser(t, "dmiconerasureadmin")
	convID := ts.createTestGroupDM(t, creator, uploader)
	_, err := ts.db.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, uploader)
	require.NoError(t, err)
	body, ct := multipartBody(t, "file", fileIconPng, makePNG(t, 100, 100), map[string]string{keyConversationID: convID})
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, probe.Close()) })

	locked := make(chan int64, 1)
	release := make(chan struct{})
	erasureDone := make(chan error, 1)
	go func() { erasureDone <- eraseUserForTier1Race(ts.db, uploader, locked, release) }()
	txID := <-locked
	uploadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		uploadDone <- ts.doMultipart(ts.handler.UploadDMIcon, http.MethodPost, pathUploadDMIcon, uploader, body, ct)
	}()

	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !waiting; {
		waiting = rowLockWaiterExists(t, probe, txID)
		time.Sleep(10 * time.Millisecond)
	}
	objectsBeforeErasure := ts.store.objectCount()
	close(release)
	select {
	case err := <-erasureDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("account erasure did not finish")
	}
	select {
	case w := <-uploadDone:
		assert.Equal(t, http.StatusForbidden, w.Code)
	case <-time.After(10 * time.Second):
		t.Fatal("icon upload did not finish after erasure")
	}
	assert.True(t, waiting, "icon upload should wait for the uploader's user lock")
	assert.Zero(t, objectsBeforeErasure, "icon object must not be written before erasure commits")
	assert.Zero(t, ts.store.objectCount())
}

type blockingDMIconStore struct {
	*mockStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockingDMIconStore) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	close(s.entered)
	<-s.release
	return s.mockStore.PutObject(ctx, key, reader, size, contentType)
}

func TestUploadDMIconHoldsAdminRoleUntilObjectWrite(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconlockadmin")
	member := ts.createTestUser(t, "dmiconlockmember")
	convID := ts.createTestGroupDM(t, admin, member)
	body, ct := multipartBody(t, "file", fileIconPng, makePNG(t, 100, 100), map[string]string{
		keyConversationID: convID,
	})
	store := &blockingDMIconStore{mockStore: ts.store, entered: make(chan struct{}), release: make(chan struct{})}
	ts.handler.store = store
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.doMultipart(ts.handler.UploadDMIcon, http.MethodPost, pathUploadDMIcon, admin, body, ct)
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		close(store.release)
		t.Fatal("icon upload did not reach object storage")
	}

	demotionDone := make(chan error, 1)
	go func() {
		_, err := ts.db.Exec(`UPDATE dm_participants SET role = 'member' WHERE conversation_id = $1 AND user_id = $2`, convID, admin)
		demotionDone <- err
	}()
	waiting := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		require.NoError(t, ts.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%UPDATE dm_participants SET role%')`).Scan(&waiting))
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Zero(t, ts.store.objectCount(), "the fixed key should not be overwritten before PutObject resumes")
	close(store.release)
	require.True(t, waiting, "role demotion should wait until the authorized object write commits")
	select {
	case w := <-done:
		assert.Equal(t, http.StatusCreated, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("icon upload did not finish")
	}
	select {
	case err := <-demotionDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("role demotion did not finish")
	}
	assert.Equal(t, 1, ts.store.objectCount())
}

func callStoreDMIcon(t *testing.T, ts *testSetup, userID, conversationID string) *httptest.ResponseRecorder {
	t.Helper()
	data := makePNG(t, 100, 100)
	processed := &ProcessedImage{Data: data, ContentType: mimePNG, Width: 100, Height: 100}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, pathUploadDMIcon, nil)
	ts.handler.storeDMIcon(c, ts.handler.store, userID, conversationID, int64(len(data)), processed)
	return w
}

func TestStoreDMIconDeniedAfterGroupOrParticipantRemoval(t *testing.T) {
	for _, removeGroup := range []bool{true, false} {
		name := "participant"
		if removeGroup {
			name = "group"
		}
		t.Run(name, func(t *testing.T) {
			ts := setupMediaTest(t)
			admin := ts.createTestUser(t, "dmiconremovedadmin")
			peer := ts.createTestUser(t, "dmiconremovedpeer")
			convID := ts.createTestGroupDM(t, admin, peer)
			var err error
			if removeGroup {
				_, err = ts.db.Exec(`DELETE FROM dm_conversations WHERE id = $1`, convID)
			} else {
				_, err = ts.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, admin)
			}
			require.NoError(t, err)

			w := callStoreDMIcon(t, ts, admin, convID)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Zero(t, ts.store.objectCount())
		})
	}
}

func TestStoreDMIconDatabaseUnavailableDoesNotWrite(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmicondbunavailable")
	peer := ts.createTestUser(t, "dmicondbunavailablepeer")
	convID := ts.createTestGroupDM(t, admin, peer)
	closedDB, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, closedDB.Close())
	ts.handler.db = closedDB

	w := callStoreDMIcon(t, ts, admin, convID)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, ts.store.objectCount())
}

func TestStoreDMIconObjectWriteFailureLeavesNoMetadata(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconputfailure")
	peer := ts.createTestUser(t, "dmiconputfailurepeer")
	convID := ts.createTestGroupDM(t, admin, peer)
	ts.store.putErr = errors.New("synthetic icon write failure")

	w := callStoreDMIcon(t, ts, admin, convID)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, ts.store.objectCount())
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1`, fmt.Sprintf(fmtDMIconsKey, convID)).Scan(&count))
	assert.Zero(t, count)
}

func TestStoreDMIconURLUpdateFailureRollsBackMetadata(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconurlfailure")
	peer := ts.createTestUser(t, "dmiconurlfailurepeer")
	convID := ts.createTestGroupDM(t, admin, peer)
	_, err := ts.db.Exec(`CREATE FUNCTION reject_dm_icon_url_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic icon URL refusal'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropTriggerErr := ts.db.Exec(`DROP TRIGGER IF EXISTS reject_dm_icon_url_test ON dm_conversations`)
		assert.NoError(t, dropTriggerErr)
		_, dropFunctionErr := ts.db.Exec(`DROP FUNCTION IF EXISTS reject_dm_icon_url_test()`)
		assert.NoError(t, dropFunctionErr)
	})
	_, err = ts.db.Exec(`CREATE TRIGGER reject_dm_icon_url_test BEFORE UPDATE OF icon_url ON dm_conversations FOR EACH ROW EXECUTE FUNCTION reject_dm_icon_url_test()`)
	require.NoError(t, err)

	w := callStoreDMIcon(t, ts, admin, convID)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.True(t, ts.store.hasObject(fmt.Sprintf(fmtDMIconsKey, convID)), "a failed record must not delete the fixed-key icon")
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1`, fmt.Sprintf(fmtDMIconsKey, convID)).Scan(&count))
	assert.Zero(t, count)
}

func TestStoreDMIconMetadataInsertFailureKeepsFixedKeyObject(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconinsertfailure")
	peer := ts.createTestUser(t, "dmiconinsertfailurepeer")
	convID := ts.createTestGroupDM(t, admin, peer)
	_, err := ts.db.Exec(`CREATE FUNCTION reject_dm_icon_insert_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic icon insert refusal'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropTriggerErr := ts.db.Exec(`DROP TRIGGER IF EXISTS reject_dm_icon_insert_test ON media_files`)
		assert.NoError(t, dropTriggerErr)
		_, dropFunctionErr := ts.db.Exec(`DROP FUNCTION IF EXISTS reject_dm_icon_insert_test()`)
		assert.NoError(t, dropFunctionErr)
	})
	_, err = ts.db.Exec(`CREATE TRIGGER reject_dm_icon_insert_test BEFORE INSERT ON media_files FOR EACH ROW EXECUTE FUNCTION reject_dm_icon_insert_test()`)
	require.NoError(t, err)

	w := callStoreDMIcon(t, ts, admin, convID)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.True(t, ts.store.hasObject(fmt.Sprintf(fmtDMIconsKey, convID)))
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1`, fmt.Sprintf(fmtDMIconsKey, convID)).Scan(&count))
	assert.Zero(t, count)
}

func TestStoreDMIconCommitFailureKeepsFixedKeyObject(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "dmiconcommitfailure")
	peer := ts.createTestUser(t, "dmiconcommitfailurepeer")
	convID := ts.createTestGroupDM(t, admin, peer)
	_, err := ts.db.Exec(`CREATE FUNCTION reject_dm_icon_commit_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic icon commit refusal'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropTriggerErr := ts.db.Exec(`DROP TRIGGER IF EXISTS reject_dm_icon_commit_test ON media_files`)
		assert.NoError(t, dropTriggerErr)
		_, dropFunctionErr := ts.db.Exec(`DROP FUNCTION IF EXISTS reject_dm_icon_commit_test()`)
		assert.NoError(t, dropFunctionErr)
	})
	_, err = ts.db.Exec(`CREATE CONSTRAINT TRIGGER reject_dm_icon_commit_test AFTER INSERT ON media_files DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_dm_icon_commit_test()`)
	require.NoError(t, err)

	w := callStoreDMIcon(t, ts, admin, convID)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.True(t, ts.store.hasObject(fmt.Sprintf(fmtDMIconsKey, convID)), "a failed commit must not delete the fixed-key icon")
	var count int
	require.NoError(t, ts.db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1`, fmt.Sprintf(fmtDMIconsKey, convID)).Scan(&count))
	assert.Zero(t, count)
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

// TestProxyDMIconNonmember denies a known group UUID with a stored icon.
func TestProxyDMIconNonmember(t *testing.T) {
	ts := setupMediaTest(t)
	admin := ts.createTestUser(t, "proxydmadm")
	member := ts.createTestUser(t, "proxydmmem")
	outsider := ts.createTestUser(t, "proxydmout")
	convID := ts.createTestGroupDM(t, admin, member)
	key := fmt.Sprintf(fmtDMIconsKey, convID)
	require.NoError(t, ts.store.PutObject(context.Background(), key, bytes.NewReader(makePNG(t, 64, 64)), 100, mimeImagePNG))
	w := ts.doJSON(ts.handler.ProxyDMIcon, http.MethodGet, "/api/v1/media/dm-icons/"+convID, outsider, gin.Params{{Key: "conversationId", Value: convID}})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.JSONEq(t, `{"error":"Not found"}`, w.Body.String())
	assert.Zero(t, ts.store.GetObjectCalls())
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
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
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, pngData, w.Body.Bytes())
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
	testdb.AttachRememberedActor(c, "00000000-0000-0000-0000-000000000001")
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
	testdb.AttachRememberedActor(c, "00000000-0000-0000-0000-000000000001")
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
	data, contentType := ts.storedObject(t, ts.liveProfileStorageKey(t, userID, "avatar"))
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
	assert.False(t, ts.store.hasObject("avatars/"+userID), "nothing stored on reject")
}

// Aardvark regression: animation entitlement is determined from the container
// structure before full GIF decoding. This starts with a valid static GIF and
// appends a second, structurally complete image descriptor whose empty image
// data makes gif.DecodeAll fail. The structure classifier can still prove the
// upload is animated without materializing either frame.
func TestUploadAvatarAnimatedGIF_FreeRejectedBeforeFullDecode(t *testing.T) {
	raw := makeStaticGIFUpload(t)
	require.Equal(t, gifTrailer, raw[len(raw)-1], "static fixture ends at the GIF trailer")
	raw = append(raw[:len(raw)-1],
		gifImageDescriptor,
		0, 0, // left
		0, 0, // top
		1, 0, // width
		1, 0, // height
		0, // packed: no local color table
		8, // LZW minimum code size
		0, // empty image-data sub-block chain
		gifTrailer,
	)
	animated, err := isAnimatedGIFStructure(raw)
	require.NoError(t, err)
	require.True(t, animated, "fixture must be structurally identifiable as animated before decode")
	_, err = gif.DecodeAll(bytes.NewReader(raw))
	require.Error(t, err, "fixture must fail if the handler performs a full decode before entitlement")

	body, ct := multipartBody(t, "file", "avatar.gif", raw, nil)
	req := httptest.NewRequest(http.MethodPost, pathUploadAvatar, body)
	req.Header.Set(hdrContentType, ct)
	require.NoError(t, req.ParseMultipartForm(1<<20))
	file, _, err := req.FormFile("file")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h := NewHandler(nil, nil, logger.New("test"), &config.Config{}, nil, freeTierStub{})
	processed, ok := h.processGIFTier1Image(c, file, tier1ImageJob{
		userID:  "test-free-user",
		purpose: purposeAvatar,
		maxW:    AvatarMaxDim,
		maxH:    AvatarMaxDim,
	})

	require.False(t, ok)
	require.Nil(t, processed)
	require.Equal(t, http.StatusForbidden, w.Code,
		"free-user animation entitlement must be rejected before full GIF decoding; body: %s", w.Body.String())
	resp := parseBody(t, w)
	assert.Equal(t, "animated_profile_premium", resp["code"], "pre-decode rejection keeps the typed upsell code")
}

func TestProcessGIFTier1Image_ReturnsBusyWhenProcessingSlotIsOccupied(t *testing.T) {
	raw := makeStaticGIFUpload(t)
	body, ct := multipartBody(t, "file", "avatar.gif", raw, nil)
	req := httptest.NewRequest(http.MethodPost, pathUploadAvatar, body)
	req.Header.Set(hdrContentType, ct)
	require.NoError(t, req.ParseMultipartForm(1<<20))
	file, _, err := req.FormFile("file")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h := NewHandler(nil, nil, logger.New("test"), &config.Config{}, nil, freeTierStub{})

	gifProcessingSlots <- struct{}{}
	t.Cleanup(func() { <-gifProcessingSlots })
	processed, ok := h.processGIFTier1Image(c, file, tier1ImageJob{
		userID:  "test-free-user",
		purpose: purposeAvatar,
		maxW:    AvatarMaxDim,
		maxH:    AvatarMaxDim,
	})
	require.False(t, ok)
	require.Nil(t, processed)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestUploadAvatarGIFDeclaredPNG_SerializesAndFlattens(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "mislabeledgif")
	raw := makeAnimatedGIFUpload(t)
	makeBody := func() (*bytes.Buffer, string) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="file"; filename="avatar.png"`)
		header.Set("Content-Type", mimePNG)
		part, err := writer.CreatePart(header)
		require.NoError(t, err)
		_, err = part.Write(raw)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		return &body, writer.FormDataContentType()
	}

	gifProcessingSlots <- struct{}{}
	slotHeld := true
	t.Cleanup(func() {
		if slotHeld {
			<-gifProcessingSlots
		}
	})
	body, ct := makeBody()
	busy := ts.doMultipart(ts.handler.UploadAvatar, http.MethodPost, pathUploadAvatar, userID, body, ct)
	require.Equal(t, http.StatusServiceUnavailable, busy.Code, "body: %s", busy.Body.String())
	require.Equal(t, "1", busy.Header().Get("Retry-After"))
	<-gifProcessingSlots
	slotHeld = false

	body, ct = makeBody()
	success := ts.doMultipart(ts.handler.UploadAvatar, http.MethodPost, pathUploadAvatar, userID, body, ct)
	require.Equal(t, http.StatusCreated, success.Code, "body: %s", success.Body.String())
	data, contentType := ts.storedObject(t, ts.liveProfileStorageKey(t, userID, "avatar"))
	require.Equal(t, mimePNG, contentType, "mislabeled animation remains flattened")
	_, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
}

type gifProbeFaultFile struct {
	*bytes.Reader
	readErr error
	seekErr error
}

func (f *gifProbeFaultFile) Read(p []byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return f.Reader.Read(p)
}

func (f *gifProbeFaultFile) Seek(offset int64, whence int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	return f.Reader.Seek(offset, whence)
}

func (f *gifProbeFaultFile) Close() error { return nil }

func TestProcessTier1Image_GIFProbeFailureRejectsUpload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readErr error
		seekErr error
	}{
		{name: "read failure", readErr: io.ErrClosedPipe},
		{name: "rewind failure", seekErr: io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			h := NewHandler(nil, nil, logger.New("test"), &config.Config{}, nil, freeTierStub{})
			file := &gifProbeFaultFile{Reader: bytes.NewReader(makeStaticGIFUpload(t)), readErr: tc.readErr, seekErr: tc.seekErr}
			processed, ok := h.processTier1Image(c, file, tier1ImageJob{
				userID: "test-user", purpose: purposeAvatar, contentType: mimePNG,
				maxW: AvatarMaxDim, maxH: AvatarMaxDim,
			})
			require.False(t, ok)
			require.Nil(t, processed)
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestHasGIFSignature_ShortFileRewindsWithoutClassifyingGIF(t *testing.T) {
	file := &gifProbeFaultFile{Reader: bytes.NewReader([]byte("GIF"))}
	isGIF, err := hasGIFSignature(file)
	require.NoError(t, err)
	require.False(t, isGIF)
	pos, err := file.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	require.Zero(t, pos)
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
	_, contentType := ts.storedObject(t, ts.liveProfileStorageKey(t, userID, "avatar"))
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

// ============================================================
// ProxyFriendCodeAvatar (#945)
//
// The internal/api suite drives this handler through the production route and
// proves the privacy contract end to end. It cannot credit coverage to THIS
// package: `go test -coverprofile` instruments only the package under test, so
// a test binary living in another directory records nothing here. The tests
// below exercise the handler from its own package, branch by branch.
// ============================================================

const (
	friendCodesPath        = "/api/v1/friends/codes/"
	friendCodeAvatarSuffix = "/avatar"
	mimeImageSVG           = "image/svg+xml"
)

// setUserAvatarURL points a user's avatar_url at a stored object, which is what
// lets ProxyFriendCodeAvatar reach the object store instead of the fallback.
func (ts *testSetup) setUserAvatarURL(t *testing.T, userID, avatarURL string) {
	t.Helper()
	_, err := ts.db.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`, avatarURL, userID)
	require.NoError(t, err)
}

// createTestFriendCode inserts a friend_codes row verbatim. maxUses is written
// as given: 0 means "unlimited" to the handler's validity predicate, so the
// exhausted case is spelled maxUses=1, useCount=1.
func (ts *testSetup) createTestFriendCode(
	t *testing.T, userID, code string, revoked bool, expiresAt *time.Time, maxUses, useCount int,
) {
	t.Helper()
	_, err := ts.db.Exec(
		`INSERT INTO friend_codes (user_id, code, max_uses, use_count, is_revoked, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, code, maxUses, useCount, revoked, expiresAt,
	)
	require.NoError(t, err)
}

// getFriendCodeAvatar invokes ProxyFriendCodeAvatar over a complete wire path
// while supplying the DECODED :code gin would have extracted from it - so a
// percent-encoded request reaches the handler in exactly the state the router
// leaves it in, which is the state the RawPath guard exists to reject.
func (ts *testSetup) getFriendCodeAvatar(path, code string) *httptest.ResponseRecorder {
	return ts.doNoAuth(
		ts.handler.ProxyFriendCodeAvatar, http.MethodGet, path,
		gin.Params{{Key: "code", Value: code}},
	)
}

// friendCodeAvatarPath is the canonical wire path for a code's avatar.
func friendCodeAvatarPath(code string) string {
	return friendCodesPath + code + friendCodeAvatarSuffix
}

func TestProxyFriendCodeAvatarSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "fcavatarok")
	ts.setUserAvatarURL(t, owner, "/api/v1/media/avatars/"+owner)
	ts.createTestFriendCode(t, owner, "ABCDEFGH", false, nil, 0, 0)

	avatar := makePNG(t, 32, 32)
	key := fmt.Sprintf("avatars/%s", owner)
	ts.createTestTier1Media(t, owner, key)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(avatar), 100, mimeImagePNG))

	w := ts.getFriendCodeAvatar(friendCodeAvatarPath("ABCDEFGH"), "ABCDEFGH")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, avatar, w.Body.Bytes(), "the owner's avatar must be proxied, never redirected to")
	assert.Equal(t, mimeImagePNG, w.Header().Get(hdrContentType))
	assert.Equal(t, cacheControlNoStore, w.Header().Get(hdrCacheControl))
	// The route is keyed by CODE precisely so the owner's UUID never escapes -
	// including via a redirect, which would put it in Location. Sweep every
	// header rather than the one or two a redirect would have used.
	assert.Empty(t, w.Header().Get("Location"))
	for name, values := range w.Result().Header {
		for _, v := range values {
			assert.NotContainsf(t, v, owner, "header %s leaked the owner UUID", name)
		}
	}
}

func TestProxyFriendCodeAvatarTombstoneUsesUniformFallback(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "fcavatartombstone")
	ts.setUserAvatarURL(t, owner, "/api/v1/media/avatars/"+owner)
	ts.createTestFriendCode(t, owner, "ABCDEFGH", false, nil, 0, 0)
	key := "avatars/" + owner
	_, err := ts.db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
	require.NoError(t, err)
	store := &tier1RaceStore{mockStore: newMockStore()}
	ts.handler.store = store

	w := ts.getFriendCodeAvatar(friendCodeAvatarPath("ABCDEFGH"), "ABCDEFGH")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, cacheControlNoStore, w.Header().Get(hdrCacheControl))
	assert.Equal(t, []byte(invitecodes.PublicInviteIconSVG), w.Body.Bytes())
	assert.Zero(t, store.getCount(), "tombstoned friend-code avatars must not reach object storage")
}

// TestProxyFriendCodeAvatarFallbackClasses proves every rejected class serves
// the shared silhouette, byte for byte and header for header. An attacker who
// could tell "unknown code" from "revoked code" would have an enumeration
// oracle, so the assertion is byte equality against a reference - not a status
// code, which all of them share with the success path anyway.
func TestProxyFriendCodeAvatarFallbackClasses(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "fcavatarfb")
	ts.setUserAvatarURL(t, owner, "/api/v1/media/avatars/"+owner)
	ts.createTestTier1Media(t, owner, "avatars/"+owner)
	require.NoError(t, ts.store.PutObject(
		context.TODO(), "avatars/"+owner, bytes.NewReader(makePNG(t, 32, 32)), 100, mimeImagePNG,
	))

	past := time.Now().UTC().Add(-time.Hour)
	ts.createTestFriendCode(t, owner, "ABCDEFGH", false, nil, 0, 0) // live, has avatar
	ts.createTestFriendCode(t, owner, "BCDEFGHJ", true, nil, 0, 0)  // revoked
	ts.createTestFriendCode(t, owner, "CDEFGHJK", false, &past, 0, 0)
	ts.createTestFriendCode(t, owner, "DEFGHJKL", false, nil, 1, 1) // exhausted

	// A live code whose owner has no avatar at all: valid, but nothing to serve.
	bare := ts.createTestUser(t, "fcavatarbare")
	ts.createTestFriendCode(t, bare, "EFGHJKLM", false, nil, 0, 0)

	reference := ts.getFriendCodeAvatar(friendCodeAvatarPath("ZZZZZZZZ"), "ZZZZZZZZ")
	require.Equal(t, http.StatusOK, reference.Code)
	require.Contains(t, reference.Header().Get(hdrContentType), mimeImageSVG)
	require.Equal(t, cacheControlNoStore, reference.Header().Get(hdrCacheControl))

	for _, tc := range []struct{ name, path, code string }{
		{"malformed charset", friendCodeAvatarPath("AAAA000I"), "AAAA000I"},
		{"length 7", friendCodeAvatarPath("AAAAAAA"), "AAAAAAA"},
		{"length 9", friendCodeAvatarPath("AAAAAAAAA"), "AAAAAAAAA"},
		{"unknown", friendCodeAvatarPath("ZZZZZZZZ"), "ZZZZZZZZ"},
		{"revoked", friendCodeAvatarPath("BCDEFGHJ"), "BCDEFGHJ"},
		{"expired", friendCodeAvatarPath("CDEFGHJK"), "CDEFGHJK"},
		{"max used", friendCodeAvatarPath("DEFGHJKL"), "DEFGHJKL"},
		{"owner has no avatar", friendCodeAvatarPath("EFGHJKLM"), "EFGHJKLM"},
		// Percent-encoded: gin routes on the DECODED path while the edge
		// rate-limit rule matches the RAW one, so this reaches the handler with
		// no managed challenge and no edge bucket. The code is a LIVE one whose
		// owner does have an avatar - serving those bytes here is exactly the
		// bypass the guard closes (#945, VULN-001).
		{"encoded separator", friendCodesPath + "ABCDEFGH%2Favatar", "ABCDEFGH"},
		{"encoded character inside the code", friendCodesPath + "%41BCDEFGH" + friendCodeAvatarSuffix, "ABCDEFGH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ts.getFriendCodeAvatar(tc.path, tc.code)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, reference.Body.Bytes(), w.Body.Bytes(),
				"every rejected class must serve the fallback, byte for byte")
			assert.Equal(t, reference.Header().Get(hdrContentType), w.Header().Get(hdrContentType))
			assert.Equal(t, reference.Header().Get(hdrCacheControl), w.Header().Get(hdrCacheControl))
			assert.NotContains(t, w.Body.String(), owner)
		})
	}
}

// TestProxyFriendCodeAvatarDatabaseError isolates the handler's defensive 500
// branch. The pool is closed rather than a shared table renamed: every query on
// a closed pool fails with a real driver error and no other test's state moves.
func TestProxyFriendCodeAvatarDatabaseError(t *testing.T) {
	closed, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	store := newMockStore()
	h := NewHandler(closed, store, logger.New("test"), &config.Config{}, nil, freeTierStub{})
	ts := &testSetup{handler: h, store: store, db: closed}

	w := ts.getFriendCodeAvatar(friendCodeAvatarPath("ABCDEFGH"), "ABCDEFGH")

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"`+errMsgInternalServer+`"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), "ABCDEFGH", "the code is bearer material")
}

func TestProfileTier1OwnedAdmissionAndSlotResolution(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "profileadmitowner")
	other := ts.createTestUser(t, "profileadmitother")
	key := "avatars/" + owner + "/" + uuid.NewString()
	_, err := ts.db.Exec(`
		INSERT INTO media_files (
			id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot
		) VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/png', 4, $2, 'avatar')`, owner, key)
	require.NoError(t, err)

	admitted, err := ProfileTier1MediaOwnedAdmitted(context.Background(), ts.db, key, owner)
	require.NoError(t, err)
	assert.True(t, admitted)
	admitted, err = ProfileTier1MediaOwnedAdmitted(context.Background(), ts.db, key, other)
	require.NoError(t, err)
	assert.False(t, admitted)
	admitted, err = ProfileTier1MediaOwnedSlotAdmitted(context.Background(), ts.db, owner, ProfileSlotAvatar)
	require.NoError(t, err)
	assert.True(t, admitted)

	_, _, err = ProfileTier1StorageKey(context.Background(), ts.db, owner, "invalid")
	require.ErrorContains(t, err, "invalid profile slot")
	_, err = ts.db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
	require.NoError(t, err)
	admitted, err = ProfileTier1MediaOwnedAdmitted(context.Background(), ts.db, key, owner)
	require.NoError(t, err)
	assert.False(t, admitted)
	admitted, err = ProfileTier1MediaOwnedSlotAdmitted(context.Background(), ts.db, owner, ProfileSlotAvatar)
	require.NoError(t, err)
	assert.False(t, admitted)
}

// =====================================================================
// Feedback Screenshot Upload/Proxy Tests (#1747 review)
// =====================================================================

const pathUploadFeedbackScreenshot = "/api/v1/media/upload/feedback-screenshot"
const feedbackScreenshotsPathPrefix = "/api/v1/media/feedback-screenshots/"

func TestUploadFeedbackScreenshotSuccess(t *testing.T) {
	ts := setupMediaTest(t)
	counters := &mediaOpsCounterSpy{}
	ts.handler.SetOpsCounter(counters)
	userID := ts.createTestUser(t, "screenshotuser")

	body, ct := multipartBody(t, "file", "screenshot.png", makePNG(t, 200, 200), nil)

	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)

	require.Equal(t, http.StatusCreated, w.Code)
	resp := parseBody(t, w)
	url, ok := resp["url"].(string)
	require.True(t, ok, "url must be a string, got %#v", resp["url"])
	id, ok := strings.CutPrefix(url, "/api/v1/media/feedback-screenshots/")
	require.True(t, ok, "url must be shaped /api/v1/media/feedback-screenshots/<uuid>, got %q", url)
	parsed, err := uuid.Parse(id)
	require.NoError(t, err)
	assert.Equal(t, parsed.String(), id, "the served id must be the canonical UUID form")
	assert.Equal(t, 1, counters.uploads)
}

func TestUploadFeedbackScreenshotRejectsNonImage(t *testing.T) {
	ts := setupMediaTest(t)
	counters := &mediaOpsCounterSpy{}
	ts.handler.SetOpsCounter(counters)
	userID := ts.createTestUser(t, "screenshotbadtype")

	body, ct := multipartBody(t, "file", "notes.txt", []byte("not an image"), nil)

	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Invalid image type")
	assert.Zero(t, counters.uploads)
}

// TestUploadFeedbackScreenshotRejectsGIF locks in Fix 2 (#1747 review): the
// feedback-screenshot purpose narrows the shared Tier 1 allowlist to
// PNG/JPEG/WebP. GIF remains a valid Tier 1 type for every OTHER purpose
// (avatars/banners/dm-icons/server-icons) — see TestUploadAvatarInvalidType's
// sibling coverage for those, which this test must not narrow.
func TestUploadFeedbackScreenshotRejectsGIF(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "screenshotgif")

	frame := solidPalettedFrame(t, image.Rect(0, 0, 10, 10), gifColRed, gifTestPalette())
	gifData := encodeTestGIF(t, []*image.Paletted{frame}, []int{0}, []byte{0}, 0, 10, 10)
	body, ct := multipartBody(t, "file", "screenshot.gif", gifData, nil)

	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)

	require.Equal(t, http.StatusBadRequest, w.Code)
	resp := parseBody(t, w)
	assert.Contains(t, resp["error"], "Invalid image type")
	assert.NotContains(t, resp["allowed_types"], mimeGIF)
}

func TestProxyFeedbackScreenshotRequiresLiveRow(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "screenshotreader")
	id := uuid.New().String()
	key := fmt.Sprintf("feedback-screenshots/%s", id)
	jpeg, err := processImage(bytes.NewReader(makePNG(t, 64, 64)), purposeFeedbackScreenshot, FeedbackScreenshotMaxDim, FeedbackScreenshotMaxDim)
	require.NoError(t, err)
	require.NoError(t, ts.store.PutObject(context.TODO(), key, bytes.NewReader(jpeg.Data), int64(len(jpeg.Data)), mimeImageJPEG))

	proxy := func() *httptest.ResponseRecorder {
		return ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, "GET", feedbackScreenshotsPathPrefix+id, gin.Params{{Key: "id", Value: id}})
	}
	assert.Equal(t, http.StatusNotFound, proxy().Code, "a rowless object must not be public")
	assert.Zero(t, ts.store.GetObjectCalls())
	_, err = ts.db.Exec(`INSERT INTO media_files
		(uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		VALUES ($1, 'photo', 1, $2, 100, $3)`, userID, mimeImageJPEG, key)
	require.NoError(t, err)
	w := proxy()

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mimeImageJPEG, w.Header().Get(hdrContentType))
	assert.Equal(t, cacheControlNoStore, w.Header().Get(headerCacheControl))
	_, err = ts.db.Exec(`DELETE FROM media_files WHERE storage_key = $1`, key)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, proxy().Code)
	assert.Equal(t, 1, ts.store.GetObjectCalls(), "erasure must block before object storage")
}

func TestProxyFeedbackScreenshotMissingObjectAndBackendFailure(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "screenshotfault")
	id := uuid.New().String()
	key := "feedback-screenshots/" + id
	_, err := ts.db.Exec(`INSERT INTO media_files
		(uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		VALUES ($1, 'photo', 1, $2, 100, $3)`, userID, mimeImageJPEG, key)
	require.NoError(t, err)

	proxy := func() *httptest.ResponseRecorder {
		return ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, "GET", feedbackScreenshotsPathPrefix+id, gin.Params{{Key: "id", Value: id}})
	}
	missing := proxy()
	assert.Equal(t, http.StatusNotFound, missing.Code)
	assert.Equal(t, cacheControlNoStore, missing.Header().Get(headerCacheControl))

	ts.store.getErr = errors.New("backend unavailable")
	unavailable := proxy()
	assert.Equal(t, http.StatusServiceUnavailable, unavailable.Code)
	assert.Contains(t, unavailable.Body.String(), errMsgStorageUnavailable)
	assert.Equal(t, cacheControlNoStore, unavailable.Header().Get(headerCacheControl))
}

func TestUploadFeedbackScreenshotRefusesUnavailableWriteBackend(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "screenshotwritefault")
	ts.handler.SetWriteRouter(stubWriteRouter{screenshotErr: errors.New("R2 unavailable")})
	body, ct := multipartBody(t, "file", "screenshot.png", makePNG(t, 32, 32), nil)
	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, ts.store.objects, "an unavailable R2 backend must not fall back to MinIO")
	vendor := newMockStore()
	vendor.putErr = errors.New("R2 write unavailable")
	ts.handler.SetWriteRouter(stubWriteRouter{screenshot: vendor, screenshotID: "r2-feedback-useast"})
	body, ct = multipartBody(t, "file", "screenshot.png", makePNG(t, 32, 32), nil)
	w = ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)
	require.Equal(t, http.StatusCreated, w.Code)
	url, ok := parseBody(t, w)["url"].(string)
	require.True(t, ok)
	lifecycle := feedbackscreenshots.New(ts.db)
	receipt, err := lifecycle.Pin(context.Background(), userID, []string{url})
	require.NoError(t, err)
	require.NoError(t, lifecycle.ConfirmPublished(context.Background(), receipt, []string{url}))
	_, err = lifecycle.Offload(context.Background(), func(*string) (feedbackscreenshots.ObjectPutter, error) {
		return vendor, nil
	}, allowScreenshotOffload{})
	require.Error(t, err)
	assert.Empty(t, vendor.objects)
	assert.Empty(t, ts.store.objects, "a failed R2 offload must not fall back to MinIO")
	var rows int
	require.NoError(t, ts.db.QueryRow(`SELECT count(*) FROM media_files WHERE uploader_id = $1`, userID).Scan(&rows))
	assert.Equal(t, 1, rows, "staged bytes remain available for a later retry")
}

func TestFeedbackScreenshotUploadRecordsSeparateBackendAndFailsClosedWithoutReader(t *testing.T) {
	ts := setupMediaTest(t)
	userID := ts.createTestUser(t, "screenshotr2")
	vendor := newMockStore()
	ts.handler.SetWriteRouter(stubWriteRouter{tier1: ts.store, screenshot: vendor, screenshotID: "r2-feedback-useast"})
	body, ct := multipartBody(t, "file", "screenshot.png", makePNG(t, 32, 32), nil)
	w := ts.doMultipart(ts.handler.UploadFeedbackScreenshot, "POST", pathUploadFeedbackScreenshot, userID, body, ct)
	require.Equal(t, http.StatusCreated, w.Code)
	url, ok := parseBody(t, w)["url"].(string)
	require.True(t, ok)
	id := strings.TrimPrefix(url, feedbackScreenshotsPathPrefix)
	key := "feedback-screenshots/" + id
	var backend string
	require.NoError(t, ts.db.QueryRow(`SELECT storage_backend FROM media_files WHERE storage_key = $1`, key).Scan(&backend))
	assert.Equal(t, "r2-feedback-useast", backend)
	require.NoError(t, ts.db.QueryRow(`SELECT storage_backend FROM feedback_screenshot_uploads WHERE object_key = $1`, id).Scan(&backend))
	assert.Equal(t, "r2-feedback-useast", backend, "deferred writes keep the upload's selected backend")
	assert.Empty(t, ts.store.objects)
	assert.Empty(t, vendor.objects, "pending screenshots remain staged in PostgreSQL")
	proxy := func() *httptest.ResponseRecorder {
		return ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, "GET", url, gin.Params{{Key: "id", Value: id}})
	}
	assert.Equal(t, http.StatusOK, proxy().Code, "staged bytes are readable before offload")
	lifecycle := feedbackscreenshots.New(ts.db)
	receipt, err := lifecycle.Pin(context.Background(), userID, []string{url})
	require.NoError(t, err)
	require.NoError(t, lifecycle.ConfirmPublished(context.Background(), receipt, []string{url}))
	_, err = lifecycle.Offload(context.Background(), func(selected *string) (feedbackscreenshots.ObjectPutter, error) {
		require.NotNil(t, selected)
		assert.Equal(t, "r2-feedback-useast", *selected)
		return vendor, nil
	}, allowScreenshotOffload{})
	require.NoError(t, err)
	assert.Empty(t, ts.store.objects, "offload must not write to MinIO")
	assert.NotEmpty(t, vendor.objects)
	unavailable := proxy()
	assert.Equal(t, http.StatusServiceUnavailable, unavailable.Code, "an unwired backend must not fall back to MinIO")
	assert.Equal(t, cacheControlNoStore, unavailable.Header().Get(headerCacheControl))
	ts.handler.SetStoreResolver(stubDeleteStoreResolver{store: vendor})
	assert.Equal(t, http.StatusOK, proxy().Code)
}

func TestProxyFeedbackScreenshotInvalidID(t *testing.T) {
	ts := setupMediaTest(t)

	w := ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, "GET", feedbackScreenshotsPathPrefix+valueNotUUID, gin.Params{{Key: "id", Value: valueNotUUID}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestProxyFeedbackScreenshotRejectsNonCanonicalID locks in the ProxyFeedbackScreenshot
// half of Fix 1 (#1747 review): uuid.Parse alone accepts non-canonical forms
// (braces, urn:uuid:, uppercase, ...), so a :id in one of those shapes must be
// rejected with 400 rather than falling through to a 404 at storage.
func TestProxyFeedbackScreenshotRejectsNonCanonicalID(t *testing.T) {
	ts := setupMediaTest(t)
	braced := "{" + uuid.New().String() + "}"

	w := ts.doNoAuth(ts.handler.ProxyFeedbackScreenshot, "GET", feedbackScreenshotsPathPrefix+braced, gin.Params{{Key: "id", Value: braced}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
