package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/redis/go-redis/v9"
)

const cacheTTL = 5 * time.Minute

// Cache is a Redis-backed, read-through cache of a user's entitlement tier.
// Modeled on internal/rbac/cache.go (5-min TTL, Unlink invalidation). One key
// per user (ent:{userID}) so Invalidate needs no SCAN. GetTier never errors —
// every failure path fails closed to TierFree (least privilege).
type Cache struct {
	redis        *redis.Client
	db           *sql.DB
	ttl          time.Duration
	instanceType string
}

// NewCache builds the cache from the existing Redis + DB handles.
func NewCache(redisClient *redis.Client, db *sql.DB) *Cache {
	return NewCacheForInstance(redisClient, db, config.InstanceTypeSaaS)
}

// NewCacheForInstance builds the cache with the deployment-mode seam. In
// self-hosted mode, all users resolve to the maximal current entitlement tier.
func NewCacheForInstance(redisClient *redis.Client, db *sql.DB, instanceType string) *Cache {
	return &Cache{redis: redisClient, db: db, ttl: cacheTTL, instanceType: instanceType}
}

func (c *Cache) key(userID string) string { return "ent:" + userID }

// IsSelfHosted reports whether this cache resolves every user to the maximal
// tier (self-hosted deployment mode). Callers that resolve a post-change tier
// outside GetTier (e.g. the expiry sweeper) use this to mirror GetTier's
// self-hosted short-circuit instead of a DB resolve, which would otherwise read
// TierFree and manufacture a spurious downgrade on self-hosted instances.
func (c *Cache) IsSelfHosted() bool {
	return config.IsSelfHostedInstance(c.instanceType)
}

// GetTier returns the user's tier, reading through to the subscriptions table on
// a cache miss and populating the cache. On a Redis error (other than a miss) it
// degrades to a direct DB resolve rather than failing open. A database failure
// returns TierFree for THIS call and is never cached: a cached fallback would
// hold a paying user to free for the TTL after the database recovers, and
// GetTierChecked would read it back as an answer.
func (c *Cache) GetTier(ctx context.Context, userID string) string {
	if config.IsSelfHostedInstance(c.instanceType) {
		return TierPremium
	}

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

	tier, resolveErr := resolveTierChecked(ctx, c.db, userID)
	if resolveErr != nil {
		log.Printf("entitlements: ResolveTier failed for user %q: %v", userID, resolveErr)
		return TierFree
	}
	if setErr := c.SetTier(ctx, userID, tier); setErr != nil {
		log.Printf("entitlements: cache SetTier failed for user %q: %v", userID, setErr)
	}
	return tier
}

// GetTierChecked is GetTier for a caller that PUSHES the tier rather than
// enforcing with it (#1308's resync). It returns ("", err) when the tier could
// not be read, where GetTier returns its fail-closed TierFree: least privilege
// for one request, but a wrong set for a client to hold. A cached value and a
// database answer, including "no live row" (TierFree), are both returned with
// a nil error, and a database answer is cached as GetTier caches it. A failed
// read is returned, not logged -- the caller owns that line; a failed cache
// fill is logged as GetTier logs it, since the read itself succeeded.
func (c *Cache) GetTierChecked(ctx context.Context, userID string) (string, error) {
	if config.IsSelfHostedInstance(c.instanceType) {
		return TierPremium, nil
	}
	val, err := c.redis.Get(ctx, c.key(userID)).Result()
	if err == nil {
		return val, nil
	}
	tier, dbErr := resolveTierChecked(ctx, c.db, userID)
	if dbErr != nil {
		return "", dbErr
	}
	if errors.Is(err, redis.Nil) {
		// A miss is filled as GetTier fills it; after a Redis error the write
		// would fail the same way, so it is not attempted.
		if setErr := c.SetTier(ctx, userID, tier); setErr != nil {
			log.Printf("entitlements: cache SetTier failed for user %q: %v", userID, setErr)
		}
	}
	return tier, nil
}

// SetTier stores the tier string with the cache TTL.
func (c *Cache) SetTier(ctx context.Context, userID, tier string) error {
	return c.redis.Set(ctx, c.key(userID), tier, c.ttl).Err()
}

// Invalidate removes the cached tier (called on any subscription change).
func (c *Cache) Invalidate(ctx context.Context, userID string) error {
	return c.redis.Unlink(ctx, c.key(userID)).Err()
}
