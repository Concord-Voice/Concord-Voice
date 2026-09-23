package voice_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type blockingServerVoiceTerminalPermissionChecker struct {
	checked chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

type blockingPostMutationRedisHook struct {
	db        *sql.DB
	channelID uuid.UUID
	userID    uuid.UUID
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (h *blockingPostMutationRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *blockingPostMutationRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		var present bool
		if err := h.db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM voice_participants
				WHERE channel_id = $1 AND user_id = $2
			)
		`, h.channelID, h.userID).Scan(&present); err == nil && present {
			h.once.Do(func() {
				close(h.entered)
				<-h.release
			})
		}
		return next(ctx, cmd)
	}
}

func (h *blockingPostMutationRedisHook) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

func (c *blockingServerVoiceTerminalPermissionChecker) HasChannelPermission(
	context.Context, string, string, string, int64,
) (bool, error) {
	return true, nil
}

func (c *blockingServerVoiceTerminalPermissionChecker) HasChannelPermissionsUncached(
	context.Context, string, string, string, ...int64,
) (bool, error) {
	c.calls.Add(1)
	c.once.Do(func() {
		close(c.checked)
		<-c.release
	})
	return true, nil
}

func TestServerVoiceTerminalOutbox_OrdinaryLeaveInvalidatesAdmission(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "terminal-ordinary-leave-owner-"+suffix)
	user := ts.CreateTestUser(t, "terminal-ordinary-leave-user-"+suffix)
	viewer := ts.CreateTestUser(t, "terminal-ordinary-leave-viewer-"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-ordinary-leave-server-"+suffix)
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-ordinary-leave-channel-"+suffix)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	checker := &blockingServerVoiceTerminalPermissionChecker{
		checked: make(chan struct{}),
		release: make(chan struct{}),
	}
	hub.SetChannelPermissionChecker(checker)
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
	case <-checker.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal leave did not enter authorization")
	}

	insertVoiceParticipant(t, ts.DB, channelID, user.ID)
	applied, err := sub.DeleteServerVoiceParticipantForTest(
		context.Background(), uuid.MustParse(serverID), uuid.MustParse(channelID),
		uuid.MustParse(user.ID), time.Now().UTC(),
	)
	require.NoError(t, err)
	require.True(t, applied)
	close(checker.release)

	envelope := waitForVoiceWireType(t, conn, "voice_state_update")
	require.Equal(t, "left", envelope.Data["action"])
	require.Equal(t, int32(2), checker.calls.Load(), "ordinary leaves must revalidate terminal admission")
	require.Eventually(t, func() bool {
		var count int
		return ts.DB.QueryRow(`
			SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1
		`, op).Scan(&count) == nil && count == 0
	}, 3*time.Second, 20*time.Millisecond)
}

func TestServerVoiceTerminalOutbox_SuccessorJoinDuringAdmissionSuppressesLeave(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "terminal-successor-race-owner-"+suffix)
	user := ts.CreateTestUser(t, "terminal-successor-race-user-"+suffix)
	viewer := ts.CreateTestUser(t, "terminal-successor-race-viewer-"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-successor-race-server-"+suffix)
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	ts.AddMemberToServer(t, serverID, user.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-successor-race-channel-"+suffix)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, 2, TRUE)
	`, user.ID)
	require.NoError(t, err)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	checker := &blockingServerVoiceTerminalPermissionChecker{
		checked: make(chan struct{}),
		release: make(chan struct{}),
	}
	hub.SetChannelPermissionChecker(checker)
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
	case <-checker.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal leave did not enter authorization")
	}

	// Model this replica observing the participant after another replica already
	// committed it. The local heartbeat is accepted but reports added=false.
	insertVoiceParticipant(t, ts.DB, channelID, user.ID)
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{user.ID},
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}))
	close(checker.release)

	require.Eventually(t, func() bool {
		var count int
		return ts.DB.QueryRow(`
			SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1
		`, op).Scan(&count) == nil && count == 0
	}, 3*time.Second, 20*time.Millisecond)
}

func TestServerVoiceTerminalOutbox_PostMutationSuccessorCannotAdmitLeave(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "terminal-postmutation-owner-"+suffix)
	user := ts.CreateTestUser(t, "terminal-postmutation-user-"+suffix)
	viewer := ts.CreateTestUser(t, "terminal-postmutation-viewer-"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-postmutation-server-"+suffix)
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	ts.AddMemberToServer(t, serverID, user.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-postmutation-channel-"+suffix)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, 2, TRUE)
	`, user.ID)
	require.NoError(t, err)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	checker := &blockingServerVoiceTerminalPermissionChecker{
		checked: make(chan struct{}),
		release: make(chan struct{}),
	}
	hub.SetChannelPermissionChecker(checker)
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
	case <-checker.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal leave did not enter authorization")
	}

	activityEntered := make(chan struct{})
	activityRelease := make(chan struct{})
	ts.Redis.AddHook(&blockingPostMutationRedisHook{
		db: ts.DB, channelID: uuid.MustParse(channelID), userID: uuid.MustParse(user.ID),
		entered: activityEntered, release: activityRelease,
	})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
			"channelId": channelID,
			"userIds":   []string{user.ID},
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		}))
	}()
	select {
	case <-activityEntered:
	case <-time.After(2 * time.Second):
		close(activityRelease)
		t.Fatal("successor heartbeat did not reach postmutation Rich Presence")
	}

	close(checker.release)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	for {
		var envelope voiceWireEnvelope
		err := conn.ReadJSON(&envelope)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			require.NoError(t, err)
		}
		if envelope.Type == "voice_state_update" && envelope.Data["action"] == "left" {
			t.Fatal("stale terminal leave crossed the postmutation successor window")
		}
	}
	close(activityRelease)
	select {
	case <-heartbeatDone:
	case <-time.After(2 * time.Second):
		t.Fatal("successor heartbeat did not complete")
	}
	require.Eventually(t, func() bool {
		var count int
		return ts.DB.QueryRow(`
			SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1
		`, op).Scan(&count) == nil && count == 0
	}, 3*time.Second, 20*time.Millisecond)
}

func TestServerVoiceTerminalOutbox_RemoteErasureClearInvalidatesAdmission(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "terminal-erasure-race-owner-"+suffix)
	erasedUser := ts.CreateTestUser(t, "terminal-erasure-race-user-"+suffix)
	viewer := ts.CreateTestUser(t, "terminal-erasure-race-viewer-"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-erasure-race-server-"+suffix)
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-erasure-race-channel-"+suffix)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), uuid.MustParse(serverID), op)

	hubA, baseURL := newVoiceReplicaHub(t, ts)
	checker := &blockingServerVoiceTerminalPermissionChecker{
		checked: make(chan struct{}),
		release: make(chan struct{}),
	}
	hubA.SetChannelPermissionChecker(checker)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, hubA, baseURL, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server",
		"data": map[string]interface{}{"server_id": serverID},
	}))
	synchronizeVoiceWireClient(t, conn)

	subA := newTestSubscriberWithHub(ts, hubA)
	require.NoError(t, subA.DrainServerVoiceTerminalOutboxCandidateForTest(
		context.Background(), uuid.MustParse(channelID), uuid.MustParse(erasedUser.ID), op,
	))
	select {
	case <-checker.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal leave did not enter authorization")
	}

	hubB := concordws.NewHub(ts.DB, ts.Redis)
	erasing := users.NewAccountService(ts.DB, logger.New("erasing-replica"))
	erasing.SetAudienceFence(hubB)
	require.NoError(t, erasing.DeleteAccount(context.Background(), erasedUser.ID))
	subA.HandlePresenceErasureClearedForTest([]byte(`{"user_id":"` + erasedUser.ID + `"}`))
	close(checker.release)

	require.Eventually(t, func() bool {
		var count int
		return ts.DB.QueryRow(`
			SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1
		`, op).Scan(&count) == nil && count == 0
	}, 3*time.Second, 20*time.Millisecond)
	assertNoServerVoiceLeftBeforeSentinel(t, hubA, conn, uuid.MustParse(serverID))
}

var _ concordws.ChannelPermissionChecker = (*blockingServerVoiceTerminalPermissionChecker)(nil)
