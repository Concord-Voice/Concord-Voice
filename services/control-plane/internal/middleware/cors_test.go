package middleware_test

import (
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

const (
	testOrigin                     = "https://concordvoice.chat"
	testAppOrigin                  = "app://concord"
	headerAccessControlAllowOrigin = "Access-Control-Allow-Origin"
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
