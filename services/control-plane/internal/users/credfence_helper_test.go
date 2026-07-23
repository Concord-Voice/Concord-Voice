package users_test

import (
	"database/sql"
	"io"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// testCredFence builds a working credential-epoch fence for directly-constructed
// users.Handler tests (#2201): ChangePassword and ReplaceMyKeys fail closed
// (503) without one. The fence's Redis half is a per-test miniredis — these
// tests exercise handler transaction behavior, not cross-surface cache reads,
// so isolation from the shared test Redis is a feature.
func testCredFence(t *testing.T, db *sql.DB) *credepoch.Fence {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return credepoch.New(db, rdb, logger.NewWithWriter(io.Discard))
}
