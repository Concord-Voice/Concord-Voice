package websocket

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

type blockingSecurityEventEmitter struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	firstOnce    sync.Once
	mu           sync.Mutex
	events       []securityevent.Event
}

func newBlockingSecurityEventEmitter() *blockingSecurityEventEmitter {
	return &blockingSecurityEventEmitter{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (e *blockingSecurityEventEmitter) Emit(_ context.Context, event securityevent.Event) {
	if event.Outcome == securityevent.OutcomeDegraded {
		e.firstOnce.Do(func() { close(e.firstStarted) })
		<-e.releaseFirst
	}
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *blockingSecurityEventEmitter) snapshot() []securityevent.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]securityevent.Event(nil), e.events...)
}

func requireSecurityEventDrain(t *testing.T, hub *Hub) {
	t.Helper()
	hub.securityEventsMu.Lock()
	drainDone := hub.securityEventsDrainDone
	hub.securityEventsMu.Unlock()
	if drainDone == nil {
		return
	}
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("security-event drain did not complete")
	}
}

type concurrentSecurityEventEmitter struct{ emitted atomic.Uint64 }

func (e *concurrentSecurityEventEmitter) Emit(_ context.Context, _ securityevent.Event) {
	e.emitted.Add(1)
}

type reentrantSecurityEventEmitter struct {
	hub     *Hub
	entered chan struct{}
	once    sync.Once
	mu      sync.Mutex
	events  []securityevent.Event
}

func (e *reentrantSecurityEventEmitter) Emit(_ context.Context, event securityevent.Event) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
	if event.Outcome == securityevent.OutcomeDegraded {
		e.once.Do(func() {
			probe := e.hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
			e.hub.completeSecurityEventProbeSuccess(probe)
			close(e.entered)
		})
	}
}

func (e *reentrantSecurityEventEmitter) snapshot() []securityevent.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]securityevent.Event(nil), e.events...)
}

func TestChannelDeliveryMissingCheckerReportsDependencyUnavailable(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Hub)
	}{
		{name: "missing checker", run: func(hub *Hub) { hub.dispatchChannelDelivery(nightwatchChannelDeliveryRequest()) }},
		{name: "checker error", run: func(hub *Hub) {
			hub.SetChannelPermissionChecker(staticChannelPermissionChecker{err: errors.New("unavailable")})
			hub.clientHasChannelPermission(context.Background(), uuid.New(), uuid.New(), newTestClient(hub, uuid.New()), 1)
		}},
		{name: "result queue unavailable", run: func(hub *Hub) {
			hub.SetChannelPermissionChecker(staticChannelPermissionChecker{allowed: true})
			hub.channelDeliveryResults = nil
			hub.dispatchChannelDelivery(nightwatchChannelDeliveryRequest())
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := NewHub(nil, nil)
			recorder := &securityEventRecorder{}
			hub.SetSecurityEvents(recorder)
			test.run(hub)
			requireSecurityEventDrain(t, hub)
			require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)
		})
	}
}

func TestChannelDeliverySecurityEventsReportQueueRecovery(t *testing.T) {
	hub := NewHub(nil, nil)
	recorder := &securityEventRecorder{}
	hub.SetSecurityEvents(recorder)
	checker := newBlockingChannelPermissionChecker(true)
	hub.SetChannelPermissionChecker(checker)
	hub.channelDeliveryResults = nil
	hub.dispatchChannelDelivery(nightwatchChannelDeliveryRequest())
	hub.channelDeliveryResults = make(chan channelDeliveryResult, 1)
	hub.dispatchChannelDelivery(nightwatchChannelDeliveryRequest())
	select {
	case <-checker.entered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected permission checker to be called")
	}
	// Queue availability alone is not a successful permission check; recovery
	// must wait until the asynchronous batch completes successfully.
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
	}, recorder.events)
	close(checker.release)
	applyAsyncChannelDelivery(t, hub)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestChannelDeliveryMixedBatchFailureDoesNotRestoreDependency(t *testing.T) {
	hub := NewHub(nil, nil)
	recorder := &securityEventRecorder{}
	hub.SetSecurityEvents(recorder)
	hub.SetChannelPermissionChecker(&mixedChannelPermissionChecker{})
	recipients := []channelDeliveryRecipient{{clientID: uuid.New(), userID: uuid.New()}, {clientID: uuid.New(), userID: uuid.New()}}
	hub.dispatchChannelDelivery(channelDeliveryRequest{serverID: uuid.New(), channelID: uuid.New(), viewPerm: 1, recipients: recipients})
	applyAsyncChannelDelivery(t, hub)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)

	hub.SetChannelPermissionChecker(staticChannelPermissionChecker{allowed: true})
	allowed, definitive := hub.clientHasChannelPermission(context.Background(), uuid.New(), uuid.New(), newTestClient(hub, uuid.New()), 1)
	require.True(t, allowed)
	require.True(t, definitive)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)

	hub.dispatchChannelDelivery(nightwatchChannelDeliveryRequest())
	applyAsyncChannelDelivery(t, hub)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)

	msg := IncomingMessage{ClientID: uuid.New()}
	chCtx := &channelContext{serverUUID: uuid.New()}
	require.True(t, hub.authorizeChannelPermissions(msg, uuid.New(), chCtx, uuid.New(), []int64{permViewTextChannels}, "denied", true))
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestChannelDeliverySecurityEventTransitionsRemainOrdered(t *testing.T) {
	emitter := newBlockingSecurityEventEmitter()
	hub := NewHub(nil, nil)
	hub.SetSecurityEvents(emitter)
	degradedProbe := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
	degradedDone := make(chan struct{})
	go func() {
		hub.completeSecurityEventProbeFailure(degradedProbe)
		close(degradedDone)
	}()
	<-emitter.firstStarted
	restoredDone := make(chan struct{})
	go func() {
		restoredProbe := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
		hub.completeSecurityEventProbeSuccess(restoredProbe)
		close(restoredDone)
	}()
	select {
	case <-restoredDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("recovery transition was not queued while the degradation event was blocked")
	}
	close(emitter.releaseFirst)
	<-degradedDone
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, emitter.snapshot())
}

func TestSecurityEventTransitionDoesNotBlockCallerAndShutdownDrains(t *testing.T) {
	hub := NewHub(nil, nil)
	emitter := newBlockingSecurityEventEmitter()
	hub.SetSecurityEvents(emitter)
	probe := securityEventProbe{source: securityEventSourceChannelDelivery, generation: 1}

	completed := make(chan struct{})
	go func() {
		hub.completeSecurityEventProbeFailure(probe)
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("security-event transition blocked its caller")
	}
	<-emitter.firstStarted

	go hub.Run()
	shutdownComplete := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(shutdownComplete)
	}()
	select {
	case <-shutdownComplete:
		t.Fatal("shutdown returned before the security-event drain")
	case <-time.After(50 * time.Millisecond):
	}
	close(emitter.releaseFirst)
	select {
	case <-shutdownComplete:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete after the security-event drain")
	}
}

func TestSecurityEventShutdownRacesWithNewTransitionWithoutWaitGroupHazard(t *testing.T) {
	hub := NewHub(nil, nil)
	emitter := newBlockingSecurityEventEmitter()
	hub.SetSecurityEvents(emitter)
	first := hub.beginSecurityEventProbe(securityEventSourceChannelDelivery)
	hub.completeSecurityEventProbeFailure(first)
	<-emitter.firstStarted

	go hub.Run()
	shutdownDone := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(shutdownDone)
	}()

	transitionDone := make(chan struct{})
	go func() {
		probe := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
		hub.completeSecurityEventProbeFailure(probe)
		close(transitionDone)
	}()
	select {
	case <-transitionDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent transition blocked during shutdown")
	}
	close(emitter.releaseFirst)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wait for its in-flight security-event drain")
	}
}

func TestChannelDeliverySecurityEventQueueCoalescesBlockedFlapsToFinalState(t *testing.T) {
	emitter := newBlockingSecurityEventEmitter()
	hub := NewHub(nil, nil)
	hub.SetSecurityEvents(emitter)
	firstDone := make(chan struct{})
	go func() {
		hub.completeSecurityEventProbeFailure(hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority))
		close(firstDone)
	}()
	<-emitter.firstStarted

	for range 8 {
		restored := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
		hub.completeSecurityEventProbeSuccess(restored)
		degraded := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
		hub.completeSecurityEventProbeFailure(degraded)
		requireSecurityEventEmissionQueueBounded(t, hub)
	}
	final := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
	hub.completeSecurityEventProbeSuccess(final)
	requireSecurityEventEmissionQueueBounded(t, hub)

	close(emitter.releaseFirst)
	select {
	case <-firstDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("security-event transition drain did not complete")
	}
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, emitter.snapshot(), "the final health state must survive coalescing")
}

func TestSecurityEventInitialEmissionIsReservedBeforeDrain(t *testing.T) {
	hub := NewHub(nil, nil)
	recorder := &securityEventRecorder{}
	hub.SetSecurityEvents(recorder)
	hub.securityEventsMu.Lock()
	hub.securityEventSourceFailureActive[securityEventSourcePermissionAuthority] = true
	initial, drain := hub.updateSecurityEventsDegradedLocked()
	require.True(t, drain)
	require.Equal(t, securityevent.OutcomeDegraded, initial.event.Outcome)
	hub.securityEventSourceFailureActive[securityEventSourcePermissionAuthority] = false
	_, queuedDrain := hub.updateSecurityEventsDegradedLocked()
	require.False(t, queuedDrain)
	require.Len(t, hub.securityEventEmissionQueue, 1)
	hub.securityEventsMu.Unlock()

	hub.drainSecurityEventEmissions(initial)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func requireSecurityEventEmissionQueueBounded(t *testing.T, hub *Hub) {
	t.Helper()
	hub.securityEventsMu.Lock()
	defer hub.securityEventsMu.Unlock()
	require.LessOrEqual(t, len(hub.securityEventEmissionQueue), securityEventEmissionQueueLimit)
}

func TestChannelDeliverySecurityEventEmissionReleasesStateMutex(t *testing.T) {
	hub := NewHub(nil, nil)
	emitter := &reentrantSecurityEventEmitter{hub: hub, entered: make(chan struct{})}
	hub.SetSecurityEvents(emitter)
	done := make(chan struct{})
	go func() {
		hub.completeSecurityEventProbeFailure(hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority))
		close(done)
	}()
	select {
	case <-emitter.entered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reentrant security-event emitter did not complete a transition")
	}
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reentrant security-event emitter blocked transition completion")
	}
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, emitter.snapshot())
}

func TestChannelPermissionSecurityEventUncachedFailureSurvivesCachedDeliveryRecovery(t *testing.T) {
	hub := NewHub(nil, nil)
	recorder := &securityEventRecorder{}
	hub.SetSecurityEvents(recorder)
	checker := &splitChannelPermissionChecker{cachedEntered: make(chan struct{}), releaseCached: make(chan struct{}), uncachedFailure: true}
	hub.SetChannelPermissionChecker(checker)
	hub.dispatchChannelDelivery(nightwatchChannelDeliveryRequest())
	select {
	case <-checker.cachedEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected cached delivery permission check")
	}

	msg := IncomingMessage{ClientID: uuid.New()}
	chCtx := &channelContext{serverUUID: uuid.New()}
	require.False(t, hub.authorizeChannelPermissions(msg, uuid.New(), chCtx, uuid.New(), []int64{permViewTextChannels}, "denied", true))
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)

	close(checker.releaseCached)
	applyAsyncChannelDelivery(t, hub)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)

	checker.uncachedFailure = false
	require.True(t, hub.authorizeChannelPermissions(msg, uuid.New(), chCtx, uuid.New(), []int64{permViewTextChannels}, "denied", true))
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestChannelPermissionSecurityEventOlderSuccessCannotClearLaterFailure(t *testing.T) {
	hub := NewHub(nil, nil)
	recorder := &securityEventRecorder{}
	hub.SetSecurityEvents(recorder)

	earlierSuccess := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
	laterFailure := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
	hub.completeSecurityEventProbeFailure(laterFailure)
	hub.completeSecurityEventProbeSuccess(earlierSuccess)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)

	freshSuccess := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
	hub.completeSecurityEventProbeSuccess(freshSuccess)
	requireSecurityEventDrain(t, hub)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

type splitChannelPermissionChecker struct {
	cachedEntered   chan struct{}
	releaseCached   chan struct{}
	cachedOnce      sync.Once
	uncachedFailure bool
}

func (c *splitChannelPermissionChecker) HasChannelPermission(context.Context, string, string, string, int64) (bool, error) {
	c.cachedOnce.Do(func() { close(c.cachedEntered) })
	<-c.releaseCached
	return true, nil
}

func (c *splitChannelPermissionChecker) HasChannelPermissionsUncached(context.Context, string, string, string, ...int64) (bool, error) {
	if c.uncachedFailure {
		return false, errors.New("uncached permission backend unavailable")
	}
	return true, nil
}

type mixedChannelPermissionChecker struct{ calls int }

func (c *mixedChannelPermissionChecker) HasChannelPermission(context.Context, string, string, string, int64) (bool, error) {
	c.calls++
	if c.calls == 2 {
		return false, errors.New("permission backend unavailable")
	}
	return true, nil
}

func (c *mixedChannelPermissionChecker) HasChannelPermissionsUncached(context.Context, string, string, string, ...int64) (bool, error) {
	return true, nil
}

func TestChannelDeliverySecurityEventSetterIsRaceSafe(t *testing.T) {
	hub := NewHub(nil, nil)
	emitter := &concurrentSecurityEventEmitter{}
	hub.SetSecurityEvents(emitter)
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			hub.SetSecurityEvents(emitter)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for index := range 500 {
			probe := hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority)
			if index%2 == 0 {
				hub.completeSecurityEventProbeFailure(probe)
			} else {
				hub.completeSecurityEventProbeSuccess(probe)
			}
		}
	}()
	close(start)
	group.Wait()
	require.Positive(t, emitter.emitted.Load())
}

func nightwatchChannelDeliveryRequest() channelDeliveryRequest {
	return channelDeliveryRequest{serverID: uuid.New(), channelID: uuid.New(), viewPerm: 1, recipients: []channelDeliveryRecipient{{clientID: uuid.New(), userID: uuid.New()}}}
}
