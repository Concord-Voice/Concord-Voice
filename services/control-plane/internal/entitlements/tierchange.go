package entitlements

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SessionNotifier is the subset of the WebSocket hub the tier-change path needs:
// force-disconnect (downgrade revocation) and a soft entitlements_changed push.
// Declaring the interface here (rather than importing websocket) keeps the
// entitlements package free of a websocket import — websocket already depends on
// entitlements, so the reverse edge would be an import cycle. The concrete
// adapter lives in internal/api (see entitlements_wiring.go).
type SessionNotifier interface {
	DisconnectUser(userID uuid.UUID)
	BroadcastEntitlements(userID uuid.UUID, dto EntitlementDTO)
}

// Rank orders tiers for downgrade detection. Unknown tiers rank 0 (fail-closed:
// an unrecognized new tier is treated as least-privileged, so a change INTO it
// counts as a downgrade and forces re-auth).
func Rank(tier string) int {
	if tier == TierPremium {
		return 1
	}
	return 0
}

// IsDowngrade reports whether moving oldTier -> newTier loses privilege.
func IsDowngrade(oldTier, newTier string) bool {
	return Rank(newTier) < Rank(oldTier)
}

// OnTierChange invalidates the user's cached tier, pushes entitlements_changed
// with the NEW capability set on EVERY change (live client update), and on a
// DOWNGRADE additionally force-disconnects (forced re-auth -> fresh free claim).
// Invalidation runs regardless of direction so server enforcement is current
// within one cache miss. notifier may be nil (invalidate-only) for non-WS callers.
//
// This is the single convergence point future callers use (#1303 redemption,
// #1306 Stripe webhook). It has no live caller in #1297 beyond exposure + the
// hub adapter.
func OnTierChange(ctx context.Context, cache *Cache, notifier SessionNotifier, userID uuid.UUID, oldTier, newTier string) error {
	// Attempt cache invalidation but CAPTURE the error rather than returning early:
	// on a downgrade the eager disconnect (revocation) is the fail-closed priority
	// and MUST run even when Redis is unavailable. Returning early on an invalidate
	// error would skip the disconnect, leaving a downgraded user on a stale premium
	// session during a Redis hiccup (fail-open). The error is still surfaced below.
	invalidateErr := cache.Invalidate(ctx, userID.String())

	if notifier != nil {
		// Push the NEW capability set to all of the user's connected clients so the
		// UI live-updates without a re-fetch round-trip (works for upgrade, lateral,
		// and downgrade alike).
		notifier.BroadcastEntitlements(userID, ToDTO(For(newTier)))

		// notifier.DisconnectUser severs the user's CONTROL-PLANE WebSocket only,
		// forcing re-auth -> a fresh free claim for all subsequent API/WS/join
		// decisions. It does NOT tear down an in-progress media-plane SFU voice
		// session (a separate transport); eager media-session teardown on downgrade
		// (via the voice.enforce.disconnect NATS path) is media-plane tier
		// enforcement, deferred to #1300/#1542.
		if IsDowngrade(oldTier, newTier) {
			notifier.DisconnectUser(userID)
		}
	}

	if invalidateErr != nil {
		return fmt.Errorf("invalidate tier cache: %w", invalidateErr)
	}
	return nil
}
