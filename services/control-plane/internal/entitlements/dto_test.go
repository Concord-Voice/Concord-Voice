package entitlements_test

import (
	"encoding/json"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToDTO_FreeTier_MapsAllFieldsAndDurationToSeconds(t *testing.T) {
	dto := entitlements.ToDTO(entitlements.For(entitlements.TierFree))
	assert.Equal(t, "free", dto.Tier)
	assert.False(t, dto.AllowCustomScheme)
	assert.Equal(t, []string{"minimum", "low", "moderate", "standard"}, dto.AllowedAudioTiers)
	assert.Equal(t, 20, dto.MinPtimeMs)
	assert.Equal(t, 8, dto.MaxAudioLastN)
	assert.Equal(t, 5120, dto.MaxMessageChars)
	assert.Equal(t, int64(26214400), dto.MaxAttachmentBytes)
	// 365 days in seconds
	assert.Equal(t, int64(31536000), dto.UsernameChangeIntervalSeconds)
}

func TestToDTO_PremiumTier(t *testing.T) {
	dto := entitlements.ToDTO(entitlements.For(entitlements.TierPremium))
	assert.Equal(t, "premium", dto.Tier)
	assert.True(t, dto.AllowMusicMode)
	assert.Equal(t, 16, dto.MaxAudioLastN)
	assert.Equal(t, int64(536870912), dto.MaxAttachmentBytes)
	assert.Equal(t, int64(91*24*3600), dto.UsernameChangeIntervalSeconds)
}

func TestToDTO_JSONUsesCamelCaseKeys(t *testing.T) {
	b, err := json.Marshal(entitlements.ToDTO(entitlements.For(entitlements.TierFree)))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{
		"tier", "allowCustomScheme", "allowedAudioTiers", "minPtimeMs", "allowMusicMode",
		"maxAudioLastN", "maxVideoHeight", "maxVideoFps", "maxVideoPixelRate", "maxManualBitrateBps",
		"maxWebcamPublishers", "maxScreensharePublishers", "maxMessageChars", "maxAttachmentBytes",
		"maxAvatarBytes", "maxBannerBytes", "allowAnimatedProfile", "usernameChangeIntervalSeconds",
	} {
		_, ok := m[k]
		assert.Truef(t, ok, "missing wire key %q", k)
	}
	assert.Len(t, m, 18, "DTO must serialize exactly 18 keys")
}

func TestDTOToMap_RoundTripsKeys(t *testing.T) {
	m, err := entitlements.DTOToMap(entitlements.ToDTO(entitlements.For(entitlements.TierPremium)))
	require.NoError(t, err)
	assert.Equal(t, "premium", m["tier"])
	assert.Len(t, m, 18)
}
