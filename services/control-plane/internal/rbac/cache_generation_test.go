package rbac_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Generation-tagged permission cache tests, #3453 spec §5 and §13 I-3/I-4.

var generationShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

func userGenKey(userID string) string     { return "permgen:u:" + userID }
func serverGenKey(serverID string) string { return "permgen:s:" + serverID }

// I-4: an absent generation is a miss, Set seeds it and writes NO value.
// Kills: Set publishing with an absent tag (the value appears).
func TestPermissionCache_AbsentGenerationSeedsAndSkips(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()

	_, ok, tags := cache.Get(ctx, "s-absent", "u-absent", "")
	require.False(t, ok)
	require.Equal(t, rbac.GenTags{}, tags)

	require.NoError(t, cache.Set(ctx, "s-absent", "u-absent", "", rbac.PermKick, tags))
	exists, err := ts.Redis.Exists(ctx, "perm:s-absent:u-absent").Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "a value computed without both generations must not be written")

	for _, key := range []string{userGenKey("u-absent"), serverGenKey("s-absent")} {
		gen, err := ts.Redis.Get(ctx, key).Result()
		require.NoError(t, err, key)
		assert.Regexp(t, generationShape, gen, "%s must be 64 random bits as 16 hex characters", key)
		ttl, err := ts.Redis.TTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, 23*time.Hour, key)
		assert.LessOrEqual(t, ttl, 24*time.Hour, key)
	}

	// The next miss carries both tags, and its Set publishes.
	_, ok, tags = cache.Get(ctx, "s-absent", "u-absent", "")
	require.False(t, ok)
	require.NotEmpty(t, tags.User)
	require.NotEmpty(t, tags.Server)
	require.NoError(t, cache.Set(ctx, "s-absent", "u-absent", "", rbac.PermKick, tags))
	got, ok, _ := cache.Get(ctx, "s-absent", "u-absent", "")
	require.True(t, ok)
	assert.Equal(t, rbac.PermKick, got)
	raw, err := ts.Redis.Get(ctx, "perm:s-absent:u-absent").Result()
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("2|%d|%s|%s", int64(rbac.PermKick), tags.User, tags.Server), raw)
}

// Seeding never overwrites a generation another writer created (SET NX).
func TestPermissionCache_SeedDoesNotOverwriteAPresentGeneration(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()

	require.NoError(t, ts.Redis.Set(ctx, userGenKey("u-nx"), "00000000000000aa", time.Hour).Err())
	require.NoError(t, cache.Set(ctx, "s-nx", "u-nx", "", 0, rbac.GenTags{}))
	gen, err := ts.Redis.Get(ctx, userGenKey("u-nx")).Result()
	require.NoError(t, err)
	assert.Equal(t, "00000000000000aa", gen)
	server, err := ts.Redis.Get(ctx, serverGenKey("s-nx")).Result()
	require.NoError(t, err)
	assert.Regexp(t, generationShape, server)
}

// I-4: a generation that is evicted and re-seeded never matches an entry
// tagged with the one it replaced. Kills: a counter instead of a random value
// (a counter restarts where it started, and the old entry matches again).
func TestPermissionCache_ReseededGenerationNeverMatchesOldEntry(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()

	testhelpers.PublishPermissionCache(t, ts.Redis, "s-evict", "u-evict", "", rbac.PermBan)
	before, err := ts.Redis.Get(ctx, userGenKey("u-evict")).Result()
	require.NoError(t, err)

	for round := 0; round < 3; round++ {
		require.NoError(t, ts.Redis.Del(ctx, userGenKey("u-evict")).Err(), "evict the user generation")
		_, ok, tags := cache.Get(ctx, "s-evict", "u-evict", "")
		require.False(t, ok, "an absent generation is a miss")
		require.Empty(t, tags.User)
		require.NoError(t, cache.Set(ctx, "s-evict", "u-evict", "", rbac.PermBan, tags), "re-seed")

		after, err := ts.Redis.Get(ctx, userGenKey("u-evict")).Result()
		require.NoError(t, err)
		require.NotEqual(t, before, after, "round %d: a re-seeded generation repeated the evicted one", round)
		_, ok, _ = cache.Get(ctx, "s-evict", "u-evict", "")
		assert.False(t, ok, "round %d: the entry tagged with the evicted generation must never be served", round)
	}
}

// A bump makes every entry of the subject miss, on every server for a user.
func TestPermissionCache_BumpMakesEntriesMiss(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()

	testhelpers.PublishPermissionCache(t, ts.Redis, "s-bump1", "u-bump", "", rbac.PermKick)
	testhelpers.PublishPermissionCache(t, ts.Redis, "s-bump2", "u-bump", "c1", rbac.PermKick)
	testhelpers.PublishPermissionCache(t, ts.Redis, "s-bump1", "u-other", "", rbac.PermKick)

	require.NoError(t, cache.BumpUser(ctx, "u-bump"))
	for _, k := range [][3]string{{"s-bump1", "u-bump", ""}, {"s-bump2", "u-bump", "c1"}} {
		_, ok, _ := cache.Get(ctx, k[0], k[1], k[2])
		assert.False(t, ok, "%v must miss after a user bump", k)
	}
	_, ok, _ := cache.Get(ctx, "s-bump1", "u-other", "")
	assert.True(t, ok, "another user's entry is untouched by a user bump")

	require.NoError(t, cache.BumpServer(ctx, "s-bump1"))
	_, ok, _ = cache.Get(ctx, "s-bump1", "u-other", "")
	assert.False(t, ok, "a server bump reaches every member's entry")
}

// I-4: a bare integer (the pre-#3453 format) and every malformed v2 shape miss.
func TestPermissionCache_UnrecognizedValueIsAMiss(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()
	tags := testhelpers.SeedPermissionGenerations(t, ts.Redis, "s-fmt", "u-fmt")

	for _, value := range []string{
		"12345",
		"2|12345",
		"2|12345|" + tags.User,
		"2|x|" + tags.User + "|" + tags.Server,
		"3|12345|" + tags.User + "|" + tags.Server,
		"2|12345|" + tags.User + "|" + tags.Server + "|extra",
		"2|12345||",
	} {
		require.NoError(t, ts.Redis.Set(ctx, "perm:s-fmt:u-fmt", value, time.Minute).Err())
		_, ok, got := cache.Get(ctx, "s-fmt", "u-fmt", "")
		assert.False(t, ok, "value %q must miss", value)
		assert.Equal(t, tags, got, "a miss still returns the generations it read")
	}
	require.NoError(t, ts.Redis.Set(ctx, "perm:s-fmt:u-fmt", "2|12345|"+tags.User+"|"+tags.Server, time.Minute).Err())
	got, ok, _ := cache.Get(ctx, "s-fmt", "u-fmt", "")
	require.True(t, ok, "control: the well-formed value is served")
	assert.Equal(t, rbac.Permission(12345), got)
}

// A publishing Set refreshes both generations' TTL; EXPIRE cannot change them.
func TestPermissionCache_PublishRefreshesGenerationTTL(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()
	tags := testhelpers.SeedPermissionGenerations(t, ts.Redis, "s-ttl", "u-ttl")
	for _, key := range []string{userGenKey("u-ttl"), serverGenKey("s-ttl")} {
		require.NoError(t, ts.Redis.Expire(ctx, key, time.Minute).Err())
	}

	require.NoError(t, cache.Set(ctx, "s-ttl", "u-ttl", "", rbac.PermKick, tags))
	for _, key := range []string{userGenKey("u-ttl"), serverGenKey("s-ttl")} {
		ttl, err := ts.Redis.TTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, 23*time.Hour, key)
	}
	assert.Equal(t, tags, cache.Generations(ctx, "s-ttl", "u-ttl"), "EXPIRE must not change a generation")
}

// The perm:-anchored SCAN patterns never reach a generation key, and
// InvalidateUser reaches the user's entries on every server and nobody else's.
func TestPermissionCache_InvalidationLeavesGenerationsAlone(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()

	testhelpers.PublishPermissionCache(t, ts.Redis, "s-inv1", "u-inv", "", rbac.PermKick)
	testhelpers.PublishPermissionCache(t, ts.Redis, "s-inv1", "u-inv", "c-inv", rbac.PermKick)
	testhelpers.PublishPermissionCache(t, ts.Redis, "s-inv2", "u-inv", "", rbac.PermKick)
	testhelpers.PublishPermissionCache(t, ts.Redis, "s-inv1", "u-keep", "", rbac.PermKick)
	generations := []string{userGenKey("u-inv"), userGenKey("u-keep"), serverGenKey("s-inv1"), serverGenKey("s-inv2")}

	require.NoError(t, cache.InvalidateUser(ctx, "u-inv"))
	for _, k := range [][3]string{{"s-inv1", "u-inv", ""}, {"s-inv1", "u-inv", "c-inv"}, {"s-inv2", "u-inv", ""}} {
		_, ok, _ := cache.Get(ctx, k[0], k[1], k[2])
		assert.False(t, ok, "%v must be gone after InvalidateUser", k)
	}
	_, ok, _ := cache.Get(ctx, "s-inv1", "u-keep", "")
	assert.True(t, ok, "another user's entry survives InvalidateUser")

	require.NoError(t, cache.Invalidate(ctx, "s-inv1", "u-keep"))
	require.NoError(t, cache.InvalidateChannel(ctx, "s-inv1", "c-inv"))
	require.NoError(t, cache.InvalidateServer(ctx, "s-inv1"))
	require.NoError(t, cache.InvalidateServer(ctx, "s-inv2"))
	n, err := ts.Redis.Exists(ctx, generations...).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(len(generations)), n, "no invalidation may delete a generation key")
}

// enforcingModerator builds an enforcing server with an ENROLLED moderator.
func enforcingModerator(t *testing.T, ts *testhelpers.TestServer) (serverID, userID string) {
	t.Helper()
	owner := ts.CreateTestUser(t, "raceowner")
	mod := ts.CreateTestUser(t, "racemod")
	serverID = ts.CreateTestServer(t, owner.ID, "MFA Race")
	ts.AddMemberToServer(t, serverID, mod.ID, "member")
	role := ts.CreateTestRole(t, serverID, "Moderator", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, mod.ID, role)
	enrollWebAuthn(t, ts, mod.ID)
	enforceMFA(t, ts, serverID)
	return serverID, mod.ID
}

// removeFactorOnce returns a seam that, the first time a compute returns,
// commits the member's factor removal and then bumps their generation — the
// order the MFA hooks use (#3453 §6).
func removeFactorOnce(t *testing.T, ts *testhelpers.TestServer, r *rbac.Resolver, userID string) *bool {
	t.Helper()
	fired := false
	rbac.SetAfterComputeForTest(r, func() {
		if fired {
			return
		}
		fired = true
		_, err := ts.DB.Exec(`DELETE FROM user_mfa_webauthn WHERE user_id = $1`, userID)
		require.NoError(t, err)
		require.NoError(t, r.BumpUserPermissionGeneration(context.Background(), userID))
	})
	return &fired
}

// I-3: a compute in flight when a factor removal commits and bumps is never
// served afterwards. Kills: the tag read moved after the compute (the stale
// value is published under the bumped generation), and a hit accepted without
// comparing tags (the stale value is served).
func TestPermissionCache_InFlightFactorRemovalIsNotServed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	ctx := context.Background()
	serverID, modID := enforcingModerator(t, ts)
	testhelpers.SeedPermissionGenerations(t, ts.Redis, serverID, modID)
	fired := removeFactorOnce(t, ts, r, modID)

	kick, err := r.HasPermission(ctx, serverID, modID, "", rbac.PermKick)
	require.NoError(t, err)
	require.True(t, *fired, "the seam must have run")
	require.True(t, kick, "the in-flight compute ran while the member was still enrolled")
	exists, err := ts.Redis.Exists(ctx, "perm:"+serverID+":"+modID).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "the in-flight compute must have published, or the race is not exercised")

	kick, err = r.HasPermission(ctx, serverID, modID, "", rbac.PermKick)
	require.NoError(t, err)
	assert.False(t, kick, "the next read after a committed removal and bump must be masked")
	perms, err := r.GetEffectivePermissions(ctx, serverID, modID, "")
	require.NoError(t, err)
	assert.Equal(t, rbac.Permission(0x3cffbe00), perms)
}

// I-4: ResolveEffectivePermissionsFresh publishes only with the generations it
// read BEFORE its compute. Kills: Fresh reading them after the compute.
func TestResolveEffectivePermissionsFresh_TagsReadBeforeCompute(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	ctx := context.Background()
	serverID, modID := enforcingModerator(t, ts)
	testhelpers.SeedPermissionGenerations(t, ts.Redis, serverID, modID)
	fired := removeFactorOnce(t, ts, r, modID)

	fresh, err := r.ResolveEffectivePermissionsFresh(ctx, serverID, modID, "")
	require.NoError(t, err)
	require.True(t, *fired)
	require.Equal(t, rbac.Permission(0x3cfffe80), fresh, "computed while still enrolled")
	exists, err := ts.Redis.Exists(ctx, "perm:"+serverID+":"+modID).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "Fresh must have published, or the race is not exercised")

	got, err := r.GetEffectivePermissions(ctx, serverID, modID, "")
	require.NoError(t, err)
	assert.Equal(t, rbac.Permission(0x3cffbe00), got, "the pre-removal value must not be served")
}

// failingRedis injects failures into a private client on the test's own
// Redis database, so the resolver's bump retry and fallback are observable.
type failingRedis struct {
	mu          sync.Mutex
	failSets    int
	failScans   bool
	setAttempts int
	scans       int
}

var errInjected = errors.New("injected redis failure")

func (h *failingRedis) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failingRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failingRedis) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		fail := false
		args := cmd.Args()
		switch {
		case cmd.Name() == "set" && len(args) > 1 && strings.HasPrefix(fmt.Sprint(args[1]), "permgen:"):
			h.setAttempts++
			fail = h.setAttempts <= h.failSets
		case cmd.Name() == "scan":
			h.scans++
			fail = h.failScans
		}
		h.mu.Unlock()
		if fail {
			cmd.SetErr(errInjected)
			return errInjected
		}
		return next(ctx, cmd)
	}
}

func hookedResolver(t *testing.T, ts *testhelpers.TestServer, hook *failingRedis) *rbac.Resolver {
	t.Helper()
	client := redis.NewClient(redistest.Options(t))
	t.Cleanup(func() { _ = client.Close() })
	client.AddHook(hook)
	return rbac.NewResolver(ts.DB, rbac.NewPermissionCache(client), logger.New("test"))
}

// I-4: a failing bump is retried once, then falls back to deleting entries,
// and reports an error only when every step fails. Kills: no retry, no
// fallback, and a total failure swallowed.
func TestResolver_BumpRetriesThenFallsBack(t *testing.T) {
	ctx := context.Background()
	plant := func(t *testing.T, ts *testhelpers.TestServer) {
		testhelpers.PublishPermissionCache(t, ts.Redis, "s-fb1", "u-fb", "", rbac.PermKick)
		testhelpers.PublishPermissionCache(t, ts.Redis, "s-fb2", "u-fb", "c-fb", rbac.PermKick)
		testhelpers.PublishPermissionCache(t, ts.Redis, "s-fb1", "u-bystander", "", rbac.PermKick)
	}
	stored := func(t *testing.T, ts *testhelpers.TestServer, keys ...string) int64 {
		n, err := ts.Redis.Exists(ctx, keys...).Result()
		require.NoError(t, err)
		return n
	}
	userEntries := []string{"perm:s-fb1:u-fb", "perm:s-fb2:u-fb:c-fb"}

	t.Run("user: retry succeeds, no fallback", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		plant(t, ts)
		before, err := ts.Redis.Get(ctx, userGenKey("u-fb")).Result()
		require.NoError(t, err)
		hook := &failingRedis{failSets: 1}
		require.NoError(t, hookedResolver(t, ts, hook).BumpUserPermissionGeneration(ctx, "u-fb"))
		assert.Equal(t, 2, hook.setAttempts, "one failure, one retry")
		assert.Zero(t, hook.scans, "a successful retry needs no fallback")
		after, err := ts.Redis.Get(ctx, userGenKey("u-fb")).Result()
		require.NoError(t, err)
		assert.NotEqual(t, before, after)
	})

	t.Run("user: both bumps fail, fallback deletes the user's entries", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		plant(t, ts)
		hook := &failingRedis{failSets: 2}
		require.NoError(t, hookedResolver(t, ts, hook).BumpUserPermissionGeneration(ctx, "u-fb"))
		assert.Equal(t, 2, hook.setAttempts)
		assert.Positive(t, hook.scans)
		assert.Zero(t, stored(t, ts, userEntries...), "the fallback must delete every entry of the user")
		assert.Equal(t, int64(1), stored(t, ts, "perm:s-fb1:u-bystander"), "and nobody else's")
	})

	t.Run("user: bump and fallback fail, error returned", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		plant(t, ts)
		hook := &failingRedis{failSets: 2, failScans: true}
		err := hookedResolver(t, ts, hook).BumpUserPermissionGeneration(ctx, "u-fb")
		require.ErrorIs(t, err, errInjected)
		assert.Equal(t, 2, hook.setAttempts)
	})

	t.Run("server: both bumps fail, fallback deletes the server's entries", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		plant(t, ts)
		hook := &failingRedis{failSets: 2}
		require.NoError(t, hookedResolver(t, ts, hook).BumpServerPermissionGeneration(ctx, "s-fb1"))
		assert.Equal(t, 2, hook.setAttempts)
		assert.Zero(t, stored(t, ts, "perm:s-fb1:u-fb", "perm:s-fb1:u-bystander"))
		assert.Equal(t, int64(1), stored(t, ts, "perm:s-fb2:u-fb:c-fb"), "another server is untouched")
	})

	t.Run("server: bump and fallback fail, error returned", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		plant(t, ts)
		hook := &failingRedis{failSets: 2, failScans: true}
		err := hookedResolver(t, ts, hook).BumpServerPermissionGeneration(ctx, "s-fb1")
		require.ErrorIs(t, err, errInjected)
	})

	t.Run("a cancelled caller context still bumps", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		plant(t, ts)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		hook := &failingRedis{}
		require.NoError(t, hookedResolver(t, ts, hook).BumpUserPermissionGeneration(cancelled, "u-fb"))
		_, ok, _ := rbac.NewPermissionCache(ts.Redis).Get(ctx, "s-fb1", "u-fb", "")
		assert.False(t, ok, "a post-commit bump must not be abandoned by the request's cancellation")
	})
}

// A resolver built without a cache has nothing that can be stale.
func TestResolver_BumpWithoutCacheIsANoOp(t *testing.T) {
	r := rbac.NewResolver(nil, nil, nil)
	assert.NoError(t, r.BumpUserPermissionGeneration(context.Background(), "u"))
	assert.NoError(t, r.BumpServerPermissionGeneration(context.Background(), "s"))
}
