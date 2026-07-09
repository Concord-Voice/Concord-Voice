package attestation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
)

// TestParsePlatform_Valid covers the happy path for each recognised platform.
// Mirror of the Platform.Valid() switch in types.go — keep in sync if the
// enum grows.
func TestParsePlatform_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want attestation.Platform
	}{
		{"macos", attestation.PlatformMacOS},
		{"windows", attestation.PlatformWindows},
		{"linux", attestation.PlatformLinux},
		{"web", attestation.PlatformWeb},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := attestation.ParsePlatform(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParsePlatform_Invalid covers the rejection branch. Both unknown values
// and edge cases (empty string, mixed case, surrounding whitespace) must
// fail — ParsePlatform does NOT normalize, by design. The wire-string
// protocol is exact-match; normalization would mask drift between the
// client's platform-detection logic and the server's enum.
func TestParsePlatform_Invalid(t *testing.T) {
	cases := []string{
		"",
		"freebsd",
		"MACOS",     // wrong case
		" macos ",   // whitespace
		"macos\x00", // null byte
		"web ",      // trailing space
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := attestation.ParsePlatform(in)
			require.Error(t, err)
			assert.ErrorIs(t, err, attestation.ErrInvalidPlatform)
			assert.Equal(t, attestation.Platform(""), got)
		})
	}
}

// TestParsePlatform_ErrorIs verifies the sentinel error is matchable via
// errors.Is so callers can distinguish "invalid platform" from other
// validation failures using the standard error-wrapping idiom.
func TestParsePlatform_ErrorIs(t *testing.T) {
	_, err := attestation.ParsePlatform("invalid")
	require.Error(t, err)
	assert.True(t, errors.Is(err, attestation.ErrInvalidPlatform),
		"ParsePlatform error must wrap ErrInvalidPlatform")
}

// TestHashUserID covers the CV-CAN-013 ownership-binding digest: empty in →
// empty out (legacy unbound records stay unbound), the output is deterministic
// and non-raw (no direct identifier persisted), and distinct users produce
// distinct digests (the binding actually discriminates).
func TestHashUserID(t *testing.T) {
	assert.Equal(t, "", attestation.HashUserID(""),
		"empty user_id must stay empty so legacy records remain unbound")

	const userA = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	digest := attestation.HashUserID(userA)
	assert.NotEmpty(t, digest)
	assert.NotEqual(t, userA, digest, "raw user_id must never be the stored value")
	assert.Equal(t, digest, attestation.HashUserID(userA), "digest must be deterministic")
	assert.Len(t, digest, 64, "hex-encoded SHA-256 is 64 chars")
	assert.NotEqual(t, digest, attestation.HashUserID("00000000-0000-0000-0000-000000000000"),
		"distinct users must produce distinct digests")
}
