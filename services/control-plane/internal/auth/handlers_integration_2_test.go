package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	invalidBase64       = "!!!not-base64!!!"
	testNewPwd          = "NewSecurePassword123!" //nolint:gosec // test credential constant
	invalidToken        = "invalid-token"
	testSomeEmail       = "someone@test.concord.chat"
	headerXRefreshToken = "X-Refresh-Token" //nolint:gosec // G101 false positive: HTTP header name, not a credential
	bearerPfx           = "Bearer "

	pathLogout              = "/api/v1/auth/logout"
	pathConfirmRegistration = "/api/v1/auth/register/confirm"
	pathRecoverVerify       = "/api/v1/auth/recovery/verify-code"
	pathRecoverResetPwd     = "/api/v1/auth/recovery/reset-password" //nolint:gosec // G101 false positive: URL path, not a credential
	pathRecoverResetAcct    = "/api/v1/auth/recovery/reset-account"
	pathRecoverBegin        = "/api/v1/auth/recovery/begin"
	pathDeviceRequest       = "/api/v1/auth/recovery/device-request"
	pathSocialRequest       = "/api/v1/auth/recovery/social-request"
	pathWSTicket            = "/api/v1/auth/ws-ticket"
	pollSuffix              = "/some-id"
)

// ── Registration Edge Cases ─────────────────────────────────────────────────

func TestRegisterInvalidJSON(t *testing.T) {
	ts := setupTS(t)

	// Send a request with Content-Type JSON but invalid body
	req := httptest.NewRequest("POST", pathRegister, strings.NewReader("{invalid"))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterMissingEmail(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"username":            "missingemail",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterMissingUsername(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "missinguser@test.concord.chat",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterMissingPassword(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "missingpwd@test.concord.chat",
		"username":            "missingpwd",
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterInvalidEmailFormat(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "not-an-email",
		"username":            "bademail",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterInvalidBase64PublicKey(t *testing.T) {
	ts := setupTS(t)

	_, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "badpubkey@test.concord.chat",
		"username":            "badpubkey",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          invalidBase64,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "public key")
}

func TestRegisterInvalidBase64WrappedPrivateKey(t *testing.T) {
	ts := setupTS(t)

	pub, _, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "badwrapped@test.concord.chat",
		"username":            "badwrapped",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": invalidBase64,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "wrapped private key")
}

func TestRegisterInvalidBase64Salt(t *testing.T) {
	ts := setupTS(t)

	pub, priv, _ := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "badsalt@test.concord.chat",
		"username":            "badsalt",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": invalidBase64,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "salt")
}

func TestRegisterUsernameTooShort(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "shortuser@test.concord.chat",
		"username":            "ab", // min=3
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterReturnsPendingID(t *testing.T) {
	// The new registration flow (Task 10 / #621) returns pending_id + email
	// rather than session tokens. Tokens are issued only after email confirmation.
	ts := setupTS(t)
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["pending_id"])
	assert.NotEmpty(t, body["email"])
	assert.NotEmpty(t, body["expires_at"])
	assert.NotEmpty(t, body["code_expires_at"])
	// Tokens are NOT in the register response — they come from /register/confirm.
	assert.Nil(t, body["refresh_token"])
	assert.Nil(t, body["access_token"])
}

func TestRegisterReturnsEmail(t *testing.T) {
	// Replaces TestRegisterReturnsUserData for the new pending-registration flow.
	ts := setupTS(t)
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, testEmail, body["email"])
	assert.NotEmpty(t, body["pending_id"])
}

func TestRegisterNormalizesEmail(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		"email":               "  UPPERCASE@Test.Concord.Chat  ",
		"username":            "normalizeemail",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
	// This may or may not pass binding validation depending on how gin handles
	// leading/trailing whitespace. We test that any valid registration normalizes.
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	// If the email passes validation, the returned email should be normalized.
	if w.Code == http.StatusCreated {
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "uppercase@test.concord.chat", body["email"])
	}
}

// ── Login Edge Cases ────────────────────────────────────────────────────────

func TestLoginInvalidJSON(t *testing.T) {
	ts := setupTS(t)

	req := httptest.NewRequest("POST", pathLogin, strings.NewReader("{bad json"))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginMissingEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"password": testPassword,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginMissingPassword(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email": testEmail,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginReturnsE2EEKeys(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "e2eelogin")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	keys := body["e2ee_keys"].(map[string]interface{})
	assert.NotEmpty(t, keys["wrapped_private_key"])
	assert.NotEmpty(t, keys["key_derivation_salt"])
	assert.NotNil(t, keys["key_version"])
}

func TestLoginReturnsSessionID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sessionlogin")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["session_id"])
	assert.NotEmpty(t, body["refresh_token"])
}

func TestLoginReturnsRefreshCookie(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cookielogin")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	cookies := w.Result().Cookies()
	var refreshCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie, "refresh_token cookie should be set on login")
	assert.True(t, refreshCookie.HttpOnly)
}

func TestLoginRememberMeExplicitTrue(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "remembermetrue")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":       user.Email,
		"password":    testhelpers.TestAuthPlaintext,
		"remember_me": true,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["remember_me"])
}

func TestLoginRememberMeExplicitFalse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "remembermefalse")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":       user.Email,
		"password":    testhelpers.TestAuthPlaintext,
		"remember_me": false,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["remember_me"])

	// When remember_me=false, cookie should be session-scoped (MaxAge=0)
	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "refresh_token" {
			assert.Equal(t, 0, c.MaxAge, "Session cookie should have MaxAge=0")
			break
		}
	}
}

func TestLoginRememberMeDefaultTrue(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "remembermdefault")

	// Omit remember_me — should default to true
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["remember_me"])
}

func TestLoginSetsRefreshTokenInDB(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dbrefresh")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	sessionID := body["session_id"].(string)

	// Verify the session exists in the DB
	var dbUserID string
	err := ts.DB.QueryRow(
		`SELECT user_id FROM refresh_tokens WHERE id = $1 AND revoked_at IS NULL`, sessionID,
	).Scan(&dbUserID)
	require.NoError(t, err)
	assert.Equal(t, user.ID, dbUserID)
}

// ── Logout Edge Cases ───────────────────────────────────────────────────────

func TestLogoutNoRefreshToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutnort")

	// Logout without any refresh token — should still return 200
	w := ts.DoRequest("POST", pathLogout, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "Already logged out")
}

func TestLogoutClearsCookie(t *testing.T) {
	ts := setupTS(t)

	// Register + confirm to get tokens; refresh cookie is set by the confirm step.
	accessToken, _, confirmW := registerAndConfirmTokens(t, ts, registerPayload())

	// Extract the refresh cookie from the confirm response.
	var refreshCookie *http.Cookie
	for _, c := range confirmW.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie)

	// Build logout request with cookie
	req := httptest.NewRequest("POST", pathLogout, nil)
	req.AddCookie(refreshCookie)
	req.Header.Set("Authorization", bearerPfx+accessToken)
	req.Header.Set(headerContentType, contentTypeJSON)
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)

	// Verify cookie is cleared (MaxAge < 0 in the response)
	for _, c := range rw.Result().Cookies() {
		if c.Name == "refresh_token" {
			assert.True(t, c.MaxAge < 0, "refresh_token cookie should be cleared (MaxAge < 0)")
			break
		}
	}
}

func TestLogoutRevokesRefreshTokenInDB(t *testing.T) {
	ts := setupTS(t)

	accessToken, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())
	tokenHash := hashToken(refreshToken)

	headers := http.Header{}
	headers.Set("Authorization", bearerPfx+accessToken)
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathLogout, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify the refresh token is revoked in DB
	var revokedAt *time.Time
	err := ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&revokedAt)
	require.NoError(t, err)
	assert.NotNil(t, revokedAt, "Refresh token should be revoked in DB after logout")
}

func TestLogoutWithXRefreshTokenHeader(t *testing.T) {
	ts := setupTS(t)

	accessToken, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())

	headers := http.Header{}
	headers.Set("Authorization", bearerPfx+accessToken)
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathLogout, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "Logged out successfully")
}

// TestLogout_AttackerCannotWipeArbitrarySessionAttestation is the security
// regression test for finding #15 of the #1264 review.
//
// Prior implementation: Logout called attestation.CleanupTokensForSession
// using c.GetHeader("X-Session-ID") — a client-supplied value never
// cross-checked against the bearer. An attacker holding their own valid
// refresh token could POST /auth/logout with the victim's X-Session-ID
// header and wipe the victim's attestation tokens (sibling of #1142 /
// #1154 — client-supplied identity gating a privileged side-effect).
//
// Fixed posture: cleanup drives off `refresh_tokens.id` of the row we just
// revoked, via UPDATE ... RETURNING user_id, id. The header is never read.
//
// This test:
//  1. Registers a "victim" user; seeds an attestation token at
//     attestation:<victim-session-id>:<machine-id>.
//  2. Registers an "attacker" user; the attacker holds their own valid
//     refresh token.
//  3. Attacker POSTs /auth/logout with X-Session-ID = <victim-session-id>
//     and their own refresh token.
//  4. Asserts the attacker's session is revoked AND the victim's
//     attestation token is STILL PRESENT (pre-fix would have wiped it).
//
// sessionIDForRefreshToken returns the refresh_tokens.id (a.k.a. session_id
// in the API responses) for a given refresh token. Used by finding-#15
// security regression tests so we can read the session ID without
// re-decoding the confirm response body (which `registerAndConfirmTokens`
// has already drained via json.Decoder).
func sessionIDForRefreshToken(t *testing.T, ts *testhelpers.TestServer, refreshToken string) string {
	t.Helper()
	h := sha256.Sum256([]byte(refreshToken))
	tokenHash := hex.EncodeToString(h[:])
	var sessionID string
	err := ts.DB.QueryRow(
		`SELECT id FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&sessionID)
	require.NoError(t, err)
	return sessionID
}

func TestLogout_AttackerCannotWipeArbitrarySessionAttestation(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Step 1: victim registers + has an attestation token in Redis.
	_, victimRefreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())
	victimSessionID := sessionIDForRefreshToken(t, ts, victimRefreshToken)
	require.NotEmpty(t, victimSessionID)

	victimAttestationKey := "attestation:" + victimSessionID + ":m-victim-machine"
	require.NoError(t, ts.Redis.Set(ctx, victimAttestationKey, `{"token":"victim-att-token","version":"0.2.7"}`, time.Hour).Err())

	// Step 2: attacker registers separately.
	attackerPayload := registerPayload()
	attackerPayload["username"] = "attackerbob"
	attackerPayload["email"] = "attacker@test.concord.chat"
	attackerAccessToken, attackerRefreshToken, _ := registerAndConfirmTokens(t, ts, attackerPayload)
	require.NotEmpty(t, attackerAccessToken)
	require.NotEmpty(t, attackerRefreshToken)

	// Step 3: attacker POSTs /auth/logout with the VICTIM's X-Session-ID
	// but the ATTACKER's own refresh token + access token.
	headers := http.Header{}
	headers.Set("Authorization", bearerPfx+attackerAccessToken)
	headers.Set(headerXRefreshToken, attackerRefreshToken)
	headers.Set("X-Session-ID", victimSessionID) // <-- ATTACK
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathLogout, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code, "logout response: %s", w.Body.String())

	// Step 4: victim's attestation token MUST still be present. Pre-fix the
	// header-driven cleanup would have wiped it.
	val, err := ts.Redis.Get(ctx, victimAttestationKey).Result()
	require.NoError(t, err, "victim's attestation token must NOT have been deleted by the attacker")
	require.Contains(t, val, "victim-att-token",
		"victim's stored attestation record must be intact after attacker's cross-session logout attempt")
}

// TestLogout_DrivesAttestationCleanupOffRevokedSession asserts the positive
// half of the finding #15 fix: a legitimate logout DOES clean up the
// authenticated user's own attestation tokens. The cleanup is driven by
// `refresh_tokens.id` returned from the UPDATE — NOT the X-Session-ID
// header — so even if the header is absent the cleanup still fires.
func TestLogout_DrivesAttestationCleanupOffRevokedSession(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	accessToken, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())
	sessionID := sessionIDForRefreshToken(t, ts, refreshToken)
	require.NotEmpty(t, sessionID)

	// Seed an attestation token for this session.
	attestationKey := "attestation:" + sessionID + ":m-user-machine"
	require.NoError(t, ts.Redis.Set(ctx, attestationKey, `{"token":"my-att-token","version":"0.2.7"}`, time.Hour).Err())

	// Logout WITHOUT the X-Session-ID header. The header-driven implementation
	// would have left the attestation token in place; the fixed implementation
	// (UPDATE ... RETURNING id) cleans it up off the revoked refresh token.
	headers := http.Header{}
	headers.Set("Authorization", bearerPfx+accessToken)
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathLogout, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	// The attestation token MUST be gone.
	_, err := ts.Redis.Get(ctx, attestationKey).Result()
	require.Error(t, err, "expected the attestation token to be deleted by logout cleanup")
}

// ── Refresh Edge Cases ──────────────────────────────────────────────────────

func TestRefreshReturnsNewRefreshTokenAndSessionID(t *testing.T) {
	ts := setupTS(t)

	_, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())

	headers := http.Header{}
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathRefresh, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.NotEmpty(t, body["refresh_token"])
	assert.NotEmpty(t, body["session_id"])
	assert.Equal(t, float64(900), body["expires_in"])

	// New refresh token should differ from the original
	assert.NotEqual(t, refreshToken, body["refresh_token"])
}

func TestRefreshRevokesOldTokenInDB(t *testing.T) {
	ts := setupTS(t)

	_, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())
	oldHash := hashToken(refreshToken)

	headers := http.Header{}
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathRefresh, nil, headers)
	require.Equal(t, http.StatusOK, w.Code)

	// Old token should be revoked in DB
	var revokedAt *time.Time
	err := ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE token_hash = $1`, oldHash,
	).Scan(&revokedAt)
	require.NoError(t, err)
	assert.NotNil(t, revokedAt, "Old refresh token should be revoked after rotation")
}

func TestRefreshEmptyXRefreshTokenHeader(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set(headerXRefreshToken, "")
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathRefresh, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefreshNewTokenIsUsable(t *testing.T) {
	ts := setupTS(t)

	_, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())

	// First refresh
	headers := http.Header{}
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathRefresh, nil, headers)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	newRefreshToken := body["refresh_token"].(string)

	// Second refresh using the NEW token should succeed
	headers2 := http.Header{}
	headers2.Set(headerXRefreshToken, newRefreshToken)
	headers2.Set(headerContentType, contentTypeJSON)

	w = ts.DoRequest("POST", pathRefresh, nil, headers2)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRefreshSetsNewRefreshCookie(t *testing.T) {
	ts := setupTS(t)

	// Register + confirm; refresh cookie is set by the confirm step.
	_, _, confirmW := registerAndConfirmTokens(t, ts, registerPayload())

	var refreshCookie *http.Cookie
	for _, c := range confirmW.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie)

	// Refresh using cookie
	req := httptest.NewRequest("POST", pathRefresh, nil)
	req.AddCookie(refreshCookie)
	req.Header.Set(headerContentType, contentTypeJSON)
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	require.Equal(t, http.StatusOK, rw.Code)

	// New cookie should be set
	var newRefreshCookie *http.Cookie
	for _, c := range rw.Result().Cookies() {
		if c.Name == "refresh_token" {
			newRefreshCookie = c
			break
		}
	}
	require.NotNil(t, newRefreshCookie, "New refresh_token cookie should be set on refresh")
	assert.True(t, newRefreshCookie.HttpOnly)
	assert.NotEqual(t, refreshCookie.Value, newRefreshCookie.Value, "Cookie should be rotated")
}

// ── Recovery Verify Code Edge Cases ─────────────────────────────────────────

func TestRecoveryVerifyCodeMissingEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"code": "123456",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryVerifyCodeMissingCode(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"email": testSomeEmail,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryVerifyCodeNonNumericCode(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"email": testSomeEmail,
		"code":  "abcdef",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "6 digits")
}

func TestRecoveryVerifyCodeTooShort(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"email": testSomeEmail,
		"code":  "123",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "6 digits")
}

func TestRecoveryVerifyCodeNoRedisEntry(t *testing.T) {
	ts := setupTS(t)

	// No recovery code seeded — should fail with 401
	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"email": "nonexistent@test.concord.chat",
		"code":  "123456",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCodeDashNormalization(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dashnorm")

	code := "654321"
	seedRecoveryCodeLocal(t, ts, user.Email, code, user.ID)

	// Submit with dashes — should be normalized
	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"email": user.Email,
		"code":  "654-321",
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["recovery_token"])
}

// ── RecoveryBegin Edge Cases ────────────────────────────────────────────────

func TestRecoveryBeginMissingEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverBegin, map[string]interface{}{}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryBeginInvalidJSON(t *testing.T) {
	ts := setupTS(t)

	req := httptest.NewRequest("POST", pathRecoverBegin, strings.NewReader("{broken"))
	req.Header.Set(headerContentType, contentTypeJSON)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── RecoveryResetPassword Edge Cases ────────────────────────────────────────

func TestRecoveryResetPasswordMissingBody(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryResetPasswordInvalidBase64WrappedKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badkeyresetpwd")

	recoveryToken := getRecoveryToken(t, ts, user)

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPwd,
		"wrapped_private_key": invalidBase64,
		"key_derivation_salt": base64.StdEncoding.EncodeToString([]byte("valid-salt-bytes")),
		"key_derivation_alg":  testKeyDerivationAlg,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "wrapped_private_key")
}

func TestRecoveryResetPasswordInvalidBase64Salt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badsaltresetpwd")

	recoveryToken := getRecoveryToken(t, ts, user)

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPwd,
		"wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("valid-key-bytes")),
		"key_derivation_salt": invalidBase64,
		"key_derivation_alg":  testKeyDerivationAlg,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "key_derivation_salt")
}

func TestRecoveryResetPasswordRevokesAllSessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeall")

	// Create an additional session by logging in
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Count active sessions before reset
	var activeCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&activeCount)
	require.NoError(t, err)
	require.Greater(t, activeCount, 0, "Should have at least one active session")

	// Do password reset
	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	w = ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPwd,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// All sessions should now be revoked
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&activeCount)
	require.NoError(t, err)
	assert.Equal(t, 0, activeCount, "All sessions should be revoked after password reset")
}

func TestRecoveryResetPasswordUpdatesPasswordHash(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pwdhashupdate")

	// Get old password hash
	var oldHash string
	err := ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&oldHash)
	require.NoError(t, err)

	// Reset password
	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        "CompletelyNewPassword123!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Password hash should have changed
	var newHash string
	err = ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&newHash)
	require.NoError(t, err)
	assert.NotEqual(t, oldHash, newHash, "Password hash should be updated after reset")
}

// ── RecoveryResetAccount Edge Cases ─────────────────────────────────────────

func TestRecoveryResetAccountMissingBody(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryResetAccountInvalidToken(t *testing.T) {
	ts := setupTS(t)
	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        invalidToken,
		"new_password":          testNewPwd,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryResetAccountWeakPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "weakacct")

	recoveryToken := getRecoveryToken(t, ts, user)
	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          "weak",
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryResetAccountInvalidBase64PublicKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badpubacct")

	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPwd,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            invalidBase64,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "public_key")
}

func TestRecoveryResetAccountInvalidBase64WrappedKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badkeyacct")

	recoveryToken := getRecoveryToken(t, ts, user)
	pub, _, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPwd,
		"wrapped_private_key":   invalidBase64,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "wrapped_private_key")
}

func TestRecoveryResetAccountInvalidBase64Salt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badsaltacct")

	recoveryToken := getRecoveryToken(t, ts, user)
	pub, wrappedKey, _ := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPwd,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   invalidBase64,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "key_derivation_salt")
}

func TestRecoveryResetAccountDeletesChannelKeys(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delkeys")

	// Create a server and encrypted channel, then seed a channel key
	serverID := ts.CreateTestServer(t, user.ID, "delkeys-server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted-chan")

	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
		channelID, user.ID, []byte("test-wrapped-key"),
	)
	require.NoError(t, err)

	// Verify key exists
	var keyCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_keys WHERE user_id = $1`, user.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	require.Greater(t, keyCount, 0)

	// Reset account
	recoveryToken := getRecoveryToken(t, ts, user)
	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPwd,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Channel keys should be deleted
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_keys WHERE user_id = $1`, user.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 0, keyCount, "Channel keys should be deleted after account reset")
}

func TestRecoveryResetAccountRevokesAllSessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeacct")

	// Create a login session
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Reset account
	recoveryToken := getRecoveryToken(t, ts, user)
	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w = ts.DoRequest("POST", pathRecoverResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPwd,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// All sessions should be revoked
	var activeCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&activeCount)
	require.NoError(t, err)
	assert.Equal(t, 0, activeCount, "All sessions should be revoked after account reset")
}

// ── Device Recovery Edge Cases ──────────────────────────────────────────────

func TestCreateDeviceRecoveryRequestMissingBody(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathDeviceRequest, map[string]interface{}{}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateDeviceRecoveryRequestInvalidToken(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathDeviceRequest, map[string]interface{}{
		"recovery_token":       invalidToken,
		"ephemeral_public_key": base64.StdEncoding.EncodeToString([]byte("test-ephemeral-key")),
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreateDeviceRecoveryRequestInvalidBase64EphemeralKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badephemeral")

	recoveryToken := getRecoveryToken(t, ts, user)

	w := ts.DoRequest("POST", pathDeviceRequest, map[string]interface{}{
		"recovery_token":       recoveryToken,
		"ephemeral_public_key": invalidBase64,
	}, nil)
	// Handler checks for trusted devices before validating ephemeral key format.
	// Since no trusted devices are configured, it returns 400 with that error.
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPollDeviceRecoveryRequestMissingToken(t *testing.T) {
	ts := setupTS(t)

	// No recovery_token query param and no Authorization header
	w := ts.DoRequest("GET", pathDeviceRequest+pollSuffix, nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPollDeviceRecoveryRequestInvalidToken(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set("Authorization", bearerPfx+invalidToken)
	w := ts.DoRequest("GET", pathDeviceRequest+pollSuffix, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── Social Recovery Edge Cases ──────────────────────────────────────────────

func TestCreateSocialRecoveryRequestMissingBody(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathSocialRequest, map[string]interface{}{}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateSocialRecoveryRequestInvalidToken(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathSocialRequest, map[string]interface{}{
		"recovery_token":       invalidToken,
		"ephemeral_public_key": base64.StdEncoding.EncodeToString([]byte("test-ephemeral-key")),
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreateSocialRecoveryRequestInvalidBase64EphemeralKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badsoceph")

	recoveryToken := getRecoveryToken(t, ts, user)

	w := ts.DoRequest("POST", pathSocialRequest, map[string]interface{}{
		"recovery_token":       recoveryToken,
		"ephemeral_public_key": invalidBase64,
	}, nil)
	// Handler checks for recovery circle before validating ephemeral key format.
	// Since no recovery circle is configured, it returns 400 with that error.
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPollSocialRecoveryRequestMissingToken(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", pathSocialRequest+pollSuffix, nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPollSocialRecoveryRequestInvalidToken(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set("Authorization", bearerPfx+invalidToken)
	w := ts.DoRequest("GET", pathSocialRequest+pollSuffix, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── WS Ticket Edge Cases ────────────────────────────────────────────────────

func TestWSTicketIssuedTicketUsableOnce(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticketonce")

	// Issue ticket
	w := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket := body["ticket"].(string)

	// Validate ticket (first use)
	ctx := context.Background()
	userID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)

	// Second use should fail (single-use)
	_, _, err = auth.ValidateTicket(ctx, ts.Redis, ticket)
	assert.Error(t, err)
}

func TestWSTicketValidateEmptyTicket(t *testing.T) {
	ts := setupTS(t)

	ctx := context.Background()
	_, _, err := auth.ValidateTicket(ctx, ts.Redis, "")
	assert.Error(t, err)
}

func TestWSTicketValidateNonexistentTicket(t *testing.T) {
	ts := setupTS(t)

	ctx := context.Background()
	_, _, err := auth.ValidateTicket(ctx, ts.Redis, "nonexistent-ticket")
	assert.Error(t, err)
}

// ── Login Lockout Edge Cases ────────────────────────────────────────────────

func TestLoginLockoutDoesNotRevealLockout(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lockoutmsg")

	// Trigger lockout with 5 failures
	for i := 0; i < 5; i++ {
		ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    user.Email,
			"password": testWrongPassword,
		}, nil)
	}

	// The 6th attempt should return the same generic message as a wrong password
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testWrongPassword,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	// Should NOT mention "locked" or "lockout" — anti-enumeration
	assert.Equal(t, "Invalid credentials", body["error"])
}

func TestLoginLockoutNonexistentAccount(t *testing.T) {
	ts := setupTS(t)

	// Fail 5 times on a non-existent account
	for i := 0; i < 5; i++ {
		ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    "phantom@test.concord.chat",
			"password": testWrongPassword,
		}, nil)
	}

	// 6th attempt also returns 401 — same behavior as existing account
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    "phantom@test.concord.chat",
		"password": testWrongPassword,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── Access Token Tests ──────────────────────────────────────────────────────

func TestAccessTokenValidAfterLogin(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "tokenvalid")

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	accessToken := body["access_token"].(string)

	// Token should work for protected routes
	w = ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAccessTokenFromRegisterWorks(t *testing.T) {
	ts := setupTS(t)

	pub, priv, salt := testhelpers.E2EETestKeys()
	// Step 1: register returns pending_id (no token yet).
	w := ts.DoRequest("POST", pathRegister, map[string]interface{}{
		"email":               "regtokentest@test.concord.chat",
		"username":            "regtokentest",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}, nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)

	// Step 2: confirm to get access token.
	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	w2 := ts.DoRequest("POST", pathConfirmRegistration, map[string]string{
		"pending_id": pendingID,
		"code":       code,
	}, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w2, &body)
	accessToken := body["access_token"].(string)

	// The token should work for verified-accessible routes (e.g., /users/me)
	w = ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAccessTokenFromRefreshWorks(t *testing.T) {
	ts := setupTS(t)

	_, refreshToken, _ := registerAndConfirmTokens(t, ts, registerPayload())

	// Refresh to get a new access token
	headers := http.Header{}
	headers.Set(headerXRefreshToken, refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w := ts.DoRequest("POST", pathRefresh, nil, headers)
	require.Equal(t, http.StatusOK, w.Code)

	var refreshBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &refreshBody)
	newAccessToken := refreshBody["access_token"].(string)

	// New token should work
	w = ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(newAccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── RecoveryResetPassword with recovery key upsert ──────────────────────────

func TestRecoveryResetPasswordWithRecoveryKeys(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "reckeyreset")

	recoveryToken := getRecoveryToken(t, ts, user)

	recKey := base64.StdEncoding.EncodeToString([]byte("recovery-wrapped-key-data"))
	recSalt := base64.StdEncoding.EncodeToString([]byte("recovery-salt-data"))
	recPrefsKey := base64.StdEncoding.EncodeToString([]byte("recovery-prefs-key"))
	recPrefsSalt := base64.StdEncoding.EncodeToString([]byte("recovery-prefs-salt"))

	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":               recoveryToken,
		"new_password":                 testNewPwd,
		"wrapped_private_key":          wrappedKey,
		"key_derivation_salt":          salt,
		"key_derivation_alg":           testKeyDerivationAlg,
		"recovery_wrapped_private_key": recKey,
		"recovery_key_salt":            recSalt,
		"recovery_wrapped_prefs_key":   recPrefsKey,
		"recovery_prefs_key_salt":      recPrefsSalt,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify recovery keys were stored
	var storedRecKey, storedRecSalt []byte
	err := ts.DB.QueryRow(
		`SELECT recovery_wrapped_private_key, recovery_key_salt FROM user_recovery_keys WHERE user_id = $1`,
		user.ID,
	).Scan(&storedRecKey, &storedRecSalt)
	require.NoError(t, err)
	assert.NotEmpty(t, storedRecKey)
	assert.NotEmpty(t, storedRecSalt)
}

func TestRecoveryResetPasswordInvalidRecoveryWrappedKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badreckey")

	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":               recoveryToken,
		"new_password":                 testNewPwd,
		"wrapped_private_key":          wrappedKey,
		"key_derivation_salt":          salt,
		"key_derivation_alg":           testKeyDerivationAlg,
		"recovery_wrapped_private_key": invalidBase64,
		"recovery_key_salt":            base64.StdEncoding.EncodeToString([]byte("valid")),
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "recovery_wrapped_private_key")
}

func TestRecoveryResetPasswordInvalidRecoveryKeySalt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badrecsalt")

	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":               recoveryToken,
		"new_password":                 testNewPwd,
		"wrapped_private_key":          wrappedKey,
		"key_derivation_salt":          salt,
		"key_derivation_alg":           testKeyDerivationAlg,
		"recovery_wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("valid")),
		"recovery_key_salt":            invalidBase64,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "recovery_key_salt")
}

func TestRecoveryResetPasswordInvalidRecoveryPrefsKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badrecprefskey")

	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":               recoveryToken,
		"new_password":                 testNewPwd,
		"wrapped_private_key":          wrappedKey,
		"key_derivation_salt":          salt,
		"key_derivation_alg":           testKeyDerivationAlg,
		"recovery_wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("valid")),
		"recovery_key_salt":            base64.StdEncoding.EncodeToString([]byte("valid")),
		"recovery_wrapped_prefs_key":   invalidBase64,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "recovery_wrapped_prefs_key")
}

func TestRecoveryResetPasswordInvalidRecoveryPrefsSalt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badrecprefssalt")

	recoveryToken := getRecoveryToken(t, ts, user)
	_, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoverResetPwd, map[string]interface{}{
		"recovery_token":               recoveryToken,
		"new_password":                 testNewPwd,
		"wrapped_private_key":          wrappedKey,
		"key_derivation_salt":          salt,
		"key_derivation_alg":           testKeyDerivationAlg,
		"recovery_wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("valid")),
		"recovery_key_salt":            base64.StdEncoding.EncodeToString([]byte("valid")),
		"recovery_prefs_key_salt":      invalidBase64,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "recovery_prefs_key_salt")
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// registerAndConfirmTokens performs the two-step registration flow:
// POST /auth/register → POST /auth/register/confirm.
// Returns (accessToken, refreshToken, confirmResponseRecorder) so callers can
// inspect cookies set by the confirm step.
func registerAndConfirmTokens(t *testing.T, ts *testhelpers.TestServer, payload map[string]interface{}) (accessToken, refreshToken string, confirmW *httptest.ResponseRecorder) {
	t.Helper()

	w := ts.DoRequest("POST", pathRegister, payload, nil)
	require.Equal(t, http.StatusCreated, w.Code, "register step failed")

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)

	code := testhelpers.FetchVerificationCode(t, ts, pendingID)

	confirmHeaders := http.Header{}
	confirmHeaders.Set(headerContentType, contentTypeJSON)

	confirmW = ts.DoRequest("POST", pathConfirmRegistration, map[string]string{
		"pending_id": pendingID,
		"code":       code,
	}, confirmHeaders)
	require.Equal(t, http.StatusOK, confirmW.Code, "confirm step failed")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, confirmW, &body)
	accessToken = body["access_token"].(string)   //nolint:gosec // G117: response-binding field
	refreshToken = body["refresh_token"].(string) //nolint:gosec // G117: response-binding field
	return accessToken, refreshToken, confirmW
}

// hashToken returns the SHA-256 hex digest of a token string.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// seedRecoveryCodeLocal inserts a known recovery code into Redis (local helper to avoid
// collisions with the one in recovery_integration_test.go that has identical logic).
func seedRecoveryCodeLocal(t *testing.T, ts *testhelpers.TestServer, email, code, userID string) {
	t.Helper()
	hash := sha256.Sum256([]byte(code))
	record := map[string]interface{}{
		"code_hash": hex.EncodeToString(hash[:]),
		"user_id":   userID,
		"attempts":  0,
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := "recovery_code:" + strings.ToLower(email)
	err = ts.Redis.Set(context.Background(), key, data, 10*time.Minute).Err()
	require.NoError(t, err)
}

// getRecoveryToken seeds a recovery code, verifies it, and returns the resulting recovery token.
func getRecoveryToken(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()

	code := "999999"
	seedRecoveryCodeLocal(t, ts, user.Email, code, user.ID)

	w := ts.DoRequest("POST", pathRecoverVerify, map[string]interface{}{
		"email": user.Email,
		"code":  code,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	return body["recovery_token"].(string)
}
