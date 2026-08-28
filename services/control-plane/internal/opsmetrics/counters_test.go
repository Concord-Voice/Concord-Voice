package opsmetrics_test

import (
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/stretchr/testify/require"
)

func TestCountersSnapshotIsClosedAndConcurrentSafe(t *testing.T) {
	counters := opsmetrics.NewCounters()

	var writers sync.WaitGroup
	for range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 100 {
				counters.Increment(opsmetrics.MetricChannelMessagesTotal)
			}
		}()
	}
	writers.Wait()

	snapshot := counters.Snapshot()
	require.Equal(t, float64(800), snapshot[opsmetrics.MetricChannelMessagesTotal])
	require.Equal(t, float64(0), snapshot[opsmetrics.MetricDMMessagesTotal])
	require.Equal(t, float64(0), snapshot[opsmetrics.MetricMediaUploadsTotal])
	require.Len(t, snapshot, 8)
	for key := range snapshot {
		definition, ok := opsmetrics.Definition(key)
		require.True(t, ok)
		require.Equal(t, opsmetrics.SourceControl, definition.Source)
	}
}

func TestCountersTrackSuccessfulMediaUploads(t *testing.T) {
	counters := opsmetrics.NewCounters()

	counters.Increment(opsmetrics.MetricMediaUploadsTotal)
	counters.Increment(opsmetrics.MetricMediaUploadsTotal)

	require.Equal(t, float64(2), counters.Snapshot()[opsmetrics.MetricMediaUploadsTotal])
}

func TestCountersIgnoreKeysNotOwnedByControlPlane(t *testing.T) {
	counters := opsmetrics.NewCounters()

	counters.Increment(opsmetrics.MetricHostCPUPercent)
	counters.Increment(opsmetrics.MetricMediaRoomsCurrent)
	counters.Increment(opsmetrics.MetricKey("user_supplied"))

	snapshot := counters.Snapshot()
	for _, value := range snapshot {
		require.Zero(t, value)
	}
}
