package entitlements_test

import (
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
)

func TestForServer_Groundspeed(t *testing.T) {
	e := entitlements.ForServer(entitlements.TierGroundspeed)
	assert.Equal(t, "groundspeed", e.Tier)
	assert.Equal(t, 75, e.MaxServerCustomEmoji)
	assert.Equal(t, 10, e.MaxServerStickers)
	assert.Equal(t, entitlements.ServerStoragePoolUnset, e.MaxServerStoragePoolBytes)
	assert.False(t, e.UnlockServerAudioQualityCaps)
	assert.False(t, e.UnlockServerVideoQualityCaps)
}

func TestForServer_Mach(t *testing.T) {
	e := entitlements.ForServer(entitlements.TierMach)
	assert.Equal(t, "mach", e.Tier)
	assert.Equal(t, 125, e.MaxServerCustomEmoji)
	assert.Equal(t, 75, e.MaxServerStickers)
	// Storage pool is A11-pending: same sentinel on BOTH tiers until #1523 lands.
	assert.Equal(t, entitlements.ServerStoragePoolUnset, e.MaxServerStoragePoolBytes)
	assert.True(t, e.UnlockServerAudioQualityCaps)
	assert.True(t, e.UnlockServerVideoQualityCaps)
}

func TestForServer_UnknownTierFailsClosedToGroundspeed(t *testing.T) {
	groundspeed := entitlements.ForServer(entitlements.TierGroundspeed)
	for _, tier := range []string{"", "garbage", "MACH", "Groundspeed", "premium", "mach "} {
		t.Run("tier="+tier, func(t *testing.T) {
			assert.Equal(t, groundspeed, entitlements.ForServer(tier),
				"any unknown/empty/mis-cased tier must fail closed to Groundspeed (least privilege)")
		})
	}
}

func TestForServer_StorageSentinelIsNegativeOne(t *testing.T) {
	// The A11-pending sentinel must be a distinct "no decision" marker, NOT zero —
	// downstream storage gates must treat -1 as "do not enforce", not "zero bytes".
	assert.Equal(t, int64(-1), entitlements.ServerStoragePoolUnset)
}

func TestForServer_Deterministic(t *testing.T) {
	assert.Equal(t, entitlements.ForServer(entitlements.TierMach), entitlements.ForServer(entitlements.TierMach))
	assert.Equal(t, entitlements.ForServer(entitlements.TierGroundspeed), entitlements.ForServer(entitlements.TierGroundspeed))
}
