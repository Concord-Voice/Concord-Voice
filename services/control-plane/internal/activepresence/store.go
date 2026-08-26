package activepresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// PlanKey identifies one obligation. It is the table's primary key, and it is
// also exactly what the clear frame is keyed on.
type PlanKey struct {
	SubjectID uuid.UUID
	Category  presence.Category
}

// InsertPlanTx writes one obligation inside the mutation's own transaction.
//
// On conflict it RATCHETS to conservative rather than overwriting. The merge
// drops no obligation because the terminal is a generation-agnostic clear frame
// keyed on (user_id, category) -- one frame retracts whatever a client held for
// that pair, from either colliding generation. Rows for different subjects never
// collide, so no subject's obligation is absorbed into another's.
func InsertPlanTx(ctx context.Context, tx *sql.Tx, p Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("activepresence: plan insert requires a transaction")
	}
	// SQL NULL, not uuid.Nil: a conservative plan has no generation, and the
	// _exact_evidence_check reads NULL rather than a zero UUID.
	var lifecycle any
	if p.Resolution == ResolutionExact {
		lifecycle = p.LifecycleID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO presence_active_pending_plans (
			user_id, category, operation_id, resolution, scope_lifecycle_id,
			scope_event_at, attempts
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, category) DO UPDATE SET
			operation_id       = EXCLUDED.operation_id,
			resolution         = 'conservative',
			scope_lifecycle_id = NULL,
			scope_event_at     = GREATEST(
				presence_active_pending_plans.scope_event_at, EXCLUDED.scope_event_at),
			reconcile_after    = LEAST(
				presence_active_pending_plans.reconcile_after, EXCLUDED.reconcile_after),
			attempts           = 0,
			failure_class      = NULL`,
		p.SubjectID, string(p.Category), p.OperationID, p.Resolution.String(),
		lifecycle, p.EventAt, p.Attempts,
	)
	if err != nil {
		return fmt.Errorf("insert active-category pending plan: %w", err)
	}
	return nil
}

// DiscoverDue lists due plans without taking a lock. Its result is a HINT: the
// caller must re-read each row under FOR UPDATE before acting on it. Acting on
// this snapshot is the bug.
func DiscoverDue(ctx context.Context, db *sql.DB, limit int) ([]PlanKey, error) {
	if db == nil {
		return nil, errors.New("activepresence: discovery requires a database")
	}
	if limit < 1 || limit > maxPlanBatch {
		limit = maxPlanBatch
	}
	rows, err := db.QueryContext(ctx, `
		SELECT user_id, category
		FROM presence_active_pending_plans
		WHERE reconcile_after <= clock_timestamp()
		ORDER BY user_id, category
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("discover due active-category plans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []PlanKey
	for rows.Next() {
		var key PlanKey
		var category string
		if err := rows.Scan(&key.SubjectID, &category); err != nil {
			return nil, fmt.Errorf("scan due active-category plan: %w", err)
		}
		parsed, err := ParseCategory(category)
		if err != nil {
			return nil, err
		}
		key.Category = parsed
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due active-category plans: %w", err)
	}
	return keys, nil
}

// ClaimTx re-reads one plan under FOR UPDATE. found is false when the row is
// gone (another replica finished it) or is no longer due (another replica has
// claimed it by advancing reconcile_after).
func ClaimTx(ctx context.Context, tx *sql.Tx, key PlanKey) (Plan, bool, error) {
	if tx == nil {
		return Plan{}, false, errors.New("activepresence: claim requires a transaction")
	}
	var (
		plan       Plan
		category   string
		resolution string
		lifecycle  *uuid.UUID
		due        bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT operation_id, category, resolution, scope_lifecycle_id, scope_event_at,
		       attempts, reconcile_after <= clock_timestamp()
		FROM presence_active_pending_plans
		WHERE user_id = $1 AND category = $2
		FOR UPDATE`, key.SubjectID, string(key.Category),
	).Scan(&plan.OperationID, &category, &resolution, &lifecycle,
		&plan.EventAt, &plan.Attempts, &due)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, fmt.Errorf("claim active-category plan: %w", err)
	}
	if !due {
		return Plan{}, false, nil
	}
	parsedCategory, err := ParseCategory(category)
	if err != nil {
		return Plan{}, false, err
	}
	parsedResolution, err := ParseResolution(resolution)
	if err != nil {
		return Plan{}, false, err
	}
	plan.SubjectID = key.SubjectID
	plan.Category = parsedCategory
	plan.Resolution = parsedResolution
	if lifecycle != nil {
		plan.LifecycleID = *lifecycle
	}
	return plan, true, nil
}

// RecordAttemptTx commits the claim. Advancing reconcile_after under the row
// lock is what stops a second replica from delivering the same plan.
func RecordAttemptTx(
	ctx context.Context,
	tx *sql.Tx,
	p Plan,
	class FailureClass,
	backoff time.Duration,
) error {
	if tx == nil {
		return errors.New("activepresence: attempt record requires a transaction")
	}
	if p.Attempts+1 >= maxPlanAttempts {
		backoff = quarantineInterval
	}
	// FailureNone has no member in the failure_class vocabulary -- its String()
	// renders the fail-closed "plan_invalid". Persisting that would file a
	// SUCCESSFUL reconcile as a failure, and the CHECK would accept it. A
	// success stores SQL NULL.
	var storedClass any
	if class != FailureNone {
		storedClass = class.String()
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE presence_active_pending_plans
		SET attempts        = attempts + 1,
		    failure_class   = $3,
		    reconcile_after = clock_timestamp() + $4::interval
		WHERE user_id = $1 AND category = $2`,
		p.SubjectID, string(p.Category), storedClass,
		fmt.Sprintf("%d milliseconds", backoff.Milliseconds()),
	)
	if err != nil {
		return fmt.Errorf("record active-category plan attempt: %w", err)
	}
	return nil
}

// DeletePlanTx is the acknowledgement. operation_id is the authorization: a plan
// rewritten by a newer mutation has a different id and survives.
func DeletePlanTx(ctx context.Context, tx *sql.Tx, p Plan) error {
	if tx == nil {
		return errors.New("activepresence: acknowledgement requires a transaction")
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM presence_active_pending_plans
		WHERE user_id = $1 AND category = $2 AND operation_id = $3`,
		p.SubjectID, string(p.Category), p.OperationID)
	if err != nil {
		return fmt.Errorf("acknowledge active-category plan: %w", err)
	}
	return nil
}

// DrainSubjectTx removes every obligation for one subject and reports which
// categories it removed, so the caller can transfer the obligation to a
// post-commit clear. It exists for account erasure, where the FK's RESTRICT
// would otherwise fail the erasure with an opaque 23503.
func DrainSubjectTx(
	ctx context.Context,
	tx *sql.Tx,
	subjectID uuid.UUID,
) ([]presence.Category, error) {
	if tx == nil {
		return nil, errors.New("activepresence: drain requires a transaction")
	}
	rows, err := tx.QueryContext(ctx, `
		DELETE FROM presence_active_pending_plans
		WHERE user_id = $1
		RETURNING category`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("drain active-category plans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var categories []presence.Category
	for rows.Next() {
		var category string
		if err := rows.Scan(&category); err != nil {
			return nil, fmt.Errorf("scan drained active-category plan: %w", err)
		}
		parsed, err := ParseCategory(category)
		if err != nil {
			return nil, err
		}
		categories = append(categories, parsed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate drained active-category plans: %w", err)
	}
	return categories, nil
}
