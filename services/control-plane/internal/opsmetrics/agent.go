package opsmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const (
	minimumAgentInterval = 5 * time.Second
	maximumAgentInterval = 5 * time.Minute
)

// ErrAgentCollectionInProgress reports a skipped overlapping collection.
var ErrAgentCollectionInProgress = errors.New("operations metrics collection already in progress")

// SnapshotReader returns one closed-schema aggregate snapshot fragment.
type SnapshotReader interface {
	Read(context.Context) (map[MetricKey]float64, error)
}

// AgentPublisher is the existing NATS wrapper surface used by the agent.
type AgentPublisher interface {
	Publish(subject string, data interface{}) error
}

// AgentLogger accepts fixed, payload-free collection status messages.
type AgentLogger interface {
	Printf(format string, values ...interface{})
}

// AgentConfig contains the validated runtime values needed by the host agent.
type AgentConfig struct {
	Enabled      bool
	NodeID       string
	SharedSecret []byte
	Interval     time.Duration
}

type agentTicker interface {
	C() <-chan time.Time
	Stop()
}

type realAgentTicker struct {
	ticker *time.Ticker
}

func (t realAgentTicker) C() <-chan time.Time { return t.ticker.C }
func (t realAgentTicker) Stop()               { t.ticker.Stop() }

type agentTickerFactory func(time.Duration) agentTicker

// Agent periodically publishes signed host and allowlisted-container snapshots.
type Agent struct {
	config        AgentConfig
	hostReader    SnapshotReader
	dockerReader  SnapshotReader
	publisher     AgentPublisher
	log           AgentLogger
	now           func() time.Time
	tickerFactory agentTickerFactory
	collecting    atomic.Bool
	sequence      uint64
}

// NewAgent creates a validated host metrics agent.
func NewAgent(config AgentConfig, hostReader, dockerReader SnapshotReader, publisher AgentPublisher, logger AgentLogger) (*Agent, error) {
	return newAgent(config, hostReader, dockerReader, publisher, logger, time.Now, func(interval time.Duration) agentTicker {
		return realAgentTicker{ticker: time.NewTicker(interval)}
	})
}

func newAgent(
	config AgentConfig,
	hostReader SnapshotReader,
	dockerReader SnapshotReader,
	publisher AgentPublisher,
	logger AgentLogger,
	now func() time.Time,
	tickerFactory agentTickerFactory,
) (*Agent, error) {
	if !config.Enabled {
		return nil, errors.New("operations metrics agent is disabled")
	}
	if err := ValidateNodeID(config.NodeID); err != nil {
		return nil, fmt.Errorf("invalid operations metrics node id: %w", err)
	}
	if len(config.SharedSecret) < 32 {
		return nil, errors.New("operations metrics shared secret must be at least 32 bytes")
	}
	if config.Interval < minimumAgentInterval || config.Interval > maximumAgentInterval {
		return nil, errors.New("operations metrics interval must be from 5s through 5m")
	}
	if hostReader == nil {
		return nil, errors.New("host metrics reader is required")
	}
	if dockerReader == nil {
		return nil, errors.New("docker metrics reader is required")
	}
	if publisher == nil {
		return nil, errors.New("operations metrics publisher is required")
	}
	if now == nil {
		now = time.Now
	}
	if tickerFactory == nil {
		tickerFactory = func(interval time.Duration) agentTicker {
			return realAgentTicker{ticker: time.NewTicker(interval)}
		}
	}
	config.SharedSecret = append([]byte(nil), config.SharedSecret...)
	return &Agent{
		config:        config,
		hostReader:    hostReader,
		dockerReader:  dockerReader,
		publisher:     publisher,
		log:           logger,
		now:           now,
		tickerFactory: tickerFactory,
	}, nil
}

// Run collects on the configured interval until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	ticker := a.tickerFactory(a.config.Interval)
	if ticker == nil {
		return errors.New("operations metrics ticker is required")
	}
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
			if err := a.collect(ctx); err != nil {
				continue
			}
		}
	}
}

func (a *Agent) collect(ctx context.Context) error {
	if !a.collecting.CompareAndSwap(false, true) {
		a.logFailure("overlap")
		return ErrAgentCollectionInProgress
	}
	defer a.collecting.Store(false)

	hostMetrics, err := a.hostReader.Read(ctx)
	if err != nil {
		a.logFailure("host_read")
		return fmt.Errorf("read host metrics: %w", err)
	}
	dockerMetrics, err := a.dockerReader.Read(ctx)
	if err != nil {
		a.logFailure("docker_read")
		return fmt.Errorf("read Docker metrics: %w", err)
	}

	metrics := make(map[MetricKey]float64, len(hostMetrics)+len(dockerMetrics))
	for key, value := range hostMetrics {
		metrics[key] = value
	}
	for key, value := range dockerMetrics {
		if _, duplicate := metrics[key]; duplicate {
			a.logFailure("invalid_metrics")
			return fmt.Errorf("duplicate metric %q from host readers", key)
		}
		metrics[key] = value
	}
	if len(metrics) == 0 {
		a.logFailure("invalid_metrics")
		return errors.New("host snapshot metrics are required")
	}
	for key, value := range metrics {
		if err := ValidateSample(Sample{Key: key, Value: value, Source: SourceHost}); err != nil {
			a.logFailure("invalid_metrics")
			return fmt.Errorf("validate host snapshot: %w", err)
		}
	}

	a.sequence++
	envelope := Envelope{
		Version:    EnvelopeVersion,
		Source:     SourceHost,
		NodeID:     a.config.NodeID,
		ObservedAt: a.now().UTC(),
		Sequence:   a.sequence,
		Metrics:    metrics,
	}
	if err := SignEnvelope(&envelope, a.config.SharedSecret); err != nil {
		a.logFailure("sign")
		return fmt.Errorf("sign host snapshot: %w", err)
	}
	if err := a.publisher.Publish(HostSnapshotSubject, envelope); err != nil {
		a.logFailure("publish")
		return fmt.Errorf("publish host snapshot: %w", err)
	}
	return nil
}

func (a *Agent) logFailure(reason string) {
	if a.log != nil {
		a.log.Printf("ops-agent collection skipped: reason=%s", reason)
	}
}
