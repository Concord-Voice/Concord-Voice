package entitlements_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register the postgres driver for the closed-handle DB error case
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLogOutput is where the standard logger writes by default.
var testLogOutput = os.Stderr

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

func TestCache_GetTier_SelfHostedReturnsPremiumWithoutDBOrRedis(t *testing.T) {
	cache := entitlements.NewCacheForInstance(nil, nil, " self-hosted ")

	assert.Equal(t, entitlements.TierPremium, cache.GetTier(context.Background(), "user-1"))
}

// TestCache_IsSelfHosted reports the deployment-mode seam that callers resolving
// a tier outside GetTier (e.g. the expiry sweeper) key off. It normalizes the
// operator-authored instance-type string just like GetTier's own branch.
func TestCache_IsSelfHosted(t *testing.T) {
	assert.True(t, entitlements.NewCacheForInstance(nil, nil, " self-hosted ").IsSelfHosted(),
		"self-hosted instance type must report self-hosted (whitespace-tolerant)")
	assert.False(t, entitlements.NewCacheForInstance(nil, nil, "saas").IsSelfHosted(),
		"saas instance type must not report self-hosted")
	assert.False(t, entitlements.NewCache(nil, nil).IsSelfHosted(),
		"default cache is SaaS")
}

// GetTierChecked is what the threshold resync reads: it must tell "the user is
// free" (an answer) from "the read failed" (no answer), which GetTier cannot.

func TestCache_GetTierChecked_HitReturnsCachedWithNilError(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts) // no subscription row -> the DB would say free
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()
	require.NoError(t, cache.SetTier(ctx, uid, "premium"))

	tier, err := cache.GetTierChecked(ctx, uid)

	require.NoError(t, err)
	assert.Equal(t, "premium", tier, "the cache is honored, not the database")
}

func TestCache_GetTierChecked_MissReadsThroughAndPopulates(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()

	tier, err := cache.GetTierChecked(ctx, uid)

	require.NoError(t, err)
	assert.Equal(t, "premium", tier)
	cached, getErr := ts.Redis.Get(ctx, "ent:"+uid).Result()
	require.NoError(t, getErr, "a database answer is cached")
	assert.Equal(t, "premium", cached)
	ttl, ttlErr := ts.Redis.TTL(ctx, "ent:"+uid).Result()
	require.NoError(t, ttlErr)
	assert.True(t, ttl > 0 && ttl <= 5*time.Minute, "populated key carries the 5-min TTL")
}

func TestCache_GetTierChecked_NoRowIsFreeNotAnError(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	cache := entitlements.NewCache(ts.Redis, ts.DB)

	tier, err := cache.GetTierChecked(context.Background(), uid)

	require.NoError(t, err, "no live subscription is an answer")
	assert.Equal(t, entitlements.TierFree, tier)
}

func TestCache_GetTierChecked_DBErrorIsAnErrorNotFree(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	db, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, db.Close(), "a closed handle fails every query")
	cache := entitlements.NewCache(ts.Redis, db)
	uid := uuid.New().String()

	tier, getErr := cache.GetTierChecked(context.Background(), uid)

	require.Error(t, getErr)
	assert.Empty(t, tier, "a failed read carries no tier")
	exists, existsErr := ts.Redis.Exists(context.Background(), "ent:"+uid).Result()
	require.NoError(t, existsErr)
	assert.Zero(t, exists, "a failed read must not seed the cache")
}

func TestCache_GetTierChecked_RedisDownResolvesFromDBWithoutCaching(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = down.Close() }()
	cache := entitlements.NewCache(down, ts.DB)

	tier, err := cache.GetTierChecked(context.Background(), uid)

	require.NoError(t, err, "a Redis outage with a healthy database is still an answer")
	assert.Equal(t, "premium", tier)
}

func TestCache_GetTierChecked_SelfHostedReturnsPremiumWithoutDBOrRedis(t *testing.T) {
	cache := entitlements.NewCacheForInstance(nil, nil, " self-hosted ")

	tier, err := cache.GetTierChecked(context.Background(), "user-1")

	require.NoError(t, err)
	assert.Equal(t, entitlements.TierPremium, tier)
}

// A database failure under GetTier is the free fallback for THAT call and is
// never cached. Cached, it would hold a paying user to free for the whole TTL
// after the database recovers -- and GetTierChecked, reading the key back as a
// hit, would hand the resync pusher that fallback as if it were an answer.
func TestCache_GetTier_DBErrorReturnsFreeAndIsNotCached(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	db, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, db.Close(), "a closed handle fails every query")
	cache := entitlements.NewCache(ts.Redis, db)
	uid := uuid.New().String()
	ctx := context.Background()

	assert.Equal(t, entitlements.TierFree, cache.GetTier(ctx, uid), "fail closed for this request")

	exists, existsErr := ts.Redis.Exists(ctx, "ent:"+uid).Result()
	require.NoError(t, existsErr)
	assert.Zero(t, exists, "the fallback is never cached")

	tier, checkedErr := cache.GetTierChecked(ctx, uid)
	require.Error(t, checkedErr, "the fallback must not be readable back as an answer")
	assert.Empty(t, tier)
}

// failSetHook fails every SET on the one client it is attached to, leaving
// reads and every other client alone.
type failSetHook struct{ err error }

func (failSetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h failSetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" {
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (failSetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// The read succeeded; only the cache fill failed. That is an answer, not an
// error: the live tier is returned with a nil error, and the failed fill is
// logged (not silently dropped) as GetTier logs it.
func TestCache_GetTierChecked_FailedCacheFillStillReturnsTheLiveTier(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	insertSub(t, ts, uid, "premium", "active", nil)
	opts := *ts.Redis.Options()
	client := redis.NewClient(&opts)
	defer func() { _ = client.Close() }()
	client.AddHook(failSetHook{err: errors.New("injected set failure")})
	cache := entitlements.NewCache(client, ts.DB)

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(testLogOutput) })

	tier, err := cache.GetTierChecked(context.Background(), uid)

	require.NoError(t, err, "a failed cache fill does not fail a successful read")
	assert.Equal(t, "premium", tier)
	assert.True(t, strings.Contains(logged.String(), "cache SetTier failed"), "the failed fill is logged: %q", logged.String())
	exists, existsErr := ts.Redis.Exists(context.Background(), "ent:"+uid).Result()
	require.NoError(t, existsErr)
	assert.Zero(t, exists, "and nothing was cached")
}
