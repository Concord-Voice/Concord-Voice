// Package activepresence carries the durable reconciliation rail for the active
// Rich Presence categories (Server Voice, Private Call).
//
// It is a SIBLING of internal/presencehistory, not an extension of it. The two
// rails share exactly one thing -- the process-local sender gate array, reached
// through presencehistory.Service.WithSenders -- and share no relation, because
// presence_settings_pending_operations is PRIMARY KEY (user_id) and its
// reconciler discriminates operation kinds by a version relation that a third
// kind would be indistinguishable from.
package activepresence

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// Bounds are consts, never configuration. A deployment that needs a different
// value has a design problem, and a config surface would invite raising the
// bound instead of fixing the derivation.
const (
	// maxActiveSubjects bounds the subjects one capture may name. A group DM
	// caps at 10 participants; 16 tolerates the non-transactional TOCTOU in
	// AddMember, which counts with a bare SELECT COUNT(*) and inserts with no
	// row lock, so concurrency can produce an 11-member group. A bound of
	// exactly 10 would make such a group permanently undeletable. Exceeding 16
	// is a derivation bug and fails CLOSED.
	maxActiveSubjects = 16

	// maxPlanBatch matches presencehistory's maxReconcileBatch.
	maxPlanBatch = 1000

	// maxPlanAttempts is the quarantine threshold. A quarantined plan is never
	// deleted -- dropping it fails open.
	maxPlanAttempts = 5

	reconcileInterval  = 5 * time.Second
	quarantineInterval = 10 * time.Minute
)

// ErrInvalidPlan marks a plan that violates its own invariants. It fails closed.
var ErrInvalidPlan = errors.New("activepresence: plan is not internally consistent")

// Resolution is a CLOSED enum. It degrades exact -> conservative and never the
// reverse; the reverse is also unrepresentable in the schema.
type Resolution uint8

// Supported plan resolutions.
const (
	ResolutionExact Resolution = iota
	ResolutionConservative
)

// String fails CLOSED: an unknown value renders as the stronger arm, so a
// corrupted value can never claim exactness it cannot prove.
func (r Resolution) String() string {
	if r == ResolutionExact {
		return "exact"
	}
	return "conservative"
}

// Outcome is a CLOSED enum naming how one plan resolved. It is what the
// aggregate log line reports and what the tests assert on.
type Outcome uint8

// Supported plan outcomes.
const (
	OutcomeStateAbsent Outcome = iota
	OutcomeSuperseded
	OutcomeCleared
	OutcomeDisconnected
	OutcomeRetained
	OutcomeQuarantined
)

// String fails CLOSED: an unknown value renders as the retained arm, so a
// corrupted value can never report a terminal outcome it did not reach.
func (o Outcome) String() string {
	switch o {
	case OutcomeStateAbsent:
		return "state_absent"
	case OutcomeSuperseded:
		return "superseded"
	case OutcomeCleared:
		return "cleared"
	case OutcomeDisconnected:
		return "disconnected"
	case OutcomeQuarantined:
		return "quarantined"
	default:
		return "retained"
	}
}

// FailureClass is a FIXED enum mirroring the CHECK in migration 000111. It is
// never a wrapped database error: an untyped string field invites a caller to
// put an interpolated error -- and therefore data -- into a log line.
type FailureClass uint8

// Supported failure classes.
const (
	FailureNone FailureClass = iota
	FailureStateRead
	FailureStateUnexpiring
	FailureStateMalformed
	FailureGenerationDelete
	FailureDelivery
	FailurePlanInvalid
)

// String fails CLOSED: an unknown value renders as plan_invalid, the class that
// routes to the conservative arm.
func (f FailureClass) String() string {
	switch f {
	case FailureStateRead:
		return "state_read"
	case FailureStateUnexpiring:
		return "state_unexpiring"
	case FailureStateMalformed:
		return "state_malformed"
	case FailureGenerationDelete:
		return "generation_delete"
	case FailureDelivery:
		return "delivery"
	default:
		return "plan_invalid"
	}
}

// Plan is the durable obligation in memory.
//
// It holds NO payload and NO recipient set. The terminal needs neither, and
// holding either would put an audience inside a package that must not have one.
type Plan struct {
	SubjectID   uuid.UUID
	Category    presence.Category
	OperationID uuid.UUID
	Resolution  Resolution
	// LifecycleID is uuid.Nil if and only if Resolution is
	// ResolutionConservative.
	LifecycleID uuid.UUID
	EventAt     time.Time
	Attempts    int
}

// Validate reports whether the plan is internally consistent. Every failure
// wraps ErrInvalidPlan so a caller can route it to the conservative arm.
func (p Plan) Validate() error {
	if p.SubjectID == uuid.Nil {
		return fmt.Errorf("%w: subject is required", ErrInvalidPlan)
	}
	if p.OperationID == uuid.Nil {
		return fmt.Errorf("%w: operation is required", ErrInvalidPlan)
	}
	if p.EventAt.IsZero() {
		return fmt.Errorf("%w: event time is required", ErrInvalidPlan)
	}
	if p.Attempts < 0 {
		return fmt.Errorf("%w: attempts cannot be negative", ErrInvalidPlan)
	}
	switch p.Category {
	case presence.CategoryServerVoice, presence.CategoryPrivateCall:
	default:
		return fmt.Errorf("%w: unknown category", ErrInvalidPlan)
	}
	// Reject an out-of-range Resolution explicitly. Resolution.String() fails
	// CLOSED to "conservative", which is right for a log line but wrong here:
	// an unvalidated Resolution(99) would be PERSISTED as a conservative plan,
	// so a producer defect would silently manufacture a clear obligation
	// instead of returning ErrInvalidPlan. The category switch above already
	// works this way; the resolution check did not.
	switch p.Resolution {
	case ResolutionExact, ResolutionConservative:
	default:
		return fmt.Errorf("%w: unknown resolution", ErrInvalidPlan)
	}
	if p.Resolution == ResolutionExact && p.LifecycleID == uuid.Nil {
		return fmt.Errorf("%w: exact resolution requires lifecycle evidence", ErrInvalidPlan)
	}
	return nil
}

// ParseCategory maps a stored category string onto the closed vocabulary. An
// unrecognized value is an error, never a silently accepted third category.
func ParseCategory(value string) (presence.Category, error) {
	switch presence.Category(value) {
	case presence.CategoryServerVoice:
		return presence.CategoryServerVoice, nil
	case presence.CategoryPrivateCall:
		return presence.CategoryPrivateCall, nil
	default:
		return "", fmt.Errorf("%w: unknown category", ErrInvalidPlan)
	}
}

// ParseResolution maps a stored resolution string onto the closed vocabulary.
// An unrecognized value returns the conservative arm alongside its error, so a
// caller that ignores the error still degrades in the safe direction.
func ParseResolution(value string) (Resolution, error) {
	switch value {
	case "exact":
		return ResolutionExact, nil
	case "conservative":
		return ResolutionConservative, nil
	default:
		return ResolutionConservative, fmt.Errorf("%w: unknown resolution", ErrInvalidPlan)
	}
}
