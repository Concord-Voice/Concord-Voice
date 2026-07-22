package voice_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPrivateLeaveHealsRetainedPeerWithExactTerminalLifecycle(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_terminal_peer_sender")
	peer := ts.CreateTestUser(t, "rp_terminal_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, sender.ID, peer.ID))
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 16, 2, 20, 0, 0, time.UTC)
	peerTerminalAt := joinedAt.Add(time.Second)
	leftAt := peerTerminalAt.Add(time.Second)
	senderID := uuid.MustParse(sender.ID)
	peerID := uuid.MustParse(peer.ID)

	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: senderID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, senderID, peerID, joinedAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, senderID, callID, joinedAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, joinedAt)
	claimed, err := sub.ClaimVoiceLifecycleForTest(
		context.Background(), presence.CategoryPrivateCall,
		peerID, callID, peerTerminalAt, false,
	)
	require.NoError(t, err)
	require.True(t, claimed)

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": sender.ID, "timestamp": leftAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), sender.ID,
	))
	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), peer.ID,
	), "an exact-terminal peer row must be healed instead of reactivated")
	peerLifecycleKey, err := presence.VoiceLifecycleKey(
		peerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	peerLifecycle, err := ts.Redis.HGetAll(
		context.Background(), peerLifecycleKey,
	).Result()
	require.NoError(t, err)
	require.Equal(t, callID.String(), peerLifecycle["token"])
	require.Equal(t, strconv.FormatInt(leftAt.UnixMicro(), 10), peerLifecycle["version"])
	require.Equal(t, "0", peerLifecycle["active"],
		"participant-set repair must never reactivate the exact terminal generation")
}

func TestPrivateLeaveAdvancesMissingPeerLifecycleThenRoomEmptyClearsIt(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_missing_peer_sender")
	peer := ts.CreateTestUser(t, "rp_missing_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, sender.ID, peer.ID))
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 16, 2, 21, 0, 0, time.UTC)
	leftAt := joinedAt.Add(time.Second)
	endedAt := leftAt.Add(time.Second)
	senderID := uuid.MustParse(sender.ID)
	peerID := uuid.MustParse(peer.ID)

	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: senderID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, senderID, peerID, joinedAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, senderID, callID, joinedAt)
	peerLifecycleKey, err := presence.VoiceLifecycleKey(
		peerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.Zero(t, ts.Redis.Exists(context.Background(), peerLifecycleKey).Val())

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": sender.ID, "timestamp": leftAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), sender.ID,
	))
	require.True(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), peer.ID,
	), "the retained database row is authoritative when its lifecycle is missing")
	var peerLifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT lifecycle_event_at
		FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, peerID).Scan(&peerLifecycleAt))
	require.Equal(t, leftAt.UnixMicro(), peerLifecycleAt.UnixMicro())
	peerLifecycle, err := ts.Redis.HGetAll(
		context.Background(), peerLifecycleKey,
	).Result()
	require.NoError(t, err)
	require.Equal(t, callID.String(), peerLifecycle["token"])
	require.Equal(t, strconv.FormatInt(leftAt.UnixMicro(), 10), peerLifecycle["version"])
	require.Equal(t, "1", peerLifecycle["active"])

	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId":       sender.ID,
		"participantUserIds": []string{sender.ID, peer.ID},
		"startedAt":          joinedAt.Format(time.RFC3339Nano),
		"timestamp":          endedAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), peer.ID,
	))
	peerLifecycle, err = ts.Redis.HGetAll(
		context.Background(), peerLifecycleKey,
	).Result()
	require.NoError(t, err)
	require.Equal(t, callID.String(), peerLifecycle["token"])
	require.Equal(t, strconv.FormatInt(endedAt.UnixMicro(), 10), peerLifecycle["version"])
	require.Equal(t, "0", peerLifecycle["active"])
}
