package middleware_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// TestAuthRequired_UserDisabledDenylist_403 proves the #1623 immediate-effect
// mid-session enforcement: a cryptographically-valid, un-expired access token is
// rejected the instant the user_disabled:<id> denylist key is present — closing the
// up-to-15-min window where a live access token would otherwise outlive a terminal
// disable. (Reuses makeToken / doAuthRequest / setupTS / testSecret from auth_test.go.)
func TestAuthRequired_UserDisabledDenylist_403(t *testing.T) {
	ts := setupTS(t)
	userID := uuid.New().String()

	// Set the denylist key exactly as the age terminal-disable path does (no TTL).
	require.NoError(t, ts.Redis.Set(t.Context(), "user_disabled:"+userID, "1", 0).Err())

	// A valid bearer token (not blacklisted — no jti) for that user.
	tok := makeToken(t, jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(15 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}, testSecret)

	w := doAuthRequest(t, ts, bearerPrefix+tok)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "account_disabled", body["error_code"])
}
