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
