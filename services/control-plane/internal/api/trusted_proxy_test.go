// Package api_test (black-box) is used here because these tests exercise only
// the public Gin API surface and require no access to unexported symbols.
// Compare: privacy_wiring_test.go uses package api (white-box) to call
// unexported wiring helpers directly.
package api_test

import (
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain sets gin to test mode once for this test binary rather than
// per-call inside newEchoRouter. Because this file uses package api_test,
// it compiles into a separate test binary from privacy_wiring_test.go's
// package api, so this TestMain does not affect that file's setup.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// newEchoRouter builds a minimal Gin engine configured with the given
// trusted-proxy CIDRs and a single /echo endpoint that returns c.ClientIP()
// in the response body. Used to verify trust-boundary behavior without
// requiring the full NewRouter() dependency graph.
func newEchoRouter(t *testing.T, trustedCIDRs []string) *gin.Engine {
	t.Helper()
	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(trustedCIDRs))
	r.GET("/echo", func(c *gin.Context) {
		c.String(200, c.ClientIP())
	})
	return r
}

func TestClientIP_TrustedProxy_XForwardedFor(t *testing.T) {
	r := newEchoRouter(t, []string{"172.16.0.0/12"})

	req := httptest.NewRequest("GET", "/echo", nil)
	req.RemoteAddr = "172.19.0.5:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "1.2.3.4", w.Body.String(),
		"trusted peer should cause c.ClientIP() to honor X-Forwarded-For")
}

func TestClientIP_TrustedProxy_XRealIP(t *testing.T) {
	r := newEchoRouter(t, []string{"172.16.0.0/12"})

	req := httptest.NewRequest("GET", "/echo", nil)
	req.RemoteAddr = "172.19.0.5:51234"
	req.Header.Set("X-Real-IP", "1.2.3.4")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "1.2.3.4", w.Body.String(),
		"trusted peer should fall back to X-Real-IP when no XFF present")
}

func TestClientIP_TrustedProxy_XFFChain(t *testing.T) {
	// Simulate: real client → trusted inner proxy (10.0.0.1) → nginx (172.19.0.5) → app
	// nginx uses $proxy_add_x_forwarded_for so XFF chain has the inner proxy IP.
	// With both 172.16.0.0/12 and 10.0.0.0/8 trusted, Gin should walk past both
	// and return the leftmost untrusted address (the real client 1.2.3.4).
	r := newEchoRouter(t, []string{"172.16.0.0/12", "10.0.0.0/8"})

	req := httptest.NewRequest("GET", "/echo", nil)
	req.RemoteAddr = "172.19.0.5:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "1.2.3.4", w.Body.String(),
		"Gin should walk XFF right-to-left past all trusted hops and return leftmost untrusted")
}

func TestClientIP_UntrustedPeer_XFFIgnored(t *testing.T) {
	// Request arrives from an untrusted peer (public IP bypassing nginx).
	// X-Forwarded-For MUST be ignored — Gin should return the real peer address.
	r := newEchoRouter(t, []string{"172.16.0.0/12"})

	req := httptest.NewRequest("GET", "/echo", nil)
	req.RemoteAddr = "8.8.8.8:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "8.8.8.8", w.Body.String(),
		"untrusted peer must NOT have its X-Forwarded-For honored — spoof rejection")
}

func TestClientIP_SpoofAttempt_XRealIPIgnored(t *testing.T) {
	// Attacker tries to spoof via X-Real-IP from an untrusted peer.
	// Should be ignored — real peer returned.
	r := newEchoRouter(t, []string{"172.16.0.0/12"})

	req := httptest.NewRequest("GET", "/echo", nil)
	req.RemoteAddr = "198.51.100.1:51234"
	req.Header.Set("X-Real-IP", "1.2.3.4")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "198.51.100.1", w.Body.String(),
		"X-Real-IP from an untrusted peer must be ignored")
}

func TestClientIP_NoTrustedProxies_ReturnsPeer(t *testing.T) {
	// Regression test: with an empty trusted-proxy list (the current broken
	// state), c.ClientIP() returns the direct peer address — confirming that
	// SetTrustedProxies must be set correctly for the fix to take effect.
	r := newEchoRouter(t, []string{})

	req := httptest.NewRequest("GET", "/echo", nil)
	req.RemoteAddr = "172.19.0.5:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "172.19.0.5", w.Body.String(),
		"empty trusted-proxy list returns peer address — the pre-fix behavior")
}
