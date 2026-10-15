package voicecommand

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

func retirementDispatcher(t *testing.T, allocated, published int64, ack uint64) (*sql.DB, *Dispatcher, *capacityBroker, *outboxReports) {
	t.Helper()
	db, repository, receipt := retirementFixture(t, allocated, published)
	broker := &capacityBroker{db: db, receipt: receipt, first: 1, ack: ack,
		born: retirementConsumerBirth, latest: outboxBrokerMessage(t, outboxStoredCommand(t, db, published))}
	reports := &outboxReports{}
	return db, &Dispatcher{Repository: repository, Broker: broker, Report: reports.add}, broker, reports
}

func retirementDue(t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`UPDATE voice_command_retirement SET retry_at = clock_timestamp() - interval '1 second'
		WHERE intent_id = $1`, id)
	require.NoError(t, err)
}

func TestVoiceOutboxDispatcherRetirement_LostPurgeReplyResumesExactPendingInterval(t *testing.T) {
	db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
	broker.afterRetire = func(context.Context) error { return stmthook.ErrInjected }
	worked, err := dispatcher.Step(context.Background())
	require.ErrorIs(t, err, stmthook.ErrInjected)
	assert.True(t, worked)
	state, err := dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	intent := *state.Pending
	assert.Zero(t, state.RetiredThrough)
	assert.Equal(t, "broker_unavailable", intent.FailureClass)
	assert.Equal(t, 1, intent.Attempts)
	assert.False(t, intent.Due)
	assert.Equal(t, []uint64{4}, broker.retired)
	assert.Equal(t, uint64(5), broker.first)
	blocked, err := dispatcher.Repository.Claim(context.Background(), uuid.New(), claimLease)
	require.NoError(t, err)
	assert.Nil(t, blocked)
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.False(t, worked, "backoff cannot be bypassed by another idle worker")
	retirementDue(t, db, intent.ID)
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	state, err = dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(4), state.RetiredThrough)
	assert.Nil(t, state.Pending)
	assert.True(t, retirementCompleted(t, db, intent.ID))
	assert.Equal(t, []uint64{4}, broker.retired, "recovery reuses the recorded interval without deleting further history")
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 6, 6)
	assert.Equal(t, 6, outboxCount(t, db))
	assert.Equal(t, 1, broker.appends)
	assert.Equal(t, []string{"broker_unavailable"}, reports.values())
}

func TestVoiceOutboxDispatcherRetirement_FailedRemovalPreservesSuffixAndDurableBackoff(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient failure before mutation", true: "continuity failure before mutation"}[permanent], func(t *testing.T) {
			db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
			cause, class := stmthook.ErrInjected, "broker_unavailable"
			if permanent {
				cause, class = ErrBrokerContinuity, "broker_continuity"
			}
			broker.beforeRetire = func(context.Context) error { return cause }
			_, err := dispatcher.Step(context.Background())
			require.ErrorIs(t, err, cause)
			state, err := dispatcher.Repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			require.NotNil(t, state.Pending)
			assert.Equal(t, class, state.Pending.FailureClass)
			assert.Equal(t, 1, state.Pending.Attempts)
			assert.False(t, state.Pending.Due)
			assert.Equal(t, []string{class}, reports.values())
			assert.Empty(t, broker.retired)
			assert.Equal(t, uint64(1), broker.first)
			assert.Zero(t, state.RetiredThrough)
			retirementDue(t, db, state.Pending.ID)
			worked, err := dispatcher.Step(context.Background())
			require.NoError(t, err)
			if permanent {
				assert.False(t, worked)
				assert.Empty(t, broker.retired, "an elapsed retry timestamp cannot clear a continuity block")
				blocked, err := dispatcher.Repository.Claim(context.Background(), uuid.New(), claimLease)
				require.NoError(t, err)
				assert.Nil(t, blocked)
			} else {
				assert.True(t, worked)
				assert.Equal(t, []uint64{4}, broker.retired)
			}
			requireOutboxCursor(t, db, 6, 5)
			assert.Equal(t, 6, outboxCount(t, db))
			assert.Equal(t, 1, admissionCount(t, db, uuid.MustParse(outboxStoredCommand(t, db, 6).UserID)))
		})
	}
}

func TestVoiceOutboxDispatcherRetirement_UnknownConsumerStillAllowsOrdinaryPublication(t *testing.T) {
	for _, fault := range []error{errors.New("test named consumer missing"), stmthook.ErrInjected} {
		t.Run(fault.Error(), func(t *testing.T) {
			db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
			broker.ackFault = fault
			worked, err := dispatcher.Step(context.Background())
			require.NoError(t, err)
			assert.True(t, worked)
			requireOutboxCursor(t, db, 6, 6)
			assert.Equal(t, 1, broker.appends)
			assert.Empty(t, broker.retired)
			assert.Zero(t, retirementProofCount(t, db))
			assert.Equal(t, []string{"broker_unavailable"}, reports.values())
		})
	}
}

func TestVoiceOutboxDispatcherRetirement_InvalidProofOrPredecessorCannotAdvanceFloor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *capacityBroker)
	}{
		{"future acknowledgement", func(_ *testing.T, b *capacityBroker) { b.ack = 6 }},
		{"acknowledgement beyond BIGINT", func(_ *testing.T, b *capacityBroker) { b.ack = math.MaxUint64 }},
		{"missing consumer birth", func(_ *testing.T, b *capacityBroker) { b.born = time.Time{} }},
		{"consumer continuity uncertainty", func(_ *testing.T, b *capacityBroker) { b.ackFault = ErrBrokerContinuity }},
		{"foreign predecessor", func(t *testing.T, b *capacityBroker) {
			command, err := DecodeVoiceCommand(b.latest.Data, b.receipt.Generation)
			require.NoError(t, err)
			command.CommandID = uuid.NewString()
			b.latest.Data, err = json.Marshal(command)
			require.NoError(t, err)
		}},
		{"malformed predecessor", func(_ *testing.T, b *capacityBroker) { b.latest.Data = []byte(`{"schema":"invalid"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
			tc.change(t, broker)
			_, err := dispatcher.Step(context.Background())
			require.ErrorIs(t, err, ErrBrokerContinuity)
			state, err := dispatcher.Repository.ReadRetirement(context.Background())
			require.NoError(t, err)
			assert.Zero(t, state.RetiredThrough)
			assert.Nil(t, state.Pending)
			assert.Zero(t, retirementProofCount(t, db))
			assert.Empty(t, broker.retired)
			assert.Zero(t, broker.appends)
			assert.Equal(t, []string{"broker_continuity"}, reports.values())
			requireOutboxCursor(t, db, 6, 5)
		})
	}
}

func TestVoiceOutboxDispatcherRetirement_UncertainStoredHeadRecoversBeforeRetirement(t *testing.T) {
	db, dispatcher, broker, _ := retirementDispatcher(t, 6, 5, 6)
	command := outboxStoredCommand(t, db, 6)
	broker.latest = outboxBrokerMessage(t, command)
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 6, 6)
	assert.Zero(t, broker.appends, "an uncertain PubAck uses the exact retained command without republishing")
	assert.Empty(t, broker.retired)
	assert.Zero(t, retirementProofCount(t, db))
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	state, err := dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(5), state.RetiredThrough)
	assert.Equal(t, []uint64{5}, broker.retired)
	assert.Equal(t, uint64(6), broker.first)
	assert.Zero(t, broker.appends)
	assert.Equal(t, command, outboxStoredCommand(t, db, 6))
	assert.Equal(t, 6, outboxCount(t, db))
}

func TestVoiceOutboxDispatcherRetirement_MissingCapabilityFailsClosedAfterPreparation(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending intent", true: "completed checkpoint"}[completed], func(t *testing.T) {
			db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
			intent := retirementPrepare(t, dispatcher.Repository, broker.receipt, 5)
			if completed {
				require.NoError(t, broker.RetireVoiceCommandPrefix(boundedRetirementContext(t), broker.receipt, intent.From, intent.Through))
				require.NoError(t, dispatcher.Repository.CompleteRetirement(context.Background(), intent))
			}
			legacy := &outboxBroker{db: db, receipt: broker.receipt, latest: broker.latest}
			dispatcher.Broker = legacy
			worked, err := dispatcher.Step(context.Background())
			require.ErrorIs(t, err, ErrBrokerContinuity)
			assert.False(t, worked)
			assert.Empty(t, legacy.eventList())
			assert.Equal(t, []string{"broker_continuity"}, reports.values())
			requireOutboxCursor(t, db, 6, 5)
			assert.False(t, outboxDelivery(t, db, 6).Token.Valid)
		})
	}
}

func boundedRetirementContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestVoiceOutboxDispatcherRetirement_ConsumerReplacementResumesHistoricPendingProof(t *testing.T) {
	db, dispatcher, broker, _ := retirementDispatcher(t, 6, 5, 5)
	intent := retirementPrepare(t, dispatcher.Repository, broker.receipt, 5)
	broker.born = broker.born.Add(time.Hour)
	broker.ack = 0
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	state, err := dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(4), state.RetiredThrough)
	assert.True(t, retirementCompleted(t, db, intent.ID))
	var originalBirth string
	require.NoError(t, db.QueryRow(`SELECT consumer_created_at FROM voice_command_retirement WHERE intent_id = $1`, intent.ID).Scan(&originalBirth))
	assert.Equal(t, retirementConsumerBirth.Format(time.RFC3339Nano), originalBirth)
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 6, 6)
	assert.Equal(t, 1, retirementProofCount(t, db), "a recreated consumer's zero acknowledgement cannot authorize a new interval")
}

func TestVoiceOutboxDispatcherRetirement_CompletionRollbackKeepsPreparedIntervalForSuccessor(t *testing.T) {
	db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
	hook, wrapped := stmthook.Open(t)
	dispatcher.Repository = NewRepository(wrapped)
	broker.afterRetire = func(context.Context) error {
		hook.ArmCommit(false, stmthook.ErrInjected)
		return nil
	}
	worked, err := dispatcher.Step(context.Background())
	require.ErrorIs(t, err, stmthook.ErrInjected)
	assert.True(t, worked)
	state, err := NewRepository(db).ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	assert.Zero(t, state.RetiredThrough)
	assert.Equal(t, "receipt_unavailable", state.Pending.FailureClass)
	assert.Equal(t, []string{"receipt_unavailable"}, reports.values())
	assert.Equal(t, []uint64{4}, broker.retired)
	retirementDue(t, db, state.Pending.ID)
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	state, err = NewRepository(db).ReadRetirement(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(4), state.RetiredThrough)
	assert.Nil(t, state.Pending)
	assert.Equal(t, []uint64{4}, broker.retired)
	requireOutboxCursor(t, db, 6, 5)
}

func TestVoiceOutboxDispatcherRetirement_ConcurrentWorkersResumeOnePreparedInterval(t *testing.T) {
	db, dispatcher, broker, _ := retirementDispatcher(t, 6, 5, 5)
	entered, release := make(chan struct{}), make(chan struct{})
	broker.beforeRetire = func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	type result struct {
		worked bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		worked, err := dispatcher.Step(context.Background())
		done <- result{worked, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never reached prepared broker operation")
	}
	state, err := dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	preparedID := state.Pending.ID
	second := &Dispatcher{Repository: NewRepository(db), Broker: broker}
	worked, err := second.Step(context.Background())
	close(release)
	first := <-done
	require.NoError(t, err)
	require.NoError(t, first.err)
	assert.True(t, worked)
	assert.True(t, first.worked)
	assert.Equal(t, 1, retirementProofCount(t, db))
	assert.True(t, retirementCompleted(t, db, preparedID))
	assert.Equal(t, []uint64{4}, broker.retired)
	assert.Zero(t, broker.appends)
	requireOutboxCursor(t, db, 6, 5)
}

func TestVoiceOutboxDispatcherRetirement_CancellationPreservesIntentAndAwaitsBroker(t *testing.T) {
	db, dispatcher, broker, _ := retirementDispatcher(t, 6, 5, 5)
	entered, observedCancellation, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	broker.beforeRetire = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(observedCancellation)
		<-release
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		dispatcher.Run(ctx)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run never reached prepared broker operation")
	}
	cancel()
	select {
	case <-observedCancellation:
	case <-time.After(10 * time.Second):
		t.Fatal("broker context did not observe cancellation")
	}
	select {
	case <-done:
		t.Error("Run returned while its broker operation was still active")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish its canceled bounded attempt")
	}
	state, err := dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	assert.Equal(t, "broker_unavailable", state.Pending.FailureClass)
	assert.Zero(t, state.RetiredThrough)
	assert.Empty(t, broker.retired)
	assert.Zero(t, broker.appends)
	retirementDue(t, db, state.Pending.ID)
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 6, 5)
}

func TestVoiceOutboxDispatcherRetirement_CanceledAttemptBeforePurgeLeavesPreparedProof(t *testing.T) {
	db, dispatcher, broker, _ := retirementDispatcher(t, 6, 5, 5)
	intent := retirementPrepare(t, dispatcher.Repository, broker.receipt, 5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worked, err := dispatcher.Step(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, worked)
	state, err := dispatcher.Repository.ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	assert.Equal(t, intent, *state.Pending)
	assert.Empty(t, broker.retired)
	assert.Zero(t, broker.appends)
	assert.Zero(t, state.RetiredThrough)
	requireOutboxCursor(t, db, 6, 5)
}

func TestVoiceOutboxDispatcherRetirement_RetryPersistenceFailureRemainsVisible(t *testing.T) {
	db, dispatcher, broker, reports := retirementDispatcher(t, 6, 5, 5)
	hook, wrapped := stmthook.Open(t)
	dispatcher.Repository = NewRepository(wrapped)
	broker.beforeRetire = func(context.Context) error {
		hook.Arm([]string{"UPDATE voice_command_retirement"}, nil, stmthook.ErrInjected)
		return errOutboxNetwork
	}
	worked, err := dispatcher.Step(context.Background())
	assert.True(t, worked)
	require.ErrorIs(t, err, errOutboxNetwork)
	require.ErrorIs(t, err, stmthook.ErrInjected, "retry persistence is not discarded behind the broker failure")
	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	assert.Equal(t, 1, seen)
	assert.Equal(t, []string{"broker_unavailable", "receipt_unavailable"}, reports.values())
	state, err := NewRepository(db).ReadRetirement(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	assert.Zero(t, state.Pending.Attempts)
	assert.Zero(t, state.RetiredThrough)
	assert.Empty(t, broker.retired)
	blocked, err := NewRepository(db).Claim(context.Background(), uuid.New(), claimLease)
	require.NoError(t, err)
	assert.Nil(t, blocked)
	requireOutboxCursor(t, db, 6, 5)
}
