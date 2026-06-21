package testhelpers

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

var defaultTestRedisURL = "redis://:" + testRedisVal + "@localhost:6379" //nolint:gosec // matches docker-compose dev default

var testRedisVal = "concord_dev_redis" //nolint:gosec // dev-only default

// SetupTestRedis creates a Redis client on DB index 1 (isolated from dev data).
func SetupTestRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = defaultTestRedisURL
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("testhelpers: failed to parse redis URL: %v", err)
	}

	// Use DB 1 to avoid colliding with development data on DB 0
	opts.DB = 1

	client := redis.NewClient(opts)

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("testhelpers: failed to ping redis: %v", err)
	}

	cleanup := func() {
		_ = client.FlushDB(context.Background()).Err()
		_ = client.Close()
	}

	// Start clean
	client.FlushDB(ctx)

	return client, cleanup
}
