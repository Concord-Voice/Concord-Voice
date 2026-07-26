package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func liveTokenStateRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { assert.NoError(t, rdb.Close()) })
	return rdb
}

func TestVerifyLiveTokenState(t *testing.T) {
	const userID = "11111111-1111-4111-8111-111111111111"

	t.Run("disabled key fails closed", func(t *testing.T) {
		rdb := liveTokenStateRedis(t)
		require.NoError(t, rdb.Set(context.Background(), UserDisabledKey(userID), "1", time.Minute).Err())

		emailVerified, err := VerifyLiveTokenState(context.Background(), rdb, userID, jwt.MapClaims{
			"email_verified": true,
		})

		assert.False(t, emailVerified)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "account disabled")
	})

	t.Run("Redis error fails closed", func(t *testing.T) {
		// Redis pointed at a closed port so every command fails — no infra needed.
		rdb := redis.NewClient(&redis.Options{
			Addr:        "127.0.0.1:1",
			DialTimeout: 100 * time.Millisecond,
			MaxRetries:  -1,
		})
		t.Cleanup(func() { assert.NoError(t, rdb.Close()) })

		emailVerified, err := VerifyLiveTokenState(context.Background(), rdb, userID, jwt.MapClaims{
			"email_verified": true,
		})

		assert.False(t, emailVerified)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "check disabled user")
	})

	t.Run("explicit unverified claim is returned without error", func(t *testing.T) {
		rdb := liveTokenStateRedis(t)

		emailVerified, err := VerifyLiveTokenState(context.Background(), rdb, userID, jwt.MapClaims{
			"email_verified": false,
		})

		require.NoError(t, err)
		assert.False(t, emailVerified)
	})

	for _, tc := range []struct {
		name   string
		claims jwt.MapClaims
	}{
		{name: "absent email claim retains legacy verified behavior", claims: jwt.MapClaims{}},
		{name: "non-boolean email claim retains legacy verified behavior", claims: jwt.MapClaims{"email_verified": "false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb := liveTokenStateRedis(t)

			emailVerified, err := VerifyLiveTokenState(context.Background(), rdb, userID, tc.claims)

			require.NoError(t, err)
			assert.True(t, emailVerified)
		})
	}
}
