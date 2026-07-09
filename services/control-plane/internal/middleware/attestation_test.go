package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

func setupRouter(t *testing.T, enabled bool, rdb *redis.Client) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	called := new(bool)
	r.GET("/test",
		middleware.RequireAttestation(enabled, rdb, logger.New("development")),
		func(c *gin.Context) {
			*called = true
			c.JSON(http.StatusOK, gin.H{"ok": true})
		},
	)
	return r, called
}

// setupRouterWithLogBuf is the same as setupRouter but uses a logger backed by
// the supplied bytes.Buffer so tests can assert on emitted log lines.
func setupRouterWithLogBuf(t *testing.T, enabled bool, rdb *redis.Client, buf *bytes.Buffer) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	called := new(bool)
	r.GET("/test",
		middleware.RequireAttestation(enabled, rdb, logger.NewWithWriter(buf)),
		func(c *gin.Context) {
			*called = true
			c.JSON(http.StatusOK, gin.H{"ok": true})
		},
	)
	return r, called
}

func writeToken(t *testing.T, rdb *redis.Client, key, token, version string) {
	t.Helper()
	rec := attestation.TokenRecord{
		Token: token, Version: version, SpaVersion: "20260529",
		IssuedAt: time.Now(),
	}
	bs, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), key, bs, 2*time.Hour).Err())
}

func doRequest(t *testing.T, r *gin.Engine, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// === Disabled-mode pass-through ===

func TestRequireAttestation_Disabled_PassesThrough(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	r, called := setupRouter(t, false, rdb)
	w := doRequest(t, r, map[string]string{})

	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, *called)
}

// === MISSING token ===

func TestRequireAttestation_NoToken_403Missing(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Session-ID": "s-1",
		"X-Machine-Id": "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrMissing, resp.Code)
}

func TestRequireAttestation_MissingSession_403Invalid(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "t-abc",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrInvalid, resp.Code)
}

// === EXPIRED (no key in Redis) ===

func TestRequireAttestation_NoRedisKey_403Expired(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "t-abc",
		"X-Session-ID":        "s-never-existed",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrExpired, resp.Code)
}

// === INVALID (token mismatch) ===

func TestRequireAttestation_TokenMismatch_403Invalid(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeToken(t, rdb, "attestation:s-mismatch:m-1", "stored-token", "0.2.7")

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "different-token",
		"X-Session-ID":        "s-mismatch",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrInvalid, resp.Code)
}

// === HAPPY PATH ===

func TestRequireAttestation_ValidToken_NextCalled(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeToken(t, rdb, "attestation:s-happy:m-1", "good-token", "0.2.7")

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "good-token",
		"X-Session-ID":        "s-happy",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.True(t, *called)
}

// writeTokenWithUser writes a token record bound to a specific user (CV-CAN-013).
// UserID is stored as HashUserID(userID) to mirror the mint path, which never
// persists the raw identifier.
func writeTokenWithUser(t *testing.T, rdb *redis.Client, key, token, version, userID string) {
	t.Helper()
	rec := attestation.TokenRecord{
		Token: token, Version: version, SpaVersion: "20260529",
		IssuedAt: time.Now(), UserID: attestation.HashUserID(userID),
	}
	bs, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), key, bs, 2*time.Hour).Err())
}

// setupRouterWithUserLogBuf is setupRouterWithUser but backed by a logger
// writing to buf, so tests can assert on the emitted rejection log.
func setupRouterWithUserLogBuf(t *testing.T, rdb *redis.Client, userID string, buf *bytes.Buffer) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	called := new(bool)
	r.GET("/test",
		func(c *gin.Context) { c.Set("user_id", userID) },
		middleware.RequireAttestation(true, rdb, logger.NewWithWriter(buf)),
		func(c *gin.Context) { *called = true; c.JSON(http.StatusOK, gin.H{"ok": true}) },
	)
	return r, called
}

// setupRouterWithUser wires a route that sets an authenticated user_id before
// RequireAttestation, giving the middleware's ownership check a caller identity.
func setupRouterWithUser(t *testing.T, rdb *redis.Client, userID string) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	called := new(bool)
	r.GET("/test",
		func(c *gin.Context) { c.Set("user_id", userID) },
		middleware.RequireAttestation(true, rdb, logger.NewWithWriter(&bytes.Buffer{})),
		func(c *gin.Context) { *called = true; c.JSON(http.StatusOK, gin.H{"ok": true}) },
	)
	return r, called
}

// TestRequireAttestation_TokenBoundToDifferentUser_403Invalid covers CV-CAN-013:
// a token minted for one user must not satisfy enforcement for another.
func TestRequireAttestation_TokenBoundToDifferentUser_403Invalid(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeTokenWithUser(t, rdb, "attestation:s-owner:m-1", "owner-token", "0.2.7", "user-a")

	r, called := setupRouterWithUser(t, rdb, "user-b") // different caller
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "owner-token",
		"X-Session-ID":        "s-owner",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrInvalid, resp.Code)
}

// TestRequireAttestation_TokenBoundToSameUser_NextCalled confirms the binding does
// not over-reject: the minting user still passes.
func TestRequireAttestation_TokenBoundToSameUser_NextCalled(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeTokenWithUser(t, rdb, "attestation:s-owner2:m-1", "owner-token2", "0.2.7", "user-a")

	r, called := setupRouterWithUser(t, rdb, "user-a") // matching caller
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "owner-token2",
		"X-Session-ID":        "s-owner2",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.True(t, *called)
}

// TestRequireAttestation_TokenBoundToDifferentUser_LogsVersion asserts the
// cross-user rejection (CV-CAN-013) is a post-load path that attributes the
// bound version in the structured attestation.rejected log, mirroring the
// revoked path (finding #24). Without it, CV-CAN-013 token-transfer rejections
// would land in the hourly counter with no release attribution.
func TestRequireAttestation_TokenBoundToDifferentUser_LogsVersion(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeTokenWithUser(t, rdb, "attestation:s-owner-log:m-1", "owner-log-token", "0.2.7", "user-a")

	var buf bytes.Buffer
	r, called := setupRouterWithUserLogBuf(t, rdb, "user-b", &buf) // different caller
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "owner-log-token",
		"X-Session-ID":        "s-owner-log",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)

	output := buf.String()
	require.Contains(t, output, "attestation.rejected",
		"cross-user path must emit attestation.rejected log")
	require.Contains(t, output, "version=0.2.7",
		"cross-user rejection log must include the bound version")
	require.Contains(t, output, "reason=ATTESTATION_INVALID")
}

func TestRequireAttestation_WebClient_NoMachineID_NextCalled(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeToken(t, rdb, "attestation:s-web:web", "web-token", "0.2.7")

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "web-token",
		"X-Session-ID":        "s-web",
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.True(t, *called)
}

// === REVOKED ===

func TestRequireAttestation_RevokedVersion_403Revoked(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeToken(t, rdb, "attestation:s-revoked:m-1", "rv-token", "0.2.5")
	require.NoError(t, rdb.SAdd(context.Background(), "attestation:revoked_versions", "0.2.5").Err())

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "rv-token",
		"X-Session-ID":        "s-revoked",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrRevoked, resp.Code)
}

// TestRequireAttestation_RevokedVersion_LogsVersion asserts that the structured
// rejection log on the ErrRevoked path includes the bound version (finding #24
// of #1264 review). Without this attribution the hourly counter loses
// per-version detail.
func TestRequireAttestation_RevokedVersion_LogsVersion(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	writeToken(t, rdb, "attestation:s-revoked-log:m-1", "rv-log", "0.2.5") // #nosec G101 -- test token, not a real credential
	require.NoError(t, rdb.SAdd(context.Background(), "attestation:revoked_versions", "0.2.5").Err())

	var buf bytes.Buffer
	r, called := setupRouterWithLogBuf(t, true, rdb, &buf)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "rv-log", // #nosec G101 -- test token, not a real credential
		"X-Session-ID":        "s-revoked-log",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)

	output := buf.String()
	require.Contains(t, output, "attestation.rejected",
		"middleware revoked-path must emit attestation.rejected log")
	require.Contains(t, output, "version=0.2.5",
		"revoked-path log must include bound version (finding #24)")
	require.Contains(t, output, "reason=ATTESTATION_REVOKED")
}

// === FAIL-CLOSED ===

func TestRequireAttestation_RedisDown_503FailClosed(t *testing.T) {
	// Construct a redis client pointed at a non-existent server so every op fails.
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		ReadTimeout: 100 * time.Millisecond,
	})
	defer func() { _ = rdb.Close() }()

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "any-token",
		"X-Session-ID":        "s-down",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.False(t, *called)
}

// === CORRUPT JSON ===

func TestRequireAttestation_CorruptStoredRecord_403Invalid(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	require.NoError(t, rdb.Set(context.Background(), "attestation:s-corrupt:m-1", "not-json", time.Hour).Err())

	r, called := setupRouter(t, true, rdb)
	w := doRequest(t, r, map[string]string{
		"X-Attestation-Token": "any",
		"X-Session-ID":        "s-corrupt",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, *called)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrInvalid, resp.Code)
}
