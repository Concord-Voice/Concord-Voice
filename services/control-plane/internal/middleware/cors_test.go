package middleware_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

const (
	testOrigin                     = "https://concordvoice.chat"
	testAppOrigin                  = "app://concord"
	headerAccessControlAllowOrigin = "Access-Control-Allow-Origin"
	headerVary                     = "Vary"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func corsRouter(origins []string) *gin.Engine {
	r := gin.New()
	r.Use(middleware.CORS(origins))
	r.GET("/test", func(c *gin.Context) { c.String(200, "ok") })
	return r
}

func doCORS(r *gin.Engine, method, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/test", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// varyContainsOrigin reports whether the Vary header value contains an
// "Origin" token, comma-list aware and case-insensitive. This mirrors how a
// cache actually interprets Vary (RFC 9110 §12.5.5): the header is a
// comma-separated field-name list, not a single opaque string, so the
// correct check is CONTAINS, never EQUALS.
func varyContainsOrigin(vary string) bool {
	for _, v := range strings.Split(vary, ",") {
		if strings.EqualFold(strings.TrimSpace(v), "Origin") {
			return true
		}
	}
	return false
}

// varyHeader joins EVERY Vary field line on the response, because
// http.Header.Get returns only the FIRST one. The middleware Adds its token
// rather than clobbering an upstream Vary, so a response legitimately carries
// two field lines ("Vary: Accept-Encoding" + "Vary: Origin") -- which RFC 9110
// §5.3 says a recipient combines. Reading with Get made
// TestCORSSetsVaryOrigin_AppendsNotClobbers structurally blind to the very
// token it exists to detect: it reported "Accept-Encoding" and never saw
// "Origin" (#3025).
func varyHeader(w *httptest.ResponseRecorder) string {
	return strings.Join(w.Header().Values(headerVary), ", ")
}

func TestCORSAllowedOriginSetsHeaders(t *testing.T) {
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "GET", testOrigin)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, testOrigin, w.Header().Get(headerAccessControlAllowOrigin))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Machine-Id")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Attestation-Token")
	assert.Equal(t,
		middleware.SessionIssuedHeader+", "+middleware.SessionIDHeader+", "+
			middleware.RateLimitResetHeader+", "+middleware.RateLimitLimitHeader+", "+
			middleware.RetryAfterHeader+", "+middleware.FileMimeTypeHeader+", "+
			middleware.FileKeyVersionHeader,
		w.Header().Get("Access-Control-Expose-Headers"),
	)

	// Named individually as well as in the exact-match above: an attachment's
	// MIME type and CSK epoch exist ONLY as headers, because the body is
	// ciphertext. A cross-origin renderer that cannot read the epoch decrypts
	// with the current key and reports every pre-rotation attachment as
	// tampering, so dropping either from this list is a silent breakage.
	exposed := w.Header().Get("Access-Control-Expose-Headers")
	assert.Contains(t, exposed, middleware.FileMimeTypeHeader)
	assert.Contains(t, exposed, middleware.FileKeyVersionHeader)

	// The two rate-limit headers a client must READ, named individually for the
	// same reason. Setting a response header and exposing it are different
	// things: both are non-safelisted, so a cross-origin renderer — the packaged
	// app://concord origin and the remote SPA — reads null for anything missing
	// from this list, with no error anywhere.
	//
	// X-RateLimit-Limit is the discriminator between this API's two 429 sources
	// (a per-user route budget and a per-resource budget), which have different
	// blast radii. Without it a client cannot tell them apart and cannot scope
	// its retry, so #1218's DM key-distribution suppression silently never
	// engages in production while passing every same-origin test.
	assert.Contains(t, exposed, middleware.RateLimitLimitHeader)
	assert.Contains(t, exposed, middleware.RetryAfterHeader)
}

func TestCORSEmptyOriginNoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"*"})
	w := doCORS(r, "GET", "")

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORSNullOriginNoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"*"})
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSDisallowedOriginNoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "GET", "https://evil.com")

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "OPTIONS", testOrigin)

	assert.Equal(t, 204, w.Code)
	assert.Equal(t, testOrigin, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSPreflightEmptyOrigin(t *testing.T) {
	r := corsRouter([]string{"*"})
	w := doCORS(r, "OPTIONS", "")

	assert.Equal(t, 204, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSAppSchemeOriginSetsHeaders(t *testing.T) {
	r := corsRouter([]string{testAppOrigin})
	w := doCORS(r, "GET", testAppOrigin)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, testAppOrigin, w.Header().Get(headerAccessControlAllowOrigin))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Machine-Id")
}

func TestCORSAppSchemePreflightWithCustomHeader(t *testing.T) {
	r := corsRouter([]string{testAppOrigin})
	req := httptest.NewRequest("OPTIONS", "/test", nil)
	req.Header.Set("Origin", testAppOrigin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "x-machine-id")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 204, w.Code)
	assert.Equal(t, testAppOrigin, w.Header().Get(headerAccessControlAllowOrigin))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Machine-Id")
}

func TestCORSAppSchemeNotInAllowlistRejected(t *testing.T) {
	r := corsRouter([]string{testOrigin}) // only HTTPS origin allowed
	w := doCORS(r, "GET", testAppOrigin)

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}

// ---------------------------------------------------------------------------
// Regression tests for #3025: a response from the CORS middleware must carry
// Vary: Origin on EVERY branch — including when the Origin is absent or not
// allowlisted — because Access-Control-Allow-Origin is reflected per-request.
// Without Vary: Origin, a shared cache (browser HTTP cache, CDN, reverse
// proxy) may store one origin's response and serve it to a different origin:
// reproduced live when a Chromium client cached an
// "Access-Control-Allow-Origin: app://concord" response, then moved to
// "https://spa.concordvoice.chat" and was incorrectly blocked by its own
// cached headers.
//
// These assert CONTAINS, not EQUALS: the intended fix uses Header().Add,
// which appends to any Vary value already present, and an equality check
// would wrongly fail the moment a second legitimate Vary value (e.g.
// Accept-Encoding) exists alongside Origin.
// ---------------------------------------------------------------------------

func TestCORSSetsVaryOrigin_AllowedOrigin(t *testing.T) {
	// regression for #3025
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "GET", testOrigin)

	vary := varyHeader(w)
	assert.True(t, varyContainsOrigin(vary),
		"expected Vary to contain Origin for an allowlisted origin, got %q", vary)
}

func TestCORSSetsVaryOrigin_NonAllowlistedOrigin(t *testing.T) {
	// regression for #3025
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "GET", "https://evil.com")

	vary := varyHeader(w)
	assert.True(t, varyContainsOrigin(vary),
		"expected Vary to contain Origin for a non-allowlisted origin, got %q", vary)
}

func TestCORSSetsVaryOrigin_AbsentOrigin(t *testing.T) {
	// regression for #3025
	r := corsRouter([]string{"*"})
	w := doCORS(r, "GET", "")

	vary := varyHeader(w)
	assert.True(t, varyContainsOrigin(vary),
		"expected Vary to contain Origin when the Origin header is absent, got %q", vary)
}

func TestCORSSetsVaryOrigin_NullOrigin(t *testing.T) {
	// regression for #3025
	r := corsRouter([]string{"*"})
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	vary := varyHeader(w)
	assert.True(t, varyContainsOrigin(vary),
		"expected Vary to contain Origin for Origin: null, got %q", vary)
}

func TestCORSSetsVaryOrigin_PreflightAllowedOrigin(t *testing.T) {
	// regression for #3025
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "OPTIONS", testOrigin)

	require := assert.New(t)
	require.Equal(204, w.Code)
	vary := varyHeader(w)
	require.True(varyContainsOrigin(vary),
		"expected Vary to contain Origin on an allowlisted OPTIONS preflight, got %q", vary)
}

func TestCORSSetsVaryOrigin_PreflightNonAllowlistedOrigin(t *testing.T) {
	// regression for #3025
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "OPTIONS", "https://evil.com")

	require := assert.New(t)
	require.Equal(204, w.Code)
	vary := varyHeader(w)
	require.True(varyContainsOrigin(vary),
		"expected Vary to contain Origin on a non-allowlisted OPTIONS preflight, got %q", vary)
}

// TestCORSSetsVaryOrigin_AppendsNotClobbers proves the fix must APPEND to an
// existing Vary value rather than overwrite it. A middleware mounted ahead of
// CORS in the chain pre-sets "Vary: Accept-Encoding" (a realistic upstream
// concern — e.g. a compression middleware). After CORS runs, both values must
// be present: Accept-Encoding must survive, and Origin must be added.
func TestCORSSetsVaryOrigin_AppendsNotClobbers(t *testing.T) {
	// regression for #3025
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set(headerVary, "Accept-Encoding")
		c.Next()
	})
	r.Use(middleware.CORS([]string{testOrigin}))
	r.GET("/test", func(c *gin.Context) { c.String(200, "ok") })

	w := doCORS(r, "GET", testOrigin)

	vary := varyHeader(w)
	assert.Contains(t, vary, "Accept-Encoding",
		"expected the pre-existing Vary: Accept-Encoding to survive CORS middleware, got %q", vary)
	assert.True(t, varyContainsOrigin(vary),
		"expected Vary to gain an Origin token alongside the pre-existing Accept-Encoding value, got %q", vary)
}
