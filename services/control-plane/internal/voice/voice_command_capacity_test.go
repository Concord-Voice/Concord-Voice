package voice_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const serverVoiceActorQueueLimit = 1000

func seedServerVoiceActorQueue(t *testing.T, ts *testhelpers.TestServer, actorID, markerRoomID string) {
	t.Helper()
	specs := make([]voicecommand.CommandSpec, serverVoiceActorQueueLimit)
	for index := range specs {
		specs[index] = voicecommand.CommandSpec{
			Scope: "dm", Kind: "disconnect", Action: "disconnect", ChannelID: &markerRoomID,
			UserID: actorID, Reason: "membership_removed",
		}
	}
	tx, err := ts.DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			assert.NoError(t, rollbackErr)
		}
	})
	commands, err := voicecommand.EnqueueTx(t.Context(), tx, specs)
	require.NoError(t, err)
	require.Len(t, commands, serverVoiceActorQueueLimit)
	require.NoError(t, tx.Commit())
}

type serverVoiceCapacitySnapshot struct {
	Rows, Allocated, Published int64
	Commands                   [32]byte
}

func readServerVoiceCapacity(t *testing.T, db *sql.DB) serverVoiceCapacitySnapshot {
	t.Helper()
	var snapshot serverVoiceCapacitySnapshot
	var commands []byte
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*), COALESCE(jsonb_agg(to_jsonb(command) ORDER BY sequence), '[]'::jsonb)::text
		FROM voice_command_outbox AS command
	`).Scan(&snapshot.Rows, &commands))
	require.NoError(t, db.QueryRow(`
		SELECT last_allocated_sequence, last_published_sequence FROM voice_command_sequence WHERE id = TRUE
	`).Scan(&snapshot.Allocated, &snapshot.Published))
	snapshot.Commands = sha256.Sum256(commands)
	return snapshot
}

func requireServerVoiceCapacityRefusal(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, "30", response.Header().Get("Retry-After"))
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "voice_command_queue_full", body["error_code"], response.Body.String())
	assert.NotEqual(t, "queued", body["enforcement_status"])
	assert.NotEqual(t, true, body["applied"])
	assert.NotEqual(t, true, body["disconnected"])
}

func requireNoServerVoiceCapacityBroadcast(t *testing.T, conn *gorillaWS.Conn, targetID string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	for {
		var envelope voiceWireEnvelope
		if err := conn.ReadJSON(&envelope); err != nil {
			var timeout net.Error
			require.ErrorAs(t, err, &timeout, "the observer must stay connected through the refusal window")
			require.True(t, timeout.Timeout())
			return
		}
		assert.False(t, envelope.Type == "voice_state_update" && envelope.Data["user_id"] == targetID,
			"refused moderation must not announce a policy or media change")
	}
}

func TestVoiceCommandCapacity_ServerActorExhaustionRollsBackHTTPMutation(t *testing.T) {
	for _, test := range []struct {
		name, method, suffix        string
		beforeMuted, beforeDeafened bool
	}{
		{name: "mute", method: http.MethodPost, suffix: pathMute},
		{name: "unmute", method: http.MethodDelete, suffix: pathMute, beforeMuted: true},
		{name: "deafen", method: http.MethodPost, suffix: pathDeafen},
		{name: "undeafen", method: http.MethodDelete, suffix: pathDeafen, beforeMuted: true, beforeDeafened: true},
		{name: "user mute", method: http.MethodPost, suffix: pathUserMute},
		{name: "user deafen", method: http.MethodPost, suffix: pathUserDeafen},
		{name: "disconnect", method: http.MethodPost, suffix: pathDisconnect},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
			seedOutboxVoiceSession(t, ts, channelID, target.ID)
			_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
			require.NoError(t, err)
			_, err = ts.DB.Exec(`
				UPDATE server_members SET server_muted = $3, server_deafened = $4
				WHERE server_id = $1 AND user_id = $2
			`, serverID, target.ID, test.beforeMuted, test.beforeDeafened)
			require.NoError(t, err)
			markerRoomID := ts.CreateDMConversation(t, owner.ID, target.ID)
			// The marker batch uses the source-compatible target attribution.
			// Requested moderation must charge the authenticated owner, a different
			// identity from the moderated target.
			seedServerVoiceActorQueue(t, ts, owner.ID, markerRoomID)
			beforeQueue := readServerVoiceCapacity(t, ts.DB)
			require.Equal(t, int64(serverVoiceActorQueueLimit), beforeQueue.Rows)
			var beforeMember, beforePresence, beforeSession string
			require.NoError(t, ts.DB.QueryRow(`SELECT to_jsonb(member)::text FROM server_members AS member WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&beforeMember))
			require.NoError(t, ts.DB.QueryRow(`SELECT to_jsonb(participant)::text FROM voice_participants AS participant WHERE channel_id = $1 AND user_id = $2`, channelID, target.ID).Scan(&beforePresence))
			require.NoError(t, ts.DB.QueryRow(`SELECT to_jsonb(session)::text FROM voice_enforcement_sessions AS session WHERE room_id = $1 AND user_id = $2`, channelID, target.ID).Scan(&beforeSession))
			conn := connectVoiceWireClient(t, ts, owner)
			subscribeVoiceWireServer(t, conn, serverID)
			synchronizeVoiceWireClient(t, conn)

			response := ts.DoRequest(test.method, voiceEnforcePath(serverID, target.ID, test.suffix), nil,
				testhelpers.AuthHeaders(owner.AccessToken))
			requireServerVoiceCapacityRefusal(t, response)

			assert.Equal(t, beforeQueue, readServerVoiceCapacity(t, ts.DB), "refusal must preserve every retained command and both cursors")
			var afterMember, afterPresence, afterSession string
			require.NoError(t, ts.DB.QueryRow(`SELECT to_jsonb(member)::text FROM server_members AS member WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&afterMember))
			require.NoError(t, ts.DB.QueryRow(`SELECT to_jsonb(participant)::text FROM voice_participants AS participant WHERE channel_id = $1 AND user_id = $2`, channelID, target.ID).Scan(&afterPresence))
			require.NoError(t, ts.DB.QueryRow(`SELECT to_jsonb(session)::text FROM voice_enforcement_sessions AS session WHERE room_id = $1 AND user_id = $2`, channelID, target.ID).Scan(&afterSession))
			assert.Equal(t, beforeMember, afterMember, "saved policy and membership must roll back")
			assert.Equal(t, beforePresence, afterPresence, "refusal must preserve live presence")
			assert.Equal(t, beforeSession, afterSession, "refusal must preserve exact registered targets")
			requireNoServerVoiceCapacityBroadcast(t, conn, target.ID)
		})
	}
}

func TestVoiceCommandCapacity_ServerChargesCallerInsteadOfModeratedTarget(t *testing.T) {
	ts, owner, target, serverID, channelID := newServerOutboxFixture(t)
	seedOutboxVoiceSession(t, ts, channelID, target.ID)
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
	require.NoError(t, err)
	markerRoomID := ts.CreateDMConversation(t, owner.ID, target.ID)
	seedServerVoiceActorQueue(t, ts, target.ID, markerRoomID)
	conn := connectVoiceWireClient(t, ts, owner)
	subscribeVoiceWireServer(t, conn, serverID)
	synchronizeVoiceWireClient(t, conn)

	response := ts.DoRequest(http.MethodPost, voiceEnforcePath(serverID, target.ID, pathDeafen), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	requireOutboxQueued(t, response, http.StatusOK)
	queue := readServerVoiceCapacity(t, ts.DB)
	assert.Equal(t, int64(serverVoiceActorQueueLimit+1), queue.Rows)
	assert.Equal(t, int64(serverVoiceActorQueueLimit+1), queue.Allocated)
	assert.Zero(t, queue.Published)
	var muted, deafened bool
	require.NoError(t, ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&muted, &deafened))
	assert.True(t, muted)
	assert.True(t, deafened)
	// This positive control uses the same observer as the refusal cases; it
	// establishes that the subscription can see the effect those cases forbid.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		var envelope voiceWireEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		if envelope.Type == "voice_state_update" && envelope.Data["user_id"] == target.ID {
			assert.Equal(t, "server_deafened", envelope.Data["action"])
			return
		}
	}
}
