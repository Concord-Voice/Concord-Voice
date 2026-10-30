package loginfamily_test

import (
	"database/sql"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestResolveVerifiedAccessChecksPersistedIdentity(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	other := testdb.CreateUser(t, db)
	live := newFamily(t, db, user, expires())
	expired := newFamily(t, db, user, time.Now().Add(-time.Hour).Truncate(time.Second))
	grant, err := lf.ResolveVerifiedAccess(ctx, db, live)
	require.NoError(t, err)
	require.Equal(t, live.SID, grant.SID)
	require.True(t, grant.AccessExpiresAt.Equal(live.AccessExpiresAt))
	cases := []struct {
		name     string
		identity lf.AccessIdentity
		why      lf.ErrorReason
	}{
		{"missing membership", lf.AccessIdentity{UserID: user, SID: uuid.New(), AccessExpiresAt: live.AccessExpiresAt}, lf.ReasonAccessIdentity},
		{"foreign user", lf.AccessIdentity{UserID: other, SID: live.SID, AccessExpiresAt: live.AccessExpiresAt}, lf.ReasonAccessIdentity},
		{"signed expiry must equal persisted expiry", lf.AccessIdentity{UserID: user, SID: live.SID, AccessExpiresAt: live.AccessExpiresAt.Add(time.Second)}, lf.ReasonAccessExpired},
		{"expired access", expired, lf.ReasonAccessExpired},
		{"wrong current epoch", lf.AccessIdentity{UserID: user, SID: live.SID, AccessExpiresAt: live.AccessExpiresAt, CredentialEpoch: "11111111111111111111111111111111"}, lf.ReasonEpochMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lf.ResolveVerifiedAccess(ctx, db, tc.identity)
			reason(t, err, lf.ErrDenied, tc.why)
		})
	}
}

func TestGuardRefreshUsesRetainedMembershipWithoutAccessLifetime(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, time.Now().Add(-time.Hour).Truncate(time.Second))
	refresh(t, db, i, false)
	_, err := db.Exec(`DELETE FROM refresh_tokens WHERE id=$1`, i.SID)
	require.NoError(t, err)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	f, err := lf.GuardRefreshSIDTx(ctx, tx, u, i.SID)
	require.NoError(t, err)
	require.Equal(t, user, f.Binding().UserID)
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, i, lf.FamilyShare)
	reason(t, err, lf.ErrDenied, lf.ReasonAccessExpired)
	_, err = lf.GuardRefreshSIDTx(ctx, tx, u, uuid.New())
	require.ErrorIs(t, err, lf.ErrDenied)
	require.NoError(t, tx.Commit())
}

func TestCapabilityTransactionAndModeProvenance(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	tx := transaction(t, db)
	_, err := lf.LockUsersTx(ctx, tx, lf.WriterBarrier{}, []uuid.UUID{user}, lf.UserNoKeyUpdate)
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CreateFamilyTx(ctx, tx, lf.UserAuthority{}, "")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	reason(t, lf.BindSIDTx(ctx, tx, lf.LiveFamily{}, uuid.New(), expires()), lf.ErrContract, lf.ReasonCapabilityInvalid)
	b, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	set, err := lf.LockUsersTx(ctx, tx, b, []uuid.UUID{user}, lf.UserShare)
	require.NoError(t, err)
	u, err := set.ForUser(user)
	require.NoError(t, err)
	_, err = set.ForUser(uuid.New())
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.CreateFamilyTx(ctx, tx, u, "")
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, i, lf.FamilyNoKeyUpdate)
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, i, lf.FamilyShare)
	require.NoError(t, err)
	otherTx := transaction(t, db)
	_, err = lf.GuardAfterUserLockTx(ctx, otherTx, u, i, lf.FamilyShare)
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.LockUsersTx(ctx, otherTx, b, []uuid.UUID{user}, lf.UserShare)
	require.ErrorIs(t, err, lf.ErrContract)
	require.NoError(t, tx.Commit())
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, i, lf.FamilyShare)
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.LockUsersTx(ctx, otherTx, lf.WriterBarrier{}, []uuid.UUID{user}, lf.UserShare)
	require.ErrorIs(t, err, lf.ErrContract)
}

// A queued revoker is observed in PostgreSQL before the admission transaction
// releases its SHARE user lock. This proves serialization without a sleep.
func TestFinalGuardOrdersConcurrentRevocation(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	admission := transaction(t, db)
	u := authority(t, admission, user, lf.UserShare)
	_, err := lf.GuardAfterUserLockTx(ctx, admission, u, i, lf.FamilyShare)
	require.NoError(t, err)
	var xid int64
	require.NoError(t, admission.QueryRow(`SELECT txid_current()`).Scan(&xid))
	revoker := transaction(t, db)
	done := make(chan error, 1)
	go func() {
		barrier, e := lf.AcquireWriterBarrierTx(ctx, revoker)
		if e != nil {
			done <- e
			return
		}
		users, e := lf.LockUsersTx(ctx, revoker, barrier, []uuid.UUID{user}, lf.UserNoKeyUpdate)
		if e != nil {
			done <- e
			return
		}
		actor, e := users.ForUser(user)
		if e != nil {
			done <- e
			return
		}
		targets, e := lf.ResolveTargetSIDTx(ctx, revoker, actor, i.SID)
		if e != nil {
			done <- e
			return
		}
		_, e = lf.RevokeFamiliesTx(ctx, revoker, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke})
		if e != nil {
			done <- e
			return
		}
		done <- revoker.Commit()
	}()
	testdb.WaitForRowLockWaiter(t, db, xid)
	require.NoError(t, admission.Commit())
	require.NoError(t, <-done)
	final := transaction(t, db)
	finalUser := authority(t, final, user, lf.UserShare)
	_, err = lf.GuardAfterUserLockTx(ctx, final, finalUser, i, lf.FamilyShare)
	reason(t, err, lf.ErrDenied, lf.ReasonFamilyTerminal)
}

func TestCompleteUserSetCannotBeExtendedOrUpgraded(t *testing.T) {
	db := database(t)
	first := testdb.CreateUser(t, db)
	second := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	barrier, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	set, err := lf.LockUsersTx(ctx, tx, barrier, []uuid.UUID{second, first, second}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	_, err = set.ForUser(first)
	require.NoError(t, err)
	_, err = set.ForUser(second)
	require.NoError(t, err)
	_, err = lf.LockUsersTx(ctx, tx, barrier, []uuid.UUID{first, second}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	_, err = lf.LockUsersTx(ctx, tx, barrier, []uuid.UUID{first, second}, lf.UserUpdate)
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.LockUsersTx(ctx, tx, barrier, []uuid.UUID{first, second, uuid.New()}, lf.UserNoKeyUpdate)
	require.ErrorIs(t, err, lf.ErrContract)
}

func TestCreateAndBindRequireCurrentEpochAndImmutableSignedDeadline(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	_, err := lf.CreateFamilyTx(ctx, tx, u, "11111111111111111111111111111111")
	reason(t, err, lf.ErrDenied, lf.ReasonEpochMismatch)
	f, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	sid := uuid.New()
	deadline := expires()
	require.NoError(t, lf.BindSIDTx(ctx, tx, f, sid, deadline))
	require.NoError(t, lf.BindSIDTx(ctx, tx, f, sid, deadline))
	err = lf.BindSIDTx(ctx, tx, f, sid, deadline.Add(time.Second))
	reason(t, err, lf.ErrConflict, lf.ReasonIdentityConflict)
	other := transaction(t, db)
	err = lf.BindSIDTx(ctx, other, f, uuid.New(), deadline)
	require.ErrorIs(t, err, lf.ErrContract)
	require.NoError(t, tx.Commit())
	var persisted time.Time
	require.NoError(t, db.QueryRow(`SELECT access_expires_at FROM login_family_sids WHERE sid=$1`, sid).Scan(&persisted))
	require.True(t, deadline.Equal(persisted))
	err = lf.BindSIDTx(ctx, tx, f, uuid.New(), deadline)
	require.ErrorIs(t, err, lf.ErrContract)
}

func TestWriterBarrierRejectsDuplicateConstructorAndFinishedTransaction(t *testing.T) {
	db := database(t)
	tx := transaction(t, db)
	_, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	_, err = lf.AcquireWriterBarrierTx(ctx, tx)
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	require.NoError(t, tx.Rollback())
	_, err = lf.AcquireWriterBarrierTx(ctx, tx)
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
}

func TestCurrentEpochIsReadFreshAfterRotationInOwningTransaction(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	old := newFamily(t, db, user, expires())
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	captured, err := lf.CaptureEpochTargetsTx(ctx, tx, u, "")
	require.NoError(t, err)
	replacement := "44444444444444444444444444444444"
	generation := uuid.New()
	_, err = tx.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,credential_epoch,superseded_credential_epoch,generation) VALUES($1,$2,'',$3)`, user, replacement, generation)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, user, replacement)
	require.NoError(t, err)
	// A captured authority view cannot authorize another old-epoch mint after
	// this transaction changes the authoritative SQL epoch.
	_, err = lf.CreateFamilyTx(ctx, tx, u, "")
	reason(t, err, lf.ErrDenied, lf.ReasonEpochMismatch)
	_, err = lf.GuardRefreshSIDTx(ctx, tx, u, old.SID)
	reason(t, err, lf.ErrDenied, lf.ReasonEpochMismatch)
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, old, lf.FamilyNoKeyUpdate)
	reason(t, err, lf.ErrDenied, lf.ReasonEpochMismatch)
	continuation, err := lf.CreateFamilyTx(ctx, tx, u, replacement)
	require.NoError(t, err)
	current := lf.AccessIdentity{UserID: user, SID: uuid.New(), CredentialEpoch: replacement, AccessExpiresAt: expires()}
	require.NoError(t, lf.BindSIDTx(ctx, tx, continuation, current.SID, current.AccessExpiresAt))
	result, err := lf.RevokeFamiliesForEpochTx(ctx, tx, captured, lf.EpochOwner{Generation: generation, UserID: user, CredentialEpoch: replacement})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, current, lf.FamilyNoKeyUpdate)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = lf.ResolveVerifiedAccess(ctx, db, current)
	require.NoError(t, err)
	_, err = lf.ResolveVerifiedAccess(ctx, db, old)
	reason(t, err, lf.ErrDenied, lf.ReasonEpochMismatch)
}

func TestFinalGuardAcquiresFamilyLockAfterUserAuthority(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	grant, err := lf.ResolveVerifiedAccess(ctx, db, i)
	require.NoError(t, err)
	barrierTx := transaction(t, db)
	var family uuid.UUID
	require.NoError(t, barrierTx.QueryRow(`SELECT family_id FROM login_families WHERE family_id=$1 FOR NO KEY UPDATE`, grant.FamilyID).Scan(&family))
	var xid int64
	require.NoError(t, barrierTx.QueryRow(`SELECT txid_current()`).Scan(&xid))
	admission := transaction(t, db)
	u := authority(t, admission, user, lf.UserShare)
	done := make(chan error, 1)
	go func() { _, e := lf.GuardAfterUserLockTx(ctx, admission, u, i, lf.FamilyShare); done <- e }()
	testdb.WaitForRowLockWaiter(t, db, xid)
	require.NoError(t, barrierTx.Commit())
	require.NoError(t, <-done)
	require.NoError(t, admission.Commit())
}

func TestPublicAuthorityRejectsMalformedInputsBeforeMutation(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	f, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	for _, epoch := range []string{"bad", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		_, err = lf.CreateFamilyTx(ctx, tx, u, epoch)
		require.ErrorIs(t, err, lf.ErrContract)
		_, err = lf.CaptureEpochTargetsTx(ctx, tx, u, epoch)
		require.ErrorIs(t, err, lf.ErrContract)
	}
	err = lf.BindSIDTx(ctx, tx, f, uuid.Nil, expires())
	require.ErrorIs(t, err, lf.ErrContract)
	err = lf.BindSIDTx(ctx, tx, f, uuid.New(), time.Time{})
	require.ErrorIs(t, err, lf.ErrContract)
	err = lf.BindSIDTx(ctx, tx, f, uuid.New(), expires().Add(time.Nanosecond))
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.GuardAfterUserLockTx(ctx, tx, u, lf.AccessIdentity{UserID: user, SID: uuid.New(), AccessExpiresAt: expires()}, lf.FamilyLockMode(255))
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = lf.GuardRefreshSIDTx(ctx, tx, u, uuid.Nil)
	require.Error(t, err)
	_, err = lf.ResolveVerifiedAccess(ctx, db, lf.AccessIdentity{})
	require.ErrorIs(t, err, lf.ErrDenied)
	require.NoError(t, tx.Commit())
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_sids`))
}

func TestSnapshotDependencyFailureIsUnavailable(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	unavailable, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, unavailable.Close())
	_, err = lf.ResolveVerifiedAccess(ctx, unavailable, i)
	reason(t, err, lf.ErrUnavailable, lf.ReasonSQLUnavailable)
}

func TestUserLockAcquisitionRejectsInvalidSetsAndModes(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	for _, tc := range []struct {
		name string
		ids  []uuid.UUID
		mode lf.UserLockMode
	}{
		{"empty complete set", nil, lf.UserShare},
		{"nil principal", []uuid.UUID{uuid.Nil}, lf.UserShare},
		{"unknown mode", []uuid.UUID{user}, lf.UserLockMode(255)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := transaction(t, db)
			b, err := lf.AcquireWriterBarrierTx(ctx, tx)
			require.NoError(t, err)
			_, err = lf.LockUsersTx(ctx, tx, b, tc.ids, tc.mode)
			require.ErrorIs(t, err, lf.ErrContract)
		})
	}
	_, err := lf.AcquireWriterBarrierTx(ctx, nil)
	require.ErrorIs(t, err, lf.ErrContract)
	_, err = (lf.UserAuthoritySet{}).ForUser(user)
	require.ErrorIs(t, err, lf.ErrContract)
}

func TestWriterBarrierRejectsSnapshotIsolationBeforeAnyAuthority(t *testing.T) {
	db := database(t)
	for _, isolation := range []sql.IsolationLevel{sql.LevelReadUncommitted, sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(isolation.String(), func(t *testing.T) {
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: isolation})
			require.NoError(t, err)
			t.Cleanup(func() { err := tx.Rollback(); require.NoError(t, err) })
			_, err = lf.AcquireWriterBarrierTx(ctx, tx)
			reason(t, err, lf.ErrContract, lf.ReasonArgumentInvalid)
			// Refusal occurs before consuming the one-shot constructor marker.
			_, err = tx.Exec(`SET TRANSACTION ISOLATION LEVEL READ COMMITTED`)
			require.NoError(t, err)
			_, err = lf.AcquireWriterBarrierTx(ctx, tx)
			require.NoError(t, err)
		})
	}
}
