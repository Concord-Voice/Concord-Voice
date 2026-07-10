package entitlements_test

import (
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
)

func TestFor_Free(t *testing.T) {
	e := entitlements.For(entitlements.TierFree)
	assert.Equal(t, "free", e.Tier)
	assert.False(t, e.AllowCustomScheme)
	assert.Equal(t, []string{"minimum", "low", "moderate", "standard"}, e.AllowedAudioTiers)
	assert.Equal(t, 20, e.MinPtimeMs)
	assert.False(t, e.AllowMusicMode)
	assert.Equal(t, 8, e.MaxAudioLastN)
	// Split video axes (#1602): screen-share (stream) 1080p30/≤5M, webcam (camera) 720p60/≤2.5M.
	assert.Equal(t, 1080, e.StreamMaxHeight)
	assert.Equal(t, 30, e.StreamMaxFps)
	assert.Equal(t, 5_000_000, e.StreamMaxBitrate)
	assert.Equal(t, 720, e.CameraMaxHeight)
	assert.Equal(t, 60, e.CameraMaxFps)
	assert.Equal(t, 2_500_000, e.CameraMaxBitrate)
	assert.Equal(t, 5_000_000, e.MaxManualBitrateBps) // = max(stream, camera) bitrate
	assert.Equal(t, 8, e.MaxWebcamPublishers)
	assert.Equal(t, 8, e.MaxScreensharePublishers)
	assert.Equal(t, 5120, e.MaxMessageChars)
	assert.Equal(t, int64(33_554_432), e.MaxAttachmentBytes)
	assert.Equal(t, int64(5_242_880), e.MaxAvatarBytes)
	assert.Equal(t, int64(5_242_880), e.MaxBannerBytes)
	assert.False(t, e.AllowAnimatedProfile)
	assert.Equal(t, 365*24*time.Hour, e.UsernameChangeInterval)
}

func TestFor_Premium(t *testing.T) {
	e := entitlements.For(entitlements.TierPremium)
	assert.Equal(t, "premium", e.Tier)
	assert.True(t, e.AllowCustomScheme)
	assert.Equal(t, []string{"minimum", "low", "moderate", "standard", "high", "hifi", "studio"}, e.AllowedAudioTiers)
	assert.Equal(t, 10, e.MinPtimeMs)
	assert.True(t, e.AllowMusicMode)
	assert.Equal(t, 16, e.MaxAudioLastN)
	// Split video axes (#1602): both native (uncapped) resolution/fps; stream ≤20M, camera ≤6M.
	assert.Equal(t, entitlements.ServerLimitUnlimited, e.StreamMaxHeight)
	assert.Equal(t, entitlements.ServerLimitUnlimited, e.StreamMaxFps)
	assert.Equal(t, 20_000_000, e.StreamMaxBitrate)
	assert.Equal(t, entitlements.ServerLimitUnlimited, e.CameraMaxHeight)
	assert.Equal(t, entitlements.ServerLimitUnlimited, e.CameraMaxFps)
	assert.Equal(t, 6_000_000, e.CameraMaxBitrate)
	assert.Equal(t, 20_000_000, e.MaxManualBitrateBps) // = max(stream, camera) bitrate
	assert.Equal(t, 25, e.MaxWebcamPublishers)
	assert.Equal(t, 16, e.MaxScreensharePublishers)
	assert.Equal(t, 10240, e.MaxMessageChars)
	assert.Equal(t, int64(268_435_456), e.MaxAttachmentBytes)
	assert.Equal(t, int64(8_388_608), e.MaxAvatarBytes)
	assert.Equal(t, int64(8_388_608), e.MaxBannerBytes)
	assert.True(t, e.AllowAnimatedProfile)
	assert.Equal(t, 91*24*time.Hour, e.UsernameChangeInterval)
}

func TestFor_UnknownTierFailsClosedToFree(t *testing.T) {
	free := entitlements.For(entitlements.TierFree)
	for _, tier := range []string{"", "garbage", "PREMIUM", "Free", "enterprise"} {
		t.Run("tier="+tier, func(t *testing.T) {
			assert.Equal(t, free, entitlements.For(tier))
		})
	}
}

// TestMediaFor_ManualBitrateIsStreamCeiling locks the #1300 media-plane contract:
// the single advisory MaxManualBitrateBps the SFU parses at createTransport equals
// the STREAM bitrate ceiling (max of the split axes), so a legitimate 20 Mbps
// screen-share is never wrongly clamped. Splitting the video entitlement (#1602)
// must keep this wire value byte-stable — free 5M / premium 20M.
func TestMediaFor_ManualBitrateIsStreamCeiling(t *testing.T) {
	assert.Equal(t, 5_000_000, entitlements.MediaFor(entitlements.TierFree).MaxManualBitrateBps)
	assert.Equal(t, 20_000_000, entitlements.MediaFor(entitlements.TierPremium).MaxManualBitrateBps)
}

func TestFor_ReturnsDefensiveSliceCopy(t *testing.T) {
	e := entitlements.For(entitlements.TierFree)
	e.AllowedAudioTiers[0] = "MUTATED"
	fresh := entitlements.For(entitlements.TierFree)
	assert.Equal(t, "minimum", fresh.AllowedAudioTiers[0],
		"the package source of truth must be immune to caller mutation")
}

// TestFor_Deterministic asserts repeated calls return equal values. Independence of
// the returned values (no shared mutable state across callers) is covered separately
// by TestFor_ReturnsDefensiveSliceCopy.
func TestFor_Deterministic(t *testing.T) {
	assert.Equal(t, entitlements.For(entitlements.TierPremium), entitlements.For(entitlements.TierPremium))
}

// Upload values are pricing-pinned (2026-07-03 directive): Sonic baseline mirrors
// the public Groundspeed 32 MB per-file limit; Supersonic sells 256 MB (512 MB
// belongs to Mach 3 server-wide, NOT the personal axis).
func TestUploadValues_PricingConformance(t *testing.T) {
	assert.Equal(t, int64(33_554_432), entitlements.For(entitlements.TierFree).MaxAttachmentBytes)
	assert.Equal(t, int64(268_435_456), entitlements.For(entitlements.TierPremium).MaxAttachmentBytes)
}

// #1555 gates: server-creation cap + search-depth. Pricing-pinned (Sonic "up to
// five servers" / Supersonic "no cap"); free search depth 90d is the founder
// decision of 2026-07-03, premium 180d is page-pinned.
func TestServerCapAndSearchDepthValues(t *testing.T) {
	free := entitlements.For(entitlements.TierFree)
	premium := entitlements.For(entitlements.TierPremium)
	assert.Equal(t, 5, free.MaxServersCreated)
	assert.Equal(t, entitlements.ServerLimitUnlimited, premium.MaxServersCreated, "Supersonic: no cap on server creation")
	assert.Equal(t, 90, free.MessageHistorySearchDays)
	assert.Equal(t, 180, premium.MessageHistorySearchDays)
}

func TestEffectiveAttachmentBytes(t *testing.T) {
	free := entitlements.For(entitlements.TierFree)
	premium := entitlements.For(entitlements.TierPremium)

	cases := []struct {
		name   string
		user   entitlements.Entitlement
		server entitlements.ServerEntitlement
		want   int64
	}{
		{"free in groundspeed = server baseline", free, entitlements.ForServer(entitlements.TierGroundspeed), 33_554_432},
		{"free in mach3 = server-wide grant", free, entitlements.ForServer(entitlements.TierMach3), 536_870_912},
		{"premium in groundspeed = personal cap travels", premium, entitlements.ForServer(entitlements.TierGroundspeed), 268_435_456},
		{"premium in mach3 = server grant wins", premium, entitlements.ForServer(entitlements.TierMach3), 536_870_912},
		{"selfhost unlimited", free, entitlements.ForServer(entitlements.TierSelfHost), entitlements.ServerLimitUnlimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, entitlements.EffectiveAttachmentBytes(tc.user, tc.server))
		})
	}
}
