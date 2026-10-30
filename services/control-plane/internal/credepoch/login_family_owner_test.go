package credepoch

import (
	"context"
	"database/sql"
	"io"
	"sync"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func ownerFence(t *testing.T) (*Fence, *sql.DB, string) {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	user := testdb.CreateUser(t, db)
	return New(db, redistest.Client(t), logger.NewWithWriter(io.Discard)), db, user.String()
}

func ownerTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func ownerAuthority(t *testing.T, tx *sql.Tx, uid string) lf.UserAuthority {
	t.Helper()
	ctx := context.Background()
	barrier, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	users, err := lf.LockUsersTx(ctx, tx, barrier, []uuid.UUID{uuid.MustParse(uid)}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(uuid.MustParse(uid))
	require.NoError(t, err)
	return user
}

func ownerReason(t *testing.T, err error, reason lf.ErrorReason) {
	t.Helper()
	require.ErrorIs(t, err, lf.ErrContract)
	var classified *lf.Error
	require.ErrorAs(t, err, &classified)
	require.Equal(t, reason, classified.Reason())
}

// Sequence increments survive transaction rollback, so these BEFORE triggers
// observe even attempted mutations that a later SQL error would otherwise hide.
func ownerMutationObserver(t *testing.T, db *sql.DB) func() int64 {
	t.Helper()
	_, err := db.Exec(`CREATE SEQUENCE epoch_test_mutation_seq MINVALUE 0 START 0;
 CREATE FUNCTION epoch_test_observe() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM nextval('epoch_test_mutation_seq'); RETURN NEW; END $$;
 CREATE TRIGGER epoch_test_user BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION epoch_test_observe();
 CREATE TRIGGER epoch_test_parent BEFORE INSERT OR UPDATE ON credential_epoch_voice_ejections FOR EACH ROW EXECUTE FUNCTION epoch_test_observe()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`DROP TRIGGER epoch_test_user ON users; DROP TRIGGER epoch_test_parent ON credential_epoch_voice_ejections; DROP FUNCTION epoch_test_observe(); DROP SEQUENCE epoch_test_mutation_seq`)
		require.NoError(t, err)
	})
	return func() int64 {
		var value int64
		var called bool
		require.NoError(t, db.QueryRow(`SELECT last_value,is_called FROM epoch_test_mutation_seq`).Scan(&value, &called))
		if !called {
			return 0
		}
		return value + 1
	}
}

func TestLoginFamilyOwnerSingleUseRejectsBeforeSecondMutation(t *testing.T) {
	f, db, uid := ownerFence(t)
	ctx := context.Background()
	observe := ownerMutationObserver(t, db)
	op, err := f.Begin(ctx, uid)
	require.NoError(t, err)
	tx := ownerTx(t, db)
	owner, err := op.RotateTx(ctx, tx)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, owner.Generation)
	first := observe()
	require.Positive(t, first, "positive control: trigger observes actual rotation")
	other := ownerTx(t, db)
	for _, retry := range []*sql.Tx{tx, other, nil} {
		got, err := op.RotateTx(ctx, retry)
		ownerReason(t, err, lf.ReasonRotateRepeated)
		require.Equal(t, lf.EpochOwner{}, got)
		require.Equal(t, first, observe(), "repeat must not reach any mutating SQL")
	}
	require.NoError(t, tx.Commit())
	got, err := op.RotateTx(ctx, other)
	ownerReason(t, err, lf.ReasonRotateRepeated)
	require.Equal(t, lf.EpochOwner{}, got)
	require.Equal(t, first, observe())
}

func TestLoginFamilyOwnerConcurrentRotateHasExactlyOneWinner(t *testing.T) {
	f, db, uid := ownerFence(t)
	observe := ownerMutationObserver(t, db)
	op, err := f.Begin(context.Background(), uid)
	require.NoError(t, err)
	tx := ownerTx(t, db)
	const attempts = 12
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, err := op.RotateTx(context.Background(), tx); results <- err }()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			ownerReason(t, err, lf.ReasonRotateRepeated)
		}
	}
	require.Equal(t, 1, successes)
	// One parent insert and one users update; sequence counts SQL effects even if rolled back.
	require.EqualValues(t, 2, observe())
	require.NoError(t, tx.Commit())
}

func TestLoginFamilyOwnerFailedFirstAttemptNeverResets(t *testing.T) {
	for _, mode := range []string{"nil transaction", "wrong transaction", "SQL failure", "unknown user"} {
		t.Run(mode, func(t *testing.T) {
			f, db, uid := ownerFence(t)
			observe := ownerMutationObserver(t, db)
			ctx := context.Background()
			if mode == "unknown user" {
				uid = uuid.NewString()
			}
			op, err := f.Begin(ctx, uid)
			require.NoError(t, err)
			tx := ownerTx(t, db)
			attempt := tx
			switch mode {
			case "nil transaction":
				attempt = nil
			case "wrong transaction":
				require.NoError(t, op.CapturePreviousEpochTx(ctx, tx))
				attempt = ownerTx(t, db)
			case "SQL failure":
				_, err = db.Exec(`CREATE FUNCTION epoch_test_fail_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'epoch-test-injected'; END $$;
     CREATE TRIGGER epoch_test_fail BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION epoch_test_fail_update()`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := db.Exec(`DROP TRIGGER epoch_test_fail ON users; DROP FUNCTION epoch_test_fail_update()`)
					require.NoError(t, err)
				})
			}
			got, err := op.RotateTx(ctx, attempt)
			require.Error(t, err)
			require.Equal(t, lf.EpochOwner{}, got)
			first := observe()
			if mode == "SQL failure" {
				require.Positive(t, first, "injected failure must follow a parent write")
			} else {
				require.Zero(t, first)
			}
			// Definite rollback, a different transaction, and a valid context do not reset attempted.
			require.NoError(t, tx.Rollback())
			retry := ownerTx(t, db)
			got, err = op.RotateTx(ctx, retry)
			ownerReason(t, err, lf.ReasonRotateRepeated)
			require.Equal(t, lf.EpochOwner{}, got)
			require.Equal(t, first, observe())
			var parents int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections`).Scan(&parents))
			require.Zero(t, parents, "failed rotation cannot publish a durable parent")
		})
	}
}

func TestLoginFamilyOwnerCaptureIsCachedOnlyForExactTransaction(t *testing.T) {
	f, db, uid := ownerFence(t)
	ctx := context.Background()
	op, err := f.Begin(ctx, uid)
	require.NoError(t, err)
	ownerReason(t, op.CapturePreviousEpochTx(ctx, nil), lf.ReasonCapabilityInvalid)
	tx := ownerTx(t, db)
	require.NoError(t, op.CapturePreviousEpochTx(ctx, tx))
	other := ownerTx(t, db)
	ownerReason(t, op.CapturePreviousEpochTx(ctx, other), lf.ReasonCapabilityInvalid)
	// An aborted transaction rejects all SQL. Cached success here proves the second
	// capture does not requery it; a different pointer must still be refused.
	_, err = tx.Exec(`SELECT 1/0`)
	require.Error(t, err)
	require.NoError(t, op.CapturePreviousEpochTx(ctx, tx))
	ownerReason(t, op.CapturePreviousEpochTx(ctx, other), lf.ReasonCapabilityInvalid)
}

func TestLoginFamilyOwnerReturnedTupleOwnsFamiliesAndRetainsCompletedEvidence(t *testing.T) {
	f, db, uid := ownerFence(t)
	ctx := context.Background()
	// Retained family evidence intentionally survives users/parent deletion; remove
	// it explicitly before the baseline fixture's user cleanup.
	t.Cleanup(func() {
		_, err := testdb.ExecReplica(db, `TRUNCATE login_family_revocation_targets,login_family_sids,login_families,login_family_revocations CASCADE`)
		require.NoError(t, err)
	})
	tx := ownerTx(t, db)
	u := ownerAuthority(t, tx, uid)
	first, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	second, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	sid := uuid.New()
	require.NoError(t, lf.BindSIDTx(ctx, tx, first, sid, time.Now().Add(time.Hour).UTC().Truncate(time.Second)))
	selected, err := lf.ResolveTargetSIDTx(ctx, tx, u, sid)
	require.NoError(t, err)
	previous, err := lf.RevokeFamiliesTx(ctx, tx, selected, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonLogout})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var terminalAt time.Time
	require.NoError(t, db.QueryRow(`SELECT terminal_at FROM login_families WHERE family_id=$1`, first.Binding().FamilyID).Scan(&terminalAt))
	op, err := f.Begin(ctx, uid)
	require.NoError(t, err)
	tx = ownerTx(t, db)
	u = ownerAuthority(t, tx, uid)
	targets, err := lf.CaptureEpochTargetsTx(ctx, tx, u, "")
	require.NoError(t, err)
	owner, err := op.RotateTx(ctx, tx)
	require.NoError(t, err)
	var durable lf.EpochOwner
	require.NoError(t, tx.QueryRow(`SELECT generation,user_id,superseded_credential_epoch,credential_epoch FROM credential_epoch_voice_ejections WHERE user_id=$1`, uid).Scan(&durable.Generation, &durable.UserID, &durable.SupersededCredentialEpoch, &durable.CredentialEpoch))
	require.Equal(t, durable, owner)
	require.Equal(t, uuid.MustParse(uid), owner.UserID)
	require.Empty(t, owner.SupersededCredentialEpoch)
	require.Equal(t, op.NewEpochValue(), owner.CredentialEpoch)
	for _, field := range []string{"generation", "user", "old", "new"} {
		forged := owner
		switch field {
		case "generation":
			forged.Generation = uuid.New()
		case "user":
			forged.UserID = uuid.New()
		case "old":
			forged.SupersededCredentialEpoch = testPredecessorEpoch
		case "new":
			forged.CredentialEpoch = testPredecessorEpoch
		}
		_, err = lf.RevokeFamiliesForEpochTx(ctx, tx, targets, forged)
		require.ErrorIs(t, err, lf.ErrContract)
	}
	result, err := lf.RevokeFamiliesForEpochTx(ctx, tx, targets, owner)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	require.Len(t, result.Owners, 2)
	require.NoError(t, tx.Commit())
	var auth uuid.NullUUID
	var generation uuid.UUID
	var retainedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT auth_operation_id,epoch_generation FROM login_families WHERE family_id=$1`, second.Binding().FamilyID).Scan(&auth, &generation))
	require.False(t, auth.Valid)
	require.Equal(t, owner.Generation, generation)
	var original uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT auth_operation_id,terminal_at FROM login_families WHERE family_id=$1`, first.Binding().FamilyID).Scan(&original, &retainedAt))
	require.Equal(t, previous.Owners[0].AuthOperationID, original)
	require.True(t, terminalAt.Equal(retainedAt), "old terminal age is immutable")
	var operations int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM login_family_revocations`).Scan(&operations))
	require.Equal(t, 1, operations, "rotation creates no auth parent")
	_, err = db.Exec(`DELETE FROM credential_epoch_voice_ejections WHERE generation=$1`, owner.Generation)
	require.NoError(t, err)
	tx = ownerTx(t, db)
	u = ownerAuthority(t, tx, uid)
	all, err := lf.CaptureUserTargetsTx(ctx, tx, u, nil)
	require.NoError(t, err)
	replay, err := lf.RevokeFamiliesTx(ctx, tx, all, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.Zero(t, replay.NewlyTerminalFamilies)
	require.Equal(t, result.Owners, replay.Owners, "parent deletion retains completion locator")
	require.NoError(t, tx.Commit())
}

func TestLoginFamilyOwnerUsesReturningGenerationAndPreservesExistingParentAge(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "database generated identity", true: "existing parent"}[existing], func(t *testing.T) {
			f, db, uid := ownerFence(t)
			ctx := context.Background()
			op, err := f.Begin(ctx, uid)
			require.NoError(t, err)
			var expected uuid.UUID
			var created time.Time
			if existing {
				expected = uuid.New()
				_, err = db.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,superseded_credential_epoch,credential_epoch,generation,created_at) VALUES($1,'',$2,$3,clock_timestamp()-interval '2 hours')`, uid, op.NewEpochValue(), expected)
				require.NoError(t, err)
				require.NoError(t, db.QueryRow(`SELECT created_at FROM credential_epoch_voice_ejections WHERE generation=$1`, expected).Scan(&created))
			} else {
				// Database-side identity substitution distinguishes actual RETURNING from a
				// separately generated locator that merely happens to look like a UUID.
				_, err = db.Exec(`CREATE FUNCTION epoch_test_generation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.generation := 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee'::uuid; RETURN NEW; END $$;
     CREATE TRIGGER epoch_test_generation BEFORE INSERT ON credential_epoch_voice_ejections FOR EACH ROW EXECUTE FUNCTION epoch_test_generation()`)
				require.NoError(t, err)
				expected = uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
				t.Cleanup(func() {
					_, err := db.Exec(`DROP TRIGGER epoch_test_generation ON credential_epoch_voice_ejections; DROP FUNCTION epoch_test_generation()`)
					require.NoError(t, err)
				})
			}
			tx := ownerTx(t, db)
			owner, err := op.RotateTx(ctx, tx)
			require.NoError(t, err)
			require.Equal(t, expected, owner.Generation)
			require.NoError(t, tx.Commit())
			var actual lf.EpochOwner
			var age time.Time
			require.NoError(t, db.QueryRow(`SELECT generation,user_id,superseded_credential_epoch,credential_epoch,created_at FROM credential_epoch_voice_ejections WHERE user_id=$1`, uid).Scan(&actual.Generation, &actual.UserID, &actual.SupersededCredentialEpoch, &actual.CredentialEpoch, &age))
			require.Equal(t, actual, owner)
			if existing {
				require.True(t, created.Equal(age), "retry cannot reset original operation age")
			}
		})
	}
}

func TestLoginFamilyOwnerConflictingParentIsNotReplaced(t *testing.T) {
	f, db, uid := ownerFence(t)
	ctx := context.Background()
	original := uuid.New()
	_, err := db.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,superseded_credential_epoch,credential_epoch,generation) VALUES($1,'',$2,$3)`, uid, testPredecessorEpoch, original)
	require.NoError(t, err)
	op, err := f.Begin(ctx, uid)
	require.NoError(t, err)
	tx := ownerTx(t, db)
	got, err := op.RotateTx(ctx, tx)
	require.ErrorIs(t, err, lf.ErrContract)
	require.Equal(t, lf.EpochOwner{}, got)
	require.NoError(t, tx.Rollback())
	var generation uuid.UUID
	var replacement string
	require.NoError(t, db.QueryRow(`SELECT generation,credential_epoch FROM credential_epoch_voice_ejections WHERE user_id=$1`, uid).Scan(&generation, &replacement))
	require.Equal(t, original, generation)
	require.Equal(t, testPredecessorEpoch, replacement)
}

func TestLoginFamilyOwnerFamilyTerminalizationRollsBackWithEpoch(t *testing.T) {
	f, db, uid := ownerFence(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, err := testdb.ExecReplica(db, `TRUNCATE login_family_revocation_targets,login_family_sids,login_families,login_family_revocations CASCADE`)
		require.NoError(t, err)
	})
	tx := ownerTx(t, db)
	u := ownerAuthority(t, tx, uid)
	family, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	sid := uuid.New()
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, lf.BindSIDTx(ctx, tx, family, sid, expiry))
	_, err = tx.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, sid, uid, sid.String(), expiry)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	op, err := f.Begin(ctx, uid)
	require.NoError(t, err)
	tx = ownerTx(t, db)
	u = ownerAuthority(t, tx, uid)
	targets, err := lf.CaptureEpochTargetsTx(ctx, tx, u, "")
	require.NoError(t, err)
	owner, err := op.RotateTx(ctx, tx)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesForEpochTx(ctx, tx, targets, owner)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.NewlyTerminalFamilies)
	require.EqualValues(t, 1, result.RevokedRefreshRows)
	_, err = tx.Exec(`SELECT 1/0`)
	require.Error(t, err)
	require.Error(t, tx.Commit())
	var parents, terminal, revoked int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections`).Scan(&parents))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`).Scan(&terminal))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`).Scan(&revoked))
	require.Zero(t, parents)
	require.Zero(t, terminal)
	require.Zero(t, revoked)
	_, err = lf.ResolveVerifiedAccess(ctx, db, lf.AccessIdentity{UserID: uuid.MustParse(uid), SID: sid, AccessExpiresAt: expiry})
	require.NoError(t, err, "rollback preserves productive predecessor authority")
}

func TestLoginFamilyOwnerZeroOperationRefusesWithoutMutation(t *testing.T) {
	_, db, _ := ownerFence(t)
	observe := ownerMutationObserver(t, db)
	ctx := context.Background()
	tx := ownerTx(t, db)
	var absent *Op
	got, err := absent.RotateTx(ctx, tx)
	ownerReason(t, err, lf.ReasonCapabilityInvalid)
	require.Equal(t, lf.EpochOwner{}, got)
	ownerReason(t, absent.CapturePreviousEpochTx(ctx, tx), lf.ReasonCapabilityInvalid)
	zero := &Op{}
	got, err = zero.RotateTx(ctx, tx)
	require.Error(t, err)
	require.Equal(t, lf.EpochOwner{}, got)
	got, err = zero.RotateTx(ctx, tx)
	ownerReason(t, err, lf.ReasonRotateRepeated)
	require.Equal(t, lf.EpochOwner{}, got)
	require.Zero(t, observe())
}
