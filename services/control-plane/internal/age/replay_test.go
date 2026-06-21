package age

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCheckTimestamp_AsymmetricWindow(t *testing.T) {
	now := time.Unix(1750000000, 0)
	// Accepted: exact, full -300s into the past, +30s forward skew.
	assert.NoError(t, CheckTimestamp(now, 1750000000))
	assert.NoError(t, CheckTimestamp(now, 1750000000-300))
	assert.NoError(t, CheckTimestamp(now, 1750000000+30))
	// Rejected: just past either edge.
	assert.ErrorIs(t, CheckTimestamp(now, 1750000000-301), ErrStaleTimestamp)
	assert.ErrorIs(t, CheckTimestamp(now, 1750000000+31), ErrStaleTimestamp)
	// Far past / far future also rejected.
	assert.ErrorIs(t, CheckTimestamp(now, 1750000000-100000), ErrStaleTimestamp)
	assert.ErrorIs(t, CheckTimestamp(now, 1750000000+100000), ErrStaleTimestamp)
}

// TestNonceTTLCoversWindow locks the no-replay-seam invariant: the nonce TTL must
// be >= the full accept window (past + future), so a claim can never outlive its
// nonce (which would reopen a replay window after the nonce key expires).
func TestNonceTTLCoversWindow(t *testing.T) {
	assert.GreaterOrEqual(t, nonceTTL, windowPast+windowFuture,
		"nonce TTL must cover the full timestamp window (no replay seam)")
}
