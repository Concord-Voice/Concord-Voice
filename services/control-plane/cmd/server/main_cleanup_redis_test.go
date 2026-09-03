package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stub-based tests in main_cleanup_test.go cannot see a Redis-level
// regression, and since the guard moved into Lua that gap has teeth: the stub
// checks the script ARGUMENT equals the production constant and then reproduces
// the intended result in Go. A Lua syntax error, a wrong reply conversion, a
// pipelined EVAL that does not queue, or a SCAN pattern that matches nothing
// would leave every one of those tests green while the hourly production job
// silently reaped nothing -- or everything.
//
// These run the REAL go-redis client against a REAL Redis (miniredis, already a
// direct dependency and the idiom five other packages use) through the REAL
// cleanupStalePresence. Nothing about the outcome depends on a stub's
// semantics: the assertions read the keyspace back.
func presenceRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { assert.NoError(t, rdb.Close()) })
	return rdb, mr
}

func TestCleanupStalePresenceAgainstRealRedis(t *testing.T) {
	ctx := context.Background()
	rdb, mr := presenceRedis(t)

	liveUser, deadUser := uuid.New(), uuid.New()
	live := presence.StatusRedisKey(liveUser) // a connected user: 120s TTL
	dead := presence.StatusRedisKey(deadUser) // the anomaly: no expiry
	rich := "presence:rich:" + uuid.NewString() + ":" + string(presence.CategoryServerVoice)
	call := "presence:rich:" + uuid.NewString() + ":" + string(presence.CategoryPrivateCall)
	other := "presence:future:" + uuid.NewString() + ":thing"
	unrelated := "last_seen:" + uuid.NewString() // outside the SCAN pattern entirely

	require.NoError(t, rdb.Set(ctx, live, "online", 120*time.Second).Err())
	// Every key below is written WITHOUT an expiry, so the TTL half of the
	// predicate cannot be what spares them -- only the allowlist can.
	for _, k := range []string{dead, rich, call, other, unrelated} {
		require.NoError(t, rdb.Set(ctx, k, "x", 0).Err())
	}

	var logs bytes.Buffer
	cleanupStalePresence(ctx, rdb, logger.NewWithWriter(&logs))

	assert.False(t, mr.Exists(dead), "the unexpiring base presence key should have been reaped")
	for name, k := range map[string]string{
		"live base key":       live,
		"rich server_voice":   rich,
		"rich private_call":   call,
		"unknown family":      other,
		"non-presence prefix": unrelated,
	} {
		assert.True(t, mr.Exists(k), "%s was destroyed by the cleanup job", name)
	}

	// The live key must keep its expiry: the guard must not have rewritten it.
	ttl, err := rdb.PTTL(ctx, live).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "the live key lost its TTL")

	assert.Contains(t, logs.String(), "Cleanup: presence pass complete")
	assert.Contains(t, logs.String(), "count=1")
	assert.Contains(t, logs.String(), "partial=false")
}

// The Lua guard must delete ONLY on the -1 sentinel. Against a real server this
// also proves the script's `== -1` comparison behaves as intended, which is the
// half the stub can only assert about itself.
func TestCleanupStalePresenceRealRedisNeverReapsAnExpiringKey(t *testing.T) {
	ctx := context.Background()
	rdb, mr := presenceRedis(t)

	key := presence.StatusRedisKey(uuid.New())
	require.NoError(t, rdb.Set(ctx, key, "online", time.Second).Err())

	var logs bytes.Buffer
	cleanupStalePresence(ctx, rdb, logger.NewWithWriter(&logs))

	assert.True(t, mr.Exists(key), "a key with a positive TTL was reaped")
	assert.Contains(t, logs.String(), "count=0")
}

// A multi-page SCAN against a real server: more keys than the COUNT hint, so the
// cursor loop must actually iterate. A guard that stopped after one page would
// leave later unexpiring keys behind.
func TestCleanupStalePresenceRealRedisWalksTheWholeKeyspace(t *testing.T) {
	ctx := context.Background()
	rdb, mr := presenceRedis(t)

	const total = presenceScanBatch * 3
	keys := make([]string, 0, total)
	for i := 0; i < total; i++ {
		k := presence.StatusRedisKey(uuid.New())
		require.NoError(t, rdb.Set(ctx, k, "online", 0).Err()) // all unexpiring
		keys = append(keys, k)
	}

	var logs bytes.Buffer
	cleanupStalePresence(ctx, rdb, logger.NewWithWriter(&logs))

	for _, k := range keys {
		assert.False(t, mr.Exists(k), "an unexpiring key survived a multi-page scan")
	}
	assert.Contains(t, logs.String(), "count=300")
}
