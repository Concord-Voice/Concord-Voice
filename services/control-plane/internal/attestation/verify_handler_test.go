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

const testTokenTTL = 2 * time.Hour

func newVerifyHandler(t *testing.T) (*attestation.Handler, *attestation.Repository, *attestation.Cache, func()) {
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
	return h, repo, cache, cleanup
}

func seedRegistry(t *testing.T, repo *attestation.Repository, version, certHash, spaVersion, htmlHash string) {
	t.Helper()
	err := repo.InsertBinary(context.Background(), attestation.PublishBinaryPayload{
		Version:  version,
		Platform: attestation.PlatformMacOS,
		CertHash: certHash,
	}, "test-seed")
	require.NoError(t, err)
	err = repo.InsertSPA(context.Background(), attestation.PublishSPAPayload{
		SpaVersion: spaVersion,
		HTMLHash:   htmlHash,
	}, "test-seed")
	require.NoError(t, err)
}

func toGinVerifyRequest(t *testing.T, body string, userID, sessionID, machineID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/attestation/verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("X-Session-ID", sessionID)
	}
	if machineID != "" {
		req.Header.Set("X-Machine-Id", machineID)
	}
	c.Request = req
	if userID != "" {
		c.Set("user_id", userID)
	}
	return c, w
}

func verifyBody(version, platform, certHash, machineID, spaVersion, spaHash string) string {
	b, _ := json.Marshal(attestation.VerifyPayload{
		Version: version, Platform: attestation.Platform(platform),
		CertHash: certHash, MachineID: machineID,
		SpaVersion: spaVersion, SpaHash: spaHash,
	})
	return string(b)
}

func TestVerifyHandler_Desktop_HappyPath(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-123", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp attestation.VerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AttestationToken)
	require.Equal(t, int(testTokenTTL.Seconds()), resp.TTLSeconds)
}

func TestVerifyHandler_Linux_TwoSignal_HappyPath(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	// Linux: cert_hash absent; we still send machine_id + spa_hash.
	body := verifyBody("0.2.7", "linux", "", "m-lin", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-456", "m-lin")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestVerifyHandler_Web_SignalThreeOnly_HappyPath(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "web", "", "", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-web", "")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestVerifyHandler_WrongSpaHash_Reject(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	// Client supplies a SPA hash that does not match the registered html_hash → mismatch.
	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:wronghtml")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-mix", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrUnknownRelease, resp.Code)
	require.True(t, resp.UpdateAvailable)
}

func TestVerifyHandler_RejectUnknownVersion_403(t *testing.T) {
	h, _, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("9.9.9", "macos", "sha256:x", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-unk", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp attestation.ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	require.Equal(t, attestation.ErrUnknownRelease, resp.Code)
}

func TestVerifyHandler_RejectBadCertHash_403(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "macos", "sha256:wrong", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-bad", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestVerifyHandler_RejectMissingMachineID_Desktop_403(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-nomac", "")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp attestation.ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	require.Equal(t, attestation.ErrInvalid, resp.Code)
}

func TestVerifyHandler_RejectMissingSession_400(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestVerifyHandler_RejectMissingUser_401(t *testing.T) {
	h, repo, cache, cleanup := newVerifyHandler(t)
	defer cleanup()

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "", "s-x", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestVerifyHandler_StoresTokenInRedis(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	defer redisCleanup()

	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, rdb, logger.New("development"))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, rdb, logger.New("development"))

	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	require.NoError(t, cache.Hydrate(context.Background()))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-store", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-store", "m-store")
	h.Verify(c, testTokenTTL)
	require.Equal(t, http.StatusOK, w.Code)

	// Redis key exists with expected shape.
	raw, err := rdb.Get(context.Background(), "attestation:s-store:m-store").Result()
	require.NoError(t, err)
	var rec attestation.TokenRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &rec))
	require.Equal(t, "0.2.7", rec.Version)
	require.Equal(t, "a1b2c3d", rec.SpaVersion)
	require.NotEmpty(t, rec.Token)
}

// TestVerifyHandler_MalformedJSON_400 covers the c.ShouldBindJSON error
// branch in parseVerifyRequest.
func TestVerifyHandler_MalformedJSON_400(t *testing.T) {
	h, _, _, cleanup := newVerifyHandler(t)
	defer cleanup()

	c, w := toGinVerifyRequest(t, "not-json", "user-1", "s-x", "m-abc")
	h.Verify(c, testTokenTTL)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestVerifyHandler_InvalidPlatform_400 covers the Platform.Valid() failure
// branch in parseVerifyRequest. An unrecognized platform string is rejected
// before any signal checks run.
func TestVerifyHandler_InvalidPlatform_400(t *testing.T) {
	h, _, _, cleanup := newVerifyHandler(t)
	defer cleanup()

	body := verifyBody("0.2.7", "haiku-os", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-bad-plat", "m-abc")
	h.Verify(c, testTokenTTL)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestVerifyHandler_RevokedVersion_403 covers the IsRevoked=true branch in
// checkVerifySignals. The version is in the cache and matches the signal
// payload, but is also in the Redis revoked_versions SET — verify must
// reject with ErrRevoked.
func TestVerifyHandler_RevokedVersion_403(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	defer redisCleanup()

	repo := attestation.NewRepository(db)
	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	cache := attestation.NewCache(repo, nil, rdb, logger.New("development"))
	require.NoError(t, cache.Hydrate(context.Background()))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, rdb, logger.New("development"))

	// Mark the version as revoked in the SET so IsRevoked returns true.
	require.NoError(t, rdb.SAdd(context.Background(), "attestation:revoked_versions", "0.2.7").Err())

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-revoke", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrRevoked, resp.Code)
}

// TestVerifyHandler_NilRedis_503 covers the rdb==nil failure path in
// issueToken. Self-hosted mode without Redis is misconfiguration when
// REQUIRE_CLIENT_ATTESTATION=true; verify fails loudly.
func TestVerifyHandler_NilRedis_503(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()
	repo := attestation.NewRepository(db)
	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")
	// Construct Handler with rdb=nil — issueToken's nil-rdb branch fires.
	cache := attestation.NewCache(repo, nil, nil, logger.New("development"))
	require.NoError(t, cache.Hydrate(context.Background()))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, nil, logger.New("development"))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-nilredis", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"nil Redis client means verify cannot persist the token — fail-closed with 503")
}

// TestVerifyHandler_RedisDown_503 covers the rdb.Set error path in
// issueToken. The cache is hydrated (in-memory) so signal checks pass,
// but the broken Redis fails the token persistence step.
func TestVerifyHandler_RedisDown_503(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	defer dbCleanup()
	repo := attestation.NewRepository(db)
	seedRegistry(t, repo, "0.2.7", "sha256:bin", "a1b2c3d", "sha256:html")

	// Use a broken Redis client for both the cache (so IsRevoked fails-closed and
	// rejects) AND for token persistence — but we need the cache to NOT short-
	// circuit on IsRevoked. So we use an in-memory revocation-free cache by
	// constructing it with rdb=nil for the cache (IsRevoked → false), then use
	// the broken Redis on the Handler for the issueToken Set error path.
	cache := attestation.NewCache(repo, nil, nil, logger.New("development"))
	require.NoError(t, cache.Hydrate(context.Background()))

	brokenRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = brokenRedis.Close() }()

	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, brokenRedis, logger.New("development"))

	body := verifyBody("0.2.7", "macos", "sha256:bin", "m-abc", "a1b2c3d", "sha256:html")
	c, w := toGinVerifyRequest(t, body, "user-1", "s-redisdown", "m-abc")
	h.Verify(c, testTokenTTL)

	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"Redis Set error → 503 fail-closed per ADR-0010 D2")
}
