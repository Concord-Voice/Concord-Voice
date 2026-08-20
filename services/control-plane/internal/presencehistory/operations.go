package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
)

const (
	pendingOperationGrace = 30 * time.Second
	commitReadbackTimeout = 3 * time.Second

	// topologyDeliveryTimeout is the topology rail's UNIFORM per-delivery
	// bound: ONE delivery on this rail gets 1500ms no matter which entry point
	// reached it. It is shorter than the settings rail's because of the
	// interactive case — a friend accept must not hold an HTTP request open
	// while a stalled socket drains — but that case MOTIVATES the bound rather
	// than scoping it. With commitReadbackTimeout it puts the completion
	// ceiling at 2*1500ms + 2*3s = 9s, down from 16s.
	//
	// The background paths inherit it DELIBERATELY. reconcileTopologyMarker
	// runs off RunPendingReconciler's ticker with no HTTP request anywhere on
	// the stack, and deliverTopologyPlan / recoverTopologyPlan are reachable
	// from both that ticker and the inline completion;
	// recoverUnresolvedTopologyCommit is inline-only and takes the same uniform
	// bound. On the ticker a stalled socket consumes a reconciler tick
	// instead of a request, which is the same bounded-progress argument. Do
	// NOT revert one of those call sites to effectiveDeliveryTimeout on the
	// grounds that it "isn't interactive".
	//
	// It is a const and takes NO configuration surface. Making it configurable
	// would let a deployment raise a latency bound instead of fixing whatever
	// made delivery slow, and #2446 PR 2 ships zero new environment variables.
	// Residue past this bound converges through RunPendingReconciler's 5s
	// ticker -> reconcileTopologyMarker -> conservative reset, and that retry
	// is itself bounded by this same const: convergence comes from repeated
	// bounded attempts, never from a longer one. The explicit tradeoff is that
	// a slow path over-clears rather than resolving exactly.
	topologyDeliveryTimeout = 1500 * time.Millisecond

	senderGateStripes = 64

	// Version 8 is the RFC 9562 application-defined UUID space. Topology
	// operations use it as durable kind evidence when the settings marker is
	// later repaired or corrupted and the same-version relation is ambiguous.
	topologyOperationUUIDVersion uuid.Version = 8
)

// ErrPendingOperationEligible tells the caller to roll back and hand the
// eligible marker to reconciliation before starting a fresh operation.
var ErrPendingOperationEligible = errors.New("pending presence operation requires reconciliation")

// OperationMode distinguishes ordinary audience writes from privacy-narrowing
// security clears that may supersede an existing pending operation.
type OperationMode uint8

const (
	// OrdinaryAudienceWrite must yield to an existing pending marker.
	OrdinaryAudienceWrite OperationMode = iota
	// ForcedSecurityClear may supersede a pending marker to narrow exposure.
	ForcedSecurityClear
)

type audienceOperationKind uint8

const (
	audienceSettingsOperation audienceOperationKind = iota
	audienceTopologyOperation
)

// DeliveryMode selects exact audience reconciliation or a conservative reset.
type DeliveryMode uint8

const (
	// DeliveryExactDelta reconciles the prepared pre/post audience difference.
	DeliveryExactDelta DeliveryMode = iota
	// DeliveryConservativeReset clears or disconnects every possibly stale client.
	DeliveryConservativeReset
)

// AudienceOperation is the complete durable evidence needed to classify the
// main transaction and perform acknowledged delivery.
type AudienceOperation struct {
	kind                         audienceOperationKind
	ID                           uuid.UUID
	SenderID                     uuid.UUID
	PriorVersion                 int64
	Version                      int64
	PriorOperationID             *uuid.UUID
	SupersededPending            bool
	ReconcileAfter               time.Time
	Before                       CustomTextState
	BeforeTier                   int
	BeforeMasterEnabled          bool
	BeforeServerVoiceTier        int
	BeforeServerVoiceShowDetails bool
	BeforePrivateCallTier        int
	BeforePrivateCallShowDetails bool
}

// DeliveryPlan contains prepared, non-locking delivery inputs.
type DeliveryPlan struct {
	Mode             DeliveryMode
	OperationID      uuid.UUID
	SenderID         uuid.UUID
	ClearRecipients  map[uuid.UUID]bool
	UpdateRecipients map[uuid.UUID]bool
	Payload          *CustomTextState
	OverrideVersion  *int
}

// TopologyAudience captures the authorized Custom Status audience on both
// sides of one graph mutation for a sender already present in a topology batch.
type TopologyAudience struct {
	SenderID uuid.UUID
	Before   map[uuid.UUID]bool
	After    map[uuid.UUID]bool
}

// TopologyBatch is opaque transaction and delivery evidence. Callers prepare
// it before topology locks, attach the in-transaction audiences, and pass the
// resulting value to CompleteTopologyBatch.
type TopologyBatch struct {
	operations []AudienceOperation
	plans      []DeliveryPlan
}

// DeliveryAck authorizes marker removal only for the matching operation.
type DeliveryAck struct {
	OperationID uuid.UUID
}

// CustomTextDeliverer performs bounded Custom Status delivery and returns its exact ack.
type CustomTextDeliverer interface {
	DeliverCustomText(context.Context, DeliveryPlan) (DeliveryAck, error)
}

// Delivery preserves the original exported name for existing callers.
type Delivery = CustomTextDeliverer

// CommitOutcome classifies an ambiguous main-transaction commit.
type CommitOutcome uint8

const (
	// CommitConfirmed means the exact attempted settings and pending marker are durable.
	CommitConfirmed CommitOutcome = iota
	// RollbackConfirmed means the exact prior version, marker, and state remain.
	RollbackConfirmed
	// WriteSuperseded means a different equal-or-later operation is durable.
	WriteSuperseded
	// CommitUnresolved means primary read-back did not prove another outcome.
	CommitUnresolved
)

type senderGateSet [senderGateStripes]chan struct{}

func newSenderGateSet() senderGateSet {
	var gates senderGateSet
	for index := range gates {
		gates[index] = make(chan struct{}, 1)
	}
	return gates
}

func senderGateIndex(senderID uuid.UUID) int {
	var hash uint32
	for _, value := range senderID {
		hash = hash*33 + uint32(value)
	}
	return int(hash % senderGateStripes)
}

// WithSender serializes one sender's local mutation, commit classification,
// claim acknowledgement, and reconnect snapshot boundary.
func (s *Service) WithSender(
	ctx context.Context,
	senderID uuid.UUID,
	work func() error,
) error {
	gate := s.senderGates[senderGateIndex(senderID)]
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

// WithSenders holds every process-local sender stripe named by senderIDs for
// one publication boundary. Stripe indexes, not UUIDs, are deduplicated and
// acquired in sorted order so hash collisions and overlapping sender sets
// cannot self-deadlock. Cross-process writers remain outside these local gates.
func (s *Service) WithSenders(
	ctx context.Context,
	senderIDs []uuid.UUID,
	work func() error,
) error {
	// Fail closed on a nil receiver, matching BeginTopologyBatch and
	// validateTopologyBatchCompletion. This is not defensive padding: a typed
	// nil *Service still satisfies the graphpresence.TopologyRail interface, so
	// r.rail != nil answers TRUE and #2446's boot guard passes on a rail that
	// is dead. Without this guard the failure surfaces as a nil dereference on
	// s.senderGates below, on the first gated write rather than at boot.
	if s == nil {
		return errors.New("presence history sender gates unavailable")
	}
	var selected [senderGateStripes]bool
	indexes := make([]int, 0, min(len(senderIDs), senderGateStripes))
	for _, senderID := range senderIDs {
		index := senderGateIndex(senderID)
		if !selected[index] {
			selected[index] = true
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)

	acquired := make([]chan struct{}, 0, len(indexes))
	for _, index := range indexes {
		gate := s.senderGates[index]
		select {
		case gate <- struct{}{}:
			acquired = append(acquired, gate)
		case <-ctx.Done():
			for acquiredIndex := len(acquired) - 1; acquiredIndex >= 0; acquiredIndex-- {
				<-acquired[acquiredIndex]
			}
			return ctx.Err()
		}
	}
	defer func() {
		for index := len(acquired) - 1; index >= 0; index-- {
			<-acquired[index]
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

// BeginAudienceOperation acquires users -> settings -> pending locks and writes
// the new version and durable marker in the caller's transaction.
func (s *Service) BeginAudienceOperation(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	mode OperationMode,
) (AudienceOperation, error) {
	if mode != OrdinaryAudienceWrite && mode != ForcedSecurityClear {
		return AudienceOperation{}, fmt.Errorf("invalid audience operation mode")
	}
	prior, err := lockAudienceOperationPrior(ctx, tx, senderID)
	if err != nil {
		return AudienceOperation{}, err
	}
	pending, err := lockAudienceOperationPending(ctx, tx, senderID)
	if err != nil {
		return AudienceOperation{}, err
	}
	now, err := readAudienceOperationClock(ctx, tx)
	if err != nil {
		return AudienceOperation{}, err
	}
	pendingIsTopology := pending.exists && (isTopologyOperationID(pending.operationID) ||
		prior.marker.Valid && prior.marker.UUID == pending.operationID &&
			prior.version == pending.priorVersion)
	if err := validateAudienceOperationReadiness(mode, pending, pendingIsTopology, now); err != nil {
		return AudienceOperation{}, err
	}
	if prior.version == math.MaxInt64 {
		return AudienceOperation{}, fmt.Errorf("audience operation version exhausted")
	}

	operationID := uuid.New()
	nextVersion := prior.version + 1
	nextReconcileAfter := now.Add(pendingOperationGrace)
	if err := writeAudienceOperationPending(
		ctx, tx, senderID, operationID, prior.version, now, pending.exists,
	); err != nil {
		return AudienceOperation{}, err
	}
	if err := writeAudienceOperationVersion(ctx, tx, senderID, operationID, nextVersion, now); err != nil {
		return AudienceOperation{}, err
	}

	operation := AudienceOperation{
		ID:                operationID,
		SenderID:          senderID,
		PriorVersion:      prior.version,
		Version:           nextVersion,
		SupersededPending: pending.exists,
		ReconcileAfter:    nextReconcileAfter,
		Before: normalizeCustomTextState(CustomTextState{
			Text:  nullableString(prior.text),
			Emoji: nullableString(prior.emoji),
		}),
		BeforeTier:                   prior.tier,
		BeforeMasterEnabled:          prior.masterEnabled,
		BeforeServerVoiceTier:        prior.serverVoiceTier,
		BeforeServerVoiceShowDetails: prior.serverVoiceShowDetails,
		BeforePrivateCallTier:        prior.privateCallTier,
		BeforePrivateCallShowDetails: prior.privateCallShowDetails,
	}
	if prior.marker.Valid {
		value := prior.marker.UUID
		operation.PriorOperationID = &value
	}
	return operation, nil
}

// BeginTopologyBatch writes one same-version durable marker per distinct
// sender in deterministic UUID order. The caller must hold the matching
// process-local sender gates and must not acquire topology locks first.
func (s *Service) BeginTopologyBatch(
	ctx context.Context,
	tx *sql.Tx,
	senderIDs []uuid.UUID,
) (TopologyBatch, error) {
	if s == nil || tx == nil {
		return TopologyBatch{}, errors.New("topology batch unavailable")
	}
	ordered, err := canonicalTopologySenders(senderIDs)
	if err != nil {
		return TopologyBatch{}, err
	}
	batch := TopologyBatch{operations: make([]AudienceOperation, 0, len(ordered))}
	for _, senderID := range ordered {
		operation, beginErr := s.beginTopologyOperation(ctx, tx, senderID)
		if beginErr != nil {
			return TopologyBatch{}, fmt.Errorf("begin topology audience operation: %w", beginErr)
		}
		batch.operations = append(batch.operations, operation)
	}
	return batch, nil
}

func canonicalTopologySenders(senderIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(senderIDs) == 0 || len(senderIDs) > maxReconcileBatch {
		return nil, errors.New("invalid topology sender batch")
	}
	seen := make(map[uuid.UUID]struct{}, len(senderIDs))
	ordered := make([]uuid.UUID, 0, len(senderIDs))
	for _, senderID := range senderIDs {
		if senderID == uuid.Nil {
			return nil, errors.New("invalid topology sender")
		}
		if _, exists := seen[senderID]; exists {
			continue
		}
		seen[senderID] = struct{}{}
		ordered = append(ordered, senderID)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	return ordered, nil
}

func (s *Service) beginTopologyOperation(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (AudienceOperation, error) {
	prior, err := lockAudienceOperationPrior(ctx, tx, senderID)
	if err != nil {
		return AudienceOperation{}, err
	}
	pending, err := lockAudienceOperationPending(ctx, tx, senderID)
	if err != nil {
		return AudienceOperation{}, err
	}
	now, err := readAudienceOperationClock(ctx, tx)
	if err != nil {
		return AudienceOperation{}, err
	}
	if err := validateTopologyOperationReadiness(pending, now); err != nil {
		return AudienceOperation{}, err
	}

	operationID := newTopologyOperationID()
	if err := writeAudienceOperationPending(
		ctx, tx, senderID, operationID, prior.version, now, pending.exists,
	); err != nil {
		return AudienceOperation{}, err
	}
	if err := writeTopologyOperationMarker(ctx, tx, senderID, operationID, now); err != nil {
		return AudienceOperation{}, err
	}

	operation := AudienceOperation{
		kind:           audienceTopologyOperation,
		ID:             operationID,
		SenderID:       senderID,
		PriorVersion:   prior.version,
		Version:        prior.version,
		ReconcileAfter: now.Add(pendingOperationGrace),
		Before: normalizeCustomTextState(CustomTextState{
			Text:  nullableString(prior.text),
			Emoji: nullableString(prior.emoji),
		}),
		BeforeTier:                   prior.tier,
		BeforeMasterEnabled:          prior.masterEnabled,
		BeforeServerVoiceTier:        prior.serverVoiceTier,
		BeforeServerVoiceShowDetails: prior.serverVoiceShowDetails,
		BeforePrivateCallTier:        prior.privateCallTier,
		BeforePrivateCallShowDetails: prior.privateCallShowDetails,
	}
	if prior.marker.Valid {
		value := prior.marker.UUID
		operation.PriorOperationID = &value
	}
	return operation, nil
}

func validateTopologyOperationReadiness(
	pending audienceOperationPending,
	now time.Time,
) error {
	return validateAudienceOperationReadiness(OrdinaryAudienceWrite, pending, false, now)
}

func newTopologyOperationID() uuid.UUID {
	operationID := uuid.New()
	operationID[6] = operationID[6]&0x0f | byte(topologyOperationUUIDVersion)<<4
	return operationID
}

func isTopologyOperationID(operationID uuid.UUID) bool {
	return operationID != uuid.Nil && operationID.Version() == topologyOperationUUIDVersion
}

func writeTopologyOperationMarker(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	operationID uuid.UUID,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_operation_id = $2,
		    updated_at = $3
		WHERE user_id = $1
	`, senderID, operationID, now)
	if err != nil {
		return fmt.Errorf("store topology audience operation marker: %w", err)
	}
	return requireOneAffected(result, "store topology audience operation marker")
}

// PrepareTopologyBatch validates and clones one old/new audience pair per
// operation and computes only the exact recipients changed by the mutation.
func PrepareTopologyBatch(
	batch TopologyBatch,
	audiences []TopologyAudience,
) (TopologyBatch, error) {
	if len(batch.operations) == 0 || len(batch.operations) > maxReconcileBatch ||
		len(batch.plans) != 0 || len(audiences) != len(batch.operations) {
		return TopologyBatch{}, errors.New("invalid topology audience coverage")
	}
	operations, err := indexTopologyOperations(batch.operations)
	if err != nil {
		return TopologyBatch{}, err
	}
	covered, err := indexTopologyAudiences(audiences, operations)
	if err != nil {
		return TopologyBatch{}, err
	}

	prepared := TopologyBatch{
		operations: append([]AudienceOperation(nil), batch.operations...),
		plans:      make([]DeliveryPlan, 0, len(batch.operations)),
	}
	for _, operation := range batch.operations {
		audience, exists := covered[operation.SenderID]
		if !exists {
			return TopologyBatch{}, errors.New("missing topology audience")
		}
		plan, err := prepareTopologyPlan(operation, audience)
		if err != nil {
			return TopologyBatch{}, err
		}
		prepared.plans = append(prepared.plans, plan)
	}
	return prepared, nil
}

func indexTopologyOperations(
	operations []AudienceOperation,
) (map[uuid.UUID]AudienceOperation, error) {
	indexed := make(map[uuid.UUID]AudienceOperation, len(operations))
	var previous uuid.UUID
	for index, operation := range operations {
		if err := validateTopologyOperation(operation); err != nil {
			return nil, err
		}
		if index > 0 && previous.String() >= operation.SenderID.String() {
			return nil, errors.New("unordered topology operations")
		}
		if _, duplicate := indexed[operation.SenderID]; duplicate {
			return nil, errors.New("duplicate topology operation")
		}
		indexed[operation.SenderID] = operation
		previous = operation.SenderID
	}
	return indexed, nil
}

func validateTopologyOperation(operation AudienceOperation) error {
	if operation.kind != audienceTopologyOperation || operation.ID == uuid.Nil ||
		operation.SenderID == uuid.Nil || operation.PriorVersion < 0 ||
		operation.Version != operation.PriorVersion {
		return errors.New("invalid topology operation")
	}
	return nil
}

func indexTopologyAudiences(
	audiences []TopologyAudience,
	operations map[uuid.UUID]AudienceOperation,
) (map[uuid.UUID]TopologyAudience, error) {
	covered := make(map[uuid.UUID]TopologyAudience, len(audiences))
	for _, audience := range audiences {
		if audience.SenderID == uuid.Nil {
			return nil, errors.New("invalid topology audience sender")
		}
		if _, exists := operations[audience.SenderID]; !exists {
			return nil, errors.New("unknown topology audience sender")
		}
		if _, duplicate := covered[audience.SenderID]; duplicate {
			return nil, errors.New("duplicate topology audience")
		}
		covered[audience.SenderID] = audience
	}
	return covered, nil
}

func prepareTopologyPlan(
	operation AudienceOperation,
	audience TopologyAudience,
) (DeliveryPlan, error) {
	before, err := cloneTopologyRecipients(operation.SenderID, audience.Before)
	if err != nil {
		return DeliveryPlan{}, err
	}
	after, err := cloneTopologyRecipients(operation.SenderID, audience.After)
	if err != nil {
		return DeliveryPlan{}, err
	}
	active := operation.BeforeMasterEnabled && operation.BeforeTier > 0 && operation.Before.Text != ""
	if !active && (len(before) != 0 || len(after) != 0) {
		return DeliveryPlan{}, errors.New("inactive topology operation has audience")
	}
	plan := DeliveryPlan{
		Mode:             DeliveryExactDelta,
		OperationID:      operation.ID,
		SenderID:         operation.SenderID,
		ClearRecipients:  make(map[uuid.UUID]bool),
		UpdateRecipients: make(map[uuid.UUID]bool),
	}
	if active {
		plan.ClearRecipients, plan.UpdateRecipients = topologyRecipientDelta(before, after)
		payload := operation.Before
		plan.Payload = &payload
	}
	return plan, nil
}

func topologyRecipientDelta(
	before, after map[uuid.UUID]bool,
) (map[uuid.UUID]bool, map[uuid.UUID]bool) {
	clears := make(map[uuid.UUID]bool)
	updates := make(map[uuid.UUID]bool)
	for recipientID := range before {
		if !after[recipientID] {
			clears[recipientID] = true
		}
	}
	for recipientID := range after {
		if !before[recipientID] {
			updates[recipientID] = true
		}
	}
	return clears, updates
}

func cloneTopologyRecipients(
	senderID uuid.UUID,
	recipients map[uuid.UUID]bool,
) (map[uuid.UUID]bool, error) {
	cloned := make(map[uuid.UUID]bool, len(recipients))
	for recipientID, included := range recipients {
		if recipientID == uuid.Nil || recipientID == senderID || !included {
			return nil, errors.New("invalid topology audience recipient")
		}
		cloned[recipientID] = true
	}
	return cloned, nil
}

type audienceOperationPrior struct {
	version                int64
	marker                 uuid.NullUUID
	masterEnabled          bool
	serverVoiceTier        int
	serverVoiceShowDetails bool
	privateCallTier        int
	privateCallShowDetails bool
	tier                   int
	text                   sql.NullString
	emoji                  sql.NullString
}

func lockAudienceOperationPrior(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (audienceOperationPrior, error) {
	if err := lockUser(ctx, tx, senderID); err != nil {
		return audienceOperationPrior{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_presence_settings (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, senderID); err != nil {
		return audienceOperationPrior{}, fmt.Errorf("ensure audience operation settings: %w", err)
	}
	var prior audienceOperationPrior
	err := tx.QueryRowContext(ctx, `
		SELECT presence_settings_version,
		       presence_settings_operation_id,
		       master_enabled,
		       server_voice_tier,
		       server_voice_show_details,
		       private_call_tier,
		       private_call_show_details,
		       custom_text_tier,
		       custom_text,
		       custom_text_emoji
		FROM user_presence_settings
		WHERE user_id = $1
		FOR UPDATE
	`, senderID).Scan(
		&prior.version,
		&prior.marker,
		&prior.masterEnabled,
		&prior.serverVoiceTier,
		&prior.serverVoiceShowDetails,
		&prior.privateCallTier,
		&prior.privateCallShowDetails,
		&prior.tier,
		&prior.text,
		&prior.emoji,
	)
	if err != nil {
		return audienceOperationPrior{}, fmt.Errorf("lock audience operation settings: %w", err)
	}
	return prior, nil
}

type audienceOperationPending struct {
	operationID    uuid.UUID
	priorVersion   int64
	exists         bool
	reconcileAfter time.Time
}

func lockAudienceOperationPending(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (audienceOperationPending, error) {
	var pending audienceOperationPending
	err := tx.QueryRowContext(ctx, `
		SELECT operation_id, prior_settings_version, reconcile_after
		FROM presence_settings_pending_operations
		WHERE user_id = $1
		FOR UPDATE
	`, senderID).Scan(&pending.operationID, &pending.priorVersion, &pending.reconcileAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return pending, nil
	}
	if err != nil {
		return audienceOperationPending{}, fmt.Errorf("lock pending presence operation: %w", err)
	}
	pending.exists = true
	return pending, nil
}

func readAudienceOperationClock(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read audience operation clock: %w", err)
	}
	return now, nil
}

func validateAudienceOperationReadiness(
	mode OperationMode,
	pending audienceOperationPending,
	pendingIsTopology bool,
	now time.Time,
) error {
	if !pending.exists || mode == ForcedSecurityClear && !pendingIsTopology {
		return nil
	}
	if pending.reconcileAfter.After(now) || mode == ForcedSecurityClear && pendingIsTopology {
		return &ServiceError{
			Status:     http.StatusServiceUnavailable,
			Code:       "presence_operation_pending",
			RetryAfter: ceilRetryAfter(pending.reconcileAfter.Sub(now)),
		}
	}
	return ErrPendingOperationEligible
}

func writeAudienceOperationPending(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	operationID uuid.UUID,
	priorVersion int64,
	now time.Time,
	pendingExists bool,
) error {
	reconcileAfter := now.Add(pendingOperationGrace)
	if pendingExists {
		result, err := tx.ExecContext(ctx, `
			UPDATE presence_settings_pending_operations
			SET operation_id = $2,
			    prior_settings_version = $3,
			    created_at = $4,
			    reconcile_after = $5
			WHERE user_id = $1
		`, senderID, operationID, priorVersion, now, reconcileAfter)
		if err != nil {
			return fmt.Errorf("supersede pending presence operation: %w", err)
		}
		return requireOneAffected(result, "supersede pending presence operation")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO presence_settings_pending_operations (
			user_id,
			operation_id,
			prior_settings_version,
			created_at,
			reconcile_after
		) VALUES ($1, $2, $3, $4, $5)
	`, senderID, operationID, priorVersion, now, reconcileAfter)
	if err != nil {
		return fmt.Errorf("insert pending presence operation: %w", err)
	}
	return nil
}

func writeAudienceOperationVersion(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	operationID uuid.UUID,
	nextVersion int64,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = $2,
		    presence_settings_operation_id = $3,
		    updated_at = $4
		WHERE user_id = $1
	`, senderID, nextVersion, operationID, now)
	if err != nil {
		return fmt.Errorf("store audience operation version: %w", err)
	}
	return requireOneAffected(result, "store audience operation version")
}

func ceilRetryAfter(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	rounded := remaining.Truncate(time.Second)
	if rounded < remaining {
		rounded += time.Second
	}
	return rounded
}

func requireOneAffected(result sql.Result, operation string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s row count: %w", operation, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s: exact row count mismatch", operation)
	}
	return nil
}

func nullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

type audienceCommitState struct {
	UserExists             bool
	SettingsExists         bool
	Version                int64
	OperationID            *uuid.UUID
	Before                 CustomTextState
	BeforeTier             int
	MasterEnabled          bool
	ServerVoiceTier        int
	ServerVoiceShowDetails bool
	PrivateCallTier        int
	PrivateCallShowDetails bool
	PendingOperationID     *uuid.UUID
	PendingPriorVersion    int64
	PendingPriorVersionSet bool
}

// ClassifyAudienceCommit uses one bounded primary read-back that survives
// request cancellation, then requires exact durable evidence.
func (s *Service) ClassifyAudienceCommit(
	requestCtx context.Context,
	operation AudienceOperation,
) CommitOutcome {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), commitReadbackTimeout)
	defer cancel()
	return s.classifyAudienceCommit(readCtx, operation)
}

func (s *Service) classifyAudienceCommit(
	ctx context.Context,
	operation AudienceOperation,
) CommitOutcome {
	readState := s.readCommitState
	if readState == nil {
		readState = s.readAudienceCommitState
	}
	state, err := readState(ctx, operation.SenderID)
	if err != nil {
		return CommitUnresolved
	}
	return classifyAudienceCommitState(operation, state)
}

func (s *Service) readAudienceCommitState(
	ctx context.Context,
	senderID uuid.UUID,
) (audienceCommitState, error) {
	var (
		lockedID      uuid.UUID
		settings      bool
		version       int64
		operationID   uuid.NullUUID
		master        bool
		serverTier    int
		serverDetail  bool
		privateTier   int
		privateDetail bool
		tier          int
		text          sql.NullString
		emoji         sql.NullString
		pendingID     uuid.NullUUID
		pendingPrior  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT owners.id,
		       settings.user_id IS NOT NULL,
		       COALESCE(settings.presence_settings_version, 0),
		       settings.presence_settings_operation_id,
		       COALESCE(settings.master_enabled, TRUE),
		       COALESCE(settings.server_voice_tier, 1),
		       COALESCE(settings.server_voice_show_details, TRUE),
		       COALESCE(settings.private_call_tier, 0),
		       COALESCE(settings.private_call_show_details, FALSE),
		       COALESCE(settings.custom_text_tier, 0),
		       settings.custom_text,
		       settings.custom_text_emoji,
		       pending.operation_id,
		       pending.prior_settings_version
		FROM users AS owners
		LEFT JOIN user_presence_settings AS settings
		  ON settings.user_id = owners.id
		LEFT JOIN presence_settings_pending_operations AS pending
		  ON pending.user_id = owners.id
		WHERE owners.id = $1
	`, senderID).Scan(
		&lockedID,
		&settings,
		&version,
		&operationID,
		&master,
		&serverTier,
		&serverDetail,
		&privateTier,
		&privateDetail,
		&tier,
		&text,
		&emoji,
		&pendingID,
		&pendingPrior,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return audienceCommitState{}, nil
	}
	if err != nil {
		return audienceCommitState{}, fmt.Errorf("read audience commit state: %w", err)
	}
	state := audienceCommitState{
		UserExists:             true,
		SettingsExists:         settings,
		Version:                version,
		Before:                 normalizeCustomTextState(CustomTextState{Text: nullableString(text), Emoji: nullableString(emoji)}),
		BeforeTier:             tier,
		MasterEnabled:          master,
		ServerVoiceTier:        serverTier,
		ServerVoiceShowDetails: serverDetail,
		PrivateCallTier:        privateTier,
		PrivateCallShowDetails: privateDetail,
		PendingPriorVersion:    pendingPrior.Int64,
		PendingPriorVersionSet: pendingPrior.Valid,
	}
	if operationID.Valid {
		value := operationID.UUID
		state.OperationID = &value
	}
	if pendingID.Valid {
		value := pendingID.UUID
		state.PendingOperationID = &value
	}
	return state, nil
}

func classifyAudienceCommitState(
	operation AudienceOperation,
	state audienceCommitState,
) CommitOutcome {
	if operation.ID == uuid.Nil || operation.PriorVersion < 0 || !state.UserExists {
		return CommitUnresolved
	}
	expectedVersion := operation.PriorVersion + 1
	switch operation.kind {
	case audienceSettingsOperation:
		if operation.PriorVersion == math.MaxInt64 {
			return CommitUnresolved
		}
	case audienceTopologyOperation:
		expectedVersion = operation.PriorVersion
	default:
		return CommitUnresolved
	}
	if operation.Version != expectedVersion {
		return CommitUnresolved
	}
	if state.SettingsExists && state.Version == operation.Version &&
		uuidPointerEqual(state.OperationID, &operation.ID) &&
		uuidPointerEqual(state.PendingOperationID, &operation.ID) &&
		state.PendingPriorVersionSet && state.PendingPriorVersion == operation.PriorVersion {
		return CommitConfirmed
	}
	if rollbackStateMatches(operation, state) {
		return RollbackConfirmed
	}
	if state.SettingsExists && state.Version >= operation.Version &&
		state.OperationID != nil && *state.OperationID != operation.ID {
		return WriteSuperseded
	}
	return CommitUnresolved
}

func rollbackStateMatches(operation AudienceOperation, state audienceCommitState) bool {
	if state.PendingOperationID != nil &&
		(operation.PriorOperationID == nil || *state.PendingOperationID != *operation.PriorOperationID) {
		return false
	}
	if !state.SettingsExists {
		return operation.PriorVersion == 0 && operation.PriorOperationID == nil &&
			normalizeCustomTextState(operation.Before) == (CustomTextState{}) && operation.BeforeTier == 0 &&
			state.MasterEnabled == operation.BeforeMasterEnabled &&
			state.ServerVoiceTier == operation.BeforeServerVoiceTier &&
			state.ServerVoiceShowDetails == operation.BeforeServerVoiceShowDetails &&
			state.PrivateCallTier == operation.BeforePrivateCallTier &&
			state.PrivateCallShowDetails == operation.BeforePrivateCallShowDetails &&
			state.PendingOperationID == nil
	}
	return state.Version == operation.PriorVersion &&
		uuidPointerEqual(state.OperationID, operation.PriorOperationID) &&
		normalizeCustomTextState(state.Before) == normalizeCustomTextState(operation.Before) &&
		state.BeforeTier == operation.BeforeTier &&
		state.MasterEnabled == operation.BeforeMasterEnabled &&
		state.ServerVoiceTier == operation.BeforeServerVoiceTier &&
		state.ServerVoiceShowDetails == operation.BeforeServerVoiceShowDetails &&
		state.PrivateCallTier == operation.BeforePrivateCallTier &&
		state.PrivateCallShowDetails == operation.BeforePrivateCallShowDetails
}

func uuidPointerEqual(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
