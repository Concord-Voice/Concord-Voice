package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterClientVersionWiring(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") //nolint:gosec // G304: fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)

	authIndex := strings.Index(source, "authRequired.Use(middleware.AuthRequired")
	versionIndex := strings.Index(source, "authRequired.Use(middleware.RequireClientVersion(cfg.ClientMinVersion))")
	attestationIndex := strings.Index(source, "authRequired.Use(middleware.RequireAttestation")
	require.NotEqual(t, -1, authIndex)
	require.NotEqual(t, -1, versionIndex)
	require.NotEqual(t, -1, attestationIndex)
	assert.Less(t, authIndex, versionIndex)
	assert.Less(t, versionIndex, attestationIndex)

	for _, route := range []string{
		"authRequired.GET(\"/users/me\"",
		"protected.POST(\"/auth/ws-ticket\"",
		"mediaRoutes := protected.Group(\"/media\")",
	} {
		assert.Contains(t, source, route)
	}

	for _, route := range []string{
		"v1.GET(\"/client/config\"",
		"v1.POST(\"/attestation/verify\"",
	} {
		routeIndex := strings.Index(source, route)
		require.GreaterOrEqual(t, routeIndex, 0, "public route %q is missing", route)
		assert.Lessf(t, routeIndex, authIndex, "public route %q must precede auth middleware", route)
	}
	assert.Contains(t, source, "middleware.AuthRequired(cfg.JWTSecret, redis, credFence),\n\t\t\tfunc(c *gin.Context) { attestationHandler.Verify")
}

// TestRouterClientVersionWiring_Behavior mirrors the production group layout
// without constructing NewRouter's database, NATS, and handler dependencies.
// The fake auth middleware stands in for AuthRequired; its own JWT behavior is
// covered by middleware tests. What matters here is which routes receive the
// client-version gate.
func TestRouterClientVersionWiring_Behavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")

	// Public routes are registered before the authenticated group.
	v1.GET("/client/config", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Attestation verification has its own authentication and is intentionally
	// not behind the client-version floor.
	fakeAuth := func(c *gin.Context) {
		c.Set("user_id", "test-user-id")
		c.Next()
	}
	v1.POST("/attestation/verify", fakeAuth, func(c *gin.Context) { c.Status(http.StatusOK) })

	authRequired := v1.Group("/")
	authRequired.Use(fakeAuth)
	authRequired.Use(middleware.RequireClientVersion("0.2.44"))
	authRequired.GET("/users/me", func(c *gin.Context) { c.Status(http.StatusOK) })

	request := func(method, path, version string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if version != "" {
			req.Header.Set(middleware.ClientVersionHeader, version)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("protected route rejects missing version", func(t *testing.T) {
		w := request(http.MethodGet, "/api/v1/users/me", "")
		require.Equal(t, http.StatusForbidden, w.Code)
		var response attestation.ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		assert.Equal(t, attestation.ErrVersionTooOld, response.Code)
	})

	t.Run("protected route reaches handler with valid version", func(t *testing.T) {
		w := request(http.MethodGet, "/api/v1/users/me", "0.2.44")
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("public route remains reachable without version", func(t *testing.T) {
		w := request(http.MethodGet, "/api/v1/client/config", "")
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("attestation verify remains reachable without version", func(t *testing.T) {
		w := request(http.MethodPost, "/api/v1/attestation/verify", "")
		assert.Equal(t, http.StatusOK, w.Code)
	})
}
