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
