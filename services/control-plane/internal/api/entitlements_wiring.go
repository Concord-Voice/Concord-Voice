//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/billing"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// entitlementBroadcaster is the subset of *websocket.Hub the notifier needs.
// Declared as an interface (rather than depending on the concrete *Hub) so the
// adapter can be exercised with a recording fake in tests — the literal
// "entitlements_changed" wire string and the DTO->map shape are the live-update
// contract the client keys on. *websocket.Hub satisfies it (compile-time
// asserted below).
type entitlementBroadcaster interface {
	DisconnectUserContext(ctx context.Context, userID uuid.UUID) bool
	BroadcastToUserContext(ctx context.Context, userID uuid.UUID, msg websocket.OutgoingMessage) bool
}

var _ entitlementBroadcaster = (*websocket.Hub)(nil)

// The #1308 recorder takes the narrower EntitlementPusher, which has no
// DisconnectUser: the same adapter, structurally unable to disconnect.
var _ entitlements.EntitlementPusher = (*EntitlementNotifier)(nil)

// Compile-time proof the adapter is a valid entitlements.SessionNotifier, i.e.
// it can be injected into entitlements.OnTierChange by #1303/#1306.
var _ entitlements.SessionNotifier = (*EntitlementNotifier)(nil)

// The same adapter carries billing's seat events.
var _ billing.SeatNotifier = (*EntitlementNotifier)(nil)

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

// DisconnectUserContext bounds the downgrade enqueue by the notification budget.
func (n *EntitlementNotifier) DisconnectUserContext(ctx context.Context, userID uuid.UUID) bool {
	return n.hub.DisconnectUserContext(ctx, userID)
}

// NotifySeat pushes a Wing seat event (#2033 PR-5) to all of the user's
// connected clients. billing builds the id-only payload; this only enqueues it.
func (n *EntitlementNotifier) NotifySeat(ctx context.Context, userID uuid.UUID, event string, data map[string]any) bool {
	return n.hub.BroadcastToUserContext(ctx, userID, websocket.OutgoingMessage{Type: event, Data: data})
}

// BroadcastEntitlements pushes the new capability set to all of the user's
// connected clients as an entitlements_changed event.
func (n *EntitlementNotifier) BroadcastEntitlements(userID uuid.UUID, dto entitlements.EntitlementDTO) {
	if !n.BroadcastEntitlementsContext(context.Background(), userID, dto) {
		n.log.Error("entitlements: enqueue capability push failed")
	}
}

// BroadcastEntitlementsContext bounds the capability enqueue by the notification budget.
func (n *EntitlementNotifier) BroadcastEntitlementsContext(ctx context.Context, userID uuid.UUID, dto entitlements.EntitlementDTO) bool {
	data, err := entitlements.DTOToMap(dto)
	if err != nil {
		n.log.Error("entitlements: marshal DTO for push failed", "error", err)
		return false
	}
	return n.hub.BroadcastToUserContext(ctx, userID, websocket.OutgoingMessage{Type: "entitlements_changed", Data: data})
}

// newViolationRecorder builds #1308's misbehaviour recorder. Its threshold
// resync rides the same entitlements_changed push as a tier change, through
// this file's adapter, and reads the tier through the SAME cache the upload
// handlers enforce with. For an upload refusal that is the set that refused;
// for a media-plane report it is the current set, since the media plane
// enforces the set it read at join.
// One recorder serves both counted doors (the media upload handlers and the
// voice.tier_violation NATS door), so both share one window per user.
func newViolationRecorder(
	rdb *redis.Client,
	log *logger.Logger,
	tiers entitlements.CheckedTierResolver,
	hub entitlementBroadcaster,
	events securityevent.Emitter,
) *entitlements.ViolationRecorder {
	return entitlements.NewViolationRecorder(rdb, log, tiers, NewEntitlementNotifier(hub, log), events)
}
