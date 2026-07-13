package opsmetrics

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var agentTestSecret = []byte("0123456789abcdef0123456789abcdef") // pragma: allowlist secret -- test-only signing key

type snapshotReaderFunc func(context.Context) (map[MetricKey]float64, error)

func (f snapshotReaderFunc) Read(ctx context.Context) (map[MetricKey]float64, error) {
	return f(ctx)
}

type fakeAgentPublisher struct {
	mu        sync.Mutex
	subjects  []string
	envelopes []Envelope
	failCalls map[int]error
	published chan Envelope
}

func (p *fakeAgentPublisher) Publish(subject string, data interface{}) error {
	envelope, ok := data.(Envelope)
	if !ok {
		return errors.New("publisher received non-envelope payload")
	}
	p.mu.Lock()
	p.subjects = append(p.subjects, subject)
	p.envelopes = append(p.envelopes, envelope)
	call := len(p.envelopes)
	err := p.failCalls[call]
	p.mu.Unlock()
	if p.published != nil {
		p.published <- envelope
	}
	return err
}

func (p *fakeAgentPublisher) snapshot() ([]string, []Envelope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.subjects...), append([]Envelope(nil), p.envelopes...)
}

type fakeAgentTicker struct {
	ch      chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func (t *fakeAgentTicker) C() <-chan time.Time { return t.ch }

func (t *fakeAgentTicker) Stop() {
	t.once.Do(func() { close(t.stopped) })
}

func validAgentConfig() AgentConfig {
	return AgentConfig{
		Enabled:      true,
		NodeID:       "cvn_aaaaaaaaaaaaaaaa",
		SharedSecret: append([]byte(nil), agentTestSecret...),
		Interval:     15 * time.Second,
	}
}

func fixedHostReader() SnapshotReader {
	return snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
		return map[MetricKey]float64{
			MetricHostCPUPercent:    25,
			MetricHostMemoryPercent: 50,
		}, nil
	})
}

func fixedDockerReader() SnapshotReader {
	return snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
		return map[MetricKey]float64{
			MetricServiceControlPlaneRunning: 1,
			MetricServiceControlPlaneHealthy: 1,
		}, nil
	})
}

func TestAgentPublishesSignedMergedHostSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	publisher := &fakeAgentPublisher{failCalls: make(map[int]error)}
	agent, err := newAgent(validAgentConfig(), fixedHostReader(), fixedDockerReader(), publisher, log.New(&bytes.Buffer{}, "", 0), func() time.Time { return now }, nil)
	require.NoError(t, err)

	require.NoError(t, agent.collect(context.Background()))
	subjects, envelopes := publisher.snapshot()
	require.Equal(t, []string{HostSnapshotSubject}, subjects)
	require.Len(t, envelopes, 1)
	envelope := envelopes[0]
	require.Equal(t, EnvelopeVersion, envelope.Version)
	require.Equal(t, SourceHost, envelope.Source)
	require.Equal(t, "cvn_aaaaaaaaaaaaaaaa", envelope.NodeID)
	require.Equal(t, now, envelope.ObservedAt)
	require.Equal(t, uint64(1), envelope.Sequence)
	require.Equal(t, map[MetricKey]float64{
		MetricHostCPUPercent:             25,
		MetricHostMemoryPercent:          50,
		MetricServiceControlPlaneRunning: 1,
		MetricServiceControlPlaneHealthy: 1,
	}, envelope.Metrics)
	require.NoError(t, VerifyEnvelope(envelope, agentTestSecret, now, AcceptedPosition{}))
}

func TestAgentDropsFailedPublishWithoutRetryAndKeepsSequenceMonotonic(t *testing.T) {
	publisher := &fakeAgentPublisher{
		failCalls: map[int]error{2: errors.New("nats password secret-value\nforged")},
	}
	var logs bytes.Buffer
	agent, err := newAgent(validAgentConfig(), fixedHostReader(), fixedDockerReader(), publisher, log.New(&logs, "", 0), time.Now, nil)
	require.NoError(t, err)

	require.NoError(t, agent.collect(context.Background()))
	require.Error(t, agent.collect(context.Background()))
	require.NoError(t, agent.collect(context.Background()))
	_, envelopes := publisher.snapshot()
	require.Len(t, envelopes, 3)
	require.Equal(t, []uint64{1, 2, 3}, []uint64{envelopes[0].Sequence, envelopes[1].Sequence, envelopes[2].Sequence})
	require.Contains(t, logs.String(), "reason=publish")
	require.NotContains(t, logs.String(), "secret-value")
	require.NotContains(t, logs.String(), "forged")
}

func TestAgentRejectsOverlappingCollection(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	blockingHost := snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return map[MetricKey]float64{MetricHostCPUPercent: 10}, nil
	})
	publisher := &fakeAgentPublisher{failCalls: make(map[int]error)}
	agent, err := newAgent(validAgentConfig(), blockingHost, fixedDockerReader(), publisher, log.New(&bytes.Buffer{}, "", 0), time.Now, nil)
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() { firstDone <- agent.collect(context.Background()) }()
	<-entered
	require.ErrorIs(t, agent.collect(context.Background()), ErrAgentCollectionInProgress)
	close(release)
	require.NoError(t, <-firstDone)
	require.Equal(t, int32(1), calls.Load())
}

func TestAgentRunUsesConfiguredTickerAndStopsOnCancellation(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	ticker := &fakeAgentTicker{ch: make(chan time.Time, 1), stopped: make(chan struct{})}
	var gotInterval time.Duration
	publisher := &fakeAgentPublisher{failCalls: make(map[int]error), published: make(chan Envelope, 1)}
	agent, err := newAgent(
		validAgentConfig(),
		fixedHostReader(),
		fixedDockerReader(),
		publisher,
		log.New(&bytes.Buffer{}, "", 0),
		func() time.Time { return now },
		func(interval time.Duration) agentTicker {
			gotInterval = interval
			return ticker
		},
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- agent.Run(ctx) }()
	ticker.ch <- now
	published := <-publisher.published
	require.Equal(t, uint64(1), published.Sequence)
	cancel()
	require.NoError(t, <-runDone)
	<-ticker.stopped
	require.Equal(t, 15*time.Second, gotInterval)
}

func TestAgentLogsOnlyFixedFailureReasons(t *testing.T) {
	tests := []struct {
		name       string
		host       SnapshotReader
		docker     SnapshotReader
		publisher  AgentPublisher
		wantReason string
	}{
		{
			name: "host read",
			host: snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
				return nil, errors.New("host-secret\nforged")
			}),
			docker:     fixedDockerReader(),
			publisher:  &fakeAgentPublisher{failCalls: make(map[int]error)},
			wantReason: "host_read",
		},
		{
			name: "docker read",
			host: fixedHostReader(),
			docker: snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
				return nil, errors.New("docker-secret\nforged")
			}),
			publisher:  &fakeAgentPublisher{failCalls: make(map[int]error)},
			wantReason: "docker_read",
		},
		{
			name: "invalid metric",
			host: snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
				return map[MetricKey]float64{MetricMediaRoomsCurrent: 1}, nil
			}),
			docker:     fixedDockerReader(),
			publisher:  &fakeAgentPublisher{failCalls: make(map[int]error)},
			wantReason: "invalid_metrics",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			agent, err := newAgent(validAgentConfig(), tt.host, tt.docker, tt.publisher, log.New(&logs, "", 0), time.Now, nil)
			require.NoError(t, err)
			require.Error(t, agent.collect(context.Background()))
			require.Contains(t, logs.String(), "reason="+tt.wantReason)
			require.NotContains(t, logs.String(), "secret")
			require.NotContains(t, logs.String(), "forged")
		})
	}
}

func TestAgentRejectsDuplicateMetricOwnership(t *testing.T) {
	host := snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
		return map[MetricKey]float64{MetricHostCPUPercent: 10}, nil
	})
	docker := snapshotReaderFunc(func(context.Context) (map[MetricKey]float64, error) {
		return map[MetricKey]float64{MetricHostCPUPercent: 20}, nil
	})
	publisher := &fakeAgentPublisher{failCalls: make(map[int]error)}
	agent, err := newAgent(validAgentConfig(), host, docker, publisher, log.New(&bytes.Buffer{}, "", 0), time.Now, nil)
	require.NoError(t, err)

	require.ErrorContains(t, agent.collect(context.Background()), "duplicate metric")
	_, envelopes := publisher.snapshot()
	require.Empty(t, envelopes)
}

func TestNewAgentRejectsDisabledOrInvalidConfiguration(t *testing.T) {
	valid := validAgentConfig()
	tests := []struct {
		name      string
		config    AgentConfig
		host      SnapshotReader
		docker    SnapshotReader
		publisher AgentPublisher
	}{
		{name: "disabled", config: func() AgentConfig { cfg := valid; cfg.Enabled = false; return cfg }(), host: fixedHostReader(), docker: fixedDockerReader(), publisher: &fakeAgentPublisher{}},
		{name: "invalid node", config: func() AgentConfig { cfg := valid; cfg.NodeID = "host.example.com"; return cfg }(), host: fixedHostReader(), docker: fixedDockerReader(), publisher: &fakeAgentPublisher{}},
		{name: "short secret", config: func() AgentConfig { cfg := valid; cfg.SharedSecret = []byte("too-short-secret"); return cfg }(), host: fixedHostReader(), docker: fixedDockerReader(), publisher: &fakeAgentPublisher{}},
		{name: "interval too short", config: func() AgentConfig { cfg := valid; cfg.Interval = time.Second; return cfg }(), host: fixedHostReader(), docker: fixedDockerReader(), publisher: &fakeAgentPublisher{}},
		{name: "interval too long", config: func() AgentConfig { cfg := valid; cfg.Interval = 6 * time.Minute; return cfg }(), host: fixedHostReader(), docker: fixedDockerReader(), publisher: &fakeAgentPublisher{}},
		{name: "missing host reader", config: valid, host: nil, docker: fixedDockerReader(), publisher: &fakeAgentPublisher{}},
		{name: "missing Docker reader", config: valid, host: fixedHostReader(), docker: nil, publisher: &fakeAgentPublisher{}},
		{name: "missing publisher", config: valid, host: fixedHostReader(), docker: fixedDockerReader(), publisher: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, err := NewAgent(tt.config, tt.host, tt.docker, tt.publisher, log.New(&bytes.Buffer{}, "", 0))
			require.Error(t, err)
			require.Nil(t, agent)
			require.NotContains(t, err.Error(), string(valid.SharedSecret))
		})
	}
}
