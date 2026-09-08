package opsmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/nats-io/nats.go"
)

const (
	// HostSnapshotSubject carries signed host-agent operations snapshots.
	HostSnapshotSubject = "ops.metrics.host.v1"
	// MediaSnapshotSubject carries signed media-plane operations snapshots.
	MediaSnapshotSubject = "ops.metrics.media.v1"
)

// RejectionReason is a fixed, payload-free receiver rejection category.
type RejectionReason string

const (
	// RejectionDecode reports a payload that failed closed-schema decoding.
	RejectionDecode RejectionReason = "decode_failed"
	// RejectionSourceMismatch reports a valid source on the wrong fixed subject.
	RejectionSourceMismatch RejectionReason = "source_mismatch"
	// RejectionVerification reports failed signature, freshness, ownership, or replay validation.
	RejectionVerification RejectionReason = "verification_failed"
)

// Subscriber is the narrow NATS subscription surface required by Receiver.
type Subscriber interface {
	Subscribe(subject string, handler func([]byte)) (*nats.Subscription, error)
}

// Receiver verifies and retains the latest signed snapshot from each remote source.
type Receiver struct {
	subscriber Subscriber
	nodeID     string
	secret     []byte
	counters   *Counters
	log        *logger.Logger
	now        func() time.Time

	mu        sync.RWMutex
	latest    map[Source]Envelope
	positions map[Source]AcceptedPosition
	subs      []*nats.Subscription
	eventsMu  sync.RWMutex
	events    securityevent.Emitter
	degraded  bool
}

// NewReceiver creates a signed snapshot receiver.
func NewReceiver(subscriber Subscriber, nodeID string, secret []byte, counters *Counters, log *logger.Logger, now func() time.Time) *Receiver {
	if now == nil {
		now = time.Now
	}
	return &Receiver{
		subscriber: subscriber,
		nodeID:     nodeID,
		secret:     append([]byte(nil), secret...),
		counters:   counters,
		log:        log,
		now:        now,
		latest:     make(map[Source]Envelope, 2),
		positions:  make(map[Source]AcceptedPosition, 2),
		events:     securityevent.Discard,
	}
}

// SetSecurityEvents injects bounded Nightwatch telemetry without changing the receiver constructor.
func (r *Receiver) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	r.eventsMu.Lock()
	r.events = events
	r.eventsMu.Unlock()
}

func (r *Receiver) securityEventEmitter() securityevent.Emitter {
	r.eventsMu.RLock()
	events := r.events
	r.eventsMu.RUnlock()
	return events
}

func (r *Receiver) setDependencyState(degraded bool) {
	r.mu.Lock()
	changed := r.degraded != degraded
	r.degraded = degraded
	r.mu.Unlock()
	if !changed {
		return
	}
	if degraded {
		r.securityEventEmitter().Emit(context.Background(), securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable})
		return
	}
	r.securityEventEmitter().Emit(context.Background(), securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered})
}

// MarkDependencyDegraded reports a dependency failure after subscriptions have
// been registered but before they became active.
func (r *Receiver) MarkDependencyDegraded() {
	if r != nil {
		r.setDependencyState(true)
	}
}

// Subscribe installs handlers for the two fixed v1 operations subjects.
func (r *Receiver) Subscribe() error {
	if r.subscriber == nil {
		r.setDependencyState(true)
		return errors.New("operations metrics subscriber is required")
	}
	hostSubscription, err := r.subscriber.Subscribe(HostSnapshotSubject, func(raw []byte) {
		r.receive(SourceHost, raw)
	})
	if err != nil {
		r.setDependencyState(true)
		return fmt.Errorf("subscribe %s: %w", HostSnapshotSubject, err)
	}
	mediaSubscription, err := r.subscriber.Subscribe(MediaSnapshotSubject, func(raw []byte) {
		r.receive(SourceMedia, raw)
	})
	if err != nil {
		r.setDependencyState(true)
		if hostSubscription != nil {
			_ = hostSubscription.Unsubscribe()
		}
		return fmt.Errorf("subscribe %s: %w", MediaSnapshotSubject, err)
	}
	r.subs = []*nats.Subscription{hostSubscription, mediaSubscription}
	r.setDependencyState(false)
	return nil
}

// Unsubscribe removes every fixed-subject handler installed by Subscribe.
func (r *Receiver) Unsubscribe() error {
	if r == nil {
		return nil
	}
	var result error
	for _, subscription := range r.subs {
		if subscription == nil {
			continue
		}
		if err := subscription.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrBadSubscription) {
			result = errors.Join(result, err)
		}
	}
	r.subs = nil
	return result
}

func (r *Receiver) receive(expectedSource Source, raw []byte) {
	envelope, err := DecodeEnvelope(raw)
	if err != nil {
		r.reject(expectedSource, RejectionDecode)
		return
	}
	if envelope.Source != expectedSource {
		r.reject(expectedSource, RejectionSourceMismatch)
		return
	}
	if envelope.NodeID != r.nodeID {
		r.reject(expectedSource, RejectionVerification)
		return
	}

	r.mu.Lock()
	if err := VerifyEnvelope(envelope, r.secret, r.now(), r.positions[expectedSource]); err != nil {
		r.mu.Unlock()
		r.reject(expectedSource, RejectionVerification)
		return
	}

	r.positions[expectedSource] = AcceptedPosition{ObservedAt: envelope.ObservedAt, Sequence: envelope.Sequence}
	r.latest[expectedSource] = cloneEnvelope(envelope)
	r.mu.Unlock()
}

func (r *Receiver) reject(source Source, reason RejectionReason) {
	r.counters.Increment(MetricSnapshotRejectionsTotal)
	r.securityEventEmitter().Emit(context.Background(), securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonSignedTelemetryRejected})
	if r.log != nil {
		r.log.Warn("Rejected operations snapshot", "source", source, "reason", reason)
	}
}

// Latest returns an independent copy of the newest accepted snapshot for source.
func (r *Receiver) Latest(source Source) (Envelope, bool) {
	if r == nil {
		return Envelope{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	envelope, ok := r.latest[source]
	if !ok {
		return Envelope{}, false
	}
	return cloneEnvelope(envelope), true
}

func cloneEnvelope(envelope Envelope) Envelope {
	clone := envelope
	clone.Metrics = make(map[MetricKey]float64, len(envelope.Metrics))
	for key, value := range envelope.Metrics {
		clone.Metrics[key] = value
	}
	return clone
}
