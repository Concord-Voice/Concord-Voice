// Package entitlements is the single source of truth for Concord Voice
// free/premium capability limits. Server enforcement (control-plane handlers,
// media-plane via the join-authorize tier seam) and client UX gating read these
// exact values, so the two can never drift apart.
//
// For(tier) is pure (no DB, no I/O). The subscriptions/redemption tables created
// in migrations 000068-000070 are consumed by downstream issues (#1296 cache+JWT,
// #1297 endpoint, #1303 redemption engine); this package only defines the values.
package entitlements

import "time"

// Tier identifiers. The schema stores tier as VARCHAR for extensibility; these
// are the values defined today.
const (
	TierFree    = "free"
	TierPremium = "premium"
)

// Entitlement is the capability set for one tier. Field subjects differ: most
// fields are user-scoped (resolved from the acting user's tier), but
// MaxWebcamPublishers, MaxScreensharePublishers, and MaxAudioLastN are ROOM-scoped
// — resolved from the room owner's tier (see media-plane.md resolveVideoPublisherCap).
// The consumer picks the subject; this struct is only the tier->values table.
type Entitlement struct {
	Tier string

	// Cosmetic (Class 4 — client-only).
	AllowCustomScheme bool

	// Audio (Class 3 — media-plane).
	AllowedAudioTiers []string
	MinPtimeMs        int
	AllowMusicMode    bool
	MaxAudioLastN     int // room-owner-scoped; matches media-plane resolveAudioLastN

	// Video (Class 3 — client-enforced + bitrate-backstopped).
	MaxVideoHeight           int
	MaxVideoFps              int
	MaxVideoPixelRate        int64 // width*height*fps ceiling; separates 1080p30/720p60 (free) from 1080p60 (premium)
	MaxManualBitrateBps      int
	MaxWebcamPublishers      int // room-owner-scoped; matches media-plane resolveVideoPublisherCap
	MaxScreensharePublishers int // room-owner-scoped; media-plane enforcement is #1542 (flat cap today)

	// Messaging (Class 2 — client-enforced char count).
	MaxMessageChars int

	// Uploads (Class 1 — server-enforced on ciphertext bytes).
	MaxAttachmentBytes   int64
	MaxAvatarBytes       int64
	MaxBannerBytes       int64
	AllowAnimatedProfile bool

	// Account (Class 1 — server-enforced).
	UsernameChangeInterval time.Duration
}

// freeEntitlement and premiumEntitlement are the ONE definition of the limits.
var (
	freeEntitlement = Entitlement{
		Tier:                     TierFree,
		AllowCustomScheme:        false,
		AllowedAudioTiers:        []string{"minimum", "low", "moderate", "standard"},
		MinPtimeMs:               20,
		AllowMusicMode:           false,
		MaxAudioLastN:            8,
		MaxVideoHeight:           1080,
		MaxVideoFps:              60,
		MaxVideoPixelRate:        62_208_000, // 1920*1080*30 (admits 720p60 = 55,296,000)
		MaxManualBitrateBps:      5_000_000,
		MaxWebcamPublishers:      8,
		MaxScreensharePublishers: 1,
		MaxMessageChars:          5120,
		MaxAttachmentBytes:       26_214_400, // 25 MiB — matches current UPLOAD_MAX_SIZE
		MaxAvatarBytes:           5_242_880,  // 5 MiB
		MaxBannerBytes:           5_242_880,  // 5 MiB
		AllowAnimatedProfile:     false,
		UsernameChangeInterval:   365 * 24 * time.Hour,
	}

	premiumEntitlement = Entitlement{
		Tier:                     TierPremium,
		AllowCustomScheme:        true,
		AllowedAudioTiers:        []string{"minimum", "low", "moderate", "standard", "high", "hifi", "studio"},
		MinPtimeMs:               10,
		AllowMusicMode:           true,
		MaxAudioLastN:            16,
		MaxVideoHeight:           1080,
		MaxVideoFps:              60,
		MaxVideoPixelRate:        124_416_000, // 1920*1080*60
		MaxManualBitrateBps:      10_000_000,
		MaxWebcamPublishers:      25,
		MaxScreensharePublishers: 3,
		MaxMessageChars:          10240,
		MaxAttachmentBytes:       536_870_912, // 512 MiB
		MaxAvatarBytes:           8_388_608,   // 8 MiB
		MaxBannerBytes:           8_388_608,   // 8 MiB
		AllowAnimatedProfile:     true,
		UsernameChangeInterval:   91 * 24 * time.Hour,
	}
)

// For returns the capability set for the given tier. Unknown or empty tiers fail
// closed to the free set (least privilege) — a typo or stale claim can never grant
// premium. The returned AllowedAudioTiers slice is a defensive copy so callers
// cannot mutate the shared source-of-truth tables.
func For(tier string) Entitlement {
	e := freeEntitlement
	if tier == TierPremium {
		e = premiumEntitlement
	}
	tiers := make([]string, len(e.AllowedAudioTiers))
	copy(tiers, e.AllowedAudioTiers)
	e.AllowedAudioTiers = tiers
	return e
}

// MediaEntitlements is the server-authoritative media-entitlement
// payload the join-authorize responses carry to the media-plane (#1300). The
// media-plane parses these caps to enforce send bitrate and the audio
// tier/ptime floor at the produce boundary; video resolution/fps stay
// client-enforced with the bitrate cap as the backstop (the SFU does NOT enforce
// pixel dimensions). Tier and max manual bitrate stay per-user; fixed channel
// audio standards may widen AllowedAudioTiers/MinPtimeMs only, marked by
// ChannelAudioUplift. The room-owner-scoped caps (MaxWebcamPublishers etc.) are
// #1542's seam and are intentionally NOT carried here.
type MediaEntitlements struct {
	Tier                string   `json:"tier"`
	AllowedAudioTiers   []string `json:"allowed_audio_tiers"`
	MinPtimeMs          int      `json:"min_ptime_ms"`
	MaxManualBitrateBps int      `json:"max_manual_bitrate_bps"`
	ChannelAudioUplift  bool     `json:"channel_audio_uplift,omitempty"`
}

// MediaFor resolves the media-entitlement payload for a tier string. It funnels
// through For, so an unknown/empty tier fails closed to the free floor (premium
// is never granted by accident). The returned AllowedAudioTiers slice is the
// defensive copy For already makes.
func MediaFor(tier string) MediaEntitlements {
	e := For(tier)
	return MediaEntitlements{
		Tier:                e.Tier,
		AllowedAudioTiers:   e.AllowedAudioTiers,
		MinPtimeMs:          e.MinPtimeMs,
		MaxManualBitrateBps: e.MaxManualBitrateBps,
	}
}
