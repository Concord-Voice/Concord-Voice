package entitlements_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every server resolves to Groundspeed today (inert Mach hook); the cache
// read-through populates ent:server:{id} with the 5-min TTL.
func TestServerCache_GetServerTier_MissReadsThroughGroundspeed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := entitlements.NewServerCache(ts.Redis, ts.DB)
	sid := uuid.New().String()
	ctx := context.Background()

	assert.Equal(t, entitlements.TierGroundspeed, cache.GetServerTier(ctx, sid))
	ttl, err := ts.Redis.TTL(ctx, "ent:server:"+sid).Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0 && ttl <= 5*time.Minute, "populated key carries the 5-min TTL")
}

// A cache hit is honored verbatim — GetServerTier must not re-resolve when a
// value is already cached (this is how a future Mach value survives the TTL).
func TestServerCache_HitHonorsCachedValue(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := entitlements.NewServerCache(ts.Redis, ts.DB)
	sid := uuid.New().String()
	ctx := context.Background()

	require.NoError(t, cache.SetServerTier(ctx, sid, entitlements.TierMach))
	assert.Equal(t, entitlements.TierMach, cache.GetServerTier(ctx, sid))
}

// Invalidate clears the key so the next read re-resolves (fail-closed to free).
func TestServerCache_InvalidateForcesReResolve(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := entitlements.NewServerCache(ts.Redis, ts.DB)
	sid := uuid.New().String()
	ctx := context.Background()

	require.NoError(t, cache.SetServerTier(ctx, sid, entitlements.TierMach))
	require.NoError(t, cache.Invalidate(ctx, sid))
	assert.Equal(t, entitlements.TierGroundspeed, cache.GetServerTier(ctx, sid))
}

// *ServerCache satisfies the ServerTierResolver interface (compile-time + behavioral).
func TestServerCache_SatisfiesServerTierResolver(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	var r entitlements.ServerTierResolver = entitlements.NewServerCache(ts.Redis, ts.DB)
	assert.Equal(t, entitlements.TierGroundspeed, r.GetServerTier(context.Background(), uuid.New().String()))
}
