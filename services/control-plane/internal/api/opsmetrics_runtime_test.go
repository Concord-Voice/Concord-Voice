package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

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

func TestOpsMetricsRuntimeStopHonorsContext(t *testing.T) {
	runtime := &OpsMetricsRuntime{cancel: func() {}, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, runtime.Stop(ctx), context.Canceled)
}
