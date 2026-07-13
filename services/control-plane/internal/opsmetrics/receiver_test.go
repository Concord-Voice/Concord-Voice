package opsmetrics_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

type fakeOpsSubscriber struct {
	handlers map[string]func([]byte)
	failOn   string
}

func (s *fakeOpsSubscriber) Subscribe(subject string, handler func([]byte)) (*nats.Subscription, error) {
	if subject == s.failOn {
		return nil, errors.New("subscribe failed")
	}
	if s.handlers == nil {
		s.handlers = make(map[string]func([]byte))
	}
	s.handlers[subject] = handler
	return nil, nil
}

func signedSnapshot(t *testing.T, source opsmetrics.Source, observedAt time.Time, sequence uint64) []byte {
	return signedSnapshotForNode(t, source, observedAt, sequence, "cvn_aaaaaaaaaaaaaaaa")
}

func signedSnapshotForNode(t *testing.T, source opsmetrics.Source, observedAt time.Time, sequence uint64, nodeID string) []byte {
	t.Helper()
	metrics := map[opsmetrics.MetricKey]float64{opsmetrics.MetricHostCPUPercent: 42}
	if source == opsmetrics.SourceMedia {
		metrics = map[opsmetrics.MetricKey]float64{opsmetrics.MetricMediaRoomsCurrent: 3}
	}
	envelope := opsmetrics.Envelope{
		Version:    opsmetrics.EnvelopeVersion,
		Source:     source,
		NodeID:     nodeID,
		ObservedAt: observedAt,
		Sequence:   sequence,
		Metrics:    metrics,
	}
	require.NoError(t, opsmetrics.SignEnvelope(&envelope, []byte("0123456789abcdef0123456789abcdef"))) // pragma: allowlist secret -- test-only signing key
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)
	return raw
}

func TestReceiverSubscribesOnlyToFixedSubjects(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	subscriber := &fakeOpsSubscriber{}
	receiver := opsmetrics.NewReceiver(
		subscriber,
		"cvn_aaaaaaaaaaaaaaaa",
		[]byte("0123456789abcdef0123456789abcdef"), // pragma: allowlist secret -- test-only signing key
		opsmetrics.NewCounters(),
		logger.NewWithWriter(&bytes.Buffer{}),
		func() time.Time { return now },
	)

	require.NoError(t, receiver.Subscribe())
	require.ElementsMatch(t, []string{opsmetrics.HostSnapshotSubject, opsmetrics.MediaSnapshotSubject}, mapKeys(subscriber.handlers))
}

func TestReceiverAcceptsHostAndMediaWithIndependentPositions(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	subscriber := &fakeOpsSubscriber{}
	receiver := opsmetrics.NewReceiver(
		subscriber,
		"cvn_aaaaaaaaaaaaaaaa",
		[]byte("0123456789abcdef0123456789abcdef"), // pragma: allowlist secret -- test-only signing key
		opsmetrics.NewCounters(),
		logger.NewWithWriter(&bytes.Buffer{}),
		func() time.Time { return now },
	)
	require.NoError(t, receiver.Subscribe())

	subscriber.handlers[opsmetrics.HostSnapshotSubject](signedSnapshot(t, opsmetrics.SourceHost, now, 1))
	subscriber.handlers[opsmetrics.MediaSnapshotSubject](signedSnapshot(t, opsmetrics.SourceMedia, now, 1))

	host, ok := receiver.Latest(opsmetrics.SourceHost)
	require.True(t, ok)
	require.Equal(t, float64(42), host.Metrics[opsmetrics.MetricHostCPUPercent])
	media, ok := receiver.Latest(opsmetrics.SourceMedia)
	require.True(t, ok)
	require.Equal(t, float64(3), media.Metrics[opsmetrics.MetricMediaRoomsCurrent])

	host.Metrics[opsmetrics.MetricHostCPUPercent] = 99
	hostAgain, ok := receiver.Latest(opsmetrics.SourceHost)
	require.True(t, ok)
	require.Equal(t, float64(42), hostAgain.Metrics[opsmetrics.MetricHostCPUPercent])
}

func TestReceiverRejectsBeforeMutatingLatestSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	counters := opsmetrics.NewCounters()
	subscriber := &fakeOpsSubscriber{}
	receiver := opsmetrics.NewReceiver(
		subscriber,
		"cvn_aaaaaaaaaaaaaaaa",
		[]byte("0123456789abcdef0123456789abcdef"), // pragma: allowlist secret -- test-only signing key
		counters,
		logger.NewWithWriter(&logs),
		func() time.Time { return now },
	)
	require.NoError(t, receiver.Subscribe())
	valid := signedSnapshot(t, opsmetrics.SourceHost, now, 2)
	subscriber.handlers[opsmetrics.HostSnapshotSubject](valid)

	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "replay", raw: valid},
		{name: "invalid signature", raw: bytes.Replace(valid, []byte(`"signature":"`), []byte(`"signature":"00`), 1)},
		{name: "wrong subject source", raw: signedSnapshot(t, opsmetrics.SourceMedia, now.Add(time.Second), 3)},
		{name: "wrong configured node", raw: signedSnapshotForNode(t, opsmetrics.SourceHost, now.Add(time.Second), 3, "cvn_bbbbbbbbbbbbbbbb")},
		{name: "unknown field", raw: append(valid[:len(valid)-1], []byte(`,"content":"private"}`)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(_ *testing.T) {
			subscriber.handlers[opsmetrics.HostSnapshotSubject](test.raw)
		})
	}

	latest, ok := receiver.Latest(opsmetrics.SourceHost)
	require.True(t, ok)
	require.Equal(t, uint64(2), latest.Sequence)
	require.Equal(t, float64(5), counters.Snapshot()[opsmetrics.MetricSnapshotRejectionsTotal])
	require.NotContains(t, logs.String(), "private")
	require.NotContains(t, logs.String(), "cvn_aaaaaaaaaaaaaaaa")
	require.NotContains(t, logs.String(), string(opsmetrics.MetricHostCPUPercent))
	require.Contains(t, logs.String(), string(opsmetrics.RejectionDecode))
	require.Contains(t, logs.String(), string(opsmetrics.RejectionSourceMismatch))
	require.Contains(t, logs.String(), string(opsmetrics.RejectionVerification))
}

func TestReceiverReturnsSubscriptionFailure(t *testing.T) {
	subscriber := &fakeOpsSubscriber{failOn: opsmetrics.MediaSnapshotSubject}
	receiver := opsmetrics.NewReceiver(
		subscriber,
		"cvn_aaaaaaaaaaaaaaaa",
		[]byte("0123456789abcdef0123456789abcdef"), // pragma: allowlist secret -- test-only signing key
		nil,
		logger.NewWithWriter(&bytes.Buffer{}),
		time.Now,
	)

	require.Error(t, receiver.Subscribe())
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
