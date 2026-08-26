package activepresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ErrTooManySubjects reports a capture naming more subjects than the rail's
// bound. Exceeding the bound is a derivation bug, not load, so it fails CLOSED.
var ErrTooManySubjects = errors.New("activepresence: subject count exceeds the rail bound")

// ErrRailNotWired reports a rail missing the collaborators its entry point
// needs. It fails CLOSED: a mutation that cannot capture or deliver its
// obligation must fail rather than proceed and drop the clear.
var ErrRailNotWired = errors.New("activepresence: rail is not wired")

// ErrDeliveryIncomplete marks a POST-COMMIT failure: the mutation is durable and
// only its presence delivery did not settle. Callers map it to 503, never 500 --
// a 500 on a mutation that DID commit invites a destructive retry against state
// that no longer exists. The plan row survives for the reconciler either way.
var ErrDeliveryIncomplete = errors.New("activepresence: presence delivery did not settle")

// The closed category vocabulary, re-exported at the seam.
//
// A producer package names its plans' category through these rather than
// importing internal/presence directly: the rail is the leaf that owns the
// presence dependency, and internal/dm deliberately has none (spec section 7).
const (
	CategoryServerVoice = presence.CategoryServerVoice
	CategoryPrivateCall = presence.CategoryPrivateCall
)

// Rail is the public seam consumer packages hold.
//
// TWO FLAVOURS OF EVERY ENTRY POINT, and choosing wrong hangs the process
// forever rather than returning an error:
//
//   - WithGatedTx acquires the sender gates and then opens the transaction. Use
//     it from a caller that is NOT already inside a gate (internal/dm).
//   - CompleteAlreadyGated / DrainAlreadyGated assume the gate is already held.
//     Use them from a caller that IS (internal/users, whose DeleteAccount
//     documents at delete_account.go:309-311 that it deliberately avoids
//     presencehook.WithGatedTx for exactly this reason, and internal/dm's own
//     post-commit completion, which is still inside WithGatedTx's closure).
//
// The gate is a buffer-1 channel with `defer func() { <-gate }()` and no
// timeout (internal/presencehistory/operations.go:182-198), so re-entry blocks
// forever -- with no deadlock detector to break it, because half the cycle is a
// Go channel rather than a database lock.
type Rail struct {
	db         *sql.DB
	gate       Gate
	reconciler *Reconciler
	log        *logger.Logger
}

// NewRail wires the seam. reconciler and log may be nil; db and gate are
// required by WithGatedTx and checked there.
func NewRail(db *sql.DB, gate Gate, reconciler *Reconciler, log *logger.Logger) *Rail {
	return &Rail{db: db, gate: gate, reconciler: reconciler, log: log}
}

// HasReconciler reports whether the rail can deliver. The boot guard asks this.
//
// It interrogates the TERMINAL, not the pointer: NewRail and NewReconciler both
// always return a non-nil value, so a nil check on the constructed value is a
// tautology that still boots with the wiring line deleted. HasTopologyRail
// (internal/graphpresence/topology.go:63-71) carries the same warning for the
// same reason.
func (r *Rail) HasReconciler() bool {
	return r != nil && r.reconciler.HasDeliverer()
}

// WithGatedTx acquires every named subject's gate -- deduped and in sorted
// order, so two overlapping captures cannot self-deadlock -- and only then opens
// the transaction. Gates before BeginTx is not a style preference: acquiring a
// gate while holding row locks inverts the order every other writer uses.
//
// work owns the commit. A work that returns without committing is rolled back.
func (r *Rail) WithGatedTx(
	ctx context.Context,
	subjects []uuid.UUID,
	work func(tx *sql.Tx) error,
) error {
	if r == nil || r.db == nil || r.gate == nil {
		return ErrRailNotWired
	}
	if len(subjects) > maxActiveSubjects {
		return fmt.Errorf("%w: %d", ErrTooManySubjects, len(subjects))
	}
	ordered := canonicalSubjects(subjects)

	return r.gate.WithSenders(ctx, ordered, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin gated active-presence mutation: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		return work(tx)
	})
}

// canonicalSubjects sorts and dedupes by string form, matching
// presencehistory's canonicalTopologySenders (operations.go:349-367). Two
// concurrent captures with overlapping subject sets must agree on acquisition
// order, and uuid.Nil is not a subject.
func canonicalSubjects(subjects []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(subjects))
	ordered := make([]uuid.UUID, 0, len(subjects))
	for _, subject := range subjects {
		if subject == uuid.Nil {
			continue
		}
		if _, exists := seen[subject]; exists {
			continue
		}
		seen[subject] = struct{}{}
		ordered = append(ordered, subject)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	return ordered
}

// CapturePlansTx writes every obligation inside the caller's transaction. It
// must run BEFORE the first evidence-destroying DELETE.
func (r *Rail) CapturePlansTx(ctx context.Context, tx *sql.Tx, plans []Plan) error {
	if len(plans) > maxActiveSubjects {
		return fmt.Errorf("%w: %d", ErrTooManySubjects, len(plans))
	}
	for _, plan := range plans {
		if err := InsertPlanTx(ctx, tx, plan); err != nil {
			return err
		}
	}
	return nil
}

// CompleteAlreadyGated resolves and delivers the just-committed plans without
// re-entering the gate. Call it AFTER the caller's commit, from inside the same
// WithGatedTx closure.
//
// The tx parameter is deliberately unused: the caller has already committed, and
// accepting it keeps the call site's ownership obvious. Every key is attempted
// and every failure is joined -- returning on the first would leave a later
// subject's undelivered clear invisible.
//
// Each key's failure is wrapped in ErrDeliveryIncomplete INDIVIDUALLY rather
// than the join being wrapped once: wrapping the join would add a second
// element to its Unwrap() slice, so a caller counting reported failures would
// see two for one failing key and the count would stop meaning anything.
func (r *Rail) CompleteAlreadyGated(ctx context.Context, _ *sql.Tx, keys []PlanKey) error {
	if r == nil || r.reconciler == nil {
		return ErrRailNotWired
	}
	// DETACHED. By the time this runs the caller's mutation has COMMITTED: the
	// conversation is gone and the evidence a resolver would need to rebuild
	// the obligation was destroyed by that same commit. A client that
	// disconnects or times out mid-request must not take the retraction with
	// it. deliver() detaches internally, but the claim, resolve and
	// acknowledge steps ahead of it ran on the request context -- so a late
	// cancellation lost the synchronous fast path and left the row for the
	// 5-second reconciler. Correct but slower, and it contradicted the
	// acceptance criterion; detaching here makes the whole completion match it.
	// WithoutCancel strips the DEADLINE as well as cancellation. That is the
	// point for cancellation and a hazard for the deadline: without a
	// replacement bound the claim and resolve steps would run unbounded while
	// the caller holds its sender gate. resolveOneAlreadyGated therefore
	// applies claimTimeout to its own transactional segment -- see the const
	// for why that bound is longer than deliveryTimeout rather than a sum of
	// the others. Found by review after the first version of this detachment.
	ctx = context.WithoutCancel(ctx)

	pass := &passState{}
	var deliveryErr error
	for _, key := range keys {
		if err := r.reconciler.resolveOneAlreadyGated(ctx, key, pass); err != nil {
			deliveryErr = errors.Join(deliveryErr,
				fmt.Errorf("%w: %w", ErrDeliveryIncomplete, err))
		}
	}
	return deliveryErr
}

// DrainAlreadyGated removes every obligation for one subject inside the
// caller's transaction and reports which categories it removed. The caller MUST
// transfer the obligation by calling ClearDrained after its commit.
func (r *Rail) DrainAlreadyGated(
	ctx context.Context,
	tx *sql.Tx,
	subjectID uuid.UUID,
) ([]presence.Category, error) {
	return DrainSubjectTx(ctx, tx, subjectID)
}

// ClearDrained is the post-commit half of the drain: one proportional clear
// frame per drained category. Best effort by design -- a presence delivery
// failure must never fail an account erasure.
func (r *Rail) ClearDrained(_ context.Context, subjectID uuid.UUID, categories []presence.Category) {
	if r == nil || r.reconciler == nil || r.reconciler.deliverer == nil {
		return
	}
	for _, category := range categories {
		r.reconciler.deliverer.ClearSenderActiveCategory(subjectID, category)
	}
}
