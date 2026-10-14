package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	natssdk "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
)

// Voice command resource names are shared with the authoritative media process.
const (
	VoiceCommandStream             = "VOICE_COMMANDS_V1"
	VoiceCommandSubject            = "voice.commands.v1"
	VoiceCommandOwnerBucket        = "VOICE_COMMAND_OWNER_V1"
	VoiceCommandOwnerKey           = "authoritative"
	voiceCommandGenerationMetadata = "concord.voice-enforcement.generation"
	voiceCommandAPIBudget          = 2 * time.Second
)

// ErrVoiceCommandContinuity never carries rejected wire values.
var ErrVoiceCommandContinuity = errors.New("voice command transport continuity unavailable")

var canonicalVoiceCommandUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type voiceCommandOwnerBirth struct {
	StreamCreatedAt     string `json:"streamCreatedAt"`
	OwnerStoreCreatedAt string `json:"ownerStoreCreatedAt"`
}

type voiceCommandOwnerProof struct {
	StreamCreatedAt      string `json:"streamCreatedAt"`
	OwnerStoreCreatedAt  string `json:"ownerStoreCreatedAt"`
	InitializingRevision string `json:"initializingRevision"`
}

type voiceCommandOwnerRecord struct {
	Version          int                     `json:"version"`
	StreamGeneration string                  `json:"streamGeneration"`
	Phase            string                  `json:"phase"`
	NodeBootID       *string                 `json:"nodeBootId"`
	Birth            *voiceCommandOwnerBirth `json:"birth,omitempty"`
	Proof            *voiceCommandOwnerProof `json:"proof,omitempty"`
}

func voiceCommandDescription(generation uuid.UUID) string {
	return "concord.voice-enforcement generation=" + generation.String()
}

func voiceCommandStreamConfig(generation uuid.UUID) natssdk.StreamConfig {
	return natssdk.StreamConfig{
		Name: VoiceCommandStream, Description: voiceCommandDescription(generation),
		Subjects: []string{VoiceCommandSubject},
		Storage:  natssdk.FileStorage, Replicas: 1, Retention: natssdk.LimitsPolicy,
		Discard: natssdk.DiscardNew, MaxAge: 0, MaxBytes: 67108864,
		MaxMsgs: 50000, MaxMsgSize: 4096, MaxMsgsPerSubject: -1, MaxConsumers: -1,
		Duplicates: 120 * time.Second,
		Metadata:   map[string]string{voiceCommandGenerationMetadata: generation.String()},
	}
}

func voiceCommandOwnerStreamConfig(generation uuid.UUID) natssdk.StreamConfig {
	// Use AddStream for initial creation only. Classic CreateKeyValue includes
	// compatibility updates that must not repair an existing ownership resource.
	return natssdk.StreamConfig{
		Name: "KV_" + VoiceCommandOwnerBucket, Description: voiceCommandDescription(generation),
		Subjects: []string{"$KV." + VoiceCommandOwnerBucket + ".>"},
		Storage:  natssdk.FileStorage, Replicas: 1, Retention: natssdk.LimitsPolicy,
		Discard: natssdk.DiscardNew, MaxAge: 0, MaxBytes: -1, MaxMsgs: -1,
		MaxMsgSize: -1, MaxMsgsPerSubject: 1, MaxConsumers: -1,
		Duplicates: 120 * time.Second, AllowRollup: true, DenyDelete: true, AllowDirect: true,
		Metadata: map[string]string{voiceCommandGenerationMetadata: generation.String()},
	}
}

func (c *Client) voiceCommandJetStream(ctx context.Context) (natssdk.JetStreamContext, jetstream.JetStream, error) {
	if c == nil || c.conn == nil || !c.conn.IsConnected() {
		return nil, nil, natssdk.ErrInvalidConnection
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	js, err := c.conn.JetStream()
	if err != nil {
		return nil, nil, fmt.Errorf("bind voice command stream manager: %w", err)
	}
	// The modern KV API takes a context on every operation. Classic KV methods
	// do not propagate a default JetStream context into Get/Create/Update.
	kvManager, err := jetstream.New(c.conn)
	if err != nil {
		return nil, nil, fmt.Errorf("bind voice command owner manager: %w", err)
	}
	return js, kvManager, nil
}

// Initialize creates only generation-bound initializing resources. A result is
// provisional: only its independently committed SQL receipt may release it.
func (c *Client) Initialize(ctx context.Context, generation uuid.UUID) (voicecommand.TransportIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	var identity voicecommand.TransportIdentity
	if !canonicalVoiceCommandUUID.MatchString(generation.String()) {
		return identity, ErrVoiceCommandContinuity
	}
	js, kvManager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return identity, err
	}
	stream, err := initializeVoiceCommandStream(ctx, js, generation)
	if err != nil {
		return identity, err
	}
	if err := validateVoiceCommandStreamFeatures(ctx, kvManager, stream); err != nil {
		return identity, err
	}
	bucket, err := initializeVoiceCommandBucket(ctx, js, generation, stream)
	if err != nil {
		return identity, err
	}
	if err := validateVoiceCommandStreamFeatures(ctx, kvManager, bucket); err != nil {
		return identity, err
	}
	identity.StreamCreatedAt, identity.OwnerStoreCreatedAt = stream.Created.UTC(), bucket.Created.UTC()
	kv, err := kvManager.KeyValue(ctx, VoiceCommandOwnerBucket)
	if err != nil {
		return identity, fmt.Errorf("bind initializing owner bucket: %w", err)
	}
	entry, err := initializingVoiceCommandOwner(ctx, kv, generation, identity, stream, bucket)
	if err != nil {
		return identity, err
	}
	owner, err := decodeVoiceCommandOwner(entry.Value(), generation)
	if err != nil || owner.Phase != "initializing" || !birthMatches(owner.Birth, identity) {
		return identity, errors.Join(ErrVoiceCommandContinuity, err)
	}
	identity.InitializingRevision = entry.Revision()
	if !identity.Valid() {
		return identity, ErrVoiceCommandContinuity
	}
	return identity, nil
}

func initializingVoiceCommandOwner(ctx context.Context, kv jetstream.KeyValue, generation uuid.UUID, identity voicecommand.TransportIdentity, stream, bucket *natssdk.StreamInfo) (jetstream.KeyValueEntry, error) {
	entry, err := kv.Get(ctx, VoiceCommandOwnerKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		// A nonzero bucket sequence proves that this key was lost or deleted;
		// classic KV Create would otherwise resurrect a deletion tombstone.
		if bucket.State.LastSeq != 0 || stream.State.LastSeq != 0 || stream.State.Consumers != 0 {
			return nil, ErrVoiceCommandContinuity
		}
		owner := voiceCommandOwnerRecord{
			Version: 1, StreamGeneration: generation.String(), Phase: "initializing",
			Birth: &voiceCommandOwnerBirth{
				StreamCreatedAt:     identity.StreamCreatedAt.Format(time.RFC3339Nano),
				OwnerStoreCreatedAt: identity.OwnerStoreCreatedAt.Format(time.RFC3339Nano),
			},
		}
		encoded, encodeErr := json.Marshal(owner)
		if encodeErr != nil {
			return nil, fmt.Errorf("encode initializing owner: %w", encodeErr)
		}
		if _, createErr := kv.Create(ctx, VoiceCommandOwnerKey, encoded); createErr != nil && !errors.Is(createErr, jetstream.ErrKeyExists) {
			return nil, fmt.Errorf("create initializing owner: %w", createErr)
		}
		entry, err = kv.Get(ctx, VoiceCommandOwnerKey)
	}
	if err != nil {
		return nil, fmt.Errorf("read initializing owner: %w", err)
	}
	return entry, nil
}

func initializeVoiceCommandStream(ctx context.Context, js natssdk.JetStreamContext, generation uuid.UUID) (*natssdk.StreamInfo, error) {
	stream, err := js.StreamInfo(VoiceCommandStream, natssdk.Context(ctx))
	if errors.Is(err, natssdk.ErrStreamNotFound) {
		if _, bucketErr := js.StreamInfo("KV_"+VoiceCommandOwnerBucket, natssdk.Context(ctx)); !errors.Is(bucketErr, natssdk.ErrStreamNotFound) {
			return nil, errors.Join(ErrVoiceCommandContinuity, bucketErr)
		}
		config := voiceCommandStreamConfig(generation)
		stream, err = addOrReadVoiceCommandStream(ctx, js, &config)
	}
	if err != nil {
		return nil, fmt.Errorf("initialize voice command stream: %w", err)
	}
	if err := validateVoiceCommandStream(stream, generation, false); err != nil {
		return nil, err
	}
	return stream, nil
}

func initializeVoiceCommandBucket(ctx context.Context, js natssdk.JetStreamContext, generation uuid.UUID, stream *natssdk.StreamInfo) (*natssdk.StreamInfo, error) {
	bucket, err := js.StreamInfo("KV_"+VoiceCommandOwnerBucket, natssdk.Context(ctx))
	if errors.Is(err, natssdk.ErrStreamNotFound) {
		if stream.State.LastSeq != 0 || stream.State.Consumers != 0 {
			return nil, ErrVoiceCommandContinuity
		}
		config := voiceCommandOwnerStreamConfig(generation)
		bucket, err = addOrReadVoiceCommandStream(ctx, js, &config)
	}
	if err != nil {
		return nil, fmt.Errorf("initialize voice command owner bucket: %w", err)
	}
	if err := validateVoiceCommandStream(bucket, generation, true); err != nil {
		return nil, err
	}
	return bucket, nil
}

func addOrReadVoiceCommandStream(ctx context.Context, js natssdk.JetStreamContext, config *natssdk.StreamConfig) (*natssdk.StreamInfo, error) {
	stream, err := js.AddStream(config, natssdk.Context(ctx))
	if errors.Is(err, natssdk.ErrStreamNameAlreadyInUse) {
		return js.StreamInfo(config.Name, natssdk.Context(ctx))
	}
	return stream, err
}

func validateVoiceCommandStream(stream *natssdk.StreamInfo, generation uuid.UUID, ownerBucket bool) error {
	if stream == nil || stream.Created.IsZero() {
		return ErrVoiceCommandContinuity
	}
	want := voiceCommandStreamConfig(generation)
	if ownerBucket {
		want = voiceCommandOwnerStreamConfig(generation)
	}
	got := stream.Config
	if got.Name != want.Name || got.Description != want.Description ||
		len(got.Subjects) != 1 || got.Subjects[0] != want.Subjects[0] ||
		got.Metadata[voiceCommandGenerationMetadata] != generation.String() ||
		got.Storage != want.Storage || got.Replicas != want.Replicas ||
		got.Retention != want.Retention || got.Discard != want.Discard ||
		got.MaxAge != want.MaxAge || got.MaxBytes != want.MaxBytes ||
		got.MaxMsgs != want.MaxMsgs || got.MaxMsgSize != want.MaxMsgSize ||
		got.MaxMsgsPerSubject != want.MaxMsgsPerSubject || got.MaxConsumers != want.MaxConsumers ||
		got.FirstSeq != 0 ||
		got.Compression != want.Compression || got.Placement != nil || got.Template != "" ||
		got.Duplicates != want.Duplicates || got.NoAck || got.Sealed ||
		got.Mirror != nil || len(got.Sources) != 0 || got.RePublish != nil ||
		got.SubjectTransform != nil || got.AllowMsgTTL || got.SubjectDeleteMarkerTTL != 0 ||
		got.DiscardNewPerSubject || got.AllowRollup != want.AllowRollup ||
		got.DenyDelete != want.DenyDelete || got.DenyPurge != want.DenyPurge ||
		got.AllowDirect != want.AllowDirect || got.MirrorDirect ||
		got.ConsumerLimits.InactiveThreshold != 0 || got.ConsumerLimits.MaxAckPending != 0 {
		return ErrVoiceCommandContinuity
	}
	return nil
}

func validateVoiceCommandStreamFeatures(ctx context.Context, manager jetstream.JetStream, classic *natssdk.StreamInfo) error {
	stream, err := manager.Stream(ctx, classic.Config.Name)
	if err != nil {
		return fmt.Errorf("verify voice command stream features: %w", err)
	}
	// Classic StreamInfo omits newer persistence and message features. Tie the
	// modern read to the same resource birth before accepting either snapshot.
	info := stream.CachedInfo()
	if info == nil || !info.Created.Equal(classic.Created) {
		return ErrVoiceCommandContinuity
	}
	config := info.Config
	if config.FirstSeq != 0 || config.PersistMode != jetstream.DefaultPersistMode ||
		config.AllowMsgCounter || config.AllowMsgSchedules || config.AllowAtomicPublish || config.AllowBatchPublish {
		return ErrVoiceCommandContinuity
	}
	return nil
}

// Verify cannot recreate anything. Both immutable broker creation times and the
// record's receipt proof must match the SQL receipt supplied by the caller.
func (c *Client) Verify(ctx context.Context, receipt voicecommand.TransportReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	js, kvManager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return err
	}
	_, _, _, err = verifiedVoiceCommandOwner(ctx, js, kvManager, receipt)
	return err
}

func verifiedVoiceCommandOwner(ctx context.Context, js natssdk.JetStreamContext, kvManager jetstream.JetStream, receipt voicecommand.TransportReceipt) (jetstream.KeyValue, jetstream.KeyValueEntry, voiceCommandOwnerRecord, error) {
	var owner voiceCommandOwnerRecord
	if !receipt.Identity.Valid() || !canonicalVoiceCommandUUID.MatchString(receipt.Generation.String()) {
		return nil, nil, owner, ErrVoiceCommandContinuity
	}
	if err := verifyVoiceCommandResourceBirths(ctx, js, kvManager, receipt); err != nil {
		return nil, nil, owner, err
	}
	kv, err := kvManager.KeyValue(ctx, VoiceCommandOwnerBucket)
	if err != nil {
		return nil, nil, owner, fmt.Errorf("verify voice command owner bucket: %w", err)
	}
	entry, err := kv.Get(ctx, VoiceCommandOwnerKey)
	if err != nil {
		return nil, nil, owner, fmt.Errorf("verify voice command owner entry: %w", err)
	}
	owner, err = decodeVoiceCommandOwner(entry.Value(), receipt.Generation)
	if err != nil {
		return nil, nil, owner, err
	}
	if owner.Phase == "initializing" {
		if entry.Revision() != receipt.Identity.InitializingRevision || !birthMatches(owner.Birth, receipt.Identity) {
			return nil, nil, owner, ErrVoiceCommandContinuity
		}
	} else if entry.Revision() <= receipt.Identity.InitializingRevision || !proofMatches(owner.Proof, receipt.Identity) {
		return nil, nil, owner, ErrVoiceCommandContinuity
	}
	return kv, entry, owner, nil
}

func verifyVoiceCommandResourceBirths(ctx context.Context, js natssdk.JetStreamContext, manager jetstream.JetStream, receipt voicecommand.TransportReceipt) error {
	for _, ownerBucket := range []bool{false, true} {
		name, creation := VoiceCommandStream, receipt.Identity.StreamCreatedAt
		if ownerBucket {
			name, creation = "KV_"+VoiceCommandOwnerBucket, receipt.Identity.OwnerStoreCreatedAt
		}
		stream, err := js.StreamInfo(name, natssdk.Context(ctx))
		if err != nil {
			return fmt.Errorf("verify voice command resource: %w", err)
		}
		if err := validateVoiceCommandStream(stream, receipt.Generation, ownerBucket); err != nil {
			return err
		}
		if !stream.Created.Equal(creation) {
			return ErrVoiceCommandContinuity
		}
		if err := validateVoiceCommandStreamFeatures(ctx, manager, stream); err != nil {
			return err
		}
	}
	return nil
}

func birthMatches(birth *voiceCommandOwnerBirth, identity voicecommand.TransportIdentity) bool {
	return birth != nil && birth.StreamCreatedAt == identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano) &&
		birth.OwnerStoreCreatedAt == identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano)
}

func proofMatches(proof *voiceCommandOwnerProof, identity voicecommand.TransportIdentity) bool {
	return proof != nil && proof.StreamCreatedAt == identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano) &&
		proof.OwnerStoreCreatedAt == identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano) &&
		proof.InitializingRevision == strconv.FormatUint(identity.InitializingRevision, 10)
}

func ownerObject(data []byte, required, optional []string) (map[string]json.RawMessage, error) {
	if len(data) > 4096 || !utf8.Valid(data) {
		return nil, ErrVoiceCommandContinuity
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrVoiceCommandContinuity
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range append(append([]string{}, required...), optional...) {
		allowed[key] = true
	}
	fields, err := readOwnerFields(decoder, allowed)
	if err != nil {
		return nil, err
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, ErrVoiceCommandContinuity
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrVoiceCommandContinuity
	}
	for _, key := range required {
		if _, present := fields[key]; !present {
			return nil, ErrVoiceCommandContinuity
		}
	}
	return fields, nil
}

func readOwnerFields(decoder *json.Decoder, allowed map[string]bool) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok || !allowed[key] {
			return nil, ErrVoiceCommandContinuity
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrVoiceCommandContinuity
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrVoiceCommandContinuity
		}
		fields[key] = value
	}
	return fields, nil
}

func decodeVoiceCommandOwner(data []byte, generation uuid.UUID) (voiceCommandOwnerRecord, error) {
	var owner voiceCommandOwnerRecord
	fields, err := ownerObject(data, []string{"version", "streamGeneration", "phase", "nodeBootId"}, []string{"birth", "proof"})
	if err != nil {
		return owner, err
	}
	if err := json.Unmarshal(data, &owner); err != nil || owner.Version != 1 ||
		owner.StreamGeneration != generation.String() || !canonicalVoiceCommandUUID.MatchString(owner.StreamGeneration) {
		return owner, ErrVoiceCommandContinuity
	}
	if owner.NodeBootID != nil && !canonicalVoiceCommandUUID.MatchString(*owner.NodeBootID) {
		return owner, ErrVoiceCommandContinuity
	}
	if owner.Phase == "initializing" {
		return owner, validateInitializingOwner(owner, fields)
	}
	return owner, validateConfirmedOwner(owner, fields)
}

func validateInitializingOwner(owner voiceCommandOwnerRecord, fields map[string]json.RawMessage) error {
	if _, present := fields["proof"]; present {
		return ErrVoiceCommandContinuity
	}
	if owner.NodeBootID != nil || owner.Birth == nil {
		return ErrVoiceCommandContinuity
	}
	_, err := ownerObject(fields["birth"], []string{"streamCreatedAt", "ownerStoreCreatedAt"}, nil)
	if err != nil || !canonicalOwnerTime(owner.Birth.StreamCreatedAt) || !canonicalOwnerTime(owner.Birth.OwnerStoreCreatedAt) {
		return ErrVoiceCommandContinuity
	}
	return nil
}

func validateConfirmedOwner(owner voiceCommandOwnerRecord, fields map[string]json.RawMessage) error {
	if owner.Phase != "held" && owner.Phase != "released" {
		return ErrVoiceCommandContinuity
	}
	if _, present := fields["birth"]; present {
		return ErrVoiceCommandContinuity
	}
	if owner.Proof == nil || (owner.Phase == "held" && owner.NodeBootID == nil) {
		return ErrVoiceCommandContinuity
	}
	if _, err := ownerObject(fields["proof"], []string{"streamCreatedAt", "ownerStoreCreatedAt", "initializingRevision"}, nil); err != nil {
		return err
	}
	if !canonicalOwnerTime(owner.Proof.StreamCreatedAt) || !canonicalOwnerTime(owner.Proof.OwnerStoreCreatedAt) {
		return ErrVoiceCommandContinuity
	}
	revision, err := strconv.ParseUint(owner.Proof.InitializingRevision, 10, 63)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != owner.Proof.InitializingRevision {
		return ErrVoiceCommandContinuity
	}
	return nil
}

func canonicalOwnerTime(value string) bool {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !timestamp.IsZero() && timestamp.UTC().Format(time.RFC3339Nano) == value
}

// ReleaseInitialOwner CAS-releases only the original receipt-bound initializing
// value. It never adopts a later initialization revision or resets a held owner.
func (c *Client) ReleaseInitialOwner(ctx context.Context, receipt voicecommand.TransportReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	js, kvManager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return err
	}
	kv, entry, owner, err := verifiedVoiceCommandOwner(ctx, js, kvManager, receipt)
	if err != nil {
		return err
	}
	if owner.Phase != "initializing" {
		return nil
	}
	owner.Phase, owner.Birth = "released", nil
	owner.Proof = &voiceCommandOwnerProof{
		StreamCreatedAt:      receipt.Identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
		OwnerStoreCreatedAt:  receipt.Identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano),
		InitializingRevision: strconv.FormatUint(receipt.Identity.InitializingRevision, 10),
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encode released initial owner: %w", err)
	}
	if _, err := kv.Update(ctx, VoiceCommandOwnerKey, encoded, entry.Revision()); err != nil {
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return fmt.Errorf("release initial owner: %w", err)
		}
		_, _, winner, verifyErr := verifiedVoiceCommandOwner(ctx, js, kvManager, receipt)
		if verifyErr != nil || winner.Phase == "initializing" {
			return errors.Join(ErrVoiceCommandContinuity, verifyErr)
		}
	}
	return nil
}

// RecoverVoiceCommandOwner performs only the operator's exact held-record CAS.
// Its caller must require physical predecessor-stop proof and supply the committed
// SQL receipt. Recovery never adopts broker proof or creates any missing resource.
func (c *Client) RecoverVoiceCommandOwner(ctx context.Context, expectedRevision uint64, expectedBootID uuid.UUID, receipts ...voicecommand.TransportReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	if len(receipts) != 1 || expectedRevision == 0 || !canonicalVoiceCommandUUID.MatchString(expectedBootID.String()) {
		return ErrVoiceCommandContinuity
	}
	receipt := receipts[0]
	if !receipt.Identity.Valid() || !canonicalVoiceCommandUUID.MatchString(receipt.Generation.String()) {
		return ErrVoiceCommandContinuity
	}
	js, kvManager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return err
	}
	kv, entry, owner, err := verifiedVoiceCommandOwner(ctx, js, kvManager, receipt)
	if err != nil || owner.Phase != "held" || owner.NodeBootID == nil ||
		*owner.NodeBootID != expectedBootID.String() || entry.Revision() != expectedRevision {
		return errors.Join(ErrVoiceCommandContinuity, err)
	}
	owner.Phase = "released"
	encoded, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encode recovered owner: %w", err)
	}
	if err := verifyVoiceCommandResourceBirths(ctx, js, kvManager, receipt); err != nil {
		return err
	}
	if _, err := kv.Update(ctx, VoiceCommandOwnerKey, encoded, expectedRevision); err != nil {
		return fmt.Errorf("recover exact held owner: %w", err)
	}
	// A delayed CAS may cross resource replacement. Its unchanged old proof is
	// inert there, and changed births must never be reported as recovered authority.
	return verifyVoiceCommandResourceBirths(ctx, js, kvManager, receipt)
}

// VoiceCommandClient owns only synchronous bootstrap and recovery operations.
// Canceling a request can leave its broker result ambiguous; receipt birth and
// original-revision CAS resolve that ambiguity. There is no asynchronous publish
// queue or core subscriber lifecycle to drain on this dedicated connection.
type VoiceCommandClient struct {
	*Client
}

// Close immediately closes this dedicated connection within the caller's budget.
// Ordinary Client.Close retains its existing subscriber/publisher drain behavior.
func (client *VoiceCommandClient) Close() error {
	if client != nil && client.Client != nil && client.conn != nil {
		client.conn.Close()
	}
	return nil
}

type voiceCommandDialFunc func(network, address string) (net.Conn, error)

func (dial voiceCommandDialFunc) Dial(network, address string) (net.Conn, error) {
	return dial(network, address)
}

type voiceCommandConnectDialer struct {
	mu          sync.Mutex
	connections []net.Conn
	closeErr    error
}

func (dialer *voiceCommandConnectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, connection.Close())
	}
	dialer.connections = append(dialer.connections, connection)
	return connection, nil
}

func (dialer *voiceCommandConnectDialer) closeConnections() {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	for _, connection := range dialer.connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			dialer.closeErr = errors.Join(dialer.closeErr, err)
		}
	}
}

// NewVoiceCommandClient makes one cancellable connection attempt. Its worker owns
// subsequent retry; no permanent nil connection survives an initial broker outage.
func NewVoiceCommandClient(ctx context.Context, url string) (*VoiceCommandClient, error) {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dialer := &voiceCommandConnectDialer{}
	closed := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		dialer.closeConnections()
		close(closed)
	})
	connection, err := natssdk.Connect(url,
		natssdk.Name("concordvoice-voice-command-transport"),
		natssdk.NoReconnect(), natssdk.Timeout(voiceCommandAPIBudget),
		natssdk.SkipHostLookup(), natssdk.SetCustomDialer(voiceCommandDialFunc(func(network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		})),
	)
	if !stopCancellation() {
		<-closed
	}
	if err != nil || ctx.Err() != nil {
		dialer.closeConnections()
		return nil, errors.Join(err, ctx.Err(), dialer.closeErr)
	}
	return &VoiceCommandClient{Client: &Client{conn: connection}}, nil
}
