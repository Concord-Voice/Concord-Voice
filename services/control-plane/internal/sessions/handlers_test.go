package sessions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// Helper: register a user via the two-step register→confirm flow, and return access token + refresh cookie.
func registerAndGetTokens(t *testing.T, ts *testhelpers.TestServer, email, username string) (string, *http.Cookie) {
	t.Helper()
	pub, priv, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":               email,
		"username":            username,
		"password":            "TestPassword123!",
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}, nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var regBody struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &regBody)

	code := testhelpers.FetchVerificationCode(t, ts, regBody.PendingID)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": regBody.PendingID, "code": code}, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var confirmBody struct {
		AccessToken string `json:"access_token"` //nolint:gosec
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	testhelpers.ParseJSON(t, w2, &confirmBody)

	var refreshCookie *http.Cookie
	for _, c := range w2.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	return confirmBody.AccessToken, refreshCookie
}

// --- List Sessions ---

func TestListSessionsSuccess(t *testing.T) {
	ts := setupTS(t)
	accessToken, refreshCookie := registerAndGetTokens(t, ts, "sessions1@test.concord.chat", "sessions1")

	// Build request with both auth header and refresh cookie
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if refreshCookie != nil {
		req.AddCookie(refreshCookie)
	}

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	sessions := testhelpers.JSONField[[]interface{}](t, body, "sessions")
	assert.GreaterOrEqual(t, len(sessions), 1)
	assert.NotNil(t, body["total"])
}

func TestListSessionsUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/sessions", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Revoke Session ---

func TestRevokeSessionSuccess(t *testing.T) {
	ts := setupTS(t)
	accessToken, refreshCookie := registerAndGetTokens(t, ts, "revoke1@test.concord.chat", "revoke1")

	// List sessions to get the session ID
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if refreshCookie != nil {
		req.AddCookie(refreshCookie)
	}
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)
	require.Equal(t, http.StatusOK, rw.Code)

	var listBody map[string]interface{}
	testhelpers.ParseJSON(t, rw, &listBody)
	sessions := testhelpers.JSONField[[]interface{}](t, listBody, "sessions")
	require.GreaterOrEqual(t, len(sessions), 1)
	sessionID := testhelpers.JSONField[string](t, testhelpers.JSONElem[map[string]interface{}](t, sessions, 0), "id")

	// Revoke that session (password required when < 3 active sessions)
	w := ts.DoRequest("DELETE", "/api/v1/sessions/"+sessionID, map[string]interface{}{
		"password": "TestPassword123!",
	}, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Session revoked successfully", body["message"])
}

func TestRevokeSessionNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokenotfound")

	w := ts.DoRequest("DELETE", "/api/v1/sessions/00000000-0000-0000-0000-000000000000", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Revoke All Sessions ---

func TestRevokeAllSessionsSuccess(t *testing.T) {
	ts := setupTS(t)
	accessToken, _ := registerAndGetTokens(t, ts, "revokeall@test.concord.chat", "revokeall")

	w := ts.DoRequest("POST", "/api/v1/sessions/revoke-all", map[string]interface{}{
		"password":        "TestPassword123!",
		"include_current": true,
	}, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "All sessions revoked successfully", body["message"])
	assert.Equal(t, true, body["include_current"])
}

func TestRevokeAllSessionsExceptCurrent(t *testing.T) {
	ts := setupTS(t)
	accessToken, refreshCookie := registerAndGetTokens(t, ts, "revokeother@test.concord.chat", "revokeother")

	// The bearer SID identifies the family to preserve.
	jsonBytes, _ := json.Marshal(map[string]interface{}{
		"password": "TestPassword123!",
	})
	req := httptest.NewRequest("POST", "/api/v1/sessions/revoke-all", bytes.NewReader(jsonBytes))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	if refreshCookie != nil {
		req.AddCookie(refreshCookie)
	}

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	assert.Equal(t, "All other sessions revoked successfully", body["message"])
}

func TestRevokeAllSessionsExceptCurrent_UsesBearerSessionWithoutCookie(t *testing.T) {
	ts := setupTS(t)
	accessToken, _ := registerAndGetTokens(t, ts, "revokeotherbearer@test.concord.chat", "revokeotherbearer")
	claims, err := auth.ValidateAccessToken(accessToken, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	userID, currentSessionID := claims.UserID, claims.SessionID
	require.NotEmpty(t, currentSessionID)
	otherSessionID := createSession(t, ts, userID, "OtherDevice", testIP2)

	w := ts.DoRequest("POST", "/api/v1/sessions/revoke-all", map[string]interface{}{
		"password": "TestPassword123!",
	}, testhelpers.AuthHeaders(accessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var currentActive, otherActive int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE id = $1 AND revoked_at IS NULL`, currentSessionID).Scan(&currentActive))
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE id = $1 AND revoked_at IS NULL`, otherSessionID).Scan(&otherActive))
	assert.Equal(t, 1, currentActive, "the bearer-authenticated session must be preserved without a refresh cookie")
	assert.Zero(t, otherActive, "all non-current sessions must be revoked")
	assertSessionFamilyLive(t, ts, userID, currentSessionID)
	assertSessionFamilyRevoked(t, ts, userID, otherSessionID)
}

func TestRevokeAllSessionsExceptCurrent_StaleBearerPreservesCurrentCookieSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeallstalebearer")

	loginW := ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	require.Equal(t, http.StatusOK, loginW.Code)
	var loginBody struct {
		AccessToken string `json:"access_token"` //nolint:gosec // G117: response-binding field
		SessionID   string `json:"session_id"`
	}
	testhelpers.ParseJSON(t, loginW, &loginBody)
	require.NotEmpty(t, loginBody.AccessToken)
	require.NotEmpty(t, loginBody.SessionID)
	var refreshCookieA *http.Cookie
	for _, cookie := range loginW.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			refreshCookieA = cookie
			break
		}
	}
	require.NotNil(t, refreshCookieA)

	refreshReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	refreshReq.AddCookie(refreshCookieA)
	refreshW := httptest.NewRecorder()
	ts.Router.ServeHTTP(refreshW, refreshReq)
	require.Equal(t, http.StatusOK, refreshW.Code)
	var refreshBody struct {
		SessionID string `json:"session_id"`
	}
	testhelpers.ParseJSON(t, refreshW, &refreshBody)
	require.NotEmpty(t, refreshBody.SessionID)
	require.NotEqual(t, loginBody.SessionID, refreshBody.SessionID)
	var refreshCookieB *http.Cookie
	for _, cookie := range refreshW.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			refreshCookieB = cookie
			break
		}
	}
	require.NotNil(t, refreshCookieB)

	// Both predecessor and successor SID retain immutable same-family authority.
	var sameFamily bool
	require.NoError(t, ts.DB.QueryRow(`SELECT a.family_id=b.family_id FROM login_family_sids a JOIN login_family_sids b ON b.sid=$2 WHERE a.sid=$1`, loginBody.SessionID, refreshBody.SessionID).Scan(&sameFamily))
	require.True(t, sameFamily)
	// Vary the cookie independently: neither a rotated cookie nor another login's
	// valid cookie may choose the family preserved by the old bearer.
	for _, cookie := range []*http.Cookie{refreshCookieA, refreshCookieB, nil} {
		otherSessionID := createSession(t, ts, user.ID, "OtherDevice", testIP2)
		var cookieSID string
		if cookie == nil {
			h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
			_, refresh, sid, err := h.IssueAccessAndRefresh(context.Background(), user.ID)
			require.NoError(t, err)
			cookieSID = sid
			cookie = &http.Cookie{Name: "refresh_token", Value: refresh, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
		}
		body, err := json.Marshal(map[string]interface{}{"password": testhelpers.TestAuthPlaintext})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, revokeAllPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+loginBody.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertSessionFamilyLive(t, ts, user.ID, refreshBody.SessionID)
		assertSessionFamilyRevoked(t, ts, user.ID, otherSessionID)
		if cookieSID != "" {
			assertSessionFamilyRevoked(t, ts, user.ID, cookieSID)
		}
	}
}

func TestRevokeAllSessionsExceptCurrent_WithoutSessionIdentityFailsSafely(t *testing.T) {
	ts := setupTS(t)
	accessToken, _ := registerAndGetTokens(t, ts, "revokeotherlegacy@test.concord.chat", "revokeotherlegacy")
	claims, err := auth.ValidateAccessToken(accessToken, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	legacyAccessToken, err := auth.GenerateAccessToken(claims.UserID, testhelpers.TestJWTSecret, true, "", "")
	require.NoError(t, err)

	w := ts.DoRequest("POST", revokeAllPath, map[string]interface{}{
		"password": "TestPassword123!",
	}, testhelpers.AuthHeaders(legacyAccessToken))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Authentication required", body["error"])
	var operations int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, claims.UserID).Scan(&operations))
	assert.Zero(t, operations)
	assert.Empty(t, w.Result().Cookies(), "rejected admission must not clear a refresh cookie")

	assertSessionFamilyLive(t, ts, claims.UserID, claims.SessionID)
	assertSessionCounts(t, ts, claims.UserID, 1, 0)
	var active int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, claims.UserID,
	).Scan(&active))
	assert.Equal(t, 1, active, "a request without a current-session identity must not revoke any session")
}

func TestRevokeAllSessions_EpochAdvanceAfterAuthenticationReturnsUnauthorized(t *testing.T) {
	ts := setupTS(t)
	accessToken, _ := registerAndGetTokens(t, ts, "revokeepoch@test.concord.chat", "revokeepoch")
	claims, err := auth.ValidateAccessToken(accessToken, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	admitted := false

	router := gin.New()
	router.Use(middleware.AuthRequired(testhelpers.TestJWTSecret, ts.Redis, ts.DB, nil))
	router.Use(func(c *gin.Context) {
		admitted = true
		if _, updateErr := ts.DB.Exec(`UPDATE users SET credential_epoch = '0123456789abcdef0123456789abcdef' WHERE id = $1`, claims.UserID); updateErr != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	})
	router.POST(revokeAllPath, sessions.NewHandler(ts.DB, ts.Redis, logger.New("test"), nil, nil).RevokeAllSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, revokeAllPath, bytes.NewBufferString(`{"password":"TestPassword123!","include_current":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, admitted, "real family must pass AuthRequired before epoch advance")
	assertSessionFamilyLive(t, ts, claims.UserID, claims.SessionID)
	assertSessionCounts(t, ts, claims.UserID, 1, 0)
	var operations int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, claims.UserID).Scan(&operations))
	assert.Zero(t, operations)
}
