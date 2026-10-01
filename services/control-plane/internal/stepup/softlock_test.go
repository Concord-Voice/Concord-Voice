package stepup

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newSoftLockRedis backs a test with its own in-process miniredis (#2324).
// miniredis stays load-bearing here for the same reason it does in
// internal/api/router_klipy_ratelimit_test.go:30-38: not process isolation
// (every test process already owns a private logical database), but its
// frozen clock — TTLs advance only via FastForward — which is what lets the
// window-boundary and H6 tests below cross the 30s window without a 30s
// sleep, on a keyspace with no network to fail open on.
func newSoftLockRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// deadRedisClientAt builds a client pointed at addr with the fast-retry
// options every dead-client test case uses (C8, design spec §2.3). Both
// option fields are load-bearing for test speed: DialerRetries defaults to 5
// dial attempts, which measured ~1.7s for one dead call against an
// unreachable address; MaxRetries: -1 (no command-level retry) plus
// DialerRetries: 1 (one dial attempt) measured ~230µs for the same call —
// see the T1 report for the full before/after timing. This applies equally
// to a client whose miniredis gets closed out from under it: after Close,
// the address is exactly as dead as 127.0.0.1:1, and the default retry
// settings pay the same ~1.7s. Without this helper, the three capped
// dead-client cases per file would add seconds to this package's shard.
func deadRedisClientAt(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:          addr,
		MaxRetries:    -1,
		DialerRetries: 1,
	})
}

func deadRedisClient() *redis.Client { return deadRedisClientAt("127.0.0.1:1") }

// --- Boundary --------------------------------------------------------------

// TestDeleteSoftLock_BoundaryAndWindowRollover proves the threshold edge (15
// admitted, 16 over) and that the window fully rolls over once its TTL
// actually elapses.
//
// Mutation: change `count > DeleteSoftLockThreshold` to
// `count >= DeleteSoftLockThreshold` in Hit. That off-by-one makes the 15th
// hit (count == 15) report Over, which fails the loop below.
func TestDeleteSoftLock_BoundaryAndWindowRollover(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	for i := 1; i <= DeleteSoftLockThreshold; i++ {
		v, e := l.Hit(ctx, uid, scope)
		require.Nilf(t, e, "hit %d must not error", i)
		require.Falsef(t, v.Over, "hit %d (at the threshold) must not be over", i)
	}

	v, e := l.Hit(ctx, uid, scope)
	require.Nil(t, e)
	require.True(t, v.Over, "the 16th hit must be over")

	mr.FastForward(29900 * time.Millisecond)
	v, e = l.Hit(ctx, uid, scope)
	require.Nil(t, e)
	require.True(t, v.Over, "still inside the original 30s window, still over")

	mr.FastForward(200 * time.Millisecond)
	v, e = l.Hit(ctx, uid, scope)
	require.Nil(t, e)
	require.False(t, v.Over, "the window has fully elapsed: this is a fresh key at count 1")
	// D-2: RetryAfter is only meaningful for a tier that IS over, so a
	// non-over verdict says nothing about it here — check the key's own TTL
	// directly to prove this is genuinely a fresh 30s window, not a carried-
	// over one.
	require.Equal(t, DeleteSoftLockWindow, mr.TTL(l.Key(uid, scope)), "a fresh key carries a full 30s TTL")
	require.Equal(t, "1", mustGet(t, mr, l.Key(uid, scope)), "the rolled-over key starts at count 1")
}

// --- H6: refused hits never extend the window ------------------------------

// TestDeleteSoftLock_H6RefusalsDoNotExtendWindow is the deciding test for H6
// (design spec §5): a hit that is already Over must not push the window's
// expiry further out, or an attacker who keeps hitting the counter would
// never actually cross into a fresh window.
//
// Mutation: change `p.ExpireNX(ctx, key, DeleteSoftLockWindow)` to
// `p.Expire(ctx, key, DeleteSoftLockWindow)` (drop NX). Every refused hit
// would then re-arm the TTL to a full 30s, so the FastForward(1100ms) below
// would still land inside a (continually renewed) window and the final
// assertion — a fresh count-1 key — would fail. RUN, per task instruction:
// see the T1 report for the red/green evidence.
func TestDeleteSoftLock_H6RefusalsDoNotExtendWindow(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	for i := 1; i <= DeleteSoftLockThreshold+1; i++ { // 16 hits: the 16th is refused
		_, e := l.Hit(ctx, uid, scope)
		require.Nil(t, e)
	}

	mr.FastForward(29 * time.Second)
	v, e := l.Hit(ctx, uid, scope) // the 17th, also refused
	require.Nil(t, e)
	require.True(t, v.Over, "still refused at t=29s")
	require.Equal(t, time.Second, v.RetryAfter, "the TTL was never extended: ~1s of the ORIGINAL window is left")

	mr.FastForward(1100 * time.Millisecond) // t=30.1s from the original key's creation
	v, e = l.Hit(ctx, uid, scope)
	require.Nil(t, e)
	require.False(t, v.Over, "the original window has finally elapsed: this is a fresh key")
	// D-2: RetryAfter is only meaningful when Over, so verify the fresh
	// window on the key's own TTL rather than the (unset) verdict field.
	require.Equal(t, DeleteSoftLockWindow, mr.TTL(l.Key(uid, scope)))
	require.Equal(t, "1", mustGet(t, mr, l.Key(uid, scope)))
}

// --- TTL repair --------------------------------------------------------------

// TestDeleteSoftLock_RepairsATTLLessKey covers the "reserveVerifyAttempt
// property" from design spec §2.3: a key that already exists with no TTL
// (e.g. because an earlier EXPIRE was lost) is repaired on the very next Hit,
// rather than living forever with no window.
//
// Mutation: delete the `p.ExpireNX(...)` line from Hit entirely. The key
// would keep its TTL of 0 (unset) after this Hit, failing the TTL assertion.
// RUN, per task instruction: see the T1 report for the red/green evidence.
func TestDeleteSoftLock_RepairsATTLLessKey(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	key := l.Key(uid, scope)
	require.NoError(t, rdb.Set(ctx, key, 5, 0).Err()) // pre-existing count, deliberately no TTL
	require.Equal(t, time.Duration(0), mr.TTL(key), "precondition: the seeded key has no TTL")

	v, e := l.Hit(ctx, uid, scope)
	require.Nil(t, e)
	require.False(t, v.Over, "count 6 is under the threshold")
	require.Equal(t, "6", mustGet(t, mr, key))
	require.Equal(t, DeleteSoftLockWindow, mr.TTL(key), "the TTL-less key is repaired to a full window")
}

// --- Atomicity ---------------------------------------------------------------

// TestDeleteSoftLock_AtomicityUnderConcurrency proves design spec §2.7's
// atomicity claim: N concurrent requests at count 15 receive the distinct
// values 16..15+N, with no check-then-act overshoot and no lost update.
//
// Mutation (reasoned, not run — see the T1 report): replacing the atomic
// Redis INCR with a Go-side "read current count, then write count+1"
// sequence would lose updates under 32 concurrent goroutines, so the final
// key value would land below "32" and fewer than 17 calls would observe
// Over. This specific mutant is not exercised here because it requires
// restructuring Hit's body rather than a single line; the boundary and H6
// tests above already exercise the two named single-line mutants directly.
func TestDeleteSoftLock_AtomicityUnderConcurrency(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]*Error, n)
	overs := make([]bool, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			v, e := l.Hit(ctx, uid, scope)
			errs[i] = e
			overs[i] = v.Over
		}(i)
	}
	wg.Wait()

	overTotal := 0
	for i := 0; i < n; i++ {
		require.Nilf(t, errs[i], "goroutine %d must not error", i)
		if overs[i] {
			overTotal++
		}
	}
	require.Equal(t, n-DeleteSoftLockThreshold, overTotal, "exactly the hits past the threshold must be Over")
	require.Equal(t, "32", mustGet(t, mr, l.Key(uid, scope)), "all 32 increments land: no lost update")
}

// --- Fail closed --------------------------------------------------------------

// TestDeleteSoftLock_FailsClosed covers the three capped dead-client shapes
// (C8): a nil client, an unreachable address, and a miniredis instance closed
// out from under a live client. Every shape must deny with a 503 carrying
// Cause, and never report Over=false as if the request were merely under the
// limit.
//
// Mutation: remove the `if err != nil { return ... }` check after
// TxPipelined (or the `if l.rdb == nil` guard, for the nil case) so Hit
// returns a bare SoftLockVerdict{} with a nil *Error on failure. RUN: see the
// T1 report for the red/green evidence.
func TestDeleteSoftLock_FailsClosed(t *testing.T) {
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	assertDenies := func(t *testing.T, v SoftLockVerdict, e *Error, wantCause bool) {
		t.Helper()
		require.NotNil(t, e, "a failure must never be reported as a nil *Error")
		require.Equal(t, http.StatusServiceUnavailable, e.Status)
		require.Equal(t, ErrMsgDeleteGuardUnavailable, e.Body["error"])
		if wantCause {
			require.NotNil(t, e.Cause)
		}
		require.False(t, v.Over, "the zero-value verdict must never be mistaken for an admitted hit")
	}

	t.Run("nil client", func(t *testing.T) {
		v, e := NewDeleteSoftLock(nil).Hit(ctx, uid, scope)
		assertDenies(t, v, e, false)
		require.ErrorIs(t, e, errDeleteSoftLockUnwired)
	})

	t.Run("unreachable address", func(t *testing.T) {
		rdb := deadRedisClient()
		defer func() { _ = rdb.Close() }()
		v, e := NewDeleteSoftLock(rdb).Hit(ctx, uid, scope)
		assertDenies(t, v, e, true)
	})

	t.Run("closed miniredis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := deadRedisClientAt(mr.Addr())
		defer func() { _ = rdb.Close() }()
		mr.Close()
		v, e := NewDeleteSoftLock(rdb).Hit(ctx, uid, scope)
		assertDenies(t, v, e, true)
	})
}

// --- Scope isolation -----------------------------------------------------

// TestDeleteSoftLock_ScopesAreIndependent proves DM and per-server counters
// for the same user never share state, and neither do two servers.
//
// Mutation: make Key ignore s.kind/s.serverID and always return the DM
// spelling. Every cross-scope assertion below would then fail, because all
// three Hit sequences would collide on one counter.
func TestDeleteSoftLock_ScopesAreIndependent(t *testing.T) {
	rdb, _ := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()

	dm := DMDeleteScope()
	serverA := ServerDeleteScope(uuid.New())
	serverB := ServerDeleteScope(uuid.New())

	require.NotEqual(t, l.Key(uid, dm), l.Key(uid, serverA))
	require.NotEqual(t, l.Key(uid, serverA), l.Key(uid, serverB))

	for i := 0; i < DeleteSoftLockThreshold; i++ {
		_, e := l.Hit(ctx, uid, dm)
		require.Nil(t, e)
	}
	v, e := l.Hit(ctx, uid, dm)
	require.Nil(t, e)
	require.True(t, v.Over, "the DM scope alone reached 16 hits")

	v, e = l.Hit(ctx, uid, serverA)
	require.Nil(t, e)
	require.False(t, v.Over, "server A's counter is untouched by the DM scope")

	v, e = l.Hit(ctx, uid, serverB)
	require.Nil(t, e)
	require.False(t, v.Over, "server B's counter is untouched by server A's")
}

// --- Reset -----------------------------------------------------------------

// TestDeleteSoftLock_Reset covers both Reset outcomes: a successful DEL that
// starts the window over, and a failure that is returned rather than
// swallowed (the caller logs it at Warn per design spec §2.4 step 9.3).
//
// Mutation: make Reset swallow the Del error and always return nil. The
// "closed miniredis" case below would then fail. RUN: see the T1 report for
// the red/green evidence.
func TestDeleteSoftLock_Reset(t *testing.T) {
	// This test ends by closing mr mid-test (a capped dead-client shape, C8),
	// so rdb is built with the fast-retry options from the start rather than
	// paying the ~1.7s default DialerRetries cost only on the final call.
	mr := miniredis.RunT(t)
	rdb := deadRedisClientAt(mr.Addr())
	t.Cleanup(func() { _ = rdb.Close() })
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	for i := 0; i <= DeleteSoftLockThreshold; i++ {
		_, e := l.Hit(ctx, uid, scope)
		require.Nil(t, e)
	}
	require.True(t, mr.Exists(l.Key(uid, scope)))

	require.NoError(t, l.Reset(ctx, uid, scope))
	require.False(t, mr.Exists(l.Key(uid, scope)), "Reset deletes the key")

	v, e := l.Hit(ctx, uid, scope)
	require.Nil(t, e)
	require.False(t, v.Over, "after Reset the window starts over")

	mr.Close()
	require.Error(t, l.Reset(ctx, uid, scope), "a Reset failure is returned, not swallowed")
}

// A nil client has nothing to reset; Reset must not panic or error.
func TestDeleteSoftLock_ResetOnNilClientIsANoOp(t *testing.T) {
	require.NoError(t, NewDeleteSoftLock(nil).Reset(context.Background(), uuid.New(), DMDeleteScope()))
}

// --- D-2: sustained (day) tier -----------------------------------------

// TestDeleteSoftLock_DayTierTripsOnATrickle proves the day tier bounds a
// paced script the burst tier and the route limiter both let through: one
// hit every 31s never lets the burst key accumulate (each is a fresh key by
// the time the next hit lands, since 31s > the 30s burst window), yet the
// day tier still trips at the 101st hit.
//
// Mutation: fold `dayOver` to always false (drop the day-tier comparison).
// Every hit in the loop would still pass (they already expect Over=false),
// but the trip at 101 would never happen, failing the final assertion. RUN.
func TestDeleteSoftLock_DayTierTripsOnATrickle(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	for i := 1; i <= DeleteSoftLockDayThreshold; i++ {
		v, e := l.Hit(ctx, uid, scope)
		require.Nilf(t, e, "hit %d must not error", i)
		require.Falsef(t, v.Over, "hit %d: the burst key resets every 31s and the day count is still <=100", i)
		mr.FastForward(31 * time.Second)
	}

	v, e := l.Hit(ctx, uid, scope) // the 101st hit, ~52 minutes into a 24h window
	require.Nil(t, e)
	require.True(t, v.Over, "the day tier trips at 101 even though every hit is 31s apart")
	require.Equal(t, "1", mustGet(t, mr, l.Key(uid, scope)), "the burst counter never accumulated across the trickle")
}

// TestDeleteSoftLock_HitNTripsBothTiersAtOnce covers the self-purge case
// (D-1): a single HitN(101) crosses both the burst threshold (15) and the
// day threshold (100) in one call.
//
// Mutation: fold `dayOver` to always false. Over would still be true (via
// burstOver, since 101 > 15), but RetryAfter would then reflect only the
// burst tier's freshly-set 30s TTL rather than the day tier's larger,
// freshly-set 24h TTL, failing the RetryAfter assertion below. RUN.
func TestDeleteSoftLock_HitNTripsBothTiersAtOnce(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	v, e := l.HitN(ctx, uid, scope, DeleteSoftLockDayThreshold+1)
	require.Nil(t, e)
	require.True(t, v.Over, "both tiers trip on one HitN(101)")
	require.Equal(t, DeleteSoftLockDayWindow, v.RetryAfter,
		"the day tier's freshly-set 24h TTL is numerically larger than the burst tier's freshly-set 30s TTL")
	require.Equal(t, "101", mustGet(t, mr, l.Key(uid, scope)))
	require.Equal(t, "101", mustGet(t, mr, l.DayKey(uid)))
}

// TestDeleteSoftLock_ResetClearsBothTiers proves Reset's single DEL call
// clears the scope's burst key and the user's day key together.
//
// Mutation: a Reset that only deletes the burst key (drop l.DayKey(userID)
// from the Del call's argument list). The day-key existence check below
// would fail. RUN.
func TestDeleteSoftLock_ResetClearsBothTiers(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	_, e := l.HitN(ctx, uid, scope, DeleteSoftLockDayThreshold+1)
	require.Nil(t, e)
	require.True(t, mr.Exists(l.Key(uid, scope)))
	require.True(t, mr.Exists(l.DayKey(uid)))

	require.NoError(t, l.Reset(ctx, uid, scope))
	require.False(t, mr.Exists(l.Key(uid, scope)), "Reset deletes the burst key")
	require.False(t, mr.Exists(l.DayKey(uid)), "Reset deletes the day key")
}

// TestDeleteSoftLock_DayTierExpireNXNonExtending is H6's day-tier analog: a
// later hit must not re-arm the day key's TTL once it is already set.
//
// Mutation: change the day key's `p.ExpireNX(ctx, dayKey, DeleteSoftLockDayWindow)`
// to `p.Expire(...)` (drop NX). The second Hit below, arriving 23h after the
// first, would re-arm the TTL back to a full 24h instead of leaving ~1h,
// failing the final assertion. RUN.
func TestDeleteSoftLock_DayTierExpireNXNonExtending(t *testing.T) {
	rdb, mr := newSoftLockRedis(t)
	l := NewDeleteSoftLock(rdb)
	ctx := context.Background()
	uid := uuid.New()
	scope := DMDeleteScope()

	_, e := l.Hit(ctx, uid, scope) // creates the day key with a fresh 24h TTL
	require.Nil(t, e)
	require.Equal(t, DeleteSoftLockDayWindow, mr.TTL(l.DayKey(uid)))

	mr.FastForward(23 * time.Hour)
	_, e = l.Hit(ctx, uid, scope) // the day key already has a TTL: ExpireNX must be a no-op
	require.Nil(t, e)
	require.Equal(t, time.Hour, mr.TTL(l.DayKey(uid)), "the day key's TTL was never re-armed by a later hit")
}

func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	require.NoError(t, err)
	return v
}

// TestSettleContext_DetachedAndBounded: cancelling the request leaves the
// settlement context live, and the settlement context carries its own
// deadline no later than SettleTimeout from now (review of #3509).
func TestSettleContext_DetachedAndBounded(t *testing.T) {
	request, cancelRequest := context.WithCancel(context.Background())
	ctx, cancel := SettleContext(request)
	defer cancel()

	cancelRequest()
	require.NoError(t, ctx.Err(), "a hang-up does not cancel settlement")
	deadline, ok := ctx.Deadline()
	require.True(t, ok, "settlement is bounded")
	require.LessOrEqual(t, time.Until(deadline), SettleTimeout)
	require.Greater(t, time.Until(deadline), time.Duration(0))
}
