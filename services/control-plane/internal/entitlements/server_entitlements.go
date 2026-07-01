package entitlements

// Server-axis entitlements. This is a SEPARATE axis from the user-scoped
// Entitlement above: the user axis (TierFree/TierPremium) is resolved from the
// acting user's subscription; the server axis (Groundspeed/Mach) gates
// server-scoped features (custom emoji, stickers, shared storage, server-wide
// quality caps) and will be resolved from a server's subscription once those
// ship (v1.0 / #211). The two are kept structurally distinct so neither can
// leak privilege into the other.
//
// ForServer(tier) is pure (no DB, no I/O) — the tier->values table only. The
// server-subscription source of truth does not exist yet; ResolveServerTier
// (server_resolver.go) is the single seam where it will land.

// Server-tier identifiers. Stored as VARCHAR for extensibility (same shape as
// the user-tier strings); these are the values defined today.
const (
	TierGroundspeed = "groundspeed" // free default
	TierMach        = "mach"        // boosted
)

// ServerStoragePoolUnset is the sentinel for MaxServerStoragePoolBytes meaning
// "limit not yet decided". The shared-storage byte value is OPEN pending the
// A11 / #1523 cost discussion, so BOTH tiers carry this sentinel for now.
// Downstream storage gates MUST treat -1 as "no decision / do not enforce",
// NOT as "zero bytes". Flipping in the real value later is a one-line table
// change in groundspeedServerEntitlement / machServerEntitlement.
const ServerStoragePoolUnset int64 = -1

// ServerEntitlement is the server-axis capability set for one server tier. Every
// downstream gate-check reads serverEnt.MaxX / serverEnt.UnlockX, so features
// (emoji, stickers, storage) can be stubbed today and flipped on later with no
// gate rewrite — these values are the stable interface (the #1294 modularity
// contract).
type ServerEntitlement struct {
	Tier string

	// Cosmetic count-gates (A10 — server-scoped).
	MaxServerCustomEmoji int
	MaxServerStickers    int
	MaxServerIconBytes   int64
	MaxServerBannerBytes int64

	// Shared storage pool (A11 — value OPEN, see ServerStoragePoolUnset).
	MaxServerStoragePoolBytes int64

	// Server-wide media quality unlocks.
	UnlockServerAudioQualityCaps bool
	UnlockServerVideoQualityCaps bool
}

// groundspeedServerEntitlement and machServerEntitlement are the ONE definition
// of the server-axis limits.
var (
	groundspeedServerEntitlement = ServerEntitlement{
		Tier:                         TierGroundspeed,
		MaxServerCustomEmoji:         75,
		MaxServerStickers:            10,
		MaxServerIconBytes:           5_242_880,
		MaxServerBannerBytes:         5_242_880,
		MaxServerStoragePoolBytes:    ServerStoragePoolUnset,
		UnlockServerAudioQualityCaps: false,
		UnlockServerVideoQualityCaps: false,
	}

	machServerEntitlement = ServerEntitlement{
		Tier:                         TierMach,
		MaxServerCustomEmoji:         125,
		MaxServerStickers:            75,
		MaxServerIconBytes:           8_388_608,
		MaxServerBannerBytes:         8_388_608,
		MaxServerStoragePoolBytes:    ServerStoragePoolUnset, // A11-pending; same sentinel as free until #1523
		UnlockServerAudioQualityCaps: true,
		UnlockServerVideoQualityCaps: true,
	}
)

// ForServer returns the capability set for the given server tier. Unknown, empty,
// or mis-cased tiers fail closed to the Groundspeed set (least privilege),
// mirroring For() on the user axis — a typo or stale claim can never grant Mach.
// ServerEntitlement has no slice/pointer fields, so a plain copy is returned and
// no defensive deep-copy is needed.
func ForServer(serverTier string) ServerEntitlement {
	if serverTier == TierMach {
		return machServerEntitlement
	}
	return groundspeedServerEntitlement
}
