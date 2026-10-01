package websocket

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeRespawnFrame(t *testing.T, raw []byte) (string, map[string]interface{}) {
	t.Helper()
	var frame struct {
		Type string                 `json:"type"`
		Data map[string]interface{} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &frame))
	return frame.Type, frame.Data
}

func TestPublishDMRespawnFromRun_DeliversNullHiddenStateOnlyToRespawnedUsers(t *testing.T) {
	hub := newMinimalHub()
	respawnedUser, bystander := uuid.New(), uuid.New()
	respawnedClient := newTestClient(hub, respawnedUser)
	bystanderClient := newTestClient(hub, bystander)
	for _, c := range []*Client{respawnedClient, bystanderClient} {
		hub.clients[c.ID] = c
		hub.userClients[c.UserID] = map[uuid.UUID]bool{c.ID: true}
	}
	conversationID := uuid.New()

	hub.publishDMRespawnFromRun(conversationID, []uuid.UUID{respawnedUser})

	require.Len(t, respawnedClient.Send, 1)
	frameType, data := decodeRespawnFrame(t, <-respawnedClient.Send)
	assert.Equal(t, "dm_conversation_hidden", frameType)
	assert.Equal(t, conversationID.String(), data[keyConversationID])
	assert.Contains(t, data, "hidden_at")
	assert.Nil(t, data["hidden_at"], "a respawn is the visible (null) state")
	assert.Empty(t, bystanderClient.Send, "only respawned users learn about the respawn")
}

func TestPublishDMRespawn_QueuesOneUserBroadcastPerRespawnedUser(t *testing.T) {
	hub := newMinimalHub()
	first, second := uuid.New(), uuid.New()
	conversationID := uuid.New()

	hub.PublishDMRespawn(conversationID, []uuid.UUID{first, second})

	require.Len(t, hub.userBroadcast, 2)
	for _, want := range []uuid.UUID{first, second} {
		queued := <-hub.userBroadcast
		assert.Equal(t, want, queued.UserID)
		assert.Equal(t, "dm_conversation_hidden", queued.Data.Type)
	}
}

func TestPublishDMRespawn_NilHubAndEmptyListAreNoOps(t *testing.T) {
	var nilHub *Hub
	assert.NotPanics(t, func() { nilHub.PublishDMRespawn(uuid.New(), []uuid.UUID{uuid.New()}) })

	hub := newMinimalHub()
	hub.PublishDMRespawn(uuid.New(), nil)
	assert.Empty(t, hub.userBroadcast)
}

func TestPublishDMRespawnContext_ReportsDeliveryAndHonoursCancellation(t *testing.T) {
	var nilHub *Hub
	assert.True(t, nilHub.PublishDMRespawnContext(context.Background(), uuid.New(), []uuid.UUID{uuid.New()}),
		"a nil hub has nothing to drop")

	hub := newMinimalHub()
	assert.True(t, hub.PublishDMRespawnContext(context.Background(), uuid.New(), []uuid.UUID{uuid.New()}))
	require.Len(t, hub.userBroadcast, 1)
	<-hub.userBroadcast

	// A bounded caller whose budget is spent must not queue a late frame.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, hub.PublishDMRespawnContext(ctx, uuid.New(), []uuid.UUID{uuid.New()}))
	assert.Empty(t, hub.userBroadcast)
}
