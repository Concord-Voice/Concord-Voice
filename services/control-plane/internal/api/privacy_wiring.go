//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"database/sql"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/privacy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// buildPrivacyHandler constructs the privacy handler wired to the
// account-deletion service. Telemetry-related wiring was removed in #758 (sub-epic G);
// the handler now exposes only POST /api/v1/privacy/erase-account.
//
// It also returns the AccountService itself (#2447). The service is constructed
// here rather than in NewRouter, so returning it is what lets the boot guard
// interrogate its wiring directly instead of trusting that the two setter calls
// below still exist.
func buildPrivacyHandler(
	db *sql.DB,
	redisClient *redis.Client,
	log *logger.Logger,
	activityCleanup *users.Handler,
	hub *websocket.Hub,
	graphPresence presencecapture.GraphPresenceCapture,
	natsClient *natsclient.Client,
) (*privacy.Handler, *users.AccountService) {
	account := users.NewAccountService(db, log)
	account.SetActivitySettingsCleanupHandler(activityCleanup)
	// #2447: the erasure capture and the cross-replica clear publisher. Returned
	// alongside the handler so the router's boot guard can interrogate the
	// service itself rather than trusting that this line still exists.
	account.SetGraphPresenceCapture(graphPresence)
	account.SetNATS(natsClient)
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
	return privacy.NewHandler(account, redisClient, log), account
}
