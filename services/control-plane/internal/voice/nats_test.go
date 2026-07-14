package voice_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSubscriber creates a NATSSubscriber backed by the test server's DB and Hub.
// The NATS client is nil because tests call handler methods directly. A real resolver
// (backed by the test DB/Redis) is supplied so the #487 temp-SBAC cleanup path is
// exercised end-to-end.
func newTestSubscriber(ts *testhelpers.TestServer) *voice.NATSSubscriber {
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	return voice.NewNATSSubscriber(ts.DB, log, ts.Hub, nil, ts.Redis, resolver)
}

// countVoiceParticipants returns the number of rows in voice_participants for a channel.
func countVoiceParticipants(t *testing.T, db *sql.DB, channelID string) int {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1", channelID).Scan(&count)
	if err != nil {
		t.Fatalf("failed to count voice_participants: %v", err)
	}
	return count
}

// countDMVoiceParticipants returns the number of rows in dm_voice_participants for a conversation.
func countDMVoiceParticipants(t *testing.T, db *sql.DB, conversationID string) int {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM dm_voice_participants WHERE conversation_id = $1", conversationID).Scan(&count)
	if err != nil {
		t.Fatalf("failed to count dm_voice_participants: %v", err)
	}
	return count
}

// voiceParticipantExists checks whether a specific user is in voice_participants for a channel.
func voiceParticipantExists(t *testing.T, db *sql.DB, channelID, userID string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM voice_participants WHERE channel_id = $1 AND user_id = $2)",
		channelID, userID,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("failed to check voice_participant existence: %v", err)
	}
	return exists
}

// dmVoiceParticipantExists checks whether a specific user is in dm_voice_participants.
func dmVoiceParticipantExists(t *testing.T, db *sql.DB, conversationID, userID string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2)",
		conversationID, userID,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("failed to check dm_voice_participant existence: %v", err)
	}
	return exists
}

// insertVoiceParticipant directly inserts a voice participant row for test setup.
func insertVoiceParticipant(t *testing.T, db *sql.DB, channelID, userID string) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO voice_participants (channel_id, user_id, joined_at) VALUES ($1, $2, NOW())",
		channelID, userID,
	)
	if err != nil {
		t.Fatalf("failed to insert voice_participant: %v", err)
	}
}

// insertDMVoiceParticipant directly inserts a DM voice participant row for test setup.
func insertDMVoiceParticipant(t *testing.T, db *sql.DB, conversationID, userID string) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at) VALUES ($1, $2, NOW())",
		conversationID, userID,
	)
	if err != nil {
		t.Fatalf("failed to insert dm_voice_participant: %v", err)
	}
}

// mustJSON marshals v to JSON bytes, failing the test on error.
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return data
}

// ---------------------------------------------------------------------------
// resolveRoom tests
// ---------------------------------------------------------------------------

func TestResolveRoom_ServerChannel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "resolveroom_owner")
	serverID := ts.CreateTestServer(t, user.ID, "ResolveRoom Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-test")

	ctx, err := sub.ResolveRoom(channelID)
	if err != nil {
		t.Fatalf("resolveRoom returned error: %v", err)
	}
	if ctx == nil {
		t.Fatal("resolveRoom returned nil context")
	}
	if ctx.IsDM {
		t.Error("expected IsDM=false for server channel")
	}
	if ctx.ServerID != serverID {
		t.Errorf("expected serverID=%s, got %s", serverID, ctx.ServerID)
	}
}

func TestResolveRoom_DMConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user1 := ts.CreateTestUser(t, "resolvedm_user1")
	user2 := ts.CreateTestUser(t, "resolvedm_user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	ctx, err := sub.ResolveRoom(convID)
	if err != nil {
		t.Fatalf("resolveRoom returned error: %v", err)
	}
	if ctx == nil {
		t.Fatal("resolveRoom returned nil context")
	}
	if !ctx.IsDM {
		t.Error("expected IsDM=true for DM conversation")
	}
}

func TestResolveRoom_InvalidID(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	_, err := sub.ResolveRoom("00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Error("expected error for non-existent room, got nil")
	}
}

// ---------------------------------------------------------------------------
// handleJoined tests
// ---------------------------------------------------------------------------

func TestHandleJoined_ServerChannel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "joined_owner")
	serverID := ts.CreateTestServer(t, user.ID, "Joined Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-joined")

	event := map[string]interface{}{
		"channelId":   channelID,
		"userId":      user.ID,
		"username":    "joined_owner",
		"displayName": "Joined Owner",
		"timestamp":   "2026-03-30T00:00:00Z",
	}

	sub.HandleJoined(mustJSON(t, event))

	if !voiceParticipantExists(t, ts.DB, channelID, user.ID) {
		t.Error("expected voice participant to be inserted")
	}
}

func TestHandleJoined_DMConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user1 := ts.CreateTestUser(t, "joineddm_user1")
	user2 := ts.CreateTestUser(t, "joineddm_user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	event := map[string]interface{}{
		"channelId": convID,
		"callId":    uuid.New().String(),
		"userId":    user1.ID,
		"username":  "joineddm_user1",
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleJoined(mustJSON(t, event))

	if !dmVoiceParticipantExists(t, ts.DB, convID, user1.ID) {
		t.Error("expected DM voice participant to be inserted")
	}
}

func TestHandleJoined_DuplicateJoin(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "dupjoin_owner")
	serverID := ts.CreateTestServer(t, user.ID, "DupJoin Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-dup")

	event := map[string]interface{}{
		"channelId":   channelID,
		"userId":      user.ID,
		"username":    "dupjoin_owner",
		"displayName": "Dup Join",
		"timestamp":   "2026-03-30T00:00:00Z",
	}

	// Join twice — should not error, ON CONFLICT updates joined_at
	sub.HandleJoined(mustJSON(t, event))
	sub.HandleJoined(mustJSON(t, event))

	count := countVoiceParticipants(t, ts.DB, channelID)
	if count != 1 {
		t.Errorf("expected 1 participant after duplicate join, got %d", count)
	}
}

func TestHandleJoined_InvalidRoom(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "badjoin_user")

	event := map[string]interface{}{
		"channelId": "00000000-0000-0000-0000-000000000000",
		"userId":    user.ID,
		"username":  "badjoin_user",
		"timestamp": "2026-03-30T00:00:00Z",
	}

	// Should not panic; logs error and returns
	sub.HandleJoined(mustJSON(t, event))
}

func TestHandleJoined_InvalidJSON(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	// Should not panic; logs error and returns
	sub.HandleJoined([]byte(`{invalid json`))
}

// ---------------------------------------------------------------------------
// handleLeft tests
// ---------------------------------------------------------------------------

func TestHandleLeft_ServerChannel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "left_owner")
	serverID := ts.CreateTestServer(t, user.ID, "Left Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-left")

	// Pre-insert participant
	insertVoiceParticipant(t, ts.DB, channelID, user.ID)

	event := map[string]interface{}{
		"channelId": channelID,
		"userId":    user.ID,
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleLeft(mustJSON(t, event))

	if voiceParticipantExists(t, ts.DB, channelID, user.ID) {
		t.Error("expected voice participant to be deleted after leave")
	}
}

func TestHandleLeft_DMConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user1 := ts.CreateTestUser(t, "leftdm_user1")
	user2 := ts.CreateTestUser(t, "leftdm_user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Pre-insert DM participant
	insertDMVoiceParticipant(t, ts.DB, convID, user1.ID)

	event := map[string]interface{}{
		"channelId": convID,
		"userId":    user1.ID,
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleLeft(mustJSON(t, event))

	if dmVoiceParticipantExists(t, ts.DB, convID, user1.ID) {
		t.Error("expected DM voice participant to be deleted after leave")
	}
}

func TestHandleLeft_NotInRoom(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "leftnotinroom_owner")
	serverID := ts.CreateTestServer(t, user.ID, "LeftNotInRoom Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-notinroom")

	event := map[string]interface{}{
		"channelId": channelID,
		"userId":    user.ID,
		"timestamp": "2026-03-30T00:00:00Z",
	}

	// Should not panic or error — DELETE affects 0 rows
	sub.HandleLeft(mustJSON(t, event))
}

func TestHandleLeft_InvalidRoom(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	event := map[string]interface{}{
		"channelId": "00000000-0000-0000-0000-000000000000",
		"userId":    "00000000-0000-0000-0000-000000000001",
		"timestamp": "2026-03-30T00:00:00Z",
	}

	// Should not panic; logs error and returns
	sub.HandleLeft(mustJSON(t, event))
}

func TestHandleLeft_InvalidJSON(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sub.HandleLeft([]byte(`not json`))
}

// ---------------------------------------------------------------------------
// handleRoomEmpty tests
// ---------------------------------------------------------------------------

func TestHandleRoomEmpty_ServerChannel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user1 := ts.CreateTestUser(t, "empty_owner")
	user2 := ts.CreateTestUser(t, "empty_member")
	serverID := ts.CreateTestServer(t, user1.ID, "Empty Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-empty")

	// Pre-insert multiple participants
	insertVoiceParticipant(t, ts.DB, channelID, user1.ID)
	ts.AddMemberToServer(t, serverID, user2.ID, "member")
	insertVoiceParticipant(t, ts.DB, channelID, user2.ID)

	if countVoiceParticipants(t, ts.DB, channelID) != 2 {
		t.Fatal("expected 2 participants before room_empty")
	}

	event := map[string]interface{}{
		"channelId": channelID,
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleRoomEmpty(mustJSON(t, event))

	count := countVoiceParticipants(t, ts.DB, channelID)
	if count != 0 {
		t.Errorf("expected 0 participants after room_empty, got %d", count)
	}
}

func TestHandleRoomEmpty_DMConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user1 := ts.CreateTestUser(t, "emptydm_user1")
	user2 := ts.CreateTestUser(t, "emptydm_user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	insertDMVoiceParticipant(t, ts.DB, convID, user1.ID)
	insertDMVoiceParticipant(t, ts.DB, convID, user2.ID)

	if countDMVoiceParticipants(t, ts.DB, convID) != 2 {
		t.Fatal("expected 2 DM participants before room_empty")
	}

	event := map[string]interface{}{
		"channelId": convID,
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleRoomEmpty(mustJSON(t, event))

	count := countDMVoiceParticipants(t, ts.DB, convID)
	if count != 0 {
		t.Errorf("expected 0 DM participants after room_empty, got %d", count)
	}
}

func TestHandleRoomEmpty_DMSummarySurvivesLeavesAndIsIdempotent(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "emptydm_summary_caller")
	callee := ts.CreateTestUser(t, "emptydm_summary_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	callID := uuid.New().String()

	for _, participant := range []testhelpers.TestUser{caller, callee} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": convID,
			"callId":    callID,
			"userId":    participant.ID,
			"username":  participant.Username,
			"timestamp": "2026-07-14T12:00:00Z",
		}))
		sub.HandleLeft(mustJSON(t, map[string]interface{}{
			"channelId": convID,
			"callId":    callID,
			"userId":    participant.ID,
			"timestamp": "2026-07-14T12:01:00Z",
		}))
	}
	require.Zero(t, countDMVoiceParticipants(t, ts.DB, convID))

	roomEmpty := mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             callID,
		"callerUserId":       caller.ID,
		"participantUserIds": []string{caller.ID, callee.ID},
		"startedAt":          "2026-07-14T12:00:00Z",
		"timestamp":          "2026-07-14T12:01:00Z",
	})
	sub.HandleRoomEmpty(roomEmpty)
	sub.HandleRoomEmpty(roomEmpty)

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_messages WHERE id = $1`, callID).Scan(&count))
	assert.Equal(t, 1, count, "duplicate room-empty delivery must be idempotent")

	var payloadJSON []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT call_event_payload FROM dm_messages WHERE id = $1`, callID).Scan(&payloadJSON))
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(payloadJSON, &payload))
	assert.Equal(t, caller.ID, payload["caller_user_id"])
	participants, ok := payload["participant_user_ids"].([]interface{})
	require.True(t, ok)
	assert.ElementsMatch(t, []interface{}{caller.ID, callee.ID}, participants)
}

func TestHandleRoomEmpty_RingWithOnlyOneParticipantIsFailed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "emptydm_failed_caller")
	callee := ts.CreateTestUser(t, "emptydm_failed_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	callID := uuid.New().String()

	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             callID,
		"ringId":             callID,
		"callerUserId":       caller.ID,
		"participantUserIds": []string{caller.ID},
		"startedAt":          "2026-07-14T12:00:00Z",
		"timestamp":          "2026-07-14T12:00:02Z",
	}))

	var payloadJSON []byte
	require.NoError(t, ts.DB.QueryRow(
		`SELECT call_event_payload FROM dm_messages WHERE id = $1`, callID,
	).Scan(&payloadJSON))
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(payloadJSON, &payload))
	assert.Equal(t, "failed", payload["status"])
	assert.EqualValues(t, 2, payload["duration_seconds"])
}

func TestHandleRoomEmpty_DMConversationPreservesRingCaller(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "emptydm_caller")
	callee := ts.CreateTestUser(t, "emptydm_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	w := ts.DoRequest(
		"POST",
		"/api/v1/dm/conversations/"+convID+"/voice/ring",
		nil,
		testhelpers.AuthHeaders(caller.AccessToken),
	)
	require.Equal(t, http.StatusOK, w.Code, "caller starts the ring: %s", w.Body.String())

	w = ts.DoRequest(
		"POST",
		"/api/v1/dm/conversations/"+convID+"/voice/join",
		nil,
		testhelpers.AuthHeaders(callee.AccessToken),
	)
	require.Equal(t, http.StatusOK, w.Code, "callee accepts the ring: %s", w.Body.String())
	var acceptedResponse map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &acceptedResponse))
	callID, ok := acceptedResponse["call_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, callID)

	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
		VALUES ($1, $2, $4), ($1, $3, $5)
	`, convID, callee.ID, caller.ID, time.Now().Add(-time.Minute), time.Now())
	require.NoError(t, err)

	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             callID,
		"ringId":             callID,
		"callerUserId":       caller.ID,
		"participantUserIds": []string{callee.ID, caller.ID},
		"startedAt":          "2026-07-13T12:04:00Z",
		"timestamp":          "2026-07-13T12:05:00Z",
	}))

	var payloadJSON []byte
	err = ts.DB.QueryRow(`
		SELECT call_event_payload FROM dm_messages
		WHERE conversation_id = $1 AND type = 'call_event'
		ORDER BY created_at DESC LIMIT 1
	`, convID).Scan(&payloadJSON)
	require.NoError(t, err)
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(payloadJSON, &payload))
	assert.Equal(t, caller.ID, payload["caller_user_id"])
}

func TestHandleRoomEmpty_DMParticipantDeleteRetriesTransientFailure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "emptydm_retry_caller")
	callee := ts.CreateTestUser(t, "emptydm_retry_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(),
		ts.Redis,
		dm.VoiceCallLease{
			ConversationID: uuid.MustParse(convID),
			CallID:         callID,
			CallerUserID:   uuid.MustParse(caller.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))
	insertDMVoiceParticipant(t, ts.DB, convID, caller.ID)

	_, err := ts.DB.Exec(`
		CREATE SEQUENCE test_dm_voice_delete_attempt;
		CREATE FUNCTION test_fail_first_dm_voice_delete() RETURNS trigger AS $$
		BEGIN
			IF nextval('test_dm_voice_delete_attempt') = 1 THEN
				RAISE EXCEPTION 'simulated transient DM participant delete failure';
			END IF;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_first_dm_voice_delete
			BEFORE DELETE ON dm_voice_participants
			FOR EACH STATEMENT EXECUTE FUNCTION test_fail_first_dm_voice_delete();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_first_dm_voice_delete ON dm_voice_participants;
			DROP FUNCTION IF EXISTS test_fail_first_dm_voice_delete();
			DROP SEQUENCE IF EXISTS test_dm_voice_delete_attempt;
		`)
	})

	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             callID.String(),
		"callerUserId":       caller.ID,
		"participantUserIds": []string{caller.ID},
		"startedAt":          "2026-07-14T12:00:00Z",
		"timestamp":          "2026-07-14T12:01:00Z",
	}))

	var attempts int
	require.NoError(t, ts.DB.QueryRow(`SELECT last_value FROM test_dm_voice_delete_attempt`).Scan(&attempts))
	assert.Equal(t, 2, attempts, "the first delete fails and the guarded retry succeeds")
	assert.Zero(t, countDMVoiceParticipants(t, ts.DB, convID),
		"a one-shot delete failure must not leave presence that blocks a replacement ring")
}

func TestHandleRoomEmpty_InvalidRoom(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	event := map[string]interface{}{
		"channelId": "00000000-0000-0000-0000-000000000000",
		"timestamp": "2026-03-30T00:00:00Z",
	}

	// Should not panic; logs error and returns
	sub.HandleRoomEmpty(mustJSON(t, event))
}

func TestHandleRoomEmpty_InvalidJSON(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sub.HandleRoomEmpty([]byte(`!!!`))
}

// ---------------------------------------------------------------------------
// handleHeartbeat tests
// ---------------------------------------------------------------------------

func TestHandleHeartbeat_NoStaleParticipants(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "hb_owner")
	serverID := ts.CreateTestServer(t, user.ID, "HB Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hb")

	insertVoiceParticipant(t, ts.DB, channelID, user.ID)

	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{user.ID},
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleHeartbeat(mustJSON(t, event))

	// Participant should still exist — not stale
	if !voiceParticipantExists(t, ts.DB, channelID, user.ID) {
		t.Error("expected participant to remain after heartbeat with matching userIds")
	}
}

func TestHandleHeartbeat_RemovesStaleParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "hbstale_owner")
	staleUser := ts.CreateTestUser(t, "hbstale_stale")
	serverID := ts.CreateTestServer(t, owner.ID, "HBStale Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hbstale")

	ts.AddMemberToServer(t, serverID, staleUser.ID, "member")
	insertVoiceParticipant(t, ts.DB, channelID, owner.ID)
	insertVoiceParticipant(t, ts.DB, channelID, staleUser.ID)

	// Heartbeat only reports owner — staleUser should be removed
	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{owner.ID},
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleHeartbeat(mustJSON(t, event))

	if !voiceParticipantExists(t, ts.DB, channelID, owner.ID) {
		t.Error("expected owner to remain after heartbeat")
	}
	if voiceParticipantExists(t, ts.DB, channelID, staleUser.ID) {
		t.Error("expected stale user to be removed after heartbeat")
	}
}

func TestHandleHeartbeat_EmptyRoomClearsAll(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "hbempty_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "HBEmpty Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hbempty")

	insertVoiceParticipant(t, ts.DB, channelID, owner.ID)

	// Heartbeat with empty userIds — all DB entries are stale
	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{},
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleHeartbeat(mustJSON(t, event))

	count := countVoiceParticipants(t, ts.DB, channelID)
	if count != 0 {
		t.Errorf("expected 0 participants after empty heartbeat, got %d", count)
	}
}

func TestHandleHeartbeat_DMConversation_RemovesStale(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user1 := ts.CreateTestUser(t, "hbdm_user1")
	user2 := ts.CreateTestUser(t, "hbdm_user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	callID := uuid.New().String()

	insertDMVoiceParticipant(t, ts.DB, convID, user1.ID)
	insertDMVoiceParticipant(t, ts.DB, convID, user2.ID)

	// Heartbeat only reports user1 — user2 is stale
	event := map[string]interface{}{
		"channelId":    convID,
		"callId":       callID,
		"callerUserId": user1.ID,
		"userIds":      []string{user1.ID},
		"timestamp":    "2026-03-30T00:00:00Z",
	}

	sub.HandleHeartbeat(mustJSON(t, event))

	if !dmVoiceParticipantExists(t, ts.DB, convID, user1.ID) {
		t.Error("expected user1 to remain after heartbeat")
	}
	if dmVoiceParticipantExists(t, ts.DB, convID, user2.ID) {
		t.Error("expected stale user2 to be removed after heartbeat")
	}
}

func TestHandleHeartbeat_DMEmptyTerminatesCallLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "hbdm_empty_caller")
	callee := ts.CreateTestUser(t, "hbdm_empty_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	convUUID := uuid.MustParse(convID)
	callID := uuid.New()
	ringID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID,
		CallID:         callID,
		RingID:         ringID,
		CallerUserID:   uuid.MustParse(caller.ID),
	}, dm.DMVoiceCallLeaseTTL, true))
	insertDMVoiceParticipant(t, ts.DB, convID, caller.ID)
	insertDMVoiceParticipant(t, ts.DB, convID, callee.ID)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    convID,
		"callId":       callID.String(),
		"ringId":       ringID.String(),
		"callerUserId": caller.ID,
		"userIds":      []string{},
		"timestamp":    "2026-07-14T12:00:30Z",
	}))

	assert.False(t, dmVoiceParticipantExists(t, ts.DB, convID, caller.ID))
	assert.False(t, dmVoiceParticipantExists(t, ts.DB, convID, callee.ID))
	_, hasLease, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	assert.False(t, hasLease, "empty heartbeat is terminal and must clear the exact lease")
	err = dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID,
		CallID:         callID,
		CallerUserID:   uuid.MustParse(caller.ID),
	}, dm.DMVoiceCallLeaseTTL, true)
	assert.ErrorIs(t, err, dm.ErrDMVoiceCallLeaseClosed,
		"terminal heartbeat must tombstone the closed call ID")
	var callEventCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_messages
		WHERE conversation_id = $1 AND type = 'call_event'
	`, convID).Scan(&callEventCount))
	assert.Equal(t, 1, callEventCount, "terminal heartbeat preserves best-effort call history")

	// The authoritative room-empty snapshot may arrive after the heartbeat.
	// Both signals identify one call and must converge on one, richer row.
	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             callID.String(),
		"ringId":             ringID.String(),
		"callerUserId":       caller.ID,
		"participantUserIds": []string{caller.ID, callee.ID},
		"startedAt":          "2026-07-14T11:59:30Z",
		"timestamp":          "2026-07-14T12:00:31Z",
	}))
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_messages
		WHERE conversation_id = $1 AND type = 'call_event'
	`, convID).Scan(&callEventCount))
	assert.Equal(t, 1, callEventCount,
		"empty heartbeat followed by room-empty must remain one idempotent call event")
	var payloadJSON []byte
	require.NoError(t, ts.DB.QueryRow(`
		SELECT call_event_payload FROM dm_messages WHERE id = $1
	`, callID).Scan(&payloadJSON))
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(payloadJSON, &payload))
	assert.Equal(t, ringID.String(), payload["ring_id"])
	assert.Equal(t, caller.ID, payload["caller_user_id"])
	assert.ElementsMatch(t, []interface{}{caller.ID, callee.ID}, payload["participant_user_ids"])
	assert.Equal(t, "2026-07-14T11:59:30Z", payload["started_at"])
	assert.Equal(t, "2026-07-14T12:00:31Z", payload["ended_at"])
	assert.EqualValues(t, 61, payload["duration_seconds"],
		"the authoritative room-empty snapshot must upgrade the heartbeat fallback")
}

func TestHandleHeartbeat_DMEmptyWithoutCallIDFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "hbdm_idless_empty_caller")
	callee := ts.CreateTestUser(t, "hbdm_idless_empty_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	insertDMVoiceParticipant(t, ts.DB, convID, caller.ID)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"userIds":   []string{},
		"timestamp": "2026-07-14T12:00:30Z",
	}))

	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, caller.ID),
		"an uncorrelated empty heartbeat must not clear live presence")
	var callEventCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_messages
		WHERE conversation_id = $1 AND type = 'call_event'
	`, convID).Scan(&callEventCount))
	assert.Zero(t, callEventCount, "an uncorrelated heartbeat must not create call history")
}

func TestHandleHeartbeat_DMStaleEmptyPreservesReplacementCall(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "hbdm_stale_empty_caller")
	other := ts.CreateTestUser(t, "hbdm_stale_empty_other")
	convID := ts.CreateDMConversation(t, caller.ID, other.ID)
	convUUID := uuid.MustParse(convID)
	activeCallID := uuid.New()
	staleCallID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID,
		CallID:         activeCallID,
		CallerUserID:   uuid.MustParse(caller.ID),
	}, dm.DMVoiceCallLeaseTTL, true))
	insertDMVoiceParticipant(t, ts.DB, convID, caller.ID)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    convID,
		"callId":       staleCallID.String(),
		"callerUserId": other.ID,
		"userIds":      []string{},
		"timestamp":    "2026-07-14T12:00:30Z",
	}))

	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, caller.ID))
	lease, hasLease, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	require.True(t, hasLease)
	assert.Equal(t, activeCallID, lease.CallID,
		"stale empty heartbeat must not clear the replacement lease")
	var callEventCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_messages
		WHERE conversation_id = $1 AND type = 'call_event'
	`, convID).Scan(&callEventCount))
	assert.Zero(t, callEventCount, "stale empty heartbeat must not create fallback history")
}

func TestHandleHeartbeat_DMRestoresMissingLeaseAndPresence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "hbdm_restore_caller")
	callee := ts.CreateTestUser(t, "hbdm_restore_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	convUUID := uuid.MustParse(convID)
	callID := uuid.New()

	// Simulate an outage long enough for the lease to expire and for /ring's
	// bounded stale-presence cleanup to remove the old rows.
	require.NoError(t, ts.Redis.FlushDB(context.Background()).Err())
	_, err := ts.DB.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1`, convID)
	require.NoError(t, err)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    convID,
		"callId":       callID.String(),
		"callerUserId": caller.ID,
		"userIds":      []string{caller.ID, callee.ID},
		"timestamp":    "2026-07-14T12:00:00Z",
	}))

	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, caller.ID))
	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, callee.ID))
	lease, ok, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, callID, lease.CallID)
	assert.Equal(t, uuid.MustParse(caller.ID), lease.CallerUserID)
}

func TestHandleVoiceLifecycle_DMLeaseConflictFencesStaleEvents(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "hbdm_fence_caller")
	staleUser := ts.CreateTestUser(t, "hbdm_fence_stale")
	convID := ts.CreateDMConversation(t, caller.ID, staleUser.ID)
	activeCallID := uuid.New().String()
	staleCallID := uuid.New().String()

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"callId":    activeCallID,
		"userId":    caller.ID,
		"username":  caller.Username,
		"timestamp": "2026-07-14T12:00:00Z",
	}))

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"callId":    staleCallID,
		"userId":    staleUser.ID,
		"username":  staleUser.Username,
		"timestamp": "2026-07-14T12:00:01Z",
	}))
	assert.False(t, dmVoiceParticipantExists(t, ts.DB, convID, staleUser.ID))

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    convID,
		"callId":       staleCallID,
		"callerUserId": staleUser.ID,
		"userIds":      []string{staleUser.ID},
		"timestamp":    "2026-07-14T12:00:30Z",
	}))
	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, caller.ID))
	assert.False(t, dmVoiceParticipantExists(t, ts.DB, convID, staleUser.ID))
}

func TestHandleVoiceLifecycle_DMPendingRingFencesExpiredRoomReclaim(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "hbdm_pending_caller")
	callee := ts.CreateTestUser(t, "hbdm_pending_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	convUUID := uuid.MustParse(convID)
	staleCallID := uuid.New().String()

	ring := ts.DoRequest(
		"POST",
		"/api/v1/dm/conversations/"+convID+"/voice/ring",
		nil,
		testhelpers.AuthHeaders(caller.AccessToken),
	)
	require.Equal(t, http.StatusOK, ring.Code, "create replacement pending ring: %s", ring.Body.String())
	t.Cleanup(func() {
		_ = ts.DoRequest(
			"POST",
			"/api/v1/dm/conversations/"+convID+"/voice/cancel",
			nil,
			testhelpers.AuthHeaders(caller.AccessToken),
		)
	})
	require.True(t, dm.HasLocalPendingDMCall(convUUID))

	// Model the first events from an older media room after its 90-second lease
	// and presence rows expired while NATS was unavailable. Serialization alone
	// is insufficient here: the pending ring already exists before these arrive.
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"callId":    staleCallID,
		"userId":    callee.ID,
		"username":  callee.Username,
		"timestamp": "2026-07-14T12:00:00Z",
	}))
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    convID,
		"callId":       staleCallID,
		"callerUserId": callee.ID,
		"userIds":      []string{callee.ID},
		"timestamp":    "2026-07-14T12:00:30Z",
	}))

	assert.False(t, dmVoiceParticipantExists(t, ts.DB, convID, callee.ID))
	_, hasLease, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	assert.False(t, hasLease, "expired room must not reclaim the lease while a ring is pending")
	assert.True(t, dm.HasLocalPendingDMCall(convUUID), "replacement ring remains coherent")
}

func TestHandleVoiceLifecycle_DMRedisErrorFailsClosedBeforePresenceMutation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	activeUser := ts.CreateTestUser(t, "hbdm_redis_active")
	staleUser := ts.CreateTestUser(t, "hbdm_redis_stale")
	convID := ts.CreateDMConversation(t, activeUser.ID, staleUser.ID)
	insertDMVoiceParticipant(t, ts.DB, convID, activeUser.ID)

	brokenRedis := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = brokenRedis.Close() })
	sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, nil, brokenRedis, nil)
	callID := uuid.New().String()

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"callId":    callID,
		"userId":    staleUser.ID,
		"username":  staleUser.Username,
		"timestamp": "2026-07-14T12:00:00Z",
	}))
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    convID,
		"callId":       callID,
		"callerUserId": staleUser.ID,
		"userIds":      []string{staleUser.ID},
		"timestamp":    "2026-07-14T12:00:30Z",
	}))

	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, activeUser.ID),
		"unfenced heartbeat must not reconcile away current presence")
	assert.False(t, dmVoiceParticipantExists(t, ts.DB, convID, staleUser.ID),
		"unfenced lifecycle event must not publish stale presence")
}

func TestHandleVoiceLifecycle_DMStaleTerminalPreservesReplacementLiveState(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	activeCaller := ts.CreateTestUser(t, "emptydm_active_caller")
	oldCaller := ts.CreateTestUser(t, "emptydm_old_caller")
	convID := ts.CreateDMConversation(t, activeCaller.ID, oldCaller.ID)
	convUUID := uuid.MustParse(convID)
	activeCallID := uuid.New()
	oldCallID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID,
		CallID:         activeCallID,
		CallerUserID:   uuid.MustParse(activeCaller.ID),
	}, dm.DMVoiceCallLeaseTTL, true))
	insertDMVoiceParticipant(t, ts.DB, convID, activeCaller.ID)

	// ID-less legacy terminal events cannot prove ownership while an exact call
	// is active, so neither live cleanup nor the presence-derived fallback runs.
	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"timestamp": "2026-07-14T11:59:59Z",
	}))
	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, activeCaller.ID))

	// An exact delayed left from old call A must not remove a participant in B.
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"callId":    oldCallID.String(),
		"userId":    activeCaller.ID,
		"timestamp": "2026-07-14T12:00:00Z",
	}))
	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, activeCaller.ID))

	// The old call's self-contained summary is still valuable history, but its
	// terminal event must not clear or broadcast room_empty for replacement B.
	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          convID,
		"callId":             oldCallID.String(),
		"callerUserId":       oldCaller.ID,
		"participantUserIds": []string{oldCaller.ID},
		"startedAt":          "2026-07-14T11:59:00Z",
		"timestamp":          "2026-07-14T12:00:00Z",
	}))

	assert.True(t, dmVoiceParticipantExists(t, ts.DB, convID, activeCaller.ID))
	lease, ok, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, activeCallID, lease.CallID)
	var oldSummaryRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_messages WHERE id = $1`, oldCallID,
	).Scan(&oldSummaryRows))
	assert.Equal(t, 1, oldSummaryRows, "stale exact summary remains idempotent history")
}

func TestHandleJoined_DirectReservationPromotesToActiveLeaseTTL(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "direct_reservation_caller")
	callee := ts.CreateTestUser(t, "direct_reservation_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	joined := ts.DoRequest(
		"POST",
		"/api/v1/dm/conversations/"+convID+"/voice/join",
		nil,
		testhelpers.AuthHeaders(caller.AccessToken),
	)
	require.Equal(t, http.StatusOK, joined.Code, "reserve direct call: %s", joined.Body.String())
	var response map[string]interface{}
	require.NoError(t, json.Unmarshal(joined.Body.Bytes(), &response))
	callID, ok := response["call_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, callID)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	tokenDigest := sha256.Sum256([]byte(caller.AccessToken))
	payload := strings.Join([]string{
		"v1",
		timestamp,
		"POST",
		convID,
		callID,
		hex.EncodeToString(tokenDigest[:]),
	}, "\n")
	keyMAC := hmac.New(sha256.New, []byte(testhelpers.TestJWTSecret))
	_, _ = keyMAC.Write([]byte("concord/dm-voice-media-authorization/v1"))
	proofMAC := hmac.New(sha256.New, keyMAC.Sum(nil))
	_, _ = proofMAC.Write([]byte(payload))
	mediaHeaders := testhelpers.AuthHeaders(caller.AccessToken)
	mediaHeaders.Set("X-Concord-Media-Timestamp", timestamp)
	mediaHeaders.Set("X-Concord-Media-Proof", hex.EncodeToString(proofMAC.Sum(nil)))
	authorized := ts.DoRequest(
		"POST",
		"/api/v1/dm/conversations/"+convID+"/voice/authorize",
		map[string]interface{}{"call_id": callID},
		mediaHeaders,
	)
	require.Equal(t, http.StatusOK, authorized.Code, "authorize reserved call: %s", authorized.Body.String())

	leaseKey := "dm_voice_call_lease:" + convID
	reservationTTL, err := ts.Redis.PTTL(context.Background(), leaseKey).Result()
	require.NoError(t, err)
	assert.Positive(t, reservationTTL)
	assert.LessOrEqual(t, reservationTTL, dm.DMVoiceCallReservationTTL)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": convID,
		"callId":    callID,
		"userId":    caller.ID,
		"username":  caller.Username,
		"timestamp": "2026-07-14T12:00:00Z",
	}))

	activeTTL, err := ts.Redis.PTTL(context.Background(), leaseKey).Result()
	require.NoError(t, err)
	assert.Greater(t, activeTTL, dm.DMVoiceCallReservationTTL,
		"joined must promote the short authorization handoff to an active lease")
	assert.LessOrEqual(t, activeTTL, dm.DMVoiceCallLeaseTTL)
}

func TestHandleHeartbeat_NoDBParticipants_Noop(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "hbnoop_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "HBNoop Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hbnoop")

	// No participants in DB — heartbeat should be a no-op
	event := map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{owner.ID},
		"timestamp": "2026-03-30T00:00:00Z",
	}

	// Should not panic
	sub.HandleHeartbeat(mustJSON(t, event))
}

func TestHandleHeartbeat_InvalidRoom(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	event := map[string]interface{}{
		"channelId": "00000000-0000-0000-0000-000000000000",
		"userIds":   []string{},
		"timestamp": "2026-03-30T00:00:00Z",
	}

	// Should not panic; logs error and returns
	sub.HandleHeartbeat(mustJSON(t, event))
}

func TestHandleHeartbeat_InvalidJSON(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sub.HandleHeartbeat([]byte(`{`))
}

// ---------------------------------------------------------------------------
// reEnforceServer / reEnforceDM tests
// ---------------------------------------------------------------------------

func TestReEnforceServer(t *testing.T) {
	t.Run("NoPublishWhenNotFlagged", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		user := ts.CreateTestUser(t, "reenforce_noflag_owner")
		serverID := ts.CreateTestServer(t, user.ID, "ReEnforce NoFlag Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-reenforce-noflag")
		sub.ReEnforceServer(serverID, channelID, user.ID)
	})

	t.Run("QueriesFlagsWhenMuted", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		user := ts.CreateTestUser(t, "reenforce_muted_owner")
		serverID := ts.CreateTestServer(t, user.ID, "ReEnforce Muted Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-reenforce-muted")
		_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`, serverID, user.ID)
		require.NoError(t, err)
		sub.ReEnforceServer(serverID, channelID, user.ID)
	})

	t.Run("HandlesJoinedWithMutedFlag", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		user := ts.CreateTestUser(t, "reenforce_joined_owner")
		serverID := ts.CreateTestServer(t, user.ID, "ReEnforce Joined Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-reenforce-joined")
		_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`, serverID, user.ID)
		require.NoError(t, err)
		event := map[string]interface{}{
			"channelId": channelID, "userId": user.ID, "username": "reenforce_joined_owner",
			"displayName": "ReEnforce Owner", "timestamp": "2026-04-05T00:00:00Z",
		}
		sub.HandleJoined(mustJSON(t, event))
		assert.True(t, voiceParticipantExists(t, ts.DB, channelID, user.ID))
	})

	t.Run("InvalidUser", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		user := ts.CreateTestUser(t, "reenforce_invalid_owner")
		serverID := ts.CreateTestServer(t, user.ID, "ReEnforce Invalid Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-reenforce-invalid")
		sub.ReEnforceServer(serverID, channelID, "00000000-0000-0000-0000-000000000099")
	})
}

func TestReEnforceDM(t *testing.T) {
	t.Run("NoPublishWhenNotFlagged", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		user1 := ts.CreateTestUser(t, "reenforce_dm_user1")
		user2 := ts.CreateTestUser(t, "reenforce_dm_user2")
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		sub.ReEnforceDM(convID, user1.ID)
	})

	t.Run("QueriesFlagsWhenMuted", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		user1 := ts.CreateTestUser(t, "reenforce_dm_muted1")
		user2 := ts.CreateTestUser(t, "reenforce_dm_muted2")
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
		_, err := ts.DB.Exec(`UPDATE dm_participants SET server_muted = true WHERE conversation_id = $1 AND user_id = $2`, convID, user1.ID)
		require.NoError(t, err)
		sub.ReEnforceDM(convID, user1.ID)
	})
}

// ---------------------------------------------------------------------------
// NewNATSSubscriber constructor test
// ---------------------------------------------------------------------------

func TestNewNATSSubscriber(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	log := logger.New("test")

	sub := voice.NewNATSSubscriber(ts.DB, log, ts.Hub, nil, ts.Redis, nil)
	if sub == nil {
		t.Fatal("NewNATSSubscriber returned nil")
	}
}
