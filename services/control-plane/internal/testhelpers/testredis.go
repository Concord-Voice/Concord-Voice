package testhelpers

import (
	"context"
	"os"
	"strings"
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
		t.Fatal(pingFailureMessage(err))
	}

	if err := flushTestRedis(ctx, client); err != nil {
		t.Fatalf("testhelpers: failed to flush redis: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
	}

	return client, cleanup
}

// pingFailureMessage explains a failed test-Redis ping. Auth failures get their
// own message because docker-compose starts Redis with --requirepass, so an
// uncredentialed (NOAUTH) or stale-password (WRONGPASS) REDIS_URL fails every
// Redis-backed test identically and the bare driver error names no cause.
//
// redis.IsAuthError is the authoritative check — it covers NOAUTH, WRONGPASS and
// "unauthenticated", and unwraps. The string fallback exists because it matches
// on redis' own error TYPES, which callers cannot construct: proto.RedisError is
// internal, so the unit tests can only reach this branch by content.
//
// Never echo the URL — it carries a password.
func pingFailureMessage(err error) string {
	msg := err.Error()
	if redis.IsAuthError(err) || strings.Contains(msg, "NOAUTH") || strings.Contains(msg, "WRONGPASS") {
		return "testhelpers: redis rejected the connection (auth failed). REDIS_URL must carry the " +
			"docker-compose REDIS_PASSWORD and select DB 1, e.g. " +
			"redis://:<password>@localhost:6379/1 — see docs/development.md"
	}
	return "testhelpers: failed to ping redis: " + msg
}

func flushTestRedis(ctx context.Context, client *redis.Client) error {
	return client.Do(ctx, "FLUSHDB", "SYNC").Err()
}
