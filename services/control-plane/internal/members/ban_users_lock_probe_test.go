package members

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

// banUsersLockError reads whether the server still exists before choosing
// between the ban's 403 and the target-gone 404. A read that fails decides
// neither: it stays a fault, because an outage must never read as a denial.
// The transaction is ended first, so the read fails with sql.ErrTxDone.
func TestBanUsersLockError_AFailedServerReadStaysAFault(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	got := banUsersLockError(context.Background(), tx, uuid.NewString(),
		fmt.Errorf("moderation user no longer exists: %w", errModerationTargetGone))

	assert.ErrorIs(t, got, sql.ErrTxDone, "the failed read is what is reported")
	assert.NotErrorIs(t, got, errModerationTargetGone)
	assert.NotErrorIs(t, got, errModerationPermissionDenied)
}
