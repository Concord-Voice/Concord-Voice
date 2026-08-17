package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
)

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
	// redistest.URL carries this process's own allocated logical database (#2680).
	// The helper this replaced fell back to a URL with NO path segment, which
	// go-redis resolves to DB 0 — the dev app's live database.
	// No "Redis not available" skip: redistest.URL allocates and pings first, so
	// an unreachable Redis fails there and never reaches this call. Keeping the
	// skip would describe behaviour that can no longer happen — and silently
	// vanishing suites is what #2680 removed from feedback, rbac and voice.
	client, err := NewRedisClient(redistest.URL(t))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, client.Close())
	}()
	require.NotNil(t, client)
}

func TestNewRedisClientEnablesContextTimeouts(t *testing.T) {
	// Same as above: redistest.URL fails closed before this can error.
	client, err := NewRedisClient(redistest.URL(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	assert.True(t, client.Options().ContextTimeoutEnabled)
}
