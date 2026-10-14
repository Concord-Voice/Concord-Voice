package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

type transportWorkerTestBroker struct {
	name       string
	closeCount int
	closeErr   error
	events     *[]string
}

var _ voiceCommandTransportBroker = (*transportWorkerTestBroker)(nil)

func (b *transportWorkerTestBroker) Initialize(context.Context, uuid.UUID) (voicecommand.TransportIdentity, error) {
	return voicecommand.TransportIdentity{}, errors.New("worker must use its injected bootstrap operation")
}

func (b *transportWorkerTestBroker) Verify(context.Context, voicecommand.TransportReceipt) error {
	return errors.New("worker must use its injected verification operation")
}

func (b *transportWorkerTestBroker) ReleaseInitialOwner(context.Context, voicecommand.TransportReceipt) error {
	return errors.New("worker must use its injected bootstrap operation")
}

func (b *transportWorkerTestBroker) Close() error {
	b.closeCount++
	if b.events != nil {
		*b.events = append(*b.events, "close "+b.name)
	}
	return b.closeErr
}

func requireWorkerAttemptContexts(t *testing.T, contexts []context.Context, before, after time.Time) {
	t.Helper()
	require.NotEmpty(t, contexts)
	for _, operation := range contexts {
		deadline, present := operation.Deadline()
		require.True(t, present, "every broker attempt has a deadline")
		assert.False(t, deadline.Before(before.Add(10*time.Second)), "attempt timeout must be ten seconds")
		assert.False(t, deadline.After(after.Add(10*time.Second)), "attempt timeout cannot exceed ten seconds")
		assert.ErrorIs(t, operation.Err(), context.Canceled, "completed attempts release their timer contexts")
	}
}

func requireWorkerWaitContext(ctx context.Context, t *testing.T) {
	t.Helper()
	_, bounded := ctx.Deadline()
	assert.False(t, bounded, "the 30-second wait cannot inherit the ten-second operation deadline")
}

func TestVoiceCommandTransportWorkerReusesOwnedBrokerAndVerifiesEveryThirtySeconds(t *testing.T) {
	var events []string
	broker := &transportWorkerTestBroker{name: "first", events: &events}
	var contexts []context.Context
	var delays []time.Duration
	var reports []string
	deps := voiceCommandTransportWorkerDependencies{
		Connect: func(ctx context.Context) (voiceCommandTransportBroker, error) {
			contexts = append(contexts, ctx)
			events = append(events, "connect")
			return broker, nil
		},
		Bootstrap: func(ctx context.Context, actual voicecommand.TransportProvisioner) error {
			contexts = append(contexts, ctx)
			require.Same(t, broker, actual)
			events = append(events, "bootstrap")
			return nil
		},
		Verify: func(ctx context.Context, actual voicecommand.TransportProvisioner) error {
			contexts = append(contexts, ctx)
			require.Same(t, broker, actual)
			events = append(events, "verify")
			return nil
		},
		Wait: func(ctx context.Context, delay time.Duration) bool {
			requireWorkerWaitContext(ctx, t)
			delays = append(delays, delay)
			events = append(events, "wait")
			return len(delays) < 3
		},
		Report: func(class string) { reports = append(reports, class) },
	}
	before := time.Now()
	runVoiceCommandTransportWorker(context.Background(), deps)
	after := time.Now()
	assert.Equal(t, []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second}, delays)
	assert.Equal(t, []string{"connect", "bootstrap", "wait", "verify", "wait", "verify", "wait", "close first"}, events)
	assert.Equal(t, 1, broker.closeCount, "the worker owns and closes one broker exactly once")
	assert.Empty(t, reports)
	requireWorkerAttemptContexts(t, contexts, before, after)
	connectDeadline, _ := contexts[0].Deadline()
	bootstrapDeadline, _ := contexts[1].Deadline()
	assert.Equal(t, connectDeadline, bootstrapDeadline, "connect and initial bootstrap share one attempt budget")
}

func TestVoiceCommandTransportWorkerRetriesInitialConnectionOutageWithCappedBackoff(t *testing.T) {
	var contexts []context.Context
	var delays []time.Duration
	var reports []string
	connects, bootstraps, verifies := 0, 0, 0
	deps := voiceCommandTransportWorkerDependencies{
		Connect: func(ctx context.Context) (voiceCommandTransportBroker, error) {
			contexts = append(contexts, ctx)
			connects++
			return nil, errors.New("private connection failure payload")
		},
		Bootstrap: func(context.Context, voicecommand.TransportProvisioner) error { bootstraps++; return nil },
		Verify:    func(context.Context, voicecommand.TransportProvisioner) error { verifies++; return nil },
		Wait: func(ctx context.Context, delay time.Duration) bool {
			requireWorkerWaitContext(ctx, t)
			delays = append(delays, delay)
			return len(delays) < 8
		},
		Report: func(class string) { reports = append(reports, class) },
	}
	before := time.Now()
	runVoiceCommandTransportWorker(context.Background(), deps)
	after := time.Now()
	assert.Equal(t, 8, connects, "a first-connect outage must keep the independent worker retrying")
	assert.Zero(t, bootstraps)
	assert.Zero(t, verifies)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}, delays)
	require.Len(t, reports, 8)
	for _, class := range reports {
		assert.Equal(t, "voice_command_transport_retry", class)
		assert.NotContains(t, class, "private connection failure payload")
	}
	requireWorkerAttemptContexts(t, contexts, before, after)
}

func TestVoiceCommandTransportWorkerRebootsAfterFailureAndResetsBackoff(t *testing.T) {
	var events []string
	brokers := []*transportWorkerTestBroker{
		{name: "first", events: &events}, {name: "second", events: &events}, {name: "third", events: &events},
	}
	var contexts []context.Context
	var delays []time.Duration
	var reports []string
	var bootstrapped []voicecommand.TransportProvisioner
	var verified []voicecommand.TransportProvisioner
	connects := 0
	deps := voiceCommandTransportWorkerDependencies{
		Connect: func(ctx context.Context) (voiceCommandTransportBroker, error) {
			contexts = append(contexts, ctx)
			require.Less(t, connects, len(brokers))
			broker := brokers[connects]
			connects++
			events = append(events, "connect "+broker.name)
			return broker, nil
		},
		Bootstrap: func(ctx context.Context, actual voicecommand.TransportProvisioner) error {
			contexts = append(contexts, ctx)
			bootstrapped = append(bootstrapped, actual)
			if len(bootstrapped) == 1 {
				events = append(events, "bootstrap failure")
				return errors.New("private bootstrap failure payload")
			}
			events = append(events, "bootstrap success")
			return nil
		},
		Verify: func(ctx context.Context, actual voicecommand.TransportProvisioner) error {
			contexts = append(contexts, ctx)
			verified = append(verified, actual)
			events = append(events, "verify failure")
			return errors.New("private verification failure payload")
		},
		Wait: func(ctx context.Context, delay time.Duration) bool {
			requireWorkerWaitContext(ctx, t)
			delays = append(delays, delay)
			events = append(events, "wait")
			return len(delays) < 4
		},
		Report: func(class string) { reports = append(reports, class) },
	}
	before := time.Now()
	runVoiceCommandTransportWorker(context.Background(), deps)
	after := time.Now()
	assert.Equal(t, []time.Duration{time.Second, 30 * time.Second, time.Second, 30 * time.Second}, delays)
	assert.Equal(t, []string{"connect first", "bootstrap failure", "close first", "wait", "connect second", "bootstrap success", "wait", "verify failure", "close second", "wait", "connect third", "bootstrap success", "wait", "close third"}, events)
	assert.Equal(t, []voicecommand.TransportProvisioner{brokers[0], brokers[1], brokers[2]}, bootstrapped)
	assert.Equal(t, []voicecommand.TransportProvisioner{brokers[1]}, verified)
	assert.Equal(t, []string{"voice_command_transport_retry", "voice_command_transport_retry"}, reports)
	for _, broker := range brokers {
		assert.Equal(t, 1, broker.closeCount)
	}
	requireWorkerAttemptContexts(t, contexts, before, after)
}

func TestVoiceCommandTransportWorkerCancellationClosesOwnedBroker(t *testing.T) {
	for _, stage := range []string{"bootstrap", "verify"} {
		t.Run("during "+stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			broker := &transportWorkerTestBroker{}
			connects, bootstraps, verifies := 0, 0, 0
			var contexts []context.Context
			deps := voiceCommandTransportWorkerDependencies{
				Connect: func(operation context.Context) (voiceCommandTransportBroker, error) {
					contexts = append(contexts, operation)
					connects++
					return broker, nil
				},
				Bootstrap: func(operation context.Context, _ voicecommand.TransportProvisioner) error {
					contexts = append(contexts, operation)
					bootstraps++
					if stage == "bootstrap" {
						cancel()
						return operation.Err()
					}
					return nil
				},
				Verify: func(operation context.Context, _ voicecommand.TransportProvisioner) error {
					contexts = append(contexts, operation)
					verifies++
					cancel()
					return operation.Err()
				},
				Wait:   func(waitContext context.Context, _ time.Duration) bool { return waitContext.Err() == nil },
				Report: func(class string) { assert.NotContains(t, class, context.Canceled.Error()) },
			}
			before := time.Now()
			runVoiceCommandTransportWorker(ctx, deps)
			after := time.Now()
			assert.Equal(t, 1, connects)
			assert.Equal(t, 1, bootstraps)
			if stage == "bootstrap" {
				assert.Zero(t, verifies)
			} else {
				assert.Equal(t, 1, verifies)
			}
			assert.Equal(t, 1, broker.closeCount)
			requireWorkerAttemptContexts(t, contexts, before, after)
		})
	}
}

func TestVoiceCommandTransportWorkerAlreadyCanceledDoesNoWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	deps := voiceCommandTransportWorkerDependencies{
		Connect: func(context.Context) (voiceCommandTransportBroker, error) {
			calls++
			return &transportWorkerTestBroker{}, nil
		},
		Bootstrap: func(context.Context, voicecommand.TransportProvisioner) error { calls++; return nil },
		Verify:    func(context.Context, voicecommand.TransportProvisioner) error { calls++; return nil },
		Wait:      func(context.Context, time.Duration) bool { calls++; return false },
		Report:    func(string) { calls++ },
	}
	runVoiceCommandTransportWorker(ctx, deps)
	assert.Zero(t, calls)
}

func TestVoiceCommandTransportWorkerCloseFailuresUseFixedClass(t *testing.T) {
	for _, tc := range []struct {
		name        string
		closeErr    error
		wantReports []string
	}{
		{name: "successful close"},
		{name: "benign drain with nothing pending", closeErr: natsclient.ErrDrainNothingToDo},
		{name: "first raw close error", closeErr: errors.New("private close payload one"), wantReports: []string{"voice_command_transport_close_failed"}},
		{name: "different raw close error", closeErr: errors.New("private close payload two"), wantReports: []string{"voice_command_transport_close_failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := &transportWorkerTestBroker{closeErr: tc.closeErr}
			var reports []string
			deps := voiceCommandTransportWorkerDependencies{
				Connect:   func(context.Context) (voiceCommandTransportBroker, error) { return broker, nil },
				Bootstrap: func(context.Context, voicecommand.TransportProvisioner) error { return nil },
				Verify: func(context.Context, voicecommand.TransportProvisioner) error {
					t.Error("verification cannot begin after wait cancellation")
					return nil
				},
				Wait:   func(context.Context, time.Duration) bool { return false },
				Report: func(class string) { reports = append(reports, class) },
			}
			runVoiceCommandTransportWorker(context.Background(), deps)
			assert.Equal(t, 1, broker.closeCount)
			assert.Equal(t, tc.wantReports, reports)
			for _, class := range reports {
				assert.NotContains(t, class, "private close payload")
			}
		})
	}
}

type workerLogCapture struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	written chan struct{}
}

func (capture *workerLogCapture) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	count, err := capture.buffer.Write(data)
	select {
	case capture.written <- struct{}{}:
	default:
	}
	return count, err
}

func (capture *workerLogCapture) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.buffer.String()
}

func TestStartVoiceCommandTransportWorkerBootstrapsAndCancelsRealServices(t *testing.T) {
	db, generation := mainVoiceReceiptDatabase(t)
	url, js, parent := mainVoiceCommandBroker(t)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	wait := startVoiceCommandTransportWorker(ctx, db, url, logger.NewWithWriter(io.Discard))
	t.Cleanup(func() { cancel(); wait() })
	require.Eventually(t, func() bool {
		var provisioned bool
		if err := db.QueryRow(`SELECT provisioned_at IS NOT NULL FROM voice_command_transport WHERE id = TRUE`).Scan(&provisioned); err != nil || !provisioned {
			return false
		}
		kv, err := js.KeyValue("VOICE_COMMAND_OWNER_V1")
		if err != nil {
			return false
		}
		entry, err := kv.Get("authoritative")
		if err != nil {
			return false
		}
		var owner mainCommandOwner
		return json.Unmarshal(entry.Value(), &owner) == nil && owner.Phase == "released"
	}, 5*time.Second, 10*time.Millisecond, "production startup must commit its receipt and release only initializing ownership")
	receipt, err := voicecommand.LoadReceipt(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, generation, receipt.Generation)
	before, revision := readMainCommandOwner(t, js)
	cancel()
	wait()
	after, afterRevision := readMainCommandOwner(t, js)
	assert.Equal(t, before, after)
	assert.Equal(t, revision, afterRevision, "shutdown closes the connection without changing permanent ownership state")
	unchanged, err := voicecommand.LoadReceipt(parent, db)
	require.NoError(t, err)
	assert.Equal(t, receipt, unchanged)
}

func TestStartVoiceCommandTransportWorkerInitialOutageIsIndependentAndLogsFixedClass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := &workerLogCapture{written: make(chan struct{}, 1)}
	started := make(chan func(), 1)
	go func() {
		started <- startVoiceCommandTransportWorker(ctx, nil, "://private-outage-payload", logger.NewWithWriter(capture))
	}()
	var wait func()
	select {
	case wait = <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("broker outage blocked background worker startup")
	}
	t.Cleanup(func() { cancel(); wait() })
	select {
	case <-capture.written:
	case <-time.After(time.Second):
		t.Fatal("initial outage emitted no fixed retry class")
	}
	cancel()
	wait()
	output := capture.String()
	assert.Contains(t, output, "failure_class=voice_command_transport_retry")
	assert.NotContains(t, output, "private-outage-payload")
}

func TestStartVoiceCommandTransportWorkerContinuityLossRetainsHeldOwnerRealServices(t *testing.T) {
	db, _ := mainVoiceReceiptDatabase(t)
	url, js, parent := mainVoiceCommandBroker(t)
	_, receipt := provisionMainHeldOwner(parent, t, db, url, js)
	held, revision := readMainCommandOwner(t, js)
	require.NoError(t, js.DeleteStream("VOICE_COMMANDS_V1"))
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	capture := &workerLogCapture{written: make(chan struct{}, 1)}
	wait := startVoiceCommandTransportWorker(ctx, db, url, logger.NewWithWriter(capture))
	t.Cleanup(func() { cancel(); wait() })
	select {
	case <-capture.written:
	case <-time.After(3 * time.Second):
		t.Fatal("lost provisioned stream did not fail closed")
	}
	cancel()
	wait()
	_, err := js.StreamInfo("VOICE_COMMANDS_V1")
	require.ErrorIs(t, err, natssdk.ErrStreamNotFound)
	retained, retainedRevision := readMainCommandOwner(t, js)
	assert.Equal(t, held, retained)
	assert.Equal(t, revision, retainedRevision)
	unchanged, err := voicecommand.LoadReceipt(parent, db)
	require.NoError(t, err)
	assert.Equal(t, receipt, unchanged)
	assert.Contains(t, capture.String(), "failure_class=voice_command_transport_retry")
}

func TestVoiceCommandTransportWorkerRejectsIncompleteDependenciesAndFactory(t *testing.T) {
	t.Run("unwired dependencies", func(t *testing.T) {
		var reports []string
		runVoiceCommandTransportWorker(context.Background(), voiceCommandTransportWorkerDependencies{Report: func(class string) { reports = append(reports, class) }})
		assert.Equal(t, []string{"voice_command_transport_unwired"}, reports)
	})
	t.Run("factory returns no broker", func(t *testing.T) {
		var reports []string
		bootstraps := 0
		runVoiceCommandTransportWorker(context.Background(), voiceCommandTransportWorkerDependencies{
			Connect:   func(context.Context) (voiceCommandTransportBroker, error) { return nil, nil },
			Bootstrap: func(context.Context, voicecommand.TransportProvisioner) error { bootstraps++; return nil },
			Verify:    func(context.Context, voicecommand.TransportProvisioner) error { return nil },
			Wait:      func(context.Context, time.Duration) bool { return false },
			Report:    func(class string) { reports = append(reports, class) },
		})
		assert.Zero(t, bootstraps)
		assert.Equal(t, []string{"voice_command_transport_retry"}, reports)
	})
}

func TestWaitVoiceCommandTransportHonorsElapsedAndCanceledTimers(t *testing.T) {
	assert.True(t, waitVoiceCommandTransport(context.Background(), 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, waitVoiceCommandTransport(ctx, time.Hour), "cancellation must interrupt a pending retry or verification wait")
}
