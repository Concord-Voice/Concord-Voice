// Package erasureclear protects the authorized viewer audience carried by
// account-erasure events across control-plane replicas.
package erasureclear

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
)

const domain = "concord/presence-erasure-clear/v2"

// MaxViewersPerEvent keeps a padded JWE batch below the accepted NATS payload
// ceiling. JSON carries each UUID separately, so 9,000 leaves framing room.
const MaxViewersPerEvent = 9_000

// MaxEventPayloadBytes bounds work before JSON parsing or JWE authentication.
const MaxEventPayloadBytes = 500_000

const audiencePaddingBucketBytes = 4096

var errInvalidEnvelope = errors.New("invalid erasure-clear envelope")

type audiencePayload struct {
	SenderID string   `json:"sender_id"`
	Chunk    int      `json:"chunk"`
	Viewers  []string `json:"viewers"`
	Padding  string   `json:"padding"`
}

// DeriveKey separates JWE encryption from every other use of the deployment
// secret using standard HKDF. An unwired secret cannot authenticate an event.
func DeriveKey(secret string) []byte {
	if secret == "" {
		return nil
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, domain, 32)
	if err != nil {
		return nil
	}
	return key
}

// Batches sorts the captured audience and covers every viewer with a bounded
// event. An empty audience still needs one event for sender-state invalidation.
func Batches(viewerIDs []string) [][]string {
	canonical := append([]string(nil), viewerIDs...)
	sort.Strings(canonical)
	if len(canonical) == 0 {
		return [][]string{nil}
	}
	batches := make([][]string, 0, (len(canonical)+MaxViewersPerEvent-1)/MaxViewersPerEvent)
	for len(canonical) > 0 {
		n := min(len(canonical), MaxViewersPerEvent)
		batches = append(batches, canonical[:n])
		canonical = canonical[n:]
	}
	return batches
}

// Seal encrypts and authenticates one batch with RFC 7516 compact JWE using
// direct AES-256-GCM. Sender and chunk are inside the authenticated plaintext.
func Seal(key []byte, userID string, chunk int, viewerIDs []string) (string, error) {
	if len(key) != 32 || chunk < 0 || len(viewerIDs) > MaxViewersPerEvent {
		return "", errInvalidEnvelope
	}
	canonical := append([]string(nil), viewerIDs...)
	sort.Strings(canonical)
	payload := audiencePayload{SenderID: userID, Chunk: chunk, Viewers: canonical}
	plain, err := json.Marshal(payload)
	if err != nil || len(plain) > MaxEventPayloadBytes {
		return "", errInvalidEnvelope
	}
	// Padding is an application-level size bucket; JWE supplies all encryption,
	// nonce generation, integrity, and wire framing. No compression is enabled.
	padLength := (audiencePaddingBucketBytes - len(plain)%audiencePaddingBucketBytes) % audiencePaddingBucketBytes
	payload.Padding = strings.Repeat("0", padLength)
	plain, err = json.Marshal(payload)
	if err != nil || len(plain)%audiencePaddingBucketBytes != 0 {
		return "", errInvalidEnvelope
	}
	encrypter, err := jose.NewEncrypter(jose.A256GCM,
		jose.Recipient{Algorithm: jose.DIRECT, Key: key},
		(&jose.EncrypterOptions{}).WithType(jose.ContentType(domain)))
	if err != nil {
		return "", errInvalidEnvelope
	}
	sealed, err := encrypter.Encrypt(plain)
	if err != nil {
		return "", errInvalidEnvelope
	}
	envelope, err := sealed.CompactSerialize()
	if err != nil || len(envelope) > MaxEventPayloadBytes {
		return "", errInvalidEnvelope
	}
	return envelope, nil
}

// Fingerprint is a cheap pre-authentication replay lookup. Only an envelope
// that Open authenticated and the subscriber accepted may be marked as seen.
// Open rejects noncanonical compact JWE spellings, so the wire bytes are a
// stable identity for every valid envelope.
func Fingerprint(envelope string) [sha256.Size]byte {
	return sha256.Sum256([]byte(envelope))
}

// Open refuses missing, malformed, or modified envelopes before exposing an
// audience. It binds the authenticated sender and chunk to the NATS metadata.
func Open(key []byte, userID string, chunk int, envelope string) ([]string, error) {
	if len(key) != 32 || chunk < 0 || len(envelope) > MaxEventPayloadBytes {
		return nil, errInvalidEnvelope
	}
	sealed, err := jose.ParseEncryptedCompact(envelope,
		[]jose.KeyAlgorithm{jose.DIRECT}, []jose.ContentEncryption{jose.A256GCM})
	if err != nil {
		return nil, errInvalidEnvelope
	}
	canonical, err := sealed.CompactSerialize()
	if err != nil || canonical != envelope {
		return nil, errInvalidEnvelope
	}
	plain, err := sealed.Decrypt(key)
	if err != nil || len(plain) < audiencePaddingBucketBytes ||
		len(plain)%audiencePaddingBucketBytes != 0 {
		return nil, errInvalidEnvelope
	}
	if sealed.Header.ExtraHeaders[jose.HeaderType] != domain {
		return nil, errInvalidEnvelope
	}
	var payload audiencePayload
	if err := json.Unmarshal(plain, &payload); err != nil ||
		payload.SenderID != userID || payload.Chunk != chunk ||
		len(payload.Viewers) > MaxViewersPerEvent {
		return nil, errInvalidEnvelope
	}
	return payload.Viewers, nil
}
