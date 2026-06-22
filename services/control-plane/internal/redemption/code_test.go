package redemption

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateRawCode_ShapeAndChecksum verifies generated codes have the right
// length, only Crockford symbols, and a self-consistent checksum.
func TestGenerateRawCode_ShapeAndChecksum(t *testing.T) {
	for i := 0; i < 200; i++ {
		raw, err := generateRawCode()
		require.NoError(t, err)
		require.Len(t, raw, payloadSymbols+1, "payload + 1 checksum symbol")

		for j := 0; j < len(raw); j++ {
			assert.GreaterOrEqual(t, crockfordValue(raw[j]), 0, "symbol %q must be valid Crockford", string(raw[j]))
		}

		// The checksum symbol must match the payload.
		payload := raw[:len(raw)-1]
		assert.Equal(t, raw[len(raw)-1], checksumSymbol(payload), "checksum self-consistency")
	}
}

// TestGenerateRawCode_Uniqueness asserts no collisions across many draws (a
// smoke test of the CSPRNG, not a statistical proof).
func TestGenerateRawCode_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 5000)
	for i := 0; i < 5000; i++ {
		raw, err := generateRawCode()
		require.NoError(t, err)
		_, dup := seen[raw]
		require.False(t, dup, "generated a duplicate code (entropy failure)")
		seen[raw] = struct{}{}
	}
}

// TestNormalizeAndValidate_RoundTrip ensures a formatted code (with prefix +
// hyphen grouping) normalizes back to a checksum-valid canonical string and
// hashes deterministically regardless of formatting noise.
func TestNormalizeAndValidate_RoundTrip(t *testing.T) {
	raw, err := generateRawCode()
	require.NoError(t, err)

	cases := []string{
		formatCode(raw, ""),               // grouped, no prefix
		formatCode(raw, "KS"),             // grouped, with prefix
		formatCode(raw, "promo"),          // lowercase prefix
		strings.ToLower(raw),              // ungrouped lowercase, no hyphens
		"  " + formatCode(raw, "") + "  ", // surrounding whitespace
	}

	want := raw // canonical = ungrouped uppercase payload+checksum
	var firstHash string
	for i, in := range cases {
		got, err := NormalizeAndValidate(in)
		require.NoErrorf(t, err, "case %d %q should validate", i, in)
		assert.Equalf(t, want, got, "case %d normalized form", i)

		h := HashCode(got)
		if firstHash == "" {
			firstHash = h
		} else {
			assert.Equalf(t, firstHash, h, "case %d hash must match (formatting-invariant)", i)
		}
	}
}

// TestNormalizeAndValidate_CrockfordAliases verifies I/L→1 and O→0 folding so a
// user who types the ambiguous letters still redeems successfully.
func TestNormalizeAndValidate_CrockfordAliases(t *testing.T) {
	raw, err := generateRawCode()
	require.NoError(t, err)

	canonical, err := NormalizeAndValidate(raw)
	require.NoError(t, err)

	// Replace digits 1/0 in the canonical form with their letter aliases and
	// confirm it still normalizes to the same canonical string.
	aliased := strings.NewReplacer("1", "I", "0", "O").Replace(canonical)
	got, err := NormalizeAndValidate(aliased)
	require.NoError(t, err)
	assert.Equal(t, canonical, got, "alias-folded input must normalize identically")
}

// TestNormalizeAndValidate_RejectsBadChecksum covers the pre-DB typo/probe
// filter: a flipped symbol fails the checksum.
func TestNormalizeAndValidate_RejectsBadChecksum(t *testing.T) {
	raw, err := generateRawCode()
	require.NoError(t, err)

	// Flip the first payload symbol to a different valid symbol.
	bad := []byte(raw)
	if bad[0] == '0' {
		bad[0] = '1'
	} else {
		bad[0] = '0'
	}

	_, err = NormalizeAndValidate(string(bad))
	require.ErrorIs(t, err, errBadChecksum)
}

// TestNormalizeAndValidate_RejectsGarbage covers empty / too-short / invalid
// rune inputs.
func TestNormalizeAndValidate_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "X", "-", "   ", "!!!!"} {
		_, err := NormalizeAndValidate(in)
		assert.ErrorIs(t, err, errBadChecksum, "input %q must be rejected", in)
	}
}

// TestHashCode_Deterministic confirms HashCode is a stable lowercase-hex
// SHA-256 (the value stored in code_hash).
func TestHashCode_Deterministic(t *testing.T) {
	const fixtureCode = "ABCDE12345" // pragma: allowlist secret -- test fixture, not a credential
	const otherCode = "ABCDE12346"   // pragma: allowlist secret -- test fixture, not a credential
	h1 := HashCode(fixtureCode)
	h2 := HashCode(fixtureCode)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 64, "hex SHA-256 is 64 chars")
	assert.Equal(t, strings.ToLower(h1), h1, "lowercase hex")
	assert.NotEqual(t, h1, HashCode(otherCode), "different input → different hash")
}

// TestFormatCode_Grouping checks hyphen grouping and prefix prepending.
func TestFormatCode_Grouping(t *testing.T) {
	raw := "ABCDEFGHIJ" // 10 symbols → two groups of 5
	assert.Equal(t, "ABCDE-FGHIJ", formatCode(raw, ""))
	assert.Equal(t, "KS-ABCDE-FGHIJ", formatCode(raw, "KS"))
	assert.Equal(t, "PROMO-ABCDE-FGHIJ", formatCode(raw, "promo"))
}
