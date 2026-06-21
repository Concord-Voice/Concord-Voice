package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// ServerCache is a Redis-backed, read-through cache of a server's entitlement
// tier. It mirrors Cache (the user axis): one key per server
// (ent:server:{serverID}), the shared 5-min cacheTTL, and Unlink invalidation.
// GetServerTier never errors — every failure path fails closed to
// TierGroundspeed (least privilege), so a Redis/DB hiccup can never escalate a
// server to Mach.
type ServerCache struct {
	redis *redis.Client
	db    *sql.DB
	ttl   time.Duration
}

// NewServerCache builds the server-tier cache from the existing Redis + DB handles.
func NewServerCache(redisClient *redis.Client, db *sql.DB) *ServerCache {
	return &ServerCache{redis: redisClient, db: db, ttl: cacheTTL}
}

func (c *ServerCache) key(serverID string) string { return "ent:server:" + serverID }

// GetServerTier returns the server's tier, reading through to ResolveServerTier
// on a cache miss and populating the cache. On a Redis error (other than a miss)
// it degrades to a direct resolve rather than failing open. Today the resolver is
// the inert Groundspeed hook, so this returns TierGroundspeed for every server.
func (c *ServerCache) GetServerTier(ctx context.Context, serverID string) string {
	val, err := c.redis.Get(ctx, c.key(serverID)).Result()
	switch {
	case err == nil:
		return val
	case errors.Is(err, redis.Nil):
		// genuine cache miss — continue past the switch to the read-through below
	default:
		log.Printf("entitlements: server cache GetServerTier redis error for server %q: %v", serverID, err)
		return ResolveServerTier(ctx, c.db, serverID)
	}

	tier := ResolveServerTier(ctx, c.db, serverID)
	if setErr := c.SetServerTier(ctx, serverID, tier); setErr != nil {
		log.Printf("entitlements: server cache SetServerTier failed for server %q: %v", serverID, setErr)
	}
	return tier
}

// SetServerTier stores the tier string with the cache TTL.
func (c *ServerCache) SetServerTier(ctx context.Context, serverID, tier string) error {
	return c.redis.Set(ctx, c.key(serverID), tier, c.ttl).Err()
}

// Invalidate removes the cached tier (called on any server-subscription change).
func (c *ServerCache) Invalidate(ctx context.Context, serverID string) error {
	return c.redis.Unlink(ctx, c.key(serverID)).Err()
}
