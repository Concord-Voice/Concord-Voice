package middleware_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- Auth Middleware ---

func TestAuthRequiredValidToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "authvalid")

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthRequiredNoHeader(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequiredInvalidFormat(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set("Authorization", "NotBearer token")
	w := ts.DoRequest("GET", "/api/v1/users/me", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequiredExpiredToken(t *testing.T) {
	ts := setupTS(t)

	// Use a hand-crafted expired token
	headers := http.Header{}
	headers.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1c2VyX2lkIjoiMTIzIiwiZXhwIjoxNjAwMDAwMDAwfQ.invalid")
	w := ts.DoRequest("GET", "/api/v1/users/me", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequiredBlacklistedToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "blacklist1")

	// Logout (blacklists token)
	ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	// Try to use blacklisted token
	w := ts.DoRequest("GET", "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Security Headers ---

func TestSecurityHeadersPresent(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/health", nil, nil)

	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.NotEmpty(t, w.Header().Get("Referrer-Policy"))
}
