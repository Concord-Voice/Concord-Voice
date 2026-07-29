//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"database/sql"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/privacy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// buildPrivacyHandler constructs the privacy handler wired to the
// account-deletion service. Telemetry-related wiring was removed in #758 (sub-epic G);
// the handler now exposes only POST /api/v1/privacy/erase-account.
func buildPrivacyHandler(
	db *sql.DB,
	redisClient *redis.Client,
	log *logger.Logger,
	activityCleanup *users.Handler,
	hub *websocket.Hub,
) *privacy.Handler {
	account := users.NewAccountService(db, log)
	account.SetActivitySettingsCleanupHandler(activityCleanup)
	if hub != nil {
		account.SetChannelDeletedBroadcaster(func(serverID, channelID string) {
			serverUUID, err := uuid.Parse(serverID)
			if err != nil {
				if log != nil {
					log.Error("erase-account: invalid deleted channel server", "error", err)
				}
				return
			}
			hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_deleted",
				Data: map[string]interface{}{"channel_id": channelID, "server_id": serverID},
			})
		})
	}
	return privacy.NewHandler(account, redisClient, log)
}
