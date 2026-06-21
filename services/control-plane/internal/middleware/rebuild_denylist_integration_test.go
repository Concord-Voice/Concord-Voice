package middleware_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
)

// RebuildDisabledDenylist re-seeds the user_disabled:<id> keys from the
// users.disabled source of truth (#1623). It runs at process start and on Redis
// reconnect: a Redis flush degrades terminal-disable enforcement to the
// login/refresh DB gates until the rebuild closes the gap. These tests exercise
// the happy path plus both fail-loud error returns (the caller logs them).
// (deadRedis() — a client pointed at a closed port — is shared from ratelimit_test.go.)

func TestRebuildDisabledDenylist_SeedsKeyFromDB(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rebuildseed")
	_, err := ts.DB.Exec(
		`UPDATE users SET disabled=TRUE, disabled_reason='age_verification', disabled_at=NOW() WHERE id=$1`,
		user.ID)
	require.NoError(t, err)

	// Clear any pre-seeded key, then rebuild from the DB.
	require.NoError(t, ts.Redis.Del(t.Context(), middleware.UserDisabledKey(user.ID)).Err())
	require.NoError(t, middleware.RebuildDisabledDenylist(t.Context(), ts.DB, ts.Redis))

	n, err := ts.Redis.Exists(t.Context(), middleware.UserDisabledKey(user.ID)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "rebuild must seed the denylist key for a disabled user")
}

func TestRebuildDisabledDenylist_RedisSetError_ReturnsError(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rebuildrediserr")
	_, err := ts.DB.Exec(`UPDATE users SET disabled=TRUE WHERE id=$1`, user.ID)
	require.NoError(t, err)

	// The DB query succeeds and yields ≥1 disabled user, but the per-row Set
	// hits a dead Redis — the rebuild must surface the error, not swallow it.
	rdb := deadRedis()
	defer func() { _ = rdb.Close() }()

	err = middleware.RebuildDisabledDenylist(t.Context(), ts.DB, rdb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set", "set failure must be reported by the rebuild")
}

func TestRebuildDisabledDenylist_DBQueryError_ReturnsError(t *testing.T) {
	// A *sql.DB pointed at an unreachable port (no credentials in the DSN — the
	// connect never succeeds): the initial SELECT fails, so the rebuild returns
	// the query error without touching Redis. (lib/pq is the "postgres" driver,
	// registered transitively via testhelpers.)
	badDB, err := sql.Open("postgres", "postgres://127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	defer func() { _ = badDB.Close() }()

	rdb := deadRedis()
	defer func() { _ = rdb.Close() }()

	err = middleware.RebuildDisabledDenylist(context.Background(), badDB, rdb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query", "query failure must be reported by the rebuild")
}
