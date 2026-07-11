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
// MaxWebcamPublishers, MaxScreensharePublishers, and MaxAudioLastN are ROOM-scoped.
// The room-scope resolution is asymmetric by room kind (ADR-0029): DM rooms take
// the max present-participant tier, while CHANNEL rooms resolve from the SERVER's
// Mach tier (via RoomCapTierForServer, not any personal/owner plan) — so these
// user-tier publisher values are the DM-scoped table; channels read the
// server-axis collapse. The consumer picks the subject; this struct is only the
// tier->values table.
type Entitlement struct {
	Tier string

	// Cosmetic (Class 4 — client-only).
	AllowCustomScheme bool

	// Audio (Class 3 — media-plane).
	AllowedAudioTiers []string
	MinPtimeMs        int
	AllowMusicMode    bool
	MaxAudioLastN     int // room-owner-scoped; matches media-plane resolveAudioLastN

	// Video (Class 3 — client-enforced + bitrate-backstopped). The screen-share
	// (stream) and webcam (camera) axes are split so each carries its own
	// resolution/fps/bitrate ceiling (see the #1602 matrix). A negative height/fps
	// is the native/uncapped sentinel (mirrors ServerLimitUnlimited): the client
	// treats it as "no ceiling". Screen-share may be lifted by the server floor
	// (#1522, max(personal, ServerVideoFloor)); webcam is personal-tier only.
	StreamMaxHeight  int // free 1080; premium -1 (native)
	StreamMaxFps     int // free 60 (absolute ceiling; StreamMaxPixelRate tiers it, #2163); premium -1
	StreamMaxBitrate int // free 5_000_000; premium 20_000_000
	CameraMaxHeight  int // free 720;  premium -1 (native)
	CameraMaxFps     int // free 60;   premium -1
	CameraMaxBitrate int // free 2_500_000; premium 6_000_000
	// StreamMaxPixelRate is the screen-share (stream) axis tiered fps ceiling: a
	// (width*height*fps) budget in px/s, client-enforced (#2163). Composed with
	// StreamMaxHeight (hard resolution ceiling) and StreamMaxFps (absolute fps
	// ceiling), it expresses "1080p30 max, 60fps reserved for 720p and below":
	// admit iff height<=StreamMaxHeight AND fps<=StreamMaxFps AND w*h*fps<=this.
	// Free 62_208_000 (=1080p30); premium -1 (native). The CAMERA axis has no
	// pixel-rate cap (free 720p60 is a single tier that fits trivially) — the
	// client sets its axis pixelRate to Infinity.
	StreamMaxPixelRate int
	// MaxManualBitrateBps is #1300's media_entitlements advisory cap the
	// media-plane parses at createTransport. It is set to the stream ceiling
	// (max stream/camera bitrate) so the SFU's single advisory cap never wrongly
	// clamps a legitimate 20 Mbps screen-share. Kept for wire stability (#1300).
	MaxManualBitrateBps      int
	MaxWebcamPublishers      int // DM-scoped only; channels resolve the cap from the server Mach tier (ADR-0029). media-plane resolveVideoPublisherCap
	MaxScreensharePublishers int // DM-scoped only; channels resolve from the server Mach tier (ADR-0029). media-plane resolveScreenProducerCap (#1542)

	// Messaging (Class 2 — client-enforced char count).
	MaxMessageChars int

	// Uploads (Class 1 — server-enforced on ciphertext bytes).
	MaxAttachmentBytes   int64
	MaxAvatarBytes       int64
	MaxBannerBytes       int64
	AllowAnimatedProfile bool

	// Account (Class 1 — server-enforced).
	UsernameChangeInterval time.Duration
	// MaxServersCreated caps servers currently OWNED (deleting one frees a
	// slot). Negative = unlimited (ServerLimitUnlimited). Enforced at
	// CreateServer (#1555).
	MaxServersCreated int
	// MessageHistorySearchDays bounds the search-BACKFILL window (bulk fetch
	// path only — GET /channels/:id/messages/bulk). History ACCESS is never
	// gated (privacy stance; E2EE search is client-side). Negative = unlimited.
	MessageHistorySearchDays int
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
		StreamMaxHeight:          1080,
		StreamMaxFps:             60,         // #2163: absolute ceiling; pixel-rate tiers it (720p60 ok, 1080p60 rejected)
		StreamMaxPixelRate:       62_208_000, // #2163: = 1920*1080*30 (1080p30)
		StreamMaxBitrate:         5_000_000,
		CameraMaxHeight:          720,
		CameraMaxFps:             60,
		CameraMaxBitrate:         2_500_000,
		MaxManualBitrateBps:      5_000_000, // = max(StreamMaxBitrate, CameraMaxBitrate)
		MaxWebcamPublishers:      8,
		MaxScreensharePublishers: 8, // raised 1→8 for Discord parity (Discord caps stream quality, not concurrency)
		MaxMessageChars:          5120,
		MaxAttachmentBytes:       33_554_432, // 32 MiB — mirrors the public Groundspeed per-file baseline
		MaxAvatarBytes:           5_242_880,  // 5 MiB
		MaxBannerBytes:           5_242_880,  // 5 MiB
		AllowAnimatedProfile:     false,
		UsernameChangeInterval:   365 * 24 * time.Hour,
		MaxServersCreated:        5,
		MessageHistorySearchDays: 90,
	}

	premiumEntitlement = Entitlement{
		Tier:                     TierPremium,
		AllowCustomScheme:        true,
		AllowedAudioTiers:        []string{"minimum", "low", "moderate", "standard", "high", "hifi", "studio"},
		MinPtimeMs:               10,
		AllowMusicMode:           true,
		MaxAudioLastN:            16,
		StreamMaxHeight:          ServerLimitUnlimited, // native (uncapped)
		StreamMaxFps:             ServerLimitUnlimited,
		StreamMaxPixelRate:       ServerLimitUnlimited, // #2163: native (no pixel-rate cap)
		StreamMaxBitrate:         20_000_000,
		CameraMaxHeight:          ServerLimitUnlimited, // native (uncapped)
		CameraMaxFps:             ServerLimitUnlimited,
		CameraMaxBitrate:         6_000_000,
		MaxManualBitrateBps:      20_000_000, // = max(StreamMaxBitrate, CameraMaxBitrate)
		MaxWebcamPublishers:      25,
		MaxScreensharePublishers: 16, // 2× the free cap (raised 3→16 alongside the free 1→8 Discord-parity bump)
		MaxMessageChars:          10240,
		MaxAttachmentBytes:       268_435_456, // 256 MiB — Supersonic's pinned 256 MB (512 MB is Mach 3 server-wide)
		MaxAvatarBytes:           8_388_608,   // 8 MiB
		MaxBannerBytes:           8_388_608,   // 8 MiB
		AllowAnimatedProfile:     true,
		UsernameChangeInterval:   91 * 24 * time.Hour,
		MaxServersCreated:        ServerLimitUnlimited,
		MessageHistorySearchDays: 180,
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
