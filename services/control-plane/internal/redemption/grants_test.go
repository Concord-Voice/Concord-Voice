package redemption

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCatalog_LookupAndSupports verifies exact + prefix dispatch and the
// unknown-kind rejection.
func TestCatalog_LookupAndSupports(t *testing.T) {
	c := NewCatalog()

	t.Run("exact premium", func(t *testing.T) {
		_, err := c.lookup(GrantPremiumSubscription)
		require.NoError(t, err)
		assert.True(t, c.Supports(GrantPremiumSubscription))
	})

	t.Run("feature prefix with capability", func(t *testing.T) {
		_, err := c.lookup("feature:custom_themes")
		require.NoError(t, err)
		assert.True(t, c.Supports("feature:custom_themes"))
	})

	t.Run("cosmetic prefix with id", func(t *testing.T) {
		_, err := c.lookup("cosmetic:founder_badge")
		require.NoError(t, err)
		assert.True(t, c.Supports("cosmetic:founder_badge"))
	})

	t.Run("bare prefix (no capability) is NOT supported", func(t *testing.T) {
		// "feature:" with nothing after must not match — there is no capability.
		_, err := c.lookup("feature:")
		assert.ErrorIs(t, err, errUnknownGrantKind)
		assert.False(t, c.Supports("feature:"))
	})

	t.Run("unknown kind rejected", func(t *testing.T) {
		_, err := c.lookup("nonsense:whatever")
		assert.ErrorIs(t, err, errUnknownGrantKind)
		assert.False(t, c.Supports("nonsense:whatever"))
	})
}

// TestGrantFeatureFlag_And_Cosmetic exercise the no-DB ledger-of-record effects.
func TestGrantFeatureFlag_And_Cosmetic(t *testing.T) {
	ctx := context.Background()
	uid := uuid.New()

	res, err := grantFeatureFlag(ctx, nil, uid, "feature:custom_themes", nil)
	require.NoError(t, err)
	assert.Contains(t, res.Description, "custom_themes")
	assert.False(t, res.TierChanged)

	res, err = grantCosmetic(ctx, nil, uid, "cosmetic:founder_badge", nil)
	require.NoError(t, err)
	assert.Contains(t, res.Description, "founder_badge")

	// Empty capability/id → unknown (defends a malformed issued code).
	_, err = grantFeatureFlag(ctx, nil, uid, "feature:", nil)
	assert.ErrorIs(t, err, errUnknownGrantKind)
	_, err = grantCosmetic(ctx, nil, uid, "cosmetic:", nil)
	assert.ErrorIs(t, err, errUnknownGrantKind)
}

// TestMonthsFromParams covers default, explicit, clamp-low, and clamp-high.
func TestMonthsFromParams(t *testing.T) {
	assert.Equal(t, 1, monthsFromParams(nil), "missing → default 1")
	assert.Equal(t, 1, monthsFromParams(map[string]any{}), "empty → default 1")
	assert.Equal(t, 12, monthsFromParams(map[string]any{"months": float64(12)}), "json float decode")
	assert.Equal(t, 6, monthsFromParams(map[string]any{"months": 6}), "int")
	assert.Equal(t, 1, monthsFromParams(map[string]any{"months": float64(0)}), "zero clamps to 1")
	assert.Equal(t, 1, monthsFromParams(map[string]any{"months": float64(-5)}), "negative clamps to 1")
	assert.Equal(t, 1200, monthsFromParams(map[string]any{"months": float64(99999)}), "absurd clamps to max")
}

// TestParseMaxRedeems covers the CLI flag parsing.
func TestParseMaxRedeems(t *testing.T) {
	p, err := ParseMaxRedeems("")
	require.NoError(t, err)
	assert.Nil(t, p, "empty → unlimited (nil)")

	p, err = ParseMaxRedeems("unlimited")
	require.NoError(t, err)
	assert.Nil(t, p)

	p, err = ParseMaxRedeems("5")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, 5, *p)

	_, err = ParseMaxRedeems("0")
	assert.Error(t, err, "0 is invalid")
	_, err = ParseMaxRedeems("-3")
	assert.Error(t, err)
	_, err = ParseMaxRedeems("abc")
	assert.Error(t, err)
}

// TestWriteCSV checks header + rows + flush.
func TestWriteCSV(t *testing.T) {
	codes := []IssuedCode{
		{ID: uuid.New(), Plaintext: "KS-ABCDE-FGHIJ"},
		{ID: uuid.New(), Plaintext: "KS-KLMNP-QRSTV"},
	}
	var buf bytes.Buffer
	require.NoError(t, WriteCSV(&buf, codes, "ks-2026-founder", GrantPremiumSubscription))

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3, "header + 2 rows")
	assert.Equal(t, "code,batch_id,grant_kind", lines[0])
	assert.Contains(t, lines[1], "KS-ABCDE-FGHIJ")
	assert.Contains(t, lines[1], "ks-2026-founder")
	assert.Contains(t, lines[1], GrantPremiumSubscription)
	assert.Contains(t, lines[2], "KS-KLMNP-QRSTV")
}

// silence unused sql import if future test seams drop the tx-typed effects.
var _ = (*sql.Tx)(nil)
