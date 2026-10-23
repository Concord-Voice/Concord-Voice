package entitlements_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNotifier struct {
	disconnected []uuid.UUID
	broadcasts   []entitlements.EntitlementDTO
}

func (f *fakeNotifier) DisconnectUserContext(ctx context.Context, id uuid.UUID) bool {
	if ctx.Err() != nil {
		return false
	}
	f.disconnected = append(f.disconnected, id)
	return true
}
func (f *fakeNotifier) BroadcastEntitlementsContext(ctx context.Context, _ uuid.UUID, dto entitlements.EntitlementDTO) bool {
	if ctx.Err() != nil {
		return false
	}
	f.broadcasts = append(f.broadcasts, dto)
	return true
}

func TestIsDowngrade(t *testing.T) {
	assert.True(t, entitlements.IsDowngrade("premium", "free"))
	assert.True(t, entitlements.IsDowngrade("premium", "garbage")) // unknown ranks 0 -> downgrade (fail-closed)
	assert.False(t, entitlements.IsDowngrade("free", "premium"))
	assert.False(t, entitlements.IsDowngrade("premium", "premium"))
	assert.False(t, entitlements.IsDowngrade("free", "free"))
}

func TestOnTierChange_DowngradeInvalidatesAndDisconnects(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()
	require.NoError(t, cache.SetTier(ctx, uid, "premium"))

	disc := &fakeNotifier{}
	require.NoError(t, entitlements.OnTierChange(ctx, cache, disc, uuid.MustParse(uid), "premium", "free"))

	// cache invalidated
	_, err := ts.Redis.Get(ctx, "ent:"+uid).Result()
	assert.ErrorIs(t, err, redis.Nil)
	// disconnect fired
	require.Len(t, disc.disconnected, 1)
	assert.Equal(t, uid, disc.disconnected[0].String())
	// entitlements_changed pushed with the NEW (free) set
	require.Len(t, disc.broadcasts, 1)
	assert.Equal(t, "free", disc.broadcasts[0].Tier)
}

func TestOnTierChange_UpgradeInvalidatesNoDisconnect(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()
	require.NoError(t, cache.SetTier(ctx, uid, "free"))

	disc := &fakeNotifier{}
	require.NoError(t, entitlements.OnTierChange(ctx, cache, disc, uuid.MustParse(uid), "free", "premium"))

	assert.Empty(t, disc.disconnected, "upgrade must not disconnect")
}

func TestOnTierChange_PushesNewEntitlementsOnEveryChange(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	ctx := context.Background()
	require.NoError(t, cache.SetTier(ctx, uid, "free"))

	n := &fakeNotifier{}
	require.NoError(t, entitlements.OnTierChange(ctx, cache, n, uuid.MustParse(uid), "free", "premium"))

	require.Len(t, n.broadcasts, 1, "every change pushes entitlements_changed")
	assert.Equal(t, "premium", n.broadcasts[0].Tier)
	assert.Empty(t, n.disconnected, "upgrade must not disconnect")
}

func TestOnTierChange_NilDisconnectorTolerated(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	cache := entitlements.NewCache(ts.Redis, ts.DB)
	require.NoError(t, entitlements.OnTierChange(context.Background(), cache, nil, uuid.MustParse(uid), "premium", "free"))
}

// Regression lock (Gitar finding, PR #1681): a downgrade MUST still force the
// eager disconnect even when cache invalidation fails (e.g. Redis down) — the
// fail-closed revocation priority. The invalidate error is surfaced, but the
// disconnect is NOT skipped.
func TestOnTierChange_DowngradeDisconnectsEvenIfInvalidateFails(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	uid := insertUser(t, ts)
	// Unreachable Redis → Invalidate errors; DB stays healthy.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = down.Close() }()
	cache := entitlements.NewCache(down, ts.DB)

	disc := &fakeNotifier{}
	err := entitlements.OnTierChange(context.Background(), cache, disc, uuid.MustParse(uid), "premium", "free")

	require.Error(t, err, "the invalidate error must still be surfaced")
	require.Len(t, disc.disconnected, 1, "downgrade disconnect must fire despite the invalidate error")
	assert.Equal(t, uid, disc.disconnected[0].String())
	// BroadcastEntitlements does not touch Redis, so the push still records.
	require.Len(t, disc.broadcasts, 1)
	assert.Equal(t, "free", disc.broadcasts[0].Tier)
}

func TestOnTierChange_CacheTimeoutLeavesDowngradeQueueBudget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	release := make(chan struct{})
	joined := make(chan struct{})
	t.Cleanup(func() {
		require.NoError(t, listener.Close())
		close(release)
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("stalled Redis fixture did not exit")
		}
	})
	go func() {
		defer close(joined)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		<-release
		if err := conn.Close(); err != nil {
			t.Errorf("close stalled Redis connection: %v", err)
		}
	}()
	client := redis.NewClient(&redis.Options{Addr: listener.Addr().String(), ContextTimeoutEnabled: true, ReadTimeout: -1, MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	notifier := &fakeNotifier{}
	err = entitlements.OnTierChange(ctx, entitlements.NewCache(client, nil), notifier, uuid.New(), "premium", "free")
	require.Error(t, err, "stalled cache invalidation must be surfaced")
	require.NoError(t, ctx.Err(), "invalidation must leave time for downgrade enforcement")
	assert.Len(t, notifier.disconnected, 1, "cache timeout must not consume the downgrade's queue budget")
	assert.Len(t, notifier.broadcasts, 1)
}
