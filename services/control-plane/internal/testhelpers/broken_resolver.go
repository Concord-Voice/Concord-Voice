package testhelpers

import (
	"database/sql"
	"testing"

	dbtest "github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// BrokenResolver returns a *rbac.Resolver whose backing database is closed, so
// every permission computation (HasPermission / GetEffectivePermissions) fails
// with an error instead of returning a boolean.
//
// Pair it with a WORKING *sql.DB on the handler under test: the handler's own
// pre-permission queries (membership lookups, etc.) run on that working DB and
// still succeed, so control reaches — and isolates — the handler's
// `if permErr != nil { ... HTTP 500 }` branch. The full-router integration
// tests cannot otherwise reach that branch, because a healthy resolver never
// errors on a valid request; this is the sanctioned error-injection seam for
// covering those defensive 500 paths.
//
// The cache is backed by the supplied (working) Redis so a fresh lookup misses
// the cache and falls through to the closed DB (an empty/poisoned cache would
// otherwise short-circuit before the DB query).
func BrokenResolver(t *testing.T, rdb *redis.Client) *rbac.Resolver {
	t.Helper()
	closed, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	// Close immediately: every subsequent query returns "sql: database is closed".
	require.NoError(t, closed.Close())
	return rbac.NewResolver(closed, rbac.NewPermissionCache(rdb), logger.New("test"))
}
