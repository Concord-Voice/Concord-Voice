package rbac

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWithAuthorityCapturePinsReadCommitted prevents a server-default change
// from reopening the lock/snapshot race. The visibility advisory lock is the
// first statement in this transaction; READ COMMITTED is what gives the guard
// query a snapshot after that lock is granted.
func TestWithAuthorityCapturePinsReadCommitted(t *testing.T) {

	source, err := os.ReadFile("authority_tx.go")
	require.NoError(t, err)
	require.Contains(t, string(source),
		"h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})")
}
