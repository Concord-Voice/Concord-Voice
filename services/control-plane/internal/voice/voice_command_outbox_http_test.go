package voice_test

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
	outboxSessionGeneration  = "26230000-0000-4000-8000-000000000001"
	outboxNodeBootID         = "26230000-0000-4000-8000-000000000002"
	outboxSocketID           = "outbox-test-socket"
	outboxUnavailableNATSURL = "nats://127.0.0.1:1"
)

func seedOutboxVoiceSession(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2
	`, outboxSessionGeneration, outboxNodeBootID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DELETE FROM voice_enforcement_sessions WHERE session_generation = $1 AND node_boot_id = $2
		`, outboxSessionGeneration, outboxNodeBootID)
		assert.NoError(t, cleanupErr)
	})
	_, err = ts.DB.Exec(`
		INSERT INTO voice_enforcement_sessions
			(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'channel', $4, '', $5)
	`, outboxSessionGeneration, outboxNodeBootID, channelID, userID, outboxSocketID)
	require.NoError(t, err)
}

func requireOutboxQueued(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	// Check the public acceptance contract before inspecting the new schema.
	require.Equal(t, "queued", body["enforcement_status"], response.Body.String())
	assert.NotEqual(t, true, body["applied"], "durable acceptance does not establish a live media effect")
	assert.NotEqual(t, true, body["disconnected"], "durable acceptance does not establish transport closure")
}

func TestVoiceCommandOutbox_ServerSnapshotBrokerOutage(t *testing.T) {
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
			t.Setenv("NATS_URL", outboxUnavailableNATSURL)
			ts := setupTS(t)
			owner := ts.CreateTestUser(t, "outbox-snapshot-owner")
			target := ts.CreateTestUser(t, "outbox-snapshot-target")
			serverID := ts.CreateTestServer(t, owner.ID, "Outbox Snapshot")
			ts.AddMemberToServer(t, serverID, target.ID, roleMember)
			channelID := ts.CreateVoiceChannel(t, serverID, "outbox-snapshot-channel")
			// A2 registration exists before the voice-presence event arrives.
			seedOutboxVoiceSession(t, ts, channelID, target.ID)
			_, err := ts.DB.Exec(`
				UPDATE server_members SET server_muted = $3, server_deafened = $4
				WHERE server_id = $1 AND user_id = $2
			`, serverID, target.ID, test.beforeMuted, test.beforeDeafened)
			require.NoError(t, err)

			response := ts.DoRequest(test.method, voiceEnforcePath(serverID, target.ID, test.suffix), nil,
				testhelpers.AuthHeaders(owner.AccessToken))
			requireOutboxQueued(t, response, http.StatusOK)

			var muted, deafened bool
			require.NoError(t, ts.DB.QueryRow(`
				SELECT server_muted, server_deafened FROM server_members
				WHERE server_id = $1 AND user_id = $2
			`, serverID, target.ID).Scan(&muted, &deafened))
			assert.Equal(t, test.wantMuted, muted)
			assert.Equal(t, test.wantDeafened, deafened)
			command := readServerOutboxCommand(t, ts.DB, target.ID)
			assert.Equal(t, "channel", command.Scope)
			assert.Equal(t, sql.NullString{String: channelID, Valid: true}, command.ChannelID)
			assert.Equal(t, sql.NullString{String: serverID, Valid: true}, command.ServerID)
			assertServerOutboxTarget(t, command)
			assertServerSnapshotFields(t, command, test.name, test.wantMuted, test.wantDeafened)
		})
	}
}

func TestVoiceCommandOutbox_ServerTransientBrokerOutage(t *testing.T) {
	for _, test := range []struct{ name, suffix string }{
		{name: "user mute", suffix: pathUserMute},
		{name: "user deafen", suffix: pathUserDeafen},
		{name: "disconnect", suffix: pathDisconnect},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NATS_URL", outboxUnavailableNATSURL)
			ts := setupTS(t)
			owner := ts.CreateTestUser(t, "outbox-transient-owner")
			target := ts.CreateTestUser(t, "outbox-transient-target")
			serverID := ts.CreateTestServer(t, owner.ID, "Outbox Transient")
			ts.AddMemberToServer(t, serverID, target.ID, roleMember)
			channelID := ts.CreateVoiceChannel(t, serverID, "outbox-transient-channel")
			seedOutboxVoiceSession(t, ts, channelID, target.ID)
			_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
			require.NoError(t, err)

			response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, test.suffix), nil,
				testhelpers.AuthHeaders(owner.AccessToken))
			requireOutboxQueued(t, response, http.StatusAccepted)
			command := readServerOutboxCommand(t, ts.DB, target.ID)
			kind, action := "user_moderation", "mute"
			if test.suffix == pathUserDeafen {
				action = "deafen"
			}
			if test.suffix == pathDisconnect {
				kind, action = "disconnect", "disconnect"
				assert.Equal(t, sql.NullString{String: "moderator_request", Valid: true}, command.Reason)
			} else {
				assert.False(t, command.Reason.Valid)
			}
			assert.Equal(t, "channel", command.Scope)
			assert.Equal(t, kind, command.Kind)
			assert.Equal(t, action, command.Action)
			assert.Equal(t, sql.NullString{String: channelID, Valid: true}, command.ChannelID)
			assert.False(t, command.ServerID.Valid)
			assertServerOutboxTarget(t, command)
			assert.False(t, command.Revision.Valid)
			assert.False(t, command.Muted.Valid)
			assert.False(t, command.Deafened.Valid)
		})
	}
}
