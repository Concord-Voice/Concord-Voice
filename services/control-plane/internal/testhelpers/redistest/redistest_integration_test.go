//go:build integration

package redistest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE regression lock for #2680 itself. Every other test in this package passed
// against a mutant that ignored the ticket and pinned every process to DB 1 —
// the original defect — because each of them compares values that all derive
// from the same allocation. Nothing asserted that two allocations DIFFER, which
// is the entire property this package exists to provide.
func TestTwoAllocationsGetDistinctIndexes(t *testing.T) {
	t.Setenv(dbOverrideEnv, "") // the pin deliberately skips allocation

	_, first, err := allocate()
	require.NoError(t, err)
	_, second, err := allocate()
	require.NoError(t, err)

	assert.NotEqual(t, first, second,
		"per-process isolation IS #2680: two allocations must not share an index")
}

// Reset must be FLUSHDB-scoped. A mutant issuing FLUSHALL passed the entire
// suite, because the only assertion was that the caller's OWN key had gone —
// which FLUSHALL also satisfies, while additionally erasing every peer process's
// database plus DB 0's ticket counter and the dev app's data. This asserts on
// what must SURVIVE, not only on what must vanish.
func TestResetLeavesOtherDatabasesIntact(t *testing.T) {
	ctx := context.Background()

	own := Client(t)

	zero := Options(t)
	zero.DB = 0
	sentinel := redis.NewClient(zero)
	t.Cleanup(func() { _ = sentinel.Close() })

	// Makes this test's power self-evident without anyone having to RUN a
	// FLUSHALL to prove it: the sentinel provably lives in a different database
	// from the one Reset is about to flush, so any unscoped flush erases it and
	// the survival assertion below fails. (Verifying by executing the mutant is
	// what wiped the shared dev Redis during review.)
	require.NotEqual(t, 0, own.Options().DB,
		"the allocated DB must differ from the sentinel's DB 0, or this test proves nothing")

	// The sentinel lives in SHARED DB 0, so a fixed key would collide with a
	// concurrent run of this same test — making a test about cross-process
	// isolation itself cross-process unsafe. Key it per process, and register the
	// cleanup immediately on success so a later failure cannot leave it behind.
	sentinelKey := fmt.Sprintf("redistest:sentinel:%d", os.Getpid())
	require.NoError(t, sentinel.Set(ctx, sentinelKey, "keep", time.Minute).Err())
	t.Cleanup(func() {
		if err := sentinel.Del(context.Background(), sentinelKey).Err(); err != nil {
			// The one-minute TTL means a failure here self-heals rather than
			// accumulating, but silently leaving a key in the ONE database this
			// design does not isolate is exactly the class of thing this test
			// exists to catch elsewhere.
			t.Errorf("redistest: failed to delete the DB 0 sentinel: %v", err)
		}
	})

	require.NoError(t, own.Set(ctx, "redistest:own", "v", 0).Err())

	require.NoError(t, Reset(ctx, own))

	gone, err := own.Exists(ctx, "redistest:own").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), gone, "Reset must clear this process's own namespace")

	survived, err := sentinel.Exists(ctx, sentinelKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), survived,
		"Reset must be FLUSHDB-scoped — FLUSHALL would erase DB 0 and every peer process")
}

// Covers baseURL() — previously untested — using the EXACT shape CI sets:
// a REDIS_URL with no DB path segment, which before #2680 meant DB 0 for every
// package in the shard.
func TestSegmentlessRedisURLStillGetsAPrivateIndex(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://:"+devRedisVal+"@localhost:6379")
	t.Setenv(dbOverrideEnv, "")

	url, db, err := allocate()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, db, 1, "CI's segment-less URL used to mean DB 0 for every package")

	opts, err := redis.ParseURL(url)
	require.NoError(t, err)
	assert.Equal(t, db, opts.DB)
}

// Extends the CWE-209 discipline past pingFailureMessage to the allocation path
// itself. The existing unit lock covers only that one helper and asserts on the
// compiled-in dev default; allocate() wraps raw driver errors with %w at two
// sites that never route through the helper, and CI supplies its own password
// via REDIS_URL rather than using the default. This drives the real path with a
// distinctive configured password and an unreachable host.
func TestAllocationFailureNeverLeaksTheConfiguredPassword(t *testing.T) {
	const configured = "n0t-the-c0mpiled-in-default"
	t.Setenv("REDIS_URL", "redis://:"+configured+"@127.0.0.1:1") // nothing listens on port 1
	t.Setenv(dbOverrideEnv, "")

	_, _, err := allocate()

	require.Error(t, err, "an unreachable Redis must fail allocation, not proceed")
	assert.NotContains(t, err.Error(), configured,
		"the configured REDIS_URL password must never reach an allocation error")

	// The host:port deliberately IS allowed through. observability.md #1 forbids
	// echoing the URL because it carries a password, not because an address is
	// secret — and the driver naming the endpoint it could not reach is the whole
	// diagnostic value of the message. An earlier draft of this test asserted the
	// address was withheld too; that was the assertion being wrong, not the code.
	assert.Contains(t, err.Error(), "failed to ping redis",
		"the failure must still say what went wrong")
}

func TestAllocatedIndexIsNeverZero(t *testing.T) {
	assert.NotEqual(t, 0, DB(t), "DB 0 holds the counter and the dev app's data")
	assert.GreaterOrEqual(t, DB(t), 1)
}

func TestResetFlushesOwnDatabase(t *testing.T) {
	ctx := context.Background()
	c := Client(t)
	require.NoError(t, c.Set(ctx, "redistest:probe", "v", 0).Err())

	require.NoError(t, Reset(ctx, c))

	n, err := c.Exists(ctx, "redistest:probe").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "Reset must clear this process's own namespace")
}

func TestResetRefusesForeignDatabase(t *testing.T) {
	ctx := context.Background()

	foreignDB := DB(t) + 1
	if foreignDB > fallbackPoolSize {
		foreignDB = 1
	}
	opts := Options(t)
	opts.DB = foreignDB
	foreign := redis.NewClient(opts)
	t.Cleanup(func() { _ = foreign.Close() })

	err := Reset(ctx, foreign)

	require.Error(t, err, "criterion-4 regression lock: a flush that could reach another process must fail")
	assert.Contains(t, err.Error(), "refusing to flush")
}

func TestResetRejectsNilClient(t *testing.T) {
	require.Error(t, Reset(context.Background(), nil))
}

// Removed in the #2789 review: TestExplicitRedisURLDatabaseIsOverridden was a
// byte-for-byte duplicate of the unit-test TestRewriteURL_ReplacesExistingDBSegment,
// misfiled behind the integration tag — and its comment claimed to prove the CI
// shape ("REDIS_URL with no DB segment") while passing a URL that HAS one, so the
// case it advertised was never exercised at all. The claim it reached for is now
// genuinely covered by TestSegmentlessRedisURLStillGetsAPrivateIndex above, which
// drives baseURL() and allocate() with the exact URL CI sets.

func TestEnvOverridePinsIndexWithoutIncrementing(t *testing.T) {
	ctx := context.Background()

	// The counter lives in DB 0, NOT in this process's allocated DB. Reading it
	// through Client(t) — which is bound to the allocated index — made both the
	// before and after reads return redis.Nil, normalize to "0", and compare
	// equal no matter what allocate() did to the counter. The test could not
	// fail. Read DB 0 explicitly.
	opts := Options(t)
	opts.DB = 0
	counter := redis.NewClient(opts)
	t.Cleanup(func() { _ = counter.Close() })

	readSeq := func() string {
		v, err := counter.Get(ctx, seqKey).Result()
		if errors.Is(err, redis.Nil) {
			return "0"
		}
		require.NoError(t, err)
		return v
	}

	before := readSeq()

	t.Setenv(dbOverrideEnv, "5")
	url, db, err := allocate()
	require.NoError(t, err)
	assert.Equal(t, 5, db)
	assert.Contains(t, url, "/5")

	assert.Equal(t, before, readSeq(), "the escape hatch must not consume a ticket")
}

func TestAllocationDoesConsumeATicket(t *testing.T) {
	// The positive control for the test above. Without it, a bug that stopped
	// allocate() from ever INCRing would leave the escape-hatch assertion
	// trivially satisfied and nothing would notice.
	ctx := context.Background()
	opts := Options(t)
	opts.DB = 0
	counter := redis.NewClient(opts)
	t.Cleanup(func() { _ = counter.Close() })

	before, err := counter.Get(ctx, seqKey).Int64()
	if errors.Is(err, redis.Nil) {
		before = 0
	} else {
		require.NoError(t, err)
	}

	_, _, err = allocate() // no CONCORD_TEST_REDIS_DB set — takes the INCR path
	require.NoError(t, err)

	after, err := counter.Get(ctx, seqKey).Int64()
	require.NoError(t, err)
	assert.Greater(t, after, before, "the allocation path must consume a ticket")
}

func TestEnvOverrideRejectsReservedZero(t *testing.T) {
	t.Setenv(dbOverrideEnv, "0")
	_, _, err := allocate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

// Removed in the #2789 review: TestAllocationSurvivesMissingConfigGet claimed to
// exercise the fail-open path "end to end", but its body never called allocate()
// and never touched Redis — it re-asserted poolSizeFrom, which
// TestPoolSizeFrom_FallsBackWhenUnavailable already covers as a unit test. It was
// also misfiled behind the integration tag for a check needing no Redis. A test
// that advertises coverage it does not provide is worse than no test, because it
// stops anyone from writing the real one.
