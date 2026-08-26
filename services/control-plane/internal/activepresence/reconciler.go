package activepresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// deliveryTimeout bounds one plan's terminal. Detached from the request context
// so a cancelled request cannot cancel reconciliation.
const deliveryTimeout = 5 * time.Second

// claimTimeout bounds the TRANSACTIONAL segment of one plan -- BeginTx, the
// FOR UPDATE claim, and the resolver's Redis read -- which deliver() and
// acknowledge() do NOT cover: each of those detaches and applies its own
// deliveryTimeout, so neither inherits a bound from here.
//
// It exists because CompleteAlreadyGated detaches with context.WithoutCancel,
// and that strips the DEADLINE as well as cancellation. Without this the claim
// and resolve steps ran unbounded while the caller held its sender gate -- a
// buffer-1 channel -- so one hung database or Redis pinned that stripe
// indefinitely and every sender hashing to it stalled behind the hang. The
// blast radius is the stripe, not the request, which is what makes it a
// liveness bug rather than a slow call.
//
// Deliberately LONGER than deliveryTimeout, and not a sum of the other bounds.
// The claim is a FOR UPDATE that may legitimately wait on another replica
// holding the same row, so a tight bound would convert an outage-only hang
// into a premature abort under ordinary contention -- failing when the system
// is merely busy rather than broken, which is the worse trade. Ten seconds is
// two reconcile intervals: long enough that a contended claim completes,
// short enough that a wedged dependency releases the gate in bounded time.
const claimTimeout = 10 * time.Second

// Deliverer is the terminal. ClearSenderActiveCategory is the ORDINARY arm;
// DisconnectAllRichPresenceClients is reserved for the integrity anomalies no
// in-repo writer can produce (resolver branches B2 and B3). Never route the
// ordinary arm through the disconnect -- PR #2840 proved that sink is a
// denial-of-service primitive, and this rail's ordinary producer is
// DELETE /api/v1/dm/conversations/:id, bounded only by RateLimitByUser
// (5/min/user) -- a bound, but not one that makes a fleet-wide disconnect safe.
type Deliverer interface {
	ClearSenderActiveCategory(uuid.UUID, presence.Category)
	DisconnectAllRichPresenceClients(context.Context) error
}

// GenerationDeleter removes the stale generation exactly. It is
// ActivityStore.CompareAndDelete and never ActivityStore.Delete, which ignores
// the generation and would destroy a newer one that raced in.
type GenerationDeleter interface {
	CompareAndDelete(
		context.Context, uuid.UUID, presence.Category, uuid.UUID, int64,
	) (bool, error)
}

// Gate is presencehistory.Service.WithSenders. Both rails share ONE gate array;
// a duplicated array would not be a gate.
type Gate interface {
	WithSenders(context.Context, []uuid.UUID, func() error) error
}

// ErrReconcilerNotWired fails the pass CLOSED. A pass that discovered, claimed
// and acknowledged plans it could never deliver would discharge the obligation
// on paper and drop the clear.
var ErrReconcilerNotWired = errors.New("activepresence: reconciler is not wired")

// Reconciler owns the claim/resolve/deliver/ack loop and its ticker.
type Reconciler struct {
	db        *sql.DB
	gate      Gate
	reader    StateReader
	deleter   GenerationDeleter
	deliverer Deliverer
	log       *logger.Logger
	interval  time.Duration
}

// NewReconciler wires the pass. log may be nil.
func NewReconciler(
	db *sql.DB,
	gate Gate,
	reader StateReader,
	deleter GenerationDeleter,
	deliverer Deliverer,
	log *logger.Logger,
) *Reconciler {
	return &Reconciler{
		db: db, gate: gate, reader: reader, deleter: deleter,
		deliverer: deliverer, log: log, interval: reconcileInterval,
	}
}

// HasDeliverer reports whether the terminal is wired. The boot guard asks this
// rather than checking the Reconciler pointer, which is never nil.
func (r *Reconciler) HasDeliverer() bool {
	return r != nil && r.deliverer != nil
}

// PassStats is the aggregate one pass reports. Counts only -- no subject, no
// category, no payload.
type PassStats struct {
	Acked        int
	Cleared      int
	Disconnected int
	Retained     int
	Quarantined  int
}

// passState coalesces the anomaly arm. At most ONE fleet-wide disconnect per
// pass, no matter how many anomalous plans resolve in it, and every plan that
// reached the arm acknowledges off that single success. The OUTCOME is latched,
// not merely the attempt: a failed disconnect must not be retried once per plan.
type passState struct {
	stats             PassStats
	disconnectDone    bool
	disconnectSucceed bool
}

// Run ticks the pass until the context is cancelled. A structural twin of
// presencehistory's RunPendingReconciler, deliberately duplicated rather than
// hooked: the rails must not starve each other, and presencehistory's
// reconciler stays untouched.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.interval
	if interval <= 0 {
		interval = reconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = r.ReconcilePass(ctx, maxPlanBatch)
		}
	}
}

// ReconcilePass discovers due plans and runs each one's terminal, one subject
// at a time inside that subject's sender gate.
func (r *Reconciler) ReconcilePass(ctx context.Context, limit int) (PassStats, error) {
	if r == nil || r.db == nil || r.gate == nil || r.deliverer == nil {
		return PassStats{}, ErrReconcilerNotWired
	}
	keys, err := DiscoverDue(ctx, r.db, limit)
	if err != nil {
		// Run discards this error, so without a line here a rail that can no
		// longer reach its own table reconciles nothing and says nothing --
		// plans accumulate with zero operator signal. The boot guard covers an
		// UNWIRED rail; it cannot see a runtime database failure.
		//
		// The class is a fixed literal rather than a FailureClass member on
		// purpose: that vocabulary is closed because its values are PERSISTED
		// under a CHECK, and discovery fails before any plan is claimed, so
		// there is no row to attach one to. A member that must never be
		// written would be a footgun. The error itself is never interpolated.
		if r.log != nil {
			r.log.Error("active-category plan discovery failed", "failure_class", "discovery")
		}
		return PassStats{}, err
	}
	pass := &passState{}
	for _, key := range keys {
		subject := key.SubjectID
		gateErr := r.gate.WithSenders(ctx, []uuid.UUID{subject}, func() error {
			return r.resolveOneAlreadyGated(ctx, key, pass)
		})
		// Fixed class only. An interpolated database error here would put data
		// into a log line; the row survives and the next pass retries it.
		r.logFailure("active-category reconciliation failed", gateErr, FailureDelivery)
	}
	r.logPass(pass.stats)
	return pass.stats, nil
}

// resolveOneAlreadyGated MUST be called with the subject's sender gate already
// held. It never re-enters WithSenders: that gate is a buffer-1 channel with no
// timeout, so re-entry blocks forever.
func (r *Reconciler) resolveOneAlreadyGated(
	ctx context.Context,
	key PlanKey,
	pass *passState,
) error {
	// Bound the transactional segment. deliver() and acknowledge() below keep
	// taking the caller's ctx: each detaches and applies deliveryTimeout of its
	// own, so they are already bounded and must not inherit this one.
	txCtx, cancelTx := context.WithTimeout(ctx, claimTimeout)
	defer cancelTx()

	tx, err := r.db.BeginTx(txCtx, nil)
	if err != nil {
		return fmt.Errorf("begin active-category reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-reading under FOR UPDATE is mandatory. Acting on the discovery
	// snapshot is the bug: the row may already be gone, or already claimed.
	plan, found, err := ClaimTx(txCtx, tx, key)
	if err != nil {
		return err
	}
	if !found {
		// The plan is already gone or already claimed by another replica. That
		// is a no-op, not a failure -- but the commit can still fail, and an
		// unwrapped error here is indistinguishable from any other database
		// error in the logs. The three sibling commits below all name their
		// operation; this one was the odd one out.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit active-category no-op: %w", err)
		}
		return nil
	}

	decision := Resolve(txCtx, r.reader, plan, time.Now())

	switch decision.Outcome {
	case OutcomeStateAbsent, OutcomeSuperseded:
		// No delivery is owed. B1's viewers have already expired their copy;
		// B4's generation belongs to a LIVE successor, so touching Redis here
		// would kill a live call's presence.
		if err := DeletePlanTx(txCtx, tx, plan); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit active-category acknowledgement: %w", err)
		}
		pass.stats.Acked++
		return nil

	case OutcomeCleared, OutcomeDisconnected:
		// Commit the claim BEFORE delivering. Advancing reconcile_after under
		// the row lock is what stops a second replica delivering the same plan.
		if err := RecordAttemptTx(txCtx, tx, plan, FailureNone, r.interval); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit active-category claim: %w", err)
		}
		if !r.deliver(ctx, plan, decision, pass) {
			return nil
		}
		return r.acknowledge(ctx, plan)

	default:
		// OutcomeRetained, OutcomeQuarantined, and any value outside the closed
		// vocabulary. Never acknowledge: the row is the evidence that an
		// obligation is undischarged, and dropping it fails open.
		if err := RecordAttemptTx(txCtx, tx, plan, decision.Failure, r.interval); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit active-category retention: %w", err)
		}
		if decision.Outcome == OutcomeQuarantined {
			pass.stats.Quarantined++
		} else {
			pass.stats.Retained++
		}
		return nil
	}
}

// deliver runs the terminal and reports whether the plan may be acknowledged.
//
// The two arms are structurally separate so the ordinary one cannot acquire the
// disconnect sink by accident: step (a) lives inside the CLEAR arm only, and
// the disconnect arm reaches no Redis write at all.
func (r *Reconciler) deliver(
	ctx context.Context,
	plan Plan,
	decision Decision,
	pass *passState,
) bool {
	// Detached: a cancelled request must not cancel reconciliation.
	deliverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
	defer cancel()

	switch decision.Outcome {
	case OutcomeCleared:
		r.deleteGeneration(deliverCtx, plan, decision)
		// Step (b): one proportional clear frame for this subject. NEVER a
		// disconnect -- this arm is user-reachable.
		r.deliverer.ClearSenderActiveCategory(plan.SubjectID, plan.Category)
		pass.stats.Cleared++
		return true

	case OutcomeDisconnected:
		// B2/B3 only, and coalesced to one disconnect per pass. The precondition
		// is unreachable through any in-repo writer, so a user cannot create it.
		if !pass.disconnectDone {
			pass.disconnectDone = true
			err := r.deliverer.DisconnectAllRichPresenceClients(deliverCtx)
			pass.disconnectSucceed = err == nil
			r.logFailure("active-category anomaly disconnect failed", err, FailureDelivery)
		}
		if !pass.disconnectSucceed {
			return false
		}
		pass.stats.Disconnected++
		return true

	default:
		// Unreachable through Resolve. Fails CLOSED: no delivery, no
		// acknowledgement, so the obligation survives for the next pass.
		return false
	}
}

// deleteGeneration is terminal step (a): remove the stale generation, exactly.
//
// B5 ONLY, and only when the decision carries a generation. B2 and B3 return
// before decoding, so they have nothing to compare against; and the
// absent-but-young case resolves Cleared with a ZERO state, which
// activityGenerationKey rejects. Decision.HasGeneration is that gate -- a bare
// token != uuid.Nil test admits a zero source version and turns every such plan
// into a spurious generation_delete failure.
//
// Best effort: a failure here does not block the clear frame, which is what
// actually retracts the display.
func (r *Reconciler) deleteGeneration(ctx context.Context, plan Plan, decision Decision) {
	if !decision.HasGeneration() || r.deleter == nil {
		return
	}
	_, err := r.deleter.CompareAndDelete(
		ctx, plan.SubjectID, plan.Category,
		decision.State.SourceToken, decision.State.SourceVersion,
	)
	r.logFailure("active-category generation delete failed", err, FailureGenerationDelete)
}

// acknowledge is the second short transaction. operation_id is the
// authorization: a plan rewritten between claim and ack has a different id and
// survives.
func (r *Reconciler) acknowledge(ctx context.Context, plan Plan) error {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
	defer cancel()

	tx, err := r.db.BeginTx(ackCtx, nil)
	if err != nil {
		return fmt.Errorf("begin active-category acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := DeletePlanTx(ackCtx, tx, plan); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit active-category acknowledgement: %w", err)
	}
	return nil
}

// logFailure emits a FIXED class and nothing else. The error itself is never
// interpolated: an untyped class field is how a wrapped database error -- and
// therefore data -- ends up in a log line.
func (r *Reconciler) logFailure(msg string, err error, class FailureClass) {
	if err == nil || r.log == nil {
		return
	}
	r.log.Error(msg, "failure_class", class.String())
}

// logPass emits ONE aggregate line per pass. Fixed classes and counts only --
// never a subject id, a category payload, or a wrapped database error.
func (r *Reconciler) logPass(stats PassStats) {
	if r.log == nil || stats == (PassStats{}) {
		return
	}
	r.log.Info("active-category reconciliation pass complete",
		"acked_count", stats.Acked,
		"cleared_count", stats.Cleared,
		"disconnected_count", stats.Disconnected,
		"retained_count", stats.Retained,
		"quarantined_count", stats.Quarantined,
	)
}
