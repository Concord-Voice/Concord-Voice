package voice_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type serverVoiceTerminalErasurePermissionChecker struct {
	checked   chan struct{}
	rechecked chan struct{}
	release   chan struct{}
	calls     *atomic.Int32
}

func (c serverVoiceTerminalErasurePermissionChecker) HasChannelPermission(
	context.Context, string, string, string, int64,
) (bool, error) {
	return true, nil
}

func (c serverVoiceTerminalErasurePermissionChecker) HasChannelPermissionsUncached(
	context.Context, string, string, string, ...int64,
) (bool, error) {
	if c.calls == nil {
		return true, nil
	}
	call := c.calls.Add(1)
	if call == 1 && c.checked != nil {
		close(c.checked)
		<-c.release
	}
	if call == 2 && c.rechecked != nil {
		close(c.rechecked)
	}
	return true, nil
}

func TestRemoteErasureClearDoesNotRetryOtherServerVoiceParticipants(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "remote-erasure-owner")
	participant := ts.CreateTestUser(t, "remote-erasure-participant")
	viewer := ts.CreateTestUser(t, "remote-erasure-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "remote-erasure-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "remote-erasure-channel")
	insertVoiceParticipant(t, ts.DB, channelID, participant.ID)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	checked := make(chan struct{})
	release := make(chan struct{})
	var checks atomic.Int32
	hub.SetChannelPermissionChecker(serverVoiceTerminalErasurePermissionChecker{
		checked: checked, release: release, calls: &checks,
	})
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server",
		"data": map[string]interface{}{"server_id": serverID},
	}))
	synchronizeVoiceWireClient(t, conn)

	require.True(t, hub.BroadcastToServerVoiceParticipantContext(
		context.Background(), uuid.MustParse(serverID), uuid.MustParse(channelID), uuid.MustParse(participant.ID),
		concordws.OutgoingMessage{Type: "voice_state_update", Data: map[string]interface{}{
			"channel_id": channelID, "user_id": participant.ID, "action": "joined", "server_id": serverID,
		}},
	))
	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated Server Voice frame did not enter authorization")
	}

	newTestSubscriberWithHub(ts, hub).HandlePresenceErasureClearedForTest(
		[]byte(`{"user_id":"` + uuid.NewString() + `"}`),
	)
	close(release)

	envelope := waitForVoiceWireType(t, conn, "voice_state_update")
	assert.Equal(t, participant.ID, envelope.Data["user_id"])
	require.Equal(t, int32(1), checks.Load(),
		"a forged clear for another UUID must not retry this participant's delivery")
}

func TestServerVoiceMoveFencesTheRemovedChannelTerminalDelivery(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "move-fence-owner")
	participant := ts.CreateTestUser(t, "move-fence-participant")
	viewer := ts.CreateTestUser(t, "move-fence-viewer")
	serverID := ts.CreateTestServer(t, owner.ID, "move-fence-server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	oldChannelID := ts.CreateVoiceChannel(t, serverID, "move-fence-old")
	newChannelID := ts.CreateVoiceChannel(t, serverID, "move-fence-new")
	insertVoiceParticipant(t, ts.DB, oldChannelID, participant.ID)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server",
		"data": map[string]interface{}{"server_id": serverID},
	}))
	synchronizeVoiceWireClient(t, conn)

	sub := newTestSubscriberWithHub(ts, hub)
	mutationStarted := make(chan struct{})
	release := make(chan struct{})
	moveDone := make(chan error, 1)
	go func() {
		_, err := sub.ApplyServerVoiceParticipantMutationForTest(
			uuid.MustParse(serverID), uuid.MustParse(newChannelID),
			presence.Scope{RoomID: uuid.MustParse(oldChannelID)}, uuid.MustParse(serverID), true,
			func() (bool, error) {
				close(mutationStarted)
				<-release
				_, mutationErr := ts.DB.Exec(`
					WITH removed AS (
						DELETE FROM voice_participants
						WHERE channel_id = $1 AND user_id = $3
					)
					INSERT INTO voice_participants
						(channel_id, user_id, joined_at, lifecycle_event_at)
					VALUES ($2, $3, NOW(), NOW())
				`, oldChannelID, newChannelID, participant.ID)
				return mutationErr == nil, mutationErr
			},
		)
		moveDone <- err
	}()
	select {
	case <-mutationStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("move did not reach its guarded mutation")
	}

	delivery, queued := hub.BroadcastToServerVoiceParticipantReliableContext(
		context.Background(), uuid.MustParse(serverID), uuid.MustParse(oldChannelID), uuid.MustParse(participant.ID),
		concordws.OutgoingMessage{Type: "voice_state_update", Data: map[string]interface{}{
			"channel_id": oldChannelID, "user_id": participant.ID, "action": "left", "server_id": serverID,
		}},
	)
	require.True(t, queued)
	select {
	case outcome := <-delivery.Outcome:
		t.Fatalf("removed-channel terminal delivery settled before move commit: %v", outcome)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-moveDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("move did not complete")
	}
	select {
	case outcome := <-delivery.Outcome:
		require.Equal(t, concordws.ServerVoiceTerminalDeliveryApplied, outcome)
	case <-time.After(2 * time.Second):
		t.Fatal("removed-channel terminal delivery did not settle after move commit")
	}
}

func TestServerVoiceCrossServerMoveFencesTheRemovedChannelTerminalDelivery(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "cross-server-move-fence-owner")
	participant := ts.CreateTestUser(t, "cross-server-move-fence-participant")
	viewer := ts.CreateTestUser(t, "cross-server-move-fence-viewer")
	oldServerID := ts.CreateTestServer(t, owner.ID, "cross-server-move-fence-old-server")
	newServerID := ts.CreateTestServer(t, owner.ID, "cross-server-move-fence-new-server")
	ts.AddMemberToServer(t, oldServerID, viewer.ID, "member")
	ts.AddMemberToServer(t, newServerID, viewer.ID, "member")
	oldChannelID := ts.CreateVoiceChannel(t, oldServerID, "cross-server-move-fence-old")
	newChannelID := ts.CreateVoiceChannel(t, newServerID, "cross-server-move-fence-new")
	hub, baseURL := newVoiceReplicaHub(t, ts)
	sub := newTestSubscriberWithHub(ts, hub)
	joinedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": oldChannelID, "userId": participant.ID, "username": participant.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	require.True(t, voiceParticipantExists(t, ts.DB, oldChannelID, participant.ID))
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server",
		"data": map[string]interface{}{"server_id": oldServerID},
	}))
	synchronizeVoiceWireClient(t, conn)

	mutationStarted := make(chan struct{})
	release := make(chan struct{})
	reachedOldFence := make(chan struct{}, 1)
	hub.SetServerVoiceDeliveryMutationWaitHookForTest(func(serverID, channelID uuid.UUID) {
		if serverID == uuid.MustParse(oldServerID) && channelID == uuid.MustParse(oldChannelID) {
			select {
			case reachedOldFence <- struct{}{}:
			default:
			}
		}
	})
	var claimedOnce sync.Once
	sub.SetVoiceLifecycleClaimedHookForTest(func(category presence.Category, senderID uuid.UUID, _ time.Time) {
		if category == presence.CategoryServerVoice && senderID == uuid.MustParse(participant.ID) {
			claimedOnce.Do(func() { close(mutationStarted) })
			<-release
		}
	})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	movePayload := mustJSON(t, map[string]interface{}{
		"channelId": newChannelID, "userId": participant.ID, "username": participant.Username,
		"timestamp": joinedAt.Add(time.Second).Format(time.RFC3339Nano),
	})
	moveDone := make(chan struct{})
	go func() {
		defer close(moveDone)
		sub.HandleJoined(movePayload)
	}()
	select {
	case <-mutationStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cross-server move did not reach its guarded mutation")
	}

	delivery, queued := hub.BroadcastToServerVoiceParticipantReliableContext(
		context.Background(), uuid.MustParse(oldServerID), uuid.MustParse(oldChannelID), uuid.MustParse(participant.ID),
		concordws.OutgoingMessage{Type: "voice_state_update", Data: map[string]interface{}{
			"channel_id": oldChannelID, "user_id": participant.ID, "action": "left", "server_id": oldServerID,
		}},
	)
	require.True(t, queued)
	select {
	case outcome := <-delivery.Outcome:
		t.Fatalf("cross-server removed-channel terminal delivery settled before move commit: %v", outcome)
	case <-reachedOldFence:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal delivery did not reach the old-channel mutation fence")
	}
	select {
	case outcome := <-delivery.Outcome:
		t.Fatalf("cross-server removed-channel terminal delivery settled at the mutation fence: %v", outcome)
	default:
	}

	close(release)
	released = true
	select {
	case <-moveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cross-server move did not complete")
	}
	require.False(t, voiceParticipantExists(t, ts.DB, oldChannelID, participant.ID))
	require.True(t, voiceParticipantExists(t, ts.DB, newChannelID, participant.ID))
	select {
	case outcome := <-delivery.Outcome:
		require.Equal(t, concordws.ServerVoiceTerminalDeliveryApplied, outcome)
	case <-time.After(2 * time.Second):
		t.Fatal("cross-server removed-channel terminal delivery did not settle after move commit")
	}
}

func TestServerVoiceTerminalOutbox_ErasureCannotDeliverAdmittedLeave(t *testing.T) {
	t.Run("honest leave is observable", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		owner := ts.CreateTestUser(t, "terminal-erasure-control-owner")
		user := ts.CreateTestUser(t, "terminal-erasure-control-user")
		viewer := ts.CreateTestUser(t, "terminal-erasure-control-viewer")
		serverID := ts.CreateTestServer(t, owner.ID, "terminal-erasure-control-server")
		ts.AddMemberToServer(t, serverID, viewer.ID, "member")
		channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erasure-control-channel")
		op := uuid.New()
		insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

		hub, baseURL := newVoiceReplicaHub(t, ts)
		hub.SetChannelPermissionChecker(serverVoiceTerminalErasurePermissionChecker{})
		conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
		require.NoError(t, conn.WriteJSON(map[string]interface{}{
			"type": "subscribe_server",
			"data": map[string]interface{}{"server_id": serverID},
		}))
		synchronizeVoiceWireClient(t, conn)

		sub := newTestSubscriberWithHub(ts, hub)
		require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(
			context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op,
		))
		envelope := waitForVoiceWireType(t, conn, "voice_state_update")
		assert.Equal(t, "left", envelope.Data["action"])
		assert.Equal(t, user.ID, envelope.Data["user_id"])
	})

	t.Run("unrelated invalidation rechecks and delivers", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		owner := ts.CreateTestUser(t, "terminal-retry-owner")
		user := ts.CreateTestUser(t, "terminal-retry-user")
		viewer := ts.CreateTestUser(t, "terminal-retry-viewer")
		serverID := ts.CreateTestServer(t, owner.ID, "terminal-retry-server")
		ts.AddMemberToServer(t, serverID, viewer.ID, "member")
		channelID := ts.CreateVoiceChannel(t, serverID, "terminal-retry-channel")
		op := uuid.New()
		insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

		hub, baseURL := newVoiceReplicaHub(t, ts)
		checked := make(chan struct{})
		release := make(chan struct{})
		var checks atomic.Int32
		hub.SetChannelPermissionChecker(serverVoiceTerminalErasurePermissionChecker{
			checked: checked,
			release: release,
			calls:   &checks,
		})
		conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
		require.NoError(t, conn.WriteJSON(map[string]interface{}{
			"type": "subscribe_server",
			"data": map[string]interface{}{"server_id": serverID},
		}))
		synchronizeVoiceWireClient(t, conn)

		sub := newTestSubscriberWithHub(ts, hub)
		require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(
			context.Background(), uuid.MustParse(channelID), uuid.MustParse(user.ID), op,
		))
		select {
		case <-checked:
		case <-time.After(2 * time.Second):
			t.Fatal("terminal leave did not enter authorization")
		}
		hub.InvalidatePresenceAudiences()
		close(release)

		envelope := waitForVoiceWireType(t, conn, "voice_state_update")
		assert.Equal(t, "left", envelope.Data["action"])
		assert.Equal(t, user.ID, envelope.Data["user_id"])
		require.Equal(t, int32(2), checks.Load(), "invalidation must recheck the durable leave")
	})

	t.Run("leave admitted before erasure is suppressed", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		owner := ts.CreateTestUser(t, "terminal-erasure-race-owner")
		erasedUser := ts.CreateTestUser(t, "terminal-erasure-race-user")
		viewer := ts.CreateTestUser(t, "terminal-erasure-race-viewer")
		serverID := ts.CreateTestServer(t, owner.ID, "terminal-erasure-race-server")
		ts.AddMemberToServer(t, serverID, viewer.ID, "member")
		channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erasure-race-channel")
		op := uuid.New()
		insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), uuid.MustParse(serverID), op)

		hub, baseURL := newVoiceReplicaHub(t, ts)
		checked := make(chan struct{})
		release := make(chan struct{})
		var checks atomic.Int32
		hub.SetChannelPermissionChecker(serverVoiceTerminalErasurePermissionChecker{
			checked: checked,
			release: release,
			calls:   &checks,
		})
		conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
		require.NoError(t, conn.WriteJSON(map[string]interface{}{
			"type": "subscribe_server",
			"data": map[string]interface{}{"server_id": serverID},
		}))
		synchronizeVoiceWireClient(t, conn)

		sub := newTestSubscriberWithHub(ts, hub)
		require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(
			context.Background(), uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), op,
		))
		select {
		case <-checked:
		case <-time.After(2 * time.Second):
			t.Fatal("server voice leave was not admitted to the authorization worker")
		}

		svc := users.NewAccountService(ts.DB, logger.New("test"))
		svc.SetAudienceFence(hub)
		require.NoError(t, svc.DeleteAccount(context.Background(), erasedUser.ID))
		close(release)

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		for {
			var envelope voiceWireEnvelope
			if err := conn.ReadJSON(&envelope); err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					require.Equal(t, int32(1), checks.Load(), "erasure must suppress the event before retrying viewer authorization")
					return
				}
				t.Fatalf("unexpected websocket read error: %v", err)
			}
			if envelope.Type == "voice_state_update" {
				if envelope.Data["action"] == "left" || envelope.Data["user_id"] == erasedUser.ID {
					t.Fatalf("an admitted terminal leave revealed an erased user: %#v", envelope.Data)
				}
			}
		}
	})

	t.Run("leave waits for an open erasure before checking its subject", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		suffix := uuid.NewString()[:8]
		owner := ts.CreateTestUser(t, "terminal-open-owner-"+suffix)
		erasedUser := ts.CreateTestUser(t, "terminal-open-user-"+suffix)
		viewer := ts.CreateTestUser(t, "terminal-open-viewer-"+suffix)
		serverID := ts.CreateTestServer(t, owner.ID, "terminal-open-server-"+suffix)
		ts.AddMemberToServer(t, serverID, viewer.ID, "member")
		channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erasure-open-channel")
		op := uuid.New()
		insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), uuid.MustParse(serverID), op)

		hub, baseURL := newVoiceReplicaHub(t, ts)
		checked := make(chan struct{})
		rechecked := make(chan struct{})
		release := make(chan struct{})
		var checks atomic.Int32
		hub.SetChannelPermissionChecker(serverVoiceTerminalErasurePermissionChecker{
			checked:   checked,
			rechecked: rechecked,
			release:   release,
			calls:     &checks,
		})
		conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
		require.NoError(t, conn.WriteJSON(map[string]interface{}{
			"type": "subscribe_server",
			"data": map[string]interface{}{"server_id": serverID},
		}))
		synchronizeVoiceWireClient(t, conn)

		sub := newTestSubscriberWithHub(ts, hub)
		require.NoError(t, sub.DrainServerVoiceTerminalOutboxCandidateForTest(
			context.Background(), uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), op,
		))
		select {
		case <-checked:
		case <-time.After(2 * time.Second):
			t.Fatal("server voice leave did not enter authorization")
		}
		closeRevocation := hub.BeginAudienceRevocation()
		tx, err := ts.DB.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		_, err = tx.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, erasedUser.ID)
		require.NoError(t, err)
		close(release)

		select {
		case <-rechecked:
			t.Fatal("terminal leave retried before the erasure transaction committed")
		case <-time.After(150 * time.Millisecond):
		}
		require.NoError(t, tx.Commit())
		closeRevocation()

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		for {
			var envelope voiceWireEnvelope
			if err := conn.ReadJSON(&envelope); err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					require.Equal(t, int32(1), checks.Load())
					return
				}
				t.Fatalf("unexpected websocket read error: %v", err)
			}
			if envelope.Type == "voice_state_update" {
				t.Fatalf("an open erasure leaked a terminal leave: %#v", envelope.Data)
			}
		}
	})

	t.Run("leave queued after erasure is suppressed before authorization", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		suffix := uuid.NewString()[:8]
		owner := ts.CreateTestUser(t, "terminal-after-owner-"+suffix)
		erasedUser := ts.CreateTestUser(t, "terminal-after-user-"+suffix)
		viewer := ts.CreateTestUser(t, "terminal-after-viewer-"+suffix)
		serverID := ts.CreateTestServer(t, owner.ID, "terminal-after-server-"+suffix)
		ts.AddMemberToServer(t, serverID, viewer.ID, "member")
		channelID := ts.CreateVoiceChannel(t, serverID, "terminal-after-channel")

		hub, baseURL := newVoiceReplicaHub(t, ts)
		var checks atomic.Int32
		hub.SetChannelPermissionChecker(serverVoiceTerminalErasurePermissionChecker{calls: &checks})
		conn := connectVoiceWireClientAtURL(t, ts.Redis, hub, baseURL, viewer)
		require.NoError(t, conn.WriteJSON(map[string]interface{}{
			"type": "subscribe_server",
			"data": map[string]interface{}{"server_id": serverID},
		}))
		synchronizeVoiceWireClient(t, conn)

		svc := users.NewAccountService(ts.DB, logger.New("test"))
		svc.SetAudienceFence(hub)
		require.NoError(t, svc.DeleteAccount(context.Background(), erasedUser.ID))
		_, accepted := hub.BroadcastToServerVoiceParticipantReliableContext(
			context.Background(), uuid.MustParse(serverID), uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID),
			concordws.OutgoingMessage{Type: "voice_state_update", Data: map[string]interface{}{
				"channel_id": channelID, "user_id": erasedUser.ID, "action": "left", "server_id": serverID,
			}},
		)
		require.True(t, accepted)

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		for {
			var envelope voiceWireEnvelope
			if err := conn.ReadJSON(&envelope); err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					require.Zero(t, checks.Load(), "erased terminal leave must not query viewer authorization")
					return
				}
				t.Fatalf("unexpected websocket read error: %v", err)
			}
			if envelope.Type == "voice_state_update" {
				t.Fatalf("a queued terminal leave revealed an erased user: %#v", envelope.Data)
			}
		}
	})
}

var _ concordws.ChannelPermissionChecker = serverVoiceTerminalErasurePermissionChecker{}
