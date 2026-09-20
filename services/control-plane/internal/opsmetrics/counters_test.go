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
	require.Len(t, snapshot, 16)
	for key := range snapshot {
		definition, ok := opsmetrics.Definition(key)
		require.True(t, ok)
		require.Equal(t, opsmetrics.SourceControl, definition.Source)
	}
}

// TestEverySourceControlCounterIsSampled closes the CONVERSE of the loop above,
// which only proves snapshot is a subset of SourceControl.
//
// Without this, a key can be added to the catalog, the migration, the OpenAPI and
// the admin contract while its Counters field is forgotten -- and the whole suite
// stays green, because an absent key is not a contract violation on either half.
// The console then renders that metric as permanently "Unavailable", with no error
// banner and nothing in the backend contradicting it. That is the #2975 / #3004 /
// #3094 defect family one layer further in, and this is what catches it.
func TestEverySourceControlCounterIsSampled(t *testing.T) {
	snapshot := opsmetrics.NewCounters().Snapshot()
	sampled := 0
	for _, definition := range opsmetrics.Catalog() {
		if definition.Source != opsmetrics.SourceControl || definition.Kind != opsmetrics.KindCounter {
			continue
		}
		sampled++
		require.Contains(t, snapshot, definition.Key,
			"catalog admits %q as a control counter but Counters never samples it: "+
				"it would read Unavailable in the console forever, with every test green",
			definition.Key)
	}
	require.Positive(t, sampled, "a zero control-counter set would make this assertion vacuous")
}

func TestCountersTrackSuccessfulMediaUploads(t *testing.T) {
	counters := opsmetrics.NewCounters()

	counters.Increment(opsmetrics.MetricMediaUploadsTotal)
	counters.Increment(opsmetrics.MetricMediaUploadsTotal)

	require.Equal(t, float64(2), counters.Snapshot()[opsmetrics.MetricMediaUploadsTotal])
}

func TestCountersTrackTerminalOutboxOutcomesAndIgnoreNil(t *testing.T) {
	var nilCounters *opsmetrics.Counters
	nilCounters.Increment(opsmetrics.MetricServerVoiceTerminalOutboxCapturedTotal)

	counters := opsmetrics.NewCounters()
	keys := []opsmetrics.MetricKey{
		opsmetrics.MetricServerVoiceTerminalOutboxCapturedTotal,
		opsmetrics.MetricServerVoiceTerminalOutboxDeliveredTotal,
		opsmetrics.MetricServerVoiceTerminalOutboxSuccessorSuppressedTotal,
		opsmetrics.MetricServerVoiceTerminalOutboxChannelSuppressedTotal,
		opsmetrics.MetricServerVoiceTerminalOutboxLockRetainedTotal,
		opsmetrics.MetricServerVoiceTerminalOutboxQueueRescheduledTotal,
	}
	for _, key := range keys {
		counters.Increment(key)
	}

	snapshot := counters.Snapshot()
	for _, key := range keys {
		require.Equal(t, float64(1), snapshot[key], "terminal outbox counter %q", key)
	}
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
