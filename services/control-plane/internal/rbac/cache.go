package rbac

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// PermissionCache provides Redis-backed caching for computed permissions
// to avoid expensive database queries on every permission check.
// Cache TTL: 5 minutes (balance between consistency and performance)
//
// Generation tags (#3453). Every cached value carries the user generation
// (permgen:u:{userID}) and the server generation (permgen:s:{serverID}) that
// were current BEFORE the compute that produced it began. The invariant:
//
//   - a read serves a value only when both generations exist and equal the
//     value's tags, all three keys read in ONE MGET;
//   - a value is written only with tags read before its compute started;
//   - a writer that bumps does so AFTER its commit.
//
// So a bump that lands after the tag read makes the value's tags stale and it
// is never served, and a bump that lands before the tag read means the commit
// preceded the compute, which therefore saw it. For a bumping writer every
// interleaving ends in a miss or a correct value. What breaks it: reading the
// tags after the compute, serving a value without comparing its tags, bumping
// before commit, or making a generation predictable.
//
// Only two writers bump today: the MFA factor hooks (user generation) and the
// server's MFA-enforcement toggle (server generation). Role, membership and
// override writers still DELETE entries through Invalidate, InvalidateServer
// and InvalidateChannel, and so keep the pre-#3453 residual: a compute already
// in flight when they commit can republish the value they replaced, for up to
// the 5-minute TTL. Moving them onto bumps closes that too, and is a separate
// change.
//
// Generations are 64 random bits, NEVER a counter. A generation that
// disappears (TTL, eviction, Redis loss) and is re-seeded must not equal the
// one it replaced, or an entry tagged with the old value matches again; a
// counter restarts where it started. When a tag was absent at read time, Set
// seeds it with SET NX and writes no value (seed-and-skip): the compute cannot
// be pinned to a generation that did not exist when it began. A publishing Set
// refreshes both generations with EXPIRE, which cannot change a value and so
// cannot revive a replaced generation (the #3328 argument).
//
// perm: and permgen: first differ at byte 5, so the perm:-anchored SCAN
// patterns in Invalidate, InvalidateServer and InvalidateChannel never match a
// generation key.
type PermissionCache struct {
	redis *redis.Client
	ttl   time.Duration
}

const (
	// permValueVersion prefixes every value this binary writes. A pre-#3453
	// binary's strconv.ParseInt rejects "2|…", and parseCachedValue rejects a
	// bare integer, so neither binary can serve the other's entries during a
	// rolling deploy.
	permValueVersion = "2"

	// permGenerationTTL bounds how long an idle subject's generation lives.
	permGenerationTTL = 24 * time.Hour

	// permGenerationBytes is 64 bits, encoded as 16 hex characters. Only two
	// consecutive generations of one subject need to differ, so wider values
	// would spend memory on every perm:* value for nothing (spec L3).
	permGenerationBytes = 8
)

// GenTags are the generations a value is tagged with. An empty field means the
// generation was absent or unreadable.
type GenTags struct {
	User   string
	Server string
}

func (t GenTags) complete() bool {
	return t.User != "" && t.Server != ""
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
		return fmt.Sprintf("perm:%s:%s", cacheID(serverID), cacheID(userID))
	}
	return fmt.Sprintf("perm:%s:%s:%s", cacheID(serverID), cacheID(userID), cacheID(channelID))
}

func userGenerationKey(userID string) string { return "permgen:u:" + cacheID(userID) }

func serverGenerationKey(serverID string) string { return "permgen:s:" + cacheID(serverID) }

// cacheID is the form an id takes inside every key this cache builds. A uuid,
// in any spelling PostgreSQL resolves to the same row (either case, braces,
// hyphens anywhere or nowhere), becomes its canonical lowercase 8-4-4-4-12
// form; anything else is used as given.
//
// Keys must follow the database's equality, not the request's spelling. Before
// this, the key was the RAW path parameter, so a member who read through
// /servers/<UPPERCASE-ID>/... got an entry tagged with a server generation that
// a canonical bump never reaches, and after an enforcement flip kept reading
// and using the dangerous bits through that spelling until the TTL (#3453
// red-team; the canonical-only invalidation patterns had the same hole for
// role and override changes). Folding a string PostgreSQL would reject merges
// nothing a database read ever accepted, so it can never join two rows.
func cacheID(s string) string {
	var hex [32]byte
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			c += 'a' - 'A'
		case c == '-', c == '{', c == '}':
			continue
		default:
			return s
		}
		if n == len(hex) {
			return s
		}
		hex[n] = c
		n++
	}
	if n != len(hex) {
		return s
	}
	h := string(hex[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Get retrieves cached permissions with one MGET of the value and both
// generations. It is a hit only when the value parses as v2 and both
// generations exist and equal its tags. Anything else — including a Redis
// error — is a miss that returns the generations it read, which the caller
// must hand to Set so the compute is tagged with what was current before it.
func (c *PermissionCache) Get(ctx context.Context, serverID, userID, channelID string) (Permission, bool, GenTags) {
	vals, err := c.redis.MGet(ctx,
		c.cacheKey(serverID, userID, channelID), userGenerationKey(userID), serverGenerationKey(serverID),
	).Result()
	if err != nil || len(vals) != 3 {
		return 0, false, GenTags{}
	}
	current := GenTags{User: redisString(vals[1]), Server: redisString(vals[2])}
	perm, tagged, ok := parseCachedValue(redisString(vals[0]))
	if !ok || !current.complete() || tagged != current {
		return 0, false, current
	}
	return perm, true, current
}

// Generations reads both generations in one MGET, for a caller that computes
// without calling Get first (ResolveEffectivePermissionsFresh). It must run
// BEFORE that compute. A Redis error yields empty tags, so Set seeds and skips.
func (c *PermissionCache) Generations(ctx context.Context, serverID, userID string) GenTags {
	vals, err := c.redis.MGet(ctx, userGenerationKey(userID), serverGenerationKey(serverID)).Result()
	if err != nil || len(vals) != 2 {
		return GenTags{}
	}
	return GenTags{User: redisString(vals[0]), Server: redisString(vals[1])}
}

// Set publishes perm tagged with tags, which must have been read before perm
// was computed. When either tag is absent it seeds the missing generation(s)
// with SET NX and writes NO value. Otherwise it pipelines the value SET with an
// EXPIRE on both generations.
func (c *PermissionCache) Set(ctx context.Context, serverID, userID, channelID string, perm Permission, tags GenTags) error {
	if !tags.complete() {
		return c.seedGenerations(ctx, serverID, userID, tags)
	}
	value := strings.Join([]string{
		permValueVersion, strconv.FormatInt(int64(perm), 10), tags.User, tags.Server,
	}, "|")
	_, err := c.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, c.cacheKey(serverID, userID, channelID), value, c.ttl)
		pipe.Expire(ctx, userGenerationKey(userID), permGenerationTTL)
		pipe.Expire(ctx, serverGenerationKey(serverID), permGenerationTTL)
		return nil
	})
	return err
}

// seedGenerations creates each generation absent from tags, never overwriting
// one another writer created in the meantime (SET NX).
func (c *PermissionCache) seedGenerations(ctx context.Context, serverID, userID string, tags GenTags) error {
	var absent []string
	if tags.User == "" {
		absent = append(absent, userGenerationKey(userID))
	}
	if tags.Server == "" {
		absent = append(absent, serverGenerationKey(serverID))
	}
	_, err := c.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, key := range absent {
			gen, err := newGeneration()
			if err != nil {
				return err
			}
			pipe.SetNX(ctx, key, gen, permGenerationTTL)
		}
		return nil
	})
	return err
}

// BumpUser replaces the user's generation, so every entry tagged with the old
// one misses. Call it after the commit that changed the user's inputs.
func (c *PermissionCache) BumpUser(ctx context.Context, userID string) error {
	return c.bump(ctx, userGenerationKey(userID))
}

// BumpServer replaces the server's generation, so every entry tagged with the
// old one misses. Call it after the commit that changed the server's inputs.
func (c *PermissionCache) BumpServer(ctx context.Context, serverID string) error {
	return c.bump(ctx, serverGenerationKey(serverID))
}

func (c *PermissionCache) bump(ctx context.Context, key string) error {
	gen, err := newGeneration()
	if err != nil {
		return err
	}
	return c.redis.Set(ctx, key, gen, permGenerationTTL).Err()
}

// InvalidateUser deletes every cached value for userID across all servers:
// perm:*:{userID} and perm:*:{userID}:*. It is the fallback for a failed
// BumpUser. A pattern can over-match only keys whose ids collide, which costs
// a recompute and nothing else.
func (c *PermissionCache) InvalidateUser(ctx context.Context, userID string) error {
	userID = cacheID(userID)
	return c.scanAndUnlink(ctx, "perm:*:"+userID, "perm:*:"+userID+":*")
}

// scanAndUnlink deletes every key matching any of patterns, in one UNLINK
// round trip once every pattern has been scanned.
func (c *PermissionCache) scanAndUnlink(ctx context.Context, patterns ...string) error {
	var keys []string
	for _, pattern := range patterns {
		iter := c.redis.Scan(ctx, 0, pattern, 100).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			return err
		}
	}
	if len(keys) > 0 {
		return c.redis.Unlink(ctx, keys...).Err()
	}
	return nil
}

func newGeneration() (string, error) {
	var b [permGenerationBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate permission generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// parseCachedValue accepts exactly "2|<int64>|<ugen>|<sgen>". A bare integer
// (the pre-#3453 format) and anything else is rejected.
func parseCachedValue(s string) (Permission, GenTags, bool) {
	parts := strings.Split(s, "|")
	if len(parts) != 4 || parts[0] != permValueVersion {
		return 0, GenTags{}, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, GenTags{}, false
	}
	return Permission(n), GenTags{User: parts[2], Server: parts[3]}, true
}

// redisString returns an MGET element as a string, "" for a missing key.
func redisString(v any) string {
	s, _ := v.(string)
	return s
}

// Invalidate removes cached permissions for a user (called after role changes)
func (c *PermissionCache) Invalidate(ctx context.Context, serverID, userID string) error {
	// Delete server-level key directly
	serverKey := c.cacheKey(serverID, userID, "")

	// SCAN for channel-level keys: perm:{serverID}:{userID}:{channelID}
	pattern := fmt.Sprintf("perm:%s:%s:*", cacheID(serverID), cacheID(userID))
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
	return c.scanAndUnlink(ctx, fmt.Sprintf("perm:%s:*", cacheID(serverID)))
}

// InvalidateChannel removes cached permissions for a channel (called after channel permission overrides change)
func (c *PermissionCache) InvalidateChannel(ctx context.Context, serverID, channelID string) error {
	return c.scanAndUnlink(ctx, fmt.Sprintf("perm:%s:*:%s", cacheID(serverID), cacheID(channelID)))
}
