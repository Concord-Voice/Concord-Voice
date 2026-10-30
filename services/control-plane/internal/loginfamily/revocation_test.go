package loginfamily_test

import (
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRevocationCountsRefreshRowsIncludingExpiredAndRetainsOwner(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	first := newFamily(t, db, user, expires())
	refresh(t, db, first, true)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	f, err := lf.GuardRefreshSIDTx(ctx, tx, u, first.SID)
	require.NoError(t, err)
	sibling := first
	sibling.SID = uuid.New()
	require.NoError(t, lf.BindSIDTx(ctx, tx, f, sibling.SID, sibling.AccessExpiresAt))
	require.NoError(t, tx.Commit())
	refresh(t, db, sibling, false)
	tx2 := transaction(t, db)
	u2 := authority(t, tx2, user, lf.UserNoKeyUpdate)
	targets, err := lf.ResolveTargetSIDTx(ctx, tx2, u2, first.SID)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(ctx, tx2, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	require.EqualValues(t, 2, result.RevokedRefreshRows)
	require.Len(t, result.Owners, 1)
	require.NoError(t, tx2.Commit())
	require.EqualValues(t, 2, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
	tx3 := transaction(t, db)
	u3 := authority(t, tx3, user, lf.UserNoKeyUpdate)
	targets, err = lf.ResolveTargetSIDTx(ctx, tx3, u3, sibling.SID)
	require.NoError(t, err)
	replay, err := lf.RevokeFamiliesTx(ctx, tx3, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonLogout})
	require.NoError(t, err)
	require.Zero(t, replay.NewlyTerminalFamilies)
	require.Zero(t, replay.RevokedRefreshRows)
	require.Equal(t, result.Owners, replay.Owners)
	require.NoError(t, tx3.Commit())
	var scope, why string
	require.NoError(t, db.QueryRow(`SELECT scope,reason FROM login_family_revocations`).Scan(&scope, &why))
	require.Equal(t, "session", scope)
	require.Equal(t, "explicit_revoke", why)
}
func TestRevocationRollbackRemovesEntireSealedFrontier(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	refresh(t, db, i, false)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureUserTargetsTx(ctx, tx, u, nil)
	require.NoError(t, err)
	_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonDisablement})
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT 1/0`)
	require.Error(t, err)
	require.Error(t, tx.Commit())
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocation_targets`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
	_, err = lf.ResolveVerifiedAccess(ctx, db, i)
	require.NoError(t, err)
}
func TestEpochRevocationChecksActualParentTupleAndRetainsDeletedParentLocator(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	replacement := "11111111111111111111111111111111"
	generation := uuid.New()
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureEpochTargetsTx(ctx, tx, u, "")
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,credential_epoch,superseded_credential_epoch,generation) VALUES($1,$2,'',$3)`, user, replacement, generation)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, user, replacement)
	require.NoError(t, err)
	owner := lf.EpochOwner{Generation: generation, UserID: user, CredentialEpoch: replacement}
	wrongNew := owner
	wrongNew.CredentialEpoch = "22222222222222222222222222222222"
	wrongOld := owner
	wrongOld.SupersededCredentialEpoch = "33333333333333333333333333333333"
	wrongGeneration := owner
	wrongGeneration.Generation = uuid.New()
	wrongUser := owner
	wrongUser.UserID = uuid.New()
	for _, forged := range []lf.EpochOwner{wrongNew, wrongOld, wrongGeneration, wrongUser} {
		_, err = lf.RevokeFamiliesForEpochTx(ctx, tx, targets, forged)
		require.ErrorIs(t, err, lf.ErrContract)
	}
	result, err := lf.RevokeFamiliesForEpochTx(ctx, tx, targets, owner)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	require.Equal(t, []lf.DeliveryOwner{{Kind: lf.OwnerEpoch, UserID: user, EpochGeneration: generation}}, result.Owners)
	require.NoError(t, tx.Commit())
	_, err = db.Exec(`DELETE FROM credential_epoch_voice_ejections WHERE generation=$1`, generation)
	require.NoError(t, err)
	tx2 := transaction(t, db)
	u2 := authority(t, tx2, user, lf.UserNoKeyUpdate)
	captured, err := lf.ResolveTargetSIDTx(ctx, tx2, u2, i.SID)
	require.NoError(t, err)
	replay, err := lf.RevokeFamiliesTx(ctx, tx2, captured, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonLogout})
	require.NoError(t, err)
	require.Zero(t, replay.NewlyTerminalFamilies)
	require.Equal(t, result.Owners, replay.Owners)
	require.NoError(t, tx2.Commit())
	require.Zero(t, count(t, db, `SELECT count(*) FROM credential_epoch_voice_ejections`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
}

func TestRevocationRejectsInvalidTargetCapabilitiesAndCause(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	cause := lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke}
	_, err := lf.RevokeFamiliesTx(ctx, tx, lf.Targets{}, cause)
	require.ErrorIs(t, err, lf.ErrContract)
	targets, err := lf.ResolveTargetSIDTx(ctx, tx, u, i.SID)
	require.NoError(t, err)
	other := transaction(t, db)
	_, err = lf.RevokeFamiliesTx(ctx, other, targets, cause)
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonTheft})
	require.ErrorIs(t, err, lf.ErrContract)
	require.NoError(t, tx.Commit())
	_, err = lf.RevokeFamiliesTx(ctx, tx, targets, cause)
	require.ErrorIs(t, err, lf.ErrContract)
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
}
