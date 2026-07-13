package opsmetrics

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"time"
)

// EnvelopeVersion is the only signed snapshot schema accepted by this release.
const EnvelopeVersion = 1

const maxClockSkew = 5 * time.Minute

const canonicalTimestampLayout = "2006-01-02T15:04:05.000Z"

var nodeIDPattern = regexp.MustCompile(`^cvn_[a-z2-7]{16}$`)

// Envelope is a signed, closed-schema snapshot from an internal producer.
type Envelope struct {
	Version    int                   `json:"version"`
	Source     Source                `json:"source"`
	NodeID     string                `json:"node_id"`
	ObservedAt time.Time             `json:"observed_at"`
	Sequence   uint64                `json:"sequence"`
	Metrics    map[MetricKey]float64 `json:"metrics"`
	Signature  string                `json:"signature"`
}

// AcceptedPosition records the newest snapshot accepted from one source.
type AcceptedPosition struct {
	ObservedAt time.Time
	Sequence   uint64
}

// ValidateNodeID requires an opaque assigned token rather than discovered host data.
func ValidateNodeID(nodeID string) error {
	if !nodeIDPattern.MatchString(nodeID) {
		return errors.New("node id must be an assigned cvn_ token with 16 lowercase base32 characters")
	}
	return nil
}

func signingPayload(envelope Envelope) ([]byte, error) {
	keys := make([]string, 0, len(envelope.Metrics))
	for key := range envelope.Metrics {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)

	var payload bytes.Buffer
	writer := bufio.NewWriter(&payload)
	if _, err := fmt.Fprintf(writer, "%d\n%s\n%s\n%s\n%d\n", envelope.Version, envelope.Source, envelope.NodeID, envelope.ObservedAt.UTC().Format(canonicalTimestampLayout), envelope.Sequence); err != nil {
		return nil, err
	}
	for _, rawKey := range keys {
		valueBits := math.Float64bits(envelope.Metrics[MetricKey(rawKey)])
		if _, err := fmt.Fprintf(writer, "%s=%016x\n", rawKey, valueBits); err != nil {
			return nil, err
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	return payload.Bytes(), nil
}

// SignEnvelope replaces Signature with a canonical HMAC-SHA-256 signature.
func SignEnvelope(envelope *Envelope, secret []byte) error {
	if envelope == nil {
		return errors.New("envelope is required")
	}
	if len(secret) < 32 {
		return errors.New("snapshot signing secret must be at least 32 bytes")
	}
	envelope.ObservedAt = envelope.ObservedAt.UTC().Truncate(time.Millisecond)
	payload, err := signingPayload(*envelope)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write(payload); err != nil {
		return fmt.Errorf("sign snapshot: %w", err)
	}
	envelope.Signature = hex.EncodeToString(mac.Sum(nil))
	return nil
}

// DecodeEnvelope rejects unknown fields and trailing JSON documents.
func DecodeEnvelope(raw []byte) (Envelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Envelope{}, errors.New("trailing JSON data")
		}
		return Envelope{}, fmt.Errorf("trailing JSON data: %w", err)
	}
	return envelope, nil
}

// VerifyEnvelope validates schema, freshness, ownership, bounds, replay position, and signature.
func VerifyEnvelope(envelope Envelope, secret []byte, now time.Time, previous AcceptedPosition) error {
	if err := validateEnvelopeMetadata(envelope, now, previous); err != nil {
		return err
	}
	if err := validateEnvelopeMetrics(envelope); err != nil {
		return err
	}
	return verifyEnvelopeSignature(envelope, secret)
}

func validateEnvelopeMetadata(envelope Envelope, now time.Time, previous AcceptedPosition) error {
	if envelope.Version != EnvelopeVersion {
		return fmt.Errorf("unsupported envelope version %d", envelope.Version)
	}
	if envelope.Source != SourceHost && envelope.Source != SourceMedia {
		return fmt.Errorf("unsupported envelope source %q", envelope.Source)
	}
	if err := ValidateNodeID(envelope.NodeID); err != nil {
		return err
	}
	if !envelope.ObservedAt.Equal(envelope.ObservedAt.Truncate(time.Millisecond)) {
		return errors.New("snapshot timestamp must use millisecond precision")
	}
	if envelope.Sequence == 0 {
		return errors.New("sequence must be positive")
	}
	if envelope.ObservedAt.Before(now.Add(-maxClockSkew)) || envelope.ObservedAt.After(now.Add(maxClockSkew)) {
		return errors.New("snapshot timestamp is outside allowed clock skew")
	}
	if !previous.ObservedAt.IsZero() {
		if envelope.ObservedAt.Before(previous.ObservedAt) ||
			(envelope.ObservedAt.Equal(previous.ObservedAt) && envelope.Sequence <= previous.Sequence) {
			return errors.New("snapshot position was already accepted")
		}
	}
	return nil
}

func validateEnvelopeMetrics(envelope Envelope) error {
	if len(envelope.Metrics) == 0 {
		return errors.New("snapshot metrics are required")
	}
	for key, value := range envelope.Metrics {
		if err := ValidateSample(Sample{Key: key, Value: value, Source: envelope.Source}); err != nil {
			return err
		}
	}
	return nil
}

func verifyEnvelopeSignature(envelope Envelope, secret []byte) error {
	provided, err := hex.DecodeString(envelope.Signature)
	if err != nil || len(provided) != sha256.Size {
		return errors.New("snapshot signature is invalid")
	}
	unsigned := envelope
	unsigned.Signature = ""
	payload, err := signingPayload(unsigned)
	if err != nil {
		return err
	}
	if len(secret) < 32 {
		return errors.New("snapshot signing secret must be at least 32 bytes")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return errors.New("snapshot signature is invalid")
	}
	return nil
}
