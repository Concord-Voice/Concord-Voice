package entitlements

// Server-axis entitlements. This is a SEPARATE axis from the user-scoped
// Entitlement: the user axis (TierFree/TierPremium) is resolved from the
// acting user's subscription; the server axis (Groundspeed/Mach 1–3) gates
// server-scoped features (custom emoji, stickers, soundboards, shared storage,
// server-wide quality caps/floors) and will be resolved from a server's
// subscription once #1556 ships. The two are kept structurally distinct so
// neither can leak privilege into the other.
//
// ForServer(tier) is pure (no DB, no I/O) — the tier->values table only. The
// server-subscription source of truth does not exist yet; ResolveServerTier
// (server_resolver.go) is the single seam where it will land.
//
// Values are the pricing ground truth (concordvoice-com teams.astro/compare.astro,
// finalized 2026-06-18) — see ADR-0028. Do not change a row without checking the
// live pricing pages; the client mirror (client/desktop/src/renderer/utils/
// serverEntitlements.ts) must change in lockstep.

// Server-tier identifiers (ADR-0028 ladder). Stored as VARCHAR for
// extensibility. The pre-ladder binary "mach" string is RETIRED — it was never
// persisted (no server-subscription table exists), and ForServer fails it
// closed to Groundspeed like any unknown tier.
const (
	TierGroundspeed = "groundspeed" // free default
	TierMach1       = "mach1"       // $4.99 boost
	TierMach2       = "mach2"       // $14.99 boost
	TierMach3       = "mach3"       // $34.99 boost
	TierSelfHost    = "selfhost"    // deployment mode, not purchasable (uncapped)
)

// ServerStoragePoolUnset is the sentinel for MaxServerStoragePoolBytes meaning
// "limit not yet decided". The shared-storage byte value is OPEN pending the
// A11 / #1523 cost discussion, so every SaaS row carries this sentinel for now.
// Downstream storage gates MUST treat negative as "no decision / do not
// enforce", NOT as "zero bytes".
const ServerStoragePoolUnset int64 = -1

// ServerLimitUnlimited is the sentinel for "explicitly no limit" on selfhost
// rows (marketing: uncapped, hardware-limited). Semantically distinct from
// ServerStoragePoolUnset ("no decision yet") even though both are negative:
// gate helpers treat ANY negative limit as non-enforcing.
const ServerLimitUnlimited = -1

// ServerEntitlement is the server-axis capability set for one server tier. Every
// downstream gate-check reads serverEnt.MaxX / serverEnt.UnlockX, so features
// (emoji, stickers, soundboards, storage) can be stubbed today and flipped on
// later with no gate rewrite — these values are the stable interface (the #1294
// modularity contract).
type ServerEntitlement struct {
	Tier string

	// Cosmetic count-gates (#1522 — server-scoped; negative = unlimited).
	MaxServerCustomEmoji int // includes animated at every tier (founder 2026-06-15)
	MaxServerStickers    int
	MaxServerSoundboards int // feature not built yet; values recorded per pricing
	MaxServerIconBytes   int64
	MaxServerBannerBytes int64

	// Per-file upload cap, server-wide (composes with the user axis via
	// EffectiveAttachmentBytes; handler wiring deferred to #1556 — see spec S3).
	MaxServerUploadBytes int64

	// Shared storage pool (#1523 — value OPEN, see ServerStoragePoolUnset).
	MaxServerStoragePoolBytes int64

	// Server-wide audio unlock (binary by design: any Mach level → studio; the
	// #179 channel-standard uplift derives from this).
	UnlockServerAudioQualityCaps bool

	// Server-wide video FLOOR (granted minimum quality for every member under
	// "video = better of personal cap or server floor"; consumed by #1542/#1602).
	// Zero = no floor.
	ServerVideoFloorHeight    int
	ServerVideoFloorFps       int
	ServerVideoFloorPixelRate int64 // width(16:9) * height * fps
}

// The tier rows are the ONE definition of the server-axis limits.
var (
	groundspeedServerEntitlement = ServerEntitlement{
		Tier:                 TierGroundspeed,
		MaxServerCustomEmoji: 75,
		MaxServerStickers:    10,
		// Groundspeed soundboard baseline 15 per the founder entitlement matrix
		// (docs/design/entitlements/entitlement-matrix.md §1/§2), the SoT the
		// pricing pages derive from — the public teams page omits the free line
		// item; omission is not a zero grant.
		MaxServerSoundboards:         15,
		MaxServerIconBytes:           5_242_880,
		MaxServerBannerBytes:         5_242_880,
		MaxServerUploadBytes:         33_554_432, // 32 MiB — the public Groundspeed per-file baseline
		MaxServerStoragePoolBytes:    ServerStoragePoolUnset,
		UnlockServerAudioQualityCaps: false,
	}

	mach1ServerEntitlement = ServerEntitlement{
		Tier:                         TierMach1,
		MaxServerCustomEmoji:         250,
		MaxServerStickers:            75,
		MaxServerSoundboards:         30,
		MaxServerIconBytes:           8_388_608,
		MaxServerBannerBytes:         8_388_608,
		MaxServerUploadBytes:         134_217_728,            // 128 MiB
		MaxServerStoragePoolBytes:    ServerStoragePoolUnset, // 250 GB pending #1523
		UnlockServerAudioQualityCaps: true,
		ServerVideoFloorHeight:       1080,
		ServerVideoFloorFps:          60,
		ServerVideoFloorPixelRate:    124_416_000, // 1920*1080*60
	}

	mach2ServerEntitlement = ServerEntitlement{
		Tier:                         TierMach2,
		MaxServerCustomEmoji:         350,
		MaxServerStickers:            100,
		MaxServerSoundboards:         40,
		MaxServerIconBytes:           8_388_608,
		MaxServerBannerBytes:         8_388_608,
		MaxServerUploadBytes:         268_435_456,            // 256 MiB
		MaxServerStoragePoolBytes:    ServerStoragePoolUnset, // 1 TB pending #1523
		UnlockServerAudioQualityCaps: true,
		ServerVideoFloorHeight:       1440,
		ServerVideoFloorFps:          60,
		ServerVideoFloorPixelRate:    221_184_000, // 2560*1440*60
	}

	mach3ServerEntitlement = ServerEntitlement{
		Tier:                         TierMach3,
		MaxServerCustomEmoji:         500,
		MaxServerStickers:            150,
		MaxServerSoundboards:         55,
		MaxServerIconBytes:           8_388_608,
		MaxServerBannerBytes:         8_388_608,
		MaxServerUploadBytes:         536_870_912,            // 512 MiB
		MaxServerStoragePoolBytes:    ServerStoragePoolUnset, // 2.5 TB pending #1523
		UnlockServerAudioQualityCaps: true,
		ServerVideoFloorHeight:       2160,
		ServerVideoFloorFps:          60,
		ServerVideoFloorPixelRate:    497_664_000, // 3840*2160*60
	}

	selfhostServerEntitlement = ServerEntitlement{
		Tier:                         TierSelfHost,
		MaxServerCustomEmoji:         ServerLimitUnlimited,
		MaxServerStickers:            ServerLimitUnlimited,
		MaxServerSoundboards:         ServerLimitUnlimited,
		MaxServerIconBytes:           8_388_608, // parity with Mach; not marketing-pinned
		MaxServerBannerBytes:         8_388_608,
		MaxServerUploadBytes:         ServerLimitUnlimited, // "no file-upload size caps"
		MaxServerStoragePoolBytes:    ServerLimitUnlimited, // "unlimited server file storage"
		UnlockServerAudioQualityCaps: true,
		// Floor = top ladder floor: floors are GRANTS (minimum quality), and
		// "uncapped" applies to caps — this is what #1542/#1602 read as the
		// server-granted minimum.
		ServerVideoFloorHeight:    2160,
		ServerVideoFloorFps:       60,
		ServerVideoFloorPixelRate: 497_664_000,
	}
)

// ForServer returns the capability set for the given server tier. Unknown, empty,
// mis-cased, or retired (pre-ladder "mach") tiers fail closed to the Groundspeed
// set (least privilege), mirroring For() on the user axis — a typo or stale claim
// can never grant a paid row. ServerEntitlement has no slice/pointer fields, so a
// plain copy is returned and no defensive deep-copy is needed.
func ForServer(serverTier string) ServerEntitlement {
	switch serverTier {
	case TierMach1:
		return mach1ServerEntitlement
	case TierMach2:
		return mach2ServerEntitlement
	case TierMach3:
		return mach3ServerEntitlement
	case TierSelfHost:
		return selfhostServerEntitlement
	default:
		return groundspeedServerEntitlement
	}
}
