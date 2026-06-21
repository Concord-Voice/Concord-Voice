package database

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRedisURL returns the Redis URL for tests, matching testhelpers.SetupTestRedis.
func testRedisURL() string {
	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}
	const devRedisVal = "concord_dev_redis" //nolint:gosec // dev-only default
	return "redis://:" + devRedisVal + "@localhost:6379"
}

func TestNewRedisClientInvalidURL(t *testing.T) {
	_, err := NewRedisClient("not-a-valid-url")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid redis URL")
}

func TestNewRedisClientEmptyURL(t *testing.T) {
	_, err := NewRedisClient("")
	require.Error(t, err)
}

func TestNewRedisClientMalformedScheme(t *testing.T) {
	_, err := NewRedisClient("postgres://localhost:5432")
	require.Error(t, err)
	// go-redis rejects non-redis/rediss schemes
}

func TestNewRedisClientSuccess(t *testing.T) {
	client, err := NewRedisClient(testRedisURL())
	if err != nil {
		// Redis not available in this environment — skip rather than fail
		t.Skipf("Redis not available: %v", err)
	}
	defer func() {
		require.NoError(t, client.Close())
	}()
	require.NotNil(t, client)
}
