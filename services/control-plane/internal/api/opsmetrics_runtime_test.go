package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

type fakeAccountActivityFlusher struct {
	called chan time.Time
	err    error
}

func (flusher *fakeAccountActivityFlusher) FlushQualifications(_ context.Context, at time.Time) error {
	flusher.called <- at
	return flusher.err
}

func TestOpsMetricsRuntimeWiresAccountActivityAfterSuccessfulSetup(t *testing.T) {
	sourceBytes, err := os.ReadFile("opsmetrics_runtime.go") // #nosec G304 -- fixed test-owned source path
	require.NoError(t, err)
	source := string(sourceBytes)
	needles := []string{
		"if err := natsClient.Flush(); err != nil",
		"tracker := opsmetrics.NewActivityTracker()",
		"hub.SetActivityObserver(tracker)",
		"accounts := opsmetrics.NewAccountProvider(db, tracker)",
		"opsmetrics.NewCollector(store, receiver, counters, hub, cfg.Interval, log, accounts)",
	}
	prior := -1
	for _, needle := range needles {
		require.Equal(t, 1, strings.Count(source, needle), needle)
		position := strings.Index(source, needle)
		require.Greater(t, position, prior, needle)
		prior = position
	}
}

func TestWireOpsMetricsRuntimeDegradesWithSanitizedStartupReason(t *testing.T) {
	var output bytes.Buffer
	runtime := wireOpsMetricsRuntime(nil, nil, nil, nil, config.OpsMetricsConfig{
		Enabled: true,
	}, logger.NewWithWriter(&output))

	require.Nil(t, runtime)
	require.Contains(t, output.String(), "startup_failed")
	require.NotContains(t, output.String(), "NATS")
}

func TestWireOpsMetricsRuntimeDisabledIsSilent(t *testing.T) {
	var output bytes.Buffer
	runtime := wireOpsMetricsRuntime(nil, nil, nil, nil, config.OpsMetricsConfig{}, logger.NewWithWriter(&output))
	require.Nil(t, runtime)
	require.Empty(t, output.String())
}

func TestOpsMetricsRuntimeStopCancelsAndWaits(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{})
	runtime := &OpsMetricsRuntime{
		cancel: func() { close(cancelled) },
		done:   done,
	}

	result := make(chan error, 1)
	go func() { result <- runtime.Stop(context.Background()) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("runtime was not cancelled")
	}
	select {
	case <-result:
		t.Fatal("Stop returned before the collector finished")
	default:
	}

	close(done)
	require.NoError(t, <-result)
}

func TestOpsMetricsRuntimeStopFlushesAccountActivityAfterCollectorStops(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{})
	flusher := &fakeAccountActivityFlusher{called: make(chan time.Time, 1)}
	now := time.Date(2026, 7, 17, 2, 30, 0, 0, time.UTC)
	runtime := &OpsMetricsRuntime{
		cancel:   func() { close(cancelled) },
		done:     done,
		accounts: flusher,
		now:      func() time.Time { return now },
	}

	result := make(chan error, 1)
	go func() { result <- runtime.Stop(context.Background()) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("runtime was not cancelled")
	}
	select {
	case <-flusher.called:
		t.Fatal("activity flushed before the collector stopped")
	default:
	}

	close(done)
	require.Equal(t, now, <-flusher.called)
	require.NoError(t, <-result)
}

func TestOpsMetricsRuntimeStopReturnsAccountActivityFlushError(t *testing.T) {
	done := make(chan struct{})
	close(done)
	flushErr := errors.New("activity flush failed")
	runtime := &OpsMetricsRuntime{
		cancel:   func() {},
		done:     done,
		accounts: &fakeAccountActivityFlusher{called: make(chan time.Time, 1), err: flushErr},
	}

	require.ErrorIs(t, runtime.Stop(context.Background()), flushErr)
}

func TestOpsMetricsRuntimeStopHonorsContext(t *testing.T) {
	runtime := &OpsMetricsRuntime{cancel: func() {}, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, runtime.Stop(ctx), context.Canceled)
}
