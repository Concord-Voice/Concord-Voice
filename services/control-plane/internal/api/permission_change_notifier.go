//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// permissionsChangedTimeout bounds the permissions_changed broadcast (#3456).
// The MFA factor writes call the notifier on the request goroutine after their
// commit, so an unbounded send on a saturated hub queue would hang a request
// whose change has already committed. Same one second, for the same reason, as
// the servers package's server_permissions_changed broadcast.
const permissionsChangedTimeout = time.Second

// failureClassPermChangeBroadcast is shared, byte for byte, with the servers
// package's server_permissions_changed failure line.
const failureClassPermChangeBroadcast = "perm_change_broadcast"

// permissionChangeBroadcaster is the subset of Hub the notifier needs.
type permissionChangeBroadcaster interface {
	BroadcastToUserContext(context.Context, uuid.UUID, websocket.OutgoingMessage) bool
}

var _ permissionChangeBroadcaster = (*websocket.Hub)(nil)
var _ mfa.PermissionChangeNotifier = (*permissionChangeNotifier)(nil)

// permissionChangeNotifier adapts the WebSocket hub to mfa.PermissionChangeNotifier.
// It lives here rather than in mfa so that mfa never imports websocket.
type permissionChangeNotifier struct {
	hub permissionChangeBroadcaster
	log *logger.Logger
}

func newPermissionChangeNotifier(hub permissionChangeBroadcaster, log *logger.Logger) *permissionChangeNotifier {
	return &permissionChangeNotifier{hub: hub, log: log}
}

// NotifyPermissionsChanged sends permissions_changed to every connected client
// of userID, and to no one else. The frame is exactly
// {"type":"permissions_changed","data":{}}: it says only that the user's
// permissions may have changed, so it is identical for a gained factor and a
// lost one. A failure logs one line that carries no identifier and no
// direction (I7, observability principle 7).
func (n *permissionChangeNotifier) NotifyPermissionsChanged(ctx context.Context, userID string) {
	delivered := false
	if user, err := uuid.Parse(userID); err == nil {
		broadcastCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), permissionsChangedTimeout)
		delivered = n.hub.BroadcastToUserContext(broadcastCtx, user, permissionsChangedMessage())
		cancel()
	}
	if !delivered {
		n.log.Error("Failed to announce a permission change", "failure_class", failureClassPermChangeBroadcast)
	}
}

// permissionsChangedMessage is the permissions_changed frame. Data is a
// non-nil empty map, never nil: OutgoingMessage.Data has no omitempty, so a
// nil map would go out as "data":null, which PermissionsChangedSchema in
// client/desktop/src/renderer/types/ws-events.ts rejects (heartbeatAckFrame is
// the byte-pinned precedent). Keep the two in sync.
func permissionsChangedMessage() websocket.OutgoingMessage {
	return websocket.OutgoingMessage{
		Type: "permissions_changed",
		Data: map[string]interface{}{},
	}
}
