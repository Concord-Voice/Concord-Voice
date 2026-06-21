package age

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// TestClaimNonce_RedisError_FailsClosed: a non-redis.Nil error (here a dead
// Redis) must map to ErrUnavailable. The replay guard fails CLOSED — a single-
// use nonce has no other backstop, so a degraded Redis must reject, not admit.
func TestClaimNonce_RedisError_FailsClosed(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: time.Second,
		MaxRetries:  -1,
	})
	defer func() { _ = rdb.Close() }()

	err := ClaimNonce(t.Context(), rdb, "00000000-0000-0000-0000-000000000000", "deadbeef")
	assert.ErrorIs(t, err, ErrUnavailable)
}
