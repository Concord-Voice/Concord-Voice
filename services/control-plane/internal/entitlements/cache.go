package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const cacheTTL = 5 * time.Minute

// Cache is a Redis-backed, read-through cache of a user's entitlement tier.
// Modeled on internal/rbac/cache.go (5-min TTL, Unlink invalidation). One key
// per user (ent:{userID}) so Invalidate needs no SCAN. GetTier never errors —
// every failure path fails closed to TierFree (least privilege).
type Cache struct {
	redis *redis.Client
	db    *sql.DB
	ttl   time.Duration
}

// NewCache builds the cache from the existing Redis + DB handles.
func NewCache(redisClient *redis.Client, db *sql.DB) *Cache {
	return &Cache{redis: redisClient, db: db, ttl: cacheTTL}
}

func (c *Cache) key(userID string) string { return "ent:" + userID }

// GetTier returns the user's tier, reading through to the subscriptions table on
// a cache miss and populating the cache. On a Redis error (other than a miss) it
// degrades to a direct DB resolve rather than failing open.
func (c *Cache) GetTier(ctx context.Context, userID string) string {
	val, err := c.redis.Get(ctx, c.key(userID)).Result()
	switch {
	case err == nil:
		return val
	case errors.Is(err, redis.Nil):
		// genuine cache miss — continue past the switch to the read-through below
	default:
		log.Printf("entitlements: cache GetTier redis error for user %q: %v", userID, err)
		return ResolveTier(ctx, c.db, userID)
	}

	tier := ResolveTier(ctx, c.db, userID)
	if setErr := c.SetTier(ctx, userID, tier); setErr != nil {
		log.Printf("entitlements: cache SetTier failed for user %q: %v", userID, setErr)
	}
	return tier
}

// SetTier stores the tier string with the cache TTL.
func (c *Cache) SetTier(ctx context.Context, userID, tier string) error {
	return c.redis.Set(ctx, c.key(userID), tier, c.ttl).Err()
}

// Invalidate removes the cached tier (called on any subscription change).
func (c *Cache) Invalidate(ctx context.Context, userID string) error {
	return c.redis.Unlink(ctx, c.key(userID)).Err()
}
