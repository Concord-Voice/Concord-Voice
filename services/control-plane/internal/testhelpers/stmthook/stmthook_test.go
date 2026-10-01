package stmthook_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Between's deferred rollback is a no-op only after a successful Commit. When an
// earlier step fails it is the real cleanup, and a rollback that fails too must
// reach the caller rather than be dropped behind the first error.
func TestBetween_ReportsARollbackThatFails(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	// The statement ends its own session, so it fails, and the rollback that
	// follows runs on a connection the server has already closed.
	sc := stmthook.Scenario{
		Name:      "the interleaved write kills its own connection",
		DeleteSQL: `SELECT pg_terminate_backend(pg_backend_pid()) WHERE $1::text IS NOT NULL`,
	}
	err := sc.Between(db, "owner", "server")()
	require.Error(t, err)
	require.ErrorContains(t, err, "rollback")
}
