package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathRegister      = "/api/v1/auth/register"
	pathLogin         = "/api/v1/auth/login"
	pathRefresh       = "/api/v1/auth/refresh"
	pathUsersMe       = "/api/v1/users/me"
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"
	testEmail         = "newuser@test.concord.chat"
	testPassword      = "TestPassword123!"  //nolint:gosec // test credential constant
	testWrongPassword = "WrongPassword999!" //nolint:gosec // test credential constant
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

func registerPayload() map[string]interface{} {
	pub, priv, salt := testhelpers.E2EETestKeys()
	return map[string]interface{}{
		"email":               testEmail,
		"username":            "newuser",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
}

// --- Registration Tests ---

// TestRegisterCreatesPendingNotUser verifies that POST /register creates a
// pending_registrations row but NOT a users row, and returns a pending_id
// rather than an access token. See #621.
func TestRegisterCreatesPendingNotUser(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	require.Equal(t, http.StatusCreated, w.Code)

	var body struct {
		PendingID     string `json:"pending_id"`
		Email         string `json:"email"`
		ExpiresAt     string `json:"expires_at"`
		CodeExpiresAt string `json:"code_expires_at"`
		AccessToken   string `json:"access_token"` //nolint:gosec // G117: asserting this field is absent in the response
	}
	testhelpers.ParseJSON(t, w, &body)

	assert.NotEmpty(t, body.PendingID, "response must contain pending_id")
	assert.NotEmpty(t, body.Email)
	assert.NotEmpty(t, body.ExpiresAt)
	assert.NotEmpty(t, body.CodeExpiresAt)
	assert.Empty(t, body.AccessToken, "no access_token until verification")

	// No user row should exist yet.
	var userCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE LOWER(email) = LOWER($1)`,
		body.Email,
	).Scan(&userCount)
	require.NoError(t, err)
	assert.Equal(t, 0, userCount, "registration must not create users row")

	// Pending row should exist.
	var pendingCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations WHERE id = $1`,
		body.PendingID,
	).Scan(&pendingCount)
	require.NoError(t, err)
	assert.Equal(t, 1, pendingCount)
}

// TestRegisterTakeoverByEmailMatch verifies that a second registration with
// the SAME email+password replaces the earlier pending row (takeover flow).
func TestRegisterTakeoverByEmailMatch(t *testing.T) {
	ts := setupTS(t)

	body1 := registerPayload()
	w1 := ts.DoRequest("POST", pathRegister, body1, nil)
	require.Equal(t, http.StatusCreated, w1.Code)
	var first struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w1, &first)

	// Second registration with SAME email + password -> takeover.
	w2 := ts.DoRequest("POST", pathRegister, body1, nil)
	require.Equal(t, http.StatusCreated, w2.Code)
	var second struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w2, &second)

	assert.NotEqual(t, first.PendingID, second.PendingID, "new pending_id issued")

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations
		 WHERE LOWER(email) = LOWER($1)`,
		body1["email"],
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only one pending for this email")
}

// TestRegisterTakeoverByUsernameMatch verifies that a second registration with
// the SAME username + password (but a new email) replaces the earlier pending
// row via the username-match takeover branch.
func TestRegisterTakeoverByUsernameMatch(t *testing.T) {
	ts := setupTS(t)

	body1 := registerPayload()
	w1 := ts.DoRequest("POST", pathRegister, body1, nil)
	require.Equal(t, http.StatusCreated, w1.Code)
	var first struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w1, &first)

	// Second registration: same username + password, DIFFERENT email.
	body2 := registerPayload()
	body2["email"] = "different@test.concord.chat"
	w2 := ts.DoRequest("POST", pathRegister, body2, nil)
	require.Equal(t, http.StatusCreated, w2.Code)
	var second struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w2, &second)

	assert.NotEqual(t, first.PendingID, second.PendingID, "new pending_id issued")

	// Exactly one pending row should survive for this username.
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations
		 WHERE LOWER(username) = LOWER($1)`,
		body1["username"],
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only one pending for this username")

	// The survivor's email should be the second one.
	var survivingEmail string
	err = ts.DB.QueryRow(
		`SELECT email FROM pending_registrations WHERE id = $1`,
		second.PendingID,
	).Scan(&survivingEmail)
	require.NoError(t, err)
	assert.Equal(t, "different@test.concord.chat", survivingEmail)
}

// TestRegisterUsernamePendingWrongPasswordRejects verifies a caller who knows
// a pending username but supplies the wrong password (and a different email)
// is rejected with ErrUsernameTaken.
func TestRegisterUsernamePendingWrongPasswordRejects(t *testing.T) {
	ts := setupTS(t)

	body1 := registerPayload()
	w1 := ts.DoRequest("POST", pathRegister, body1, nil)
	require.Equal(t, http.StatusCreated, w1.Code)

	// Same username, different email, wrong password.
	body2 := registerPayload()
	body2["email"] = "another@test.concord.chat"
	body2["password"] = testWrongPassword //nolint:gosec // pragma: allowlist secret -- test credential constant
	w2 := ts.DoRequest("POST", pathRegister, body2, nil)

	assert.Equal(t, http.StatusConflict, w2.Code)
	var resp struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &resp)
	assert.Equal(t, "username_taken", resp.Code)
}

// TestRegisterEmailPendingWrongPasswordRejects verifies a caller with the
// wrong password cannot take over an existing pending registration.
func TestRegisterEmailPendingWrongPasswordRejects(t *testing.T) {
	ts := setupTS(t)

	body1 := registerPayload()
	w1 := ts.DoRequest("POST", pathRegister, body1, nil)
	require.Equal(t, http.StatusCreated, w1.Code)

	body2 := registerPayload()
	body2["email"] = body1["email"]
	body2["password"] = "DifferentWrongPassword123!" //nolint:gosec // pragma: allowlist secret -- test credential constant
	w2 := ts.DoRequest("POST", pathRegister, body2, nil)

	assert.Equal(t, http.StatusConflict, w2.Code)
	var resp struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &resp)
	assert.Equal(t, "registration_pending", resp.Code)
}

func TestRegisterDuplicateEmail(t *testing.T) {
	ts := setupTS(t)

	// First registration
	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	// Second with same email, different username, different password -> conflict.
	payload := registerPayload()
	payload["username"] = "differentuser"
	payload["password"] = "AnotherDifferentPass123!" //nolint:gosec // pragma: allowlist secret -- test credential constant
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "registration_pending", body.Code)
}

func TestRegisterDuplicateUsername(t *testing.T) {
	ts := setupTS(t)

	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	payload := registerPayload()
	payload["email"] = "different@test.concord.chat"
	payload["password"] = "YetAnotherDifferentPass123!" //nolint:gosec // pragma: allowlist secret -- test credential constant
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "username_taken", body.Code)
}

// TestRegisterRejectsEmailAlreadyInUsersTable exercises the checkUsersUniqueness
// email-exists branch: a user already promoted into the `users` table blocks
// re-registration with that email.
func TestRegisterRejectsEmailAlreadyInUsersTable(t *testing.T) {
	ts := setupTS(t)
	existing := ts.CreateTestUser(t, "existinguser")

	payload := registerPayload()
	payload["email"] = existing.Email
	payload["username"] = "brandnewuser"
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "email_already_registered", body.Code)
}

// TestRegisterRejectsUsernameAlreadyInUsersTable exercises the
// checkUsersUniqueness username-exists branch: a user already promoted into
// the `users` table blocks re-registration with that username.
func TestRegisterRejectsUsernameAlreadyInUsersTable(t *testing.T) {
	ts := setupTS(t)
	existing := ts.CreateTestUser(t, "reservedname")

	payload := registerPayload()
	payload["email"] = "unused-email@test.concord.chat"
	payload["username"] = existing.Username
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "username_taken", body.Code)
}

func TestRegisterWeakPassword(t *testing.T) {
	ts := setupTS(t)

	payload := registerPayload()
	payload["password"] = "weak"
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterMissingE2EEKeys(t *testing.T) {
	ts := setupTS(t)

	payload := map[string]interface{}{
		"email":            "nokeys@test.concord.chat",
		"username":         "nokeysuser",
		"password":         testPassword,
		"age_confirmation": true,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterNoAgeConfirmation(t *testing.T) {
	ts := setupTS(t)

	payload := registerPayload()
	payload["age_confirmation"] = false
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterSetsRefreshCookie(t *testing.T) {
	ts := setupTS(t)

	// New flow: cookie is set by the confirm step, not the register step.
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)

	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID,
		"code":       code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)

	var refreshCookie *http.Cookie
	for _, c := range confirmW.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie, "refresh_token cookie should be set after confirm")
	assert.True(t, refreshCookie.HttpOnly)
}

// --- Login Tests ---

func TestLoginSuccess(t *testing.T) {
	ts := setupTS(t)

	// Register + confirm so the user exists in the users table (not just pending).
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)
	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID, "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)

	// Login
	w = ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    testEmail,
		"password": testPassword,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.NotNil(t, body["user"])
	assert.NotNil(t, body["e2ee_keys"])
}

func TestLoginWrongPassword(t *testing.T) {
	ts := setupTS(t)

	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    testEmail,
		"password": "WrongPassword123!",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLoginNonexistentEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    "nonexistent@test.concord.chat",
		"password": testPassword,
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Logout Tests ---

func TestLogoutSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutuser")

	w := ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLogoutBlacklistsToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutuser2")

	// Logout
	ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	// Try to use the same token — should fail
	w := ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Protected Route Tests ---

func TestProtectedRouteNoToken(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", pathUsersMe, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProtectedRouteInvalidToken(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer invalid.token.here")
	w := ts.DoRequest("GET", pathUsersMe, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProtectedRouteValidToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "validuser")

	w := ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, user.Username, userData["username"])
}

// --- Refresh Tests ---

func TestRefreshSuccess(t *testing.T) {
	ts := setupTS(t)

	// Register + confirm to get a refresh cookie (cookie is set by confirm step).
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)
	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID, "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)

	// Extract refresh_token cookie from confirm response
	var refreshCookie *http.Cookie
	for _, c := range confirmW.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie)

	// Build request with cookie
	req := httptest.NewRequest("POST", pathRefresh, nil)
	req.AddCookie(refreshCookie)
	req.Header.Set(headerContentType, contentTypeJSON)

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.Equal(t, float64(900), body["expires_in"])
}

func TestRefreshNoCookie(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRefresh, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefreshInvalidToken(t *testing.T) {
	ts := setupTS(t)

	req := httptest.NewRequest("POST", pathRefresh, nil)
	//nolint:gosec // G124: test fixture intentionally uses an unsafe cookie to exercise refresh-token rejection; security attrs irrelevant to the test (sibling Semgrep suppression on next line).
	// nosemgrep: go.lang.security.audit.net.cookie-missing-httponly.cookie-missing-httponly, go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "totally-invalid-token"})
	req.Header.Set(headerContentType, contentTypeJSON)

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusUnauthorized, rw.Code)
}

func TestRefreshRotatesToken(t *testing.T) {
	ts := setupTS(t)

	// Register + confirm to get a refresh cookie.
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)
	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID, "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)

	var firstCookie *http.Cookie
	for _, c := range confirmW.Result().Cookies() {
		if c.Name == "refresh_token" {
			firstCookie = c
			break
		}
	}
	require.NotNil(t, firstCookie)

	// First refresh — should succeed
	req := httptest.NewRequest("POST", pathRefresh, nil)
	req.AddCookie(firstCookie)
	req.Header.Set(headerContentType, contentTypeJSON)
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)
	require.Equal(t, http.StatusOK, rw.Code)

	// Replay old token — should fail (breach detection)
	req2 := httptest.NewRequest("POST", pathRefresh, nil)
	req2.AddCookie(firstCookie)
	req2.Header.Set(headerContentType, contentTypeJSON)
	rw2 := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw2, req2)
	assert.Equal(t, http.StatusUnauthorized, rw2.Code)
}

// --- WS Ticket Tests ---

func TestIssueWSTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticketuser")

	w := ts.DoRequest("POST", "/api/v1/auth/ws-ticket", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["ticket"])
}

func TestIssueWSTicketUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/ws-ticket", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Health Check ---

func TestHealthCheck(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/health", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "healthy", body["status"])
}

// --- Login Lockout ---

func TestLoginLockoutAfterFailedAttempts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lockoutuser")

	// Fail login 5 times to trigger lockout
	for i := 0; i < 5; i++ {
		ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    user.Email,
			"password": testWrongPassword, //nolint:gosec // test credential
		}, nil)
	}

	// 6th attempt: lockout returns 401 with generic message (anti-enumeration)
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testWrongPassword, //nolint:gosec // test credential
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Even correct password should fail during lockout
	w = ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLoginSucceedsAfterFailedAttempts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lockoutok")

	// Fail a few times (not enough for lockout)
	for i := 0; i < 2; i++ {
		ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    user.Email,
			"password": testWrongPassword, //nolint:gosec // test credential
		}, nil)
	}

	// Correct password should still work
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Refresh with Bearer Header ---

func TestRefreshWithXRefreshTokenHeader(t *testing.T) {
	ts := setupTS(t)

	// Register + confirm to get a refresh token.
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)
	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID, "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)
	var confirmBody map[string]interface{}
	testhelpers.ParseJSON(t, confirmW, &confirmBody)
	refreshToken := confirmBody["refresh_token"].(string)

	// Use X-Refresh-Token header instead of cookie
	headers := http.Header{}
	headers.Set("X-Refresh-Token", refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w = ts.DoRequest("POST", pathRefresh, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
}

// --- Device Recovery ---

func TestCreateDeviceRecoveryRequestNoTrustedDevices(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "devrecovery")

	// Seed a recovery code and get a recovery token
	recoveryToken := seedAndVerifyRecovery(t, ts, user)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/device-request", map[string]interface{}{
		"recovery_token": recoveryToken,
	}, nil)

	// Should fail — no trusted devices configured
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict}, w.Code)
}

func TestPollDeviceRecoveryRequestNotFound(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/auth/recovery/device-request/00000000-0000-0000-0000-000000000000", nil, nil)
	assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, w.Code)
}

// --- Social Recovery ---

func TestCreateSocialRecoveryRequestNoCircle(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "socialrecovery")

	recoveryToken := seedAndVerifyRecovery(t, ts, user)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/social-request", map[string]interface{}{
		"recovery_token": recoveryToken,
	}, nil)

	// Should fail — no recovery circle configured
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict}, w.Code)
}

func TestPollSocialRecoveryRequestNotFound(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/auth/recovery/social-request/00000000-0000-0000-0000-000000000000", nil, nil)
	assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, w.Code)
}

// --- Confirm Registration Tests ---

func TestConfirmPromotesToUser(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
		Email     string `json:"email"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	code := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)

	confirmBody := map[string]string{"pending_id": reg.PendingID, "code": code}
	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm", confirmBody, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var ok struct {
		AccessToken  string `json:"access_token"`  //nolint:gosec // G117: response-binding field, not a credential
		RefreshToken string `json:"refresh_token"` //nolint:gosec // G117: response-binding field, not a credential
		User         struct {
			ID            string `json:"id"`
			EmailVerified bool   `json:"email_verified"`
		} `json:"user"`
	}
	testhelpers.ParseJSON(t, w2, &ok)
	require.NotEmpty(t, ok.AccessToken)
	require.NotEmpty(t, ok.RefreshToken)
	require.True(t, ok.User.EmailVerified)

	var userCount, pendingCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, ok.User.ID).Scan(&userCount))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations WHERE id = $1`, reg.PendingID).Scan(&pendingCount))
	require.Equal(t, 1, userCount)
	require.Equal(t, 0, pendingCount)
}

func TestConfirmInvalidCode(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": "000000"}, nil) // pragma: allowlist secret
	require.Equal(t, http.StatusUnauthorized, w2.Code)
	var resp struct {
		Code              string `json:"code"`
		AttemptsRemaining int    `json:"attempts_remaining"`
	}
	testhelpers.ParseJSON(t, w2, &resp)
	require.Equal(t, "invalid_code", resp.Code)
	require.Equal(t, 3, resp.AttemptsRemaining)
}

// --- Resend Tests ---

func TestResendRegistrationCode(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	firstCode := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": reg.PendingID}, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var body struct {
		CodeExpiresAt    string `json:"code_expires_at"`
		ResendsRemaining int    `json:"resends_remaining"`
	}
	testhelpers.ParseJSON(t, w2, &body)
	require.Equal(t, 3, body.ResendsRemaining)

	secondCode := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)
	require.NotEqual(t, firstCode, secondCode)
}

func TestResendCooldown(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	w1 := ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": reg.PendingID}, nil)
	require.Equal(t, http.StatusOK, w1.Code)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": reg.PendingID}, nil)
	require.Equal(t, http.StatusTooManyRequests, w2.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &body)
	require.Equal(t, "cooldown_active", body.Code)
}

// --- Change Email Tests ---

func TestChangeEmailMidRegistration(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
		Email     string `json:"email"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	newEmail := "corrected-" + strings.ToLower(time.Now().Format("150405")) + "@example.com"
	w2 := ts.DoRequest("POST", "/api/v1/auth/register/change-email",
		map[string]string{"pending_id": reg.PendingID, "new_email": newEmail}, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var resp struct {
		Email         string `json:"email"`
		CodeExpiresAt string `json:"code_expires_at"`
	}
	testhelpers.ParseJSON(t, w2, &resp)
	require.Equal(t, newEmail, resp.Email)

	var count int
	err := ts.DB.QueryRow(
		`SELECT resend_count FROM pending_registrations WHERE id = $1`,
		reg.PendingID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

// --- Abandon Registration Tests ---

func TestAbandonPendingRegistration(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	w2 := ts.DoRequest("DELETE", "/api/v1/auth/register/"+reg.PendingID, nil, nil)
	require.Equal(t, http.StatusNoContent, w2.Code)

	var count int
	_ = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations WHERE id = $1`,
		reg.PendingID).Scan(&count)
	require.Equal(t, 0, count)

	w3 := ts.DoRequest("DELETE", "/api/v1/auth/register/"+reg.PendingID, nil, nil)
	require.Equal(t, http.StatusNotFound, w3.Code)
}

func TestFullRegistrationFlow(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
		Email     string `json:"email"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": reg.PendingID}, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	code := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)

	w3 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": code}, nil)
	require.Equal(t, http.StatusOK, w3.Code)

	var promoted struct {
		AccessToken string `json:"access_token"` //nolint:gosec
		User        struct {
			ID            string `json:"id"`
			EmailVerified bool   `json:"email_verified"`
		} `json:"user"`
	}
	testhelpers.ParseJSON(t, w3, &promoted)

	require.NotEmpty(t, promoted.AccessToken)
	require.True(t, promoted.User.EmailVerified)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+promoted.AccessToken)
	w4 := ts.DoRequest("GET", "/api/v1/users/me", nil, hdr)
	require.Equal(t, http.StatusOK, w4.Code)

	var pendingCount, userCount int
	_ = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations WHERE id = $1`,
		reg.PendingID).Scan(&pendingCount)
	_ = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`,
		promoted.User.ID).Scan(&userCount)
	require.Equal(t, 0, pendingCount)
	require.Equal(t, 1, userCount)
}

// --- ConfirmRegistration error branches ---

func TestConfirmRegistration_InvalidJSON(t *testing.T) {
	ts := setupTS(t)
	req := httptest.NewRequest("POST", "/api/v1/auth/register/confirm", strings.NewReader("{bad"))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfirmRegistration_InvalidCodeFormat(t *testing.T) {
	ts := setupTS(t)
	// Register first to get a valid pending_id
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	// Code that is not 6 digits
	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": "abc"}, nil)
	assert.Equal(t, http.StatusBadRequest, w2.Code)
}

func TestConfirmRegistration_TooManyAttempts(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	// Exhaust all attempts (MaxCodeAttempts = 4)
	for i := 0; i < 4; i++ {
		ts.DoRequest("POST", "/api/v1/auth/register/confirm",
			map[string]string{"pending_id": reg.PendingID, "code": "000000"}, nil) //nolint:gosec // G101 false positive: test value, not a credential
	}

	// Next attempt should be too_many_attempts
	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": "000000"}, nil) //nolint:gosec // G101 false positive: test value, not a credential
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &body)
	assert.Equal(t, "too_many_attempts", body.Code)
}

func TestConfirmRegistration_CodeExpired(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	// Delete the Redis key to simulate expiry
	ts.Redis.Del(context.Background(), "email_verify:"+reg.PendingID)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": "123456"}, nil)
	assert.Equal(t, http.StatusGone, w2.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &body)
	assert.Equal(t, "code_expired", body.Code)
}

func TestConfirmRegistration_PendingNotFound(t *testing.T) {
	ts := setupTS(t)
	nonexistent := "00000000-0000-0000-0000-000000000000"
	w := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": nonexistent, "code": "123456"}, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "pending_not_found", body.Code)
}

// --- ResendRegistrationCode error branches ---

func TestResendRegistrationCode_InvalidJSON(t *testing.T) {
	ts := setupTS(t)
	req := httptest.NewRequest("POST", "/api/v1/auth/register/resend", strings.NewReader("{bad"))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestResendRegistrationCode_PendingNotFound(t *testing.T) {
	ts := setupTS(t)
	nonexistent := "00000000-0000-0000-0000-000000000001"
	w := ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": nonexistent}, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "pending_not_found", body.Code)
}

func TestResendRegistrationCode_ResendsExhausted(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	// Exhaust resends by directly updating the DB (MaxResends = 4)
	_, err := ts.DB.Exec(
		`UPDATE pending_registrations SET resend_count = 4, last_resend_at = NOW() - INTERVAL '2 minutes' WHERE id = $1`,
		reg.PendingID)
	require.NoError(t, err)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/resend",
		map[string]string{"pending_id": reg.PendingID}, nil)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &body)
	assert.Equal(t, "resends_exhausted", body.Code)
}

// --- ChangeRegistrationEmail error branches ---

func TestChangeRegistrationEmail_InvalidJSON(t *testing.T) {
	ts := setupTS(t)
	req := httptest.NewRequest("POST", "/api/v1/auth/register/change-email", strings.NewReader("{bad"))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangeRegistrationEmail_PendingNotFound(t *testing.T) {
	ts := setupTS(t)
	nonexistent := "00000000-0000-0000-0000-000000000002"
	w := ts.DoRequest("POST", "/api/v1/auth/register/change-email",
		map[string]string{"pending_id": nonexistent, "new_email": "new@example.com"}, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "pending_not_found", body.Code)
}

func TestChangeRegistrationEmail_EmailAlreadyRegistered(t *testing.T) {
	ts := setupTS(t)

	// Create a confirmed user with a known email
	existingUser := ts.CreateTestUser(t, "confirmeduser")

	// Start a new registration (username avoids reserved substrings like "mail", "admin", etc.)
	pub, priv, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":               "pendingreguser@test.concord.chat",
		"username":            "pendingreguser",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}, nil)
	require.Equal(t, http.StatusCreated, w.Code)
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)

	// Try to change email to the already-registered user's email
	w2 := ts.DoRequest("POST", "/api/v1/auth/register/change-email",
		map[string]string{"pending_id": reg.PendingID, "new_email": existingUser.Email}, nil)
	assert.Equal(t, http.StatusConflict, w2.Code)
	var body struct {
		Code string `json:"code"`
	}
	testhelpers.ParseJSON(t, w2, &body)
	assert.Equal(t, "email_already_registered", body.Code)
}

// --- AbandonRegistration error branches ---

func TestAbandonRegistration_InvalidUUID(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("DELETE", "/api/v1/auth/register/not-a-uuid", nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Helpers ---

// seedAndVerifyRecovery creates a recovery code and verifies it, returning a recovery token.
func seedAndVerifyRecovery(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()

	// Seed a known recovery code in Redis
	code := "123456"
	codeHash := sha256Hex(code)
	redisKey := "recovery_code:" + normalizeEmail(user.Email)
	record := map[string]interface{}{
		"code_hash": codeHash,
		"user_id":   user.ID,
		"attempts":  0,
	}
	recordJSON, _ := json.Marshal(record)
	ts.Redis.Set(context.Background(), redisKey, recordJSON, 10*time.Minute)

	// Verify the code to get a recovery token
	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  code,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	return body["recovery_token"].(string)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

// --- SSO Short-Circuit Tests (Task 11 / issue #270) ---

// TestLogin_PasswordDisabled_ReturnsAccountUsesSSO verifies that /login on an
// SSO-only account returns 403 account_uses_sso with the linked-provider list
// (instead of 401 invalid_credentials), so the renderer can swap the form for
// the SSO button.
func TestLogin_PasswordDisabled_ReturnsAccountUsesSSO(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssoonly")

	// Flip the SSO-only flag and link a Google identity.
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'google-sub-X', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": "any-password-the-user-types", //nolint:gosec // test credential, irrelevant
	}, nil)

	require.Equal(t, http.StatusForbidden, w.Code)
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "account_uses_sso", resp["error_code"])
	providers, ok := resp["providers"].([]interface{})
	require.True(t, ok, "providers field should be a JSON array")
	assert.Contains(t, providers, "google")
}

// TestLogin_PasswordDisabled_DoesNotIncrementLockout verifies that the lockout
// counter is NOT engaged for SSO-only short-circuited responses. Lockout is
// for password-credential-bruteforce, not user navigation errors.
//
// We verify the precise invariant: the per-email Redis keys
// (login_attempts:<email>, login_lockout_count:<email>) are never created by
// the SSO short-circuit path. Then we flush the per-IP rate-limit key (which
// IS expected to engage — it's a separate, IP-scoped defense applied via
// middleware) and confirm normal login succeeds, proving no per-email lockout
// was sticky.
func TestLogin_PasswordDisabled_DoesNotIncrementLockout(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nolock")

	// Set password_login_disabled = TRUE AND insert an SSO identity so this test // pragma: allowlist secret
	// exercises the 403 account_uses_sso path (its original intent). Without the
	// identity row, the new len(providers) == 0 branch in Login() returns 500
	// sso_account_misconfigured for the data-integrity violation case — that
	// path is covered by TestLogin_PasswordDisabled_NoIdentities_Returns500.
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'google-sub-NOLOCK', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	rctx := context.Background()

	// Hammer /login 5 times — past the 5-attempt lockout threshold for password
	// failures. If the short-circuit accidentally called recordFailedLogin, we'd
	// see login_attempts:<email> populated.
	for i := 0; i < 5; i++ {
		w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    user.Email,
			"password": "x", //nolint:gosec // test credential, irrelevant
		}, nil)
		require.Equal(t, http.StatusForbidden, w.Code)
	}

	// Precise invariant: per-email lockout counters MUST NOT exist.
	attemptsKey := "login_attempts:" + user.Email
	lockoutKey := "login_lockout:" + user.Email
	lockoutCountKey := "login_lockout_count:" + user.Email
	exists, err := ts.Redis.Exists(rctx, attemptsKey, lockoutKey, lockoutCountKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "SSO short-circuit must not engage per-email lockout counter")

	// Flush the per-IP rate-limit key (separate defense; not the lockout counter)
	// so we can verify normal login succeeds when the flag is cleared.
	ts.Redis.Del(rctx, "ratelimit:ip:192.0.2.1:POST:"+pathLogin)

	// Flip the flag back and verify normal login still works.
	_, err = ts.DB.Exec(`UPDATE users SET password_login_disabled = FALSE WHERE id = $1`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)
}

// TestLogin_PasswordDisabled_NoIdentities_Returns500 verifies the
// data-integrity invariant: if a user has password_login_disabled = TRUE but // pragma: allowlist secret
// no SSO identity rows, /login returns 500 sso_account_misconfigured (NOT 403
// account_uses_sso with an empty providers array). The API surface
// (PatchSecurity, DeleteSSOIdentity) refuses transitions that would create
// this state, so reaching this branch implies direct DB mutation, a buggy
// migration, or a race that bypassed the row-lock — all of which are
// server-side faults the renderer cannot recover from with "try again". A
// distinct 500 error_code makes the case discoverable in observability and
// lets the renderer route around the transient-failure UX wording.
//
// Also asserts that this path, like the 403 short-circuit, does NOT engage
// the per-email lockout counter — this is a server fault, not a credential
// bruteforce signal.
func TestLogin_PasswordDisabled_NoIdentities_Returns500(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssomisconfig")

	// Flip the SSO-only flag WITHOUT inserting any user_sso_identities row.
	// This is the data-integrity violation we want to surface.
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": "any-password-the-user-types", //nolint:gosec // test credential, irrelevant
	}, nil)

	require.Equal(t, http.StatusInternalServerError, w.Code,
		"empty providers array on password-disabled account must return 500, not 403")
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "sso_account_misconfigured", resp["error_code"])
	assert.NotContains(t, resp, "providers",
		"500 sso_account_misconfigured response must not include a providers field — that's the 403 path's contract")

	// Per-email lockout counter MUST NOT be engaged for the integrity-error path.
	rctx := context.Background()
	attemptsKey := "login_attempts:" + user.Email
	exists, err := ts.Redis.Exists(rctx, attemptsKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists,
		"sso_account_misconfigured path must not engage per-email lockout — it's a server fault, not a credential failure")
}
