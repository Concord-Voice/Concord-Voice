package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

type adminWiringMetricsReader struct{}

func (adminWiringMetricsReader) Latest(context.Context, string, time.Time) ([]opsmetrics.Point, error) {
	return []opsmetrics.Point{}, nil
}

func (adminWiringMetricsReader) Series(context.Context, string, opsmetrics.MetricKey, time.Time, time.Time) ([]opsmetrics.Bucket, error) {
	return []opsmetrics.Bucket{}, nil
}

// TestWireAdminRoutes_MountsAdminSurface verifies the #1688 wiring builds the
// admin Handler and registers the /admin route set on the engine. admin.NewHandler
// only needs cfg (for the WebAuthn RP) + crypto/rand (for the dummy hash) at
// construction; the repo/session/lockout stores wrap db/rdb lazily, so nil db/rdb
// are acceptable for this construction-and-mount check (no request is served).
func TestWireAdminRoutes_MountsAdminSurface(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	cfg := &config.Config{
		AdminConsoleEnabled:         true,
		AdminWebAuthnRPID:           "admin.example.org",
		AdminWebAuthnRPOrigins:      []string{"https://admin.example.org"},
		AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"},
		OpsMetrics: config.OpsMetricsConfig{
			Enabled:  true,
			NodeID:   "cvn_aaaaaaaaaaaaaaaa",
			Interval: 15 * time.Second,
		},
	}

	wireAdminRoutes(router, nil, nil, adminWiringMetricsReader{}, cfg, logger.New("test"))

	var adminRoutes []string
	for _, r := range router.Routes() {
		if strings.HasPrefix(r.Path, "/admin") {
			adminRoutes = append(adminRoutes, r.Method+" "+r.Path)
		}
	}

	require.NotEmpty(t, adminRoutes, "wireAdminRoutes must register /admin routes")
	assert.Contains(t, adminRoutes, "POST /admin/api/v1/auth/password")
	assert.Contains(t, adminRoutes, "POST /admin/api/v1/auth/webauthn")
	assert.Contains(t, adminRoutes, "POST /admin/api/v1/admins")
	assert.Contains(t, adminRoutes, "GET /admin/")
	assert.Contains(t, adminRoutes, "GET /admin/enroll")
	assert.Contains(t, adminRoutes, "GET /admin/assets/*filepath")
	assert.Contains(t, adminRoutes, "GET /admin/api/v1/health")
	assert.Contains(t, adminRoutes, "GET /admin/api/v1/metrics/current")
	assert.Contains(t, adminRoutes, "GET /admin/api/v1/metrics/series")
	assert.Contains(t, adminRoutes, "GET /admin/api/v1/counters")
}

func TestWireAdminRoutes_KnownHostWithoutCFAccessIsRejectedBeforeAdminHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := &config.Config{
		AdminConsoleEnabled:         true,
		AdminWebAuthnRPID:           "admin-codename.concordvoice.chat",
		AdminWebAuthnRPOrigins:      []string{"https://admin-codename.concordvoice.chat"},
		AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"},
		CFAccessAUD:                 "test-access-aud",
		CFAccessTeamDomain:          "https://team.cloudflareaccess.com",
	}

	wireAdminRoutes(router, nil, rdb, nil, cfg, logger.New("test"))

	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{name: "index", method: http.MethodGet, target: "/admin/"},
		{name: "enroll", method: http.MethodGet, target: "/admin/enroll"},
		{name: "asset", method: http.MethodGet, target: "/admin/assets/app.abc123.js"},
		{name: "auth", method: http.MethodPost, target: "/admin/api/v1/auth/password"},
		{name: "metrics", method: http.MethodGet, target: "/admin/api/v1/health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Host = "admin-codename.concordvoice.chat"
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusForbidden, rec.Code)
			assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
			assert.Empty(t, rec.Body.String(), "known host without CF Access must not reach %s handler", tc.name)
		})
	}
}

func TestWireAdminRoutes_ValidCFAccessWithoutAdminSessionIsUnauthorized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, marshalErr := json.Marshal(map[string]any{"keys": []map[string]string{{
			"kid": "admin-wiring-key",
			"kty": "RSA",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
		require.NoError(t, marshalErr)
		_, _ = w.Write(body)
	}))
	t.Cleanup(jwks.Close)

	router := gin.New()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	cfg := &config.Config{
		AdminConsoleEnabled:         true,
		AdminWebAuthnRPID:           "admin.example.org",
		AdminWebAuthnRPOrigins:      []string{"https://admin.example.org"},
		AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"},
		CFAccessAUD:                 "test-access-aud",
		CFAccessTeamDomain:          jwks.URL,
	}
	wireAdminRoutes(router, nil, rdb, nil, cfg, logger.New("test"))

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"aud": "test-access-aud",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iss": jwks.URL,
	})
	token.Header["kid"] = "admin-wiring-key"
	signed, err := token.SignedString(key)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/health", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", signed)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Body.String())
}

// TestWireAdminRoutes_DisabledMountsNothing is the dormant-by-default property
// (#1688): with ADMIN_CONSOLE_ENABLED=false (the default), wireAdminRoutes mounts
// NO /admin routes — so the unexposed auth backend presents no public endpoints
// until #1691/#1692 enable the console.
func TestWireAdminRoutes_DisabledMountsNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	cfg := &config.Config{AdminConsoleEnabled: false}

	wireAdminRoutes(router, nil, nil, nil, cfg, logger.New("test"))

	for _, r := range router.Routes() {
		assert.False(t, strings.HasPrefix(r.Path, "/admin"),
			"disabled console must not register %s %s", r.Method, r.Path)
	}
}
