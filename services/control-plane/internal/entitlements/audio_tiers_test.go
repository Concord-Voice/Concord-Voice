package entitlements_test

import (
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
)

func TestServerAudioCeilingTier(t *testing.T) {
	assert.Equal(t, "standard", entitlements.ServerAudioCeilingTier(entitlements.TierGroundspeed))
	assert.Equal(t, "studio", entitlements.ServerAudioCeilingTier(entitlements.TierMach))
	// Unknown server tier fails closed to Groundspeed → standard.
	assert.Equal(t, "standard", entitlements.ServerAudioCeilingTier("garbage"))
}

func TestAudioTierAllowedForServer(t *testing.T) {
	// Groundspeed: up to standard allowed; high/hifi/studio rejected.
	assert.True(t, entitlements.AudioTierAllowedForServer("standard", entitlements.TierGroundspeed))
	assert.False(t, entitlements.AudioTierAllowedForServer("high", entitlements.TierGroundspeed))
	assert.False(t, entitlements.AudioTierAllowedForServer("studio", entitlements.TierGroundspeed))
	// Mach: studio allowed.
	assert.True(t, entitlements.AudioTierAllowedForServer("studio", entitlements.TierMach))
	// Unknown tier fails closed.
	assert.False(t, entitlements.AudioTierAllowedForServer("garbage", entitlements.TierGroundspeed))
	assert.False(t, entitlements.AudioTierAllowedForServer("", entitlements.TierMach))
}

func TestMediaForChannel_Personal_EqualsMediaFor(t *testing.T) {
	// Empty channel tier (Personal) → identical to the user's own entitlement.
	for _, ut := range []string{entitlements.TierFree, entitlements.TierPremium} {
		assert.Equal(t, entitlements.MediaFor(ut), entitlements.MediaForChannel(ut, entitlements.TierGroundspeed, ""))
	}
}

func TestMediaForChannel_FreeUser_StudioChannel_OnMachServer_Uplifted(t *testing.T) {
	// A free user in a Studio channel on a Mach server is granted Studio:
	// allowed tiers include studio, ptime floor drops to 10ms (so the media
	// gate accepts a 10ms producer), bitrate cap unchanged (already >510k).
	got := entitlements.MediaForChannel(entitlements.TierFree, entitlements.TierMach, "studio")
	assert.Contains(t, got.AllowedAudioTiers, "studio")
	assert.Contains(t, got.AllowedAudioTiers, "high")
	assert.Equal(t, 10, got.MinPtimeMs)
	assert.Equal(t, entitlements.MediaFor(entitlements.TierFree).MaxManualBitrateBps, got.MaxManualBitrateBps)
	assert.Equal(t, entitlements.TierFree, got.Tier) // user-tier label preserved
	assert.True(t, got.ChannelAudioUplift)
}

func TestMediaForChannel_ClampsToServerCeiling(t *testing.T) {
	// Studio channel on a Groundspeed server clamps to Standard: studio NOT
	// granted, ptime floor stays 20ms.
	got := entitlements.MediaForChannel(entitlements.TierFree, entitlements.TierGroundspeed, "studio")
	assert.NotContains(t, got.AllowedAudioTiers, "studio")
	assert.NotContains(t, got.AllowedAudioTiers, "high")
	assert.Contains(t, got.AllowedAudioTiers, "standard")
	assert.Equal(t, 20, got.MinPtimeMs)
	assert.True(t, got.ChannelAudioUplift)
}

func TestMediaForChannel_UnknownChannelTier_FallsBackToPersonal(t *testing.T) {
	assert.Equal(t,
		entitlements.MediaFor(entitlements.TierFree),
		entitlements.MediaForChannel(entitlements.TierFree, entitlements.TierMach, "garbage"))
}
