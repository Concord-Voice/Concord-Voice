package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"
)

const maxReconcileBatch = 1000

// ReconcileStats contains aggregate-only outcomes for one bounded pass.
type ReconcileStats struct {
	DiscoveredCount  int
	ResolvedCount    int
	ProvenCount      int
	CompensatedCount int
	RetainedCount    int
	FailedCount      int
}

type pendingOperationRow struct {
	ID             uuid.UUID
	PriorVersion   int64
	ReconcileAfter time.Time
}

type lockedPresenceSettings struct {
	Version                int64
	OperationID            uuid.NullUUID
	MasterEnabled          bool
	ServerVoiceTier        int
	ServerVoiceShowDetails bool
	PrivateCallTier        int
	PrivateCallShowDetails bool
	Tier                   int
	Text                   sql.NullString
	Emoji                  sql.NullString
}

type reconcileResult struct {
	resolved    bool
	proven      bool
	compensated bool
	retained    bool
}

type claimCommitState uint8

const (
	claimCommitUnresolved claimCommitState = iota
	claimCommitConfirmed
	claimCommitQuarantined
	claimCommitSuperseded
)

// ClaimOutcome identifies the durable/privacy outcome of a post-commit claim.
type ClaimOutcome uint8

const (
	// ClaimAcknowledged means exact delivery and marker deletion are durable.
	ClaimAcknowledged ClaimOutcome = iota
	// ClaimRecovered means conservative delivery completed while quarantine remains.
	ClaimRecovered
	// ClaimSuperseded means a newer completed operation or authoritative pending clear made the old claim irrelevant.
	ClaimSuperseded
	// ClaimPreDeliveryFailed means exact delivery was never attempted.
	ClaimPreDeliveryFailed
	// ClaimUnresolved means neither acknowledgement nor conservative recovery was proven.
	ClaimUnresolved
)

// ClaimCompletion returns the typed post-commit outcome and its preserved cause.
type ClaimCompletion struct {
	Outcome ClaimOutcome
	Err     error
}

// BindDelivery binds the one process-local typed delivery adapter.
func (s *Service) BindDelivery(delivery Delivery) error {
	if s == nil || nilDelivery(delivery) {
		return errors.New("presence history delivery unavailable")
	}
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if !nilDelivery(s.delivery) {
		return errors.New("presence history delivery already bound")
	}
	s.delivery = delivery
	return nil
}

func nilDelivery(delivery Delivery) bool {
	if delivery == nil {
		return true
	}
	value := reflect.ValueOf(delivery)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (s *Service) boundDelivery() (Delivery, error) {
	if s == nil {
		return nil, &ServiceError{
			Status: http.StatusServiceUnavailable,
			Code:   "activity_history_delivery_unavailable",
		}
	}
	s.deliveryMu.RLock()
	delivery := s.delivery
	s.deliveryMu.RUnlock()
	if nilDelivery(delivery) {
		return nil, &ServiceError{
			Status: http.StatusServiceUnavailable,
			Code:   "activity_history_delivery_unavailable",
		}
	}
	return delivery, nil
}

// WithReadySender retains one sender gate through reconciliation and work.
func (s *Service) WithReadySender(ctx context.Context, senderID uuid.UUID, work func() error) error {
	return s.withReadySenderMode(ctx, senderID, OrdinaryAudienceWrite, nil, work)
}

// WithReadySenderMode retains the same single sender gate while allowing a
// forced privacy-narrowing clear to supersede any ordinary marker.
func (s *Service) WithReadySenderMode(
	ctx context.Context,
	senderID uuid.UUID,
	mode OperationMode,
	work func() error,
) error {
	return s.withReadySenderMode(ctx, senderID, mode, nil, work)
}

// WithReadySenderModeBeforeReconcile retains one sender gate while running a
// required pre-reconciliation step, Custom Status readiness, and the requested
// work in that order.
func (s *Service) WithReadySenderModeBeforeReconcile(
	ctx context.Context,
	senderID uuid.UUID,
	mode OperationMode,
	beforeReconcile func() error,
	work func() error,
) error {
	if beforeReconcile == nil {
		return errors.New("presence history pre-reconcile step unavailable")
	}
	return s.withReadySenderMode(ctx, senderID, mode, beforeReconcile, work)
}

func (s *Service) withReadySenderMode(
	ctx context.Context,
	senderID uuid.UUID,
	mode OperationMode,
	beforeReconcile func() error,
	work func() error,
) error {
	if s == nil || s.db == nil || senderID == uuid.Nil || work == nil {
		return errors.New("presence history ready sender unavailable")
	}
	if mode != OrdinaryAudienceWrite && mode != ForcedSecurityClear {
		return errors.New("invalid presence history ready sender mode")
	}
	return s.WithSender(ctx, senderID, func() error {
		return s.runReadySenderMode(ctx, senderID, mode, beforeReconcile, work)
	})
}

func (s *Service) runReadySenderMode(
	ctx context.Context,
	senderID uuid.UUID,
	mode OperationMode,
	beforeReconcile func() error,
	work func() error,
) error {
	if beforeReconcile != nil {
		if err := beforeReconcile(); err != nil {
			return err
		}
	}
	if _, err := s.boundDelivery(); err != nil {
		return err
	}
	if _, err := s.reconcileSenderAlreadyGated(ctx, senderID, mode); err != nil {
		var pending *ServiceError
		if mode != ForcedSecurityClear || !errors.As(err, &pending) ||
			err != pending || pending.Code != "presence_operation_pending" {
			return err
		}
	}
	return work()
}

// ClaimAndDeliver holds canonical database locks until exact delivery is
// acknowledged, removes only the matching marker, and then commits.
func (s *Service) ClaimAndDeliver(ctx context.Context, plan DeliveryPlan) (returnErr error) {
	_, returnErr = s.claimAndDeliver(ctx, plan)
	return returnErr
}

// CompleteClaim finishes post-commit work under a bounded context detached from
// request cancellation and conservatively recovers failures before delivery.
func (s *Service) CompleteClaim(requestCtx context.Context, plan DeliveryPlan) ClaimCompletion {
	claimCtx, cancel := context.WithTimeout(
		context.WithoutCancel(requestCtx),
		2*s.effectiveDeliveryTimeout()+2*commitReadbackTimeout,
	)
	defer cancel()
	outcome, err := s.claimAndDeliver(claimCtx, plan)
	if err != nil && outcome == ClaimPreDeliveryFailed {
		return s.recoverPreDeliveryClaim(claimCtx, plan, err)
	}
	return ClaimCompletion{Outcome: outcome, Err: err}
}

func boundedDetachedContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return context.WithDeadline(context.WithoutCancel(parent), deadline)
}

func (s *Service) claimAndDeliver(ctx context.Context, plan DeliveryPlan) (outcome ClaimOutcome, returnErr error) {
	delivery, err := s.validateDeliveryPlan(plan)
	if err != nil {
		return ClaimPreDeliveryFailed, err
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return ClaimPreDeliveryFailed, fmt.Errorf("begin presence delivery claim: %w", err)
	}
	if tx == nil {
		return ClaimPreDeliveryFailed, errors.New("begin presence delivery claim: missing transaction")
	}
	defer tx.Rollback() //nolint:errcheck
	defer s.joinTransactionRollback(tx, "presence_delivery_claim", &returnErr)

	if err := lockUser(ctx, tx, plan.SenderID); err != nil {
		return ClaimPreDeliveryFailed, err
	}
	settings, err := lockPresenceSettings(ctx, tx, plan.SenderID)
	if err != nil {
		return ClaimPreDeliveryFailed, err
	}
	pending, err := lockPendingRow(ctx, tx, plan.SenderID)
	if err != nil {
		return ClaimPreDeliveryFailed, err
	}
	if !settings.OperationID.Valid || settings.OperationID.UUID != plan.OperationID ||
		pending.ID != plan.OperationID {
		return ClaimPreDeliveryFailed, errors.New("presence delivery claim marker mismatch")
	}

	deliveryCtx, cancel := context.WithTimeout(ctx, s.effectiveDeliveryTimeout())
	ack, deliveryErr := delivery.DeliverCustomText(deliveryCtx, plan)
	cancel()
	if deliveryErr != nil {
		return s.failExactClaim(ctx, plan, deliveryErr)
	}
	if ack.OperationID != plan.OperationID {
		return s.failExactClaim(ctx, plan, errors.New("presence delivery acknowledgement mismatch"))
	}
	result, err := s.deleteClaimedPending(ctx, tx, plan.SenderID, plan.OperationID)
	if err != nil {
		return s.failExactClaim(ctx, plan, fmt.Errorf("delete claimed presence operation: %w", err))
	}
	if err := requireOneAffected(result, "delete claimed presence operation"); err != nil {
		return s.failExactClaim(ctx, plan, err)
	}
	if err := s.CommitTx(tx); err != nil {
		return s.resolveClaimCommitError(ctx, tx, plan, pending, err)
	}
	return ClaimAcknowledged, nil
}

func (s *Service) resolveClaimCommitError(
	ctx context.Context,
	tx *sql.Tx,
	plan DeliveryPlan,
	pending pendingOperationRow,
	commitErr error,
) (ClaimOutcome, error) {
	cause := fmt.Errorf("commit presence delivery claim: %w", commitErr)
	rollbackErr := s.RollbackTx(tx)
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		cause = errors.Join(cause, fmt.Errorf("rollback presence delivery claim: %w", rollbackErr))
	}
	switch s.classifyClaimCommit(ctx, plan, pending) {
	case claimCommitConfirmed:
		return ClaimAcknowledged, cause
	case claimCommitSuperseded:
		return ClaimSuperseded, cause
	case claimCommitQuarantined:
		return s.failExactClaim(ctx, plan, cause)
	default:
		repairState, restoreErr := s.ensureClaimQuarantine(ctx, plan, pending)
		if restoreErr != nil {
			cause = errors.Join(cause, fmt.Errorf("restore presence delivery quarantine: %w", restoreErr))
		}
		if repairState == claimCommitConfirmed {
			return ClaimAcknowledged, cause
		}
		if repairState == claimCommitSuperseded {
			return ClaimSuperseded, cause
		}
		return s.failExactClaim(ctx, plan, cause)
	}
}

func (s *Service) recoverPreDeliveryClaim(
	ctx context.Context,
	plan DeliveryPlan,
	cause error,
) (completion ClaimCompletion) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return s.recoverAllLocalClaim(ctx, plan, errors.Join(cause, err))
	}
	if tx == nil {
		return s.recoverAllLocalClaim(
			ctx,
			plan,
			errors.Join(cause, errors.New("begin presence claim recovery: missing transaction")),
		)
	}
	defer tx.Rollback() //nolint:errcheck
	defer s.joinTransactionRollback(tx, "presence_pre_delivery_recovery", &completion.Err)
	if err := lockUser(ctx, tx, plan.SenderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ClaimCompletion{Outcome: ClaimSuperseded, Err: cause}
		}
		return s.recoverAllLocalClaim(ctx, plan, errors.Join(cause, err))
	}
	settings, err := lockPresenceSettings(ctx, tx, plan.SenderID)
	if err != nil {
		return s.recoverAllLocalClaim(ctx, plan, errors.Join(cause, err))
	}
	pending, pendingErr := lockPendingRow(ctx, tx, plan.SenderID)
	if errors.Is(pendingErr, sql.ErrNoRows) {
		if settings.OperationID.Valid && settings.OperationID.UUID != plan.OperationID {
			return ClaimCompletion{Outcome: ClaimSuperseded, Err: cause}
		}
		return s.recoverAllLocalClaim(ctx, plan, cause)
	}
	if pendingErr != nil {
		return s.recoverAllLocalClaim(ctx, plan, errors.Join(cause, pendingErr))
	}
	if !settings.OperationID.Valid || settings.OperationID.UUID != pending.ID {
		return s.recoverAllLocalClaim(
			ctx,
			plan,
			errors.Join(cause, errors.New("presence claim recovery marker mismatch")),
		)
	}
	if isAuthoritativeSupersedingClear(settings, pending, plan.OperationID) {
		return ClaimCompletion{Outcome: ClaimSuperseded, Err: cause}
	}
	return s.recoverMappedClaim(ctx, plan, cause)
}

func isAuthoritativeSupersedingClear(
	settings lockedPresenceSettings,
	pending pendingOperationRow,
	planOperationID uuid.UUID,
) bool {
	return pending.ID != planOperationID &&
		pending.PriorVersion != math.MaxInt64 && settings.Version == pending.PriorVersion+1 &&
		settings.OperationID.Valid && settings.OperationID.UUID == pending.ID &&
		(!settings.MasterEnabled ||
			(settings.Tier == 0 && !settings.Text.Valid && !settings.Emoji.Valid))
}

func (s *Service) recoverAllLocalClaim(
	ctx context.Context,
	plan DeliveryPlan,
	cause error,
) ClaimCompletion {
	plan.ClearRecipients = nil
	plan.UpdateRecipients = nil
	plan.Payload = nil
	return s.recoverMappedClaim(ctx, plan, cause)
}

func (s *Service) recoverMappedClaim(
	ctx context.Context,
	plan DeliveryPlan,
	cause error,
) ClaimCompletion {
	if err := s.EmergencyReset(ctx, plan); err != nil {
		return ClaimCompletion{Outcome: ClaimUnresolved, Err: errors.Join(cause, err)}
	}
	return ClaimCompletion{Outcome: ClaimRecovered, Err: cause}
}

func (s *Service) classifyClaimCommit(
	ctx context.Context,
	plan DeliveryPlan,
	prior pendingOperationRow,
) claimCommitState {
	readCtx, cancel := boundedDetachedContext(ctx, commitReadbackTimeout)
	defer cancel()
	readState := s.readClaimState
	if readState == nil {
		readState = s.readAudienceCommitState
	}
	state, err := readState(readCtx, plan.SenderID)
	if err != nil {
		return claimCommitUnresolved
	}
	settingsMatch := state.SettingsExists && uuidPointerEqual(state.OperationID, &plan.OperationID)
	if settingsMatch && state.PendingOperationID == nil {
		return claimCommitConfirmed
	}
	if settingsMatch && uuidPointerEqual(state.PendingOperationID, &plan.OperationID) {
		return claimCommitQuarantined
	}
	if !state.UserExists {
		return claimCommitConfirmed
	}
	if state.SettingsExists && state.PendingOperationID == nil &&
		state.OperationID != nil && *state.OperationID != plan.OperationID &&
		state.Version > prior.PriorVersion {
		return claimCommitSuperseded
	}
	return claimCommitUnresolved
}

func (s *Service) ensureClaimQuarantine(
	ctx context.Context,
	plan DeliveryPlan,
	prior pendingOperationRow,
) (repairState claimCommitState, returnErr error) {
	repairCtx, cancel := boundedDetachedContext(ctx, commitReadbackTimeout)
	defer cancel()
	tx, err := s.BeginTx(repairCtx, nil)
	if err != nil {
		return claimCommitUnresolved, err
	}
	if tx == nil {
		return claimCommitUnresolved, errors.New("missing quarantine repair transaction")
	}
	defer tx.Rollback() //nolint:errcheck
	defer s.joinTransactionRollback(tx, "presence_claim_quarantine_repair", &returnErr)
	repairState, needsInsert, err := inspectClaimQuarantine(repairCtx, tx, plan, prior)
	if err != nil || !needsInsert {
		return repairState, err
	}
	if err := s.insertClaimQuarantine(repairCtx, tx, plan, prior); err != nil {
		return claimCommitUnresolved, err
	}
	return s.commitClaimQuarantineRepair(repairCtx, tx, plan, prior)
}

func inspectClaimQuarantine(
	ctx context.Context,
	tx *sql.Tx,
	plan DeliveryPlan,
	prior pendingOperationRow,
) (claimCommitState, bool, error) {
	if err := lockUser(ctx, tx, plan.SenderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return claimCommitConfirmed, false, nil
		}
		return claimCommitUnresolved, false, err
	}
	settings, err := lockPresenceSettings(ctx, tx, plan.SenderID)
	if err != nil {
		return claimCommitUnresolved, false, err
	}
	pending, pendingErr := lockPendingRow(ctx, tx, plan.SenderID)
	if pendingErr == nil {
		if pending.ID == plan.OperationID ||
			(settings.OperationID.Valid && settings.OperationID.UUID == pending.ID) {
			return claimCommitQuarantined, false, nil
		}
		return claimCommitUnresolved, false, errors.New("quarantine repair found inconsistent pending marker")
	}
	if !errors.Is(pendingErr, sql.ErrNoRows) {
		return claimCommitUnresolved, false, pendingErr
	}
	if settings.OperationID.Valid && settings.OperationID.UUID != plan.OperationID &&
		settings.Version > prior.PriorVersion {
		return claimCommitSuperseded, false, nil
	}
	if !settings.OperationID.Valid || settings.OperationID.UUID != plan.OperationID {
		return claimCommitUnresolved, false, errors.New("quarantine repair settings marker is uncertain")
	}
	return claimCommitUnresolved, true, nil
}

func (s *Service) insertClaimQuarantine(
	ctx context.Context,
	tx *sql.Tx,
	plan DeliveryPlan,
	prior pendingOperationRow,
) error {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read quarantine repair clock: %w", err)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version, created_at, reconcile_after
		) VALUES ($1, $2, $3, $4, $5)
	`, plan.SenderID, plan.OperationID, prior.PriorVersion, now, now.Add(s.effectiveReconcileInterval()))
	if err != nil {
		return fmt.Errorf("restore pending presence operation: %w", err)
	}
	return nil
}

func (s *Service) commitClaimQuarantineRepair(
	ctx context.Context,
	tx *sql.Tx,
	plan DeliveryPlan,
	prior pendingOperationRow,
) (claimCommitState, error) {
	if err := s.CommitTx(tx); err != nil {
		rollbackErr := s.RollbackTx(tx)
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return claimCommitUnresolved, errors.Join(
				fmt.Errorf("commit pending presence repair: %w", err),
				fmt.Errorf("rollback pending presence repair: %w", rollbackErr),
			)
		}
		state := s.classifyClaimCommit(ctx, plan, prior)
		if state == claimCommitConfirmed || state == claimCommitQuarantined ||
			state == claimCommitSuperseded {
			return state, nil
		}
		return claimCommitUnresolved, fmt.Errorf("commit pending presence repair: %w", err)
	}
	return claimCommitQuarantined, nil
}

func (s *Service) deleteClaimedPending(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	operationID uuid.UUID,
) (sql.Result, error) {
	s.transactionMu.RLock()
	deleteClaim := s.transactionHooks.DeleteClaim
	s.transactionMu.RUnlock()
	if deleteClaim != nil {
		return deleteClaim(ctx, tx, senderID, operationID)
	}
	return tx.ExecContext(ctx, `
		DELETE FROM presence_settings_pending_operations
		WHERE user_id = $1 AND operation_id = $2
	`, senderID, operationID)
}

func (s *Service) validateDeliveryPlan(plan DeliveryPlan) (Delivery, error) {
	if s == nil || s.db == nil || s.repository == nil {
		return nil, errors.New("presence history claim unavailable")
	}
	if plan.OperationID == uuid.Nil || plan.SenderID == uuid.Nil ||
		(plan.Mode != DeliveryExactDelta && plan.Mode != DeliveryConservativeReset) {
		return nil, errors.New("invalid presence delivery claim")
	}
	return s.boundDelivery()
}

func (s *Service) failExactClaim(ctx context.Context, plan DeliveryPlan, cause error) (ClaimOutcome, error) {
	if plan.Mode != DeliveryExactDelta {
		return ClaimUnresolved, cause
	}
	resetCtx, cancel := boundedDetachedContext(ctx, s.effectiveDeliveryTimeout())
	defer cancel()
	if err := s.EmergencyReset(resetCtx, plan); err != nil {
		return ClaimUnresolved, errors.Join(cause, fmt.Errorf("emergency presence reset: %w", err))
	}
	return ClaimRecovered, cause
}

// EmergencyReset performs a marker-preserving conservative safety delivery.
func (s *Service) EmergencyReset(ctx context.Context, plan DeliveryPlan) error {
	if plan.OperationID == uuid.Nil || plan.SenderID == uuid.Nil {
		return errors.New("invalid emergency presence reset")
	}
	delivery, err := s.boundDelivery()
	if err != nil {
		return err
	}
	plan.Mode = DeliveryConservativeReset
	deliveryCtx, cancel := context.WithTimeout(ctx, s.effectiveDeliveryTimeout())
	defer cancel()
	ack, err := delivery.DeliverCustomText(deliveryCtx, plan)
	if err != nil {
		return err
	}
	if ack.OperationID != plan.OperationID {
		return errors.New("emergency presence reset acknowledgement mismatch")
	}
	return nil
}

func (s *Service) effectiveDeliveryTimeout() time.Duration {
	if s.deliveryTimeout <= 0 {
		return 5 * time.Second
	}
	return s.deliveryTimeout
}

// ReconcilePending processes one bounded snapshot of eligible markers.
func (s *Service) ReconcilePending(ctx context.Context, limit int) (ReconcileStats, error) {
	runStart := time.Now()
	if s == nil || s.db == nil || s.repository == nil {
		return ReconcileStats{}, errors.New("presence reconciliation unavailable")
	}
	if _, err := s.boundDelivery(); err != nil {
		return ReconcileStats{}, err
	}
	if limit < 1 || limit > maxReconcileBatch {
		return ReconcileStats{}, errors.New("invalid presence reconciliation limit")
	}
	senderIDs, err := s.discoverPending(ctx, limit)
	if err != nil {
		stats := ReconcileStats{FailedCount: 1}
		s.logReconcileStats(stats, time.Since(runStart))
		return stats, err
	}
	stats := ReconcileStats{DiscoveredCount: len(senderIDs)}
	var passErr error
	for _, senderID := range senderIDs {
		result, reconcileErr := s.reconcileOne(ctx, senderID)
		addReconcileResult(&stats, result, reconcileErr)
		if reconcileErr != nil {
			passErr = errors.Join(passErr, reconcileErr)
		}
	}
	s.logReconcileStats(stats, time.Since(runStart))
	return stats, passErr
}

func (s *Service) discoverPending(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id
		FROM presence_settings_pending_operations
		WHERE reconcile_after <= clock_timestamp()
		ORDER BY user_id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("discover pending presence operations: %w", err)
	}
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var senderID uuid.UUID
		if err := rows.Scan(&senderID); err != nil {
			return nil, closeListRowsWithError(rows, fmt.Errorf("scan pending presence operation: %w", err))
		}
		ids = append(ids, senderID)
	}
	if err := rows.Err(); err != nil {
		return nil, closeListRowsWithError(rows, fmt.Errorf("read pending presence operations: %w", err))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close pending presence operations: %w", err)
	}
	return ids, nil
}

func (s *Service) reconcileOne(ctx context.Context, senderID uuid.UUID) (reconcileResult, error) {
	var result reconcileResult
	err := s.WithSender(ctx, senderID, func() error {
		var err error
		result, err = s.reconcileSenderAlreadyGated(ctx, senderID, OrdinaryAudienceWrite)
		return err
	})
	if err != nil && result == (reconcileResult{}) {
		result.retained = true
	}
	return result, err
}

func addReconcileResult(stats *ReconcileStats, result reconcileResult, err error) {
	if result.resolved {
		stats.ResolvedCount++
	}
	if result.proven {
		stats.ProvenCount++
	}
	if result.compensated {
		stats.CompensatedCount++
	}
	if result.retained {
		stats.RetainedCount++
	}
	if err != nil {
		stats.FailedCount++
	}
}

func (s *Service) logReconcileStats(stats ReconcileStats, duration time.Duration) {
	if s.repository == nil || s.repository.log == nil {
		return
	}
	s.repository.log.Info(
		"pending presence reconciliation completed",
		"operation", "presence_reconciliation",
		"discovered_count", stats.DiscoveredCount,
		"resolved_count", stats.ResolvedCount,
		"proven_count", stats.ProvenCount,
		"compensated_count", stats.CompensatedCount,
		"retained_count", stats.RetainedCount,
		"failed_count", stats.FailedCount,
		"duration_ms", duration.Milliseconds(),
	)
}

// RunPendingReconciler retries eligible markers every five seconds until the
// cleanup context is canceled. Startup reconciliation remains explicit.
func (s *Service) RunPendingReconciler(ctx context.Context) {
	interval := s.reconcileInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.ReconcilePending(ctx, maxReconcileBatch)
		}
	}
}

func (s *Service) reconcileSenderAlreadyGated(
	ctx context.Context,
	senderID uuid.UUID,
	mode OperationMode,
) (result reconcileResult, returnErr error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("begin presence reconciliation: %w", err)
	}
	if tx == nil {
		return reconcileResult{retained: true}, errors.New("begin presence reconciliation: missing transaction")
	}
	defer tx.Rollback() //nolint:errcheck
	defer s.joinTransactionRollback(tx, "presence_reconciliation", &returnErr)

	settings, pending, alreadyResolved, err := lockReconcileState(ctx, tx, senderID)
	if alreadyResolved {
		return reconcileResult{resolved: true, proven: true}, nil
	}
	if err != nil {
		return reconcileResult{retained: true}, err
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("read presence reconciliation clock: %w", err)
	}
	if mode == ForcedSecurityClear {
		return reconcileResult{retained: true}, pendingOperationError(pending, now)
	}
	if pending.ReconcileAfter.After(now) {
		return reconcileResult{retained: true}, pendingOperationError(pending, now)
	}
	return s.reconcileLockedPending(ctx, tx, senderID, settings, pending, now)
}

func lockReconcileState(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (lockedPresenceSettings, pendingOperationRow, bool, error) {
	if err := lockUser(ctx, tx, senderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lockedPresenceSettings{}, pendingOperationRow{}, true, nil
		}
		return lockedPresenceSettings{}, pendingOperationRow{}, false, err
	}
	settings, settingsErr := lockPresenceSettings(ctx, tx, senderID)
	pending, pendingErr := lockPendingRow(ctx, tx, senderID)
	if errors.Is(pendingErr, sql.ErrNoRows) {
		return lockedPresenceSettings{}, pendingOperationRow{}, true, nil
	}
	if pendingErr != nil {
		return lockedPresenceSettings{}, pendingOperationRow{}, false, pendingErr
	}
	if settingsErr != nil {
		return lockedPresenceSettings{}, pendingOperationRow{}, false, settingsErr
	}
	return settings, pending, false, nil
}

func pendingOperationError(pending pendingOperationRow, now time.Time) *ServiceError {
	return &ServiceError{
		Status:     503,
		Code:       "presence_operation_pending",
		RetryAfter: ceilRetryAfter(pending.ReconcileAfter.Sub(now)),
	}
}

func (s *Service) reconcileLockedPending(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	settings lockedPresenceSettings,
	pending pendingOperationRow,
	now time.Time,
) (reconcileResult, error) {
	if !settings.OperationID.Valid || settings.OperationID.UUID != pending.ID {
		return s.resolveProvenNonmatching(ctx, tx, senderID, settings, pending)
	}
	if pending.PriorVersion == math.MaxInt64 || settings.Version != pending.PriorVersion+1 {
		return reconcileResult{retained: true}, errors.New("pending presence operation version is uncertain")
	}
	if settings.Version == math.MaxInt64 {
		return reconcileResult{retained: true}, errors.New("presence settings version exhausted")
	}
	if !settings.MasterEnabled || settings.Tier == 0 {
		if err := s.RollbackTx(tx); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return reconcileResult{retained: true}, fmt.Errorf("release presence reconciliation locks: %w", err)
		}
		err := s.ClaimAndDeliver(ctx, DeliveryPlan{
			Mode:        DeliveryConservativeReset,
			OperationID: pending.ID,
			SenderID:    senderID,
		})
		if err != nil {
			return reconcileResult{retained: true}, err
		}
		return reconcileResult{resolved: true}, nil
	}
	return s.compensateAndClaim(ctx, tx, senderID, settings, pending, now)
}

func lockPresenceSettings(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (lockedPresenceSettings, error) {
	var settings lockedPresenceSettings
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
		&settings.Version,
		&settings.OperationID,
		&settings.MasterEnabled,
		&settings.ServerVoiceTier,
		&settings.ServerVoiceShowDetails,
		&settings.PrivateCallTier,
		&settings.PrivateCallShowDetails,
		&settings.Tier,
		&settings.Text,
		&settings.Emoji,
	)
	if err != nil {
		return lockedPresenceSettings{}, fmt.Errorf("lock presence reconciliation settings: %w", err)
	}
	return settings, nil
}

func lockPendingRow(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (pendingOperationRow, error) {
	var pending pendingOperationRow
	err := tx.QueryRowContext(ctx, `
		SELECT operation_id, prior_settings_version, reconcile_after
		FROM presence_settings_pending_operations
		WHERE user_id = $1
		FOR UPDATE
	`, senderID).Scan(&pending.ID, &pending.PriorVersion, &pending.ReconcileAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingOperationRow{}, sql.ErrNoRows
	}
	if err != nil {
		return pendingOperationRow{}, fmt.Errorf("lock pending presence reconciliation: %w", err)
	}
	return pending, nil
}

func (s *Service) resolveProvenNonmatching(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	settings lockedPresenceSettings,
	pending pendingOperationRow,
) (reconcileResult, error) {
	rollbackProven := settings.Version == pending.PriorVersion
	superseded := settings.Version > pending.PriorVersion &&
		settings.OperationID.Valid && settings.OperationID.UUID != pending.ID
	if !rollbackProven && !superseded {
		return reconcileResult{retained: true}, errors.New("pending presence operation proof is uncertain")
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM presence_settings_pending_operations
		WHERE user_id = $1 AND operation_id = $2
	`, senderID, pending.ID)
	if err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("delete proven pending presence operation: %w", err)
	}
	if err := requireOneAffected(result, "delete proven pending presence operation"); err != nil {
		return reconcileResult{retained: true}, err
	}
	if err := s.CommitTx(tx); err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("commit proven presence reconciliation: %w", err)
	}
	return reconcileResult{resolved: true, proven: true}, nil
}

func (s *Service) compensateAndClaim(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	settings lockedPresenceSettings,
	pending pendingOperationRow,
	now time.Time,
) (reconcileResult, error) {
	operationID := uuid.New()
	retryAfter := now.Add(s.effectiveReconcileInterval())
	result, err := tx.ExecContext(ctx, `
		UPDATE presence_settings_pending_operations
		SET operation_id = $3,
		    prior_settings_version = $4,
		    created_at = $5,
		    reconcile_after = $6
		WHERE user_id = $1 AND operation_id = $2
	`, senderID, pending.ID, operationID, settings.Version, now, retryAfter)
	if err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("replace compensated presence operation: %w", err)
	}
	if err := requireOneAffected(result, "replace compensated presence operation"); err != nil {
		return reconcileResult{retained: true}, err
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET master_enabled = FALSE,
		    custom_text_tier = 0,
		    custom_text = NULL,
		    custom_text_emoji = NULL,
		    presence_settings_version = $2,
		    presence_settings_operation_id = $3,
		    updated_at = $4
		WHERE user_id = $1
	`, senderID, settings.Version+1, operationID, now)
	if err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("store compensated presence reset: %w", err)
	}
	if err := requireOneAffected(result, "store compensated presence reset"); err != nil {
		return reconcileResult{retained: true}, err
	}
	before := normalizeCustomTextState(CustomTextState{
		Text:  nullableString(settings.Text),
		Emoji: nullableString(settings.Emoji),
	})
	if err := s.RecordCustomTextTransition(ctx, tx, senderID, before, CustomTextState{}); err != nil {
		return reconcileResult{retained: true}, fmt.Errorf("record compensated presence reset: %w", err)
	}
	operation := compensationAudienceOperation(senderID, operationID, settings, pending)
	if err := s.CommitTx(tx); err != nil {
		rollbackErr := s.RollbackTx(tx)
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return reconcileResult{retained: true}, errors.Join(
				fmt.Errorf("commit compensated presence reset: %w", err),
				fmt.Errorf("rollback compensated presence reset: %w", rollbackErr),
			)
		}
		if s.ClassifyAudienceCommit(ctx, operation) != CommitConfirmed {
			return reconcileResult{retained: true}, fmt.Errorf("commit compensated presence reset: %w", err)
		}
	}
	claimErr := s.ClaimAndDeliver(ctx, DeliveryPlan{
		Mode:        DeliveryConservativeReset,
		OperationID: operationID,
		SenderID:    senderID,
	})
	if claimErr != nil {
		return reconcileResult{compensated: true, retained: true}, claimErr
	}
	return reconcileResult{resolved: true, compensated: true}, nil
}

func compensationAudienceOperation(
	senderID uuid.UUID,
	operationID uuid.UUID,
	settings lockedPresenceSettings,
	pending pendingOperationRow,
) AudienceOperation {
	priorID := pending.ID
	return AudienceOperation{
		ID:                           operationID,
		SenderID:                     senderID,
		PriorVersion:                 settings.Version,
		Version:                      settings.Version + 1,
		PriorOperationID:             &priorID,
		Before:                       normalizeCustomTextState(CustomTextState{Text: nullableString(settings.Text), Emoji: nullableString(settings.Emoji)}),
		BeforeTier:                   settings.Tier,
		BeforeMasterEnabled:          settings.MasterEnabled,
		BeforeServerVoiceTier:        settings.ServerVoiceTier,
		BeforeServerVoiceShowDetails: settings.ServerVoiceShowDetails,
		BeforePrivateCallTier:        settings.PrivateCallTier,
		BeforePrivateCallShowDetails: settings.PrivateCallShowDetails,
	}
}

func (s *Service) effectiveReconcileInterval() time.Duration {
	if s.reconcileInterval <= 0 {
		return 5 * time.Second
	}
	return s.reconcileInterval
}

func (s *Service) joinTransactionRollback(tx *sql.Tx, operation string, returnErr *error) {
	rollbackErr := s.RollbackTx(tx)
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return
	}
	*returnErr = errors.Join(*returnErr, fmt.Errorf("rollback %s: %w", operation, rollbackErr))
	if s.repository == nil || s.repository.log == nil {
		return
	}
	s.repository.log.Warn(
		"presence reconciliation rollback failed",
		"operation", operation,
		"error_class", "rollback_failure",
		"rollback_count", 1,
	)
}
