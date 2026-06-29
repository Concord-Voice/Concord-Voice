package admin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// pendingAdminWithToken creates a pending admin and mints a real enrollment token
// via the EnrollmentStore, returning the admin id, username, and plaintext token.
func pendingAdminWithToken(t *testing.T, db *sql.DB, rdb *redis.Client) (adminID, username, token string) {
	t.Helper()
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)
	enroll := admin.NewEnrollmentStore(rdb)

	hash, err := auth.HashPassword(adminTestPassword)
	require.NoError(t, err)

	username = uniqueAdminUsername("enroll")
	created, err := repo.CreatePending(ctx, username, hash)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)

	token, err = enroll.MintEnrollmentToken(ctx, created.ID)
	require.NoError(t, err)
	return created.ID, username, token
}

// adminEnrollEngine wires the enrollment + admin-create + page routes.
func adminEnrollEngine(t *testing.T, db *sql.DB, rdb *redis.Client) (*gin.Engine, *admin.Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/enroll/begin", h.EnrollBegin)
	r.POST("/admin/api/v1/enroll/finish", h.EnrollFinish)
	r.GET("/admin/enroll", h.EnrollPage)
	return r, h
}

func TestEnroll_HappyPath_RegistersKeyAndActivates(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()

	adminID, username, token := pendingAdminWithToken(t, db, rdb)
	sum := sha256.Sum256([]byte(token))
	tokenKey := "admin_enroll:" + hex.EncodeToString(sum[:])
	boundID, err := rdb.Get(context.Background(), tokenKey).Result()
	require.NoError(t, err)
	require.Equal(t, adminID, boundID, "test setup token must bind to pending admin")
	engine, _ := adminEnrollEngine(t, db, rdb)

	// Begin.
	beginRec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username,
		"password": adminTestPassword,
		"token":    token,
	})
	require.Equal(t, http.StatusOK, beginRec.Code, beginRec.Body.String())
	var beginResp struct {
		Handle    string                       `json:"handle"`
		PublicKey *protocol.CredentialCreation `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(beginRec.Body.Bytes(), &beginResp))
	require.NotEmpty(t, beginResp.Handle)
	require.NotNil(t, beginResp.PublicKey)

	// Sign the registration challenge with an allow-listed virtual authenticator.
	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	attBody := va.attestationResponse(t, testAdminRPID, beginResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)

	finishRec := postJSON(engine, "/admin/api/v1/enroll/finish", map[string]any{ // #nosec G101 -- "credential_name" is a key-name label, not a secret
		"handle":          beginResp.Handle,
		"attestation":     json.RawMessage(attBody),
		"credential_name": "yubikey-primary",
	})
	require.Equal(t, http.StatusOK, finishRec.Code, finishRec.Body.String())

	// The admin is now active with one credential.
	repo := admin.NewAdminRepo(db)
	got, err := repo.GetByUsername(ctx, username)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusActive, got.Status)
	creds, err := repo.ListCredentials(ctx, adminID)
	require.NoError(t, err)
	assert.Len(t, creds, 1)
}

func TestEnroll_WrongPassword_Rejected(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, token := pendingAdminWithToken(t, db, rdb)
	engine, _ := adminEnrollEngine(t, db, rdb)

	rec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username,
		"password": "wrong-password",
		"token":    token,
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestEnroll_BadToken_Rejected(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := pendingAdminWithToken(t, db, rdb)
	engine, _ := adminEnrollEngine(t, db, rdb)

	rec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username,
		"password": adminTestPassword,
		"token":    "0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestEnroll_TokenIsSingleUse asserts a token consumed by begin cannot be reused.
func TestEnroll_TokenIsSingleUse(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, username, token := pendingAdminWithToken(t, db, rdb)
	sum := sha256.Sum256([]byte(token))
	tokenKey := "admin_enroll:" + hex.EncodeToString(sum[:])
	boundID, err := rdb.Get(context.Background(), tokenKey).Result()
	require.NoError(t, err)
	require.Equal(t, adminID, boundID, "test setup token must bind to pending admin")
	got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), username)
	require.NoError(t, err)
	ok, err := auth.VerifyPassword(adminTestPassword, got.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok, "test setup password must verify")
	engine, _ := adminEnrollEngine(t, db, rdb)

	first := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username,
		"password": adminTestPassword,
		"token":    token,
	})
	require.Equal(t, http.StatusOK, first.Code)

	second := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username,
		"password": adminTestPassword,
		"token":    token,
	})
	assert.Equal(t, http.StatusUnauthorized, second.Code, "a consumed token must not be reusable")
}

func TestEnrollPage_ServesFunctionalHTML(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	engine, _ := adminEnrollEngine(t, db, rdb)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/enroll", nil)
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	assert.Contains(t, body, "navigator.credentials.create")
	assert.Contains(t, body, "/admin/api/v1/enroll/begin")
	assert.Contains(t, body, "/admin/api/v1/enroll/finish")
	// Root-relative API URLs only (no SPA-origin-relative or external hosts).
	assert.NotContains(t, body, "http://")
}

// --- admin-create endpoint (behind AdminAuthRequired) ---

// adminCreateEngine wires CreateAdmin under a synthetic admin_id (simulating
// AdminAuthRequired having set it).
func adminCreateEngine(t *testing.T, db *sql.DB, rdb *redis.Client, actingAdminID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/admins", func(c *gin.Context) {
		c.Set("admin_id", actingAdminID)
		h.CreateAdmin(c)
	})
	return r
}

func TestCreateAdmin_ProvisionsPendingAndReturnsTokenOnce(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()

	// An acting admin must exist (FK target for the audit row).
	repo := admin.NewAdminRepo(db)
	acting, err := repo.CreatePending(ctx, uniqueAdminUsername("actor"), "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, acting.ID)

	engine := adminCreateEngine(t, db, rdb, acting.ID)

	newUsername := uniqueAdminUsername("new")
	rec := postJSON(engine, "/admin/api/v1/admins", map[string]string{
		"username": newUsername,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Username        string `json:"username"`
		Status          string `json:"status"`
		EnrollmentToken string `json:"enrollment_token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, newUsername, resp.Username)
	assert.Equal(t, string(admin.StatusPending), resp.Status)
	require.NotEmpty(t, resp.EnrollmentToken, "the one-time enrollment token must be returned once")

	// The created admin exists and is pending; clean it up.
	created, err := repo.GetByUsername(ctx, newUsername)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
	assert.Equal(t, admin.StatusPending, created.Status)

	// The returned token actually consumes against the new admin id.
	enroll := admin.NewEnrollmentStore(rdb)
	boundID, err := enroll.ConsumeEnrollmentToken(ctx, resp.EnrollmentToken)
	require.NoError(t, err)
	assert.Equal(t, created.ID, boundID)
}

func TestCreateAdmin_RejectsWeakPassword(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	engine := adminCreateEngine(t, db, rdb, "00000000-0000-0000-0000-000000000000")

	rec := postJSON(engine, "/admin/api/v1/admins", map[string]string{
		"username": uniqueAdminUsername("weak"),
		"password": "short",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAdmin_RejectsDuplicateUsername(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()

	repo := admin.NewAdminRepo(db)
	acting, err := repo.CreatePending(ctx, uniqueAdminUsername("actor2"), "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, acting.ID)

	existing, err := repo.CreatePending(ctx, uniqueAdminUsername("taken"), "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, existing.ID)

	engine := adminCreateEngine(t, db, rdb, acting.ID)
	rec := postJSON(engine, "/admin/api/v1/admins", map[string]string{
		"username": existing.Username,
		"password": adminTestPassword,
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}
