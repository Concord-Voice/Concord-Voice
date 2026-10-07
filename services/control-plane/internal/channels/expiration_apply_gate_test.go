package channels_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func insertAgedChannelMessage(t *testing.T, db *sql.DB, channelID, authorID, age string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		 VALUES ($1, $2, $3, 'history', 1, false, NOW() - $4::interval, NOW() - $4::interval)`,
		id, channelID, authorID, age)
	require.NoError(t, err)
	return id
}

func channelMessageExpiresAt(t *testing.T, db *sql.DB, messageID string) sql.NullTime {
	t.Helper()
	var v sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT expires_at FROM messages WHERE id = $1`, messageID).Scan(&v))
	return v
}

// TestChannelExpirationGate_ApplyToHistoryIsGated: a set with retroactive
// "apply" back-fills expires_at onto every message an earlier "new_only"
// policy kept, so on an enforcing server it needs a factor even when the
// window is the same or longer. Found by the #3454 red-team pass: the gate
// compared window lengths only, and a stolen session could re-send the
// current window with "apply" and turn 90 days of kept history into
// already-expired rows with no factor.
//
// Mutant killed: dropping the retroactive arm from shortensRetention (the
// no-code request is admitted and the old message gets a past expires_at).
func TestChannelExpirationGate_ApplyToHistoryIsGated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prior, next int
	}{
		{"same window", 2592000, 2592000},
		{"longer window", 86400, 604800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			f := newChanGateFixture(t, ts)
			path := pathChannelsPrefix + f.channelID + "/expiration"
			auth := testhelpers.AuthHeaders(f.mod.AccessToken)
			old := insertAgedChannelMessage(t, ts.DB, f.channelID, f.owner.ID, "90 days")

			w := ts.DoRequest(http.MethodPatch, path,
				map[string]any{"mode": "set", "window_seconds": tc.prior, "retroactive": "new_only"}, auth)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.False(t, channelMessageExpiresAt(t, ts.DB, old).Valid, "new_only keeps history unstamped")

			testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
			enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

			apply := map[string]any{"mode": "set", "window_seconds": tc.next, "retroactive": "apply"}
			w = ts.DoRequest(http.MethodPatch, path, apply, auth)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String())
			assert.False(t, channelMessageExpiresAt(t, ts.DB, old).Valid, "a refused apply stamps nothing")

			apply["mfa_code"] = mintChanGateToken(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten)
			w = ts.DoRequest(http.MethodPatch, path, apply, auth)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			exp := channelMessageExpiresAt(t, ts.DB, old)
			assert.True(t, exp.Valid && exp.Time.Before(time.Now()), "a confirmed apply stamps the kept history (got %+v)", exp)
		})
	}
}

// TestChannelExpirationGate_LongerNewOnlyStaysUngated pins the other side:
// lengthening the window for new messages only touches no kept message, so
// it stays ungated on an enforcing server.
func TestChannelExpirationGate_LongerNewOnlyStaysUngated(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	path := pathChannelsPrefix + f.channelID + "/expiration"
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)
	old := insertAgedChannelMessage(t, ts.DB, f.channelID, f.owner.ID, "90 days")

	w := ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

	w = ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 604800, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, channelMessageExpiresAt(t, ts.DB, old).Valid)
}
