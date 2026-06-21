package auth_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// noopDisconnector satisfies auth.SessionDisconnector for tests that construct a
// bare auth.Handler (the SSO adapter path) without a real WebSocket hub.
type noopDisconnector struct{}

func (noopDisconnector) DisconnectUser(uuid.UUID) {}

// These integration tests prove the #1623 age-verification disabled-account gates
// on the auth surface (spec "integration-proven" AC). They disable an account
// directly via SQL (the age-claim path that sets users.disabled is covered by the
// internal/age suite); here we verify login and refresh both fail closed.

// Task 7: a disabled account is rejected at login AFTER the credential check
// (so disabled status is not revealed to an unauthenticated prober).
func TestLogin_DisabledAccount_403(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "disabledloginuser")

	_, err := ts.DB.Exec(
		`UPDATE users SET disabled=TRUE, disabled_reason='age_verification', disabled_at=NOW() WHERE id=$1`,
		user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext, // correct credentials
	}, nil)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "account_disabled", body["error_code"])
}

// Task 8: a disabled account cannot refresh — the disabled JOIN in
// fetchActiveRefreshToken treats its token as not-found, and no new token is minted.
func TestRefresh_DisabledAccount_Blocked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "disabledrefreshuser")

	// Log in while still enabled to obtain a real, live refresh token.
	lw := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
	var lb map[string]interface{}
	testhelpers.ParseJSON(t, lw, &lb)
	refreshToken, _ := lb["refresh_token"].(string)
	require.NotEmpty(t, refreshToken, "login must return a refresh token")

	// Disable the account (mirrors the terminal-disable state the age path produces).
	_, err := ts.DB.Exec(
		`UPDATE users SET disabled=TRUE, disabled_reason='age_verification', disabled_at=NOW() WHERE id=$1`,
		user.ID)
	require.NoError(t, err)

	// Refresh must fail closed: no new access/refresh token for a disabled account.
	hdr := http.Header{}
	hdr.Set("X-Refresh-Token", refreshToken)
	rw := ts.DoRequest("POST", pathRefresh, nil, hdr)

	require.NotEqual(t, http.StatusOK, rw.Code,
		"disabled account must not refresh; got %d: %s", rw.Code, rw.Body.String())
	var rb map[string]interface{}
	testhelpers.ParseJSON(t, rw, &rb)
	assert.Empty(t, rb["access_token"], "no access token may be minted for a disabled account")
}

// Fix #3: the SSO token-mint adapter gates on users.disabled, returning
// auth.ErrAccountDisabled so a disabled user cannot establish a session via SSO
// (the oauth handler maps this sentinel to 403 account_disabled).
func TestIssueAccessAndRefresh_DisabledAccount_ReturnsSentinel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssodisableduser")
	_, err := ts.DB.Exec(`UPDATE users SET disabled=TRUE WHERE id=$1`, user.ID)
	require.NoError(t, err)

	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, noopDisconnector{})
	_, _, err = h.IssueAccessAndRefresh(context.Background(), user.ID)
	require.ErrorIs(t, err, auth.ErrAccountDisabled)
}

// And an enabled account still mints via the same path (no false positive).
func TestIssueAccessAndRefresh_EnabledAccount_Mints(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssoenableduser")

	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, noopDisconnector{})
	access, refresh, err := h.IssueAccessAndRefresh(context.Background(), user.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, access)
	assert.NotEmpty(t, refresh)
}

// A userID with no users row hits the sql.ErrNoRows branch and returns a
// "user not found" error — distinct from the disabled sentinel, and never a
// minted token.
func TestIssueAccessAndRefresh_UserNotFound(t *testing.T) {
	ts := setupTS(t)
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, noopDisconnector{})

	access, refresh, err := h.IssueAccessAndRefresh(context.Background(), uuid.New().String())
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrAccountDisabled)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, access)
	assert.Empty(t, refresh)
}
