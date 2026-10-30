package loginfamily_test

import (
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCapturePreservesWholeFamilyAndDistinguishesUnavailable(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	preserve := newFamily(t, db, user, expires())
	selected := newFamily(t, db, user, expires())
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	f, err := lf.GuardRefreshSIDTx(ctx, tx, u, preserve.SID)
	require.NoError(t, err)
	sibling := uuid.New()
	require.NoError(t, lf.BindSIDTx(ctx, tx, f, sibling, expires()))
	targets, err := lf.CaptureUserTargetsTx(ctx, tx, u, &preserve)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	require.NoError(t, tx.Commit())
	_, err = lf.ResolveVerifiedAccess(ctx, db, preserve)
	require.NoError(t, err)
	siblingIdentity := preserve
	siblingIdentity.SID = sibling
	_, err = lf.ResolveVerifiedAccess(ctx, db, siblingIdentity)
	require.NoError(t, err)
	_, err = lf.ResolveVerifiedAccess(ctx, db, selected)
	reason(t, err, lf.ErrDenied, lf.ReasonFamilyTerminal)
	tx2 := transaction(t, db)
	u2 := authority(t, tx2, user, lf.UserNoKeyUpdate)
	_, err = lf.CaptureUserTargetsTx(ctx, tx2, u2, &selected)
	reason(t, err, lf.ErrDenied, lf.ReasonPreserveUnknown)
	_, err = lf.CaptureUserTargetsTx(ctx, tx2, u2, &lf.AccessIdentity{UserID: user, SID: uuid.New(), AccessExpiresAt: expires()})
	reason(t, err, lf.ErrDenied, lf.ReasonPreserveUnknown)
	_, err = tx2.Exec(`SELECT 1/0`)
	require.Error(t, err)
	_, err = lf.CaptureUserTargetsTx(ctx, tx2, u2, &preserve)
	reason(t, err, lf.ErrUnavailable, lf.ReasonSQLUnavailable)
}
func TestTargetResolutionDeduplicatesAndRejectsForeignSID(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	foreignUser := testdb.CreateUser(t, db)
	own := newFamily(t, db, user, expires())
	foreign := newFamily(t, db, foreignUser, expires())
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	for _, sid := range []uuid.UUID{foreign.SID, uuid.New()} {
		_, err := lf.ResolveTargetSIDTx(ctx, tx, u, sid)
		reason(t, err, lf.ErrTargetNotFound, lf.ReasonTargetUnknown)
	}
	targets, err := lf.ResolveMatchedSIDsTx(ctx, tx, u, []uuid.UUID{own.SID, own.SID})
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonDisplacement})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	require.NoError(t, tx.Commit())
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_family_revocation_targets`))
}
func TestCapturedFrontierExceedsPageAndReplayLeavesFreshLoginLive(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	var first uuid.UUID
	for n := 0; n < 205; n++ {
		f, err := lf.CreateFamilyTx(ctx, tx, u, "")
		require.NoError(t, err)
		sid := uuid.New()
		if n == 0 {
			first = sid
		}
		require.NoError(t, lf.BindSIDTx(ctx, tx, f, sid, expires()))
	}
	targets, err := lf.CaptureUserTargetsTx(ctx, tx, u, nil)
	require.NoError(t, err)
	// A login minted after capture in this same owning transaction is outside
	// the immutable captured frontier.
	laterFamily, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	later := lf.AccessIdentity{UserID: user, SID: uuid.New(), AccessExpiresAt: expires()}
	require.NoError(t, lf.BindSIDTx(ctx, tx, laterFamily, later.SID, later.AccessExpiresAt))
	result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonTheft})
	require.NoError(t, err)
	require.EqualValues(t, 205, result.NewlyTerminalFamilies)
	require.Len(t, result.Owners, 1)
	require.NoError(t, tx.Commit())
	_, err = lf.ResolveVerifiedAccess(ctx, db, later)
	require.NoError(t, err)
	fresh := newFamily(t, db, user, expires())
	tx2 := transaction(t, db)
	u2 := authority(t, tx2, user, lf.UserNoKeyUpdate)
	old, err := lf.ResolveTargetSIDTx(ctx, tx2, u2, first)
	require.NoError(t, err)
	replay, err := lf.RevokeFamiliesTx(ctx, tx2, old, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonLogout})
	require.NoError(t, err)
	require.Zero(t, replay.NewlyTerminalFamilies)
	require.Equal(t, result.Owners, replay.Owners)
	require.NoError(t, tx2.Commit())
	_, err = lf.ResolveVerifiedAccess(ctx, db, fresh)
	require.NoError(t, err)
	require.EqualValues(t, 205, count(t, db, `SELECT count(*) FROM login_family_revocation_targets`))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_family_revocations WHERE sealed_at IS NOT NULL`))
}

func TestEmptyFrontierCreatesNoOperationAndBadTargetInputCannotBroadenCapture(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	empty, err := lf.ResolveMatchedSIDsTx(ctx, tx, u, nil)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(ctx, tx, empty, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonErasure})
	require.NoError(t, err)
	require.Zero(t, result.NewlyTerminalFamilies)
	require.Empty(t, result.Owners)
	_, err = lf.ResolveMatchedSIDsTx(ctx, tx, u, []uuid.UUID{uuid.Nil})
	require.Error(t, err)
	require.NoError(t, tx.Commit())
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
}
