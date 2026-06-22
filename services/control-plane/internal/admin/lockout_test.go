package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// uniqueLockoutIP returns a per-run-unique synthetic IP so concurrent runs and
// re-runs against the shared test Redis do not collide (the lockout keys are
// hashed, but the inputs must still be unique to self-isolate).
func uniqueLockoutIP(prefix string) string {
	return prefix + "-" + uniqueAdminUsername("ip")
}

func TestLockout_LocksAfterFiveFailures(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	lk := admin.NewLockout(rdb, func() time.Time { return now })

	user := uniqueAdminUsername("acct")
	ip := uniqueLockoutIP("1.2.3")

	// First 4 failures: still unlocked (threshold is 5).
	for i := 0; i < 4; i++ {
		require.NoError(t, lk.RecordFailure(ctx, user, ip))
		locked, _, err := lk.IsLocked(ctx, user, ip)
		require.NoError(t, err)
		assert.False(t, locked, "must not lock before the 5th failure (i=%d)", i)
	}

	// 5th failure trips the lock.
	require.NoError(t, lk.RecordFailure(ctx, user, ip))
	locked, retryAfter, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.True(t, locked, "must lock on the 5th consecutive failure")
	assert.Greater(t, retryAfter, time.Duration(0), "a locked account must report a positive retry-after")
}

func TestLockout_ExponentialGrowth(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	lk := admin.NewLockout(rdb, func() time.Time { return now })

	user := uniqueAdminUsername("acct")
	ip := uniqueLockoutIP("1.2.4")

	// Drive 5 failures to the first lock window (failure 5 -> 2^0 = 1 min).
	for i := 0; i < 5; i++ {
		require.NoError(t, lk.RecordFailure(ctx, user, ip))
	}
	_, first, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.Equal(t, 1*time.Minute, first, "5th failure -> 2^(5-5)=1 min window")

	// 6th failure -> 2^1 = 2 min.
	require.NoError(t, lk.RecordFailure(ctx, user, ip))
	_, second, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, second, "6th failure -> 2^(6-5)=2 min window")

	// 7th failure -> 2^2 = 4 min.
	require.NoError(t, lk.RecordFailure(ctx, user, ip))
	_, third, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.Equal(t, 4*time.Minute, third, "7th failure -> 2^(7-5)=4 min window")
}

func TestLockout_CapsAtMax(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	lk := admin.NewLockout(rdb, func() time.Time { return now })

	user := uniqueAdminUsername("acct")
	ip := uniqueLockoutIP("1.2.5")

	// Many failures: the window must saturate at the 60m cap, never grow past it.
	for i := 0; i < 20; i++ {
		require.NoError(t, lk.RecordFailure(ctx, user, ip))
	}
	_, retryAfter, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.LessOrEqual(t, retryAfter, 60*time.Minute, "lock window must be capped at 60 min")
	assert.Equal(t, 60*time.Minute, retryAfter, "deep failure count saturates at the 60 min cap")
}

func TestLockout_PerAccountIndependentOfPerIP(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	lk := admin.NewLockout(rdb, func() time.Time { return now })

	userA := uniqueAdminUsername("acctA")
	userB := uniqueAdminUsername("acctB")
	ipShared := uniqueLockoutIP("9.9.9")
	ipOther := uniqueLockoutIP("8.8.8")

	// 5 failures for userA from ipShared -> locks BOTH acct:userA and ip:ipShared.
	for i := 0; i < 5; i++ {
		require.NoError(t, lk.RecordFailure(ctx, userA, ipShared))
	}

	// userA is locked (its account key tripped).
	lockedA, _, err := lk.IsLocked(ctx, userA, ipOther)
	require.NoError(t, err)
	assert.True(t, lockedA, "userA's account lock holds regardless of IP")

	// userB from the SHARED ip is locked too — the per-IP key tripped, even though
	// userB's own account has zero failures. (per-IP defends against spraying.)
	lockedBShared, _, err := lk.IsLocked(ctx, userB, ipShared)
	require.NoError(t, err)
	assert.True(t, lockedBShared, "the per-IP lock blocks a second account from the same IP")

	// userB from a DIFFERENT ip is NOT locked: neither its account key nor that
	// IP key has failures — proving the two axes are tracked independently.
	lockedBOther, _, err := lk.IsLocked(ctx, userB, ipOther)
	require.NoError(t, err)
	assert.False(t, lockedBOther, "an unrelated account+IP must not be locked")
}

func TestLockout_ResetClearsBoth(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	lk := admin.NewLockout(rdb, func() time.Time { return now })

	user := uniqueAdminUsername("acct")
	ip := uniqueLockoutIP("7.7.7")

	for i := 0; i < 5; i++ {
		require.NoError(t, lk.RecordFailure(ctx, user, ip))
	}
	locked, _, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	require.True(t, locked)

	// A successful login resets both axes.
	require.NoError(t, lk.Reset(ctx, user, ip))

	locked, retryAfter, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.False(t, locked, "Reset must clear the lock")
	assert.Equal(t, time.Duration(0), retryAfter)

	// And the failure counter is back to zero: 4 fresh failures stay unlocked.
	for i := 0; i < 4; i++ {
		require.NoError(t, lk.RecordFailure(ctx, user, ip))
		locked, _, err := lk.IsLocked(ctx, user, ip)
		require.NoError(t, err)
		assert.False(t, locked, "the failure counter must reset to zero (i=%d)", i)
	}
}

func TestLockout_RawIPNeverStored(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	lk := admin.NewLockout(rdb, func() time.Time { return now })

	user := uniqueAdminUsername("acct")
	ip := "203.0.113.42"

	require.NoError(t, lk.RecordFailure(ctx, user, ip))

	keys, err := rdb.Keys(ctx, "admin_lockout:ip:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	for _, k := range keys {
		assert.NotContains(t, k, ip, "raw IP must never appear in a Redis key (it is SHA-256 hashed)")
	}
}
