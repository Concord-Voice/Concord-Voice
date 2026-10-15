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

var retirementConsumerBirth = time.Date(2026, 10, 14, 0, 0, 0, 123456789, time.UTC)

func retirementFixture(t *testing.T, allocated, published int64) (*sql.DB, *Repository, TransportReceipt) {
	t.Helper()
	db, generation := outboxDatabase(t)
	capacitySeed(t, db, generation, allocated, published)
	_, broker, _ := outboxDispatcher(t, db)
	return db, NewRepository(db), broker.receipt
}

func retirementPrepare(t *testing.T, repository *Repository, receipt TransportReceipt, acknowledged uint64) RetirementIntent {
	t.Helper()
	intent, err := repository.PrepareRetirement(context.Background(), receipt, retirementConsumerBirth, acknowledged)
	require.NoError(t, err)
	require.NotNil(t, intent)
	return *intent
}

func retirementProofCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_command_retirement`).Scan(&count))
	return count
}

func retirementCompleted(t *testing.T, db *sql.DB, id uuid.UUID) bool {
	t.Helper()
	var completed bool
	require.NoError(t, db.QueryRow(`SELECT completed_at IS NOT NULL FROM voice_command_retirement
		WHERE intent_id = $1`, id).Scan(&completed))
	return completed
}

func TestVoiceOutboxRetirement_ClampsAcknowledgementBeforePublishedPredecessor(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ack, target uint64
	}{
		{"no acknowledgement", 0, 0},
		{"partial acknowledgement", 2, 2},
		{"acknowledged published predecessor", 5, 4},
		{"uncertain appended head also acknowledged", 6, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, repository, receipt := retirementFixture(t, 6, 5)
			before := outboxStoredCommand(t, db, 5)
			intent, err := repository.PrepareRetirement(context.Background(), receipt, retirementConsumerBirth, tc.ack)
			require.NoError(t, err)
			state, err := repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			assert.Equal(t, uint64(6), state.Allocated)
			assert.Equal(t, uint64(5), state.Published)
			assert.Zero(t, state.RetiredThrough)
			if tc.target == 0 {
				assert.Nil(t, intent)
				assert.Nil(t, state.Pending)
				assert.Zero(t, retirementProofCount(t, db))
				return
			}
			require.NotNil(t, intent)
			require.NotNil(t, state.Pending)
			assert.Equal(t, intent.ID, state.Pending.ID)
			assert.NotEqual(t, uuid.Nil, intent.ID)
			assert.Zero(t, intent.From)
			assert.Equal(t, tc.target, intent.Through)
			assert.Equal(t, uint64(5), intent.Published)
			assert.Equal(t, tc.ack, intent.AckFloor)
			assert.Equal(t, receipt, intent.Receipt)
			assert.Equal(t, retirementConsumerBirth, intent.ConsumerCreated)
			assert.False(t, retirementCompleted(t, db, intent.ID))
			requireOutboxCursor(t, db, 6, 5)
			assert.Equal(t, before, outboxStoredCommand(t, db, 5))
			assert.Equal(t, 6, outboxCount(t, db))
		})
	}
}

func TestVoiceOutboxRetirement_PendingIntentBlocksClaimUntilCheckpointCommits(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 4, 3)
	intent := retirementPrepare(t, repository, receipt, 3)
	blocked, err := repository.Claim(context.Background(), uuid.New(), claimLease)
	require.NoError(t, err)
	assert.Nil(t, blocked, "a prepared deletion and a live publisher cannot overlap")
	assert.False(t, outboxDelivery(t, db, 4).Token.Valid)
	require.NoError(t, repository.CompleteRetirement(context.Background(), intent))
	state, err := repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Nil(t, state.Pending)
	assert.Equal(t, uint64(2), state.RetiredThrough)
	assert.True(t, retirementCompleted(t, db, intent.ID))
	claim := outboxClaim(t, repository)
	assert.Equal(t, "4", claim.Command.Sequence)
	assert.Equal(t, uint64(2), claim.RetiredThrough)
	require.NoError(t, repository.MarkPublished(context.Background(), claim, outboxReceipt(4)))
	requireOutboxCursor(t, db, 4, 4)
	assert.Equal(t, 4, outboxCount(t, db))
}

func TestVoiceOutboxRetirement_LiveClaimPreventsPreparationAndExpiredClaimCannotPublish(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 4, 3)
	stale := outboxClaim(t, repository)
	intent, err := repository.PrepareRetirement(context.Background(), receipt, retirementConsumerBirth, 3)
	require.NoError(t, err)
	assert.Nil(t, intent)
	assert.Zero(t, retirementProofCount(t, db))
	outboxExpireLease(t, db, 4)
	prepared := retirementPrepare(t, repository, receipt, 3)
	before := outboxDelivery(t, db, 4)
	require.ErrorIs(t, repository.MarkPublished(context.Background(), stale, outboxReceipt(4)), ErrClaimLost)
	require.ErrorIs(t, repository.Retry(context.Background(), stale, "broker_unavailable", 0), ErrClaimLost)
	assert.Equal(t, before, outboxDelivery(t, db, 4))
	requireOutboxCursor(t, db, 4, 3)
	require.NoError(t, repository.CompleteRetirement(context.Background(), prepared))
	current := outboxClaim(t, repository)
	assert.NotEqual(t, stale.Token, current.Token)
	require.NoError(t, repository.MarkPublished(context.Background(), current, outboxReceipt(4)))
}

func TestVoiceOutboxRetirement_ConcurrentPreparationHasOneImmutableIntent(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 6, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	type result struct {
		intent *RetirementIntent
		err    error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			intent, err := repository.PrepareRetirement(ctx, receipt, retirementConsumerBirth, 5)
			done <- result{intent, err}
		}()
	}
	close(start)
	first, second := <-done, <-done
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotNil(t, first.intent)
	require.NotNil(t, second.intent)
	assert.Equal(t, *first.intent, *second.intent)
	assert.Equal(t, 1, retirementProofCount(t, db))
	require.NoError(t, repository.CompleteRetirement(ctx, *first.intent))
	require.NoError(t, repository.CompleteRetirement(ctx, *second.intent), "completion of the same durable proof is idempotent")
	state, err := repository.ReadRetirement(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(4), state.RetiredThrough)
	assert.Nil(t, state.Pending)
}

func TestVoiceOutboxRetirement_SubstitutedProofCannotCompleteOrAlterRetry(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 4, 3)
	intent := retirementPrepare(t, repository, receipt, 3)
	for _, tc := range []struct {
		name   string
		change func(*RetirementIntent)
	}{
		{"intent identity", func(i *RetirementIntent) { i.ID = uuid.New() }},
		{"previous floor", func(i *RetirementIntent) { i.From++ }},
		{"target floor", func(i *RetirementIntent) { i.Through-- }},
		{"published cursor", func(i *RetirementIntent) { i.Published++ }},
		{"acknowledgement floor", func(i *RetirementIntent) { i.AckFloor++ }},
		{"consumer birth", func(i *RetirementIntent) { i.ConsumerCreated = i.ConsumerCreated.Add(time.Nanosecond) }},
		{"generation", func(i *RetirementIntent) { i.Receipt.Generation = uuid.New() }},
		{"stream birth", func(i *RetirementIntent) {
			i.Receipt.Identity.StreamCreatedAt = i.Receipt.Identity.StreamCreatedAt.Add(time.Nanosecond)
		}},
		{"owner birth", func(i *RetirementIntent) {
			i.Receipt.Identity.OwnerStoreCreatedAt = i.Receipt.Identity.OwnerStoreCreatedAt.Add(time.Nanosecond)
		}},
		{"initializing revision", func(i *RetirementIntent) { i.Receipt.Identity.InitializingRevision++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := intent
			tc.change(&forged)
			require.Error(t, repository.CompleteRetirement(context.Background(), forged))
			require.Error(t, repository.RetryRetirement(context.Background(), forged, "broker_unavailable", 0))
			state, err := repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			require.NotNil(t, state.Pending)
			assert.Equal(t, intent, *state.Pending)
			assert.Zero(t, state.RetiredThrough)
			assert.False(t, retirementCompleted(t, db, intent.ID))
		})
	}
}

func TestVoiceOutboxRetirement_RetryKeepsProofAndClaimBarrier(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 4, 3)
	intent := retirementPrepare(t, repository, receipt, 3)
	require.NoError(t, repository.RetryRetirement(context.Background(), intent, "broker_unavailable", 30*time.Second))
	state, err := repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	assert.Equal(t, intent.ID, state.Pending.ID)
	assert.Equal(t, intent.Through, state.Pending.Through)
	assert.Equal(t, "broker_unavailable", state.Pending.FailureClass)
	assert.Greater(t, state.Pending.Attempts, intent.Attempts)
	assert.False(t, state.Pending.Due)
	blocked, err := repository.Claim(context.Background(), uuid.New(), claimLease)
	require.NoError(t, err)
	assert.Nil(t, blocked)
	_, err = db.Exec(`UPDATE voice_command_retirement SET retry_at = clock_timestamp() - interval '1 second'
		WHERE intent_id = $1`, intent.ID)
	require.NoError(t, err)
	state, err = repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	assert.True(t, state.Pending.Due)
	require.NoError(t, repository.CompleteRetirement(context.Background(), *state.Pending))
	require.Error(t, repository.RetryRetirement(context.Background(), intent, "broker_unavailable", 0), "a stale retry cannot reopen completed evidence")
	assert.True(t, retirementCompleted(t, db, intent.ID))
}

func TestVoiceOutboxRetirement_CompletionCommitAmbiguityUsesExactProof(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed reply lost", false: "rolled back reply lost"}[committed], func(t *testing.T) {
			db, repository, receipt := retirementFixture(t, 4, 3)
			intent := retirementPrepare(t, repository, receipt, 3)
			hook, wrapped := stmthook.Open(t)
			hook.ArmCommit(committed, stmthook.ErrInjected)
			err := NewRepository(wrapped).CompleteRetirement(context.Background(), intent)
			state, readErr := repository.ReadRetirement(context.Background())
			require.NoError(t, readErr)
			assert.Equal(t, committed, retirementCompleted(t, db, intent.ID))
			if committed {
				require.NoError(t, err)
				assert.Equal(t, uint64(2), state.RetiredThrough)
				assert.Nil(t, state.Pending)
			} else {
				require.Error(t, err)
				assert.Zero(t, state.RetiredThrough)
				require.NotNil(t, state.Pending)
				assert.Equal(t, intent.ID, state.Pending.ID)
				require.NoError(t, repository.CompleteRetirement(context.Background(), intent))
			}
			requireOutboxCursor(t, db, 4, 3)
			assert.Equal(t, 4, outboxCount(t, db))
		})
	}
}

func TestVoiceOutboxRetirement_CompletedProofSurvivesConsumerReplacement(t *testing.T) {
	db, repository, receipt := retirementFixture(t, 6, 5)
	old := retirementPrepare(t, repository, receipt, 2)
	require.NoError(t, repository.CompleteRetirement(context.Background(), old))
	newBirth := retirementConsumerBirth.Add(time.Hour)
	none, err := repository.PrepareRetirement(context.Background(), receipt, newBirth, 0)
	require.NoError(t, err)
	assert.Nil(t, none)
	state, err := repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(2), state.RetiredThrough, "new consumer progress cannot erase historical retirement proof")
	assert.True(t, retirementCompleted(t, db, old.ID))
	next, err := repository.PrepareRetirement(context.Background(), receipt, newBirth, 5)
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, uint64(2), next.From)
	assert.Equal(t, uint64(4), next.Through)
	assert.Equal(t, newBirth, next.ConsumerCreated)
	assert.NotEqual(t, old.ID, next.ID)
	assert.Equal(t, 2, retirementProofCount(t, db))
	require.NoError(t, repository.CompleteRetirement(context.Background(), *next))
	_, err = db.Exec(`UPDATE voice_command_retirement SET acknowledged_sequence = acknowledged_sequence + 1
		WHERE intent_id = $1`, old.ID)
	require.Error(t, err, "completed acknowledgement evidence is immutable")
	assert.True(t, retirementCompleted(t, db, old.ID))
}

func TestVoiceOutboxRetirement_PreparationCommitAmbiguityConfirmsDurableIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		committed, readFails bool
	}{
		{name: "committed reply lost", committed: true},
		{name: "rolled back reply lost"},
		{name: "committed proof read unavailable", committed: true, readFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, repository, receipt := retirementFixture(t, 4, 3)
			hook, wrapped := stmthook.Open(t)
			if tc.readFails {
				hook.Arm([]string{"UPDATE voice_command_sequence", "FROM voice_command_retirement"}, nil, stmthook.ErrInjected)
			}
			hook.ArmCommit(tc.committed, stmthook.ErrInjected)
			intent, err := NewRepository(wrapped).PrepareRetirement(context.Background(), receipt, retirementConsumerBirth, 3)
			state, readErr := repository.ReadRetirement(context.Background())
			require.NoError(t, readErr)
			if tc.committed {
				require.NotNil(t, state.Pending)
				assert.Equal(t, uint64(2), state.Pending.Through)
				assert.Equal(t, 1, retirementProofCount(t, db))
				if tc.readFails {
					require.Error(t, err, "an unconfirmed acknowledgement cannot claim successful preparation")
					assert.Nil(t, intent)
					seen, betweenErr := hook.Report()
					require.NoError(t, betweenErr)
					assert.Equal(t, 2, seen, "the confirmation read was actually faulted")
				} else {
					require.NoError(t, err)
					require.NotNil(t, intent)
					assert.Equal(t, *state.Pending, *intent)
				}
			} else {
				require.Error(t, err)
				assert.Nil(t, intent)
				assert.Nil(t, state.Pending)
				assert.Zero(t, retirementProofCount(t, db))
			}
			assert.Zero(t, state.RetiredThrough)
			requireOutboxCursor(t, db, 4, 3)
			assert.Equal(t, 4, outboxCount(t, db))
		})
	}
}
