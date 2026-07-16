package opsmetrics

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ConnectionProvider supplies the current aggregate WebSocket connection count.
type ConnectionProvider interface {
	ConnectionCount() int
}

// CollectorTicker is the injectable ticker boundary used by Collector.Run.
type CollectorTicker interface {
	C() <-chan time.Time
	Stop()
}

type collectorClock interface {
	Now() time.Time
	NewTicker(time.Duration) CollectorTicker
}

type systemCollectorClock struct{}

func (systemCollectorClock) Now() time.Time { return time.Now() }
func (systemCollectorClock) NewTicker(interval time.Duration) CollectorTicker {
	return systemCollectorTicker{ticker: time.NewTicker(interval)}
}

type systemCollectorTicker struct{ ticker *time.Ticker }

func (ticker systemCollectorTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker systemCollectorTicker) Stop()               { ticker.ticker.Stop() }

// Collector combines fresh remote snapshots and local aggregates into store batches.
type Collector struct {
	store       MetricStore
	receiver    *Receiver
	counters    *Counters
	connections ConnectionProvider
	interval    time.Duration
	log         *logger.Logger
	clock       collectorClock

	busy     atomic.Bool
	active   sync.WaitGroup
	consumed map[Source]AcceptedPosition
	retained time.Time
}

// NewCollector creates a periodic operations metrics collector.
func NewCollector(store MetricStore, receiver *Receiver, counters *Counters, connections ConnectionProvider, interval time.Duration, log *logger.Logger) *Collector {
	return &Collector{
		store:       store,
		receiver:    receiver,
		counters:    counters,
		connections: connections,
		interval:    interval,
		log:         log,
		clock:       systemCollectorClock{},
		consumed:    make(map[Source]AcceptedPosition, 2),
	}
}

// Run collects on ticks until cancellation. An in-flight tick suppresses overlap.
func (collector *Collector) Run(ctx context.Context) {
	if collector == nil || collector.store == nil || collector.interval <= 0 {
		return
	}
	ticker := collector.clock.NewTicker(collector.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			collector.active.Wait()
			return
		case <-ticker.C():
			if !collector.busy.CompareAndSwap(false, true) {
				continue
			}
			collectedAt := collector.clock.Now().UTC()
			collector.active.Add(1)
			go func() {
				defer collector.active.Done()
				defer collector.busy.Store(false)
				collector.collect(ctx, collectedAt)
			}()
		}
	}
}

func (collector *Collector) collect(ctx context.Context, collectedAt time.Time) {
	if ctx.Err() != nil {
		return
	}
	samples := collector.snapshot(collectedAt)
	if len(samples) > 0 {
		if err := collector.store.WriteSamples(ctx, samples); err != nil {
			collector.logError("Failed to write operations metric samples", err)
		}
	}
	if ctx.Err() != nil {
		return
	}
	retentionHour := collectedAt.UTC().Truncate(time.Hour)
	if !collector.retained.IsZero() && !retentionHour.After(collector.retained) {
		return
	}
	if err := collector.store.Maintain(ctx, collectedAt); err != nil {
		collector.logError("Failed to maintain operations metric retention", err)
		return
	}
	collector.retained = retentionHour
}

func (collector *Collector) snapshot(collectedAt time.Time) []Sample {
	samples := make([]Sample, 0, CatalogSize())
	if collector.receiver != nil {
		for _, source := range []Source{SourceHost, SourceMedia} {
			envelope, ok := collector.receiver.Latest(source)
			if !ok || !positionAfter(envelope, collector.consumed[source]) {
				continue
			}
			collector.consumed[source] = AcceptedPosition{ObservedAt: envelope.ObservedAt, Sequence: envelope.Sequence}
			if envelope.ObservedAt.Before(collectedAt.Add(-2 * collector.interval)) {
				continue
			}
			for key, value := range envelope.Metrics {
				samples = append(samples, Sample{Key: key, Value: value, Source: source})
			}
		}
	}

	for key, value := range collector.counters.Snapshot() {
		samples = append(samples, Sample{Key: key, Value: value, Source: SourceControl})
	}
	if collector.connections != nil {
		samples = append(samples, Sample{
			Key:    MetricWebSocketConnections,
			Value:  float64(collector.connections.ConnectionCount()),
			Source: SourceControl,
		})
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i].Key < samples[j].Key })
	return samples
}

func positionAfter(envelope Envelope, previous AcceptedPosition) bool {
	return previous.ObservedAt.IsZero() || envelope.ObservedAt.After(previous.ObservedAt) ||
		(envelope.ObservedAt.Equal(previous.ObservedAt) && envelope.Sequence > previous.Sequence)
}

func (collector *Collector) logError(message string, err error) {
	if collector.log != nil {
		collector.log.Error(message, "error", err)
	}
}
