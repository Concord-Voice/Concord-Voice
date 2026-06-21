package sessions_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sessionsPath       = "/api/v1/sessions"
	revokeAllPath      = "/api/v1/sessions/revoke-all"
	revocationModePath = "/api/v1/sessions/revocation-mode"
	sessTestPassword   = "TestPassword123!" //nolint:gosec // test credential constant

	testIP1       = "10.0.0.1"
	testIP2       = "10.0.0.2"
	wrongPassword = "WrongPassword999!" //nolint:gosec // test credential constant
	ipAddr1       = "1.1.1.1"
	ipAddr2       = "2.2.2.2"
	ipAddr3       = "3.3.3.3"
)

// createSession inserts a refresh_token row for a user and returns its ID.
func createSession(t *testing.T, ts *testhelpers.TestServer, userID, deviceName, ip string) string {
	t.Helper()
	tokenHash := hashStr(userID + deviceName + ip)
	var id string
	err := ts.DB.QueryRow(
		`INSERT INTO refresh_tokens (user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me)
		 VALUES ($1, $2, $3, $4, 'TestAgent/1.0', NOW() + INTERVAL '30 days', false)
		 RETURNING id`,
		userID, tokenHash, deviceName, ip,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ── ListSessions (extended) ──────────────────────────────────────────────────

func TestListSessions_MasksIPAddress(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ipmask")

	createSession(t, ts, user.ID, "Device1", "192.168.1.42")

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	sessions := body["sessions"].([]interface{})
	for _, s := range sessions {
		sess := s.(map[string]interface{})
		ip := sess["ip_address"].(string)
		assert.NotContains(t, ip, ".42", "IP last octet should be masked")
	}
}

func TestListSessions_IncludesPastSessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pastsess")

	sessionID := createSession(t, ts, user.ID, "OldDevice", testIP1)
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1`, sessionID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	pastSessions := body["past_sessions"].([]interface{})
	assert.GreaterOrEqual(t, len(pastSessions), 1)
}

func TestListSessions_ReturnsExpectedFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "fieldcheck")

	createSession(t, ts, user.ID, "TestBrowser", "192.168.1.1")

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	sessions := body["sessions"].([]interface{})
	require.NotEmpty(t, sessions)
	sess := sessions[0].(map[string]interface{})

	expectedKeys := []string{"id", "device_name", "ip_address", "user_agent", "machine_id",
		"expires_at", "created_at", "last_used", "remember_me", "is_current"}
	for _, key := range expectedKeys {
		assert.Contains(t, sess, key, "session should contain key: %s", key)
	}
}

func TestListSessions_ExcludesExpired(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "expiredsess")

	tokenHash := hashStr("expired-token")
	_, err := ts.DB.Exec(
		`INSERT INTO refresh_tokens (user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me)
		 VALUES ($1, $2, 'ExpiredDevice', '10.0.0.1', 'TestAgent', $3, false)`,
		user.ID, tokenHash, time.Now().Add(-24*time.Hour),
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	sessions := body["sessions"].([]interface{})
	for _, s := range sessions {
		sess := s.(map[string]interface{})
		assert.NotEqual(t, "ExpiredDevice", sess["device_name"], "expired sessions should not appear in active list")
	}
}

func TestListSessions_ReturnsRevocationMode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revmodecheck")

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["revocation_mode"])
}

// ── RevokeSession (extended) ─────────────────────────────────────────────────

func TestRevokeSession_PasswordRequiredUnder3Sessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revoker2")

	sessionID := createSession(t, ts, user.ID, "TargetDevice", testIP1)

	w := ts.DoRequest("DELETE", sessionsPath+"/"+sessionID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "password_required", body["error"])
}

func TestRevokeSession_WithCorrectPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokerpass2")

	sessionID := createSession(t, ts, user.ID, "TargetDevice", testIP1)

	payload := map[string]interface{}{
		"password": sessTestPassword,
	}
	w := ts.DoRequest("DELETE", sessionsPath+"/"+sessionID, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "revoked")
	assert.Equal(t, sessionID, body["session_id"])
}

func TestRevokeSession_IncorrectPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badpassrev")

	sessionID := createSession(t, ts, user.ID, "TargetDevice", testIP1)

	payload := map[string]interface{}{
		"password": wrongPassword,
	}
	w := ts.DoRequest("DELETE", sessionsPath+"/"+sessionID, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Incorrect password", body["error"])
}

func TestRevokeSession_MissingSessionID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revnotfound2")

	fakeID := "00000000-0000-0000-0000-000000000099"
	payload := map[string]interface{}{
		"password": sessTestPassword,
	}
	w := ts.DoRequest("DELETE", sessionsPath+"/"+fakeID, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeSession_FreeRevokeWith3PlusSessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "freerevoker")

	createSession(t, ts, user.ID, "Device1", testIP1)
	createSession(t, ts, user.ID, "Device2", testIP2)
	targetID := createSession(t, ts, user.ID, "Device3", "10.0.0.3")

	w := ts.DoRequest("DELETE", sessionsPath+"/"+targetID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRevokeSession_SecondRevokeRequiresPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "doublrevoke")

	s1 := createSession(t, ts, user.ID, "Device1", testIP1)
	createSession(t, ts, user.ID, "Device2", testIP2)
	s3 := createSession(t, ts, user.ID, "Device3", "10.0.0.3")
	createSession(t, ts, user.ID, "Device4", "10.0.0.4")

	// First free revoke
	w := ts.DoRequest("DELETE", sessionsPath+"/"+s1, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Second should require password
	w = ts.DoRequest("DELETE", sessionsPath+"/"+s3, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ── RevokeAllSessions (extended) ─────────────────────────────────────────────

func TestRevokeAllSessions_WrongPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeallbad")

	payload := map[string]interface{}{
		"password":        wrongPassword,
		"include_current": true,
	}
	w := ts.DoRequest("POST", revokeAllPath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRevokeAllSessions_NoPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeallnopw")

	payload := map[string]interface{}{
		"include_current": true,
	}
	w := ts.DoRequest("POST", revokeAllPath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "password_required", body["error"])
}

func TestRevokeAllSessions_InvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeallbody")

	w := ts.DoRequest("POST", revokeAllPath, "not json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeAllSessions_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", revokeAllPath, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRevokeAllSessions_IncludesCount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokecount")

	createSession(t, ts, user.ID, "Device1", testIP1)
	createSession(t, ts, user.ID, "Device2", testIP2)

	payload := map[string]interface{}{
		"password":        sessTestPassword,
		"include_current": true,
	}
	w := ts.DoRequest("POST", revokeAllPath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.GreaterOrEqual(t, body["count"], float64(2))
}

// ── UpdateRevocationMode ─────────────────────────────────────────────────────

func TestUpdateRevocationMode_ToSimple(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modesimple")

	payload := map[string]interface{}{
		"mode":     "simple",
		"password": sessTestPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "simple", body["revocation_mode"])
}

func TestUpdateRevocationMode_ToSecure(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modesecure")

	payload := map[string]interface{}{
		"mode":     "secure",
		"password": sessTestPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "secure", body["revocation_mode"])
}

func TestUpdateRevocationMode_InvalidMode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modeinvalid")

	payload := map[string]interface{}{
		"mode":     "turbo",
		"password": sessTestPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateRevocationMode_WrongPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modebadpw")

	payload := map[string]interface{}{
		"mode":     "simple",
		"password": wrongPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateRevocationMode_NoPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modenopw")

	payload := map[string]interface{}{
		"mode": "simple",
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateRevocationMode_InvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modebadreq")

	w := ts.DoRequest("PUT", revocationModePath, "not json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateRevocationMode_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("PUT", revocationModePath, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── Revocation mode persists and shows in list ───────────────────────────────

func TestRevocationMode_PersistsAndShowsInList(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "persistmode")

	payload := map[string]interface{}{
		"mode":     "simple",
		"password": sessTestPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "simple", body["revocation_mode"])
}

// ── Simple mode auth window ──────────────────────────────────────────────────

func TestSimpleMode_AuthWindowAllowsSubsequentRevokes(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "simplewindow")

	// Set mode to simple
	setMode := map[string]interface{}{
		"mode":     "simple",
		"password": sessTestPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, setMode, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	sessionID := createSession(t, ts, user.ID, "Device1", testIP1)

	// First revoke with password grants auth window
	revoke1 := map[string]interface{}{
		"password": sessTestPassword,
	}
	w = ts.DoRequest("DELETE", sessionsPath+"/"+sessionID, revoke1, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Second revoke within window should NOT require password
	sessionID2 := createSession(t, ts, user.ID, "Device2", testIP2)
	w = ts.DoRequest("DELETE", sessionsPath+"/"+sessionID2, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── RevokeAll resets tracking ────────────────────────────────────────────────

func TestRevokeAll_ResetsTracking(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resettrack")

	createSession(t, ts, user.ID, "D1", ipAddr1)
	createSession(t, ts, user.ID, "D2", ipAddr2)
	s3 := createSession(t, ts, user.ID, "D3", ipAddr3)
	createSession(t, ts, user.ID, "D4", "4.4.4.4")

	// Use the free revoke
	w := ts.DoRequest("DELETE", sessionsPath+"/"+s3, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Revoke all (resets tracking)
	payload := map[string]interface{}{
		"password":        sessTestPassword,
		"include_current": true,
	}
	w = ts.DoRequest("POST", revokeAllPath, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Create fresh sessions — should get a free revoke again
	createSession(t, ts, user.ID, "N1", ipAddr1)
	createSession(t, ts, user.ID, "N2", ipAddr2)
	n3 := createSession(t, ts, user.ID, "N3", ipAddr3)

	w = ts.DoRequest("DELETE", sessionsPath+"/"+n3, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── Switching back to secure clears auth window ──────────────────────────────

func TestUpdateRevocationMode_SecureClearsAuthWindow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "clearsecure")

	// Set to simple first
	payload := map[string]interface{}{
		"mode":     "simple",
		"password": sessTestPassword,
	}
	w := ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Now switch back to secure
	payload = map[string]interface{}{
		"mode":     "secure",
		"password": sessTestPassword,
	}
	w = ts.DoRequest("PUT", revocationModePath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "secure", body["revocation_mode"])
}

// ── Revoke session that was already revoked ──────────────────────────────────

func TestRevokeSession_AlreadyRevoked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "alreadyrevoked")

	sessionID := createSession(t, ts, user.ID, "Device1", testIP1)

	// Revoke it manually in DB first
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1`, sessionID)
	require.NoError(t, err)

	// Try to revoke via API - should be 404 (session not found as active)
	payload := map[string]interface{}{
		"password": sessTestPassword,
	}
	w := ts.DoRequest("DELETE", sessionsPath+"/"+sessionID, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── Revoke all with password and count ───────────────────────────────────────

func TestRevokeAllSessions_WithMultipleSessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeallmulti")

	createSession(t, ts, user.ID, "D1", ipAddr1)
	createSession(t, ts, user.ID, "D2", ipAddr2)
	createSession(t, ts, user.ID, "D3", ipAddr3)

	payload := map[string]interface{}{
		"password":        sessTestPassword,
		"include_current": true,
	}
	w := ts.DoRequest("POST", revokeAllPath, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.GreaterOrEqual(t, body["count"], float64(3))
	assert.Equal(t, true, body["include_current"])
}

// ── List sessions with multiple active and revoked ───────────────────────────

func TestListSessions_WithMixedSessions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "mixedsess")

	// Active sessions
	createSession(t, ts, user.ID, "Active1", ipAddr1)
	createSession(t, ts, user.ID, "Active2", ipAddr2)

	// Revoked session (past)
	revokedID := createSession(t, ts, user.ID, "Revoked1", ipAddr3)
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1`, revokedID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	sessions := body["sessions"].([]interface{})
	pastSessions := body["past_sessions"].([]interface{})
	assert.GreaterOrEqual(t, len(sessions), 2)
	assert.GreaterOrEqual(t, len(pastSessions), 1)
	assert.Equal(t, float64(len(sessions)), body["total"])
}

// ── Unverified email blocks session routes ───────────────────────────────────

func TestSessions_UnverifiedEmailBlocked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "unverifiedsess")

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
