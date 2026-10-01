package rbac_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// GuardActorGoneWithServer answers true only for a missing row at the guard on
// a server confirmed absent. Each case names an absent server, so the only
// thing that varies is the guard's error and whether the confirming read can
// run: a fault is never reclassified, and a read that fails confirms nothing
// and is returned as the error rather than dropped.
func TestGuardActorGoneWithServer(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	missingRow := fmt.Errorf("credepoch: guard read: %w", sql.ErrNoRows)

	cases := []struct {
		name     string
		guardErr error
		endTx    bool
		want     bool
		wantErr  error
	}{
		{name: "a missing row on an absent server", guardErr: missingRow, want: true},
		{name: "a fault on an absent server", guardErr: errors.New("connection reset"), want: false},
		{name: "a missing row whose server read fails", guardErr: missingRow, endTx: true, want: false, wantErr: sql.ErrTxDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			if tc.endTx {
				require.NoError(t, tx.Rollback())
			} else {
				defer func() {
					if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
						t.Errorf("rollback: %v", rbErr)
					}
				}()
			}

			gone, err := rbac.GuardActorGoneWithServer(ctx, tx, uuid.NewString(), tc.guardErr)
			assert.Equal(t, tc.want, gone)
			if tc.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.wantErr, "a failed server read is reported, not dropped")
			}
		})
	}
}
