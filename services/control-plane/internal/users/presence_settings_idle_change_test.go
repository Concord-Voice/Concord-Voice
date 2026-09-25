package users_test

// Regression tests for Rich Presence sharing changes made while the sender has
// no published Server Voice or Private Call activity. Before the fix, the
// second such change could not resolve its prior audience, fell back to
// disconnecting every local WebSocket client, answered 503, and left a pending
// cleanup marker that made every later change fail the same way.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func dialPresenceObserver(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	ticket := "presence-idle-" + uuid.NewString()
	require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":presence-idle", time.Minute).Err())
	srv := httptest.NewServer(ts.Router)
	t.Cleanup(srv.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+srv.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(userID)) > 0
	}, 2*time.Second, 10*time.Millisecond)
}

func patchPresenceTier(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, field string, tier int) {
	t.Helper()
	w := ts.DoRequest(methodPatch, urlUsersMePresence, map[string]interface{}{field: tier},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "%s=%d: %s", field, tier, w.Body.String())
}

func pendingActivityCleanups(t *testing.T, ts *testhelpers.TestServer, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM activity_settings_pending_cleanups WHERE user_id = $1`, userID).Scan(&n))
	return n
}

// TestUpdatePresenceSettingsRepeatedTierChangesWhileIdle: every sharing change
// saves when nothing is published, not only the first one away from Off.
func TestUpdatePresenceSettingsRepeatedTierChangesWhileIdle(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presidle")

	for _, step := range []struct {
		field string
		tier  int
	}{
		{"server_voice_tier", 1}, {"server_voice_tier", 2}, {"server_voice_tier", 1},
		{"private_call_tier", 1}, {"private_call_tier", 2}, {"private_call_tier", 0},
	} {
		patchPresenceTier(t, ts, user, step.field, step.tier)
	}

	var serverTier, privateTier int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT server_voice_tier, private_call_tier FROM user_presence_settings WHERE user_id = $1`,
		user.ID).Scan(&serverTier, &privateTier))
	assert.Equal(t, 1, serverTier)
	assert.Equal(t, 0, privateTier)
	assert.Equal(t, 0, pendingActivityCleanups(t, ts, user.ID), "no cleanup may be left pending")
}

// TestUpdatePresenceSettingsIdleChangeKeepsUnrelatedClientsConnected: a sharing
// change with nothing published must not disconnect other users' sockets.
func TestUpdatePresenceSettingsIdleChangeKeepsUnrelatedClientsConnected(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "presidleactor")
	stranger := ts.CreateTestUser(t, "presidlestranger")
	dialPresenceObserver(t, ts, stranger.ID)

	// Another user being in voice or a call must not count as evidence for the actor.
	serverID := ts.CreateTestServer(t, stranger.ID, "presidle-other")
	channelID := ts.CreateVoiceChannel(t, serverID, "presidle-voice")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, NOW(), NOW())`, channelID, stranger.ID)
	require.NoError(t, err)

	patchPresenceTier(t, ts, actor, "server_voice_tier", 1)
	patchPresenceTier(t, ts, actor, "server_voice_tier", 2)

	// The 503 check above is what catches the original bug. This guards the
	// variant where a change succeeds but still disconnects everyone.
	assert.Never(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(stranger.ID)) == 0
	}, 500*time.Millisecond, 20*time.Millisecond, "an unrelated user's socket was disconnected")
}

// TestUpdatePresenceSettingsResumesLeftoverIdleCleanup: a user already stuck
// behind a marker from the old behavior recovers on their next change.
func TestUpdatePresenceSettingsResumesLeftoverIdleCleanup(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presidlestuck")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, master_enabled, server_voice_tier)
		VALUES ($1, TRUE, 2)`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO activity_settings_pending_cleanups (user_id, operation_id, evidence)
		VALUES ($1, $2, $3::jsonb)`, user.ID, uuid.NewString(), `{"version":1,
		"before":{"master_enabled":true,"server_voice_tier":1,"server_voice_show_details":true,"private_call_tier":0,"private_call_show_details":false},
		"after":{"master_enabled":true,"server_voice_tier":2,"server_voice_show_details":true,"private_call_tier":0,"private_call_show_details":false}}`)
	require.NoError(t, err)

	patchPresenceTier(t, ts, user, "server_voice_tier", 1)

	var serverTier int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT server_voice_tier FROM user_presence_settings WHERE user_id = $1`, user.ID).Scan(&serverTier))
	assert.Equal(t, 1, serverTier)
	assert.Equal(t, 0, pendingActivityCleanups(t, ts, user.ID))
}

// TestUpdatePresenceSettingsInVoiceSenderReconnectsOnlyPriorAudience: a sender
// the database shows in voice, with nothing stored, is treated like a stored
// badge. The change succeeds, the viewer the prior policy reached is cleared
// and reconnected, and a user outside that audience keeps their socket (#3444
// R2). Before, this answered 503 and disconnected every local client.
func TestUpdatePresenceSettingsInVoiceSenderReconnectsOnlyPriorAudience(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "presinvoice")
	viewer := ts.CreateTestUser(t, "presinvoiceview")
	stranger := ts.CreateTestUser(t, "presinvoiceobs")
	serverID := ts.CreateTestServer(t, actor.ID, "presinvoice")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "presinvoice-voice")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, NOW(), NOW())`, channelID, actor.ID)
	require.NoError(t, err)
	patchPresenceTier(t, ts, actor, "server_voice_tier", 2)
	dialPresenceObserver(t, ts, viewer.ID)
	dialPresenceObserver(t, ts, stranger.ID)

	patchPresenceTier(t, ts, actor, "server_voice_tier", 1)

	assert.Equal(t, 0, pendingActivityCleanups(t, ts, actor.ID))
	assert.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(viewer.ID)) == 0
	}, 2*time.Second, 20*time.Millisecond, "the prior audience was not reconnected")
	assert.Never(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(stranger.ID)) == 0
	}, 500*time.Millisecond, 20*time.Millisecond, "a user outside the audience was disconnected")
}
