package dm_test

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

const dmVoiceActorQueueLimit = 1000

func seedDMVoiceActorQueue(t *testing.T, ts *testhelpers.TestServer, actorID, convID string) {
	t.Helper()
	specs := make([]voicecommand.CommandSpec, dmVoiceActorQueueLimit)
	for index := range specs {
		specs[index] = voicecommand.CommandSpec{
			Scope: "dm", Kind: "disconnect", Action: "disconnect", ChannelID: &convID,
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
	require.Len(t, commands, dmVoiceActorQueueLimit)
	require.NoError(t, tx.Commit())
}

type dmVoiceCapacitySnapshot struct {
	Rows, Allocated, Published int64
	Commands                   [32]byte
}

func readDMVoiceCapacity(t *testing.T, db *sql.DB) dmVoiceCapacitySnapshot {
	t.Helper()
	var snapshot dmVoiceCapacitySnapshot
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

func readDMVoiceCapacityDomain(t *testing.T, db *sql.DB, convID, targetID string) [32]byte {
	t.Helper()
	var state []byte
	require.NoError(t, db.QueryRow(`
		SELECT jsonb_build_object(
			'conversation', (SELECT to_jsonb(conversation) FROM dm_conversations AS conversation WHERE id = $1),
			'participants', (SELECT COALESCE(jsonb_agg(to_jsonb(participant) ORDER BY user_id), '[]'::jsonb)
				FROM dm_participants AS participant WHERE conversation_id = $1),
			'presence', (SELECT COALESCE(jsonb_agg(to_jsonb(participant) ORDER BY user_id), '[]'::jsonb)
				FROM dm_voice_participants AS participant WHERE conversation_id = $1),
			'sessions', (SELECT COALESCE(jsonb_agg(to_jsonb(session) ORDER BY session_generation, node_boot_id), '[]'::jsonb)
				FROM voice_enforcement_sessions AS session WHERE room_id = $1 AND user_id = $2),
			'ejections', (SELECT COALESCE(jsonb_agg(to_jsonb(ejection) ORDER BY to_jsonb(ejection)::text), '[]'::jsonb)
				FROM dm_block_voice_ejections AS ejection WHERE conversation_id = $1),
			'keys', (SELECT COALESCE(jsonb_agg(to_jsonb(wrapped_key_row) ORDER BY to_jsonb(wrapped_key_row)::text), '[]'::jsonb)
				FROM dm_channel_keys AS wrapped_key_row WHERE conversation_id = $1),
			'revocations', (SELECT COALESCE(jsonb_agg(to_jsonb(revocation) ORDER BY to_jsonb(revocation)::text), '[]'::jsonb)
				FROM dm_key_revocations AS revocation WHERE conversation_id = $1),
			'plans', (SELECT COALESCE(jsonb_agg(to_jsonb(plan) ORDER BY to_jsonb(plan)::text), '[]'::jsonb)
				FROM presence_active_pending_plans AS plan WHERE user_id = $2)
		)::text
	`, convID, targetID).Scan(&state))
	return sha256.Sum256(state)
}

func requireDMVoiceCapacityRefusal(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, "30", response.Header().Get("Retry-After"))
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "voice_command_queue_full", body["error_code"],
		"a post-commit presence-delivery 503 must not satisfy the queue-refusal oracle")
	assert.NotEqual(t, "queued", body["enforcement_status"])
	assert.NotEqual(t, true, body["applied"])
	assert.NotEqual(t, true, body["disconnected"])
}

type dmVoiceCapacityEnvelope struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

func connectDMVoiceCapacityObserver(t *testing.T, ts *testhelpers.TestServer, accessToken, convID string) *gorillaWS.Conn {
	t.Helper()
	ticketResponse := ts.DoRequest(http.MethodPost, "/api/v1/auth/ws-ticket", nil, testhelpers.AuthHeaders(accessToken))
	require.Equal(t, http.StatusOK, ticketResponse.Code, ticketResponse.Body.String())
	var ticketBody struct {
		Ticket string `json:"ticket"`
	}
	testhelpers.ParseJSON(t, ticketResponse, &ticketBody)
	require.NotEmpty(t, ticketBody.Ticket)
	ticket := ticketBody.Ticket
	server := httptest.NewServer(ts.Router)
	t.Cleanup(server.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+server.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	waitForDMVoiceCapacityType(t, conn, "connected")
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_dm", "data": map[string]interface{}{"conversation_id": convID},
	}))
	waitForDMVoiceCapacityType(t, conn, "dm_subscribed")
	return conn
}

func waitForDMVoiceCapacityType(t *testing.T, conn *gorillaWS.Conn, eventType string) dmVoiceCapacityEnvelope {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		var envelope dmVoiceCapacityEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		if envelope.Type == eventType {
			return envelope
		}
	}
}

func requireNoDMVoiceCapacityBroadcast(t *testing.T, conn *gorillaWS.Conn, convID string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	for {
		var envelope dmVoiceCapacityEnvelope
		if err := conn.ReadJSON(&envelope); err != nil {
			var timeout net.Error
			require.ErrorAs(t, err, &timeout, "the observer must survive the entire refusal window")
			require.True(t, timeout.Timeout())
			return
		}
		if envelope.Data["conversation_id"] != convID && envelope.Data["channel_id"] != convID {
			continue
		}
		switch envelope.Type {
		case "dm_voice_state_update", "dm_participant_removed", "dm_creator_changed", "key_revocation":
			t.Errorf("refused queue admission broadcast %s", envelope.Type)
		}
	}
}

func TestVoiceCommandCapacity_DMActorExhaustionRollsBackHTTPMutation(t *testing.T) {
	for _, test := range []struct {
		name, method, suffix        string
		beforeMuted, beforeDeafened bool
	}{
		{name: "mute", method: http.MethodPost, suffix: pathMute},
		{name: "unmute", method: http.MethodDelete, suffix: pathMute, beforeMuted: true},
		{name: "deafen", method: http.MethodPost, suffix: pathDeafen},
		{name: "undeafen", method: http.MethodDelete, suffix: pathDeafen, beforeMuted: true, beforeDeafened: true},
		{name: "user mute", method: http.MethodPost, suffix: pathUserMute},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, admin, target, convID := newDMOutboxFixture(t)
			seedDMOutboxSession(t, ts, convID, target.ID)
			seedDMVoiceParticipant(t, ts, convID, target.ID)
			_, err := ts.DB.Exec(`
				UPDATE dm_participants SET server_muted = $3, server_deafened = $4
				WHERE conversation_id = $1 AND user_id = $2
			`, convID, target.ID, test.beforeMuted, test.beforeDeafened)
			require.NoError(t, err)
			seedDMVoiceActorQueue(t, ts, admin.ID, convID)
			beforeQueue := readDMVoiceCapacity(t, ts.DB)
			require.Equal(t, int64(dmVoiceActorQueueLimit), beforeQueue.Rows)
			beforeDomain := readDMVoiceCapacityDomain(t, ts.DB, convID, target.ID)
			conn := connectDMVoiceCapacityObserver(t, ts, admin.AccessToken, convID)

			response := ts.DoRequest(test.method, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+test.suffix,
				nil, testhelpers.AuthHeaders(admin.AccessToken))
			requireDMVoiceCapacityRefusal(t, response)

			assert.Equal(t, beforeQueue, readDMVoiceCapacity(t, ts.DB), "refusal must leave retained commands and both cursors unchanged")
			assert.Equal(t, beforeDomain, readDMVoiceCapacityDomain(t, ts.DB, convID, target.ID),
				"saved policy, membership, presence and authority must roll back")
			requireNoDMVoiceCapacityBroadcast(t, conn, convID)
		})
	}
}

func TestVoiceCommandCapacity_DMRemovalExhaustionPreservesMembershipAndPresence(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "inactive provisional registration"
		if active {
			name = "active presence"
		}
		t.Run(name, func(t *testing.T) {
			ts, admin, target, convID := newDMOutboxFixture(t)
			seedDMOutboxSession(t, ts, convID, target.ID)
			if active {
				seedDMVoiceParticipant(t, ts, convID, target.ID)
			}
			ts.SeedDMKey(t, convID, admin.ID, 1)
			ts.SeedDMKey(t, convID, target.ID, 1)
			seedDMVoiceActorQueue(t, ts, admin.ID, convID)
			beforeQueue := readDMVoiceCapacity(t, ts.DB)
			beforeDomain := readDMVoiceCapacityDomain(t, ts.DB, convID, target.ID)
			conn := connectDMVoiceCapacityObserver(t, ts, target.AccessToken, convID)

			// SetupTestServer wires the real active-plan rail and sender gates.
			// Queue refusal must occur before post-commit presence delivery; its
			// exact error code distinguishes it from a delivery-pending 503.
			response := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+target.ID,
				nil, testhelpers.AuthHeaders(admin.AccessToken))
			requireDMVoiceCapacityRefusal(t, response)

			assert.Equal(t, beforeQueue, readDMVoiceCapacity(t, ts.DB), "removal refusal must consume no command position")
			assert.Equal(t, beforeDomain, readDMVoiceCapacityDomain(t, ts.DB, convID, target.ID),
				"membership, voice evidence, wrapped keys, revocations and captured presence plans must remain unchanged")
			requireNoDMVoiceCapacityBroadcast(t, conn, convID)
		})
	}
}

func TestVoiceCommandCapacity_DMChargesCallerInsteadOfModeratedTarget(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	seedDMOutboxSession(t, ts, convID, target.ID)
	seedDMVoiceParticipant(t, ts, convID, target.ID)
	seedDMVoiceActorQueue(t, ts, target.ID, convID)
	conn := connectDMVoiceCapacityObserver(t, ts, admin.AccessToken, convID)

	response := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathDeafen,
		nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)
	queue := readDMVoiceCapacity(t, ts.DB)
	assert.Equal(t, int64(dmVoiceActorQueueLimit+1), queue.Rows)
	assert.Equal(t, int64(dmVoiceActorQueueLimit+1), queue.Allocated)
	assert.Zero(t, queue.Published)
	var muted, deafened bool
	require.NoError(t, ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target.ID).Scan(&muted, &deafened))
	assert.True(t, muted)
	assert.True(t, deafened)
	// The same reader sees an allowed effect, so its negative cases cannot
	// pass merely because the observer is disconnected or unsubscribed.
	event := waitForDMVoiceCapacityType(t, conn, "dm_voice_state_update")
	assert.Equal(t, convID, event.Data["conversation_id"])
	assert.Equal(t, target.ID, event.Data["user_id"])
	assert.Equal(t, "server_deafened", event.Data["action"])
}

func TestVoiceCommandCapacity_DMRemovalSpareCallerBudgetBroadcastsCommit(t *testing.T) {
	ts, admin, target, convID := newDMOutboxFixture(t)
	seedDMOutboxSession(t, ts, convID, target.ID)
	seedDMVoiceActorQueue(t, ts, target.ID, convID)
	conn := connectDMVoiceCapacityObserver(t, ts, target.AccessToken, convID)

	response := ts.DoRequest(http.MethodDelete, pathDMConversationsPrefix+convID+pathMembersSlash+target.ID,
		nil, testhelpers.AuthHeaders(admin.AccessToken))
	requireDMOutboxQueued(t, response, http.StatusOK)
	var remains bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, target.ID).Scan(&remains))
	assert.False(t, remains)
	queue := readDMVoiceCapacity(t, ts.DB)
	assert.Equal(t, int64(dmVoiceActorQueueLimit+1), queue.Rows)
	assert.Equal(t, int64(dmVoiceActorQueueLimit+1), queue.Allocated)
	assert.Zero(t, queue.Published)
	// The removed member uses the same observer fixture as the two refusal
	// cases. A lost subscription or disconnected client must fail this control.
	event := waitForDMVoiceCapacityType(t, conn, "dm_participant_removed")
	assert.Equal(t, convID, event.Data["conversation_id"])
	assert.Equal(t, target.ID, event.Data["user_id"])
}
