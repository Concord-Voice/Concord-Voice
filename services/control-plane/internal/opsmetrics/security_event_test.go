package opsmetrics_test

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

type concurrentSecurityEventEmitter struct{ emitted atomic.Uint64 }

func (e *concurrentSecurityEventEmitter) Emit(_ context.Context, _ securityevent.Event) {
	e.emitted.Add(1)
}

type reentrantSecurityEventEmitter struct {
	receiver *opsmetrics.Receiver
	latest   opsmetrics.Envelope
	ok       bool
}

func (e *reentrantSecurityEventEmitter) Emit(_ context.Context, _ securityevent.Event) {
	e.latest, e.ok = e.receiver.Latest(opsmetrics.SourceHost)
}

func TestReceiverSecurityEventRejectsSignedTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	subscriber := &fakeOpsSubscriber{}
	recorder := &securityEventRecorder{}
	receiver := opsmetrics.NewReceiver(subscriber, "cvn_aaaaaaaaaaaaaaaa", []byte("0123456789abcdef0123456789abcdef"), opsmetrics.NewCounters(), logger.New("test"), func() time.Time { return now }) // pragma: allowlist secret -- test-only signing key
	receiver.SetSecurityEvents(recorder)
	require.NoError(t, receiver.Subscribe())
	valid := signedSnapshot(t, opsmetrics.SourceHost, now, 1)
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "schema", raw: []byte("not-json")},
		{name: "signature", raw: bytes.Replace(valid, []byte(`"signature":"`), []byte(`"signature":"00`), 1)},
		{name: "staleness", raw: signedSnapshot(t, opsmetrics.SourceHost, now.Add(-6*time.Minute), 2)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(recorder.events)
			subscriber.handlers[opsmetrics.HostSnapshotSubject](test.raw)
			require.Len(t, recorder.events, before+1)
			require.Equal(t, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonSignedTelemetryRejected}, recorder.events[before])
		})
	}
}

func TestReceiverSecurityEventsReportDependencyFailureAndRecovery(t *testing.T) {
	subscriber := &fakeOpsSubscriber{failOn: opsmetrics.HostSnapshotSubject}
	recorder := &securityEventRecorder{}
	receiver := opsmetrics.NewReceiver(subscriber, "cvn_aaaaaaaaaaaaaaaa", []byte("0123456789abcdef0123456789abcdef"), opsmetrics.NewCounters(), logger.New("test"), time.Now) // pragma: allowlist secret -- test-only signing key
	receiver.SetSecurityEvents(recorder)
	require.Error(t, receiver.Subscribe())
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)
	subscriber.failOn = ""
	require.NoError(t, receiver.Subscribe())
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestReceiverSecurityEventsReportActivationFailureAndRecovery(t *testing.T) {
	subscriber := &fakeOpsSubscriber{}
	recorder := &securityEventRecorder{}
	receiver := opsmetrics.NewReceiver(subscriber, "cvn_aaaaaaaaaaaaaaaa", []byte("0123456789abcdef0123456789abcdef"), opsmetrics.NewCounters(), logger.New("test"), time.Now) // pragma: allowlist secret -- test-only signing key
	receiver.SetSecurityEvents(recorder)
	require.NoError(t, receiver.Subscribe())

	receiver.MarkDependencyDegraded()
	require.NoError(t, receiver.Subscribe())

	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestReceiverRejectEmitterRunsAfterStateUnlock(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	subscriber := &fakeOpsSubscriber{}
	counters := opsmetrics.NewCounters()
	receiver := opsmetrics.NewReceiver(subscriber, "cvn_aaaaaaaaaaaaaaaa", []byte("0123456789abcdef0123456789abcdef"), counters, logger.New("test"), func() time.Time { return now }) // pragma: allowlist secret -- test-only signing key
	require.NoError(t, receiver.Subscribe())
	subscriber.handlers[opsmetrics.HostSnapshotSubject](signedSnapshot(t, opsmetrics.SourceHost, now, 1))

	emitter := &reentrantSecurityEventEmitter{receiver: receiver}
	receiver.SetSecurityEvents(emitter)
	invalid := bytes.Replace(signedSnapshot(t, opsmetrics.SourceHost, now.Add(time.Second), 2), []byte(`"signature":"`), []byte(`"signature":"00`), 1)
	completed := make(chan struct{})
	go func() {
		subscriber.handlers[opsmetrics.HostSnapshotSubject](invalid)
		close(completed)
	}()

	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("re-entrant security emitter blocked receiver rejection")
	}
	require.True(t, emitter.ok)
	require.Equal(t, uint64(1), emitter.latest.Sequence)
	latest, ok := receiver.Latest(opsmetrics.SourceHost)
	require.True(t, ok)
	require.Equal(t, uint64(1), latest.Sequence)
	require.Equal(t, float64(1), counters.Snapshot()[opsmetrics.MetricSnapshotRejectionsTotal])
}

func TestReceiverSecurityEventSetterIsRaceSafe(t *testing.T) {
	subscriber := &fakeOpsSubscriber{}
	receiver := opsmetrics.NewReceiver(subscriber, "cvn_aaaaaaaaaaaaaaaa", []byte("0123456789abcdef0123456789abcdef"), opsmetrics.NewCounters(), logger.NewWithWriter(&bytes.Buffer{}), time.Now) // pragma: allowlist secret -- test-only signing key
	require.NoError(t, receiver.Subscribe())
	emitter := &concurrentSecurityEventEmitter{}
	receiver.SetSecurityEvents(emitter)
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			receiver.SetSecurityEvents(emitter)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			subscriber.handlers[opsmetrics.HostSnapshotSubject]([]byte("not-json"))
		}
	}()
	close(start)
	group.Wait()
	require.Equal(t, uint64(500), emitter.emitted.Load())
}
