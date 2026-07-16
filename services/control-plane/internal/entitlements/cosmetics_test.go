package entitlements_test

import (
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllowsCosmetic_CountGates(t *testing.T) {
	gs := entitlements.ForServer(entitlements.TierGroundspeed)
	m3 := entitlements.ForServer(entitlements.TierMach3)
	sh := entitlements.ForServer(entitlements.TierSelfHost)

	cases := []struct {
		name    string
		ent     entitlements.ServerEntitlement
		kind    entitlements.CosmeticKind
		current int
		allowed bool
	}{
		{"groundspeed emoji under cap", gs, entitlements.CosmeticEmoji, 74, true},
		{"groundspeed emoji at cap", gs, entitlements.CosmeticEmoji, 75, false},
		{"groundspeed emoji over cap", gs, entitlements.CosmeticEmoji, 80, false},
		{"groundspeed sticker at cap", gs, entitlements.CosmeticSticker, 10, false},
		{"groundspeed soundboard under cap", gs, entitlements.CosmeticSoundboard, 14, true},
		{"groundspeed soundboard at cap", gs, entitlements.CosmeticSoundboard, 15, false},
		{"mach3 emoji under cap", m3, entitlements.CosmeticEmoji, 499, true},
		{"mach3 emoji at cap", m3, entitlements.CosmeticEmoji, 500, false},
		{"selfhost unlimited emoji", sh, entitlements.CosmeticEmoji, 1_000_000, true},
		{"selfhost unlimited soundboards", sh, entitlements.CosmeticSoundboard, 999, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.allowed, tc.ent.AllowsCosmetic(tc.kind, tc.current))
		})
	}
}

func TestAllowsCosmetic_UnknownKindFailsClosed(t *testing.T) {
	sh := entitlements.ForServer(entitlements.TierSelfHost)
	assert.False(t, sh.AllowsCosmetic(entitlements.CosmeticKind("banner"), 0),
		"unknown cosmetic kind must fail closed even on an unlimited tier")
}

func TestCosmeticCapError(t *testing.T) {
	gs := entitlements.ForServer(entitlements.TierGroundspeed)
	err := entitlements.CosmeticCapError(gs, entitlements.CosmeticSticker)
	require.Error(t, err)
	var capErr *entitlements.ErrCosmeticCapReached
	require.True(t, errors.As(err, &capErr))
	assert.Equal(t, entitlements.CosmeticSticker, capErr.Kind)
	assert.Equal(t, 10, capErr.Limit)
	// PII-safe by construction: message carries kind + limit only.
	assert.Equal(t, "sticker cap reached (limit 10)", capErr.Error())
}
