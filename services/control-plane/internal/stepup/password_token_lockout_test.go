package stepup_test

// The mint endpoint through the full router (#3509, design spec "Developer
// decisions, 2026-10-01", T-2): it is mounted at its documented path behind
// authentication, and it shares /login's email-keyed lockout in both
// directions, so it is never a cheaper password oracle than login.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const mintPath = "/api/v1/auth/step-up/password"

func mintVia(ts *testhelpers.TestServer, user testhelpers.TestUser, password string) int {
	return ts.DoRequest(http.MethodPost, mintPath,
		map[string]string{"current_password": password, "purpose": "dm.clear"},
		testhelpers.AuthHeaders(user.AccessToken)).Code
}

func loginVia(ts *testhelpers.TestServer, user testhelpers.TestUser, password string) int {
	return ts.DoRequest(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": user.Email, "password": password}, nil).Code
}

// Mutant killed: registering the route on the public /auth group (the
// unauthenticated request reaches the handler and is answered 400, not 401).
func TestMintRoute_IsAuthenticated(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	w := ts.DoRequest(http.MethodPost, mintPath,
		map[string]string{"current_password": "x", "purpose": "dm.clear"}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// Five wrong passwords at the mint lock the account at /login, and the
// lockout then refuses the mint even for the correct password.
//
// Mutant killed: verifying with auth.VerifyPassword directly instead of the
// lockout-sharing verifier (login still succeeds after five failed mints).
func TestMintRoute_WrongPasswordsLockLogin(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "mint_lockout_a")

	for range 5 {
		require.Equal(t, http.StatusForbidden, mintVia(ts, user, "wrong-password"))
	}

	assert.Equal(t, http.StatusUnauthorized, loginVia(ts, user, user.Password),
		"five failed mints must lock /login for the correct password")
	assert.Equal(t, http.StatusLocked, mintVia(ts, user, user.Password),
		"the lockout refuses the mint for the correct password too")
}

// The reverse: five wrong passwords at /login lock the mint.
func TestMintRoute_LoginLockoutRefusesTheMint(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "mint_lockout_b")
	require.Equal(t, http.StatusOK, mintVia(ts, user, user.Password), "control: the correct password mints")

	for range 5 {
		require.Equal(t, http.StatusUnauthorized, loginVia(ts, user, "wrong-password"))
	}

	assert.Equal(t, http.StatusLocked, mintVia(ts, user, user.Password))
}

// The password reaches no log line through the full stack either.
func TestMintRoute_PasswordNeverLogged(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "mint_logs")
	logs := ts.CaptureLogs(t)

	require.Equal(t, http.StatusOK, mintVia(ts, user, user.Password))
	require.Equal(t, http.StatusForbidden, mintVia(ts, user, "wrong-"+user.Password))

	assert.NotContains(t, logs.String(), user.Password)
}
