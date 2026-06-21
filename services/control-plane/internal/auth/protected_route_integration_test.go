package auth_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

// TestProtectedRoute_BlocksUnverified verifies that email-verified middleware
// blocks users whose email_verified=false from accessing protected routes.
func TestProtectedRoute_BlocksUnverified(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "unverified1")

	// Protected routes require verified email — GET /servers should be blocked
	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestProtectedRoute_AllowsVerified verifies that users with email_verified=true
// can access protected routes.
func TestProtectedRoute_AllowsVerified(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verified1") // email_verified=true

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
