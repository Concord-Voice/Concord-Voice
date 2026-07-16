package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
)

// ForcedClearResult is the complete transaction and delivery evidence for a
// destructive key or recovery operation that must archive Custom Status.
type ForcedClearResult struct {
	Mode      OperationMode
	Operation AudienceOperation
	Plan      DeliveryPlan
}

// ForcedClearOutcome describes the durable result of committing and
// acknowledging a forced security clear.
type ForcedClearOutcome uint8

const (
	// ForcedClearAcknowledged means the main transaction and matching delivery acknowledgement completed.
	ForcedClearAcknowledged ForcedClearOutcome = iota
	// ForcedClearQuarantined means the clear may be durable and its exact marker remains pending.
	ForcedClearQuarantined
	// ForcedClearRolledBack means the destructive main transaction is proven not durable.
	ForcedClearRolledBack
	// ForcedClearSuperseded means a later audience operation replaced the attempted clear.
	ForcedClearSuperseded
	// ForcedClearUnresolved means durability could not be proven and conservative reset was attempted.
	ForcedClearUnresolved
)

// ForcedClearCompletion preserves the exact commit/delivery outcome and every
// underlying cause without exposing private operation data.
type ForcedClearCompletion struct {
	Outcome ForcedClearOutcome
	Err     error
}

// RequiresDisconnect reports whether the destructive main transaction may be
// durable and existing authenticated sessions must therefore be disconnected.
func (c ForcedClearCompletion) RequiresDisconnect() bool {
	switch c.Outcome {
	case ForcedClearAcknowledged, ForcedClearQuarantined, ForcedClearSuperseded, ForcedClearUnresolved:
		return true
	default:
		return false
	}
}

// BeginForcedSecurityClear acquires the canonical audience-operation locks,
// captures the prior authorized audience, archives the present-to-absent
// transition, and prepares a conservative clear without committing tx.
func (s *Service) BeginForcedSecurityClear(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
) (ForcedClearResult, error) {
	if s == nil || tx == nil || senderID == uuid.Nil {
		return ForcedClearResult{}, errors.New("forced security clear unavailable")
	}
	operation, err := s.BeginAudienceOperation(ctx, tx, senderID, ForcedSecurityClear)
	if err != nil {
		return ForcedClearResult{}, err
	}
	recipients, err := prepareForcedClearAudience(ctx, tx, operation)
	if err != nil {
		return ForcedClearResult{}, fmt.Errorf("prepare forced security audience: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET custom_text_tier = 0,
		    custom_text = NULL,
		    custom_text_emoji = NULL,
		    updated_at = clock_timestamp()
		WHERE user_id = $1
	`, senderID)
	if err != nil {
		return ForcedClearResult{}, fmt.Errorf("clear forced security Custom Status: %w", err)
	}
	if err := requireOneAffected(result, "clear forced security Custom Status"); err != nil {
		return ForcedClearResult{}, err
	}
	if err := s.RecordCustomTextTransition(
		ctx, tx, senderID, operation.Before, CustomTextState{},
	); err != nil {
		return ForcedClearResult{}, fmt.Errorf("archive forced security Custom Status: %w", err)
	}
	updates := map[uuid.UUID]bool{}
	if recipients == nil {
		updates = nil
	}
	return ForcedClearResult{
		Mode:      ForcedSecurityClear,
		Operation: operation,
		Plan: DeliveryPlan{
			Mode:             DeliveryConservativeReset,
			OperationID:      operation.ID,
			SenderID:         senderID,
			ClearRecipients:  recipients,
			UpdateRecipients: updates,
		},
	}, nil
}

func prepareForcedClearAudience(
	ctx context.Context,
	tx *sql.Tx,
	operation AudienceOperation,
) (map[uuid.UUID]bool, error) {
	if operation.SupersededPending {
		return nil, nil
	}
	if !operation.BeforeMasterEnabled || operation.BeforeTier <= 0 || operation.Before.Text == "" {
		return map[uuid.UUID]bool{}, nil
	}
	return presence.ComputeCustomTextAudienceForTier(
		ctx, tx, operation.SenderID, operation.BeforeTier,
	)
}

// CompleteForcedSecurityClear commits the main transaction, classifies an
// ambiguous commit, and acknowledges only the matching security marker.
func (s *Service) CompleteForcedSecurityClear(
	ctx context.Context,
	tx *sql.Tx,
	result ForcedClearResult,
) ForcedClearCompletion {
	if err := validateForcedClearResult(s, tx, result); err != nil {
		var rollbackErr error
		if s != nil && tx != nil {
			rollbackErr = s.RollbackTx(tx)
			if errors.Is(rollbackErr, sql.ErrTxDone) {
				rollbackErr = nil
			}
		}
		return ForcedClearCompletion{
			Outcome: ForcedClearRolledBack,
			Err:     errors.Join(err, rollbackErr),
		}
	}
	commitErr := s.CommitTx(tx)
	var confirmedCommitErr error
	if commitErr != nil {
		var terminal *ForcedClearCompletion
		terminal, confirmedCommitErr = s.classifyFailedForcedClearCommit(
			ctx, tx, result, commitErr,
		)
		if terminal != nil {
			return *terminal
		}
	}
	claim := s.CompleteClaim(ctx, result.Plan)
	return forcedClearCompletionFromClaim(claim, confirmedCommitErr)
}

func (s *Service) classifyFailedForcedClearCommit(
	ctx context.Context,
	tx *sql.Tx,
	result ForcedClearResult,
	commitErr error,
) (*ForcedClearCompletion, error) {
	rollbackErr := s.RollbackTx(tx)
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		commitErr = errors.Join(
			commitErr,
			fmt.Errorf("rollback forced security clear: %w", rollbackErr),
		)
	}
	var completion ForcedClearCompletion
	switch s.ClassifyAudienceCommit(ctx, result.Operation) {
	case CommitConfirmed:
		return nil, commitErr
	case RollbackConfirmed:
		completion = ForcedClearCompletion{Outcome: ForcedClearRolledBack, Err: commitErr}
	case WriteSuperseded:
		completion = ForcedClearCompletion{Outcome: ForcedClearSuperseded, Err: commitErr}
	default:
		completion = s.resolveUnresolvedForcedClear(ctx, result, commitErr)
	}
	return &completion, nil
}

func forcedClearCompletionFromClaim(
	claim ClaimCompletion,
	confirmedCommitErr error,
) ForcedClearCompletion {
	claimErr := errors.Join(confirmedCommitErr, claim.Err)
	if claim.Outcome != ClaimAcknowledged && claimErr == nil {
		claimErr = errors.New("forced security clear claim incomplete")
	}
	switch claim.Outcome {
	case ClaimAcknowledged:
		return ForcedClearCompletion{Outcome: ForcedClearAcknowledged, Err: claimErr}
	case ClaimSuperseded:
		return ForcedClearCompletion{Outcome: ForcedClearSuperseded, Err: claimErr}
	case ClaimRecovered:
		return ForcedClearCompletion{Outcome: ForcedClearQuarantined, Err: claimErr}
	default:
		return ForcedClearCompletion{Outcome: ForcedClearQuarantined, Err: claimErr}
	}
}

func validateForcedClearResult(s *Service, tx *sql.Tx, result ForcedClearResult) error {
	if s == nil || tx == nil || result.Mode != ForcedSecurityClear ||
		result.Operation.ID == uuid.Nil || result.Operation.SenderID == uuid.Nil ||
		result.Plan.Mode != DeliveryConservativeReset ||
		result.Plan.OperationID != result.Operation.ID ||
		result.Plan.SenderID != result.Operation.SenderID {
		return errors.New("invalid forced security clear completion")
	}
	return nil
}

func (s *Service) resolveUnresolvedForcedClear(
	ctx context.Context,
	result ForcedClearResult,
	cause error,
) ForcedClearCompletion {
	prior := pendingOperationRow{
		ID:             result.Operation.ID,
		PriorVersion:   result.Operation.PriorVersion,
		ReconcileAfter: result.Operation.ReconcileAfter,
	}
	repairState, repairErr := s.ensureClaimQuarantine(ctx, result.Plan, prior)
	if repairState == claimCommitConfirmed || repairState == claimCommitSuperseded {
		return ForcedClearCompletion{
			Outcome: ForcedClearSuperseded,
			Err:     errors.Join(cause, repairErr),
		}
	}
	plan := result.Plan
	if repairState != claimCommitQuarantined {
		plan.ClearRecipients = nil
		plan.UpdateRecipients = nil
		plan.Payload = nil
	}
	resetErr := s.EmergencyReset(context.WithoutCancel(ctx), plan)
	return ForcedClearCompletion{
		Outcome: ForcedClearUnresolved,
		Err:     errors.Join(cause, repairErr, resetErr),
	}
}
