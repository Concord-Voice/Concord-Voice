package entitlements

// audioTierOrder is the canonical 7-tier audio ladder, ascending. Index = rank.
// MIRRORS the client AUDIO_QUALITY_TIERS (client/desktop/src/renderer/stores/
// voiceStore.ts), the media-plane AUDIO_QUALITY_TIERS config, and the
// media-plane AUDIO_TIER_OPUS_BITRATE_CEILING_BPS enforcement map. Keep all
// mirrors in lockstep.
var audioTierOrder = []string{"minimum", "low", "moderate", "standard", "high", "hifi", "studio"}

// audioTierPtimeMs is each tier's Opus frame size (ms). high/hifi/studio use
// 10ms; the rest ≥20ms. Mirrors preferredFrameSize in voiceStore.ts.
var audioTierPtimeMs = map[string]int{
	"minimum": 60, "low": 40, "moderate": 20, "standard": 20,
	"high": 10, "hifi": 10, "studio": 10,
}

// audioTierRank returns the ascending rank of a tier, or -1 if unknown.
func audioTierRank(tier string) int {
	for i, t := range audioTierOrder {
		if t == tier {
			return i
		}
	}
	return -1
}

// ServerAudioCeilingTier returns the highest audio tier an admin may set on a
// channel for a server of the given tier: Groundspeed → "standard" (≤96 kbps),
// any Mach → "studio" (≤510 kbps). Derived from ForServer so it tracks the
// fail-closed semantics (unknown server tier → Groundspeed → standard).
func ServerAudioCeilingTier(serverTier string) string {
	if ForServer(serverTier).UnlockServerAudioQualityCaps {
		return "studio"
	}
	return "standard"
}

// AudioTierAllowedForServer reports whether a channel audio tier may be set on a
// server of the given tier (rank ≤ the server's audio ceiling). Unknown/empty
// tiers are NOT allowed (fail closed).
func AudioTierAllowedForServer(channelTier, serverTier string) bool {
	r := audioTierRank(channelTier)
	if r < 0 {
		return false
	}
	return r <= audioTierRank(ServerAudioCeilingTier(serverTier))
}

// MediaForChannel resolves a member's media entitlements for a specific voice
// channel. When the channel has an admin-set audio standard (channelTier != ""),
// that standard is granted to EVERY member — bounded by the server's audio
// ceiling — by widening the user's audio entitlement so the media-plane produce
// gate (enforceAudioTierGate) accepts a mic producer at that tier, including its
// 10ms ptime. This is the per-channel "uplift" (matrix §2.3 / #179).
//
// When the channel has no standard (Personal, channelTier == ""), the user's own
// tier governs unchanged — audio is NOT server-uplifted in Personal mode.
//
// A channelTier above the server ceiling is clamped to the ceiling (defensive;
// UpdateChannel rejects above-ceiling values, but a stale row must not
// over-grant). An unknown channelTier degrades to the user's own entitlement.
func MediaForChannel(userTier, serverTier, channelTier string) MediaEntitlements {
	base := MediaFor(userTier)
	if channelTier == "" {
		return base // Personal — user's own tier governs
	}
	effRank := audioTierRank(channelTier)
	if effRank < 0 {
		return base // unknown channel tier — fall back to personal
	}
	if cr := audioTierRank(ServerAudioCeilingTier(serverTier)); effRank > cr {
		effRank = cr // clamp to server ceiling
	}
	effective := audioTierOrder[effRank]
	base.ChannelAudioUplift = true

	// Grant every tier up to `effective`.
	base.AllowedAudioTiers = unionAudioTiers(base.AllowedAudioTiers, audioTierOrder[:effRank+1])

	// Lower the ptime floor if the granted tier uses a finer frame size, so a
	// free member forced to a 10ms tier is not rejected by the ptime gate.
	if pt, ok := audioTierPtimeMs[effective]; ok && pt < base.MinPtimeMs {
		base.MinPtimeMs = pt
	}
	// MaxManualBitrateBps (≥5 Mbps) already exceeds the 510 kbps audio ceiling,
	// so audio never bumps it — left unchanged.
	return base
}

// unionAudioTiers merges b into a, preserving a's order and appending any tier in
// b not already present. Returns a fresh slice the caller may mutate.
func unionAudioTiers(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	out := make([]string, 0, len(a)+len(b))
	for _, t := range a {
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range b {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
