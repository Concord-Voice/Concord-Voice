package voice_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #4179: terminal dm_voice_state_update events carry the call they belong to,
// so a client ringing a successor call can ignore a late event from the old one.
func TestBroadcastPrivateVoiceTerminalEvents_CarryCallID(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "dm_callid_caller")
	peer := ts.CreateTestUser(t, "dm_callid_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	conversationUUID := uuid.MustParse(conversationID)
	callID := uuid.New()

	conn := connectVoiceWireClient(t, ts, peer)
	synchronizeVoiceWireClient(t, conn)

	sub.BroadcastPrivateVoiceLeft(conversationUUID, uuid.MustParse(caller.ID), callID)
	left := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	require.Equal(t, "left", left.Data["action"])
	require.Equal(t, callID.String(), left.Data["call_id"])

	sub.BroadcastPrivateVoiceLeft(conversationUUID, uuid.MustParse(caller.ID), uuid.Nil)
	legacyLeft := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	_, hasCallID := legacyLeft.Data["call_id"]
	require.False(t, hasCallID, "an unknown call must omit call_id rather than send the nil UUID")

	sub.BroadcastPrivateVoiceRoomEmpty(conversationID, callID.String(), conversationUUID)
	empty := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	require.Equal(t, "room_empty", empty.Data["action"])
	require.Equal(t, callID.String(), empty.Data["call_id"])

	sub.BroadcastPrivateVoiceRoomEmpty(conversationID, "not-a-uuid", conversationUUID)
	malformed := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	_, hasCallID = malformed.Data["call_id"]
	require.False(t, hasCallID, "a malformed media-plane call id must not be forwarded")
}

// #4179: the media-plane "left" event's call id reaches the wire through the
// real handler, not only through the broadcaster.
func TestHandleLeft_DMVoiceStateUpdateCarriesCallID(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "dm_left_callid_caller")
	peer := ts.CreateTestUser(t, "dm_left_callid_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis,
		dm.VoiceCallLease{
			ConversationID: uuid.MustParse(conversationID),
			CallID:         callID,
			CallerUserID:   uuid.MustParse(caller.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))

	conn := connectVoiceWireClient(t, ts, peer)
	synchronizeVoiceWireClient(t, conn)
	joinedAt := time.Now().UTC()
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	joined := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	require.Equal(t, "joined", joined.Data["action"])

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": caller.ID, "timestamp": joinedAt.Add(time.Millisecond).Format(time.RFC3339Nano),
	}))
	left := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	require.Equal(t, "left", left.Data["action"])
	require.Equal(t, callID.String(), left.Data["call_id"])
}
