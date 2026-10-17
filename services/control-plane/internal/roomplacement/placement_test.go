package roomplacement

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newSelector(t *testing.T) (*miniredis.Miniredis, *redis.Client, *Selector) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, client, &Selector{Redis: client, Enabled: true}
}

func TestHasLiveRoomSuspendsCleanupAndFailsClosed(t *testing.T) {
	server, client, selector := newSelector(t)
	const roomID = "550e8400-e29b-41d4-a716-446655440000"
	live, err := selector.HasLiveRoom(t.Context(), roomID)
	require.NoError(t, err)
	require.False(t, live)
	require.NoError(t, server.Set("voice:room_live:"+roomID, "blue-42"))
	live, err = selector.HasLiveRoom(t.Context(), roomID)
	require.NoError(t, err)
	require.True(t, live)
	require.NoError(t, client.Close())
	_, err = selector.HasLiveRoom(t.Context(), roomID)
	require.Error(t, err)
}

func TestSelectRoomPrefersLiveRegisteredColourAndRepairsOwner(t *testing.T) {
	server, _, selector := newSelector(t)
	require.NoError(t, server.Set("media:active_colour", "green"))
	_, err := server.SAdd("media:colours:registered", "blue", "green")
	require.NoError(t, err)
	require.NoError(t, server.Set("voice:room_live:room-1", "blue-184"))
	require.NoError(t, server.Set("voice:room_owner:room-1", "green"))
	require.NoError(t, server.Set("media:colour_draining:green", "183"))
	require.NoError(t, server.Set("media:colour_draining:blue", ""))

	selected, err := selector.Select(context.Background(), "room-1", "https://media.example/socket.io/?transport=polling")
	require.NoError(t, err)
	parsed, err := url.Parse(selected)
	require.NoError(t, err)
	require.Equal(t, "blue", parsed.Query().Get("colour"))
	require.Equal(t, "polling", parsed.Query().Get("transport"))
	owner, err := server.Get("voice:room_owner:room-1")
	require.NoError(t, err)
	require.Equal(t, "blue", owner)
	require.Equal(t, 90*time.Second, server.TTL("voice:room_owner:room-1"))
}

func TestSelectRoomUsesExistingLiveRoomWithoutActiveColour(t *testing.T) {
	server, _, selector := newSelector(t)
	_, err := server.SAdd("media:colours:registered", "blue")
	require.NoError(t, err)
	require.NoError(t, server.Set("voice:room_live:room-live", "blue-184"))

	selected, err := selector.Select(context.Background(), "room-live", "https://media.example")
	require.NoError(t, err)
	require.Contains(t, selected, "colour=blue")
}

func TestSelectRoomPreservesLiveColourWhenDiagnosticRegistryIsEmpty(t *testing.T) {
	server, _, selector := newSelector(t)
	require.NoError(t, server.Set("media:active_colour", "green"))
	require.NoError(t, server.Set("voice:room_live:room-live", "blue-184"))
	require.NoError(t, server.Set("voice:room_owner:room-live", "blue"))

	selected, err := selector.Select(context.Background(), "room-live", "https://media.example")
	require.NoError(t, err)
	require.Contains(t, selected, "colour=blue")
	owner, err := server.Get("voice:room_owner:room-live")
	require.NoError(t, err)
	require.Equal(t, "blue", owner)
}

func TestSelectRoomIgnoresLateLiveMarkerAfterColourIsUnregistered(t *testing.T) {
	server, _, selector := newSelector(t)
	require.NoError(t, server.Set("media:active_colour", "green"))
	_, err := server.SAdd("media:colours:registered", "green")
	require.NoError(t, err)
	require.NoError(t, server.Set("voice:room_live:room-stale", "blue-184"))
	require.NoError(t, server.Set("voice:room_owner:room-stale", "blue"))

	selected, err := selector.Select(context.Background(), "room-stale", "https://media.example")
	require.NoError(t, err)
	require.Contains(t, selected, "colour=green")
	owner, err := server.Get("voice:room_owner:room-stale")
	require.NoError(t, err)
	require.Equal(t, "green", owner)
}

func TestSelectRoomFloorsExistingReservationWithoutExtendingLongLease(t *testing.T) {
	server, _, selector := newSelector(t)
	require.NoError(t, server.Set("media:active_colour", "green"))
	_, err := server.SAdd("media:colours:registered", "blue", "green")
	require.NoError(t, err)
	require.NoError(t, server.Set("voice:room_owner:room-2", "blue"))
	server.SetTTL("voice:room_owner:room-2", 10*time.Second)

	selected, err := selector.Select(context.Background(), "room-2", "https://media.example")
	require.NoError(t, err)
	require.Contains(t, selected, "colour=blue")
	require.Equal(t, 45*time.Second, server.TTL("voice:room_owner:room-2"))

	server.SetTTL("voice:room_owner:room-2", 70*time.Second)
	_, err = selector.Select(context.Background(), "room-2", "https://media.example")
	require.NoError(t, err)
	require.Equal(t, 70*time.Second, server.TTL("voice:room_owner:room-2"))
}

func TestSelectRoomRepointsOnlyDrainingOrUnregisteredOwner(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *miniredis.Miniredis)
	}{
		{name: "draining", setup: func(t *testing.T, server *miniredis.Miniredis) {
			require.NoError(t, server.Set("voice:room_owner:room-3", "blue"))
			require.NoError(t, server.Set("media:colour_draining:blue", "1"))
		}},
		{name: "registered set excludes owner", setup: func(t *testing.T, server *miniredis.Miniredis) {
			require.NoError(t, server.Set("voice:room_owner:room-3", "blue"))
			_, err := server.SRem("media:colours:registered", "blue")
			require.NoError(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, selector := newSelector(t)
			require.NoError(t, server.Set("media:active_colour", "green"))
			_, err := server.SAdd("media:colours:registered", "blue", "green")
			require.NoError(t, err)
			tt.setup(t, server)
			selected, err := selector.Select(context.Background(), "room-3", "https://media.example")
			require.NoError(t, err)
			require.Contains(t, selected, "colour=green")
			owner, err := server.Get("voice:room_owner:room-3")
			require.NoError(t, err)
			require.Equal(t, "green", owner)
			require.Equal(t, 45*time.Second, server.TTL("voice:room_owner:room-3"))
		})
	}
}

func TestSelectRoomFailsClosedForMissingActiveColourAndRedisFailure(t *testing.T) {
	server, _, selector := newSelector(t)
	_, err := server.SAdd("media:colours:registered", "blue")
	require.NoError(t, err)
	_, err = selector.Select(context.Background(), "room-4", "https://media.example")
	require.ErrorIs(t, err, ErrUnavailable)
	require.False(t, server.Exists("voice:room_owner:room-4"))

	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, ReadTimeout: 10 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = broken.Close() })
	selector.Redis = broken
	_, err = selector.Select(context.Background(), "room-4", "https://media.example")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUnavailable), "Redis transport errors retain their cause")
}

func TestSelectRoomCanRemainInertAndRejectInvalidBaseURL(t *testing.T) {
	selector := &Selector{}
	selected, err := selector.Select(context.Background(), "room-5", "legacy-url")
	require.NoError(t, err)
	require.Equal(t, "legacy-url", selected)

	_, _, selector = newSelector(t)
	selector.Redis.Set(context.Background(), "media:active_colour", "blue", 0)
	_, err = selector.Select(context.Background(), "room-5", "legacy-url")
	require.Error(t, err)
	_, err = selector.Redis.Get(context.Background(), "voice:room_owner:room-5").Result()
	require.ErrorIs(t, err, redis.Nil, "invalid base URL is rejected before claiming a room")
}

func TestGuardLifecycleFencesOwnerAndStaleGeneration(t *testing.T) {
	tests := []struct {
		name      string
		owner     string
		live      string
		want      LifecycleAuthority
		wantOwner string
	}{
		{name: "exact generation", owner: "blue", live: "blue-9", want: LifecycleFull, wantOwner: "blue"},
		{name: "owner refresh only", owner: "blue", live: "blue-8", want: LifecycleRefreshOnly, wantOwner: "blue"},
		{name: "missing occupancy refresh only", owner: "blue", want: LifecycleRefreshOnly, wantOwner: "blue"},
		{name: "stale generation without owner refused", live: "blue-8", want: LifecycleRefuse},
		{name: "different owner refused", owner: "green", live: "blue-9", want: LifecycleRefuse, wantOwner: "green"},
		{name: "matching occupancy repairs owner", live: "blue-9", want: LifecycleFull, wantOwner: "blue"},
		{name: "no state refresh only", want: LifecycleRefreshOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, selector := newSelector(t)
			if tt.owner != "" {
				require.NoError(t, server.Set("voice:room_owner:550e8400-e29b-41d4-a716-446655440006", tt.owner))
				server.SetTTL("voice:room_owner:550e8400-e29b-41d4-a716-446655440006", 20*time.Second)
			}
			if tt.live != "" {
				require.NoError(t, server.Set("voice:room_live:550e8400-e29b-41d4-a716-446655440006", tt.live))
			}
			authority, err := selector.GuardLifecycle(context.Background(), "550e8400-e29b-41d4-a716-446655440006", "blue", "blue-9", "voice.left")
			require.NoError(t, err)
			require.Equal(t, tt.want, authority)
			if tt.wantOwner == "" {
				require.False(t, server.Exists("voice:room_owner:550e8400-e29b-41d4-a716-446655440006"))
			} else {
				owner, err := server.Get("voice:room_owner:550e8400-e29b-41d4-a716-446655440006")
				require.NoError(t, err)
				require.Equal(t, tt.wantOwner, owner)
			}
			if tt.want == LifecycleRefuse {
				if tt.owner != "" {
					require.Equal(t, 20*time.Second, server.TTL("voice:room_owner:550e8400-e29b-41d4-a716-446655440006"))
				}
				return
			}
			if tt.wantOwner == "blue" && tt.name != "missing occupancy refresh only" {
				require.Equal(t, 90*time.Second, server.TTL("voice:room_owner:550e8400-e29b-41d4-a716-446655440006"))
			}
		})
	}
}

func TestLifecycleStillFullRechecksOwnerAndGeneration(t *testing.T) {
	tests := []struct {
		name     string
		owner    string
		live     string
		wantFull bool
	}{
		{name: "matching owner and live generation", owner: "blue", live: "blue-9", wantFull: true},
		{name: "owner absent but live generation matches", live: "blue-9", wantFull: true},
		{name: "live generation changed", owner: "blue", live: "blue-10"},
		{name: "malformed owner marker", owner: "BLUE", live: "blue-9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, selector := newSelector(t)
			const roomID = "550e8400-e29b-41d4-a716-446655440020"
			if tt.owner != "" {
				require.NoError(t, server.Set("voice:room_owner:"+roomID, tt.owner))
				server.SetTTL("voice:room_owner:"+roomID, 20*time.Second)
			}
			if tt.live != "" {
				require.NoError(t, server.Set("voice:room_live:"+roomID, tt.live))
			}

			stillFull, err := selector.LifecycleStillFull(t.Context(), roomID, "blue", "blue-9")
			require.NoError(t, err)
			require.Equal(t, tt.wantFull, stillFull,
				"destructive authority requires the exact live generation and either no owner marker or the matching colour")
			if tt.owner == "" {
				require.False(t, server.Exists("voice:room_owner:"+roomID), "read-only recheck must not create an owner marker")
			} else {
				require.Equal(t, 20*time.Second, server.TTL("voice:room_owner:"+roomID), "read-only recheck must not renew the owner lease")
			}
		})
	}
}

func TestLifecycleStillFullReturnsRedisError(t *testing.T) {
	selector := &Selector{
		Redis: redis.NewClient(&redis.Options{
			Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond,
			ReadTimeout: 10 * time.Millisecond, MaxRetries: -1,
		}),
		Enabled: true,
	}
	t.Cleanup(func() { _ = selector.Redis.Close() })

	stillFull, err := selector.LifecycleStillFull(
		t.Context(), "550e8400-e29b-41d4-a716-446655440020", "blue", "blue-9",
	)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUnavailable), "Redis transport errors must retain their cause")
	require.False(t, stillFull)
}

func TestGuardLifecycleRoomEmptyDemotesAndNeverExtends(t *testing.T) {
	server, _, selector := newSelector(t)
	roomID := "550e8400-e29b-41d4-a716-446655440007"
	require.NoError(t, server.Set("voice:room_owner:"+roomID, "blue"))
	require.NoError(t, server.Set("voice:room_live:"+roomID, "blue-9"))
	server.SetTTL("voice:room_owner:"+roomID, 90*time.Second)
	authority, err := selector.GuardLifecycle(context.Background(), roomID, "blue", "blue-9", "voice.room_empty")
	require.NoError(t, err)
	require.Equal(t, LifecycleFull, authority)
	require.Equal(t, 45*time.Second, server.TTL("voice:room_owner:"+roomID))

	server.SetTTL("voice:room_owner:"+roomID, 20*time.Second)
	_, err = selector.GuardLifecycle(context.Background(), roomID, "blue", "blue-9", "voice.room_empty")
	require.NoError(t, err)
	require.Equal(t, 20*time.Second, server.TTL("voice:room_owner:"+roomID))
}

func TestGuardLifecycleFailsClosedWhenRedisIsUnavailable(t *testing.T) {
	_, client, selector := newSelector(t)
	require.NoError(t, client.Close())
	authority, err := selector.GuardLifecycle(context.Background(), "550e8400-e29b-41d4-a716-446655440008", "blue", "blue-9", "voice.heartbeat")
	require.Error(t, err)
	require.Equal(t, LifecycleRefuse, authority)
}

func TestJoinIntentIsOneUseAndDoesNotExtendOnDuplicateMint(t *testing.T) {
	server, _, selector := newSelector(t)
	roomID := "550e8400-e29b-41d4-a716-446655440000"
	userID := "550e8400-e29b-41d4-a716-446655440001"
	require.NoError(t, selector.MintJoinIntent(context.Background(), roomID, userID))
	key := "voice:join_intent:" + roomID + ":" + userID
	server.SetTTL(key, 30*time.Second)
	require.NoError(t, selector.MintJoinIntent(context.Background(), roomID, userID))
	require.Equal(t, 30*time.Second, server.TTL(key))

	consumed, err := selector.ConsumeJoinIntent(context.Background(), roomID, userID)
	require.NoError(t, err)
	require.True(t, consumed)
	consumed, err = selector.ConsumeJoinIntent(context.Background(), roomID, userID)
	require.NoError(t, err)
	require.False(t, consumed)
}

func TestJoinIntentFailsClosedWhenRedisUnavailable(t *testing.T) {
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, ReadTimeout: 10 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = broken.Close() })
	selector := &Selector{Redis: broken, Enabled: true}
	roomID := "550e8400-e29b-41d4-a716-446655440010"
	userID := "550e8400-e29b-41d4-a716-446655440011"
	require.Error(t, selector.MintJoinIntent(context.Background(), roomID, userID))
	consumed, err := selector.ConsumeJoinIntent(context.Background(), roomID, userID)
	require.Error(t, err)
	require.False(t, consumed)
}
