package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

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
	}

	wireAdminRoutes(router, nil, nil, cfg, logger.New("test"))

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
	assert.Contains(t, adminRoutes, "GET /admin/enroll")
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

	wireAdminRoutes(router, nil, rdb, cfg, logger.New("test"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/enroll", nil)
	req.Host = "admin-codename.concordvoice.chat"
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Body.String(), "known host without CF Access must not reach enroll HTML")
}

// TestWireAdminRoutes_DisabledMountsNothing is the dormant-by-default property
// (#1688): with ADMIN_CONSOLE_ENABLED=false (the default), wireAdminRoutes mounts
// NO /admin routes — so the unexposed auth backend presents no public endpoints
// until #1691/#1692 enable the console.
func TestWireAdminRoutes_DisabledMountsNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	cfg := &config.Config{AdminConsoleEnabled: false}

	wireAdminRoutes(router, nil, nil, cfg, logger.New("test"))

	for _, r := range router.Routes() {
		assert.False(t, strings.HasPrefix(r.Path, "/admin"),
			"disabled console must not register %s %s", r.Method, r.Path)
	}
}
