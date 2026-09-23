package websocket

import (
	"context"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/google/uuid"
)

var errKeyRevocationBroadcastUnavailable = errors.New("key revocation broadcast unavailable")

// KeyRevocationBroadcaster adapts a Hub to keyrotation's committed-event hook.
func KeyRevocationBroadcaster(hub *Hub) keyrotation.Broadcaster {
	return func(rotation keyrotation.Rotation) {
		if hub == nil {
			return
		}
		serverUUID, err := uuid.Parse(rotation.ServerID)
		if err != nil {
			return
		}
		for _, message := range keyRevocationMessages(rotation) {
			hub.BroadcastToServer(serverUUID, message)
		}
	}
}

// KeyRevocationContextBroadcaster adapts a Hub to bounded key-rotation delivery.
func KeyRevocationContextBroadcaster(hub *Hub) keyrotation.ContextBroadcaster {
	return func(ctx context.Context, rotation keyrotation.Rotation) error {
		if hub == nil {
			return errKeyRevocationBroadcastUnavailable
		}
		serverUUID, err := uuid.Parse(rotation.ServerID)
		if err != nil {
			return fmt.Errorf("parse key revocation server id: %w", err)
		}
		for _, message := range keyRevocationMessages(rotation) {
			if !hub.BroadcastToServerContext(ctx, serverUUID, message) {
				return errKeyRevocationBroadcastUnavailable
			}
		}
		return nil
	}
}

func keyRevocationMessages(rotation keyrotation.Rotation) []OutgoingMessage {
	if len(rotation.DeletedChannelIDs) > 0 {
		messages := make([]OutgoingMessage, 0, len(rotation.DeletedChannelIDs))
		for _, channelID := range rotation.DeletedChannelIDs {
			messages = append(messages, OutgoingMessage{
				Type: "channel_deleted",
				Data: map[string]interface{}{"channel_id": channelID, "server_id": rotation.ServerID},
			})
		}
		return messages
	}
	data := map[string]interface{}{
		"channel_id": rotation.ChannelID, "server_id": rotation.ServerID,
		"revoked_epoch": rotation.RevokedEpoch, "new_epoch": rotation.SuccessorEpoch,
		"reason": rotation.Reason,
	}
	if rotation.RemovedUserID != "" {
		data["removed_user_id"] = rotation.RemovedUserID
	}
	return []OutgoingMessage{{Type: "key_revocation", Data: data}}
}
