package attestation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const revokedVersionsKey = "attestation:revoked_versions"

func newRevokeHandler(t *testing.T) (*attestation.Handler, *attestation.Repository, func()) {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, rdb, logger.New("development"))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, rdb, logger.New("development"))
	cleanup := func() {
		dbCleanup()
		redisCleanup()
	}
	return h, repo, cleanup
}

func toGinRevokeRequest(t *testing.T, body string, adminUser string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/attestation/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if adminUser != "" {
		c.Set("admin_user", adminUser)
	}
	return c, w
}

func revokeBody(version, spaVersion, reason string) string {
	b, _ := json.Marshal(attestation.RevokePayload{Version: version, SpaVersion: spaVersion, Reason: reason})
	return string(b)
}

func seedBinary(t *testing.T, repo *attestation.Repository, version string) {
	t.Helper()
	err := repo.InsertBinary(context.Background(), attestation.PublishBinaryPayload{
		Version:  version,
		Platform: attestation.PlatformMacOS,
		CertHash: "sha256:" + version,
	}, "test-seed")
	require.NoError(t, err)
	err = repo.InsertSPA(context.Background(), attestation.PublishSPAPayload{
		SpaVersion: "spa-" + version,
		HTMLHash:   "sha256:html-" + version,
	}, "test-seed")
	require.NoError(t, err)
}

func TestRevokeHandler_Binary_HappyPath(t *testing.T) {
	h, repo, cleanup := newRevokeHandler(t)
	defer cleanup()

	seedBinary(t, repo, "0.2.5")

	body := revokeBody("0.2.5", "", "critical RCE in renderer")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// DB row marked revoked
	rb, err := repo.GetBinary(context.Background(), "0.2.5", attestation.PlatformMacOS)
	require.NoError(t, err)
	require.NotNil(t, rb)
	require.NotNil(t, rb.RevokedAt)
	require.Equal(t, "critical RCE in renderer", rb.RevokedReason)
}

func TestRevokeHandler_Binary_AddsToRedisRevokedSet(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	defer redisCleanup()

	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, rdb, logger.New("development"))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, rdb, logger.New("development"))

	seedBinary(t, repo, "0.2.6")
	body := revokeBody("0.2.6", "", "test reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)
	require.Equal(t, http.StatusOK, w.Code)

	// Redis SET contains the revoked version
	members, err := rdb.SMembers(context.Background(), revokedVersionsKey).Result()
	require.NoError(t, err)
	require.Contains(t, members, "0.2.6")
}

func TestRevokeHandler_MissingAdmin_401(t *testing.T) {
	h, _, cleanup := newRevokeHandler(t)
	defer cleanup()

	body := revokeBody("0.2.5", "", "reason")
	c, w := toGinRevokeRequest(t, body, "")
	h.Revoke(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRevokeHandler_NoVersionOrSpa_400(t *testing.T) {
	h, _, cleanup := newRevokeHandler(t)
	defer cleanup()

	body := revokeBody("", "", "reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeHandler_BothVersionAndSpa_400(t *testing.T) {
	h, _, cleanup := newRevokeHandler(t)
	defer cleanup()

	body := revokeBody("0.2.5", "a1b2c3d", "reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeHandler_NonexistentVersion_400(t *testing.T) {
	h, _, cleanup := newRevokeHandler(t)
	defer cleanup()

	body := revokeBody("9.9.9", "", "reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeHandler_SpaVersion_HappyPath(t *testing.T) {
	h, repo, cleanup := newRevokeHandler(t)
	defer cleanup()

	seedBinary(t, repo, "0.2.7")
	body := revokeBody("", "spa-0.2.7", "spa rolled back")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusOK, w.Code)
	rs, err := repo.GetSPA(context.Background(), "spa-0.2.7")
	require.NoError(t, err)
	require.NotNil(t, rs)
	require.NotNil(t, rs.RevokedAt)
}

// TestRevokeHandler_NonexistentSpaVersion_400 covers the ErrNotFound branch
// of the SPA axis (the binary-axis equivalent is TestRevokeHandler_NonexistentVersion_400).
func TestRevokeHandler_NonexistentSpaVersion_400(t *testing.T) {
	h, _, cleanup := newRevokeHandler(t)
	defer cleanup()

	body := revokeBody("", "spa-9999999", "reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestRevokeHandler_Binary_NilRedis_OK covers the rdb==nil early-return
// branch of revokeBinaryAxis. Without Redis, the Postgres revoke completes
// but the revoked_versions SET is not populated — acceptable in self-hosted
// mode where REQUIRE_CLIENT_ATTESTATION=false and the middleware is a
// pass-through.
func TestRevokeHandler_Binary_NilRedis_OK(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()

	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, nil, logger.New("development"))
	// Construct Handler with rdb=nil — the binary-axis Redis SADD must be skipped.
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, nil, logger.New("development"))

	seedBinary(t, repo, "0.2.8")
	body := revokeBody("0.2.8", "", "test reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusOK, w.Code, "nil rdb on binary revoke must still 200 OK (Postgres updated; cache unaffected)")
}

// TestRevokeHandler_Binary_RedisDown_500 covers the Redis SADD failure
// branch. The Postgres state is correctly marked revoked but the cache
// gate hasn't been populated, so we surface 500 so the operator can retry.
func TestRevokeHandler_Binary_RedisDown_500(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()

	// Pointing at a non-existent server so SADD fails.
	brokenRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = brokenRedis.Close() }()

	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, brokenRedis, logger.New("development"))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, brokenRedis, logger.New("development"))

	seedBinary(t, repo, "0.2.9")
	body := revokeBody("0.2.9", "", "test reason")
	c, w := toGinRevokeRequest(t, body, "admin@example.com")
	h.Revoke(c)

	require.Equal(t, http.StatusInternalServerError, w.Code,
		"Postgres updated but Redis SADD failed → 500 so operator retries")
}

// TestRevokeHandler_InvalidJSON_400 covers the c.ShouldBindJSON error path
// in parseRevokeRequest (malformed JSON body).
func TestRevokeHandler_InvalidJSON_400(t *testing.T) {
	h, _, cleanup := newRevokeHandler(t)
	defer cleanup()

	c, w := toGinRevokeRequest(t, "not-json", "admin@example.com")
	h.Revoke(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
}
