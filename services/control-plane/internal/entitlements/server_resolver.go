package entitlements

import (
	"context"
	"database/sql"
)

// ServerTierResolver resolves a server's current subscription tier. *ServerCache
// satisfies it (Redis read-through, fail-closed to Groundspeed). Handlers depend
// on this interface so enforcement logic can be tested with a stub, without a
// live Redis — mirroring TierResolver on the user axis.
type ServerTierResolver interface {
	GetServerTier(ctx context.Context, serverID string) string
}

// ResolveServerTier returns the server's current tier. This is the SINGLE,
// inert "Mach hook" seam: there is no server-subscription table yet (only the
// user-scoped subscriptions table exists), so EVERY server resolves to
// TierGroundspeed today.
//
// When server subscriptions ship (v1.0 / #211, group plans #1150), the real
// query lands HERE and nowhere else — the cache, the endpoint, and downstream
// gates already read through this function, so no caller changes. The query
// MUST fail closed to TierGroundspeed on sql.ErrNoRows OR any error (least
// privilege): a DB hiccup can never escalate a server to Mach. entitlements
// ForServer() is the fail-closed validation chokepoint for the returned string.
//
// The db parameter is accepted now (mirroring ResolveTier) so the signature is
// stable when the real query lands; it is intentionally unused today.
func ResolveServerTier(_ context.Context, _ *sql.DB, _ string) string {
	// Inert hook — see doc comment. No server-subscription source exists yet.
	return TierGroundspeed
}
