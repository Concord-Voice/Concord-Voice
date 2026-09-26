package testhelpers

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
)

// SeedPermissionGenerations creates the user and server permission
// generations if they are absent. Under #3453's seed-and-skip rule a resolver
// read that misses PUBLISHES only when both already exist; seeding first makes
// the next such read the one that writes the cache.
func SeedPermissionGenerations(t *testing.T, rdb *redis.Client, serverID, userID string) rbac.GenTags {
	t.Helper()
	ctx := context.Background()
	cache := rbac.NewPermissionCache(rdb)
	// Set with empty tags seeds each absent generation and writes no value.
	require.NoError(t, cache.Set(ctx, serverID, userID, "", 0, rbac.GenTags{}))
	tags := cache.Generations(ctx, serverID, userID)
	require.NotEmpty(t, tags.User, "user permission generation must exist after seeding")
	require.NotEmpty(t, tags.Server, "server permission generation must exist after seeding")
	return tags
}

// PublishPermissionCache plants perm as a permission-cache entry the cache
// will actually SERVE: tagged with the current generations, in the current
// value format. It requires the entry to be served afterwards. A raw
// `perm:` SET of a bare integer is no longer read by anything (#3453), so a
// test planting one would pass whether or not the code it names ran.
func PublishPermissionCache(t *testing.T, rdb *redis.Client, serverID, userID, channelID string, perm rbac.Permission) {
	t.Helper()
	ctx := context.Background()
	cache := rbac.NewPermissionCache(rdb)
	tags := SeedPermissionGenerations(t, rdb, serverID, userID)
	require.NoError(t, cache.Set(ctx, serverID, userID, channelID, perm, tags))
	got, ok, _ := cache.Get(ctx, serverID, userID, channelID)
	require.True(t, ok, "a published permission-cache entry must be served")
	require.Equal(t, perm, got)
}
