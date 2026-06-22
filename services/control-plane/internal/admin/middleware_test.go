package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// adminAuthTestEngine builds a gin engine with a single AdminAuthRequired-gated
// route that records whether c.Next() was reached and what admin_id (if any) was
// set on the context, so the middleware's behavior is observable from the
// response.
func adminAuthTestEngine(sessions *admin.SessionStore) (*gin.Engine, *bool, *string) {
	gin.SetMode(gin.TestMode)
	reached := false
	gotAdminID := ""
	r := gin.New()
	r.GET("/admin/api/v1/protected", admin.AdminAuthRequired(sessions), func(c *gin.Context) {
		reached = true
		if v, ok := c.Get("admin_id"); ok {
			if s, ok := v.(string); ok {
				gotAdminID = s
			}
		}
		c.Status(http.StatusOK)
	})
	return r, &reached, &gotAdminID
}

func TestAdminAuthRequired_NoCookie_401(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sessions := admin.NewSessionStore(rdb, func() time.Time { return now })
	engine, reached, _ := adminAuthTestEngine(sessions)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/protected", nil)
	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, *reached, "handler must NOT run when no session cookie is present")
}

func TestAdminAuthRequired_InvalidSid_401(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sessions := admin.NewSessionStore(rdb, func() time.Time { return now })
	engine, reached, _ := adminAuthTestEngine(sessions)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/protected", nil)
	// HttpOnly/Secure are response-side flags; on an inbound request cookie they
	// are inert (the server reads only Name/Value). Set both to keep the Semgrep
	// sensitive-cookie rule quiet on this test fixture.
	req.AddCookie(&http.Cookie{
		Name:     "__Host-cv_admin_sid",
		Value:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, *reached, "handler must NOT run for an unknown/expired session id")
}

func TestAdminAuthRequired_ExpiredSid_401(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	clock := now
	sessions := admin.NewSessionStore(rdb, func() time.Time { return clock })
	engine, reached, _ := adminAuthTestEngine(sessions)

	sid, err := sessions.Mint(ctx, "admin-xyz")
	require.NoError(t, err)

	// Advance past the absolute cap so the session is dead.
	clock = clock.Add(9 * time.Hour)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/protected", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, *reached, "handler must NOT run for an expired session")
}

func TestAdminAuthRequired_ValidSid_SetsAdminIDAndCallsNext(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sessions := admin.NewSessionStore(rdb, func() time.Time { return now })
	engine, reached, gotAdminID := adminAuthTestEngine(sessions)

	sid, err := sessions.Mint(ctx, "admin-xyz")
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/protected", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, *reached, "handler must run for a valid session")
	assert.Equal(t, "admin-xyz", *gotAdminID, "admin_id must be set on the context")
}

// TestAdminAuthRequired_IgnoresAuthorizationHeader proves the admin middleware
// NEVER consults the user-facing Authorization header / JWT — only the opaque
// admin session cookie. A request carrying a Bearer token but no admin cookie is
// rejected.
func TestAdminAuthRequired_IgnoresAuthorizationHeader(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sessions := admin.NewSessionStore(rdb, func() time.Time { return now })
	engine, reached, _ := adminAuthTestEngine(sessions)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/protected", nil)
	req.Header.Set("Authorization", "Bearer some.user.jwt")
	engine.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, *reached, "a user JWT must not grant admin access")
}
