package rbac_test

import (
	"context"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupCache(t *testing.T) (*rbac.PermissionCache, *testhelpers.TestServer) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	return cache, ts
}

func TestCacheGetMiss(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	perm, ok := cache.Get(ctx, "server1", "user1", "")
	assert.False(t, ok, "cache miss should return false")
	assert.Equal(t, rbac.Permission(0), perm)
}

func TestCacheSetAndGetServerLevel(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	expected := rbac.PermViewTextChannels | rbac.PermSendMessages
	err := cache.Set(ctx, "server1", "user1", "", expected)
	require.NoError(t, err)

	perm, ok := cache.Get(ctx, "server1", "user1", "")
	assert.True(t, ok, "cache hit should return true")
	assert.Equal(t, expected, perm)
}

func TestCacheSetAndGetChannelLevel(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	expected := rbac.PermViewTextChannels | rbac.PermSendMessages | rbac.PermKick
	err := cache.Set(ctx, "server1", "user1", "channel1", expected)
	require.NoError(t, err)

	perm, ok := cache.Get(ctx, "server1", "user1", "channel1")
	assert.True(t, ok, "cache hit should return true")
	assert.Equal(t, expected, perm)
}

func TestCacheServerAndChannelKeysAreSeparate(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	serverPerm := rbac.PermViewTextChannels
	channelPerm := rbac.PermViewTextChannels | rbac.PermKick

	require.NoError(t, cache.Set(ctx, "server1", "user1", "", serverPerm))
	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel1", channelPerm))

	// They should be independent
	perm1, ok := cache.Get(ctx, "server1", "user1", "")
	assert.True(t, ok)
	assert.Equal(t, serverPerm, perm1)

	perm2, ok := cache.Get(ctx, "server1", "user1", "channel1")
	assert.True(t, ok)
	assert.Equal(t, channelPerm, perm2)
}

func TestCacheInvalidateUser(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	// Set both server-level and channel-level permissions
	require.NoError(t, cache.Set(ctx, "server1", "user1", "", rbac.PermViewTextChannels))
	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel1", rbac.PermSendMessages))
	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel2", rbac.PermKick))

	// Invalidate user
	err := cache.Invalidate(ctx, "server1", "user1")
	require.NoError(t, err)

	// All keys should be gone
	_, ok := cache.Get(ctx, "server1", "user1", "")
	assert.False(t, ok, "server-level cache should be invalidated")

	_, ok = cache.Get(ctx, "server1", "user1", "channel1")
	assert.False(t, ok, "channel1 cache should be invalidated")

	_, ok = cache.Get(ctx, "server1", "user1", "channel2")
	assert.False(t, ok, "channel2 cache should be invalidated")
}

func TestCacheInvalidateDoesNotAffectOtherUsers(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	require.NoError(t, cache.Set(ctx, "server1", "user1", "", rbac.PermViewTextChannels))
	require.NoError(t, cache.Set(ctx, "server1", "user2", "", rbac.PermSendMessages))

	err := cache.Invalidate(ctx, "server1", "user1")
	require.NoError(t, err)

	_, ok := cache.Get(ctx, "server1", "user1", "")
	assert.False(t, ok, "user1 should be invalidated")

	_, ok = cache.Get(ctx, "server1", "user2", "")
	assert.True(t, ok, "user2 should not be affected")
}

func TestCacheInvalidateServer(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	// Set permissions for multiple users in the same server
	require.NoError(t, cache.Set(ctx, "server1", "user1", "", rbac.PermViewTextChannels))
	require.NoError(t, cache.Set(ctx, "server1", "user2", "", rbac.PermSendMessages))
	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel1", rbac.PermKick))

	// Also set for a different server (should not be affected)
	require.NoError(t, cache.Set(ctx, "server2", "user1", "", rbac.PermBan))

	err := cache.InvalidateServer(ctx, "server1")
	require.NoError(t, err)

	_, ok := cache.Get(ctx, "server1", "user1", "")
	assert.False(t, ok, "server1 user1 should be invalidated")

	_, ok = cache.Get(ctx, "server1", "user2", "")
	assert.False(t, ok, "server1 user2 should be invalidated")

	_, ok = cache.Get(ctx, "server1", "user1", "channel1")
	assert.False(t, ok, "server1 user1 channel1 should be invalidated")

	// server2 should be untouched
	_, ok = cache.Get(ctx, "server2", "user1", "")
	assert.True(t, ok, "server2 should not be affected")
}

func TestCacheInvalidateChannel(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel1", rbac.PermViewTextChannels))
	require.NoError(t, cache.Set(ctx, "server1", "user2", "channel1", rbac.PermSendMessages))
	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel2", rbac.PermKick))
	require.NoError(t, cache.Set(ctx, "server1", "user1", "", rbac.PermBan))

	err := cache.InvalidateChannel(ctx, "server1", "channel1")
	require.NoError(t, err)

	_, ok := cache.Get(ctx, "server1", "user1", "channel1")
	assert.False(t, ok, "channel1 user1 should be invalidated")

	_, ok = cache.Get(ctx, "server1", "user2", "channel1")
	assert.False(t, ok, "channel1 user2 should be invalidated")

	// Other channels and server-level should be untouched
	_, ok = cache.Get(ctx, "server1", "user1", "channel2")
	assert.True(t, ok, "channel2 should not be affected")

	_, ok = cache.Get(ctx, "server1", "user1", "")
	assert.True(t, ok, "server-level should not be affected")
}

func TestCacheTTLBehavior(t *testing.T) {
	// This test verifies TTL is set on cache entries by checking they expire.
	// We can't easily test the 5-minute default TTL, but we can verify
	// the TTL is attached by checking the Redis key's TTL.
	cache, ts := setupCache(t)
	ctx := context.Background()

	require.NoError(t, cache.Set(ctx, "server1", "user1", "", rbac.PermViewTextChannels))

	ttl, err := ts.Redis.TTL(ctx, "perm:server1:user1").Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0, "cache key should have a positive TTL")
	assert.True(t, ttl <= 5*time.Minute, "cache TTL should not exceed 5 minutes")
}

func TestCacheInvalidateEmptyServer(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	// Invalidating a server with no cached entries should not error
	err := cache.InvalidateServer(ctx, "nonexistent-server")
	assert.NoError(t, err)
}

func TestCacheInvalidateEmptyUser(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	// Invalidating a user with no cached entries should not error
	err := cache.Invalidate(ctx, "server1", "nonexistent-user")
	assert.NoError(t, err)
}

func TestCacheGetDifferentChannelReturnsOwnValue(t *testing.T) {
	cache, _ := setupCache(t)
	ctx := context.Background()

	// Getting a channel key that was never set should miss
	require.NoError(t, cache.Set(ctx, "server1", "user1", "channel1", rbac.PermKick))

	_, ok := cache.Get(ctx, "server1", "user1", "channel999")
	assert.False(t, ok, "different channel should be a cache miss")
}
