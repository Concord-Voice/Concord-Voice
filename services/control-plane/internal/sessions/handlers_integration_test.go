package sessions_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
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

const sessionsRowsDriverName = "sessions-list-past-error-test"

var sessionsRowsDriverOnce sync.Once

var errPastSessionsRows = errors.New("forced past sessions rows error")

type sessionsRowsDriver struct{}

func (sessionsRowsDriver) Open(string) (driver.Conn, error) { return sessionsRowsConn{}, nil }

type sessionsRowsConn struct{}

func (sessionsRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (sessionsRowsConn) Close() error { return nil }
func (sessionsRowsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}
func (sessionsRowsConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "revoked_at IS NOT NULL") {
		return sessionsPastErrorRows{}, nil
	}
	return sessionsEmptyRows{}, nil
}

type sessionsEmptyRows struct{}

func (sessionsEmptyRows) Columns() []string         { return []string{"id"} }
func (sessionsEmptyRows) Close() error              { return nil }
func (sessionsEmptyRows) Next([]driver.Value) error { return io.EOF }

type sessionsPastErrorRows struct{}

func (sessionsPastErrorRows) Columns() []string {
	return []string{"id", "device_name", "ip_address", "user_agent", "created_at", "last_used_at", "revoked_at"}
}
func (sessionsPastErrorRows) Close() error              { return nil }
func (sessionsPastErrorRows) Next([]driver.Value) error { return errPastSessionsRows }

func openSessionsRowsErrorDB(t *testing.T) *sql.DB {
	t.Helper()
	sessionsRowsDriverOnce.Do(func() { sql.Register(sessionsRowsDriverName, sessionsRowsDriver{}) })
	db, err := sql.Open(sessionsRowsDriverName, "past-error")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestListSessions_PastRowsErrorReturns500(t *testing.T) {
	db := openSessionsRowsErrorDB(t)
	h := sessions.NewHandler(db, nil, logger.New("test"), nil, nil)
	r := gin.New()
	r.GET(sessionsPath, func(c *gin.Context) {
		c.Set("user_id", "00000000-0000-0000-0000-000000000001")
		h.ListSessions(c)
	})

	req := httptest.NewRequest(http.MethodGet, sessionsPath, nil)
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)

	require.Equal(t, http.StatusInternalServerError, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	assert.Equal(t, "Failed to fetch sessions", testhelpers.JSONField[string](t, body, "error"))
}

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

// createNullMetadataSession inserts a refresh_tokens row shaped exactly like the
// SSO adapter's: device_name, ip_address and user_agent omitted, therefore NULL.
// It asserts that premise, because without it these tests prove nothing.
func createNullMetadataSession(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	var id string
	err := ts.DB.QueryRow(
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, remember_me)
		 VALUES ($1, $2, NOW() + INTERVAL '30 days', true)
		 RETURNING id`,
		userID, hashStr(userID+"sso-null-metadata"),
	).Scan(&id)
	require.NoError(t, err)

	var nullCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT (device_name IS NULL)::int + (ip_address IS NULL)::int + (user_agent IS NULL)::int
		 FROM refresh_tokens WHERE id = $1`, id).Scan(&nullCount))
	require.Equal(t, 3, nullCount,
		"premise: the fixture must reproduce the SSO adapter's all-NULL insert")
	return id
}

// regression for #3290
//
// An SSO session was silently dropped from this list by a swallowed scan error
// while the endpoint still returned 200 — so the user could neither see it nor
// revoke it. A session you cannot see is a session you cannot revoke.
func TestListSessions_IncludesNullMetadataSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssolist")
	id := createNullMetadataSession(t, ts, user.ID)

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	sessions := testhelpers.JSONField[[]interface{}](t, body, "sessions")

	var found map[string]interface{}
	for i := range sessions {
		sess := testhelpers.JSONElem[map[string]interface{}](t, sessions, i)
		if sess["id"] == id {
			found = sess
		}
	}
	require.NotNil(t, found,
		"a session with absent device metadata must still be listed, or it cannot be revoked")
	assert.Equal(t, "", found["device_name"])
	assert.Equal(t, "unknown", found["ip_address"], "absent IP renders as unknown, not as a value")
	assert.Equal(t, "", found["user_agent"])
}

// regression for #3290
func TestPastSessions_IncludesNullMetadataSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssopast")
	id := createNullMetadataSession(t, ts, user.ID)
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1`, id)
	require.NoError(t, err)

	w := ts.DoRequest("GET", sessionsPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	past := testhelpers.JSONField[[]interface{}](t, body, "past_sessions")

	ids := make([]interface{}, 0, len(past))
	for i := range past {
		sess := testhelpers.JSONElem[map[string]interface{}](t, past, i)
		ids = append(ids, sess["id"])
	}
	assert.Contains(t, ids, id, "a revoked session with absent metadata must appear in history")
}

// regression for #3290: the session returned by the SSO adapter must remain
// reachable through the same authorized revoke endpoint as any other session.
func TestRevokeSession_NullMetadataSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokenull")
	id := createNullMetadataSession(t, ts, user.ID)

	w := ts.DoRequest("DELETE", sessionsPath+"/"+id,
		map[string]interface{}{"password": sessTestPassword},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var revokedAt time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE id = $1`, id).Scan(&revokedAt))
	assert.False(t, revokedAt.IsZero(), "authorized revoke must persist revocation for the NULL-metadata session")
}

// regression for #3290
//
// The fixture above hard-codes three column names. This derives the real set
// from the live schema so a NEW nullable-no-default column on refresh_tokens —
// the exact shape an omitting INSERT can produce — forces a deliberate
// DISPLAY-or-EQUALITY decision instead of silently going untested.
func TestRefreshTokensNullableColumns(t *testing.T) {
	ts := setupTS(t)
	cols := testhelpers.NullableNoDefaultColumns(t, ts.DB, "refresh_tokens")

	require.NotEmpty(t, cols, "introspection returned nothing — the query or table name is wrong")
	for _, want := range []string{"device_name", "ip_address", "user_agent"} {
		assert.Contains(t, cols, want,
			"a rename or query typo must fail loudly here, not silently narrow the fixture")
	}
	assert.Len(t, cols, 6,
		"a new nullable-no-default column on refresh_tokens needs a DISPLAY-or-EQUALITY "+
			"ruling — see signalMatch in internal/auth/signal.go. Do not edit this number "+
			"to match; find out which column appeared and rule on it.")
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
	sessions := testhelpers.JSONField[[]interface{}](t, body, "sessions")
	require.Len(t, sessions, 1)

	sess := testhelpers.JSONElem[map[string]interface{}](t, sessions, 0)
	ip := testhelpers.JSONField[string](t, sess, "ip_address")

	// POSITIVE assertion (#3290). This previously read
	// assert.NotContains(ip, ".42"), which "unknown" satisfies just as well as
	// the correct mask — so it could not distinguish a working masker from a
	// broken one, and would have stayed green through an inet-rendering change
	// (e.g. ::text appending /32, which net.ParseIP rejects). Assert the shape
	// we actually want.
	assert.Equal(t, "192.168.1.x", ip)
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
