package opsmetrics

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

type collectorTestStore struct {
	mu sync.Mutex

	batches       [][]Sample
	operations    []string
	rollupCutoffs []time.Time
	pruneCutoffs  [][2]time.Time
	writeErr      error
	rollupErr     error
	pruneErr      error
	writeStarted  chan struct{}
	writeRelease  chan struct{}
	events        chan string
}

func newCollectorTestStore() *collectorTestStore {
	return &collectorTestStore{events: make(chan string, 32)}
}

func (s *collectorTestStore) WriteSamples(ctx context.Context, samples []Sample) error {
	batch := append([]Sample(nil), samples...)
	s.mu.Lock()
	s.batches = append(s.batches, batch)
	s.operations = append(s.operations, "write")
	writeErr := s.writeErr
	started := s.writeStarted
	release := s.writeRelease
	s.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.events <- "write"
	return writeErr
}

func (s *collectorTestStore) Maintain(_ context.Context, now time.Time) error {
	s.mu.Lock()
	s.operations = append(s.operations, "rollup")
	s.rollupCutoffs = append(s.rollupCutoffs, now)
	err := s.rollupErr
	s.mu.Unlock()
	s.events <- "rollup"
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.operations = append(s.operations, "prune")
	s.pruneCutoffs = append(s.pruneCutoffs, [2]time.Time{now.Add(-rawSampleRetention), now.Add(-rollupRetention)})
	err = s.pruneErr
	s.mu.Unlock()
	s.events <- "prune"
	return err
}

func (s *collectorTestStore) snapshot() ([][]Sample, []string, []time.Time, [][2]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches := make([][]Sample, len(s.batches))
	for i := range s.batches {
		batches[i] = append([]Sample(nil), s.batches[i]...)
	}
	return batches, append([]string(nil), s.operations...), append([]time.Time(nil), s.rollupCutoffs...), append([][2]time.Time(nil), s.pruneCutoffs...)
}

type collectorTestProvider struct{ count int }

func (p collectorTestProvider) ConnectionCount() int { return p.count }

type collectorUserProvider struct {
	clients int
	users   int
}

func (provider collectorUserProvider) ConnectionCount() int    { return provider.clients }
func (provider collectorUserProvider) ConnectedUserCount() int { return provider.users }

type collectorAccountProvider struct {
	metrics map[MetricKey]float64
	err     error
}

func (provider collectorAccountProvider) AccountMetrics(context.Context, time.Time) (map[MetricKey]float64, error) {
	return provider.metrics, provider.err
}

type collectorTestTicker struct {
	c       chan time.Time
	stop    chan struct{}
	stopOne sync.Once
}

func newCollectorTestTicker() *collectorTestTicker {
	return &collectorTestTicker{c: make(chan time.Time), stop: make(chan struct{})}
}

func (t *collectorTestTicker) C() <-chan time.Time { return t.c }
func (t *collectorTestTicker) Stop()               { t.stopOne.Do(func() { close(t.stop) }) }
func (t *collectorTestTicker) tick(testingT *testing.T, at time.Time) {
	testingT.Helper()
	select {
	case t.c <- at:
	case <-time.After(time.Second):
		testingT.Fatal("collector did not receive tick")
	}
}

type collectorTestClock struct {
	mu     sync.Mutex
	now    time.Time
	ticker *collectorTestTicker
}

func (c *collectorTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *collectorTestClock) NewTicker(time.Duration) CollectorTicker { return c.ticker }
func (c *collectorTestClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

func newCollectorUnderTest(now time.Time, store *collectorTestStore) (*Collector, *collectorTestClock, *collectorTestTicker, context.CancelFunc, <-chan struct{}) {
	ticker := newCollectorTestTicker()
	clock := &collectorTestClock{now: now, ticker: ticker}
	receiver := NewReceiver(nil, "cvn_aaaaaaaaaaaaaaaa", nil, nil, nil, func() time.Time { return now })
	receiver.latest[SourceHost] = Envelope{
		Source: SourceHost, ObservedAt: now.Add(-time.Second), Sequence: 1,
		Metrics: map[MetricKey]float64{MetricHostCPUPercent: 42},
	}
	receiver.latest[SourceMedia] = Envelope{
		Source: SourceMedia, ObservedAt: now.Add(-time.Second), Sequence: 1,
		Metrics: map[MetricKey]float64{MetricMediaRoomsCurrent: 3},
	}
	counters := NewCounters()
	counters.Increment(MetricHTTPRequestsTotal)
	counters.Increment(MetricChannelMessagesTotal)
	collector := NewCollector(store, receiver, counters, collectorTestProvider{count: 7}, 15*time.Second, logger.NewWithWriter(&bytes.Buffer{}))
	collector.clock = clock
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		collector.Run(ctx)
		close(done)
	}()
	return collector, clock, ticker, cancel, done
}

func waitCollectorEvents(t *testing.T, store *collectorTestStore, expected ...string) {
	t.Helper()
	for _, event := range expected {
		select {
		case actual := <-store.events:
			require.Equal(t, event, actual)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for collector event %q", event)
		}
	}
}

func TestCollectorWritesOneBatchAndRollsUpBeforePruning(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	store := newCollectorTestStore()
	collector, clock, ticker, cancel, done := newCollectorUnderTest(now, store)
	t.Cleanup(cancel)

	ticker.tick(t, now)
	waitCollectorEvents(t, store, "write", "rollup", "prune")

	batches, operations, rollupCutoffs, pruneCutoffs := store.snapshot()
	require.Len(t, batches, 1)
	require.Equal(t, []string{"write", "rollup", "prune"}, operations)
	require.Equal(t, []time.Time{now}, rollupCutoffs)
	require.Equal(t, [][2]time.Time{{now.Add(-24 * time.Hour), now.Add(-8 * 24 * time.Hour)}}, pruneCutoffs)
	values := samplesByKey(batches[0])
	require.Equal(t, float64(42), values[MetricHostCPUPercent])
	require.Equal(t, float64(3), values[MetricMediaRoomsCurrent])
	require.Equal(t, float64(7), values[MetricWebSocketConnections])
	require.Equal(t, float64(1), values[MetricHTTPRequestsTotal])
	require.Equal(t, float64(1), values[MetricChannelMessagesTotal])
	require.Zero(t, values[MetricMediaUploadsTotal])
	require.Len(t, values, 10)
	require.Eventually(t, func() bool {
		return !collector.busy.Load()
	}, time.Second, time.Millisecond)

	clock.set(now.Add(15 * time.Second))
	ticker.tick(t, now.Add(15*time.Second))
	waitCollectorEvents(t, store, "write")
	require.Never(t, func() bool {
		_, operations, _, _ := store.snapshot()
		return len(operations) > 4
	}, 100*time.Millisecond, time.Millisecond)
	batches, operations, _, _ = store.snapshot()
	require.Equal(t, []string{"write", "rollup", "prune", "write"}, operations)
	require.Len(t, batches, 2)
	secondValues := samplesByKey(batches[1])
	require.NotContains(t, secondValues, MetricHostCPUPercent)
	require.NotContains(t, secondValues, MetricMediaRoomsCurrent)
	require.Len(t, secondValues, 8)

	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

func TestCollectorAddsDistinctUsersAndAccountMetrics(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	accountMetrics := map[MetricKey]float64{
		MetricRegisteredUsersCurrent:      10,
		MetricPendingRegistrationsCurrent: 2,
		MetricActiveSessionsCurrent:       4,
		MetricActiveUsers24H:              3,
		MetricActiveUsers7D:               7,
		MetricActiveUsers15D:              8,
		MetricActiveUsers30D:              9,
	}
	collector := NewCollector(
		newCollectorTestStore(),
		nil,
		NewCounters(),
		collectorUserProvider{clients: 5, users: 3},
		15*time.Second,
		logger.NewWithWriter(&bytes.Buffer{}),
		collectorAccountProvider{metrics: accountMetrics},
	)

	values := samplesByKey(collector.snapshot(now))
	require.Equal(t, float64(5), values[MetricWebSocketConnections])
	require.Equal(t, float64(3), values[MetricUsersOnlineCurrent])
	for key, value := range accountMetrics {
		require.Equal(t, value, values[key])
	}
	require.Len(t, values, 16)
}

func TestCollectorPreservesPartialAccountMetricsOnProviderFailure(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	output := &bytes.Buffer{}
	providerErr := errors.New("account query unavailable")
	collector := NewCollector(
		newCollectorTestStore(),
		nil,
		NewCounters(),
		nil,
		15*time.Second,
		logger.NewWithWriter(output),
		collectorAccountProvider{
			metrics: map[MetricKey]float64{MetricRegisteredUsersCurrent: 12},
			err:     providerErr,
		},
	)

	values := samplesByKey(collector.snapshot(now))
	require.Equal(t, float64(12), values[MetricRegisteredUsersCurrent])
	require.Contains(t, output.String(), "Failed to collect aggregate account metrics")
}

func TestCollectorAllowsOneMissedRemoteIntervalButRejectsOlderSnapshots(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name       string
		observedAt time.Time
		wantHost   bool
	}{
		{name: "one delayed interval", observedAt: now.Add(-29 * time.Second), wantHost: true},
		{name: "older than two intervals", observedAt: now.Add(-31 * time.Second), wantHost: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			receiver := NewReceiver(nil, "cvn_aaaaaaaaaaaaaaaa", nil, nil, nil, func() time.Time { return now })
			receiver.latest[SourceHost] = Envelope{
				Source: SourceHost, ObservedAt: testCase.observedAt, Sequence: 1,
				Metrics: map[MetricKey]float64{MetricHostCPUPercent: 42},
			}
			collector := NewCollector(
				newCollectorTestStore(), receiver, NewCounters(), nil, 15*time.Second,
				logger.NewWithWriter(&bytes.Buffer{}),
			)

			values := samplesByKey(collector.snapshot(now))
			if testCase.wantHost {
				require.Equal(t, 42.0, values[MetricHostCPUPercent])
			} else {
				require.NotContains(t, values, MetricHostCPUPercent)
			}
		})
	}
}

func TestCollectorDropsFailedWriteIntervalAndContinuesRetention(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	store := newCollectorTestStore()
	store.writeErr = errors.New("forced write failure")
	_, clock, ticker, cancel, _ := newCollectorUnderTest(now, store)
	t.Cleanup(cancel)

	ticker.tick(t, now)
	waitCollectorEvents(t, store, "write", "rollup", "prune")

	store.mu.Lock()
	store.writeErr = nil
	store.mu.Unlock()
	clock.set(now.Add(15 * time.Second))
	ticker.tick(t, now.Add(15*time.Second))
	waitCollectorEvents(t, store, "write")

	batches, _, _, _ := store.snapshot()
	require.Len(t, batches, 2)
	secondValues := samplesByKey(batches[1])
	require.NotContains(t, secondValues, MetricHostCPUPercent)
	require.NotContains(t, secondValues, MetricMediaRoomsCurrent)
}

func TestCollectorRunsRetentionAgainInNextUTCHour(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 59, 50, 0, time.UTC)
	store := newCollectorTestStore()
	_, clock, ticker, cancel, _ := newCollectorUnderTest(now, store)
	t.Cleanup(cancel)

	ticker.tick(t, now)
	waitCollectorEvents(t, store, "write", "rollup", "prune")
	clock.set(now.Add(15 * time.Second))
	ticker.tick(t, now.Add(15*time.Second))
	waitCollectorEvents(t, store, "write", "rollup", "prune")

	_, operations, _, _ := store.snapshot()
	require.Equal(t, []string{"write", "rollup", "prune", "write", "rollup", "prune"}, operations)
}

func TestCollectorDoesNotPruneAfterFailedRollup(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	store := newCollectorTestStore()
	store.rollupErr = errors.New("forced rollup failure")
	_, _, ticker, cancel, _ := newCollectorUnderTest(now, store)
	t.Cleanup(cancel)

	ticker.tick(t, now)
	waitCollectorEvents(t, store, "write", "rollup")
	require.Never(t, func() bool {
		_, operations, _, _ := store.snapshot()
		return len(operations) > 2
	}, 100*time.Millisecond, time.Millisecond)
}

func TestCollectorSkipsOverlappingTick(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	store := newCollectorTestStore()
	store.writeStarted = make(chan struct{}, 1)
	store.writeRelease = make(chan struct{})
	_, clock, ticker, cancel, _ := newCollectorUnderTest(now, store)
	t.Cleanup(cancel)

	ticker.tick(t, now)
	select {
	case <-store.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("first collection did not start")
	}
	clock.set(now.Add(15 * time.Second))
	ticker.tick(t, now.Add(15*time.Second))
	require.Never(t, func() bool {
		batches, _, _, _ := store.snapshot()
		return len(batches) > 1
	}, 100*time.Millisecond, time.Millisecond)

	close(store.writeRelease)
	waitCollectorEvents(t, store, "write", "rollup", "prune")
}

func TestCollectorCancellationStopsTicker(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	store := newCollectorTestStore()
	_, _, ticker, cancel, done := newCollectorUnderTest(now, store)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector did not stop after cancellation")
	}
	select {
	case <-ticker.stop:
	case <-time.After(time.Second):
		t.Fatal("collector did not stop ticker")
	}
}

func samplesByKey(samples []Sample) map[MetricKey]float64 {
	values := make(map[MetricKey]float64, len(samples))
	for _, sample := range samples {
		values[sample.Key] = sample.Value
	}
	return values
}
