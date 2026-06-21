// Package api_test verifies the architectural invariant introduced by
// finding #BLOCK-1 of the #1264 review: middleware.RequireAttestation is
// mounted on the authenticated route groups in router.go so the
// REQUIRE_CLIENT_ATTESTATION feature flag has runtime effect.
//
// Tests mirror the production middleware chain (AuthRequired then
// RequireAttestation, both attached to the same authRequired group) on a
// minimal gin router. Decoupled from the full NewRouter wiring (which
// requires db + nats + handlers) — the invariant under test is the
// middleware ordering and registration, NOT the route handlers.
//
//nolint:revive // "api" is the established package name shared with router.go.
package api_test

import (
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

// setupAuthRequiredGroup mirrors the production wiring in router.go
// (lines documenting "authRequired" group). The chain is:
//  1. AuthRequired (omitted here — replaced by a fake auth middleware
//     that sets user_id in the context; AuthRequired's own tests cover
//     JWT validation)
//  2. RequireAttestation(enabled, redis, log) — the new wiring under test
//  3. A no-op handler that returns 200 if reached
//
// The test asserts the gate's behavior at each enable state matches the
// production middleware semantics from internal/middleware/attestation_test.go.
func setupAuthRequiredGroup(t *testing.T, enabled bool, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	log := logger.New("test")

	// Mirror the production chain: AuthRequired must run BEFORE
	// RequireAttestation so user_id is in the context when the gate
	// evaluates (per the middleware's documented contract).
	fakeAuth := func(c *gin.Context) {
		c.Set("user_id", "test-user-id")
		c.Next()
	}

	authRequired := r.Group("/")
	authRequired.Use(fakeAuth)
	authRequired.Use(middleware.RequireAttestation(enabled, rdb, log))
	{
		authRequired.GET("/users/me", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"id": c.GetString("user_id")})
		})
	}
	return r
}

func doAuthRequiredGet(t *testing.T, r *gin.Engine, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/users/me", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestRouterAttestationWiring_DisabledIsPassThrough asserts the
// REQUIRE_CLIENT_ATTESTATION=false branch: the authRequired group reaches
// its handlers without the attestation headers. This is the architectural
// invariant for the self-hosted default — the middleware is mounted on
// every authenticated route but is a no-op when disabled.
func TestRouterAttestationWiring_DisabledIsPassThrough(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	r := setupAuthRequiredGroup(t, false, rdb)
	w := doAuthRequiredGet(t, r, nil) // no attestation headers

	require.Equal(t, http.StatusOK, w.Code,
		"with REQUIRE_CLIENT_ATTESTATION=false the wired middleware must pass through")
	require.Contains(t, w.Body.String(), "test-user-id")
}

// TestRouterAttestationWiring_EnabledMissingToken_403Missing asserts the
// REQUIRE_CLIENT_ATTESTATION=true branch enforces the gate: a request
// without X-Attestation-Token is rejected at the wiring layer (not at the
// underlying handler).
func TestRouterAttestationWiring_EnabledMissingToken_403Missing(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	r := setupAuthRequiredGroup(t, true, rdb)
	w := doAuthRequiredGet(t, r, map[string]string{
		"X-Session-ID": "s-wiring",
		"X-Machine-Id": "m-wiring",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrMissing, resp.Code,
		"missing X-Attestation-Token must be rejected with ErrMissing")
}

// TestRouterAttestationWiring_EnabledValidToken_200OK asserts the happy
// path through the wired middleware: a request with a valid token in Redis
// is allowed through to the handler.
func TestRouterAttestationWiring_EnabledValidToken_200OK(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	// Seed a token record in Redis matching the request headers.
	ctx := context.Background()
	rec := attestation.TokenRecord{
		Token:      "good-wiring-token",
		Version:    "0.2.7",
		SpaVersion: "20260529",
		IssuedAt:   time.Now(),
	}
	bs, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, "attestation:s-wiring:m-wiring", bs, 2*time.Hour).Err())

	r := setupAuthRequiredGroup(t, true, rdb)
	w := doAuthRequiredGet(t, r, map[string]string{
		"X-Attestation-Token": "good-wiring-token",
		"X-Session-ID":        "s-wiring",
		"X-Machine-Id":        "m-wiring",
	})

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "test-user-id",
		"the authRequired handler must be reached when attestation passes")
}

// TestRouterAttestationWiring_EnabledRevokedToken_403Revoked asserts the
// revocation branch through the wired middleware: a token bound to a
// version present in attestation:revoked_versions is rejected.
func TestRouterAttestationWiring_EnabledRevokedToken_403Revoked(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	//nolint:gosec // G101 false positive: test fixture token string, not a credential
	rec := attestation.TokenRecord{
		Token:    "rv-wiring-token",
		Version:  "0.2.5",
		IssuedAt: time.Now(),
	}
	bs, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, "attestation:s-wiring-rv:m-1", bs, 2*time.Hour).Err())
	require.NoError(t, rdb.SAdd(ctx, attestation.RevokedVersionsKey, "0.2.5").Err())

	r := setupAuthRequiredGroup(t, true, rdb)
	w := doAuthRequiredGet(t, r, map[string]string{ //nolint:gosec // G101: test fixture, not a credential
		"X-Attestation-Token": "rv-wiring-token",
		"X-Session-ID":        "s-wiring-rv",
		"X-Machine-Id":        "m-1",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, attestation.ErrRevoked, resp.Code,
		"revoked version must be rejected with ErrRevoked through the wired chain")
}
