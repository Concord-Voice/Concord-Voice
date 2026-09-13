package auth_test

// Regression coverage for #3290: refresh metadata may be SQL NULL.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type refreshEventRecorder struct{ events []securityevent.Event }

func (r *refreshEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

func refreshDirect(t *testing.T, h *auth.Handler, token, machineID, remoteAddr, userAgent string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, pathRefresh, nil)
	c.Request.Header.Set("X-Refresh-Token", token)
	if machineID != "" {
		c.Request.Header.Set("X-Machine-Id", machineID)
	}
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	// Assign even an empty value so callers can exercise an absent client IP;
	// httptest.NewRequest otherwise supplies its synthetic 192.0.2.1 address.
	c.Request.RemoteAddr = remoteAddr
	h.Refresh(c)
	return w
}
func TestGraceRecovery_SSONullSignalsQuietlyRefuses(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grace-sso-null")
	var logBuffer bytes.Buffer
	h := auth.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(&logBuffer), testhelpers.TestJWTSecret, nil)
	_, refresh, _, err := h.IssueAccessAndRefresh(context.Background(), user.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, refreshDirect(t, h, refresh, "", "198.51.100.10:1234", "").Code)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, "", "198.51.100.99:1234", "quiet-replay-agent")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, recorder.events)
	assert.Contains(t, logBuffer.String(), "failure_class=no_known_signal")
	assert.NotContains(t, logBuffer.String(), "ip_address")
	assert.NotContains(t, logBuffer.String(), "user_agent")
	assert.NotContains(t, logBuffer.String(), "198.51.100.99")
	assert.NotContains(t, logBuffer.String(), "quiet-replay-agent")
}

func TestGraceRecovery_SSONullSignalsOutsideWindowRaisesReplayEvent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grace-sso-old-null")
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	_, refresh, sessionID, err := h.IssueAccessAndRefresh(context.Background(), user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() - INTERVAL '31 seconds' WHERE id = $1`, sessionID)
	require.NoError(t, err)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, "", "198.51.100.99:1234", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, securityevent.SeverityHigh, recorder.events[0].Severity)
	assert.Equal(t, securityevent.ReasonRefreshReplay, recorder.events[0].ReasonCode)
}
func TestGraceRecovery_KnownSignalsStillRaiseReplayEvent(t *testing.T) {
	ts := setupTS(t)
	refresh, _ := registerAndGetRefreshToken(t, ts, "grace-known-signals", "")
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET ip_address = '198.51.100.10', user_agent = 'agent-a' WHERE token_hash = $1`, auth.HashRefreshToken(refresh))
	require.NoError(t, err)
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	require.Equal(t, http.StatusOK, refreshDirect(t, h, refresh, "", "198.51.100.10:1234", "agent-a").Code)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, "", "198.51.100.99:1234", "agent-b")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, securityevent.ReasonRefreshReplay, recorder.events[0].ReasonCode)
	assert.Equal(t, securityevent.SeverityHigh, recorder.events[0].Severity)
}
func TestGraceRecovery_MatchingKnownSignalsRecoversSuccessor(t *testing.T) {
	ts := setupTS(t)
	refresh, _ := registerAndGetRefreshToken(t, ts, "grace-matching-signals", "")
	var sourceID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM refresh_tokens WHERE token_hash = $1`, auth.HashRefreshToken(refresh)).Scan(&sourceID))
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET ip_address = '198.51.100.10', user_agent = 'agent-a' WHERE token_hash = $1`, auth.HashRefreshToken(refresh))
	require.NoError(t, err)
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	require.Equal(t, http.StatusOK, refreshDirect(t, h, refresh, "", "198.51.100.10:1234", "agent-a").Code)
	var successorID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM refresh_tokens WHERE user_id = (SELECT user_id FROM refresh_tokens WHERE token_hash = $1) AND revoked_at IS NULL`, auth.HashRefreshToken(refresh)).Scan(&successorID))
	_, err = ts.DB.Exec(`UPDATE refresh_tokens SET device_name = NULL WHERE id = $1`, successorID)
	require.NoError(t, err)
	w := refreshDirect(t, h, refresh, "", "198.51.100.10:1234", "agent-a")
	assert.Equal(t, http.StatusOK, w.Code)
	var response struct {
		PreviousSessionID string `json:"previous_session_id"`
		SessionID         string `json:"session_id"`
	}
	testhelpers.ParseJSON(t, w, &response)
	assert.Equal(t, sourceID, response.PreviousSessionID)
	var liveID, deviceName string
	require.NoError(t, ts.DB.QueryRow(`SELECT id, device_name FROM refresh_tokens WHERE predecessor_id = $1 AND revoked_at IS NULL`, successorID).Scan(&liveID, &deviceName))
	assert.NotEqual(t, successorID, liveID)
	assert.Equal(t, response.SessionID, liveID)
	assert.Empty(t, deviceName)
}
func TestRefresh_SSONullMetadataWithMachineIDDoesNotTriggerTheft(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sso-machine-id")
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	_, refresh, _, err := h.IssueAccessAndRefresh(context.Background(), user.ID)
	require.NoError(t, err)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, "machine-a", "198.51.100.10:1234", "")
	assert.Equal(t, http.StatusOK, w.Code)
	for _, event := range recorder.events {
		assert.NotEqual(t, securityevent.ReasonTokenTheftSuspected, event.ReasonCode)
	}
}
func TestRefresh_NullIPMachineMismatchIsSuspiciousNotTheft(t *testing.T) {
	ts := setupTS(t)
	storedMachineID, requestMachineID := uuid.New().String(), uuid.New().String()
	refresh, userID := registerAndGetRefreshToken(t, ts, "nullipmachine", storedMachineID)
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET ip_address = NULL, machine_id = $1 WHERE token_hash = $2`, storedMachineID, auth.HashRefreshToken(refresh))
	require.NoError(t, err)
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, requestMachineID, "198.51.100.10:1234", "")
	assert.Equal(t, http.StatusOK, w.Code)
	var live int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&live))
	assert.Equal(t, 1, live)
	for _, event := range recorder.events {
		assert.NotEqual(t, securityevent.ReasonTokenTheftSuspected, event.ReasonCode)
	}
}

func TestRefresh_KnownMachineMismatchWithAbsentRequestIPTriggersTheft(t *testing.T) {
	ts := setupTS(t)
	storedMachineID, requestMachineID := uuid.New().String(), uuid.New().String()
	refresh, userID := registerAndGetRefreshToken(t, ts, "known-machine-absent-ip", storedMachineID)
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET ip_address = '198.51.100.10', machine_id = $1 WHERE token_hash = $2`, storedMachineID, auth.HashRefreshToken(refresh))
	require.NoError(t, err)
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, ts.Hub)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, requestMachineID, "", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, securityevent.ReasonTokenTheftSuspected, recorder.events[0].ReasonCode)
	var live int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&live))
	assert.Zero(t, live, "theft handling must not mint a replacement refresh token")
}
func TestGraceRecovery_KnownUserAgentMissingRequestRefuses(t *testing.T) {
	ts := setupTS(t)
	refresh, _ := registerAndGetRefreshToken(t, ts, "grace-missing-ua", "")
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET ip_address = '198.51.100.10', user_agent = 'agent-a' WHERE token_hash = $1`, auth.HashRefreshToken(refresh))
	require.NoError(t, err)
	h := auth.NewHandler(ts.DB, ts.Redis, logger.New("test"), testhelpers.TestJWTSecret, nil)
	require.Equal(t, http.StatusOK, refreshDirect(t, h, refresh, "", "198.51.100.10:1234", "agent-a").Code)
	recorder := &refreshEventRecorder{}
	h.SetSecurityEvents(recorder)
	w := refreshDirect(t, h, refresh, "", "198.51.100.10:1234", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, securityevent.ReasonRefreshReplay, recorder.events[0].ReasonCode)
	var revoked, live int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FILTER (WHERE revoked_at IS NOT NULL), count(*) FILTER (WHERE revoked_at IS NULL) FROM refresh_tokens WHERE token_hash = $1 OR predecessor_id = (SELECT id FROM refresh_tokens WHERE token_hash = $1)`, auth.HashRefreshToken(refresh)).Scan(&revoked, &live))
	assert.Equal(t, 1, revoked)
	assert.Equal(t, 1, live)
}
