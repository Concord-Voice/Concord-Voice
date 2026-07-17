package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func doSecuredRequest(t *testing.T, env, hstsValue string) http.Header {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders(env, hstsValue))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)
	return w.Header()
}

func TestSecurityHeaders_ProductionFullSet(t *testing.T) {
	h := doSecuredRequest(t, "production", DefaultHSTSHeaderValue)

	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"))
	assert.Equal(t, "strict-origin-when-cross-origin", h.Get("Referrer-Policy"))
	assert.Equal(t, "default-src 'none'; frame-ancestors 'none'", h.Get("Content-Security-Policy"))
	assert.Equal(t, "0", h.Get("X-XSS-Protection"))
	assert.Equal(t,
		"camera=(self), microphone=(self), geolocation=(), payment=(), usb=(), magnetometer=(), gyroscope=(), accelerometer=()",
		h.Get("Permissions-Policy"))
	assert.Equal(t, "max-age=63072000; includeSubDomains; preload", h.Get("Strict-Transport-Security"))
	// Single emission at the Go layer (c.Header uses Set semantics).
	assert.Len(t, h.Values("Strict-Transport-Security"), 1)
}

func TestSecurityHeaders_HSTSOverrideHonored(t *testing.T) {
	h := doSecuredRequest(t, "production", "max-age=63072000; includeSubDomains")
	assert.Equal(t, "max-age=63072000; includeSubDomains", h.Get("Strict-Transport-Security"))
}

func TestSecurityHeaders_NonProductionOmitsHSTS(t *testing.T) {
	h := doSecuredRequest(t, "development", DefaultHSTSHeaderValue)
	assert.Empty(t, h.Get("Strict-Transport-Security"))
	// The rest of the set is env-independent.
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "0", h.Get("X-XSS-Protection"))
}

func TestSecurityHeaders_EmptyHSTSValueFallsBackToDefault(t *testing.T) {
	h := doSecuredRequest(t, "production", "")
	assert.Equal(t, DefaultHSTSHeaderValue, h.Get("Strict-Transport-Security"))
}
