package voice_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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
	return voice.NewNATSSubscriber(ts.DB, log, ts.Hub, nil, resolver)
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

	insertDMVoiceParticipant(t, ts.DB, convID, user1.ID)
	insertDMVoiceParticipant(t, ts.DB, convID, user2.ID)

	// Heartbeat only reports user1 — user2 is stale
	event := map[string]interface{}{
		"channelId": convID,
		"userIds":   []string{user1.ID},
		"timestamp": "2026-03-30T00:00:00Z",
	}

	sub.HandleHeartbeat(mustJSON(t, event))

	if !dmVoiceParticipantExists(t, ts.DB, convID, user1.ID) {
		t.Error("expected user1 to remain after heartbeat")
	}
	if dmVoiceParticipantExists(t, ts.DB, convID, user2.ID) {
		t.Error("expected stale user2 to be removed after heartbeat")
	}
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

	sub := voice.NewNATSSubscriber(ts.DB, log, ts.Hub, nil, nil)
	if sub == nil {
		t.Fatal("NewNATSSubscriber returned nil")
	}
}
