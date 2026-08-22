package presencehook_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
)

func TestClassifyMapsProbeStaleToARetryable503(t *testing.T) {
	// Wrapped, because every production raise site wraps it.
	f := presencehook.Classify(fmt.Errorf("ban target: %w", presencehook.ErrProbeStale))

	require.Equal(t, http.StatusServiceUnavailable, f.Status,
		"probe_stale proves no write landed, so it is not a 500")
	require.Equal(t, "probe_stale", f.Code)

	value, ok := f.RetryAfterHeader()
	require.True(t, ok,
		"a terminal may promise a retry only if it both proves no write happened and clears itself; probe_stale does both")
	require.Equal(t, "1", value)
}

func TestClassifyLeavesTheInternalArmWithoutARetryAfter(t *testing.T) {
	// The widened RetryAfterHeader gate must not leak onto the 500 arm, which
	// does NOT resolve on its own.
	f := presencehook.Classify(errors.New("some driver fault"))

	require.Equal(t, http.StatusInternalServerError, f.Status)
	require.Equal(t, "internal", f.Code)
	_, ok := f.RetryAfterHeader()
	require.False(t, ok)
}

func TestProbeStaleDoesNotOverrideTheSiteFailureMessage(t *testing.T) {
	// Only the post-commit delivery terminal overrides Body, because only it
	// describes a mutation that actually landed. probe_stale wrote nothing, so
	// the site's own message is the honest text — telling a caller their change
	// was saved would be the duplicate-action lie the override exists to prevent.
	const siteMessage = "Failed to ban member"

	f := presencehook.Classify(presencehook.ErrProbeStale)

	require.Equal(t, siteMessage, f.Body(siteMessage))
}
