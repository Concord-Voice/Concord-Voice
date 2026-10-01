package rbac

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Codex on #3508: when the epoch guard found no row and the server-existence
// probe that follows it failed, GuardActorGoneWithServer reported false and
// dropped the probe's error, so the role routes reported only the guard's
// missing row and the cause of the failed authorization read was lost. The
// probe runs on an ended transaction here, so it fails for real.
func TestRoleGuardError_AFailedServerProbeIsReported(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	missingRow := fmt.Errorf("credepoch: guard read: %w", sql.ErrNoRows)

	got := roleGuardError(ctx, tx, uuid.NewString(), missingRow)

	require.Error(t, got)
	assert.ErrorIs(t, got, sql.ErrTxDone, "the failed probe's cause must reach the caller")
	assert.NotErrorIs(t, got, ErrNotMember, "a probe that failed confirms no deletion")
	assert.NotErrorIs(t, got, sql.ErrNoRows, "nor may it read as ReorderRoles' missing role")
}
