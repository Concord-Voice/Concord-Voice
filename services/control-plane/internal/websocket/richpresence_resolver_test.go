package websocket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// This file uses the package-local setupHubTestRedis (hub_epoch_test.go) rather
// than internal/testhelpers: testhelpers imports this package, so a
// `package websocket` test importing it forms an import cycle.

// newUnreachableRedisClient returns a client whose every command errors, standing
// in for a transport failure without needing to break the shared test Redis.
func newUnreachableRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:       "127.0.0.1:1", // nothing listens here
		MaxRetries: -1,
	})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

func TestSenderPresenceResolver_Mapping(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)

	for _, tc := range []struct {
		name      string
		stored    string // "" means the key is never written (offline)
		permitted bool
	}{
		{"online emits", statusOnline, true},
		{"dnd emits", statusDND, true},
		{"invisible suppresses", statusInvisible, false},
		{"missing key suppresses", "", false},
		{"unknown value suppresses", "bogus", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			senderID := uuid.New()
			if tc.stored != "" {
				require.NoError(t, rdb.Set(
					context.Background(), presence.StatusRedisKey(senderID), tc.stored, 0,
				).Err())
			}

			resolver := NewSenderPresenceResolver(rdb, db)
			require.Equal(t, tc.permitted,
				resolver.RichPresenceEmissionPermitted(context.Background(), senderID))
		})
	}
}

func TestSenderPresenceResolver_TransportErrorSuppresses(t *testing.T) {
	resolver := NewSenderPresenceResolver(newUnreachableRedisClient(t), nil)

	require.False(t,
		resolver.RichPresenceEmissionPermitted(context.Background(), uuid.New()),
		"a transport error must fail closed, never emit")
}

func TestSenderPresenceResolver_NilClientSuppresses(t *testing.T) {
	resolver := NewSenderPresenceResolver(nil, nil)

	require.False(t,
		resolver.RichPresenceEmissionPermitted(context.Background(), uuid.New()),
		"an unwired resolver must fail closed")
}

// The resolver reads live on EVERY call, with no cache in front of it.
//
// A ctx-scoped MGET cache used to seed verdicts for a whole bootstrap fan-out. It
// was removed because seeding a PERMIT is a TOCTOU (CWE-367): a sender who flipped
// to invisible after the MGET but before the authorization loop reached their
// candidate was still published from the stale allow. Caching only suppressions
// would have been safe but near-useless, since suppressed senders are the minority.
// One Redis GET per sender is a few hundred microseconds; do not reintroduce a
// permit cache to save it (#2444).
func TestSenderPresenceResolver_ReadsLiveOnEveryCall(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	senderID := uuid.New()
	resolver := NewSenderPresenceResolver(rdb, db)
	ctx := context.Background()

	require.NoError(t, rdb.Set(ctx, presence.StatusRedisKey(senderID), statusOnline, 0).Err())
	require.True(t, resolver.RichPresenceEmissionPermitted(ctx, senderID))

	// Flip mid-flight, exactly as a sender going invisible during a bootstrap would.
	require.NoError(t, rdb.Set(ctx, presence.StatusRedisKey(senderID), statusInvisible, 0).Err())
	require.False(t, resolver.RichPresenceEmissionPermitted(ctx, senderID),
		"a permit must never be cached; the next authorization must observe the flip")
}

func TestSenderPresenceResolver_OfflineFenceSuppressesStaleVisibleStatus(t *testing.T) {
	db := setupHubTestDB(t)
	rdb := setupHubTestRedis(t)
	senderID := presenceTestUser(t, db)
	ctx := context.Background()

	require.NoError(t, rdb.Set(ctx, presence.StatusRedisKey(senderID), statusOnline, 0).Err())
	_, err := db.ExecContext(ctx,
		`INSERT INTO presence_offline_fences (user_id) VALUES ($1)`, senderID,
	)
	require.NoError(t, err)

	resolver := NewSenderPresenceResolver(rdb, db)
	require.False(t, resolver.RichPresenceEmissionPermitted(ctx, senderID),
		"a durable offline fence must override a stale visible Redis status")
}
