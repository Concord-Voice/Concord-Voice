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
	assert.Equal(t, 1080, e.MaxVideoHeight)
	assert.Equal(t, 60, e.MaxVideoFps)
	assert.Equal(t, int64(62_208_000), e.MaxVideoPixelRate)
	assert.Equal(t, 5_000_000, e.MaxManualBitrateBps)
	assert.Equal(t, 8, e.MaxWebcamPublishers)
	assert.Equal(t, 1, e.MaxScreensharePublishers)
	assert.Equal(t, 5120, e.MaxMessageChars)
	assert.Equal(t, int64(26_214_400), e.MaxAttachmentBytes)
	assert.Equal(t, int64(1_048_576), e.MaxAvatarBytes)
	assert.Equal(t, int64(2_097_152), e.MaxBannerBytes)
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
	assert.Equal(t, 1080, e.MaxVideoHeight)
	assert.Equal(t, 60, e.MaxVideoFps)
	assert.Equal(t, int64(124_416_000), e.MaxVideoPixelRate)
	assert.Equal(t, 10_000_000, e.MaxManualBitrateBps)
	assert.Equal(t, 25, e.MaxWebcamPublishers)
	assert.Equal(t, 3, e.MaxScreensharePublishers)
	assert.Equal(t, 10240, e.MaxMessageChars)
	assert.Equal(t, int64(536_870_912), e.MaxAttachmentBytes)
	assert.Equal(t, int64(5_242_880), e.MaxAvatarBytes)
	assert.Equal(t, int64(5_242_880), e.MaxBannerBytes)
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
