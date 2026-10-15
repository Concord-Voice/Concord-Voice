package dm_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dmOutboxSessionGeneration  = "26230000-0000-4000-8000-000000000011"
	dmOutboxNodeBootID         = "26230000-0000-4000-8000-000000000012"
	dmOutboxSocketID           = "dm-outbox-test-socket"
	dmOutboxUnavailableNATSURL = "nats://127.0.0.1:1"
)

func seedDMOutboxSession(t *testing.T, ts *testhelpers.TestServer, convID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2
	`, dmOutboxSessionGeneration, dmOutboxNodeBootID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2
		`, dmOutboxSessionGeneration, dmOutboxNodeBootID)
		assert.NoError(t, cleanupErr)
	})
	_, err = ts.DB.Exec(`
		INSERT INTO voice_enforcement_sessions
			(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'dm', $4, '', $5)
	`, dmOutboxSessionGeneration, dmOutboxNodeBootID, convID, userID, dmOutboxSocketID)
	require.NoError(t, err)
}

func requireDMOutboxQueued(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	// Check the public acceptance contract before inspecting the new schema.
	require.Equal(t, "queued", body["enforcement_status"], response.Body.String())
	assert.NotEqual(t, true, body["applied"], "durable acceptance does not establish a live media effect")
	assert.NotEqual(t, true, body["disconnected"], "durable acceptance does not establish transport closure")
}

func TestVoiceCommandOutbox_DMSnapshotBrokerOutage(t *testing.T) {
	for _, test := range []struct {
		name, method, suffix        string
		beforeMuted, beforeDeafened bool
		wantMuted, wantDeafened     bool
	}{
		{name: "mute", method: http.MethodPost, suffix: pathMute, wantMuted: true},
		{name: "unmute", method: http.MethodDelete, suffix: pathMute, beforeMuted: true},
		{name: "deafen", method: http.MethodPost, suffix: pathDeafen, wantMuted: true, wantDeafened: true},
		{name: "undeafen", method: http.MethodDelete, suffix: pathDeafen, beforeMuted: true, beforeDeafened: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The retrying client keeps router dependencies wired during an outage.
			t.Setenv("NATS_URL", dmOutboxUnavailableNATSURL)
			ts := setupTS(t)
			admin := ts.CreateTestUser(t, "dm-outbox-snapshot-admin")
			target := ts.CreateTestUser(t, "dm-outbox-snapshot-target")
			convID := createTestGroup(t, ts, admin, target)
			// Registration precedes voice-presence ingestion on the join path.
			seedDMOutboxSession(t, ts, convID, target.ID)
			_, err := ts.DB.Exec(`
				UPDATE dm_participants SET server_muted = $3, server_deafened = $4
				WHERE conversation_id = $1 AND user_id = $2
			`, convID, target.ID, test.beforeMuted, test.beforeDeafened)
			require.NoError(t, err)

			response := ts.DoRequest(test.method, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+test.suffix,
				nil, testhelpers.AuthHeaders(admin.AccessToken))
			requireDMOutboxQueued(t, response, http.StatusOK)

			var muted, deafened bool
			require.NoError(t, ts.DB.QueryRow(`
				SELECT server_muted, server_deafened FROM dm_participants
				WHERE conversation_id = $1 AND user_id = $2
			`, convID, target.ID).Scan(&muted, &deafened))
			assert.Equal(t, test.wantMuted, muted)
			assert.Equal(t, test.wantDeafened, deafened)
			command := readDMOutboxCommand(t, ts.DB, target.ID)
			assert.Equal(t, "dm", command.Scope)
			assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
			assert.False(t, command.ServerID.Valid)
			assertDMOutboxTarget(t, command)
			assertDMSnapshotFields(t, command, test.name, test.wantMuted, test.wantDeafened)
		})
	}
}

func TestVoiceCommandOutbox_DMUserMuteBrokerOutage(t *testing.T) {
	t.Setenv("NATS_URL", dmOutboxUnavailableNATSURL)
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dm-outbox-user-mute-actor")
	target := ts.CreateTestUser(t, "dm-outbox-user-mute-target")
	ts.CreateFriendship(t, actor.ID, target.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, actor.ID, target.ID)
	seedDMOutboxSession(t, ts, convID, target.ID)
	seedDMVoiceParticipant(t, ts, convID, target.ID)

	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathUserMute,
		nil, testhelpers.AuthHeaders(actor.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusAccepted)
	command := readDMOutboxCommand(t, ts.DB, target.ID)
	assert.Equal(t, "dm", command.Scope)
	assert.Equal(t, "user_moderation", command.Kind)
	assert.Equal(t, "mute", command.Action)
	assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
	assert.False(t, command.ServerID.Valid)
	assertDMOutboxTarget(t, command)
	assert.False(t, command.Reason.Valid)
	assert.False(t, command.Revision.Valid)
	assert.False(t, command.Muted.Valid)
	assert.False(t, command.Deafened.Valid)
}

func TestVoiceCommandOutbox_RemoveMemberProvisionalBrokerOutage(t *testing.T) {
	t.Setenv("NATS_URL", dmOutboxUnavailableNATSURL)
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "dm-outbox-removal-admin")
	target := ts.CreateTestUser(t, "dm-outbox-removal-target")
	convID := createTestGroup(t, ts, admin, target)
	seedDMOutboxSession(t, ts, convID, target.ID)

	response := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+target.ID,
		nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)

	var exists bool
	require.NoError(t, ts.DB.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)
	`, convID, target.ID).Scan(&exists))
	assert.False(t, exists, "accepted removal commits the membership mutation")
	command := readDMOutboxCommand(t, ts.DB, target.ID)
	assert.Equal(t, "dm", command.Scope)
	assert.Equal(t, "disconnect", command.Kind)
	assert.Equal(t, "disconnect", command.Action)
	assert.Equal(t, sql.NullString{String: convID, Valid: true}, command.ChannelID)
	assert.False(t, command.ServerID.Valid)
	assertDMOutboxTarget(t, command)
	assert.Equal(t, sql.NullString{String: "membership_removed", Valid: true}, command.Reason)
	assert.False(t, command.Revision.Valid)
	assert.False(t, command.Muted.Valid)
	assert.False(t, command.Deafened.Valid)
}
