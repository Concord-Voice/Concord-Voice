package loginfamily_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type recordingCloser struct {
	users, families []uuid.UUID
	fail            error
}

func (r *recordingCloser) DisconnectFamilyContext(ctx context.Context, user, family uuid.UUID) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("post-commit close must be bounded")
	}
	r.users = append(r.users, user)
	r.families = append(r.families, family)
	return r.fail
}

func familyOf(t *testing.T, db *sql.DB, identity lf.AccessIdentity) uuid.UUID {
	t.Helper()
	var family uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, identity.SID).Scan(&family))
	return family
}

// A committed owner fences exactly its terminal families, across more than one
// page, and never a live sibling; a failing close is reported, not hidden.
func TestCloseCommittedFamiliesFencesExactlyOwnedTerminalFamilies(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	preserved := newFamily(t, db, user, expires())
	const revokedCount = 105 // more than one owner page
	want := make([]uuid.UUID, 0, revokedCount)
	for range revokedCount {
		want = append(want, familyOf(t, db, newFamily(t, db, user, expires())))
	}
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureUserTargetsTx(ctx, tx, u, &preserved)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)

	// Before commit the owner reads no terminal rows: nothing can be fenced.
	early := &recordingCloser{}
	require.NoError(t, lf.CloseCommittedFamilies(ctx, db, early, result.Owners))
	require.Empty(t, early.families)

	require.NoError(t, tx.Commit())
	closer := &recordingCloser{}
	cancelled, cancel := context.WithCancel(ctx)
	cancel() // a gone client must not abandon the fence
	require.NoError(t, lf.CloseCommittedFamilies(cancelled, db, closer, result.Owners))
	require.ElementsMatch(t, want, closer.families)
	require.NotContains(t, closer.families, familyOf(t, db, preserved))
	for _, got := range closer.users {
		require.Equal(t, user, got)
	}

	failing := &recordingCloser{fail: errors.New("injected close failure")}
	require.Error(t, lf.CloseCommittedFamilies(ctx, db, failing, result.Owners))
	require.Len(t, failing.families, revokedCount, "one failed close must not skip the rest")
}

func TestCloseCommittedFamiliesRejectsUnusableInputs(t *testing.T) {
	db := database(t)
	require.Error(t, lf.CloseCommittedFamilies(ctx, nil, &recordingCloser{}, nil))
	require.Error(t, lf.CloseCommittedFamilies(ctx, db, nil, nil))
	owner := lf.DeliveryOwner{Kind: "unknown", UserID: uuid.New(), AuthOperationID: uuid.New()}
	reason(t, lf.CloseCommittedFamilies(ctx, db, &recordingCloser{}, []lf.DeliveryOwner{owner}), lf.ErrContract, lf.ReasonOwnershipInvalid)
	reason(t, lf.CloseCommittedFamilies(ctx, db, &recordingCloser{}, []lf.DeliveryOwner{{Kind: lf.OwnerEpoch, UserID: uuid.New()}}), lf.ErrContract, lf.ReasonOwnershipInvalid)
}
