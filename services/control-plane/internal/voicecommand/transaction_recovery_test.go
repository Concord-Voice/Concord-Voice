package voicecommand

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

func TestVoiceOutboxRetirement_PreparationSQLFailureLeavesNoPartialProof(t *testing.T) {
	for _, tc := range []struct {
		name, statement, operation string
	}{
		{"cursor unavailable", "FROM voice_command_sequence", "lock outbox retirement cursor"},
		{"transport receipt unavailable", "FROM voice_command_transport", "read retirement transport receipt"},
		{"publisher lease read unavailable", "SELECT EXISTS (SELECT 1 FROM voice_command_outbox", "check live outbox publication claim"},
		{"proof insert unavailable", "INSERT INTO voice_command_retirement", "prepare outbox retirement proof"},
		{"pending checkpoint unavailable", "UPDATE voice_command_sequence", "record pending outbox retirement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, repository, receipt := retirementFixture(t, 4, 3)
			hook, wrapped := stmthook.Open(t)
			hook.Arm([]string{tc.statement}, nil, stmthook.ErrInjected)
			intent, err := NewRepository(wrapped).PrepareRetirement(context.Background(), receipt, retirementConsumerBirth, 3)
			require.ErrorIs(t, err, stmthook.ErrInjected)
			require.ErrorContains(t, err, tc.operation)
			assert.Nil(t, intent)
			seen, betweenErr := hook.Report()
			require.NoError(t, betweenErr)
			require.Equal(t, 1, seen)
			state, err := repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			assert.Nil(t, state.Pending)
			assert.Zero(t, state.RetiredThrough)
			assert.Zero(t, retirementProofCount(t, db), "even a failure after proof insertion rolls back the whole preparation")
			requireOutboxCursor(t, db, 4, 3)
			assert.Equal(t, 4, outboxCount(t, db))

			prepared := retirementPrepare(t, repository, receipt, 3)
			require.NoError(t, repository.CompleteRetirement(context.Background(), prepared))
			assert.Equal(t, "4", outboxClaim(t, repository).Command.Sequence, "a later worker can prepare, finish, and resume publication")
		})
	}
}

func TestVoiceOutboxRetirement_CompletionSQLFailureRetainsExactPendingProof(t *testing.T) {
	for _, tc := range []struct {
		name, statement, operation string
	}{
		{"cursor unavailable", "FROM voice_command_sequence", "lock outbox retirement cursor"},
		{"pending proof unavailable", "FROM voice_command_retirement", "read pending outbox retirement proof"},
		{"transport receipt unavailable", "FROM voice_command_transport", "read retirement transport receipt"},
		{"proof completion unavailable", "UPDATE voice_command_retirement", "complete outbox retirement proof"},
		{"retired checkpoint unavailable", "UPDATE voice_command_sequence", "advance outbox retirement checkpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, repository, receipt := retirementFixture(t, 4, 3)
			intent := retirementPrepare(t, repository, receipt, 3)
			hook, wrapped := stmthook.Open(t)
			hook.Arm([]string{tc.statement}, nil, stmthook.ErrInjected)
			err := NewRepository(wrapped).CompleteRetirement(context.Background(), intent)
			require.ErrorIs(t, err, stmthook.ErrInjected)
			require.ErrorContains(t, err, tc.operation)
			seen, betweenErr := hook.Report()
			require.NoError(t, betweenErr)
			require.Equal(t, 1, seen)
			state, err := repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			require.NotNil(t, state.Pending)
			assert.Equal(t, intent, *state.Pending)
			assert.Zero(t, state.RetiredThrough)
			assert.False(t, retirementCompleted(t, db, intent.ID), "the proof and checkpoint commit together")
			blocked, err := repository.Claim(context.Background(), uuid.New(), claimLease)
			require.NoError(t, err)
			assert.Nil(t, blocked, "a failed checkpoint retains the publication barrier")
			require.NoError(t, repository.CompleteRetirement(context.Background(), intent))
			assert.True(t, retirementCompleted(t, db, intent.ID))
			assert.Equal(t, "4", outboxClaim(t, repository).Command.Sequence)
			assert.Equal(t, 1, retirementProofCount(t, db), "recovery reuses the prepared proof")
		})
	}
}

func TestVoiceOutboxRetirement_UnavailableCompletedProofBlocksPublicationUntilReadRecovers(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 4, 3)
	intent := retirementPrepare(t, repository, receipt, 3)
	require.NoError(t, repository.CompleteRetirement(context.Background(), intent))
	hook, wrapped := stmthook.Open(t)
	hook.Arm([]string{"FROM voice_command_retirement"}, nil, stmthook.ErrInjected)
	claim, err := NewRepository(wrapped).Claim(context.Background(), uuid.New(), claimLease)
	require.ErrorIs(t, err, stmthook.ErrInjected)
	require.ErrorContains(t, err, "read outbox retirement checkpoint proof")
	assert.Nil(t, claim)
	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 1, seen)
	assert.False(t, outboxDelivery(t, db, 4).Token.Valid)
	assert.True(t, retirementCompleted(t, db, intent.ID))
	state, err := repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Equal(t, intent.Through, state.RetiredThrough)
	assert.Nil(t, state.Pending)
	assert.Equal(t, intent.Through, outboxClaim(t, repository).RetiredThrough)
}

func TestVoiceOutboxRetirement_LostCompletionReplyAndUnavailableConfirmationRemainRecoverable(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 4, 3)
	intent := retirementPrepare(t, repository, receipt, 3)
	hook, wrapped := stmthook.Open(t)
	// The snapshot after the lost commit reply reads the completed checkpoint
	// first; fault only its subsequent exact-intent confirmation read.
	hook.Arm([]string{"UPDATE voice_command_sequence", "FROM voice_command_retirement", "FROM voice_command_retirement"}, nil, stmthook.ErrInjected)
	hook.ArmCommit(true, stmthook.ErrInjected)
	err := NewRepository(wrapped).CompleteRetirement(context.Background(), intent)
	require.ErrorIs(t, err, stmthook.ErrInjected)
	require.ErrorContains(t, err, "commit completed outbox retirement")
	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 3, seen, "the exact confirmation read, after the committed checkpoint, must be unavailable")
	state, err := repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Nil(t, state.Pending)
	assert.Equal(t, intent.Through, state.RetiredThrough)
	assert.True(t, retirementCompleted(t, db, intent.ID))
	require.NoError(t, repository.CompleteRetirement(context.Background(), intent), "a successor confirms the same completed proof")
	assert.Equal(t, 1, retirementProofCount(t, db))
	assert.Equal(t, "4", outboxClaim(t, repository).Command.Sequence)
}

func TestVoiceOutboxRetirement_RetryCommitAmbiguityNeverRemovesPublicationBarrier(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry rolled back", true: "retry committed with lost reply"}[committed], func(t *testing.T) {
			db, repository, receipt := retirementFixture(t, 4, 3)
			intent := retirementPrepare(t, repository, receipt, 3)
			hook, wrapped := stmthook.Open(t)
			hook.ArmCommit(committed, stmthook.ErrInjected)
			err := NewRepository(wrapped).RetryRetirement(context.Background(), intent, "broker_unavailable", 30*time.Second)
			require.ErrorIs(t, err, stmthook.ErrInjected)
			require.ErrorContains(t, err, "commit pending outbox retirement retry")
			state, err := repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			require.NotNil(t, state.Pending)
			assert.Equal(t, intent.ID, state.Pending.ID)
			assert.Equal(t, intent.Through, state.Pending.Through)
			if committed {
				assert.Equal(t, intent.Attempts+1, state.Pending.Attempts)
				assert.Equal(t, "broker_unavailable", state.Pending.FailureClass)
				assert.False(t, state.Pending.Due)
			} else {
				assert.Equal(t, intent, *state.Pending)
			}
			blocked, err := repository.Claim(context.Background(), uuid.New(), claimLease)
			require.NoError(t, err)
			assert.Nil(t, blocked)
			assert.Zero(t, state.RetiredThrough)
			assert.False(t, retirementCompleted(t, db, intent.ID))
			require.NoError(t, repository.CompleteRetirement(context.Background(), intent))
			assert.Equal(t, "4", outboxClaim(t, repository).Command.Sequence)
		})
	}
}

func TestVoiceOutboxRepository_PublicationSQLFailureRollsBackReceiptCursorAndAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, statement, operation string
	}{
		{"owned head unavailable", "FROM voice_command_outbox WHERE sequence", "read owned outbox head"},
		{"receipt unavailable", "UPDATE voice_command_outbox", "record outbox publication receipt"},
		{"publication cursor unavailable", "UPDATE voice_command_sequence", "advance outbox publication cursor"},
		{"admission release unavailable", "DELETE FROM voice_command_admission", "release published outbox admission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			actor := uuid.New()
			command := admissionEnqueue(t, db, admissionSpecs(1, capacityMarker().UserID), actor)[0]
			repository := NewRepository(db)
			claim := outboxClaim(t, repository)
			before := outboxDelivery(t, db, 1)
			hook, wrapped := stmthook.Open(t)
			hook.Arm([]string{tc.statement}, nil, stmthook.ErrInjected)
			err := NewRepository(wrapped).MarkPublished(context.Background(), claim, outboxReceipt(1))
			require.ErrorIs(t, err, stmthook.ErrInjected)
			require.ErrorContains(t, err, tc.operation)
			seen, betweenErr := hook.Report()
			require.NoError(t, betweenErr)
			require.Equal(t, 1, seen)
			assert.Equal(t, before, outboxDelivery(t, db, 1))
			assert.Equal(t, 1, admissionCount(t, db, actor), "a rolled-back receipt cannot release the outstanding quota")
			requireOutboxCursor(t, db, 1, 0)
			_, err = repository.Published(context.Background(), 1)
			require.ErrorIs(t, err, sql.ErrNoRows)

			require.NoError(t, repository.MarkPublished(context.Background(), claim, outboxReceipt(1)), "the same live owner can retry the uncommitted receipt")
			proof, err := repository.Published(context.Background(), 1)
			require.NoError(t, err)
			assert.Equal(t, command, proof)
			assert.Zero(t, admissionCount(t, db, actor))
			requireOutboxCursor(t, db, 1, 1)
		})
	}
}

func TestVoiceOutboxRepository_ClaimWriteFailureLeavesHeadForSuccessor(t *testing.T) {
	for _, commitFault := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease write unavailable", true: "lease transaction rolled back"}[commitFault], func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			before := outboxDelivery(t, db, 1)
			hook, wrapped := stmthook.Open(t)
			operation := "lease outbox command head"
			if commitFault {
				hook.ArmCommit(false, stmthook.ErrInjected)
				operation = "commit outbox command claim"
			} else {
				hook.Arm([]string{"UPDATE voice_command_outbox"}, nil, stmthook.ErrInjected)
			}
			claim, err := NewRepository(wrapped).Claim(context.Background(), uuid.New(), claimLease)
			require.ErrorIs(t, err, stmthook.ErrInjected)
			require.ErrorContains(t, err, operation)
			assert.Nil(t, claim)
			if !commitFault {
				seen, betweenErr := hook.Report()
				require.NoError(t, betweenErr)
				require.Equal(t, 1, seen)
			}
			assert.Equal(t, before, outboxDelivery(t, db, 1), "the lease and attempt increment must both roll back")
			requireOutboxCursor(t, db, 1, 0)
			successor := outboxClaim(t, NewRepository(db))
			assert.Equal(t, command, successor.Command)
			assert.Equal(t, 1, successor.Attempts)
			require.NoError(t, NewRepository(db).MarkPublished(context.Background(), successor, outboxReceipt(1)))
			requireOutboxCursor(t, db, 1, 1)
		})
	}
}

func TestVoiceOutboxRepository_RetryCommitRollbackRetainsCurrentLease(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot())
	repository := NewRepository(db)
	claim := outboxClaim(t, repository)
	before := outboxDelivery(t, db, 1)
	hook, wrapped := stmthook.Open(t)
	hook.ArmCommit(false, stmthook.ErrInjected)
	err := NewRepository(wrapped).Retry(context.Background(), claim, "broker_unavailable", 0)
	require.ErrorIs(t, err, stmthook.ErrInjected)
	require.ErrorContains(t, err, "commit outbox command retry")
	assert.Equal(t, before, outboxDelivery(t, db, 1), "a failed retry commit cannot clear ownership or store a failure class")
	requireOutboxCursor(t, db, 1, 0)
	require.NoError(t, repository.Retry(context.Background(), claim, "broker_unavailable", 0))
	successor := outboxClaim(t, repository)
	assert.NotEqual(t, claim.Token, successor.Token)
	assert.Equal(t, claim.Attempts+1, successor.Attempts)
	require.NoError(t, repository.MarkPublished(context.Background(), successor, outboxReceipt(1)))
	requireOutboxCursor(t, db, 1, 1)
}
