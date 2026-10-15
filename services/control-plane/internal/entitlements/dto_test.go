package entitlements_test

import (
	"encoding/json"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToDTO_FreeTier_MapsCalendarMonths(t *testing.T) {
	dto := entitlements.ToDTO(entitlements.For(entitlements.TierFree))
	assert.Equal(t, "free", dto.Tier)
	assert.Equal(t, []string{"minimum", "low", "moderate", "standard"}, dto.AllowedAudioTiers)
	assert.Equal(t, 20, dto.MinPtimeMs)
	assert.Equal(t, 8, dto.MaxAudioLastN)
	assert.Equal(t, 5120, dto.MaxMessageChars)
	assert.Equal(t, int64(33554432), dto.MaxAttachmentBytes)
	assert.Equal(t, 6, dto.UsernameChangeIntervalMonths)
	assert.Equal(t, int64(365*24*60*60), dto.UsernameChangeIntervalSeconds)
}

func TestToDTO_PremiumTier(t *testing.T) {
	dto := entitlements.ToDTO(entitlements.For(entitlements.TierPremium))
	assert.Equal(t, "premium", dto.Tier)
	assert.True(t, dto.AllowMusicMode)
	assert.Equal(t, 16, dto.MaxAudioLastN)
	assert.Equal(t, int64(268435456), dto.MaxAttachmentBytes)
	assert.Equal(t, 3, dto.UsernameChangeIntervalMonths)
	assert.Equal(t, int64(91*24*60*60), dto.UsernameChangeIntervalSeconds)
}

// Founder un-gating (2026-10-15): custom schemes, server creation and search
// depth are free for everyone. Released clients still require these keys, so
// they stay on the wire pinned to their free values on every tier.
func TestToDTO_UngatedKeysPinnedForEveryTier(t *testing.T) {
	for _, tier := range []string{entitlements.TierFree, entitlements.TierPremium} {
		dto := entitlements.ToDTO(entitlements.For(tier))
		assert.True(t, dto.AllowCustomScheme, tier)
		assert.Equal(t, entitlements.ServerLimitUnlimited, dto.MaxServersCreated, tier)
		assert.Equal(t, entitlements.ServerLimitUnlimited, dto.MessageHistorySearchDays, tier)
	}
}

func TestToDTO_JSONUsesCamelCaseKeys(t *testing.T) {
	b, err := json.Marshal(entitlements.ToDTO(entitlements.For(entitlements.TierFree)))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{
		"tier", "allowCustomScheme", "allowedAudioTiers", "minPtimeMs", "allowMusicMode",
		"maxAudioLastN", "streamMaxHeight", "streamMaxFps", "streamMaxPixelRate", "streamMaxBitrate",
		"cameraMaxHeight", "cameraMaxFps", "cameraMaxBitrate", "maxManualBitrateBps",
		"maxWebcamPublishers", "maxScreensharePublishers", "maxMessageChars", "maxAttachmentBytes",
		"maxAvatarBytes", "maxBannerBytes", "allowAnimatedProfile", "usernameChangeIntervalMonths",
		"usernameChangeIntervalSeconds",
		"maxServersCreated", "messageHistorySearchDays",
	} {
		_, ok := m[k]
		assert.Truef(t, ok, "missing wire key %q", k)
	}
	// 21 current keys plus four deprecated fields kept for older clients.
	assert.Len(t, m, 25, "DTO must serialize exactly 25 keys")
}

func TestDTOToMap_RoundTripsKeys(t *testing.T) {
	m, err := entitlements.DTOToMap(entitlements.ToDTO(entitlements.For(entitlements.TierPremium)))
	require.NoError(t, err)
	assert.Equal(t, "premium", m["tier"])
	assert.Len(t, m, 25)
}
