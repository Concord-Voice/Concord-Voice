package rbac

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// reorderShortfallError's existence read cannot be failed by the statement
// hook, which fires once at the UPDATE. These cases run it directly: an absent
// server is the non-member denial, and a read on an ended transaction confirms
// nothing, so it is a fault rather than either the 403 or the 404.
// TestReorderRoles_UnknownRoleID_NotFound covers the live server's 404.
func TestReorderShortfallError(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("an absent server is the non-member denial", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				t.Errorf("rollback: %v", rbErr)
			}
		}()

		assert.ErrorIs(t, reorderShortfallError(ctx, tx, uuid.NewString(), 0, 2), ErrNotMember)
	})

	t.Run("a failed existence read stays a fault", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())

		got := reorderShortfallError(ctx, tx, uuid.NewString(), 0, 2)

		require.Error(t, got)
		assert.NotErrorIs(t, got, ErrNotMember, "a read that failed confirms no deletion")
		assert.NotErrorIs(t, got, sql.ErrNoRows, "nor that a role is missing")
	})
}
