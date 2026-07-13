package opsmetrics_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
)

var envelopeSecret = []byte("0123456789abcdef0123456789abcdef") // pragma: allowlist secret

func validEnvelope(t *testing.T, now time.Time) opsmetrics.Envelope {
	t.Helper()
	envelope := opsmetrics.Envelope{
		Version:    opsmetrics.EnvelopeVersion,
		Source:     opsmetrics.SourceHost,
		NodeID:     "cvn_aaaaaaaaaaaaaaaa",
		ObservedAt: now.UTC(),
		Sequence:   7,
		Metrics: map[opsmetrics.MetricKey]float64{
			opsmetrics.MetricHostMemoryPercent: 25,
			opsmetrics.MetricHostCPUPercent:    50,
		},
	}
	require.NoError(t, opsmetrics.SignEnvelope(&envelope, envelopeSecret))
	return envelope
}

func TestSignEnvelopeIsDeterministicAcrossMetricInsertionOrder(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 123_000_000, time.UTC)
	first := validEnvelope(t, now)
	second := first
	second.Metrics = map[opsmetrics.MetricKey]float64{
		opsmetrics.MetricHostCPUPercent:    50,
		opsmetrics.MetricHostMemoryPercent: 25,
	}
	second.Signature = ""
	require.NoError(t, opsmetrics.SignEnvelope(&second, envelopeSecret))
	require.Equal(t, first.Signature, second.Signature)
	require.Len(t, first.Signature, 64)
	require.Equal(t, now.Truncate(time.Millisecond), first.ObservedAt)
}

func TestVerifyEnvelopeAcceptsValidEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	envelope := validEnvelope(t, now)
	require.NoError(t, opsmetrics.VerifyEnvelope(envelope, envelopeSecret, now, opsmetrics.AcceptedPosition{}))
}

func TestVerifyEnvelopeAcceptsTypeScriptMillisecondTimestampFixture(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef") // pragma: allowlist secret
	envelope := opsmetrics.Envelope{
		Version:    opsmetrics.EnvelopeVersion,
		Source:     opsmetrics.SourceMedia,
		NodeID:     "cvn_aaaaaaaaaaaaaaaa",
		ObservedAt: time.Date(2026, 7, 12, 20, 0, 0, 120_000_000, time.UTC),
		Sequence:   1,
		Metrics: map[opsmetrics.MetricKey]float64{
			opsmetrics.MetricMediaRoomsCurrent: 2,
		},
		Signature: "12da4ba70e95328cda663187d4d180962edef8b9196678606a1bfdec3668fc8f", // pragma: allowlist secret -- deterministic HMAC fixture
	}

	require.NoError(t, opsmetrics.VerifyEnvelope(
		envelope,
		secret,
		time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC),
		opsmetrics.AcceptedPosition{},
	))
}

func TestVerifyEnvelopeRejectsSubMillisecondTimestampMalleability(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 123_000_000, time.UTC)
	envelope := validEnvelope(t, now)
	envelope.ObservedAt = envelope.ObservedAt.Add(time.Nanosecond)
	require.ErrorContains(t, opsmetrics.VerifyEnvelope(
		envelope,
		envelopeSecret,
		now,
		opsmetrics.AcceptedPosition{},
	), "millisecond precision")
}

func TestVerifyEnvelopeRejectsInvalidBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		mutate func(*opsmetrics.Envelope)
		secret []byte
		prior  opsmetrics.AcceptedPosition
	}{
		{name: "bad signature", mutate: func(e *opsmetrics.Envelope) { e.Signature = "00" }, secret: envelopeSecret},
		{name: "wrong secret", mutate: func(*opsmetrics.Envelope) {}, secret: []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{name: "old timestamp", mutate: func(e *opsmetrics.Envelope) { e.ObservedAt = now.Add(-6 * time.Minute) }, secret: envelopeSecret},
		{name: "future timestamp", mutate: func(e *opsmetrics.Envelope) { e.ObservedAt = now.Add(6 * time.Minute) }, secret: envelopeSecret},
		{name: "replayed position", mutate: func(*opsmetrics.Envelope) {}, secret: envelopeSecret, prior: opsmetrics.AcceptedPosition{ObservedAt: now, Sequence: 7}},
		{name: "wrong key owner", mutate: func(e *opsmetrics.Envelope) {
			e.Metrics = map[opsmetrics.MetricKey]float64{opsmetrics.MetricMediaRoomsCurrent: 1}
		}, secret: envelopeSecret},
		{name: "invalid node", mutate: func(e *opsmetrics.Envelope) { e.NodeID = "api.concordvoice.chat" }, secret: envelopeSecret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope := validEnvelope(t, now)
			tt.mutate(&envelope)
			if tt.name != "bad signature" {
				require.NoError(t, opsmetrics.SignEnvelope(&envelope, envelopeSecret))
			}
			require.Error(t, opsmetrics.VerifyEnvelope(envelope, tt.secret, now, tt.prior))
		})
	}
}

func TestValidateNodeIDRequiresOpaqueAssignedShape(t *testing.T) {
	require.NoError(t, opsmetrics.ValidateNodeID("cvn_aaaaaaaaaaaaaaaa"))

	for _, value := range []string{
		"node-a7",
		"api.concordvoice.chat",
		"10.0.0.3",
		"d7781e5d-e353-46aa-afe2-3ca49f13332a", // pragma: allowlist secret
		"cvn_short",
		"CVN_AAAAAAAAAAAAAAAA",
	} {
		require.Error(t, opsmetrics.ValidateNodeID(value), value)
	}
}

func TestDecodeEnvelopeRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	envelope := validEnvelope(t, now)
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)

	var object map[string]any
	require.NoError(t, json.Unmarshal(raw, &object))
	object["extra"] = 1
	withExtra, err := json.Marshal(object)
	require.NoError(t, err)

	_, err = opsmetrics.DecodeEnvelope(withExtra)
	require.ErrorContains(t, err, "unknown field")

	_, err = opsmetrics.DecodeEnvelope(append(raw, []byte(` {}`)...))
	require.ErrorContains(t, err, "trailing")
}
