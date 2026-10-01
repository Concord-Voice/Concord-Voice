package websocket

import (
	"context"

	"github.com/google/uuid"
)

// dmRespawnMessage is the visible (null) visibility state for a thread that a
// new DM row respawned — the same frame Unhide publishes, so the renderer's
// existing dm_conversation_hidden handler refetches its conversation list.
func dmRespawnMessage(conversationID uuid.UUID) OutgoingMessage {
	return OutgoingMessage{Type: "dm_conversation_hidden", Data: map[string]interface{}{
		keyConversationID: conversationID.String(),
		"hidden_at":       nil,
	}}
}

// PublishDMRespawn tells each respawned participant, and only them, that the
// thread is visible again. Call it after the respawning transaction commits,
// from outside the hub's Run goroutine. A nil hub is a no-op.
func (h *Hub) PublishDMRespawn(conversationID uuid.UUID, userIDs []uuid.UUID) {
	h.PublishDMRespawnContext(context.Background(), conversationID, userIDs)
}

// PublishDMRespawnContext is PublishDMRespawn for a bounded callback: it gives
// up when ctx ends rather than outliving the caller's budget. It reports
// whether every frame was queued. A dropped frame delays the thread's return
// until the client's next conversation-list fetch.
func (h *Hub) PublishDMRespawnContext(ctx context.Context, conversationID uuid.UUID, userIDs []uuid.UUID) bool {
	if h == nil {
		return true
	}
	delivered := true
	for _, userID := range userIDs {
		if !h.BroadcastToUserContext(ctx, userID, dmRespawnMessage(conversationID)) {
			delivered = false
		}
	}
	return delivered
}

// publishDMRespawnFromRun is PublishDMRespawn for code already running on Run.
// Run is userBroadcast's only consumer, so sending into that channel from Run
// would block forever once the buffer filled.
func (h *Hub) publishDMRespawnFromRun(conversationID uuid.UUID, userIDs []uuid.UUID) {
	for _, userID := range userIDs {
		h.handleUserBroadcast(UserBroadcastMessage{UserID: userID, Data: dmRespawnMessage(conversationID)})
	}
}
