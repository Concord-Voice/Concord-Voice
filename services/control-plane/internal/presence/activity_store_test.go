package presence

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityKeyAcceptsOnlyExactSupportedCategories(t *testing.T) {
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	serverKey, err := activityKey(userID, CategoryServerVoice)
	require.NoError(t, err)
	assert.Equal(t, "presence:rich:11111111-1111-1111-1111-111111111111:server_voice", serverKey)

	callKey, err := activityKey(userID, CategoryPrivateCall)
	require.NoError(t, err)
	assert.Equal(t, "presence:rich:11111111-1111-1111-1111-111111111111:private_call", callKey)

	for _, category := range []Category{"", "custom_text", "server_voice:*", "*"} {
		_, err := activityKey(userID, category)
		assert.Error(t, err, "category %q must be rejected", category)
	}
	_, err = activityKey(uuid.Nil, CategoryServerVoice)
	assert.Error(t, err)
}

func TestActivityStoreCurrentState(t *testing.T) {
	rdb := setupActivityStoreRedis(t)

	ctx := context.Background()
	store := NewActivityStore(rdb)
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	token := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	key, err := activityKey(userID, CategoryServerVoice)
	require.NoError(t, err)

	state := ActivityState{
		SourceToken:   token,
		SourceVersion: 1784088000000000,
		Minimized:     true,
		Payload:       json.RawMessage(`{"channel_id":"33333333-3333-3333-3333-333333333333","server_id":"44444444-4444-4444-4444-444444444444"}`),
		UpdatedAt:     1784088000,
	}

	t.Run("compare-set writes strict envelope with exact TTL", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())

		stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		require.NoError(t, err)
		assert.True(t, stored)
		assert.Equal(t, 90*time.Second, ActivityStateTTL)

		raw, err := rdb.Get(ctx, key).Result()
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"source_token":"22222222-2222-2222-2222-222222222222",
			"source_version":1784088000000000,
			"minimized":true,
			"payload":{"channel_id":"33333333-3333-3333-3333-333333333333","server_id":"44444444-4444-4444-4444-444444444444"},
			"updated_at":1784088000
		}`, raw)

		var envelope map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(raw), &envelope))
		assert.ElementsMatch(t,
			[]string{"source_token", "source_version", "minimized", "payload", "updated_at"},
			mapKeys(envelope),
		)

		ttl, err := rdb.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, 89*time.Second)
		assert.LessOrEqual(t, ttl, ActivityStateTTL)

		got, found, err := store.Get(ctx, userID, CategoryServerVoice)
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, state, got)
	})

	t.Run("compare-set rejects older and conflicting generations", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)

		older := state
		older.SourceToken = uuid.MustParse("55555555-5555-5555-5555-555555555555")
		older.SourceVersion--
		older.UpdatedAt--
		stored, err = store.CompareAndSet(ctx, userID, CategoryServerVoice, older)
		require.NoError(t, err)
		assert.False(t, stored)

		conflicting := state
		conflicting.SourceToken = older.SourceToken
		stored, err = store.CompareAndSet(ctx, userID, CategoryServerVoice, conflicting)
		require.NoError(t, err)
		assert.False(t, stored)

		got, found, err := store.Get(ctx, userID, CategoryServerVoice)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, state, got)

		newer := older
		newer.SourceVersion = state.SourceVersion + 1
		newer.UpdatedAt = state.UpdatedAt + 1
		stored, err = store.CompareAndSet(ctx, userID, CategoryServerVoice, newer)
		require.NoError(t, err)
		assert.True(t, stored)
	})

	t.Run("refresh requires exact generation and restores TTL", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		require.NoError(t, rdb.Expire(ctx, key, 10*time.Second).Err())

		refreshed, err := store.Refresh(ctx, userID, CategoryServerVoice, uuid.New(), state.SourceVersion)
		require.NoError(t, err)
		assert.False(t, refreshed)
		refreshed, err = store.Refresh(ctx, userID, CategoryServerVoice, token, state.SourceVersion-1)
		require.NoError(t, err)
		assert.False(t, refreshed)

		ttl, err := rdb.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.LessOrEqual(t, ttl, 10*time.Second)

		refreshed, err = store.Refresh(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		require.NoError(t, err)
		assert.True(t, refreshed)
		ttl, err = rdb.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, 89*time.Second)
		assert.LessOrEqual(t, ttl, ActivityStateTTL)
	})

	t.Run("compare-delete requires exact token and version", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)

		deleted, err := store.CompareAndDelete(ctx, userID, CategoryServerVoice, uuid.New(), state.SourceVersion)
		require.NoError(t, err)
		assert.False(t, deleted)
		deleted, err = store.CompareAndDelete(ctx, userID, CategoryServerVoice, token, state.SourceVersion-1)
		require.NoError(t, err)
		assert.False(t, deleted)
		assert.Equal(t, int64(1), rdb.Exists(ctx, key).Val())

		deleted, err = store.CompareAndDelete(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		require.NoError(t, err)
		assert.True(t, deleted)
		assert.Equal(t, int64(0), rdb.Exists(ctx, key).Val())
	})

	t.Run("delete removes only the validated exact category key", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		otherCategoryKey, err := activityKey(userID, CategoryPrivateCall)
		require.NoError(t, err)
		otherUserID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
		otherUserKey, err := activityKey(otherUserID, CategoryServerVoice)
		require.NoError(t, err)
		for _, exactKey := range []string{key, otherCategoryKey, otherUserKey} {
			require.NoError(t, rdb.Set(ctx, exactKey, `{}`, time.Minute).Err())
		}

		require.NoError(t, store.Delete(ctx, userID, CategoryServerVoice))
		assert.Equal(t, int64(0), rdb.Exists(ctx, key).Val())
		assert.Equal(t, int64(1), rdb.Exists(ctx, otherCategoryKey).Val())
		assert.Equal(t, int64(1), rdb.Exists(ctx, otherUserKey).Val())

		assert.ErrorIs(t, store.Delete(ctx, userID, Category("server_voice:*")), ErrInvalidActivityState)
		assert.ErrorIs(t, store.Delete(ctx, uuid.Nil, CategoryServerVoice), ErrInvalidActivityState)
	})

	t.Run("malformed state is deleted and never returned", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		for _, raw := range malformedActivityStates() {
			require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
			got, found, err := store.Get(ctx, userID, CategoryServerVoice)
			assert.ErrorIs(t, err, ErrMalformedActivityState)
			assert.False(t, found)
			assert.Equal(t, ActivityState{}, got)
			assert.Equal(t, int64(0), rdb.Exists(ctx, key).Val())
		}
	})

	t.Run("generation operations delete malformed state", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		for _, raw := range append([]string{"not-json"}, malformedActivityStates()...) {
			require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
			refreshed, err := store.Refresh(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
			assert.False(t, refreshed)
			assert.ErrorIs(t, err, ErrMalformedActivityState)
			assert.Equal(t, int64(0), rdb.Exists(ctx, key).Val())

			require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
			deleted, err := store.CompareAndDelete(
				ctx, userID, CategoryServerVoice, token, state.SourceVersion,
			)
			assert.False(t, deleted)
			assert.ErrorIs(t, err, ErrMalformedActivityState)
			assert.Equal(t, int64(0), rdb.Exists(ctx, key).Val())

			require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
			stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
			require.NoError(t, err)
			assert.True(t, stored)
			got, found, err := store.Get(ctx, userID, CategoryServerVoice)
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, state, got)
		}
	})

	t.Run("missing state is not fabricated", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		got, found, err := store.Get(ctx, userID, CategoryServerVoice)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Equal(t, ActivityState{}, got)

		refreshed, err := store.Refresh(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		require.NoError(t, err)
		assert.False(t, refreshed)
		deleted, err := store.CompareAndDelete(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		require.NoError(t, err)
		assert.False(t, deleted)
	})

	t.Run("invalid arguments fail before Redis", func(t *testing.T) {
		invalidStates := []ActivityState{
			{},
			withActivityState(state, func(value *ActivityState) { value.SourceToken = uuid.Nil }),
			withActivityState(state, func(value *ActivityState) { value.SourceVersion = 0 }),
			withActivityState(state, func(value *ActivityState) { value.SourceVersion = (1 << 53) + 1 }),
			withActivityState(state, func(value *ActivityState) { value.UpdatedAt = 0 }),
			withActivityState(state, func(value *ActivityState) {
				value.UpdatedAt = MaxActivityUnixSeconds + 1
			}),
			withActivityState(state, func(value *ActivityState) { value.Payload = json.RawMessage(`[]`) }),
			withActivityState(state, func(value *ActivityState) { value.Payload = json.RawMessage(`{`) }),
		}
		for _, invalid := range invalidStates {
			stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, invalid)
			assert.False(t, stored)
			assert.ErrorIs(t, err, ErrInvalidActivityState)
		}

		stored, err := store.CompareAndSet(ctx, userID, Category("*"), state)
		assert.False(t, stored)
		assert.ErrorIs(t, err, ErrInvalidActivityState)
		_, err = store.Refresh(ctx, userID, CategoryServerVoice, uuid.Nil, state.SourceVersion)
		assert.ErrorIs(t, err, ErrInvalidActivityState)
		_, err = store.Refresh(ctx, userID, CategoryServerVoice, token, (1<<53)+1)
		assert.ErrorIs(t, err, ErrInvalidActivityState)
		_, err = store.CompareAndDelete(ctx, userID, CategoryServerVoice, token, 0)
		assert.ErrorIs(t, err, ErrInvalidActivityState)
	})

	t.Run("unavailable Redis fails closed", func(t *testing.T) {
		unavailable := NewActivityStore(nil)
		stored, err := unavailable.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		assert.False(t, stored)
		assert.Error(t, err)
		got, found, err := unavailable.Get(ctx, userID, CategoryServerVoice)
		assert.Equal(t, ActivityState{}, got)
		assert.False(t, found)
		assert.Error(t, err)
		refreshed, err := unavailable.Refresh(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		assert.False(t, refreshed)
		assert.Error(t, err)
		deleted, err := unavailable.CompareAndDelete(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		assert.False(t, deleted)
		assert.Error(t, err)
		assert.Error(t, unavailable.Delete(ctx, userID, CategoryServerVoice))
	})

	t.Run("caller context cancellation prevents every operation", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		stored, err := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		require.NoError(t, err)
		require.True(t, stored)
		canceled, cancel := context.WithCancel(ctx)
		cancel()

		newer := state
		newer.SourceVersion++
		stored, err = store.CompareAndSet(canceled, userID, CategoryServerVoice, newer)
		assert.False(t, stored)
		assert.ErrorIs(t, err, context.Canceled)
		_, found, err := store.Get(canceled, userID, CategoryServerVoice)
		assert.False(t, found)
		assert.ErrorIs(t, err, context.Canceled)
		refreshed, err := store.Refresh(canceled, userID, CategoryServerVoice, token, state.SourceVersion)
		assert.False(t, refreshed)
		assert.ErrorIs(t, err, context.Canceled)
		deleted, err := store.CompareAndDelete(canceled, userID, CategoryServerVoice, token, state.SourceVersion)
		assert.False(t, deleted)
		assert.ErrorIs(t, err, context.Canceled)
		assert.ErrorIs(t, store.Delete(canceled, userID, CategoryServerVoice), context.Canceled)
		assert.Equal(t, int64(1), rdb.Exists(ctx, key).Val())
	})

	t.Run("Redis command errors never fabricate state", func(t *testing.T) {
		brokenClient := redis.NewClient(&redis.Options{
			Addr:         "127.0.0.1:1",
			MaxRetries:   -1,
			DialTimeout:  25 * time.Millisecond,
			ReadTimeout:  25 * time.Millisecond,
			WriteTimeout: 25 * time.Millisecond,
		})
		t.Cleanup(func() { require.NoError(t, brokenClient.Close()) })
		broken := NewActivityStore(brokenClient)

		stored, err := broken.CompareAndSet(ctx, userID, CategoryServerVoice, state)
		assert.False(t, stored)
		assert.Error(t, err)
		got, found, err := broken.Get(ctx, userID, CategoryServerVoice)
		assert.Equal(t, ActivityState{}, got)
		assert.False(t, found)
		assert.Error(t, err)
		refreshed, err := broken.Refresh(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		assert.False(t, refreshed)
		assert.Error(t, err)
		deleted, err := broken.CompareAndDelete(ctx, userID, CategoryServerVoice, token, state.SourceVersion)
		assert.False(t, deleted)
		assert.Error(t, err)
		assert.Error(t, broken.Delete(ctx, userID, CategoryServerVoice))
	})
}

func TestActivityStoreWrongTypeCurrentStateSelfHeals(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	store := NewActivityStore(rdb)
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	key, err := activityKey(userID, CategoryServerVoice)
	require.NoError(t, err)
	state := wrongTypeActivityTestState()

	operations := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "get",
			run: func(t *testing.T) {
				got, found, getErr := store.Get(ctx, userID, CategoryServerVoice)
				assert.Equal(t, ActivityState{}, got)
				assert.False(t, found)
				assert.ErrorIs(t, getErr, ErrMalformedActivityState)
				assert.Zero(t, rdb.Exists(ctx, key).Val())
			},
		},
		{
			name: "compare-set",
			run: func(t *testing.T) {
				stored, storeErr := store.CompareAndSet(ctx, userID, CategoryServerVoice, state)
				require.NoError(t, storeErr)
				assert.True(t, stored)
				assertActivityStateStored(ctx, t, store, userID, CategoryServerVoice, state)
			},
		},
		{
			name: "compare-set-active",
			run: func(t *testing.T) {
				seedActivityLifecycle(
					t, rdb, userID, CategoryServerVoice,
					state.SourceToken, state.SourceVersion, true,
				)
				stored, storeErr := store.CompareAndSetActive(
					ctx, userID, CategoryServerVoice, state,
				)
				require.NoError(t, storeErr)
				assert.True(t, stored)
				assertActivityStateStored(ctx, t, store, userID, CategoryServerVoice, state)
			},
		},
		{
			name: "refresh",
			run: func(t *testing.T) {
				refreshed, refreshErr := store.Refresh(
					ctx, userID, CategoryServerVoice,
					state.SourceToken, state.SourceVersion,
				)
				assert.False(t, refreshed)
				assert.ErrorIs(t, refreshErr, ErrMalformedActivityState)
				assert.Zero(t, rdb.Exists(ctx, key).Val())
			},
		},
		{
			name: "compare-delete",
			run: func(t *testing.T) {
				deleted, deleteErr := store.CompareAndDelete(
					ctx, userID, CategoryServerVoice,
					state.SourceToken, state.SourceVersion,
				)
				assert.False(t, deleted)
				assert.ErrorIs(t, deleteErr, ErrMalformedActivityState)
				assert.Zero(t, rdb.Exists(ctx, key).Val())
			},
		},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			for _, seed := range wrongTypeActivitySeeds() {
				t.Run(seed.name, func(t *testing.T) {
					require.NoError(t, rdb.FlushDB(ctx).Err())
					require.NoError(t, seed.seed(ctx, rdb, key))
					operation.run(t)
				})
			}
		})
	}
}

func TestActivityStoreCompareAndSetActiveChecksLifecycleBeforeWrongTypeState(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	store := NewActivityStore(rdb)
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	key, err := activityKey(userID, CategoryServerVoice)
	require.NoError(t, err)
	state := wrongTypeActivityTestState()

	for _, seed := range wrongTypeActivitySeeds() {
		t.Run(seed.name, func(t *testing.T) {
			require.NoError(t, rdb.FlushDB(ctx).Err())
			seedActivityLifecycle(
				t, rdb, userID, CategoryServerVoice,
				state.SourceToken, state.SourceVersion+1, true,
			)
			require.NoError(t, seed.seed(ctx, rdb, key))

			stored, storeErr := store.CompareAndSetActive(
				ctx, userID, CategoryServerVoice, state,
			)
			require.NoError(t, storeErr)
			assert.False(t, stored)
			assert.Equal(t, seed.redisType, rdb.Type(ctx, key).Val())
		})
	}
}

func TestActivityStoreWrongTypeHealingIsExactKeyOnly(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	store := NewActivityStore(rdb)
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	otherUserID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	targetKey, err := activityKey(userID, CategoryPrivateCall)
	require.NoError(t, err)
	otherCategoryKey, err := activityKey(userID, CategoryServerVoice)
	require.NoError(t, err)
	otherUserKey, err := activityKey(otherUserID, CategoryPrivateCall)
	require.NoError(t, err)

	require.NoError(t, rdb.HSet(ctx, targetKey, "poison", "value").Err())
	wantValues := map[string]string{
		otherCategoryKey: "neighbor-category",
		otherUserKey:     "neighbor-user",
		"sentinel:2405":  "unrelated",
	}
	for key, value := range wantValues {
		require.NoError(t, rdb.Set(ctx, key, value, time.Minute).Err())
	}

	got, found, err := store.Get(ctx, userID, CategoryPrivateCall)
	assert.Equal(t, ActivityState{}, got)
	assert.False(t, found)
	assert.ErrorIs(t, err, ErrMalformedActivityState)
	assert.Zero(t, rdb.Exists(ctx, targetKey).Val())
	for key, want := range wantValues {
		assert.Equal(t, want, rdb.Get(ctx, key).Val())
	}
}

func TestCompareAndDeleteRawActivityStateGuardsTypeAndPreservesStringSuccessor(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	key := "presence:rich:11111111-1111-1111-1111-111111111111:server_voice"

	t.Run("string successor", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		successor := `{"source_token":"22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`
		require.NoError(t, rdb.Set(ctx, key, successor, time.Minute).Err())

		deleted, err := compareAndDeleteRawActivityScript.Run(
			ctx, rdb, []string{key}, "stale-malformed-value",
		).Int()
		require.NoError(t, err)
		assert.Zero(t, deleted)
		assert.Equal(t, successor, rdb.Get(ctx, key).Val())
	})

	t.Run("wrong-type successor", func(t *testing.T) {
		require.NoError(t, rdb.FlushDB(ctx).Err())
		require.NoError(t, rdb.HSet(ctx, key, "poison", "value").Err())

		_, err := compareAndDeleteRawActivityScript.Run(
			ctx, rdb, []string{key}, "stale-malformed-value",
		).Int()
		require.NoError(t, err)
		assert.Zero(t, rdb.Exists(ctx, key).Val())
	})
}

type wrongTypeActivitySeed struct {
	name      string
	redisType string
	seed      func(context.Context, *redis.Client, string) error
}

func wrongTypeActivitySeeds() []wrongTypeActivitySeed {
	return []wrongTypeActivitySeed{
		{
			name: "hash", redisType: "hash",
			seed: func(ctx context.Context, rdb *redis.Client, key string) error {
				return rdb.HSet(ctx, key, "field", "value").Err()
			},
		},
		{
			name: "list", redisType: "list",
			seed: func(ctx context.Context, rdb *redis.Client, key string) error {
				return rdb.RPush(ctx, key, "value").Err()
			},
		},
		{
			name: "set", redisType: "set",
			seed: func(ctx context.Context, rdb *redis.Client, key string) error {
				return rdb.SAdd(ctx, key, "value").Err()
			},
		},
		{
			name: "zset", redisType: "zset",
			seed: func(ctx context.Context, rdb *redis.Client, key string) error {
				return rdb.ZAdd(ctx, key, redis.Z{Score: 1, Member: "value"}).Err()
			},
		},
		{
			name: "stream", redisType: "stream",
			seed: func(ctx context.Context, rdb *redis.Client, key string) error {
				return rdb.XAdd(ctx, &redis.XAddArgs{
					Stream: key,
					Values: map[string]interface{}{"field": "value"},
				}).Err()
			},
		},
	}
}

func wrongTypeActivityTestState() ActivityState {
	return ActivityState{
		SourceToken:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		SourceVersion: 1784088000000000,
		Minimized:     true,
		Payload:       json.RawMessage(`{"channel_id":"33333333-3333-3333-3333-333333333333","server_id":"44444444-4444-4444-4444-444444444444"}`),
		UpdatedAt:     1784088000,
	}
}

func assertActivityStateStored(
	ctx context.Context,
	t *testing.T,
	store *ActivityStore,
	userID uuid.UUID,
	category Category,
	want ActivityState,
) {
	t.Helper()
	got, found, err := store.Get(ctx, userID, category)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, want, got)
	key, err := activityKey(userID, category)
	require.NoError(t, err)
	ttl, err := store.redis.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 89*time.Second)
	assert.LessOrEqual(t, ttl, ActivityStateTTL)
}

func TestActivityStateScriptResultValidatesTaggedReplies(t *testing.T) {
	ctx := context.Background()

	for _, test := range []struct {
		name       string
		value      []interface{}
		wantStatus string
		wantRaw    []byte
	}{
		{name: "missing", value: []interface{}{"none"}, wantStatus: "none"},
		{name: "malformed", value: []interface{}{"malformed"}, wantStatus: "malformed"},
		{
			name: "string", value: []interface{}{"string", `{"payload":"value"}`},
			wantStatus: "string", wantRaw: []byte(`{"payload":"value"}`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := redis.NewCmd(ctx)
			command.SetVal(test.value)

			status, raw, err := activityStateScriptResult(command)
			require.NoError(t, err)
			assert.Equal(t, test.wantStatus, status)
			assert.Equal(t, test.wantRaw, raw)
		})
	}

	for _, test := range []struct {
		name  string
		value []interface{}
	}{
		{name: "empty", value: []interface{}{}},
		{name: "unknown tag", value: []interface{}{"unknown"}},
		{name: "non-string tag", value: []interface{}{int64(1)}},
		{name: "missing string value", value: []interface{}{"string"}},
		{name: "unexpected missing value", value: []interface{}{"none", "value"}},
		{name: "non-string value", value: []interface{}{"string", int64(1)}},
		{name: "too many values", value: []interface{}{"string", "value", "extra"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := redis.NewCmd(ctx)
			command.SetVal(test.value)

			status, raw, err := activityStateScriptResult(command)
			assert.Empty(t, status)
			assert.Nil(t, raw)
			assert.Error(t, err)
		})
	}

	t.Run("command error", func(t *testing.T) {
		command := redis.NewCmd(ctx)
		command.SetErr(context.Canceled)

		status, raw, err := activityStateScriptResult(command)
		assert.Empty(t, status)
		assert.Nil(t, raw)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestActivityStoreIsActiveGenerationStrictlyVerifiesLifecycleEnvelope(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	store := NewActivityStore(rdb)
	senderID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	token := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	version := int64(1784088000000000)
	lifecycleKey, err := VoiceLifecycleKey(senderID, CategoryServerVoice)
	require.NoError(t, err)

	active, err := store.IsActiveGeneration(
		ctx, senderID, CategoryServerVoice, token, version,
	)
	require.NoError(t, err)
	assert.False(t, active, "a missing lifecycle envelope is inactive")

	seedActivityLifecycle(
		t, rdb, senderID, CategoryServerVoice, token, version, true,
	)
	active, err = store.IsActiveGeneration(
		ctx, senderID, CategoryServerVoice, token, version,
	)
	require.NoError(t, err)
	assert.True(t, active)

	for _, test := range []struct {
		name    string
		token   uuid.UUID
		version int64
		active  bool
	}{
		{name: "terminal", token: token, version: version},
		{name: "different token", token: uuid.New(), version: version, active: true},
		{name: "different version", token: token, version: version + 1, active: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			seedActivityLifecycle(
				t, rdb, senderID, CategoryServerVoice, test.token, test.version, test.active,
			)
			matched, matchErr := store.IsActiveGeneration(
				ctx, senderID, CategoryServerVoice, token, version,
			)
			require.NoError(t, matchErr)
			assert.False(t, matched)
		})
	}

	require.NoError(t, rdb.Del(ctx, lifecycleKey).Err())
	require.NoError(t, rdb.Set(ctx, lifecycleKey, "poisoned", time.Minute).Err())
	active, err = store.IsActiveGeneration(
		ctx, senderID, CategoryServerVoice, token, version,
	)
	assert.False(t, active)
	assert.ErrorIs(t, err, ErrMalformedActivityLifecycle)
	assert.Zero(t, rdb.Exists(ctx, lifecycleKey).Val())
}

func TestActivityStoreVerifyActiveGenerationsPipelinesStrictChecksInInputOrder(t *testing.T) {
	rdb := setupActivityStoreRedis(t)
	ctx := context.Background()
	store := NewActivityStore(rdb)
	activeSender := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	terminalSender := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	missingSender := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	activeToken := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	terminalToken := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	version := int64(1784088000000000)
	seedActivityLifecycle(
		t, rdb, activeSender, CategoryServerVoice, activeToken, version, true,
	)
	seedActivityLifecycle(
		t, rdb, terminalSender, CategoryPrivateCall, terminalToken, version+1, false,
	)

	results, err := store.VerifyActiveGenerations(ctx, []ActivityGeneration{
		{UserID: activeSender, Category: CategoryServerVoice, SourceToken: activeToken, SourceVersion: version},
		{UserID: terminalSender, Category: CategoryPrivateCall, SourceToken: terminalToken, SourceVersion: version + 1},
		{UserID: missingSender, Category: CategoryServerVoice, SourceToken: uuid.New(), SourceVersion: version + 2},
	})
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false, false}, results)

	// Exercise the 64-command pipeline boundary with one item in the second
	// batch. Duplicate exact checks are intentional and preserve input order.
	batchBoundary := make([]ActivityGeneration, activityLifecycleVerificationBatchSize+1)
	for index := range batchBoundary {
		batchBoundary[index] = ActivityGeneration{
			UserID: activeSender, Category: CategoryServerVoice,
			SourceToken: activeToken, SourceVersion: version,
		}
	}
	results, err = store.VerifyActiveGenerations(ctx, batchBoundary)
	require.NoError(t, err)
	require.Len(t, results, activityLifecycleVerificationBatchSize+1)
	for _, active := range results {
		assert.True(t, active)
	}
}

func malformedActivityStates() []string {
	return []string{
		`{"source_token":"22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"payload":{},"updated_at":1784088000}`,
		`{"source_token":"22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000,"audience":[]}`,
		`{"source_token":"not-a-uuid","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`,
		`{"source_token":"00000000-0000-0000-0000-000000000000","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`,
		`{"source_token":"AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`,
		`{"source_token":"22222222222222222222222222222222","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`,
		`{"source_token":"urn:uuid:22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`,
		`{"SOURCE_TOKEN":"22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":1784088000}`,
		`{"source_token":"22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"minimized":false,"payload":null,"updated_at":1784088000}`,
		`{"source_token":"22222222-2222-2222-2222-222222222222","source_version":1784088000000000,"minimized":false,"payload":{},"updated_at":9007199255}`,
	}
}

func withActivityState(state ActivityState, mutate func(*ActivityState)) ActivityState {
	mutate(&state)
	return state
}

func setupActivityStoreRedis(t *testing.T) *redis.Client {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	useDefaultDB := redisURL == ""
	if useDefaultDB {
		redisURL = "redis://:concord_dev_redis@localhost:6379" //nolint:gosec // dev-only test default
	}
	opts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	if useDefaultDB {
		opts.DB = 1
	}

	client := redis.NewClient(opts)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.NoError(t, client.Ping(context.Background()).Err())
	require.NoError(t, client.FlushDB(context.Background()).Err())
	return client
}

func mapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
