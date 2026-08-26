package activepresence

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// A CONSERVATIVE plan must not retract a generation published after it was cut.
//
// This is the red-team finding from #2448's pre-PR pass, and it was reachable by
// an ordinary group-DM admin. The only production producer, dm.captureGroupPlans,
// emits conservative plans with NO LifecycleID -- correctly, because the
// conversation and its voice rows die in the capturing transaction, so no
// resolver can ever prove an exact generation for that leg. But isSuccessor
// short-circuited on the nil LifecycleID BEFORE consulting the watermark, so the
// successor guard was dead code for every plan that actually exists in
// production.
//
// The consequence was not a missed clear. It was the opposite: a plan deferred
// to the reconciler (any transient delivery failure defers it by >= 5s, and
// quarantine backoff stretches that to minutes) would clear whatever generation
// it found on its next pass -- including a LIVE call the subject had joined in
// the meantime. CompareAndDelete succeeded, because it is handed the
// successor's own token and version, and the obligation was then acked, so
// nothing recorded what had been destroyed.
//
// Failure direction matters here. Under-clearing degrades to the 90s
// ActivityStateTTL, which is the pre-#2448 behaviour. Over-clearing kills a live
// call's presence for every viewer. The watermark is what keeps the failure on
// the safe side.
func TestConservativePlanDoesNotRetractALiveSuccessor(t *testing.T) {
	cutAt := time.Now()
	subject := uuid.New()

	// Exactly what dm.captureGroupPlans produces: conservative, no LifecycleID.
	plan := Plan{
		SubjectID:   subject,
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		Resolution:  ResolutionConservative,
		EventAt:     cutAt,
	}

	// A call the subject joined AFTER the plan was cut. Unrelated to the deleted
	// conversation; its generation simply postdates the plan.
	successor := presence.ActivityState{
		SourceToken:   uuid.New(),
		SourceVersion: cutAt.Add(30 * time.Second).UnixMicro(),
	}

	reader := &fakeStateReader{state: successor, found: true}
	decision := Resolve(context.Background(), reader, plan, cutAt.Add(time.Minute))

	require.Equal(t, OutcomeSuperseded, decision.Outcome,
		"a live generation published after the plan was cut must be left alone")
	require.False(t, decision.HasGeneration(),
		"a superseded decision must hand the terminal nothing to delete")
	require.Equal(t, presence.ActivityState{}, decision.State,
		"the successor's token and version must not reach CompareAndDelete")
}

// The control: the generation the plan was actually cut against is still
// cleared. Without this, the fix above could be satisfied by never clearing
// anything, which would silently restore the pre-#2448 bug.
func TestConservativePlanStillClearsTheGenerationItWasCutAgainst(t *testing.T) {
	cutAt := time.Now()
	subject := uuid.New()

	plan := Plan{
		SubjectID:   subject,
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		Resolution:  ResolutionConservative,
		EventAt:     cutAt,
	}

	// State published BEFORE the plan was cut: the stale call the deletion killed.
	stale := presence.ActivityState{
		SourceToken:   uuid.New(),
		SourceVersion: cutAt.Add(-30 * time.Second).UnixMicro(),
	}

	reader := &fakeStateReader{state: stale, found: true}
	decision := Resolve(context.Background(), reader, plan, cutAt.Add(time.Minute))

	require.Equal(t, OutcomeCleared, decision.Outcome,
		"the stale generation the plan was cut against must still be retracted")
}
