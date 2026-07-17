package middleware_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// AllowUserAction is the fail-closed non-gin rate-limit primitive backing purge-on-ban/kick
// (#1353). These tests exercise it directly (miniredis, no DB) so the coverage lives in the
// middleware package's own report.

func newAllowRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestAllowUserAction_AllowsWithinBudgetThenDenies(t *testing.T) {
	rdb, _ := newAllowRedis(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		allowed, err := middleware.AllowUserAction(ctx, rdb, "k", 3, time.Hour)
		require.NoError(t, err)
		assert.True(t, allowed, "call %d within budget", i)
	}
	allowed, err := middleware.AllowUserAction(ctx, rdb, "k", 3, time.Hour)
	require.NoError(t, err)
	assert.False(t, allowed, "4th call exceeds the budget of 3")
}

func TestAllowUserAction_FirstHitSetsTTL(t *testing.T) {
	rdb, _ := newAllowRedis(t)
	ctx := context.Background()
	_, err := middleware.AllowUserAction(ctx, rdb, "k", 5, time.Hour)
	require.NoError(t, err)
	ttl, err := rdb.TTL(ctx, "k").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "first hit must set an expiry")
}

// A key that lost its TTL (e.g. an Expire that failed after the INCR persisted) must have the
// window re-applied on the next call, or the actor is locked out forever. #1353 review (Gitar).
func TestAllowUserAction_RepairsLostTTL(t *testing.T) {
	rdb, _ := newAllowRedis(t)
	ctx := context.Background()
	_, err := middleware.AllowUserAction(ctx, rdb, "k", 5, time.Hour)
	require.NoError(t, err)
	require.NoError(t, rdb.Persist(ctx, "k").Err()) // strip the TTL
	ttl, err := rdb.TTL(ctx, "k").Result()
	require.NoError(t, err)
	require.Equal(t, time.Duration(-1), ttl, "precondition: key has no expiry")

	_, err = middleware.AllowUserAction(ctx, rdb, "k", 5, time.Hour)
	require.NoError(t, err)
	ttl, err = rdb.TTL(ctx, "k").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "the lost TTL is re-applied, preventing permanent lockout")
}

func TestAllowUserAction_BackendErrorFailsClosed(t *testing.T) {
	rdb, mr := newAllowRedis(t)
	mr.Close() // Redis outage
	allowed, err := middleware.AllowUserAction(context.Background(), rdb, "k", 5, time.Hour)
	assert.Error(t, err, "a backend error is surfaced")
	assert.False(t, allowed, "fail-closed: deny on backend error")
}
