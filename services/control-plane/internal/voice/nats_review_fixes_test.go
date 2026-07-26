package voice_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type failRedisCommandHook struct {
	command string
	err     error
}

func (h failRedisCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h failRedisCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.command {
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (h failRedisCommandHook) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

func TestPrivateHeartbeatTerminalFencePreventsPostTerminalResurrection(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	heartbeatReplica := newTestSubscriber(ts)
	terminalReplica := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_terminal_fence_caller")
	peer := ts.CreateTestUser(t, "rp_terminal_fence_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	heartbeatAt := joinedAt.Add(time.Second)

	terminalReplica.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	connectVoiceWireClient(t, ts, peer)

	claimed := make(chan struct{})
	releaseHeartbeat := make(chan struct{})
	var claimedOnce sync.Once
	heartbeatReplica.SetVoiceLifecycleClaimedHookForTest(func(
		category presence.Category, _ uuid.UUID, _ time.Time,
	) {
		if category != presence.CategoryPrivateCall {
			return
		}
		claimedOnce.Do(func() { close(claimed) })
		<-releaseHeartbeat
	})
	heartbeatDone := make(chan struct{})
	heartbeatPayload := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{caller.ID, peer.ID},
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	})
	go func() {
		defer close(heartbeatDone)
		heartbeatReplica.HandleHeartbeat(heartbeatPayload)
	}()
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not pause after refreshing the exact lease")
	}

	terminalDone := make(chan struct{})
	terminalPayload := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"timestamp": heartbeatAt.Add(time.Microsecond).Format(time.RFC3339Nano),
	})
	go func() {
		defer close(terminalDone)
		terminalReplica.HandleDMRoomEmptyReplicaForTest(terminalPayload, conversationID)
	}()

	fencedBeforeRelease := false
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, found, lookupErr := dm.LookupDMVoiceCallLease(
			context.Background(), ts.Redis, conversationID,
		)
		require.NoError(t, lookupErr)
		if !found {
			fencedBeforeRelease = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(releaseHeartbeat)
	select {
	case <-heartbeatDone:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not finish after release")
	}
	select {
	case <-terminalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal cleanup did not finish")
	}

	require.True(t, fencedBeforeRelease,
		"terminal cleanup must fence Redis before waiting on participant mutation locks")
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), peer.ID))
	for _, participantID := range []string{caller.ID, peer.ID} {
		_, found, err := presence.NewActivityStore(ts.Redis).Get(
			context.Background(), uuid.MustParse(participantID), presence.CategoryPrivateCall,
		)
		require.NoError(t, err)
		require.False(t, found,
			"terminal cleanup must delete the exact stored generation even when its old-state build fails")
	}
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(peer.ID)) == 0
	}, time.Second, 10*time.Millisecond,
		"a losing heartbeat replica must not leave a joined frame usable")
}

func TestPrivateHeartbeatRejectsNonmembersBeforeLifecycleClaim(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_member_gate_caller")
	peer := ts.CreateTestUser(t, "rp_member_gate_peer")
	outsider := ts.CreateTestUser(t, "rp_member_gate_outsider")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 16, 2, 5, 0, 0, time.UTC)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{outsider.ID},
		"timestamp": eventAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), outsider.ID))
	lifecycleKey, err := presence.VoiceLifecycleKey(
		uuid.MustParse(outsider.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val(),
		"a nonmember must be rejected before a lifecycle watermark is created")
	_, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(outsider.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, found)
}

func TestPrivateJoinedRejectsNonmemberBeforeLeaseLifecycleAndFrame(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_join_member_caller")
	peer := ts.CreateTestUser(t, "rp_join_member_peer")
	outsider := ts.CreateTestUser(t, "rp_join_member_outsider")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 16, 2, 6, 0, 0, time.UTC)
	broadcasted := false
	sub.SetPrivateJoinHooksForTest(nil, func(_, _ uuid.UUID) { broadcasted = true })

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": outsider.ID, "username": outsider.Username,
		"timestamp": eventAt.Format(time.RFC3339Nano),
	}))

	_, found, err := dm.LookupDMVoiceCallLease(
		context.Background(), ts.Redis, conversationID,
	)
	require.NoError(t, err)
	require.False(t, found, "a rejected outsider join must not create a call lease")
	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), outsider.ID,
	))
	lifecycleKey, err := presence.VoiceLifecycleKey(
		uuid.MustParse(outsider.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val())
	_, found, err = presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(outsider.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, found)
	require.False(t, broadcasted, "a rejected outsider join must not emit a base frame")
}

func TestPrivateJoinedMembershipRemovalBeforeTransactionDoesNotPolluteLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_join_race_caller")
	peer := ts.CreateTestUser(t, "rp_join_race_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callerID := uuid.MustParse(caller.ID)
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 16, 2, 7, 0, 0, time.UTC)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallReservationTTL, false,
	))
	leaseKey := "dm_voice_call_lease:" + conversationID.String()
	leaseTTLBefore, err := ts.Redis.PTTL(context.Background(), leaseKey).Result()
	require.NoError(t, err)
	require.Positive(t, leaseTTLBefore)
	reachedMutation := make(chan struct{})
	releaseMutation := make(chan struct{})
	broadcasted := false
	sub.SetPrivateJoinHooksForTest(
		func(gotConversationID, gotSenderID uuid.UUID) {
			if gotConversationID != conversationID || gotSenderID != callerID {
				return
			}
			close(reachedMutation)
			<-releaseMutation
		},
		func(_, _ uuid.UUID) { broadcasted = true },
	)
	payload := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": eventAt.Format(time.RFC3339Nano),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub.HandleJoined(payload)
	}()
	select {
	case <-reachedMutation:
	case <-time.After(2 * time.Second):
		t.Fatal("private join did not pause before its guarded transaction")
	}
	result, err := ts.DB.Exec(`
		DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, caller.ID)
	require.NoError(t, err)
	removed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	close(releaseMutation)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("private join did not finish after the membership race was released")
	}

	lease, found, err := dm.LookupDMVoiceCallLease(
		context.Background(), ts.Redis, conversationID,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, callID, lease.CallID)
	require.Equal(t, callerID, lease.CallerUserID)
	leaseTTLAfter, err := ts.Redis.PTTL(context.Background(), leaseKey).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, leaseTTLAfter, leaseTTLBefore+100*time.Millisecond,
		"membership loss before the transaction must not promote or renew the reservation")
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	lifecycleKey, err := presence.VoiceLifecycleKey(callerID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val())
	_, found, err = presence.NewActivityStore(ts.Redis).Get(
		context.Background(), callerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, found)
	require.False(t, broadcasted, "a membership-race rejection must not emit a base frame")
}

func TestPrivateJoinedPostCommitActivityFailureStillBroadcasts(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "rp_join_build_failure_caller")
	peer := ts.CreateTestUser(t, "rp_join_build_failure_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callerID := uuid.MustParse(caller.ID)
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 16, 2, 7, 30, 0, time.UTC)

	builderRedis := redis.NewClient(ts.Redis.Options())
	t.Cleanup(func() { require.NoError(t, builderRedis.Close()) })
	builderRedis.AddHook(failRedisCommandHook{
		command: "hgetall", err: errors.New("forced post-commit lease build failure"),
	})
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	activityStore := presence.NewActivityStore(ts.Redis)
	activity := presence.NewActivityService(
		ts.PresenceHistory,
		presence.NewActivityBuilder(
			ts.DB, testCallLeaseVerifier{redis: builderRedis}, activityStore,
		),
		activityStore,
		ts.DB,
		resolver,
		ts.Hub,
		permitAllPresence{},
	)
	sub := voice.NewNATSSubscriber(
		ts.DB, log, ts.Hub, nil, ts.Redis, resolver, activity,
	)
	broadcasted := false
	sub.SetPrivateJoinHooksForTest(nil, func(_, _ uuid.UUID) { broadcasted = true })

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": eventAt.Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID),
		"the injected builder failure occurs after the guarded row commits")
	lease, found, err := dm.LookupDMVoiceCallLease(
		context.Background(), ts.Redis, conversationID,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, callID, lease.CallID)
	_, found, err = activityStore.Get(
		context.Background(), callerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, found)
	require.True(t, broadcasted,
		"a post-commit Rich Presence failure must not suppress the durable joined base frame")
}

func TestPrivateHeartbeatExactAllNonmembersHealsSeededGhost(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ghost := ts.CreateTestUser(t, "rp_exact_nonmember_ghost")
	peer := ts.CreateTestUser(t, "rp_exact_nonmember_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, ghost.ID, peer.ID))
	ghostID := uuid.MustParse(ghost.ID)
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 16, 2, 8, 0, 0, time.UTC)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID,
			CallerUserID: uuid.MustParse(peer.ID),
		}, dm.DMVoiceCallReservationTTL, false,
	))
	leaseKey := "dm_voice_call_lease:" + conversationID.String()
	leaseTTLBefore, err := ts.Redis.PTTL(context.Background(), leaseKey).Result()
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, conversationID, ghost.ID, eventAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, ghostID, callID, eventAt)
	_, err = ts.DB.Exec(`
		DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, ghost.ID)
	require.NoError(t, err)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": peer.ID, "userIds": []string{ghost.ID},
		"timestamp": eventAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), ghost.ID))
	_, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), ghostID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, found, "an exact heartbeat must fail closed and remove ghost activity")
	lifecycleKey, err := presence.VoiceLifecycleKey(ghostID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	require.Equal(t, "0", ts.Redis.HGet(
		context.Background(), lifecycleKey, "active",
	).Val())
	leaseTTLAfter, err := ts.Redis.PTTL(context.Background(), leaseKey).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, leaseTTLAfter, leaseTTLBefore+100*time.Millisecond,
		"an all-nonmember heartbeat must verify, not renew, the existing exact lease")
}

func TestPrivateHeartbeatStaleAllRejectedPreservesNewerLegitimateState(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_stale_rejected_caller")
	peer := ts.CreateTestUser(t, "rp_stale_rejected_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callerID := uuid.MustParse(caller.ID)
	callID := uuid.New()
	staleAt := time.Date(2026, 7, 16, 2, 9, 0, 0, time.UTC)
	newerAt := staleAt.Add(time.Second)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $3, TRUE), ($2, TRUE, $3, TRUE)
	`, caller.ID, peer.ID, presence.TierOff)
	require.NoError(t, err)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err = ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, caller.ID, peer.ID, newerAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, callerID, callID, newerAt)
	seedPrivateVoiceGeneration(t, ts, uuid.MustParse(peer.ID), callID, newerAt)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{caller.ID},
		"timestamp": staleAt.Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), peer.ID))
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), callerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, newerAt.UnixMicro(), state.SourceVersion)
	lifecycleKey, err := presence.VoiceLifecycleKey(callerID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	require.Equal(t, "1", ts.Redis.HGet(
		context.Background(), lifecycleKey, "active",
	).Val())
}

func seedPrivateVoiceGeneration(
	t *testing.T,
	ts *testhelpers.TestServer,
	participantID, callID uuid.UUID,
	eventAt time.Time,
) {
	t.Helper()
	lifecycleKey, err := presence.VoiceLifecycleKey(
		participantID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.HSet(
		context.Background(), lifecycleKey,
		"token", callID.String(), "version", eventAt.UnixMicro(), "active", "1",
	).Err())
	require.NoError(t, ts.Redis.PExpire(
		context.Background(), lifecycleKey, presence.ActivityStateTTL,
	).Err())
	stored, err := presence.NewActivityStore(ts.Redis).CompareAndSetActive(
		context.Background(), participantID, presence.CategoryPrivateCall,
		presence.ActivityState{
			SourceToken: callID, SourceVersion: eventAt.UnixMicro(),
			Payload:   json.RawMessage(`{"call_type":"dm","participant_count":1}`),
			UpdatedAt: eventAt.Unix(),
		},
	)
	require.NoError(t, err)
	require.True(t, stored)
}

func TestPrivateParticipantSetBatchMalformedEntryDoesNotAdvanceValidPeer(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	validID := uuid.New()
	poisonedID := uuid.New()
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 10, 0, time.UTC)
	seedPrivateVoiceGeneration(t, ts, validID, callID, oldAt)
	poisonedKey, err := presence.VoiceLifecycleKey(
		poisonedID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(
		context.Background(), poisonedKey, "not-a-lifecycle-hash", presence.ActivityStateTTL,
	).Err())
	validKey, err := presence.VoiceLifecycleKey(validID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	before, err := ts.Redis.HGetAll(context.Background(), validKey).Result()
	require.NoError(t, err)
	ttlBefore, err := ts.Redis.PTTL(context.Background(), validKey).Result()
	require.NoError(t, err)

	accepted, duplicate, err := sub.ClaimPrivateVoiceLifecyclesForTest(
		context.Background(),
		[]voice.TestPrivateVoiceLifecycleClaim{
			{UserID: validID, Token: callID, Version: oldAt.Add(time.Second).UnixMicro(), Active: true},
			{UserID: poisonedID, Token: callID, Version: oldAt.Add(time.Second).UnixMicro(), Active: true},
		},
	)
	require.ErrorContains(t, err, "malformed voice lifecycle watermark")
	require.False(t, accepted)
	require.False(t, duplicate)
	require.Zero(t, ts.Redis.Exists(context.Background(), poisonedKey).Val())
	after, err := ts.Redis.HGetAll(context.Background(), validKey).Result()
	require.NoError(t, err)
	require.Equal(t, before, after)
	ttlAfter, err := ts.Redis.PTTL(context.Background(), validKey).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, ttlAfter, ttlBefore,
		"a poisoned peer must not renew any valid participant")
}

func TestPrivateParticipantSetBatchStaleEntryRejectsEveryPeer(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	newerID := uuid.New()
	peerID := uuid.New()
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 20, 0, time.UTC)
	incomingAt := oldAt.Add(time.Second)
	newerAt := incomingAt.Add(time.Second)
	seedPrivateVoiceGeneration(t, ts, newerID, callID, newerAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, oldAt)
	peerKey, err := presence.VoiceLifecycleKey(peerID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	peerBefore, err := ts.Redis.HGetAll(context.Background(), peerKey).Result()
	require.NoError(t, err)
	ttlBefore, err := ts.Redis.PTTL(context.Background(), peerKey).Result()
	require.NoError(t, err)

	accepted, duplicate, err := sub.ClaimPrivateVoiceLifecyclesForTest(
		context.Background(),
		[]voice.TestPrivateVoiceLifecycleClaim{
			{UserID: newerID, Token: callID, Version: incomingAt.UnixMicro(), Active: true},
			{UserID: peerID, Token: callID, Version: incomingAt.UnixMicro(), Active: true},
		},
	)
	require.NoError(t, err)
	require.False(t, accepted)
	require.False(t, duplicate)
	peerAfter, err := ts.Redis.HGetAll(context.Background(), peerKey).Result()
	require.NoError(t, err)
	require.Equal(t, peerBefore, peerAfter)
	ttlAfter, err := ts.Redis.PTTL(context.Background(), peerKey).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, ttlAfter, ttlBefore,
		"a rejected batch must not renew an otherwise acceptable peer")
}

func TestPrivateJoinExactReplayRepairsDatabaseAfterPostClaimFailure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_set_replay_caller")
	peer := ts.CreateTestUser(t, "rp_set_replay_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 25, 0, time.UTC)
	joinAt := oldAt.Add(time.Second)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, conversationID, peerID, oldAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, oldAt)
	_, err = ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION test_fail_private_set_replay()
		RETURNS trigger AS $fn$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM users
				WHERE id = NEW.user_id AND username = 'rp_set_replay_caller'
			) THEN
				RAISE EXCEPTION 'forced post-claim participant failure';
			END IF;
			RETURN NEW;
		END;
		$fn$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_private_set_replay_trigger
		BEFORE INSERT ON dm_voice_participants
		FOR EACH ROW EXECUTE FUNCTION test_fail_private_set_replay();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_private_set_replay_trigger ON dm_voice_participants;
			DROP FUNCTION IF EXISTS test_fail_private_set_replay();
		`)
	})
	payload := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": joinAt.Format(time.RFC3339Nano),
	})

	sub.HandleJoined(payload)
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	peerLifecycleKey, err := presence.VoiceLifecycleKey(peerID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d", joinAt.UnixMicro()), ts.Redis.HGet(
		context.Background(), peerLifecycleKey, "version",
	).Val(), "the accepted Redis batch remains a fail-closed replay fence")
	_, err = ts.DB.Exec(`
		DROP TRIGGER test_fail_private_set_replay_trigger ON dm_voice_participants;
		DROP FUNCTION test_fail_private_set_replay();
	`)
	require.NoError(t, err)

	sub.HandleJoined(payload)
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	for _, participantID := range []uuid.UUID{callerID, peerID} {
		var lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRow(`
			SELECT lifecycle_event_at
			FROM dm_voice_participants
			WHERE conversation_id = $1 AND user_id = $2
		`, conversationID, participantID).Scan(&lifecycleAt))
		require.Equal(t, joinAt.UnixMicro(), lifecycleAt.UnixMicro())
	}
}

func TestPrivateTerminalHealsFailedCrossScopeJoinWithoutRebindingSuccessor(t *testing.T) {
	for _, terminal := range []string{"peer_left", "room_empty"} {
		t.Run(terminal, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			mover := ts.CreateTestUser(t, "rp_failed_move_mover_"+terminal)
			oldPeer := ts.CreateTestUser(t, "rp_failed_move_old_peer_"+terminal)
			newPeer := ts.CreateTestUser(t, "rp_failed_move_new_peer_"+terminal)
			oldConversationID := uuid.MustParse(ts.CreateDMConversation(
				t, mover.ID, oldPeer.ID,
			))
			newConversationID := uuid.MustParse(ts.CreateDMConversation(
				t, mover.ID, newPeer.ID,
			))
			oldCallID := uuid.New()
			newCallID := uuid.New()
			oldAt := time.Date(2026, 7, 16, 2, 9, 27, 0, time.UTC)
			moveAt := oldAt.Add(time.Second)
			terminalAt := moveAt.Add(time.Second)
			moverID := uuid.MustParse(mover.ID)
			oldPeerID := uuid.MustParse(oldPeer.ID)
			newPeerID := uuid.MustParse(newPeer.ID)
			ctx := context.Background()

			for _, lease := range []dm.VoiceCallLease{
				{ConversationID: oldConversationID, CallID: oldCallID, CallerUserID: moverID},
				{ConversationID: newConversationID, CallID: newCallID, CallerUserID: moverID},
			} {
				require.NoError(t, dm.RefreshDMVoiceCallLease(
					ctx, ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
				))
			}
			_, err := ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES
					($1, $3, $5, $5), ($1, $4, $5, $5),
					($2, $6, $5, $5)
			`, oldConversationID, newConversationID, moverID, oldPeerID, oldAt, newPeerID)
			require.NoError(t, err)
			seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, oldAt)
			seedPrivateVoiceGeneration(t, ts, oldPeerID, oldCallID, oldAt)
			seedPrivateVoiceGeneration(t, ts, newPeerID, newCallID, oldAt)

			_, err = ts.DB.Exec(`
				CREATE OR REPLACE FUNCTION test_fail_private_scope_successor()
				RETURNS trigger AS $fn$
				BEGIN
					IF EXISTS (
						SELECT 1 FROM users
						WHERE id = NEW.user_id
						  AND username LIKE 'rp_failed_move_mover_%'
					) THEN
						RAISE EXCEPTION 'forced post-claim cross-scope failure';
					END IF;
					RETURN NEW;
				END;
				$fn$ LANGUAGE plpgsql;
				CREATE TRIGGER test_fail_private_scope_successor_trigger
				BEFORE INSERT ON dm_voice_participants
				FOR EACH ROW EXECUTE FUNCTION test_fail_private_scope_successor();
			`)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = ts.DB.Exec(`
					DROP TRIGGER IF EXISTS test_fail_private_scope_successor_trigger
						ON dm_voice_participants;
					DROP FUNCTION IF EXISTS test_fail_private_scope_successor();
				`)
			})

			sub.HandleJoined(mustJSON(t, map[string]interface{}{
				"channelId": newConversationID.String(), "callId": newCallID.String(),
				"userId": mover.ID, "username": mover.Username,
				"timestamp": moveAt.Format(time.RFC3339Nano),
			}))
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			), "the failed PostgreSQL commit must retain the stale old-scope row")
			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			))
			moverLifecycleKey, err := presence.VoiceLifecycleKey(
				moverID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.Equal(t, newCallID.String(), ts.Redis.HGet(
				ctx, moverLifecycleKey, "token",
			).Val(), "the Redis lifecycle batch must succeed before PostgreSQL fails")
			require.Equal(t, fmt.Sprintf("%d", moveAt.UnixMicro()), ts.Redis.HGet(
				ctx, moverLifecycleKey, "version",
			).Val())

			_, err = ts.DB.Exec(`
				DROP TRIGGER test_fail_private_scope_successor_trigger
					ON dm_voice_participants;
				DROP FUNCTION test_fail_private_scope_successor();
			`)
			require.NoError(t, err)
			_, err = ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, newConversationID, moverID, moveAt)
			require.NoError(t, err)
			successorState := presence.ActivityState{
				SourceToken: newCallID, SourceVersion: moveAt.UnixMicro(),
				Payload: json.RawMessage(
					`{"call_type":"dm","participant_count":2,"marker":"successor-b"}`,
				),
				UpdatedAt: moveAt.Unix(),
			}
			store := presence.NewActivityStore(ts.Redis)
			stored, err := store.CompareAndSetActive(
				ctx, moverID, presence.CategoryPrivateCall, successorState,
			)
			require.NoError(t, err)
			require.True(t, stored)
			lifecycleBefore, err := ts.Redis.HGetAll(ctx, moverLifecycleKey).Result()
			require.NoError(t, err)
			activityKey := "presence:rich:" + mover.ID + ":" +
				string(presence.CategoryPrivateCall)
			activityBefore, err := ts.Redis.Get(ctx, activityKey).Bytes()
			require.NoError(t, err)
			connectVoiceWireClient(t, ts, oldPeer)

			switch terminal {
			case "peer_left":
				sub.HandleLeft(mustJSON(t, map[string]interface{}{
					"channelId": oldConversationID.String(), "callId": oldCallID.String(),
					"userId":    oldPeer.ID,
					"timestamp": terminalAt.Format(time.RFC3339Nano),
				}))
			case "room_empty":
				sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
					"channelId": oldConversationID.String(), "callId": oldCallID.String(),
					"callerUserId":       mover.ID,
					"participantUserIds": []string{mover.ID, oldPeer.ID},
					"startedAt":          oldAt.Format(time.RFC3339Nano),
					"timestamp":          terminalAt.Format(time.RFC3339Nano),
				}))
			}

			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			), "the terminal must heal the mover's retained stale A row")
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			), "healing A must preserve the mover's authoritative B row")
			var successorLifecycleAt time.Time
			require.NoError(t, ts.DB.QueryRow(`
				SELECT lifecycle_event_at
				FROM dm_voice_participants
				WHERE conversation_id = $1 AND user_id = $2
			`, newConversationID, moverID).Scan(&successorLifecycleAt))
			require.Equal(t, moveAt.UnixMicro(), successorLifecycleAt.UnixMicro(),
				"the A terminal must not advance or rebind the B database generation")
			require.Equal(t, lifecycleBefore, ts.Redis.HGetAll(
				ctx, moverLifecycleKey,
			).Val(), "the terminal must not claim the mover back to A")
			activityAfter, err := ts.Redis.Get(ctx, activityKey).Bytes()
			require.NoError(t, err)
			require.Equal(t, activityBefore, activityAfter,
				"the B successor ActivityState must remain byte-for-byte identical")
			currentState, found, err := store.Get(
				ctx, moverID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, successorState, currentState)
			require.Eventually(t, func() bool {
				return ts.Hub.GetUserClientCount(oldPeerID) == 0
			}, time.Second, 10*time.Millisecond,
				"healing unknown old-scope state must conservatively reconnect clients")
		})
	}
}

func TestPrivateCallJoinAdvancesEveryParticipantSetGeneration(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	mutationReplica := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_set_join_caller")
	peer := ts.CreateTestUser(t, "rp_set_join_peer")
	joiner := ts.CreateTestUser(t, "rp_set_join_joiner")
	conversationID := uuid.MustParse(ts.CreateGroupDMConversation(
		t, caller.ID, peer.ID, joiner.ID,
	))
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 30, 0, time.UTC)
	joinAt := oldAt.Add(time.Second)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)
	joinerID := uuid.MustParse(joiner.ID)

	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, callerID, peerID, oldAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, callerID, callID, oldAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, oldAt)

	mutationReplica.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": joiner.ID, "username": joiner.Username,
		"timestamp": joinAt.Format(time.RFC3339Nano),
	}))

	for _, participantID := range []uuid.UUID{callerID, peerID, joinerID} {
		var lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRow(`
			SELECT lifecycle_event_at
			FROM dm_voice_participants
			WHERE conversation_id = $1 AND user_id = $2
		`, conversationID, participantID).Scan(&lifecycleAt))
		require.Equal(t, joinAt.UnixMicro(), lifecycleAt.UnixMicro(),
			"a join changes the set revision for every post-set participant")
	}
	assertStalePrivateActivityRejectedAfterCurrentDelete(
		t, ts, callerID, callID, oldAt,
	)
}

func TestPrivateCallLeaveAdvancesEveryRemainingParticipantSetGeneration(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	mutationReplica := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_set_leave_caller")
	peer := ts.CreateTestUser(t, "rp_set_leave_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 40, 0, time.UTC)
	leaveAt := oldAt.Add(time.Second)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)

	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, callerID, peerID, oldAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, callerID, callID, oldAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, oldAt)

	mutationReplica.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId":    peer.ID,
		"timestamp": leaveAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), peer.ID,
	))
	var lifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT lifecycle_event_at
		FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, callerID).Scan(&lifecycleAt))
	require.Equal(t, leaveAt.UnixMicro(), lifecycleAt.UnixMicro(),
		"a leave changes the set revision for every remaining participant")
	assertStalePrivateActivityRejectedAfterCurrentDelete(
		t, ts, callerID, callID, oldAt,
	)
}

func TestPrivateCallCrossScopeJoinAdvancesOldAndNewParticipantSets(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	mutationReplica := newTestSubscriber(ts)
	mover := ts.CreateTestUser(t, "rp_scope_fence_mover")
	oldPeer := ts.CreateTestUser(t, "rp_scope_fence_old_peer")
	newPeer := ts.CreateTestUser(t, "rp_scope_fence_new_peer")
	oldConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, oldPeer.ID))
	newConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, newPeer.ID))
	oldCallID := uuid.New()
	newCallID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 50, 0, time.UTC)
	moveAt := oldAt.Add(time.Second)
	moverID := uuid.MustParse(mover.ID)
	oldPeerID := uuid.MustParse(oldPeer.ID)
	newPeerID := uuid.MustParse(newPeer.ID)

	for _, lease := range []dm.VoiceCallLease{
		{ConversationID: oldConversationID, CallID: oldCallID, CallerUserID: moverID},
		{ConversationID: newConversationID, CallID: newCallID, CallerUserID: moverID},
	} {
		require.NoError(t, dm.RefreshDMVoiceCallLease(
			context.Background(), ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
		))
	}
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES
			($1, $3, $5, $5), ($1, $4, $5, $5),
			($2, $6, $5, $5)
	`, oldConversationID, newConversationID, moverID, oldPeerID, oldAt, newPeerID)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, oldAt)
	seedPrivateVoiceGeneration(t, ts, oldPeerID, oldCallID, oldAt)
	seedPrivateVoiceGeneration(t, ts, newPeerID, newCallID, oldAt)

	mutationReplica.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": newConversationID.String(), "callId": newCallID.String(),
		"userId": mover.ID, "username": mover.Username,
		"timestamp": moveAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, oldConversationID.String(), mover.ID,
	))
	require.True(t, dmVoiceParticipantExists(
		t, ts.DB, oldConversationID.String(), oldPeer.ID,
	))
	for _, participant := range []struct {
		conversationID uuid.UUID
		userID         uuid.UUID
	}{
		{oldConversationID, oldPeerID},
		{newConversationID, moverID},
		{newConversationID, newPeerID},
	} {
		var lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRow(`
			SELECT lifecycle_event_at
			FROM dm_voice_participants
			WHERE conversation_id = $1 AND user_id = $2
		`, participant.conversationID, participant.userID).Scan(&lifecycleAt))
		require.Equal(t, moveAt.UnixMicro(), lifecycleAt.UnixMicro())
	}
	assertStalePrivateActivityRejectedAfterCurrentDelete(
		t, ts, oldPeerID, oldCallID, oldAt,
	)
}

func TestPrivateCallCrossScopeJoinPreservesOldPeerSuccessorLifecycle(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	mover := ts.CreateTestUser(t, "rp_scope_successor_mover")
	oldPeer := ts.CreateTestUser(t, "rp_scope_successor_old_peer")
	newPeer := ts.CreateTestUser(t, "rp_scope_successor_new_peer")
	oldConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, oldPeer.ID))
	newConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, newPeer.ID))
	oldCallID := uuid.New()
	newCallID := uuid.New()
	peerSuccessorCallID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 52, 0, time.UTC)
	moveAt := oldAt.Add(time.Second)
	peerSuccessorAt := moveAt.Add(time.Second)
	moverID := uuid.MustParse(mover.ID)
	oldPeerID := uuid.MustParse(oldPeer.ID)
	newPeerID := uuid.MustParse(newPeer.ID)
	for _, lease := range []dm.VoiceCallLease{
		{ConversationID: oldConversationID, CallID: oldCallID, CallerUserID: moverID},
		{ConversationID: newConversationID, CallID: newCallID, CallerUserID: moverID},
	} {
		require.NoError(t, dm.RefreshDMVoiceCallLease(
			context.Background(), ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
		))
	}
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES
			($1, $3, $5, $5), ($1, $4, $5, $5),
			($2, $6, $5, $5)
	`, oldConversationID, newConversationID, moverID, oldPeerID, oldAt, newPeerID)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, oldAt)
	seedPrivateVoiceGeneration(t, ts, oldPeerID, peerSuccessorCallID, peerSuccessorAt)
	seedPrivateVoiceGeneration(t, ts, newPeerID, newCallID, oldAt)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": newConversationID.String(), "callId": newCallID.String(),
		"userId": mover.ID, "username": mover.Username,
		"timestamp": moveAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, oldConversationID.String(), oldPeer.ID,
	), "a stale old-scope row is healed instead of rebound to the old call")
	oldPeerKey, err := presence.VoiceLifecycleKey(oldPeerID, presence.CategoryPrivateCall)
	require.NoError(t, err)
	watermark, err := ts.Redis.HGetAll(context.Background(), oldPeerKey).Result()
	require.NoError(t, err)
	require.Equal(t, peerSuccessorCallID.String(), watermark["token"])
	require.Equal(t, fmt.Sprintf("%d", peerSuccessorAt.UnixMicro()), watermark["version"])
	require.Equal(t, "1", watermark["active"])
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), oldPeerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, peerSuccessorCallID, state.SourceToken,
		"old-scope healing must preserve the peer's newer legitimate activity")
}

func TestPrivateHeartbeatCrossScopeMoveAdvancesOldAndNewParticipantSets(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	mutationReplica := newTestSubscriber(ts)
	mover := ts.CreateTestUser(t, "rp_hb_scope_fence_mover")
	oldPeer := ts.CreateTestUser(t, "rp_hb_scope_fence_old_peer")
	newPeer := ts.CreateTestUser(t, "rp_hb_scope_fence_new_peer")
	oldConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, oldPeer.ID))
	newConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, newPeer.ID))
	oldCallID := uuid.New()
	newCallID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 55, 0, time.UTC)
	moveAt := oldAt.Add(time.Second)
	moverID := uuid.MustParse(mover.ID)
	oldPeerID := uuid.MustParse(oldPeer.ID)
	newPeerID := uuid.MustParse(newPeer.ID)

	for _, lease := range []dm.VoiceCallLease{
		{ConversationID: oldConversationID, CallID: oldCallID, CallerUserID: moverID},
		{ConversationID: newConversationID, CallID: newCallID, CallerUserID: moverID},
	} {
		require.NoError(t, dm.RefreshDMVoiceCallLease(
			context.Background(), ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
		))
	}
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES
			($1, $3, $5, $5), ($1, $4, $5, $5),
			($2, $6, $5, $5)
	`, oldConversationID, newConversationID, moverID, oldPeerID, oldAt, newPeerID)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, oldAt)
	seedPrivateVoiceGeneration(t, ts, oldPeerID, oldCallID, oldAt)
	seedPrivateVoiceGeneration(t, ts, newPeerID, newCallID, oldAt)

	mutationReplica.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": newConversationID.String(), "callId": newCallID.String(),
		"callerUserId": mover.ID, "userIds": []string{mover.ID, newPeer.ID},
		"timestamp": moveAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, oldConversationID.String(), mover.ID,
	))
	for _, participant := range []struct {
		conversationID uuid.UUID
		userID         uuid.UUID
	}{
		{oldConversationID, oldPeerID},
		{newConversationID, moverID},
		{newConversationID, newPeerID},
	} {
		var lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRow(`
			SELECT lifecycle_event_at
			FROM dm_voice_participants
			WHERE conversation_id = $1 AND user_id = $2
		`, participant.conversationID, participant.userID).Scan(&lifecycleAt))
		require.Equal(t, moveAt.UnixMicro(), lifecycleAt.UnixMicro())
	}
	assertStalePrivateActivityRejectedAfterCurrentDelete(
		t, ts, oldPeerID, oldCallID, oldAt,
	)
}

func TestPrivateCallMissingLifecycleRejectsCrossScopeJoinReplay(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replayEqual bool
	}{
		{name: "older"},
		{name: "equal", replayEqual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			mover := ts.CreateTestUser(t, "rp_join_ttl_replay_mover_"+tc.name)
			oldPeer := ts.CreateTestUser(t, "rp_join_ttl_replay_old_"+tc.name)
			newPeer := ts.CreateTestUser(t, "rp_join_ttl_replay_new_"+tc.name)
			oldConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, oldPeer.ID))
			newConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, newPeer.ID))
			oldCallID := uuid.New()
			newCallID := uuid.New()
			initialAt := time.Date(2026, 7, 16, 2, 9, 56, 0, time.UTC)
			moveAt := initialAt.Add(2 * time.Second)
			replayAt := initialAt.Add(time.Second)
			if tc.replayEqual {
				replayAt = moveAt
			}
			moverID := uuid.MustParse(mover.ID)

			for _, lease := range []dm.VoiceCallLease{
				{ConversationID: oldConversationID, CallID: oldCallID, CallerUserID: moverID},
				{ConversationID: newConversationID, CallID: newCallID, CallerUserID: moverID},
			} {
				require.NoError(t, dm.RefreshDMVoiceCallLease(
					context.Background(), ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
				))
			}
			_, err := ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, oldConversationID, moverID, initialAt)
			require.NoError(t, err)
			seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, initialAt)

			sub.HandleJoined(mustJSON(t, map[string]interface{}{
				"channelId": newConversationID.String(), "callId": newCallID.String(),
				"userId": mover.ID, "username": mover.Username,
				"timestamp": moveAt.Format(time.RFC3339Nano),
			}))
			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			))
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			))

			lifecycleKey, err := presence.VoiceLifecycleKey(
				moverID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.NoError(t, ts.Redis.Del(context.Background(), lifecycleKey).Err())

			sub.HandleJoined(mustJSON(t, map[string]interface{}{
				"channelId": oldConversationID.String(), "callId": oldCallID.String(),
				"userId": mover.ID, "username": mover.Username,
				"timestamp": replayAt.Format(time.RFC3339Nano),
			}))

			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			), "an expired Redis watermark must not permit a reverse join")
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			))
			var lifecycleAt time.Time
			require.NoError(t, ts.DB.QueryRow(`
				SELECT lifecycle_event_at
				FROM dm_voice_participants
				WHERE conversation_id = $1 AND user_id = $2
			`, newConversationID, moverID).Scan(&lifecycleAt))
			require.Equal(t, moveAt.UnixMicro(), lifecycleAt.UnixMicro())
			require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val(),
				"the durable preflight must reject before recreating a stale watermark")
		})
	}
}

func TestPrivateCallMissingLifecycleRejectsCrossScopeHeartbeatReplay(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replayEqual bool
	}{
		{name: "older"},
		{name: "equal", replayEqual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			mover := ts.CreateTestUser(t, "rp_hb_ttl_replay_mover_"+tc.name)
			oldPeer := ts.CreateTestUser(t, "rp_hb_ttl_replay_old_"+tc.name)
			newPeer := ts.CreateTestUser(t, "rp_hb_ttl_replay_new_"+tc.name)
			oldConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, oldPeer.ID))
			newConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, newPeer.ID))
			oldCallID := uuid.New()
			newCallID := uuid.New()
			initialAt := time.Date(2026, 7, 16, 2, 9, 59, 0, time.UTC)
			moveAt := initialAt.Add(2 * time.Second)
			replayAt := initialAt.Add(time.Second)
			if tc.replayEqual {
				replayAt = moveAt
			}
			moverID := uuid.MustParse(mover.ID)

			for _, lease := range []dm.VoiceCallLease{
				{ConversationID: oldConversationID, CallID: oldCallID, CallerUserID: moverID},
				{ConversationID: newConversationID, CallID: newCallID, CallerUserID: moverID},
			} {
				require.NoError(t, dm.RefreshDMVoiceCallLease(
					context.Background(), ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
				))
			}
			_, err := ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, oldConversationID, moverID, initialAt)
			require.NoError(t, err)
			seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, initialAt)

			sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
				"channelId": newConversationID.String(), "callId": newCallID.String(),
				"callerUserId": mover.ID, "userIds": []string{mover.ID},
				"timestamp": moveAt.Format(time.RFC3339Nano),
			}))
			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			))
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			))

			lifecycleKey, err := presence.VoiceLifecycleKey(
				moverID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.NoError(t, ts.Redis.Del(context.Background(), lifecycleKey).Err())

			sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
				"channelId": oldConversationID.String(), "callId": oldCallID.String(),
				"callerUserId": mover.ID, "userIds": []string{mover.ID},
				"timestamp": replayAt.Format(time.RFC3339Nano),
			}))

			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			), "an expired Redis watermark must not permit a reverse heartbeat")
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			))
			var lifecycleAt time.Time
			require.NoError(t, ts.DB.QueryRow(`
				SELECT lifecycle_event_at
				FROM dm_voice_participants
				WHERE conversation_id = $1 AND user_id = $2
			`, newConversationID, moverID).Scan(&lifecycleAt))
			require.Equal(t, moveAt.UnixMicro(), lifecycleAt.UnixMicro())
			require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val(),
				"the durable preflight must reject before recreating a stale watermark")
		})
	}
}

func TestPrivateHeartbeatMissingLifecyclePreservesNewerOmittedParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_hb_ttl_omission_caller")
	peer := ts.CreateTestUser(t, "rp_hb_ttl_omission_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	callerAt := time.Date(2026, 7, 16, 2, 10, 2, 0, time.UTC)
	heartbeatAt := callerAt.Add(time.Second)
	peerAt := heartbeatAt.Add(time.Second)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $5, $5)
	`, conversationID, callerID, peerID, callerAt, peerAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, callerID, callID, callerAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, peerAt)
	peerLifecycleKey, err := presence.VoiceLifecycleKey(
		peerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Del(context.Background(), peerLifecycleKey).Err())

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{caller.ID},
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	}))

	for _, participant := range []struct {
		userID uuid.UUID
		wantAt time.Time
	}{
		{userID: callerID, wantAt: callerAt},
		{userID: peerID, wantAt: peerAt},
	} {
		var lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRow(`
			SELECT lifecycle_event_at
			FROM dm_voice_participants
			WHERE conversation_id = $1 AND user_id = $2
		`, conversationID, participant.userID).Scan(&lifecycleAt))
		require.Equal(t, participant.wantAt.UnixMicro(), lifecycleAt.UnixMicro(),
			"a newer omitted row must reject the entire heartbeat")
	}
	require.Zero(t, ts.Redis.Exists(context.Background(), peerLifecycleKey).Val())
	callerLifecycleKey, err := presence.VoiceLifecycleKey(
		callerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d", callerAt.UnixMicro()), ts.Redis.HGet(
		context.Background(), callerLifecycleKey, "version",
	).Val(), "the rejected heartbeat must not advance an otherwise acceptable peer")
}

func TestPrivateJoinExactDuplicatePreservesRefreshedSenderState(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_join_duplicate_state_caller")
	peer := ts.CreateTestUser(t, "rp_join_duplicate_state_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, caller.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 16, 2, 10, 5, 0, time.UTC)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err = ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, conversationID, peerID, eventAt.Add(-time.Second))
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, eventAt.Add(-time.Second))
	payload := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": eventAt.Format(time.RFC3339Nano),
	})
	store := presence.NewActivityStore(ts.Redis)

	sub.HandleJoined(payload)
	state, found, err := store.Get(
		context.Background(), callerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, callID, state.SourceToken)
	require.Equal(t, eventAt.UnixMicro(), state.SourceVersion)

	sub.HandleJoined(payload)
	state, found, err = store.Get(
		context.Background(), callerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found,
		"an exact duplicate may reconnect local clients but must retain its refreshed state")
	require.Equal(t, callID, state.SourceToken)
	require.Equal(t, eventAt.UnixMicro(), state.SourceVersion)
}

func TestPrivateJoinStaleTargetRowPreservesPeerSuccessorState(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	joiner := ts.CreateTestUser(t, "rp_join_successor_state_joiner")
	stalePeer := ts.CreateTestUser(t, "rp_join_successor_state_peer")
	successorPeer := ts.CreateTestUser(t, "rp_join_successor_state_other")
	targetConversationID := uuid.MustParse(ts.CreateDMConversation(t, joiner.ID, stalePeer.ID))
	successorConversationID := uuid.MustParse(ts.CreateDMConversation(
		t, stalePeer.ID, successorPeer.ID,
	))
	targetCallID := uuid.New()
	successorCallID := uuid.New()
	staleAt := time.Date(2026, 7, 16, 2, 10, 7, 0, time.UTC)
	successorAt := staleAt.Add(time.Second)
	joinAt := successorAt.Add(time.Second)
	stalePeerID := uuid.MustParse(stalePeer.ID)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: successorConversationID,
			CallID:         successorCallID,
			CallerUserID:   stalePeerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $3, $4, $4), ($2, $3, $5, $5)
	`, targetConversationID, successorConversationID, stalePeerID, staleAt, successorAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, stalePeerID, successorCallID, successorAt)
	successorState := presence.ActivityState{
		SourceToken: successorCallID, SourceVersion: successorAt.UnixMicro(),
		Payload:   json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		UpdatedAt: successorAt.Unix(),
	}
	store := presence.NewActivityStore(ts.Redis)
	stored, err := store.CompareAndSetActive(
		context.Background(), stalePeerID, presence.CategoryPrivateCall, successorState,
	)
	require.NoError(t, err)
	require.True(t, stored)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": targetConversationID.String(), "callId": targetCallID.String(),
		"userId": joiner.ID, "username": joiner.Username,
		"timestamp": joinAt.Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(
		t, ts.DB, targetConversationID.String(), joiner.ID,
	))
	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, targetConversationID.String(), stalePeer.ID,
	), "the target's mismatched stale row must be healed")
	require.True(t, dmVoiceParticipantExists(
		t, ts.DB, successorConversationID.String(), stalePeer.ID,
	), "healing one target must not remove the peer's successor scope")
	state, found, err := store.Get(
		context.Background(), stalePeerID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found,
		"the joining sender has no authority to delete a peer's exact successor generation")
	require.Equal(t, successorCallID, state.SourceToken)
	require.Equal(t, successorAt.UnixMicro(), state.SourceVersion)
}

func TestPrivateHeartbeatExactDuplicateReconnectsReplicaWithLostRemovalEvidence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winningReplica := newTestSubscriber(ts)
	replicaHub, replicaURL := newVoiceReplicaHub(t, ts)
	replayReplica := newTestSubscriberWithHub(ts, replicaHub)
	caller := ts.CreateTestUser(t, "rp_hb_duplicate_caller")
	peer := ts.CreateTestUser(t, "rp_hb_duplicate_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 57, 0, time.UTC)
	heartbeatAt := oldAt.Add(time.Second)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, callerID, peerID, oldAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, callerID, callID, oldAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, oldAt)
	connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, peer)

	payload := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{caller.ID},
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	})
	winningReplica.HandleHeartbeat(payload)
	require.Equal(t, 1, replicaHub.GetUserClientCount(peerID),
		"the winning replica cannot close another replica's local client")

	replayReplica.HandleHeartbeat(payload)
	require.Eventually(t, func() bool {
		return replicaHub.GetUserClientCount(peerID) == 0
	}, time.Second, 10*time.Millisecond,
		"an exact replay must conservatively reconnect clients when removal evidence is gone")
}

func TestPrivateHeartbeatRemovalRejectsStaleRemainingSenderBuild(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_hb_stale_build_caller")
	peer := ts.CreateTestUser(t, "rp_hb_stale_build_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 9, 59, 0, time.UTC)
	heartbeatAt := oldAt.Add(time.Second)
	callerID := uuid.MustParse(caller.ID)
	peerID := uuid.MustParse(peer.ID)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: conversationID, CallID: callID, CallerUserID: callerID,
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $4, $4), ($1, $3, $4, $4)
	`, conversationID, callerID, peerID, oldAt)
	require.NoError(t, err)
	seedPrivateVoiceGeneration(t, ts, callerID, callID, oldAt)
	seedPrivateVoiceGeneration(t, ts, peerID, callID, oldAt)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{caller.ID},
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), peer.ID,
	))
	assertStalePrivateActivityRejectedAfterCurrentDelete(
		t, ts, callerID, callID, oldAt,
	)
}

func assertStalePrivateActivityRejectedAfterCurrentDelete(
	t *testing.T,
	ts *testhelpers.TestServer,
	participantID, callID uuid.UUID,
	staleAt time.Time,
) {
	t.Helper()
	ctx := context.Background()
	store := presence.NewActivityStore(ts.Redis)
	current, found, err := store.Get(
		ctx, participantID, presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	if found {
		deleted, deleteErr := store.CompareAndDelete(
			ctx,
			participantID,
			presence.CategoryPrivateCall,
			current.SourceToken,
			current.SourceVersion,
		)
		require.NoError(t, deleteErr)
		require.True(t, deleted)
	}

	stored, err := store.CompareAndSetActive(
		ctx,
		participantID,
		presence.CategoryPrivateCall,
		presence.ActivityState{
			SourceToken: callID, SourceVersion: staleAt.UnixMicro(),
			Payload:   json.RawMessage(`{"call_type":"group","participant_count":2}`),
			UpdatedAt: staleAt.Unix(),
		},
	)
	require.NoError(t, err)
	require.False(t, stored,
		"a stale build from another replica must fail the active-generation CAS")
}

func TestPrivateHeartbeatMovesMemberToOneCallAndRejectsDelayedOldScope(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_scope_move_sender")
	oldPeer := ts.CreateTestUser(t, "rp_scope_move_old_peer")
	newPeer := ts.CreateTestUser(t, "rp_scope_move_new_peer")
	oldConversation := uuid.MustParse(ts.CreateDMConversation(t, sender.ID, oldPeer.ID))
	newConversation := uuid.MustParse(ts.CreateDMConversation(t, sender.ID, newPeer.ID))
	oldCallID := uuid.New()
	newCallID := uuid.New()
	oldAt := time.Date(2026, 7, 16, 2, 10, 0, 0, time.UTC)
	newAt := oldAt.Add(time.Second)

	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID: oldConversation, CallID: oldCallID,
			CallerUserID: uuid.MustParse(sender.ID),
		}, dm.DMVoiceCallLeaseTTL, true,
	))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, oldConversation, sender.ID, oldAt)
	require.NoError(t, err)
	claimed, err := sub.ClaimVoiceLifecycleForTest(
		context.Background(), presence.CategoryPrivateCall,
		uuid.MustParse(sender.ID), oldCallID, oldAt, true,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	connectVoiceWireClient(t, ts, newPeer)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": newConversation.String(), "callId": newCallID.String(),
		"callerUserId": sender.ID, "userIds": []string{sender.ID},
		"timestamp": newAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(t, ts.DB, oldConversation.String(), sender.ID))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, newConversation.String(), sender.ID))
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(newPeer.ID)) == 0
	}, time.Second, 10*time.Millisecond,
		"removing an unknown old private-call audience must force a conservative reconnect")

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": oldConversation.String(), "callId": oldCallID.String(),
		"callerUserId": sender.ID, "userIds": []string{sender.ID},
		"timestamp": oldAt.Add(time.Microsecond).Format(time.RFC3339Nano),
	}))
	require.False(t, dmVoiceParticipantExists(t, ts.DB, oldConversation.String(), sender.ID))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, newConversation.String(), sender.ID),
		"a delayed old-call heartbeat must not resurrect its scope")
}

func TestPrivateCrossScopeMoveWithoutOldLeasePreservesUnknownOldPeer(t *testing.T) {
	for _, eventType := range []string{"join", "heartbeat"} {
		t.Run(eventType, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			mover := ts.CreateTestUser(t, "rp_missing_old_lease_mover_"+eventType)
			oldPeer := ts.CreateTestUser(t, "rp_missing_old_lease_old_peer_"+eventType)
			newPeer := ts.CreateTestUser(t, "rp_missing_old_lease_new_peer_"+eventType)
			oldConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, oldPeer.ID))
			newConversationID := uuid.MustParse(ts.CreateDMConversation(t, mover.ID, newPeer.ID))
			oldCallID := uuid.New()
			newCallID := uuid.New()
			oldAt := time.Date(2026, 7, 16, 2, 12, 0, 0, time.UTC)
			moveAt := oldAt.Add(time.Second)
			moverID := uuid.MustParse(mover.ID)
			oldPeerID := uuid.MustParse(oldPeer.ID)
			newPeerID := uuid.MustParse(newPeer.ID)

			require.NoError(t, dm.RefreshDMVoiceCallLease(
				context.Background(), ts.Redis, dm.VoiceCallLease{
					ConversationID: newConversationID, CallID: newCallID,
					CallerUserID: moverID,
				}, dm.DMVoiceCallLeaseTTL, true,
			))
			_, found, err := dm.LookupDMVoiceCallLease(
				context.Background(), ts.Redis, oldConversationID,
			)
			require.NoError(t, err)
			require.False(t, found)
			_, err = ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES
					($1, $3, $5, $5), ($1, $4, $5, $5),
					($2, $6, $5, $5)
			`, oldConversationID, newConversationID, moverID, oldPeerID, oldAt, newPeerID)
			require.NoError(t, err)
			seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, oldAt)
			seedPrivateVoiceGeneration(t, ts, oldPeerID, oldCallID, oldAt)
			seedPrivateVoiceGeneration(t, ts, newPeerID, newCallID, oldAt)

			oldPeerLifecycleKey, err := presence.VoiceLifecycleKey(
				oldPeerID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			oldPeerLifecycle, err := ts.Redis.HGetAll(
				context.Background(), oldPeerLifecycleKey,
			).Result()
			require.NoError(t, err)
			store := presence.NewActivityStore(ts.Redis)
			oldPeerState, found, err := store.Get(
				context.Background(), oldPeerID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.True(t, found)
			connectVoiceWireClient(t, ts, oldPeer)

			switch eventType {
			case "join":
				sub.HandleJoined(mustJSON(t, map[string]interface{}{
					"channelId": newConversationID.String(), "callId": newCallID.String(),
					"userId": mover.ID, "username": mover.Username,
					"timestamp": moveAt.Format(time.RFC3339Nano),
				}))
			case "heartbeat":
				sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
					"channelId": newConversationID.String(), "callId": newCallID.String(),
					"callerUserId": mover.ID, "userIds": []string{mover.ID, newPeer.ID},
					"timestamp": moveAt.Format(time.RFC3339Nano),
				}))
			}

			require.False(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), mover.ID,
			), "the authoritative sender must leave its expired old scope")
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, newConversationID.String(), mover.ID,
			), "the authoritative sender must enter the target scope")
			require.True(t, dmVoiceParticipantExists(
				t, ts.DB, oldConversationID.String(), oldPeer.ID,
			), "a missing old lease leaves the unknown old peer untouched")
			var oldPeerLifecycleAt time.Time
			require.NoError(t, ts.DB.QueryRow(`
				SELECT lifecycle_event_at
				FROM dm_voice_participants
				WHERE conversation_id = $1 AND user_id = $2
			`, oldConversationID, oldPeerID).Scan(&oldPeerLifecycleAt))
			require.Equal(t, oldAt.UnixMicro(), oldPeerLifecycleAt.UnixMicro())
			require.Equal(t, oldPeerLifecycle, ts.Redis.HGetAll(
				context.Background(), oldPeerLifecycleKey,
			).Val(), "the unknown old peer lifecycle must not be rebound")
			currentOldPeerState, found, err := store.Get(
				context.Background(), oldPeerID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, oldPeerState, currentOldPeerState,
				"the unknown old peer ActivityState must remain untouched")
			require.Eventually(t, func() bool {
				return ts.Hub.GetUserClientCount(oldPeerID) == 0
			}, time.Second, 10*time.Millisecond,
				"an unverifiable old audience requires conservative reconnect")
		})
	}
}

func TestUpsertServerVoiceParticipantBoundsUnknownLegacyAudience(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_legacy_overflow_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Legacy Overflow")
	targetID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-legacy-target"))
	oldAt := time.Date(2026, 7, 16, 2, 15, 0, 0, time.UTC)
	eventAt := oldAt.Add(time.Second)
	_, err := ts.DB.Exec(`
		WITH legacy_channels AS (
			INSERT INTO channels (server_id, name, type)
			SELECT $1, 'rp-legacy-' || series, 'voice'
			FROM generate_series(1, 256) AS series
			RETURNING id
		)
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		SELECT id, $2, $3, $3 FROM legacy_channels
	`, serverID, sender.ID, oldAt)
	require.NoError(t, err)

	result, err := sub.UpsertServerVoiceParticipantForTest(
		context.Background(), targetID, uuid.MustParse(sender.ID), eventAt,
	)
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.True(t, result.Added)
	require.True(t, result.RemovedAudienceUnknown)
	require.Empty(t, result.RemovedRoomIDs,
		"an oversized old audience must not be materialized or replayed")
	var rowCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_participants WHERE user_id = $1
	`, sender.ID).Scan(&rowCount))
	require.Equal(t, 1, rowCount)
	require.True(t, voiceParticipantExists(t, ts.DB, targetID.String(), sender.ID))
	replayKey := fmt.Sprintf(
		"voice:result:server:%s:%d", sender.ID, eventAt.UnixMicro(),
	)
	require.Zero(t, ts.Redis.Exists(context.Background(), replayKey).Val(),
		"unknown audiences must skip exact replay publication")
}

func TestMalformedServerReplayJoinsPoisonDeletionFailure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "rp_replay_delete_sender").ID)
	serverID := ts.CreateTestServer(t, senderID.String(), "RP Replay Delete")
	targetID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-replay-delete-target"))
	eventAt := time.Date(2026, 7, 16, 2, 20, 0, 0, time.UTC)
	replayKey := fmt.Sprintf(
		"voice:result:server:%s:%d", senderID.String(), eventAt.UnixMicro(),
	)
	require.NoError(t, ts.Redis.Set(
		context.Background(), replayKey,
		`{"target_room_id":"bad","added":true,"removed_room_ids":[]}`,
		presence.ActivityStateTTL,
	).Err())

	redisClient := redis.NewClient(ts.Redis.Options())
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClient.AddHook(failRedisCommandHook{
		command: "del", err: errors.New("forced replay poison delete failure"),
	})
	sub := voice.NewNATSSubscriber(
		ts.DB, logger.New("test"), ts.Hub, nil, redisClient, nil, nil,
	)
	err := sub.LoadServerVoiceMutationReplayForTest(
		context.Background(), senderID, targetID, eventAt,
	)
	require.ErrorContains(t, err, "invalid server voice mutation replay target")
	require.ErrorContains(t, err, "forced replay poison delete failure")
}
