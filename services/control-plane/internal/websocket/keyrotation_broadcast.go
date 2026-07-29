package websocket

import (
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/google/uuid"
)

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
		if len(rotation.DeletedChannelIDs) > 0 {
			for _, channelID := range rotation.DeletedChannelIDs {
				hub.BroadcastToServer(serverUUID, OutgoingMessage{
					Type: "channel_deleted",
					Data: map[string]interface{}{
						"channel_id": channelID,
						"server_id":  rotation.ServerID,
					},
				})
			}
			return
		}
		data := map[string]interface{}{
			"channel_id":    rotation.ChannelID,
			"server_id":     rotation.ServerID,
			"revoked_epoch": rotation.RevokedEpoch,
			"new_epoch":     rotation.SuccessorEpoch,
			"reason":        rotation.Reason,
		}
		if rotation.RemovedUserID != "" {
			data["removed_user_id"] = rotation.RemovedUserID
		}
		hub.BroadcastToServer(serverUUID, OutgoingMessage{Type: "key_revocation", Data: data})
	}
}
