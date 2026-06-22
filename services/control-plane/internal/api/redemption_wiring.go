//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// redemptionTierNotifier adapts entitlements.OnTierChange to the
// redemption.Notifier interface the redeem engine calls AFTER a premium grant
// commits. It carries the read-through entitlement Cache (for invalidation) and
// the WS SessionNotifier (for the entitlements_changed push + downgrade kick),
// exactly the two args OnTierChange needs. Declared here so the redemption
// package stays free of websocket/cache wiring (mirrors EntitlementNotifier).
type redemptionTierNotifier struct {
	cache    *entitlements.Cache
	notifier entitlements.SessionNotifier
}

// compile-time proof the adapter satisfies the engine's Notifier interface.
var _ redemption.Notifier = (*redemptionTierNotifier)(nil)

// OnTierChange invalidates the user's cached tier and pushes the live update
// (and force-disconnects on a downgrade — not expected for a code grant, but
// the convergence point handles it uniformly). A redeem only ever upgrades or
// extends premium, so this is the invalidate + push path in practice.
func (n *redemptionTierNotifier) OnTierChange(ctx context.Context, userID uuid.UUID, oldTier, newTier string) error {
	return entitlements.OnTierChange(ctx, n.cache, n.notifier, userID, oldTier, newTier)
}

// buildRedemptionHandler constructs the #1303 redemption HTTP handler: the
// engine (atomic /redeem + grant catalog + post-commit tier notify), the issuer
// (admin generation + DB audit sink), and the admin-token gate.
//
// The grant-effect catalog is the default set (premium:subscription, feature:*,
// cosmetic:*). The notifier wires the same entitlements convergence point used
// by #1297/#1306 so a premium grant live-updates the client. adminToken comes
// from REDEMPTION_ADMIN_TOKEN (config); empty disables the HTTP generation
// endpoint (the AdminGate returns 503) — see redemption.NewHandler.
func buildRedemptionHandler(db *sql.DB, entCache *entitlements.Cache, entNotifier entitlements.SessionNotifier, cfg *config.Config, log *logger.Logger) *redemption.Handler {
	catalog := redemption.NewCatalog()
	tierNotifier := &redemptionTierNotifier{cache: entCache, notifier: entNotifier}
	engine := redemption.NewEngine(db, catalog, tierNotifier)
	issuer := redemption.NewIssuer(db, catalog, redemption.NewDBAuditSink())
	return redemption.NewHandler(engine, issuer, cfg.RedemptionAdminToken, log)
}
