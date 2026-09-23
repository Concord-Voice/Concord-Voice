package dmblock

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestReconcileConversation_EnqueuesEveryRemovedMemberAtomically(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	_, err := db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, conversation, a)
	require.NoError(t, err)
	reconciler := New(db, nil)
	require.NoError(t, reconciler.reconcileConversation(context.Background(), conversation.String(), claimedObligation{a: a, b: b, removeA: true, removeB: true, operationID: operation}))
	var ejections, plans int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_voice_ejections WHERE conversation_id = $1`, conversation).Scan(&ejections))
	require.Equal(t, 2, ejections, "outbox includes removed members without a voice row")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1 AND category = 'private_call'`, a).Scan(&plans))
	require.Zero(t, plans, "a provisional member has no active-call evidence")
}

func TestReconcileDue_DrainsCommittedVoiceEjectionsExactly(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	type voiceCall struct {
		conversation string
		user         uuid.UUID
	}
	calls := make(chan voiceCall, 2)
	reconciler := New(db, nil)
	reconciler.SetVoiceEject(func(_ context.Context, gotConversation string, userID uuid.UUID) error {
		calls <- voiceCall{conversation: gotConversation, user: userID}
		return nil
	})
	require.NoError(t, reconciler.reconcileConversation(context.Background(), conversation.String(), claimedObligation{a: a, b: b, removeA: true, removeB: true, operationID: operation}))
	_, err := reconciler.ReconcileDue(context.Background(), 10)
	require.NoError(t, err)
	delivered := make([]uuid.UUID, 0, 2)
	require.Len(t, calls, 2)
	for range 2 {
		call := <-calls
		require.Equal(t, conversation.String(), call.conversation)
		delivered = append(delivered, call.user)
	}
	require.ElementsMatch(t, []uuid.UUID{a, b}, delivered)
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_voice_ejections WHERE conversation_id = $1`, conversation).Scan(&remaining))
	require.Zero(t, remaining)
}

func TestReconcileDue_FailedEjectionRetriesAfterPairMarkerIsGone(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	attempts := map[uuid.UUID]int{}
	reconciler := New(db, nil)
	reconciler.SetVoiceEject(func(_ context.Context, _ string, userID uuid.UUID) error {
		attempts[userID]++
		if userID == a && attempts[userID] == 1 {
			return errors.New("media peer unavailable")
		}
		return nil
	})
	_, err := reconciler.ReconcileDue(context.Background(), 10)
	require.NoError(t, err)
	var markerCount, outboxAttempts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, a, b).Scan(&markerCount))
	require.Zero(t, markerCount)
	require.NoError(t, db.QueryRow(`SELECT attempts FROM dm_block_voice_ejections WHERE conversation_id = $1 AND user_id = $2`, conversation, a).Scan(&outboxAttempts))
	require.Equal(t, 1, outboxAttempts)
	require.Equal(t, 1, attempts[a])
	require.Equal(t, 1, attempts[b])
	_, err = db.Exec(`UPDATE dm_block_voice_ejections SET reconcile_after = clock_timestamp() WHERE conversation_id = $1`, conversation)
	require.NoError(t, err)
	_, err = reconciler.ReconcileDue(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 2, attempts[a])
	require.Equal(t, 1, attempts[b])
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_voice_ejections WHERE conversation_id = $1`, conversation).Scan(&outboxAttempts))
	require.Zero(t, outboxAttempts)
}

func TestReconcileDue_NilVoiceEjectRetainsOutboxForRetry(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	reconciler := New(db, nil)
	require.NoError(t, reconciler.reconcileConversation(context.Background(), conversation.String(), claimedObligation{a: a, b: b, removeA: true, removeB: true, operationID: operation}))
	_, err := reconciler.ReconcileDue(context.Background(), 10)
	require.NoError(t, err)
	var attempts int
	var failureClass string
	require.NoError(t, db.QueryRow(`SELECT attempts, failure_class FROM dm_block_voice_ejections WHERE conversation_id = $1`, conversation).Scan(&attempts, &failureClass))
	require.Equal(t, 1, attempts)
	require.Equal(t, "delivery", failureClass)
}

func TestReconcileDue_DrainsSecurityEjectionsBeforePairCancellation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	_, err := db.Exec(`DELETE FROM credential_epoch_voice_ejections`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_block_voice_ejections`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_block_reconciliations`)
	require.NoError(t, err)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)

	voiceConversation, voiceUser := uuid.New(), dbtest.CreateUser(t, db)
	_, err = db.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, reconcile_after) VALUES ($1, $2, clock_timestamp())`, voiceConversation, voiceUser)
	require.NoError(t, err)
	credentialUser := dbtest.CreateUser(t, db)
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	_, err = db.Exec(`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, generation, reconcile_after) VALUES ($1, $2, $3, $4, clock_timestamp())`, credentialUser, newEpoch, oldEpoch, uuid.New())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconciler := New(db, nil)
	reconciler.SetReconciliationNotifier(cancelAfterParticipantRemoval{cancel: cancel})
	voiceDelivered := make(chan struct{}, 1)
	credentialDelivered := make(chan uuid.UUID, 1)
	reconciler.SetVoiceEject(func(_ context.Context, gotConversation string, userID uuid.UUID) error {
		if gotConversation == voiceConversation.String() && userID == voiceUser {
			voiceDelivered <- struct{}{}
		}
		return nil
	})
	reconciler.SetCredentialEpochEject(func(_ context.Context, userID uuid.UUID, _, _ string) error {
		credentialDelivered <- userID
		return nil
	})

	_, err = reconciler.ReconcileDue(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case got := <-credentialDelivered:
		require.Equal(t, credentialUser, got)
	default:
		t.Fatal("credential ejection was not delivered before pair cancellation")
	}
	select {
	case <-voiceDelivered:
	default:
		t.Fatal("DM voice ejection was not delivered before pair cancellation")
	}
}

func TestReconcileDue_SecurityEjectionDrainsStartConcurrently(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	_, err := db.Exec(`DELETE FROM credential_epoch_voice_ejections`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_block_voice_ejections`)
	require.NoError(t, err)
	voiceConversation, voiceUser := uuid.New(), dbtest.CreateUser(t, db)
	_, err = db.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, reconcile_after) VALUES ($1, $2, clock_timestamp())`, voiceConversation, voiceUser)
	require.NoError(t, err)
	credentialUser := dbtest.CreateUser(t, db)
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	_, err = db.Exec(`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, generation, reconcile_after) VALUES ($1, $2, $3, $4, clock_timestamp())`, credentialUser, newEpoch, oldEpoch, uuid.New())
	require.NoError(t, err)

	credentialStarted := make(chan context.Context, 1)
	voiceStarted := make(chan context.Context, 1)
	voiceReady := make(chan struct{})
	releaseCredential := make(chan struct{})
	reconciler := New(db, nil)
	reconciler.SetCredentialEpochEject(func(ctx context.Context, _ uuid.UUID, _, _ string) error {
		credentialStarted <- ctx
		select {
		case <-voiceReady:
			return nil
		case <-releaseCredential:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	reconciler.SetVoiceEject(func(ctx context.Context, _ string, _ uuid.UUID) error {
		voiceStarted <- ctx
		close(voiceReady)
		return nil
	})

	done := make(chan error, 1)
	outer, cancelOuter := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelOuter()
	outerDeadline, outerHasDeadline := outer.Deadline()
	require.True(t, outerHasDeadline)
	go func() {
		_, reconcileErr := reconciler.ReconcileDue(outer, 1)
		done <- reconcileErr
	}()
	var credentialCtx, voiceCtx context.Context
	select {
	case credentialCtx = <-credentialStarted:
	case <-time.After(time.Second):
		t.Fatal("credential ejection callback was not invoked")
	}
	voiceObserved := false
	select {
	case voiceCtx = <-voiceStarted:
		voiceObserved = true
	case <-time.After(time.Second):
	}
	close(releaseCredential)
	if voiceObserved {
		require.NotNil(t, voiceCtx)
	}
	if err := <-done; voiceObserved {
		require.NoError(t, err)
	}
	require.True(t, voiceObserved, "DM voice ejection was blocked by credential delivery")
	credentialDeadline, credentialHasDeadline := credentialCtx.Deadline()
	voiceDeadline, voiceHasDeadline := voiceCtx.Deadline()
	require.True(t, credentialHasDeadline, "credential drain must have a bounded context")
	require.True(t, voiceHasDeadline, "DM voice drain must have a bounded context")
	require.False(t, credentialDeadline.After(outerDeadline))
	require.False(t, voiceDeadline.After(outerDeadline))
	require.NotEqual(t, credentialCtx, voiceCtx, "security drains must not share a context")
}

func TestReconcileDue_CanceledOuterContextSkipsSecurityEjections(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	_, err := db.Exec(`DELETE FROM credential_epoch_voice_ejections`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_block_voice_ejections`)
	require.NoError(t, err)
	voiceConversation, voiceUser := uuid.New(), dbtest.CreateUser(t, db)
	_, err = db.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, reconcile_after) VALUES ($1, $2, clock_timestamp())`, voiceConversation, voiceUser)
	require.NoError(t, err)
	credentialUser := dbtest.CreateUser(t, db)
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	_, err = db.Exec(`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, generation, reconcile_after) VALUES ($1, $2, $3, $4, clock_timestamp())`, credentialUser, newEpoch, oldEpoch, uuid.New())
	require.NoError(t, err)

	credentialCalled := make(chan struct{}, 1)
	voiceCalled := make(chan struct{}, 1)
	reconciler := New(db, nil)
	reconciler.SetCredentialEpochEject(func(context.Context, uuid.UUID, string, string) error {
		credentialCalled <- struct{}{}
		return nil
	})
	reconciler.SetVoiceEject(func(context.Context, string, uuid.UUID) error {
		voiceCalled <- struct{}{}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reconciler.ReconcileDue(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-credentialCalled:
		t.Fatal("credential ejection callback ran for canceled reconciliation")
	default:
	}
	select {
	case <-voiceCalled:
		t.Fatal("DM voice ejection callback ran for canceled reconciliation")
	default:
	}
}

type cancelAfterParticipantRemoval struct {
	cancel context.CancelFunc
}

func (n cancelAfterParticipantRemoval) ParticipantRemoved(context.Context, string, uuid.UUID) error {
	n.cancel()
	return nil
}

func (cancelAfterParticipantRemoval) GroupDeleted(context.Context, string, []uuid.UUID) error {
	return nil
}

func (cancelAfterParticipantRemoval) RoleChanged(context.Context, string, uuid.UUID) error {
	return nil
}

func (cancelAfterParticipantRemoval) KeyRevocation(context.Context, string, int, string) error {
	return nil
}

func TestDeliverCredentialEpochVoiceEjection_ExactGenerationLifecycle(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db)
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret

	insert := func() uuid.UUID {
		t.Helper()
		generation := uuid.New()
		_, err := db.Exec(`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, generation, reconcile_after) VALUES ($1, $2, $3, $4, clock_timestamp())`, userID, newEpoch, oldEpoch, generation)
		require.NoError(t, err)
		return generation
	}

	insert()
	reconciler := New(db, nil)
	var delivered uuid.UUID
	reconciler.SetCredentialEpochEject(func(_ context.Context, got uuid.UUID, gotNew, gotOld string) error {
		delivered = got
		require.Equal(t, userID, got)
		require.Equal(t, newEpoch, gotNew)
		require.Equal(t, oldEpoch, gotOld)
		return nil
	})
	require.NoError(t, reconciler.DeliverCredentialEpochVoiceEjection(context.Background(), userID.String(), newEpoch, oldEpoch))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections WHERE user_id = $1`, userID).Scan(&count))
	require.Zero(t, count)
	require.Equal(t, userID, delivered)

	insert()
	nilReconciler := New(db, nil)
	require.NoError(t, nilReconciler.DeliverCredentialEpochVoiceEjection(context.Background(), userID.String(), newEpoch, oldEpoch))
	var nilFailure string
	require.NoError(t, db.QueryRow(`SELECT failure_class FROM credential_epoch_voice_ejections WHERE user_id = $1`, userID).Scan(&nilFailure))
	require.Equal(t, "delivery", nilFailure)
	_, err := db.Exec(`DELETE FROM credential_epoch_voice_ejections WHERE user_id = $1`, userID)
	require.NoError(t, err)
	insert()
	reconciler.SetCredentialEpochEject(func(context.Context, uuid.UUID, string, string) error { return errors.New("media unavailable") })
	require.NoError(t, reconciler.DeliverCredentialEpochVoiceEjection(context.Background(), userID.String(), newEpoch, oldEpoch))
	var failure string
	require.NoError(t, db.QueryRow(`SELECT failure_class FROM credential_epoch_voice_ejections WHERE user_id = $1`, userID).Scan(&failure))
	require.Equal(t, "delivery", failure)
}

func TestDeliverCredentialEpochVoiceEjection_StaleGenerationCannotConsumeReplacement(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db)
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	firstGeneration := uuid.New()
	_, err := db.Exec(`INSERT INTO credential_epoch_voice_ejections (user_id, credential_epoch, superseded_credential_epoch, generation, reconcile_after) VALUES ($1, $2, $3, $4, clock_timestamp())`, userID, newEpoch, oldEpoch, firstGeneration)
	require.NoError(t, err)
	inserted := false
	var count int
	reconciler := New(db, nil)
	reconciler.SetCredentialEpochEject(func(_ context.Context, _ uuid.UUID, _, _ string) error {
		if !inserted {
			inserted = true
			_, err := db.Exec(`UPDATE credential_epoch_voice_ejections SET generation = $1, attempts = 0, failure_class = NULL, reconcile_after = clock_timestamp() WHERE user_id = $2 AND superseded_credential_epoch = $3`, uuid.New(), userID, oldEpoch)
			require.NoError(t, err)
		}
		return nil
	})
	require.NoError(t, reconciler.DeliverCredentialEpochVoiceEjection(context.Background(), userID.String(), newEpoch, oldEpoch))
	require.Equal(t, true, inserted)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections WHERE user_id = $1 AND superseded_credential_epoch = $2`, userID, oldEpoch).Scan(&count))
	require.Equal(t, 1, count)

	// A stale failure must not delay or annotate the replacement generation.
	inserted = false
	_, err = db.Exec(`UPDATE credential_epoch_voice_ejections SET reconcile_after = clock_timestamp() WHERE user_id = $1`, userID)
	require.NoError(t, err)
	reconciler.SetCredentialEpochEject(func(_ context.Context, _ uuid.UUID, _, _ string) error {
		if !inserted {
			inserted = true
			_, err := db.Exec(`UPDATE credential_epoch_voice_ejections SET generation = $1, attempts = 0, failure_class = NULL, reconcile_after = clock_timestamp() WHERE user_id = $2 AND superseded_credential_epoch = $3`, uuid.New(), userID, oldEpoch)
			require.NoError(t, err)
		}
		return errors.New("stale delivery failed")
	})
	require.NoError(t, reconciler.DeliverCredentialEpochVoiceEjection(context.Background(), userID.String(), newEpoch, oldEpoch))
	var attempts int
	var failure sql.NullString
	require.NoError(t, db.QueryRow(`SELECT attempts, failure_class FROM credential_epoch_voice_ejections WHERE user_id = $1 AND superseded_credential_epoch = $2`, userID, oldEpoch).Scan(&attempts, &failure))
	require.Zero(t, attempts)
	require.False(t, failure.Valid)
}

func TestDrainDueVoiceEjections_StaleCallbackCannotConsumeOrDelayReenqueue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		callback error
	}{
		{name: "acknowledgement", callback: nil},
		{name: "retry", callback: errors.New("media unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			conversation, user := uuid.New(), dbtest.CreateUser(t, db)
			_, err := db.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, reconcile_after) VALUES ($1, $2, clock_timestamp())`, conversation, user)
			require.NoError(t, err)

			reconciler := New(db, nil)
			reconciler.SetVoiceEject(func(_ context.Context, _ string, _ uuid.UUID) error {
				tx, beginErr := db.BeginTx(context.Background(), nil)
				require.NoError(t, beginErr)
				require.NoError(t, EnqueueVoiceEjectionsTx(context.Background(), tx, conversation.String(), []uuid.UUID{user}))
				require.NoError(t, tx.Commit())
				return tc.callback
			})

			require.NoError(t, reconciler.drainDueVoiceEjections(context.Background(), 1))
			var attempts int
			var failure sql.NullString
			var due bool
			require.NoError(t, db.QueryRow(`
				SELECT attempts, failure_class, reconcile_after <= clock_timestamp()
				FROM dm_block_voice_ejections
				WHERE conversation_id = $1 AND user_id = $2`, conversation, user).Scan(&attempts, &failure, &due))
			require.Zero(t, attempts, "new generation keeps its fresh attempt count")
			require.False(t, failure.Valid, "new generation keeps its fresh failure state")
			require.True(t, due, "new generation keeps its immediate retry schedule")
		})
	}
}

func TestReconcileConversationTx_RollbackLeavesNoOutboxOrPlan(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback test transaction: %v", err)
		}
	}()
	err = New(db, nil).reconcileConversationTx(context.Background(), tx, conversation.String(), claimedObligation{
		a:           a,
		b:           b,
		removeA:     true,
		removeB:     true,
		operationID: operation,
	}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	var outbox, plans, voices int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_voice_ejections WHERE conversation_id = $1`, conversation).Scan(&outbox))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1 AND category = 'private_call'`, a).Scan(&plans))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, conversation, a).Scan(&voices))
	require.Zero(t, outbox)
	require.Zero(t, plans)
	require.Equal(t, 1, voices)
}

func TestReconcileConversationTx_RequiresBlockedPairCoMembership(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	_, err := db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversation, b)
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.ErrorIs(t, New(db, nil).reconcileConversationTx(context.Background(), tx, conversation.String(), claimedObligation{
		a:           a,
		b:           b,
		removeA:     true,
		operationID: operation,
	}, nil, nil), ErrMembershipChanged)
	require.NoError(t, tx.Rollback())

	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversation, a).Scan(&remaining))
	require.Equal(t, 1, remaining, "worker must not remove a user after the pair is no longer co-members")
}

func insertReconcileConversationFixture(t *testing.T, db *sql.DB, conversation uuid.UUID, a, b, survivor, operation uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3), ($1, $4)`, conversation, a, b, survivor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b) VALUES ($1, $2, $3, true, true)`, a, b, operation)
	require.NoError(t, err)
}

func canonicalPair(a, b, survivor uuid.UUID) (uuid.UUID, uuid.UUID, uuid.UUID) {
	if a.String() > b.String() {
		a, b = b, a
	}
	return a, b, survivor
}
