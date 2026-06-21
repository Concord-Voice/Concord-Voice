package notifications_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathMute        = "/api/v1/notifications/mute"
	pathPreferences = "/api/v1/notifications/preferences"
)

// setupTS keeps the per-test boilerplate to a single line and matches the
// shape used by the channels/dm test packages.
func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// muteBody is a small helper to build the JSON payload without sprinkling
// map literals across every test. `mutedUntil` is the empty string when the
// mute should be indefinite.
func muteBody(targetType, targetID string, muted bool, mutedUntil string) map[string]interface{} {
	b := map[string]interface{}{
		"target_type": targetType,
		"target_id":   targetID,
		"muted":       muted,
	}
	if mutedUntil != "" {
		b["muted_until"] = mutedUntil
	}
	return b
}

// --- PUT /notifications/mute ---

func TestSetMute_Server_HappyPath(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser1")
	serverID := ts.CreateTestServer(t, user.ID, "Test Server")

	w := ts.DoRequest("PUT", pathMute, muteBody("server", serverID, true, ""), testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify row exists with muted=true and muted_until=NULL
	var muted bool
	var mutedUntil *time.Time
	err := ts.DB.QueryRow(
		`SELECT muted, muted_until FROM notification_preferences
		 WHERE user_id = $1 AND target_type = $2 AND target_id = $3`,
		user.ID, "server", serverID,
	).Scan(&muted, &mutedUntil)
	require.NoError(t, err)
	assert.True(t, muted)
	assert.Nil(t, mutedUntil)
}

func TestSetMute_Channel_HappyPath(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser2")
	serverID := ts.CreateTestServer(t, user.ID, "S")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("PUT", pathMute, muteBody("channel", channelID, true, ""), testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var muted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT muted FROM notification_preferences
		 WHERE user_id = $1 AND target_type = 'channel' AND target_id = $2`,
		user.ID, channelID,
	).Scan(&muted))
	assert.True(t, muted)
}

func TestSetMute_DM_HappyPath(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser3")
	// DM conversations aren't gated on existence; the server treats target_id
	// as an opaque UUID. Using a fresh UUID is faithful to how the client
	// will call this endpoint for DMs without the server having to validate.
	conversationID := uuid.NewString()

	w := ts.DoRequest("PUT", pathMute, muteBody("dm", conversationID, true, ""), testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var muted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT muted FROM notification_preferences
		 WHERE user_id = $1 AND target_type = 'dm' AND target_id = $2`,
		user.ID, conversationID,
	).Scan(&muted))
	assert.True(t, muted)
}

func TestSetMute_WithMutedUntil(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser4")
	serverID := ts.CreateTestServer(t, user.ID, "S")

	until := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	w := ts.DoRequest("PUT", pathMute, muteBody("server", serverID, true, until), testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var mutedUntil *time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT muted_until FROM notification_preferences
		 WHERE user_id = $1 AND target_type = 'server' AND target_id = $2`,
		user.ID, serverID,
	).Scan(&mutedUntil))
	require.NotNil(t, mutedUntil)
	// Allow a wide tolerance — Postgres rounds to microseconds, we sent seconds.
	assert.WithinDuration(t, time.Now().Add(1*time.Hour), *mutedUntil, 5*time.Second)
}

func TestSetMute_UpsertOverwritesPriorRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser5")
	serverID := ts.CreateTestServer(t, user.ID, "S")

	// First mute with expiry
	until := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute,
		muteBody("server", serverID, true, until), testhelpers.AuthHeaders(user.AccessToken)).Code)

	// Then explicit unmute clears both the mute flag AND the prior expiry.
	// The behaviour we want is "the new value fully replaces the old," not
	// "expiry survives an unmute" — otherwise the row would still be a tombstone
	// implying scheduled re-muting.
	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute,
		muteBody("server", serverID, false, ""), testhelpers.AuthHeaders(user.AccessToken)).Code)

	var muted bool
	var mutedUntil *time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT muted, muted_until FROM notification_preferences
		 WHERE user_id = $1 AND target_type = 'server' AND target_id = $2`,
		user.ID, serverID,
	).Scan(&muted, &mutedUntil))
	assert.False(t, muted)
	assert.Nil(t, mutedUntil)
}

func TestSetMute_MutedUntilIgnoredWhenUnmuting(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser6")
	serverID := ts.CreateTestServer(t, user.ID, "S")

	// Sending muted=false WITH muted_until is nonsense; the handler should
	// store NULL because the timer is meaningless on an unmuted row.
	until := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	w := ts.DoRequest("PUT", pathMute, muteBody("server", serverID, false, until), testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var mutedUntil *time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT muted_until FROM notification_preferences
		 WHERE user_id = $1 AND target_type = 'server' AND target_id = $2`,
		user.ID, serverID,
	).Scan(&mutedUntil))
	assert.Nil(t, mutedUntil)
}

func TestSetMute_InvalidTargetType(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser7")

	w := ts.DoRequest("PUT", pathMute,
		muteBody("workspace", uuid.NewString(), true, ""),
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSetMute_InvalidTargetID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser8")

	w := ts.DoRequest("PUT", pathMute,
		muteBody("server", "not-a-uuid", true, ""),
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSetMute_InvalidMutedUntilFormat(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "muteuser9")
	serverID := ts.CreateTestServer(t, user.ID, "S")

	w := ts.DoRequest("PUT", pathMute,
		muteBody("server", serverID, true, "next tuesday"),
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSetMute_RequiresAuth(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("PUT", pathMute,
		muteBody("server", uuid.NewString(), true, ""), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- GET /notifications/preferences ---

func TestListPreferences_EmptyByDefault(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listuser1")

	w := ts.DoRequest("GET", pathPreferences, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	prefs := body["preferences"].([]interface{})
	assert.Empty(t, prefs)
}

func TestListPreferences_ReturnsAllForCaller(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listuser2")
	serverID := ts.CreateTestServer(t, user.ID, "S")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	dmID := uuid.NewString()

	// Mute one of each target type
	for _, p := range []map[string]interface{}{
		muteBody("server", serverID, true, ""),
		muteBody("channel", channelID, true, ""),
		muteBody("dm", dmID, true, ""),
	} {
		require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute, p, testhelpers.AuthHeaders(user.AccessToken)).Code)
	}

	w := ts.DoRequest("GET", pathPreferences, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	prefs := body["preferences"].([]interface{})
	assert.Len(t, prefs, 3)

	// Snapshot the types we got back; ordering is by target_type, target_id.
	gotTypes := map[string]bool{}
	for _, raw := range prefs {
		p := raw.(map[string]interface{})
		gotTypes[p["target_type"].(string)] = true
	}
	assert.True(t, gotTypes["server"])
	assert.True(t, gotTypes["channel"])
	assert.True(t, gotTypes["dm"])
}

func TestListPreferences_IsolatedPerUser(t *testing.T) {
	ts := setupTS(t)
	alice := ts.CreateTestUser(t, "alicemute")
	bob := ts.CreateTestUser(t, "bobmute")
	serverID := ts.CreateTestServer(t, alice.ID, "Shared")

	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute,
		muteBody("server", serverID, true, ""), testhelpers.AuthHeaders(alice.AccessToken)).Code)

	// Bob hasn't muted anything — must see an empty list, not Alice's row.
	// This is the privacy invariant: muting is user-scoped, not server-scoped.
	w := ts.DoRequest("GET", pathPreferences, nil, testhelpers.AuthHeaders(bob.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Empty(t, body["preferences"].([]interface{}))
}

// --- GET /servers/:id/mute-states ---

func TestGetServerMuteStates_ServerAndChannelPrefs(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stateuser1")
	serverA := ts.CreateTestServer(t, user.ID, "Server A")
	channelA1 := ts.CreateTestChannel(t, serverA, "general")
	channelA2 := ts.CreateTestChannel(t, serverA, "random")

	// Server muted + only channelA1 muted (channelA2 has no pref)
	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute,
		muteBody("server", serverA, true, ""), testhelpers.AuthHeaders(user.AccessToken)).Code)
	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute,
		muteBody("channel", channelA1, true, ""), testhelpers.AuthHeaders(user.AccessToken)).Code)

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverA+"/mute-states", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	prefs := body["preferences"].([]interface{})
	assert.Len(t, prefs, 2)

	// channelA2 was never muted so it correctly does not appear in the response.
	for _, raw := range prefs {
		p := raw.(map[string]interface{})
		assert.NotEqual(t, channelA2, p["target_id"])
	}
}

func TestGetServerMuteStates_ChannelsFromOtherServersExcluded(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stateuser2")
	serverA := ts.CreateTestServer(t, user.ID, "A")
	serverB := ts.CreateTestServer(t, user.ID, "B")
	channelB := ts.CreateTestChannel(t, serverB, "general")

	// Mute a channel that belongs to serverB. Asking for serverA's mute-states
	// must not include this row — otherwise the join is broken and the client
	// would render mutes on the wrong server.
	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", pathMute,
		muteBody("channel", channelB, true, ""), testhelpers.AuthHeaders(user.AccessToken)).Code)

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverA+"/mute-states", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	prefs := body["preferences"].([]interface{})
	assert.Empty(t, prefs)
}

func TestGetServerMuteStates_InvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stateuser3")

	w := ts.DoRequest("GET", "/api/v1/servers/not-a-uuid/mute-states", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetServerMuteStates_RequiresAuth(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", "/api/v1/servers/"+uuid.NewString()+"/mute-states", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
