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

// fixedClock returns a controllable clock for deterministic TTL/expiry tests.
// The returned func reads *t at call time, so advancing the session's clock is
// a simple pointer write between operations.
func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestSessionStore_MintThenGet(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	adminID := uniqueAdminUsername("admin-id")
	sid, err := store.Mint(ctx, adminID)
	require.NoError(t, err)
	require.NotEmpty(t, sid)
	// 256-bit sid -> 64 hex chars.
	assert.Len(t, sid, 64)

	sess, err := store.Get(ctx, sid)
	require.NoError(t, err)
	assert.Equal(t, adminID, sess.AdminID)
}

func TestSessionStore_MintIsRandom(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	sid1, err := store.Mint(ctx, "a")
	require.NoError(t, err)
	sid2, err := store.Mint(ctx, "a")
	require.NoError(t, err)
	assert.NotEqual(t, sid1, sid2, "session ids must be unpredictable/unique")
}

func TestSessionStore_GetRejectsUnknown(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	_, err := store.Get(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.Error(t, err)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)

	_, err = store.Get(ctx, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)
}

func TestSessionStore_IdleExpiry(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	sid, err := store.Mint(ctx, "admin-id")
	require.NoError(t, err)

	// 29 minutes later: still inside the 30m idle window.
	now = now.Add(29 * time.Minute)
	_, err = store.Get(ctx, sid)
	require.NoError(t, err, "session within idle window must be valid")

	// 31 minutes after that Get (which slid the window): the prior Get refreshed
	// last_seen to t+29m, so idle expiry is now at t+59m. Advance past it.
	now = now.Add(31 * time.Minute)
	_, err = store.Get(ctx, sid)
	require.Error(t, err, "session past the idle window must be rejected")
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)
}

func TestSessionStore_IdleSlides(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	sid, err := store.Mint(ctx, "admin-id")
	require.NoError(t, err)

	// Touch every 20 minutes for 2 hours: each Get slides the idle window, so the
	// session survives well past a single 30m idle window (but under the 8h cap).
	for i := 0; i < 6; i++ {
		now = now.Add(20 * time.Minute)
		_, err = store.Get(ctx, sid)
		require.NoError(t, err, "sliding idle window should keep an actively-used session alive")
	}
}

func TestSessionStore_AbsoluteCap(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	sid, err := store.Mint(ctx, "admin-id")
	require.NoError(t, err)

	// Keep the session active (Get every 20m) but push past the 8h absolute cap.
	// The absolute expiry must reject even an actively-used session.
	expired := false
	for i := 0; i < 30; i++ { // 30 * 20m = 10h > 8h cap
		now = now.Add(20 * time.Minute)
		if _, err := store.Get(ctx, sid); err != nil {
			assert.ErrorIs(t, err, admin.ErrSessionInvalid)
			expired = true
			break
		}
	}
	assert.True(t, expired, "session must be rejected once the 8h absolute cap is exceeded")
}

func TestSessionStore_Rotate(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	adminID := uniqueAdminUsername("admin-id")
	oldSID, err := store.Mint(ctx, adminID)
	require.NoError(t, err)

	newSID, err := store.Rotate(ctx, oldSID)
	require.NoError(t, err)
	assert.NotEqual(t, oldSID, newSID)

	// Old sid is gone; new sid resolves to the same admin.
	_, err = store.Get(ctx, oldSID)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)

	sess, err := store.Get(ctx, newSID)
	require.NoError(t, err)
	assert.Equal(t, adminID, sess.AdminID)
}

func TestSessionStore_Revoke(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	store := admin.NewSessionStore(rdb, fixedClock(&now))

	sid, err := store.Mint(ctx, "admin-id")
	require.NoError(t, err)

	require.NoError(t, store.Revoke(ctx, sid))

	_, err = store.Get(ctx, sid)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)

	// Revoking an already-gone sid is a no-op (idempotent), not an error.
	require.NoError(t, store.Revoke(ctx, sid))
}

func TestSetAdminSessionCookie_HostPrefixAttributes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/webauthn", nil)

	admin.SetAdminSessionCookie(c, "abc123sid")

	setCookie := rec.Header().Get("Set-Cookie")
	require.NotEmpty(t, setCookie)

	assert.Contains(t, setCookie, "__Host-cv_admin_sid=abc123sid")
	assert.Contains(t, setCookie, "Path=/")
	assert.Contains(t, setCookie, "HttpOnly")
	assert.Contains(t, setCookie, "Secure")
	assert.Contains(t, setCookie, "SameSite=Strict")
	// __Host- prefix REQUIRES no Domain attribute.
	assert.NotContains(t, setCookie, "Domain=")
}

func TestClearAdminSessionCookie_Expires(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/logout", nil)

	admin.ClearAdminSessionCookie(c)

	setCookie := rec.Header().Get("Set-Cookie")
	require.NotEmpty(t, setCookie)

	assert.Contains(t, setCookie, "__Host-cv_admin_sid=")
	// A cleared cookie carries Max-Age=0 (or a past expiry) plus the same
	// __Host- invariants so the browser accepts the deletion.
	assert.Contains(t, setCookie, "Max-Age=0")
	assert.Contains(t, setCookie, "Path=/")
	assert.Contains(t, setCookie, "Secure")
	assert.NotContains(t, setCookie, "Domain=")
}
