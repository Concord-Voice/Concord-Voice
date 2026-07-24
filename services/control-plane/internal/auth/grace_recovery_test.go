package auth_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2428: an in-grace replay of the just-revoked source token recovers the
// session by selecting the exact successor via predecessor_id lineage.
func TestGraceRecovery_BenignRotationRaceRecovers(t *testing.T) {
	ts := setupTS(t)
	refreshToken, userID := registerAndGetRefreshToken(t, ts, "gracebenign", "")

	// First refresh rotates: source revoked, successor minted (predecessor_id = source).
	w := doRefreshWithMachineID(ts, refreshToken, "", "")
	require.Equal(t, http.StatusOK, w.Code)

	// Replay the source within the 30s grace window, same client → recovers.
	w = doRefreshWithMachineID(ts, refreshToken, "", "")
	require.Equal(t, http.StatusOK, w.Code, "in-grace replay should recover the session")

	// Exactly one live token remains (the fresh re-mint).
	var live int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&live))
	assert.Equal(t, 1, live)
}

// A replay whose lineage has no live successor is a stale replay → 401 (not a
// recovery, not a swallowed-error 500).
func TestGraceRecovery_StaleReplayNoLiveSuccessor(t *testing.T) {
	ts := setupTS(t)
	refreshToken, userID := registerAndGetRefreshToken(t, ts, "gracestale", "")

	w := doRefreshWithMachineID(ts, refreshToken, "", "")
	require.Equal(t, http.StatusOK, w.Code)

	// Revoke every live successor so the lineage lookup finds nothing.
	_, err := ts.DB.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	require.NoError(t, err)

	w = doRefreshWithMachineID(ts, refreshToken, "", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// A disabled account never grace-recovers (checked under the row lock now).
func TestGraceRecovery_DisabledAccountForbidden(t *testing.T) {
	ts := setupTS(t)
	refreshToken, userID := registerAndGetRefreshToken(t, ts, "gracedisabled", "")

	w := doRefreshWithMachineID(ts, refreshToken, "", "")
	require.Equal(t, http.StatusOK, w.Code)

	_, err := ts.DB.Exec(`UPDATE users SET disabled = TRUE WHERE id = $1`, userID)
	require.NoError(t, err)

	w = doRefreshWithMachineID(ts, refreshToken, "", "")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// The lineage bind (Defect 2): a foreign live token in the old time-window is
// NOT consumed as the successor. Recovery selects the real successor via
// predecessor_id and leaves the foreign token untouched.
func TestGraceRecovery_ForeignLiveTokenNotConsumed(t *testing.T) {
	ts := setupTS(t)
	refreshToken, userID := registerAndGetRefreshToken(t, ts, "graceforeign", "")

	w := doRefreshWithMachineID(ts, refreshToken, "", "")
	require.Equal(t, http.StatusOK, w.Code)

	// Foreign live token for the same user, no lineage edge (predecessor_id NULL),
	// created "now" so the pre-fix ±2s window query WOULD have grabbed it.
	foreignID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me)
		 VALUES ($1, $2, $3, 'foreign', '192.0.2.1', 'foreign-ua', NOW() + INTERVAL '30 days', true)`,
		foreignID, userID, "foreign-hash-"+foreignID)
	require.NoError(t, err)

	// Replay source within grace → recovers via the real successor, not the foreign token.
	w = doRefreshWithMachineID(ts, refreshToken, "", "")
	require.Equal(t, http.StatusOK, w.Code)

	var foreignRevoked sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE id = $1`, foreignID).Scan(&foreignRevoked))
	assert.False(t, foreignRevoked.Valid, "foreign token must not be consumed as the successor")
}

// A remember-me session recovers with the 30-day expiry re-mint path.
func TestGraceRecovery_RememberMeRecovers(t *testing.T) {
	ts := setupTS(t)
	refreshToken, userID := registerAndGetRefreshToken(t, ts, "graceremember", "")

	// Make the source a remember-me session; the successor inherits it, so recovery
	// exercises the long-expiry branch.
	tokenHash := auth.HashRefreshToken(refreshToken)
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET remember_me = TRUE WHERE token_hash = $1`, tokenHash)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, doRefreshWithMachineID(ts, refreshToken, "", "").Code)
	require.Equal(t, http.StatusOK, doRefreshWithMachineID(ts, refreshToken, "", "").Code)

	// The re-minted token is a remember-me session with a ~30-day expiry.
	var live int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NULL AND remember_me = TRUE
		   AND expires_at > NOW() + INTERVAL '20 days'`, userID).Scan(&live))
	assert.Equal(t, 1, live)
}

// CWE-613 (#2428): an already-expired successor must not be grace-recovered into a
// fresh access token. The lineage lookup's `expires_at > NOW()` filter fails closed
// to 401, mirroring the normal Refresh path's expiry gate.
func TestGraceRecovery_ExpiredSuccessorNotRecovered(t *testing.T) {
	ts := setupTS(t)
	refreshToken, userID := registerAndGetRefreshToken(t, ts, "graceexpiredsucc", "")

	require.Equal(t, http.StatusOK, doRefreshWithMachineID(ts, refreshToken, "", "").Code)

	// Force the live successor past its absolute expiry.
	_, err := ts.DB.Exec(
		`UPDATE refresh_tokens SET expires_at = NOW() - INTERVAL '1 hour'
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	require.NoError(t, err)

	// Replay within the grace window: no live AND unexpired successor → 401, no re-mint.
	assert.Equal(t, http.StatusUnauthorized, doRefreshWithMachineID(ts, refreshToken, "", "").Code)

	// And nothing was minted — zero live tokens remain.
	var live int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()`,
		userID).Scan(&live))
	assert.Equal(t, 0, live)
}

// A real DB/transaction error during recovery surfaces as 500, never a swallowed
// 401 (Defect-1 silent-failure discipline). An already-cancelled request context
// deterministically fails the recovery transaction's open without DB surgery.
func TestGraceRecovery_DBErrorReturns500(t *testing.T) {
	ts := setupTS(t)
	refreshToken, _ := registerAndGetRefreshToken(t, ts, "gracedberr", "")

	require.Equal(t, http.StatusOK, doRefreshWithMachineID(ts, refreshToken, "", "").Code)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/api/v1/auth/refresh", nil).WithContext(ctx)
	req.Header.Set(ctHeader, ctJSON)
	req.Header.Set("X-Refresh-Token", refreshToken)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
