//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// entitlementBroadcaster is the subset of *websocket.Hub the notifier needs.
// Declared as an interface (rather than depending on the concrete *Hub) so the
// adapter can be exercised with a recording fake in tests — the literal
// "entitlements_changed" wire string and the DTO->map shape are the live-update
// contract the client keys on. *websocket.Hub satisfies it (compile-time
// asserted below).
type entitlementBroadcaster interface {
	DisconnectUser(userID uuid.UUID)
	BroadcastToUser(userID uuid.UUID, msg websocket.OutgoingMessage)
}

var _ entitlementBroadcaster = (*websocket.Hub)(nil)

// Compile-time proof the adapter is a valid entitlements.SessionNotifier, i.e.
// it can be injected into entitlements.OnTierChange by #1303/#1306.
var _ entitlements.SessionNotifier = (*EntitlementNotifier)(nil)

// EntitlementNotifier adapts *websocket.Hub to entitlements.SessionNotifier so
// entitlements.OnTierChange can push entitlements_changed + force-disconnect
// without the entitlements package importing websocket. Exported + constructed
// here so future live callers (#1303 redemption, #1306 Stripe webhook) inject it
// into OnTierChange; it has no live caller in #1297 (matches OnTierChange's own
// build-ahead pattern from PR #1681).
type EntitlementNotifier struct {
	hub entitlementBroadcaster
	log *logger.Logger
}

// NewEntitlementNotifier builds the adapter. The concrete *websocket.Hub passed
// by router wiring satisfies entitlementBroadcaster; tests inject a fake.
func NewEntitlementNotifier(hub entitlementBroadcaster, log *logger.Logger) *EntitlementNotifier {
	return &EntitlementNotifier{hub: hub, log: log}
}

// DisconnectUser severs the user's control-plane WebSocket (forced re-auth).
func (n *EntitlementNotifier) DisconnectUser(userID uuid.UUID) { n.hub.DisconnectUser(userID) }

// BroadcastEntitlements pushes the new capability set to all of the user's
// connected clients as an entitlements_changed event.
func (n *EntitlementNotifier) BroadcastEntitlements(userID uuid.UUID, dto entitlements.EntitlementDTO) {
	data, err := entitlements.DTOToMap(dto)
	if err != nil {
		n.log.Error("entitlements: marshal DTO for push failed", "error", err)
		return
	}
	n.hub.BroadcastToUser(userID, websocket.OutgoingMessage{Type: "entitlements_changed", Data: data})
}
