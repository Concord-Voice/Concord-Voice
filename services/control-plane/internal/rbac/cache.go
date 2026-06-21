package rbac

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// PermissionCache provides Redis-backed caching for computed permissions
// to avoid expensive database queries on every permission check.
// Cache TTL: 5 minutes (balance between consistency and performance)
type PermissionCache struct {
	redis *redis.Client
	ttl   time.Duration
}

// NewPermissionCache creates a new permission cache
func NewPermissionCache(redisClient *redis.Client) *PermissionCache {
	return &PermissionCache{
		redis: redisClient,
		ttl:   5 * time.Minute,
	}
}

// cacheKey generates a Redis key for caching user permissions
// Format: "perm:{serverID}:{userID}:{channelID}"
// If channelID is empty, caches server-level permissions only
func (c *PermissionCache) cacheKey(serverID, userID, channelID string) string {
	if channelID == "" {
		return fmt.Sprintf("perm:%s:%s", serverID, userID)
	}
	return fmt.Sprintf("perm:%s:%s:%s", serverID, userID, channelID)
}

// Get retrieves cached permissions for a user
// Returns (permissions, true) if found, (0, false) if not cached
func (c *PermissionCache) Get(ctx context.Context, serverID, userID, channelID string) (Permission, bool) {
	key := c.cacheKey(serverID, userID, channelID)
	val, err := c.redis.Get(ctx, key).Result()
	if err == redis.Nil {
		return 0, false
	}
	if err != nil {
		// Non-fatal: cache miss on error
		return 0, false
	}

	perm, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, false
	}

	return Permission(perm), true
}

// Set stores computed permissions in cache with TTL
func (c *PermissionCache) Set(ctx context.Context, serverID, userID, channelID string, perm Permission) error {
	key := c.cacheKey(serverID, userID, channelID)
	return c.redis.Set(ctx, key, int64(perm), c.ttl).Err()
}

// Invalidate removes cached permissions for a user (called after role changes)
func (c *PermissionCache) Invalidate(ctx context.Context, serverID, userID string) error {
	// Delete server-level key directly
	serverKey := c.cacheKey(serverID, userID, "")

	// SCAN for channel-level keys: perm:{serverID}:{userID}:{channelID}
	pattern := fmt.Sprintf("perm:%s:%s:*", serverID, userID)
	keys := []string{serverKey}
	iter := c.redis.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return err
	}

	// Batch delete all keys in one round-trip
	if len(keys) > 0 {
		return c.redis.Unlink(ctx, keys...).Err()
	}
	return nil
}

// InvalidateServer removes all cached permissions for a server (called after role/permission changes)
func (c *PermissionCache) InvalidateServer(ctx context.Context, serverID string) error {
	pattern := fmt.Sprintf("perm:%s:*", serverID)

	var keys []string
	iter := c.redis.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return err
	}

	if len(keys) > 0 {
		return c.redis.Unlink(ctx, keys...).Err()
	}
	return nil
}

// InvalidateChannel removes cached permissions for a channel (called after channel permission overrides change)
func (c *PermissionCache) InvalidateChannel(ctx context.Context, serverID, channelID string) error {
	pattern := fmt.Sprintf("perm:%s:*:%s", serverID, channelID)

	var keys []string
	iter := c.redis.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return err
	}

	if len(keys) > 0 {
		return c.redis.Unlink(ctx, keys...).Err()
	}
	return nil
}
