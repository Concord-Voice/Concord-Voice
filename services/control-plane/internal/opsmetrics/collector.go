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

// connectionSnapshotProvider supplies both local connection counts atomically.
type connectionSnapshotProvider interface {
	ConnectionCounts() (connections, users int)
}

// AccountMetricProvider reduces account state to the fixed account metric keys.
type AccountMetricProvider interface {
	AccountMetrics(context.Context, time.Time) (map[MetricKey]float64, error)
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
	store              MetricStore
	receiver           *Receiver
	counters           *Counters
	connections        ConnectionProvider
	connectionSnapshot connectionSnapshotProvider
	accounts           AccountMetricProvider
	interval           time.Duration
	log                *logger.Logger
	clock              collectorClock

	busy     atomic.Bool
	active   sync.WaitGroup
	consumed map[Source]AcceptedPosition
	retained time.Time
}

// NewCollector creates a periodic operations metrics collector.
func NewCollector(store MetricStore, receiver *Receiver, counters *Counters, connections ConnectionProvider, interval time.Duration, log *logger.Logger, accounts ...AccountMetricProvider) *Collector {
	collector := &Collector{
		store:       store,
		receiver:    receiver,
		counters:    counters,
		connections: connections,
		interval:    interval,
		log:         log,
		clock:       systemCollectorClock{},
		consumed:    make(map[Source]AcceptedPosition, 2),
	}
	if snapshot, ok := connections.(connectionSnapshotProvider); ok {
		collector.connectionSnapshot = snapshot
	}
	if len(accounts) > 0 {
		collector.accounts = accounts[0]
	}
	return collector
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
	samples := collector.snapshotContext(ctx, collectedAt)
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
	return collector.snapshotContext(context.Background(), collectedAt)
}

func (collector *Collector) snapshotContext(ctx context.Context, collectedAt time.Time) []Sample {
	samples := make([]Sample, 0, CatalogSize())
	samples = collector.appendRemoteSamples(samples, collectedAt)

	for key, value := range collector.counters.Snapshot() {
		samples = append(samples, Sample{Key: key, Value: value, Source: SourceControl})
	}
	if collector.connectionSnapshot != nil {
		connections, users := collector.connectionSnapshot.ConnectionCounts()
		samples = append(samples,
			Sample{Key: MetricWebSocketConnections, Value: float64(connections), Source: SourceControl},
			Sample{Key: MetricUsersOnlineCurrent, Value: float64(users), Source: SourceControl},
		)
	} else if collector.connections != nil {
		samples = append(samples, Sample{
			Key:    MetricWebSocketConnections,
			Value:  float64(collector.connections.ConnectionCount()),
			Source: SourceControl,
		})
	}
	samples = collector.appendAccountSamples(ctx, samples, collectedAt)

	sort.Slice(samples, func(i, j int) bool { return samples[i].Key < samples[j].Key })
	return samples
}

func (collector *Collector) appendRemoteSamples(samples []Sample, collectedAt time.Time) []Sample {
	if collector.receiver == nil {
		return samples
	}
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
	return samples
}

func (collector *Collector) appendAccountSamples(ctx context.Context, samples []Sample, collectedAt time.Time) []Sample {
	if collector.accounts == nil {
		return samples
	}
	accountMetrics, err := collector.accounts.AccountMetrics(ctx, collectedAt)
	if err != nil {
		collector.logError("Failed to collect aggregate account metrics", err)
	}
	for key, value := range accountMetrics {
		if !isAccountProviderMetric(key) {
			continue
		}
		sample := Sample{Key: key, Value: value, Source: SourceControl}
		if ValidateSample(sample) == nil {
			samples = append(samples, sample)
		}
	}
	return samples
}

func isAccountProviderMetric(key MetricKey) bool {
	switch key {
	case MetricRegisteredUsersCurrent,
		MetricPendingRegistrationsCurrent,
		MetricActiveSessionsCurrent,
		MetricActiveUsers24H,
		MetricActiveUsers7D,
		MetricActiveUsers15D,
		MetricActiveUsers30D:
		return true
	default:
		return false
	}
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
