package activepresence

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// StateReader is the narrow slice of ActivityStore the resolver needs. Narrow so
// the branch table can be driven by a fake without a live Redis -- and narrow so
// the resolver structurally CANNOT write: it decides, and the reconciler acts.
type StateReader interface {
	GetWithLease(context.Context, uuid.UUID, presence.Category) (presence.ActivityState, bool, error)
}

// Decision is one plan's resolution. State carries the generation the terminal
// must delete, and is only meaningful when HasGeneration reports true.
type Decision struct {
	Outcome Outcome
	Failure FailureClass
	State   presence.ActivityState
}

// HasGeneration reports whether the decision authorizes an exact generation
// delete. Only the clear arm ever does: the superseded arm deliberately carries
// no generation (the one in Redis belongs to a LIVE successor), and the
// absent-but-young arm has none to carry.
//
// Callers gate ActivityStore.CompareAndDelete on this. Passing a zero generation
// to it is not a harmless no-op -- activityGenerationKey rejects it, so every
// young-absent plan would report a spurious generation_delete failure.
func (d Decision) HasGeneration() bool {
	return d.Outcome == OutcomeCleared &&
		d.State.SourceToken != uuid.Nil &&
		d.State.SourceVersion > 0
}

// Resolve runs the six-branch decision procedure. It performs no SQL and no
// delivery; it decides.
//
// Every uncertain branch fails CLOSED toward a terminal. Resolution may degrade
// exact -> conservative and never the reverse: Resolve takes p BY VALUE and
// never writes to it, and the one branch that requires exactness -- supersession
// -- is unreachable without lifecycle evidence.
func Resolve(ctx context.Context, reader StateReader, p Plan, now time.Time) Decision {
	// Validate BEFORE the read. An inconsistent plan must not reach Redis at
	// all: its category is what names the key, and a plan claiming exactness it
	// cannot prove must never get far enough to act on what it finds.
	if err := p.Validate(); err != nil {
		return Decision{Outcome: retainOrQuarantine(p), Failure: FailurePlanInvalid}
	}
	if reader == nil {
		return Decision{Outcome: retainOrQuarantine(p), Failure: FailureStateRead}
	}

	state, found, err := reader.GetWithLease(ctx, p.SubjectID, p.Category)
	switch {
	// B2. The key exists with no expiry, so the 90-second level arm does not
	// apply and reading it as "it will expire" is the failure.
	case errors.Is(err, presence.ErrUnexpiringActivityState):
		return Decision{Outcome: OutcomeDisconnected, Failure: FailureStateUnexpiring}
	// B3. The generation is unreadable, so exactness is unprovable.
	case errors.Is(err, presence.ErrMalformedActivityState):
		return Decision{Outcome: OutcomeDisconnected, Failure: FailureStateMalformed}
	// B6. Any other read failure is retained, never acked.
	case err != nil:
		return Decision{Outcome: retainOrQuarantine(p), Failure: FailureStateRead}
	}

	if !found {
		// B1. Absent AND past the level arm: every viewer's copy has expired.
		if now.Sub(p.EventAt) >= presence.ActivityStateTTL {
			return Decision{Outcome: OutcomeStateAbsent}
		}
		// B5. Absent but younger than the arm proves nothing about viewers.
		return Decision{Outcome: OutcomeCleared}
	}

	// B4. A strictly later generation has already overwritten what viewers
	// cached, so the obligation is moot -- and clearing it would kill a live
	// call. Generation uncertainty resolves HERE, never toward a wider clear.
	if isSuccessor(state, p) {
		return Decision{Outcome: OutcomeSuperseded}
	}

	// B5. The stale generation is still live.
	return Decision{Outcome: OutcomeCleared, State: state}
}

// isSuccessor reports whether the live generation postdates the plan's,
// mirroring ActivityService.hasGenerationSuccessor
// (internal/presence/activity_service.go:886-911).
//
// A plan with no lifecycle evidence (the conservative arm) can prove nothing, so
// it never takes this branch: that is the in-memory half of the one-way
// exact -> conservative degrade.
func isSuccessor(state presence.ActivityState, p Plan) bool {
	// The WATERMARK bounds every plan, exact or conservative. A generation
	// published after the plan was cut postdates it whatever the resolution,
	// and this test must come FIRST.
	//
	// Red-team finding, #2448 pre-PR pass: the only production producer
	// (dm.captureGroupPlans) emits conservative plans with NO LifecycleID,
	// because the conversation and its voice rows all die in the capturing
	// transaction and no resolver can ever prove an exact generation for that
	// leg. With the nil check first, isSuccessor returned false for EVERY
	// production plan -- the successor guard below was dead code, and a
	// deferred plan would clear whatever generation it later found, including
	// a LIVE unrelated call the subject had since joined. The CompareAndDelete
	// even succeeded, because it is handed the successor's own token and
	// version.
	//
	// This is also what migration 000111's own column comment promises:
	// "scope_event_at -- the resolver refuses an exact clear when live state is
	// newer than this." Ordering the checks this way is what makes that true.
	if state.SourceVersion > p.EventAt.UnixMicro() {
		return true
	}
	if p.LifecycleID == uuid.Nil {
		return false
	}
	// A different lifecycle entirely -- a new call, or a different voice room.
	//
	// The same-lifecycle-at-a-later-version case needs no separate test: it is
	// already caught by the watermark check above, because SourceVersion IS a
	// microsecond timestamp. An earlier revision repeated that comparison here
	// and the line was unreachable -- every path reaching it has established
	// SourceVersion <= watermark, so it could only ever return false. Found by
	// review; the misleading comment was the real hazard, since a future exact
	// producer would have trusted a branch that never ran.
	return state.SourceToken != p.LifecycleID
}

// retainOrQuarantine picks the non-terminal arm. Quarantine is a SLOW RETRY,
// never a delete: the row is the evidence that an obligation is undischarged,
// and dropping it fails open.
func retainOrQuarantine(p Plan) Outcome {
	if p.Attempts+1 >= maxPlanAttempts {
		return OutcomeQuarantined
	}
	return OutcomeRetained
}
