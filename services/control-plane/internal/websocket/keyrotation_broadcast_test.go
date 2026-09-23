package websocket

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyRevocationMessages(t *testing.T) {
	serverID := uuid.NewString()
	t.Run("deleted channels produce one event per channel", func(t *testing.T) {
		messages := keyRevocationMessages(keyrotation.Rotation{
			ServerID: serverID, DeletedChannelIDs: []string{"one", "two"},
		})
		require.Len(t, messages, 2)
		assert.Equal(t, "channel_deleted", messages[0].Type)
		assert.Equal(t, "one", messages[0].Data["channel_id"])
		assert.Equal(t, serverID, messages[0].Data["server_id"])
		assert.Equal(t, "two", messages[1].Data["channel_id"])
	})

	t.Run("rotation includes optional removed user", func(t *testing.T) {
		messages := keyRevocationMessages(keyrotation.Rotation{
			ChannelID: "channel", ServerID: serverID, RevokedEpoch: 4,
			SuccessorEpoch: 5, Reason: "member_removed", RemovedUserID: "user",
		})
		require.Len(t, messages, 1)
		assert.Equal(t, "key_revocation", messages[0].Type)
		assert.Equal(t, "channel", messages[0].Data["channel_id"])
		assert.Equal(t, 4, messages[0].Data["revoked_epoch"])
		assert.Equal(t, 5, messages[0].Data["new_epoch"])
		assert.Equal(t, "member_removed", messages[0].Data["reason"])
		assert.Equal(t, "user", messages[0].Data["removed_user_id"])
	})
}

func TestKeyRevocationBroadcasterQueuesOnlyValidRotations(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	KeyRevocationBroadcaster(hub)(keyrotation.Rotation{
		ServerID: serverID.String(), ChannelID: "channel", RevokedEpoch: 1,
		SuccessorEpoch: 2, Reason: "test",
	})
	select {
	case queued := <-hub.serverBroadcast:
		assert.Equal(t, serverID, queued.ServerID)
		assert.Equal(t, "key_revocation", queued.Data.Type)
	default:
		t.Fatal("expected valid rotation to be queued")
	}

	KeyRevocationBroadcaster(hub)(keyrotation.Rotation{ServerID: "not-a-uuid"})
	assert.Empty(t, hub.serverBroadcast)
	KeyRevocationBroadcaster(nil)(keyrotation.Rotation{ServerID: serverID.String()})
}

func TestKeyRevocationContextBroadcaster(t *testing.T) {
	serverID := uuid.New()
	hub := newMinimalHub()

	assert.ErrorIs(t, KeyRevocationContextBroadcaster(nil)(context.Background(), keyrotation.Rotation{}), errKeyRevocationBroadcastUnavailable)
	parseErr := KeyRevocationContextBroadcaster(hub)(context.Background(), keyrotation.Rotation{ServerID: "bad"})
	require.Error(t, parseErr)
	assert.NotErrorIs(t, parseErr, errKeyRevocationBroadcastUnavailable)
	assert.Contains(t, parseErr.Error(), "parse key revocation server id")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, KeyRevocationContextBroadcaster(hub)(ctx, keyrotation.Rotation{ServerID: serverID.String()}), errKeyRevocationBroadcastUnavailable)

	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	require.NoError(t, KeyRevocationContextBroadcaster(hub)(context.Background(), keyrotation.Rotation{
		ServerID: serverID.String(), ChannelID: "channel", RevokedEpoch: 3,
		SuccessorEpoch: 4, Reason: "test",
	}))
	queued := <-hub.serverBroadcast
	hub.handleServerBroadcast(queued)
	assert.Len(t, client.Send, 1)
}
