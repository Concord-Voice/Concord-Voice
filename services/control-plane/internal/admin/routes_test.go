package admin_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

type emptyAdminMetricsReader struct{}

func (emptyAdminMetricsReader) Latest(context.Context, string, time.Time) ([]opsmetrics.Point, error) {
	return []opsmetrics.Point{}, nil
}

func (emptyAdminMetricsReader) Series(context.Context, string, opsmetrics.MetricKey, time.Time, time.Time) ([]opsmetrics.Bucket, error) {
	return []opsmetrics.Bucket{}, nil
}

// registerAdminEngine builds a real engine with the full admin route set mounted.
func registerAdminEngine(t *testing.T) *gin.Engine {
	t.Helper()
	return registerAdminRouteOnlyEngine(t)
}

func registerAdminEngineAndSessions(t *testing.T) (*gin.Engine, *admin.SessionStore) {
	return registerAdminEngineAndSessionsWithLimiter(t, nil)
}

func registerAdminEngineAndSessionsWithLimiter(t *testing.T, limiterRedis *redis.Client) (*gin.Engine, *admin.SessionStore) {
	return registerAdminEngineAndSessionsWithLimiterAndMiddleware(t, limiterRedis)
}

func registerAdminRouteOnlyEngine(t *testing.T, middlewares ...gin.HandlerFunc) *gin.Engine {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	gin.SetMode(gin.TestMode)
	log := logger.NewWithWriter(&bytes.Buffer{})
	h, err := admin.NewHandler(nil, rdb, log, authHandlerCfg())
	require.NoError(t, err)
	metricsHandler, err := opsmetrics.NewAdminHandler(nil, "", 0, log)
	require.NoError(t, err)

	engine := gin.New()
	engine.Use(middlewares...)
	admin.RegisterRoutes(&engine.RouterGroup, h, metricsHandler, rdb, admin.NewUI(adminUITestFS()))
	return engine
}

func registerAdminEngineAndSessionsWithLimiterAndMiddleware(
	t *testing.T,
	limiterRedis *redis.Client,
	middlewares ...gin.HandlerFunc,
) (*gin.Engine, *admin.SessionStore) {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	if limiterRedis == nil {
		limiterRedis = rdb
	}

	gin.SetMode(gin.TestMode)
	log := logger.NewWithWriter(&bytes.Buffer{})
	h, err := admin.NewHandler(db, rdb, log, authHandlerCfg())
	require.NoError(t, err)
	metricsHandler, err := opsmetrics.NewAdminHandler(
		emptyAdminMetricsReader{},
		"cvn_aaaaaaaaaaaaaaaa",
		15*time.Second,
		log,
	)
	require.NoError(t, err)

	engine := gin.New()
	engine.Use(middlewares...)
	admin.RegisterRoutes(&engine.RouterGroup, h, metricsHandler, limiterRedis, admin.NewUI(adminUITestFS()))
	return engine, admin.NewSessionStore(rdb, time.Now)
}

// TestAdminRoutes_DenyByDefault is the reflective gate test: it enumerates EVERY
// registered /admin route via router.Routes() and asserts that any route NOT on
// the explicit pre-auth allowlist rejects an UNAUTHENTICATED request with 401/403.
// This catches a future route accidentally mounted outside the AdminAuthRequired
// group (the deny-by-default invariant of #1688).
func TestAdminRoutes_DenyByDefault(t *testing.T) {
	engine := registerAdminEngine(t)

	routes := engine.Routes()

	var adminRoutes, gatedChecked int
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "/admin") {
			continue
		}
		adminRoutes++

		if admin.IsPreAuthRoute(r.Path) {
			// Pre-auth route — intentionally reachable without a session. Skip the
			// gate assertion (it is authenticated by password/token/key, not the
			// admin session cookie).
			continue
		}

		// Gated route: an unauthenticated request (no admin session cookie) MUST be
		// rejected with 401 or 403 — it must NOT reach the handler (which would be
		// 2xx/4xx-business-logic).
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(r.Method, r.Path, nil)
		engine.ServeHTTP(rec, req)

		assert.Containsf(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
			"gated route %s %s must reject an unauthenticated request (got %d)", r.Method, r.Path, rec.Code)
		gatedChecked++
	}

	require.NotZero(t, adminRoutes, "expected at least one /admin route to be registered")
	require.NotZero(t, gatedChecked, "expected at least one AdminAuthRequired-gated route to verify")
}

// TestAdminRoutes_PreAuthAllowlistMatchesRegistered asserts the pre-auth
// allowlist contains no stale entries: every allowlisted path is actually a
// registered route. A drifted allowlist (e.g. a renamed route still allowlisted)
// would silently exempt a non-existent path and could mask a real gated route.
func TestAdminRoutes_PreAuthAllowlistMatchesRegistered(t *testing.T) {
	engine := registerAdminEngine(t)

	registered := make(map[string]struct{})
	for _, r := range engine.Routes() {
		registered[r.Path] = struct{}{}
	}

	for _, p := range []string{
		"/admin/",
		"/admin/enroll",
		"/admin/assets/*filepath",
		"/admin/api/v1/auth/password",
		"/admin/api/v1/auth/webauthn",
		"/admin/api/v1/auth/logout",
		"/admin/api/v1/enroll/begin",
		"/admin/api/v1/enroll/finish",
	} {
		assert.True(t, admin.IsPreAuthRoute(p), "%s should be on the pre-auth allowlist", p)
		_, ok := registered[p]
		assert.Truef(t, ok, "pre-auth allowlisted path %s must be a registered route (allowlist drift)", p)
	}
}

func TestAdminRoutes_UIRoutesAreExplicitPreAuth(t *testing.T) {
	engine := registerAdminEngine(t)

	registered := make(map[string]string)
	for _, r := range engine.Routes() {
		registered[r.Path] = r.Method
	}

	for _, p := range []string{"/admin/", "/admin/enroll", "/admin/assets/*filepath"} {
		assert.Equal(t, http.MethodGet, registered[p], p)
		assert.True(t, admin.IsPreAuthRoute(p), p)
	}
}

func TestAdminRoutes_AssetCacheCanReplaceAPINoStore(t *testing.T) {
	engine := registerAdminRouteOnlyEngine(t, func(c *gin.Context) {
		c.Header("Cache-Control", "private, no-store")
		c.Next()
	})

	asset := httptest.NewRecorder()
	engine.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/admin/assets/app.abc123.js", nil))
	require.Equal(t, http.StatusOK, asset.Code)
	assert.Equal(t, "private, max-age=31536000, immutable", asset.Header().Get("Cache-Control"))

	api := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/password", strings.NewReader(`{`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(api, req)
	assert.Equal(t, "private, no-store", api.Header().Get("Cache-Control"))
}

// TestAdminRoutes_AdminsRouteIsGated is an explicit (non-reflective) confirmation
// that the admin-create route specifically requires authentication.
func TestAdminRoutes_AdminsRouteIsGated(t *testing.T) {
	engine := registerAdminEngine(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/admins", nil)
	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"POST /admin/api/v1/admins must require an admin session")
	assert.False(t, admin.IsPreAuthRoute("/admin/api/v1/admins"),
		"the admin-create route must NOT be on the pre-auth allowlist")
}
