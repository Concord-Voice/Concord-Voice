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
	MaxScreensharePublishers int // room-owner-scoped; media-plane enforcement shipped with #1542 (resolveScreenProducerCap)

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
		MaxAttachmentBytes:       33_554_432, // 32 MiB — mirrors the public Groundspeed per-file baseline
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
		MaxAttachmentBytes:       268_435_456, // 256 MiB — Supersonic's pinned 256 MB (512 MB is Mach 3 server-wide)
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
// intentionally NOT carried here — #1542 shipped them via the separate
// room_owner_tier join-authorize field (channels) and Participant.tier (DMs);
// see ADR-0029.
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

// EffectiveAttachmentBytes composes the per-file upload limit for an upload
// into a server channel: the better of the user's personal allowance and the
// server-wide grant ("512 MB per-file uploads, server-wide" lifts every
// member). Negative server values mean unlimited (selfhost) and win outright.
// DM uploads stay user-axis only — do not call this without a server context.
//
// Handler adoption is DEFERRED to #1556 (see spec 2026-07-03-1522 §S3):
// UploadAttachment's DoS body cap is set before the multipart parse, but
// channel_id arrives in the multipart body, so the server tier is unknowable
// at cap time. Structural no-op until Mach tiers are purchasable.
//
// CALLER CONTRACT (#1556 wiring): a negative return is the unlimited sentinel,
// never a byte count — translate it (e.g. to a config ceiling) before handing
// it to http.MaxBytesReader or any size comparison.
func EffectiveAttachmentBytes(user Entitlement, server ServerEntitlement) int64 {
	if server.MaxServerUploadBytes < 0 {
		return ServerLimitUnlimited
	}
	if server.MaxServerUploadBytes > user.MaxAttachmentBytes {
		return server.MaxServerUploadBytes
	}
	return user.MaxAttachmentBytes
}
