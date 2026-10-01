package rbac

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// principalLockError reads whether the server still exists before choosing
// ErrNotMember over the missing principal. A read that fails decides neither:
// it stays a fault, because an outage must never read as a denial. The
// transaction is ended first, so the read fails with sql.ErrTxDone.
func TestPrincipalLockError_AFailedServerReadStaysAFault(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	got := principalLockError(context.Background(), tx, uuid.NewString(), errAuthorityPrincipalGone)

	assert.ErrorIs(t, got, sql.ErrTxDone, "the failed read is what is reported")
	assert.NotErrorIs(t, got, ErrNotMember)
	assert.NotErrorIs(t, got, errAuthorityPrincipalGone)
}
