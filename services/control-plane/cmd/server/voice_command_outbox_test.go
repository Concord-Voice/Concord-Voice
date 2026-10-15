package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natssdk "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

type outboxWorkerEvents struct {
	mu     sync.Mutex
	values []string
}

func (events *outboxWorkerEvents) record(value string) {
	if events == nil {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	events.values = append(events.values, value)
}

func (events *outboxWorkerEvents) snapshot() []string {
	events.mu.Lock()
	defer events.mu.Unlock()
	return append([]string(nil), events.values...)
}

type outboxWorkerTestBroker struct {
	mu         sync.Mutex
	name       string
	closeCount int
	closeErr   error
	closed     chan struct{}
	events     *outboxWorkerEvents
}

func newOutboxWorkerTestBroker(name string, events *outboxWorkerEvents) *outboxWorkerTestBroker {
	return &outboxWorkerTestBroker{name: name, events: events, closed: make(chan struct{})}
}

func (*outboxWorkerTestBroker) Verify(context.Context, voicecommand.TransportReceipt) error {
	return errors.New("worker must invoke its supplied dispatcher step")
}

func (*outboxWorkerTestBroker) LastVoiceCommand(context.Context, voicecommand.TransportReceipt) (*voicecommand.BrokerCommand, error) {
	return nil, errors.New("worker must invoke its supplied dispatcher step")
}

func (*outboxWorkerTestBroker) PublishVoiceCommand(context.Context, voicecommand.TransportReceipt, voicecommand.VoiceCommand) (voicecommand.PublishReceipt, error) {
	return voicecommand.PublishReceipt{}, errors.New("worker must invoke its supplied dispatcher step")
}

func (broker *outboxWorkerTestBroker) Close() error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.closeCount++
	broker.events.record("close " + broker.name)
	if broker.closeCount == 1 {
		close(broker.closed)
	}
	return broker.closeErr
}

func (broker *outboxWorkerTestBroker) closes() int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.closeCount
}

func requireOutboxWorkerContext(operation context.Context, t *testing.T, before, after time.Time, budget time.Duration) {
	t.Helper()
	deadline, bounded := operation.Deadline()
	require.True(t, bounded, "every connection and dispatcher attempt must be bounded")
	assert.False(t, deadline.Before(before.Add(budget)), "operation received too short an attempt budget")
	assert.False(t, deadline.After(after.Add(budget)), "operation exceeded its fixed attempt budget")
	assert.ErrorIs(t, operation.Err(), context.Canceled, "completed attempts release their timer context")
}

func TestVoiceCommandOutboxWorkerReusesOwnedBrokerAndDrainsBeforeIdle(t *testing.T) {
	events := &outboxWorkerEvents{}
	broker := newOutboxWorkerTestBroker("first", events)
	var connects, steps []context.Context
	var delays []time.Duration
	var reports []string
	deps := voiceCommandOutboxWorkerDependencies{
		Connect: func(ctx context.Context) (voiceCommandOutboxBroker, error) {
			connects = append(connects, ctx)
			events.record("connect")
			return broker, nil
		},
		Step: func(ctx context.Context, actual voicecommand.CommandBroker) (bool, error) {
			require.Same(t, broker, actual)
			steps = append(steps, ctx)
			events.record("step")
			return len(steps) < 3, nil
		},
		Wait: func(ctx context.Context, delay time.Duration) bool {
			_, bounded := ctx.Deadline()
			assert.False(t, bounded, "idle wait must not inherit the completed Step timeout")
			delays = append(delays, delay)
			events.record("wait")
			return false
		},
		Report: func(class string) { reports = append(reports, class) },
	}
	before := time.Now()
	runVoiceCommandOutboxWorker(context.Background(), deps)
	after := time.Now()
	assert.Equal(t, []string{"connect", "step", "step", "step", "wait", "close first"}, events.snapshot())
	require.Len(t, connects, 1)
	require.Len(t, steps, 3)
	assert.Equal(t, []time.Duration{250 * time.Millisecond}, delays)
	assert.Empty(t, reports)
	assert.Equal(t, 1, broker.closes())
	requireOutboxWorkerContext(connects[0], t, before, after, 2*time.Second)
	for _, ctx := range steps {
		requireOutboxWorkerContext(ctx, t, before, after, 10*time.Second)
	}
}

func TestVoiceCommandOutboxWorkerConnectionOutageRetriesWithCappedBackoff(t *testing.T) {
	var contexts []context.Context
	var delays []time.Duration
	var reports []string
	steps := 0
	deps := voiceCommandOutboxWorkerDependencies{
		Connect: func(ctx context.Context) (voiceCommandOutboxBroker, error) {
			contexts = append(contexts, ctx)
			return nil, errors.New("private connection error fixture")
		},
		Step: func(context.Context, voicecommand.CommandBroker) (bool, error) {
			steps++
			return false, nil
		},
		Wait: func(ctx context.Context, delay time.Duration) bool {
			_, bounded := ctx.Deadline()
			assert.False(t, bounded)
			delays = append(delays, delay)
			return len(delays) < 8
		},
		Report: func(class string) { reports = append(reports, class) },
	}
	before := time.Now()
	runVoiceCommandOutboxWorker(context.Background(), deps)
	after := time.Now()
	assert.Zero(t, steps, "a nil startup broker cannot silently accept work")
	require.Len(t, contexts, 8)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}, delays)
	require.Len(t, reports, 8)
	for _, class := range reports {
		assert.Equal(t, "voice_command_outbox_connect_failed", class)
		assert.NotContains(t, class, "private connection")
	}
	for _, ctx := range contexts {
		requireOutboxWorkerContext(ctx, t, before, after, 2*time.Second)
	}
}

func TestVoiceCommandOutboxWorkerStepFailureReplacesConnectionAndResetsBackoff(t *testing.T) {
	events := &outboxWorkerEvents{}
	brokers := []*outboxWorkerTestBroker{
		newOutboxWorkerTestBroker("first", events), newOutboxWorkerTestBroker("second", events),
		newOutboxWorkerTestBroker("third", events), newOutboxWorkerTestBroker("fourth", events),
	}
	var delays []time.Duration
	var reports []string
	connects, steps := 0, 0
	deps := voiceCommandOutboxWorkerDependencies{
		Connect: func(context.Context) (voiceCommandOutboxBroker, error) {
			require.Less(t, connects, len(brokers))
			broker := brokers[connects]
			connects++
			events.record("connect " + broker.name)
			return broker, nil
		},
		Step: func(_ context.Context, actual voicecommand.CommandBroker) (bool, error) {
			steps++
			events.record("step")
			switch steps {
			case 1, 2, 4:
				return true, errors.New("private dispatcher error fixture")
			case 3:
				require.Same(t, brokers[2], actual)
				return false, nil
			case 5:
				require.Same(t, brokers[3], actual)
				return true, nil
			default:
				return false, nil
			}
		},
		Wait: func(_ context.Context, delay time.Duration) bool {
			delays = append(delays, delay)
			events.record("wait")
			return len(delays) < 5
		},
		Report: func(class string) { reports = append(reports, class) },
	}
	runVoiceCommandOutboxWorker(context.Background(), deps)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 250 * time.Millisecond, time.Second, 250 * time.Millisecond}, delays)
	assert.Equal(t, []string{
		"connect first", "step", "close first", "wait",
		"connect second", "step", "close second", "wait",
		"connect third", "step", "wait", "step", "close third", "wait",
		"connect fourth", "step", "step", "wait", "close fourth",
	}, events.snapshot())
	assert.Equal(t, []string{"voice_command_outbox_step_failed", "voice_command_outbox_step_failed", "voice_command_outbox_step_failed"}, reports)
	for _, broker := range brokers {
		assert.Equal(t, 1, broker.closes(), "each failed independently owned connection closes exactly once")
	}
}

func TestVoiceCommandOutboxWorkerCancellationAwaitsStepBeforeClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := &outboxWorkerEvents{}
	broker := newOutboxWorkerTestBroker("owned", events)
	started := make(chan context.Context, 1)
	observedCancellation := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	deps := voiceCommandOutboxWorkerDependencies{
		Connect: func(context.Context) (voiceCommandOutboxBroker, error) {
			events.record("connect")
			return broker, nil
		},
		Step: func(operation context.Context, _ voicecommand.CommandBroker) (bool, error) {
			events.record("step started")
			started <- operation
			<-operation.Done()
			events.record("step canceled")
			close(observedCancellation)
			<-release
			events.record("step returned")
			return true, operation.Err()
		},
		Wait: func(context.Context, time.Duration) bool {
			t.Error("canceled worker must not wait or reconnect")
			return false
		},
		Report: func(string) { t.Error("normal shutdown cancellation cannot report a broker failure") },
	}
	wait := startActivityHistoryWorkers(ctx, func(workerCtx context.Context) { runVoiceCommandOutboxWorker(workerCtx, deps) })
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("owned outbox worker leaked after cancellation")
		}
	})
	select {
	case operation := <-started:
		_, bounded := operation.Deadline()
		require.True(t, bounded)
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher step never started")
	}
	cancel()
	select {
	case <-observedCancellation:
	case <-time.After(time.Second):
		t.Fatal("the in-flight step did not receive worker cancellation")
	}
	// The step is still alive and demonstrably blocked on release. Neither a
	// detached worker nor an early connection close may report shutdown completion.
	select {
	case <-done:
		t.Fatal("worker wait completed before the in-flight step returned")
	case <-broker.closed:
		t.Fatal("owned broker closed while a step still used it")
	case <-time.After(25 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish after its canceled step returned")
	}
	assert.Equal(t, []string{"connect", "step started", "step canceled", "step returned", "close owned"}, events.snapshot())
	assert.Equal(t, 1, broker.closes())
}

func TestVoiceCommandOutboxWorkerCanceledConnectionAttemptDoesNotStartStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connects, steps, waits, reports := 0, 0, 0, 0
	deps := voiceCommandOutboxWorkerDependencies{
		Connect: func(operation context.Context) (voiceCommandOutboxBroker, error) {
			connects++
			cancel()
			<-operation.Done()
			return nil, operation.Err()
		},
		Step:   func(context.Context, voicecommand.CommandBroker) (bool, error) { steps++; return false, nil },
		Wait:   func(context.Context, time.Duration) bool { waits++; return false },
		Report: func(string) { reports++ },
	}
	runVoiceCommandOutboxWorker(ctx, deps)
	assert.Equal(t, 1, connects)
	assert.Zero(t, steps)
	assert.Zero(t, waits)
	assert.Zero(t, reports)
}

func TestVoiceCommandOutboxWorkerAlreadyCanceledDoesNoWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	deps := voiceCommandOutboxWorkerDependencies{
		Connect: func(context.Context) (voiceCommandOutboxBroker, error) {
			calls++
			return newOutboxWorkerTestBroker("unused", nil), nil
		},
		Step:   func(context.Context, voicecommand.CommandBroker) (bool, error) { calls++; return false, nil },
		Wait:   func(context.Context, time.Duration) bool { calls++; return false },
		Report: func(string) { calls++ },
	}
	runVoiceCommandOutboxWorker(ctx, deps)
	assert.Zero(t, calls)
}

func TestVoiceCommandOutboxWorkerMissingDependenciesAndNilFactoryFailClosed(t *testing.T) {
	t.Run("unwired dispatcher", func(t *testing.T) {
		var reports []string
		runVoiceCommandOutboxWorker(context.Background(), voiceCommandOutboxWorkerDependencies{Report: func(class string) { reports = append(reports, class) }})
		assert.Equal(t, []string{"voice_command_outbox_unwired"}, reports)
	})
	t.Run("factory returned no connection", func(t *testing.T) {
		var reports []string
		var delays []time.Duration
		steps := 0
		runVoiceCommandOutboxWorker(context.Background(), voiceCommandOutboxWorkerDependencies{
			Connect: func(context.Context) (voiceCommandOutboxBroker, error) { return nil, nil },
			Step:    func(context.Context, voicecommand.CommandBroker) (bool, error) { steps++; return false, nil },
			Wait:    func(_ context.Context, delay time.Duration) bool { delays = append(delays, delay); return false },
			Report:  func(class string) { reports = append(reports, class) },
		})
		assert.Zero(t, steps)
		assert.Equal(t, []time.Duration{time.Second}, delays)
		assert.Equal(t, []string{"voice_command_outbox_connect_failed"}, reports)
	})
}

func TestVoiceCommandOutboxWorkerCloseFailureUsesFixedClass(t *testing.T) {
	broker := newOutboxWorkerTestBroker("owned", nil)
	broker.closeErr = errors.New("private close error fixture")
	var reports []string
	runVoiceCommandOutboxWorker(context.Background(), voiceCommandOutboxWorkerDependencies{
		Connect: func(context.Context) (voiceCommandOutboxBroker, error) { return broker, nil },
		Step:    func(context.Context, voicecommand.CommandBroker) (bool, error) { return false, nil },
		Wait:    func(context.Context, time.Duration) bool { return false },
		Report:  func(class string) { reports = append(reports, class) },
	})
	assert.Equal(t, 1, broker.closes())
	assert.Equal(t, []string{"voice_command_outbox_close_failed"}, reports)
}

func mainOutboxDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := mainVoiceReceiptDatabase(t)
	_, err := db.Exec(`TRUNCATE voice_command_admission, voice_command_retirement,
		voice_command_outbox, voice_command_sequence`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_command_sequence (id) VALUES (TRUE)`)
	require.NoError(t, err)
	// This cleanup runs before the existing receipt fixture's ON DELETE RESTRICT
	// cleanup. Worker wait closures are registered later and run before both.
	t.Cleanup(func() {
		_, cleanupErr := db.Exec(`TRUNCATE voice_command_admission, voice_command_retirement,
			voice_command_outbox, voice_command_sequence`)
		assert.NoError(t, cleanupErr)
		_, cleanupErr = db.Exec(`INSERT INTO voice_command_sequence (id) VALUES (TRUE)`)
		assert.NoError(t, cleanupErr)
	})
	return db
}

func enqueueMainOutboxCommands(t *testing.T, db *sql.DB, count int) []voicecommand.VoiceCommand {
	t.Helper()
	conversationID := "30000000-0000-4000-8000-000000000001"
	specs := make([]voicecommand.CommandSpec, count)
	for index := range specs {
		specs[index] = voicecommand.CommandSpec{
			Scope: "dm", Kind: "disconnect", Action: "disconnect",
			ChannelID: &conversationID, UserID: "40000000-0000-4000-8000-000000000001",
			Reason: "membership_removed",
		}
	}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			assert.NoError(t, rollbackErr)
		}
	})
	commands, err := voicecommand.EnqueueTx(t.Context(), tx, specs)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, commands, count)
	return commands
}

func waitMainOutboxWorker(t *testing.T, cancel context.CancelFunc, wait func()) {
	t.Helper()
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled outbox worker must finish its bounded attempt and connection close")
	}
}

func requireMainOutboxPublished(t *testing.T, db *sql.DB, js natssdk.JetStreamContext, commands []voicecommand.VoiceCommand) {
	t.Helper()
	require.Eventually(t, func() bool {
		var cursor int64
		if err := db.QueryRow(`SELECT last_published_sequence FROM voice_command_sequence WHERE id=TRUE`).Scan(&cursor); err != nil || cursor != int64(len(commands)) {
			return false
		}
		info, err := js.StreamInfo("VOICE_COMMANDS_V1")
		return err == nil && info.State.Msgs == uint64(len(commands)) && info.State.LastSeq == uint64(len(commands))
	}, 5*time.Second, 10*time.Millisecond, "production worker must retain and acknowledge every committed command in order")
	for index, command := range commands {
		var sequence int64
		var published bool
		err := db.QueryRow(`SELECT broker_sequence, published_at IS NOT NULL FROM voice_command_outbox WHERE command_id=$1`, command.CommandID).Scan(&sequence, &published)
		require.NoError(t, err)
		assert.True(t, published)
		assert.EqualValues(t, index+1, sequence)
		stored, err := js.GetMsg("VOICE_COMMANDS_V1", uint64(index+1))
		require.NoError(t, err)
		assert.Equal(t, "voice.commands.v1", stored.Subject)
		assert.Equal(t, command.CommandID, stored.Header.Get("Nats-Msg-Id"))
		generation, err := uuid.Parse(command.StreamGeneration)
		require.NoError(t, err)
		decoded, err := voicecommand.DecodeVoiceCommand(stored.Data, generation)
		require.NoError(t, err)
		assert.Equal(t, command, decoded, "the real runtime consumes its durable SQL obligation")
	}
}

func TestStartVoiceCommandOutboxWorkerPublishesRealPostgresAndJetStreamAndRetainsShutdownWork(t *testing.T) {
	db := mainOutboxDatabase(t)
	url, js, parent := mainVoiceCommandBroker(t)
	client, err := natsclient.NewVoiceCommandClient(parent, url)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	require.NoError(t, voicecommand.BootstrapTransport(parent, db, client))
	commands := enqueueMainOutboxCommands(t, db, 2)
	ctx, cancel := context.WithCancel(parent)
	wait := startVoiceCommandOutboxWorker(ctx, db, url, logger.NewWithWriter(io.Discard))
	t.Cleanup(func() { cancel(); wait() })
	requireMainOutboxPublished(t, db, js, commands)
	waitMainOutboxWorker(t, cancel, wait)
	owner, _ := readMainCommandOwner(t, js)
	assert.Equal(t, "released", owner.Phase, "publication durability does not need held media ownership")

	// A committed obligation remains after publisher shutdown. Starting with an
	// already-canceled cleanup context must not drain a backlog or erase the row.
	pending := enqueueMainOutboxCommands(t, db, 1)
	canceled, cancelAgain := context.WithCancel(parent)
	cancelAgain()
	stoppedWait := startVoiceCommandOutboxWorker(canceled, db, url, logger.NewWithWriter(io.Discard))
	waitMainOutboxWorker(t, cancelAgain, stoppedWait)
	var allocated, published int64
	err = db.QueryRow(`SELECT last_allocated_sequence, last_published_sequence FROM voice_command_sequence WHERE id=TRUE`).Scan(&allocated, &published)
	require.NoError(t, err)
	assert.EqualValues(t, 3, allocated)
	assert.EqualValues(t, 2, published)
	var retained, receiptPresent bool
	err = db.QueryRow(`SELECT TRUE, published_at IS NOT NULL FROM voice_command_outbox WHERE command_id=$1`, pending[0].CommandID).Scan(&retained, &receiptPresent)
	require.NoError(t, err)
	assert.True(t, retained)
	assert.False(t, receiptPresent)
	info, err := js.StreamInfo("VOICE_COMMANDS_V1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, info.State.Msgs)
	assert.EqualValues(t, 2, info.State.LastSeq)
}

func TestStartVoiceCommandOutboxWorkerDoesNotBootstrapAndResumesAfterTransportProvisioning(t *testing.T) {
	db := mainOutboxDatabase(t)
	url, js, parent := mainVoiceCommandBroker(t)
	commands := enqueueMainOutboxCommands(t, db, 2)
	capture := &workerLogCapture{written: make(chan struct{}, 16)}
	ctx, cancel := context.WithCancel(parent)
	wait := startVoiceCommandOutboxWorker(ctx, db, url, logger.NewWithWriter(capture))
	t.Cleanup(func() { cancel(); wait() })
	visiblePending := time.NewTimer(3 * time.Second)
	defer visiblePending.Stop()
	for !strings.Contains(capture.String(), "failure_class=transport_unavailable") {
		select {
		case <-capture.written:
		case <-visiblePending.C:
			t.Fatal("unprovisioned transport must remain visibly pending")
		}
	}
	waitMainOutboxWorker(t, cancel, wait)
	assert.Contains(t, capture.String(), "failure_class=transport_unavailable")
	assert.NotContains(t, capture.String(), commands[0].CommandID)
	assert.NotContains(t, capture.String(), commands[0].UserID)
	assert.NotContains(t, capture.String(), *commands[0].ChannelID)
	_, err := js.StreamInfo("VOICE_COMMANDS_V1")
	require.ErrorIs(t, err, natssdk.ErrStreamNotFound, "outbox worker cannot create, release or repair transport resources")
	var allocated, published, pending int64
	err = db.QueryRow(`SELECT last_allocated_sequence,last_published_sequence FROM voice_command_sequence WHERE id=TRUE`).Scan(&allocated, &published)
	require.NoError(t, err)
	assert.EqualValues(t, 2, allocated)
	assert.Zero(t, published)
	err = db.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox WHERE published_at IS NULL`).Scan(&pending)
	require.NoError(t, err)
	assert.EqualValues(t, 2, pending)

	// Existing transport bootstrap is the sole provisioner. SQL expiry models a
	// replacement's lease acquisition without weakening production duration.
	client, err := natsclient.NewVoiceCommandClient(parent, url)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	require.NoError(t, voicecommand.BootstrapTransport(parent, db, client))
	_, err = db.Exec(`UPDATE voice_command_outbox SET
		lease_until=CASE WHEN lease_token IS NULL THEN NULL ELSE clock_timestamp()-INTERVAL '1 second' END,
		retry_at=clock_timestamp() WHERE published_at IS NULL`)
	require.NoError(t, err)
	restarted, cancelRestart := context.WithCancel(parent)
	restartWait := startVoiceCommandOutboxWorker(restarted, db, url, logger.NewWithWriter(io.Discard))
	t.Cleanup(func() { cancelRestart(); restartWait() })
	requireMainOutboxPublished(t, db, js, commands)
	waitMainOutboxWorker(t, cancelRestart, restartWait)
	owner, _ := readMainCommandOwner(t, js)
	assert.Equal(t, "released", owner.Phase)
}

var _ voiceCommandOutboxBroker = (*outboxWorkerTestBroker)(nil)
