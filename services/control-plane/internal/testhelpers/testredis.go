package testhelpers

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

var defaultTestRedisURL = "redis://:" + testRedisVal + "@localhost:6379" //nolint:gosec // matches docker-compose dev default

var testRedisVal = "concord_dev_redis" //nolint:gosec // dev-only default

// SetupTestRedis creates a Redis client isolated from dev data by default.
func SetupTestRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	useDefaultDB := redisURL == ""
	if useDefaultDB {
		redisURL = defaultTestRedisURL
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("testhelpers: failed to parse redis URL: %v", err)
	}

	// Use DB 1 for the default dev URL; honor explicit REDIS_URL DBs for isolated runs.
	if useDefaultDB {
		opts.DB = 1
	}

	client := redis.NewClient(opts)

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("testhelpers: failed to ping redis: %v", err)
	}

	if err := flushTestRedis(ctx, client); err != nil {
		t.Fatalf("testhelpers: failed to flush redis: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
	}

	return client, cleanup
}

func flushTestRedis(ctx context.Context, client *redis.Client) error {
	return client.Do(ctx, "FLUSHDB", "SYNC").Err()
}
