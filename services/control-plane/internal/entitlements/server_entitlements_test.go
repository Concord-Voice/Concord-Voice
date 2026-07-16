package entitlements_test

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
)

// Ladder values are the pricing ground truth (concordvoice-com teams.astro,
// finalized 2026-06-18; ADR-0028). Byte-identical conformance is the point of
// these tests — do not "fix" a mismatch here without checking the pricing pages.
func TestForServer_LadderValues(t *testing.T) {
	cases := []struct {
		tier           string
		emoji          int
		stickers       int
		soundboards    int
		uploadBytes    int64
		iconBytes      int64
		floorH         int
		floorFps       int
		audioUnlock    bool
		animatedBanner bool
	}{
		{entitlements.TierGroundspeed, 75, 10, 15, 33_554_432, 5_242_880, 0, 0, false, false},
		{entitlements.TierMach1, 250, 75, 30, 134_217_728, 8_388_608, 1080, 60, true, true},
		{entitlements.TierMach2, 350, 100, 40, 268_435_456, 8_388_608, 1440, 60, true, true},
		{entitlements.TierMach3, 500, 150, 55, 536_870_912, 8_388_608, 2160, 60, true, true},
		{entitlements.TierSelfHost, -1, -1, -1, -1, 8_388_608, 2160, 60, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			e := entitlements.ForServer(tc.tier)
			assert.Equal(t, tc.tier, e.Tier)
			assert.Equal(t, tc.emoji, e.MaxServerCustomEmoji)
			assert.Equal(t, tc.stickers, e.MaxServerStickers)
			assert.Equal(t, tc.soundboards, e.MaxServerSoundboards)
			assert.Equal(t, tc.uploadBytes, e.MaxServerUploadBytes)
			assert.Equal(t, tc.iconBytes, e.MaxServerIconBytes)
			assert.Equal(t, tc.iconBytes, e.MaxServerBannerBytes)
			assert.Equal(t, tc.floorH, e.ServerVideoFloorHeight)
			assert.Equal(t, tc.floorFps, e.ServerVideoFloorFps)
			assert.Equal(t, int64(tc.floorH)*int64(tc.floorH)*16/9*int64(tc.floorFps), e.ServerVideoFloorPixelRate,
				"pixel rate = width(16:9)*height*fps")
			assert.Equal(t, tc.audioUnlock, e.UnlockServerAudioQualityCaps)
			assert.Equal(t, tc.animatedBanner, e.AllowAnimatedServerBanner,
				"animated GIF server banner is a Mach 1+ / selfhost perk (#1302)")
			// Storage pool stays the #1523 sentinel on every SaaS row; selfhost is unlimited.
			if tc.tier == entitlements.TierSelfHost {
				assert.Equal(t, int64(entitlements.ServerLimitUnlimited), e.MaxServerStoragePoolBytes)
			} else {
				assert.Equal(t, entitlements.ServerStoragePoolUnset, e.MaxServerStoragePoolBytes)
			}
		})
	}
}

func TestForServer_UnknownTierFailsClosedToGroundspeed(t *testing.T) {
	groundspeed := entitlements.ForServer(entitlements.TierGroundspeed)
	// "mach" is the RETIRED pre-ladder binary tier string (ADR-0028): it must fail
	// closed to groundspeed, never resolve to a Mach row. Regression-locked here.
	for _, tier := range []string{"", "garbage", "MACH", "Groundspeed", "premium", "mach ", "mach", "Mach1", "mach4"} {
		t.Run("tier="+tier, func(t *testing.T) {
			assert.Equal(t, groundspeed, entitlements.ForServer(tier),
				"any unknown/empty/mis-cased/retired tier must fail closed to Groundspeed (least privilege)")
		})
	}
}

// RoomCapTierForServer collapses the server ladder to the media-plane's binary
// channel room-cap tier (ADR-0029 amendment): Groundspeed → free, any Mach or
// selfhost → premium, everything unknown/retired → free (least privilege).
func TestRoomCapTierForServer(t *testing.T) {
	premium := []string{
		entitlements.TierMach1, entitlements.TierMach2, entitlements.TierMach3, entitlements.TierSelfHost,
	}
	for _, tier := range premium {
		t.Run("premium/"+tier, func(t *testing.T) {
			assert.Equal(t, entitlements.TierPremium, entitlements.RoomCapTierForServer(tier))
		})
	}
	// Groundspeed AND every unknown/empty/mis-cased/retired tier fail closed to free —
	// a stale/garbage server tier can never grant the premium room cap.
	free := []string{entitlements.TierGroundspeed, "", "garbage", "MACH", "mach", "Mach1", "premium"}
	for _, tier := range free {
		t.Run("free/"+tier, func(t *testing.T) {
			assert.Equal(t, entitlements.TierFree, entitlements.RoomCapTierForServer(tier))
		})
	}
}

func TestServerSentinels(t *testing.T) {
	// Two DISTINCT sentinels: Unset = "no decision yet, do not enforce" (#1523);
	// Unlimited = "explicitly no limit" (selfhost). Both negative, semantically different.
	assert.Equal(t, int64(-1), entitlements.ServerStoragePoolUnset)
	assert.Equal(t, int64(-1), int64(entitlements.ServerLimitUnlimited))
}
