package voice_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type invalidatingServerVoiceTerminalPermissionChecker struct {
	hub   *concordws.Hub
	calls *atomic.Int32
}

func (c invalidatingServerVoiceTerminalPermissionChecker) HasChannelPermission(
	context.Context, string, string, string, int64,
) (bool, error) {
	return true, nil
}

func (c invalidatingServerVoiceTerminalPermissionChecker) HasChannelPermissionsUncached(
	context.Context, string, string, string, ...int64,
) (bool, error) {
	c.calls.Add(1)
	c.hub.InvalidatePresenceAudiences()
	return true, nil
}

func TestServerVoiceTerminalOutbox_BoundedAuthorizationRetryReleasesClaim(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "terminal-claim-owner-"+suffix)
	user := ts.CreateTestUser(t, "terminal-claim-user-"+suffix)
	viewer := ts.CreateTestUser(t, "terminal-claim-viewer-"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "terminal-claim-server-"+suffix)
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "terminal-claim-channel-"+suffix)
	op := uuid.New()
	insertServerVoiceTerminalOutbox(t, ts.DB, uuid.MustParse(channelID), uuid.MustParse(user.ID), uuid.MustParse(serverID), op)

	hub, baseURL := newVoiceReplicaHub(t, ts)
	var calls atomic.Int32
	hub.SetChannelPermissionChecker(invalidatingServerVoiceTerminalPermissionChecker{hub: hub, calls: &calls})
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
	require.Eventually(t, func() bool {
		var claimID, claimUntil *string
		return ts.DB.QueryRow(`
			SELECT delivery_claim_id::text, delivery_claim_until::text
			FROM server_voice_terminal_outbox WHERE operation_id = $1
		`, op).Scan(&claimID, &claimUntil) == nil && claimID == nil && claimUntil == nil
	}, 3*time.Second, 20*time.Millisecond)
	require.Equal(t, int32(3), calls.Load(), "terminal authorization retry must be bounded")

	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE operation_id = $1
	`, op).Scan(&count))
	require.Equal(t, 1, count, "a retryable terminal result must remain durable")
}
