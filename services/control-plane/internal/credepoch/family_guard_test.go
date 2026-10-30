package credepoch

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Package-local coverage for GuardFamilyTx (#2272): every refusal branch maps
// to ErrEpochMismatch (stale credential) or ErrFamilyGuardUnavailable (guard
// fault), and only a live, matching family identity is admitted.

func familyGuardDB(t *testing.T) *sql.DB {
	t.Helper()
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	return db
}

func familyGuardActor(ctx context.Context, t *testing.T, db *sql.DB) (context.Context, string) {
	t.Helper()
	uid := testdb.CreateUser(t, db).String()
	return testdb.ActorContext(ctx, t, db, uid), uid
}

func familyGuardMarker(ctx context.Context, t *testing.T, tx *sql.Tx) string {
	t.Helper()
	var held string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting($1, true), '')`, familyGuardSetting).Scan(&held))
	return held
}

func TestGuardFamilyTx_ContractMismatchesFailClosed(t *testing.T) {
	db := familyGuardDB(t)
	ctx, uid := familyGuardActor(context.Background(), t, db)
	identity, ok := lf.AccessIdentityFromContext(ctx)
	require.True(t, ok)
	other := testdb.CreateUser(t, db).String()

	tx := ownerTx(t, db)
	cases := []struct {
		name  string
		ctx   context.Context
		tx    *sql.Tx
		actor string
		epoch string
	}{
		{"no identity on context", context.Background(), tx, uid, identity.CredentialEpoch},
		{"nil transaction", ctx, nil, uid, identity.CredentialEpoch},
		{"unparseable actor", ctx, tx, "not-a-uuid", identity.CredentialEpoch},
		{"different actor", ctx, tx, other, identity.CredentialEpoch},
		{"different token epoch", ctx, tx, uid, "0123456789abcdef0123456789abcdee"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, GuardFamilyTx(tc.ctx, tc.tx, tc.actor, tc.epoch), ErrEpochMismatch)
		})
	}
	// No refusal above reached SQL, so the transaction holds no guard marker.
	require.Empty(t, familyGuardMarker(context.Background(), t, tx))
}

func TestGuardFamilyTx_AdmitsLiveFamilyAndIsIdempotent(t *testing.T) {
	db := familyGuardDB(t)
	ctx, uid := familyGuardActor(context.Background(), t, db)
	identity, ok := lf.AccessIdentityFromContext(ctx)
	require.True(t, ok)

	tx := ownerTx(t, db)
	require.NoError(t, GuardFamilyTx(ctx, tx, uid, identity.CredentialEpoch))
	require.Equal(t, identity.UserID.String()+":"+identity.SID.String(), familyGuardMarker(ctx, t, tx))

	// The second call for the same identity is a no-op that still admits.
	require.NoError(t, GuardFamilyTx(ctx, tx, uid, identity.CredentialEpoch))
	require.NoError(t, tx.Commit())

	// The marker is transaction-local (set_config is_local=true).
	next := ownerTx(t, db)
	require.Empty(t, familyGuardMarker(ctx, t, next))
}

func TestGuardFamilyTx_SecondActorInOneTransactionIsAFault(t *testing.T) {
	db := familyGuardDB(t)
	firstCtx, first := familyGuardActor(context.Background(), t, db)
	secondCtx, second := familyGuardActor(context.Background(), t, db)
	firstID, ok := lf.AccessIdentityFromContext(firstCtx)
	require.True(t, ok)
	secondID, ok := lf.AccessIdentityFromContext(secondCtx)
	require.True(t, ok)

	tx := ownerTx(t, db)
	require.NoError(t, GuardFamilyTx(firstCtx, tx, first, firstID.CredentialEpoch))
	err := GuardFamilyTx(secondCtx, tx, second, secondID.CredentialEpoch)
	require.ErrorIs(t, err, ErrFamilyGuardUnavailable)
	require.NotErrorIs(t, err, ErrEpochMismatch)
}

func TestGuardFamilyTx_RotatedStoredEpochIsStale(t *testing.T) {
	db := familyGuardDB(t)
	ctx, uid := familyGuardActor(context.Background(), t, db)
	identity, ok := lf.AccessIdentityFromContext(ctx)
	require.True(t, ok)
	// The signed identity still carries the pre-rotation epoch.
	_, err := db.Exec(`UPDATE users SET credential_epoch = $2 WHERE id = $1`, uid, testPredecessorEpoch)
	require.NoError(t, err)

	tx := ownerTx(t, db)
	require.ErrorIs(t, GuardFamilyTx(ctx, tx, uid, identity.CredentialEpoch), ErrEpochMismatch)
	require.Empty(t, familyGuardMarker(context.Background(), t, tx))
}

func TestGuardFamilyTx_TerminalFamilyIsDenied(t *testing.T) {
	db := familyGuardDB(t)
	ctx, uid := familyGuardActor(context.Background(), t, db)
	identity, ok := lf.AccessIdentityFromContext(ctx)
	require.True(t, ok)

	revoke, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = revoke.Rollback() }()
	user := ownerAuthority(t, revoke, uid)
	targets, err := lf.ResolveTargetSIDTx(context.Background(), revoke, user, identity.SID)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(context.Background(), revoke, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonLogout})
	require.NoError(t, err)
	require.Equal(t, 1, int(result.NewlyTerminalFamilies))
	require.NoError(t, revoke.Commit())

	tx := ownerTx(t, db)
	err = GuardFamilyTx(ctx, tx, uid, identity.CredentialEpoch)
	require.ErrorIs(t, err, ErrEpochMismatch)
	require.ErrorIs(t, err, lf.ErrDenied)
	require.Empty(t, familyGuardMarker(context.Background(), t, tx))
}

func TestGuardFamilyTx_UnknownSIDIsDenied(t *testing.T) {
	db := familyGuardDB(t)
	ctx, uid := familyGuardActor(context.Background(), t, db)
	identity, ok := lf.AccessIdentityFromContext(ctx)
	require.True(t, ok)
	identity.SID = uuid.New()
	forged := lf.WithAccessIdentity(context.Background(), identity)

	tx := ownerTx(t, db)
	require.ErrorIs(t, GuardFamilyTx(forged, tx, uid, identity.CredentialEpoch), ErrEpochMismatch)
}

func TestGuardFamilyTx_SQLFaultIsUnavailable(t *testing.T) {
	db := familyGuardDB(t)
	ctx, uid := familyGuardActor(context.Background(), t, db)
	identity, ok := lf.AccessIdentityFromContext(ctx)
	require.True(t, ok)

	tx := ownerTx(t, db)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := GuardFamilyTx(canceled, tx, uid, identity.CredentialEpoch)
	require.ErrorIs(t, err, ErrFamilyGuardUnavailable)
	require.NotErrorIs(t, err, ErrEpochMismatch)
}
