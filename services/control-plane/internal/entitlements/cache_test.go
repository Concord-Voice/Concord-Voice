package entitlements_test

import (
	"context"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache_GetTier_MissReadsThroughAndPopulates(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()

	// First call: miss -> read-through -> premium, and the key is now set.
	assert.Equal(t, "premium", cache.GetTier(ctx, uid))
	ttl, err := ts.Redis.TTL(ctx, "ent:"+uid).Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0 && ttl <= 5*time.Minute, "populated key carries the 5-min TTL")
}

func TestCache_GetTier_HitReturnsCached(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts) // no subscription row -> DB would resolve free
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()

	// Seed the cache with premium; GetTier must honor the cache, not the DB.
	require.NoError(t, cache.SetTier(ctx, uid, "premium"))
	assert.Equal(t, "premium", cache.GetTier(ctx, uid))
}

func TestCache_Invalidate_ForcesReResolve(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts) // DB resolves free
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()

	require.NoError(t, cache.SetTier(ctx, uid, "premium"))
	require.NoError(t, cache.Invalidate(ctx, uid))
	// Key gone -> miss -> read-through to DB (no sub) -> free.
	assert.Equal(t, entitlements.TierFree, cache.GetTier(ctx, uid))
}

func TestCache_GetTier_RedisDownDegradesToDB(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	// Redis client pointed at an unreachable port; DB is healthy.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = down.Close() }()
	cache := entitlements.NewCache(down, ts.DB)

	// Get errors (not redis.Nil) -> degrade to direct DB resolve -> premium.
	assert.Equal(t, "premium", cache.GetTier(context.Background(), uid))
}
