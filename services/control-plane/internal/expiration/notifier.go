package expiration

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// PurgeBroadcaster is the hub surface an expiry purge announcement needs.
// *websocket.Hub satisfies it.
type PurgeBroadcaster interface {
	BroadcastToDMParticipantsContext(ctx context.Context, conversationID uuid.UUID, msg websocket.OutgoingMessage) bool
	BroadcastToServerContext(ctx context.Context, serverID uuid.UUID, msg websocket.OutgoingMessage) bool
}

// HubPurgeNotifier announces an expiry deletion over the hub: server_purged
// to a channel's server, dm_purged to a conversation's participants. It is
// the ONE implementation both the expiry sweeper and the pinned-expiry
// ContextExpirer use (#3459 §8), so a client cannot tell which of them
// deleted a row: two copies of these frames would drift in exactly the way
// no test of either caller would see.
type HubPurgeNotifier struct {
	hub PurgeBroadcaster
	log *logger.Logger
}

// NewHubPurgeNotifier validates the notifier's dependencies. A nil hub,
// including a typed-nil *websocket.Hub inside the interface, is refused here
// rather than at the first deletion, where it would panic after the rows were
// already gone.
func NewHubPurgeNotifier(hub PurgeBroadcaster, log *logger.Logger) (*HubPurgeNotifier, error) {
	if hub == nil {
		return nil, errors.New("expiry purge notifier requires a hub")
	}
	if concrete, ok := hub.(*websocket.Hub); ok && concrete == nil {
		return nil, errors.New("expiry purge notifier requires a hub")
	}
	if log == nil {
		return nil, errors.New("expiry purge notifier requires a logger")
	}
	return &HubPurgeNotifier{hub: hub, log: log}, nil
}

// EmitDMPurged announces an expiry deletion in a conversation. A suppressed
// delivery is logged by scope only, never by id.
func (n *HubPurgeNotifier) EmitDMPurged(ctx context.Context, conversationID string, cutoff time.Time) {
	conversation, parseErr := uuid.Parse(conversationID)
	if parseErr != nil {
		n.log.Warn("expiration DM purge notification suppressed", "scope", "dm")
		return
	}
	if delivered := n.hub.BroadcastToDMParticipantsContext(ctx, conversation, DMPurgedFrame(conversationID, cutoff)); !delivered && ctx.Err() == nil {
		n.log.Warn("expiration DM purge notification suppressed", "scope", "dm")
	}
}

// EmitServerPurged announces an expiry deletion in one of a server's
// channels. A suppressed delivery is logged by scope only, never by id.
func (n *HubPurgeNotifier) EmitServerPurged(ctx context.Context, serverID string, cutoff time.Time) {
	server, parseErr := uuid.Parse(serverID)
	if parseErr != nil {
		n.log.Warn("expiration server purge notification suppressed", "scope", "server")
		return
	}
	if delivered := n.hub.BroadcastToServerContext(ctx, server, ServerPurgedFrame(serverID, cutoff)); !delivered && ctx.Err() == nil {
		n.log.Warn("expiration server purge notification suppressed", "scope", "server")
	}
}

// DMPurgedFrame is the dm_purged frame for an expiry deletion. The cutoff is
// truncated to the microsecond PostgreSQL stores, so a client comparing it to
// a message's expires_at sees the value the engine compared against.
func DMPurgedFrame(conversationID string, cutoff time.Time) websocket.OutgoingMessage {
	return websocket.OutgoingMessage{
		Type: "dm_purged",
		Data: map[string]interface{}{
			"conversation_id": conversationID,
			"reason":          "expiry",
			"purged_by":       nil,
			"expires_before":  cutoff.UTC().Truncate(time.Microsecond),
		},
	}
}

// ServerPurgedFrame is the server_purged frame for an expiry deletion.
func ServerPurgedFrame(serverID string, cutoff time.Time) websocket.OutgoingMessage {
	return websocket.OutgoingMessage{
		Type: "server_purged",
		Data: map[string]interface{}{
			"server_id":      serverID,
			"reason":         "expiry",
			"purged_by":      nil,
			"expires_before": cutoff.UTC().Truncate(time.Microsecond),
		},
	}
}
