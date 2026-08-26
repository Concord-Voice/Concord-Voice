package activepresence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// fakeStateReader is a spy, not just a stub: calls is what proves a branch was
// reached THROUGH the read rather than short-circuited before it, and what
// proves the fail-closed branches are not dead code.
type fakeStateReader struct {
	state presence.ActivityState
	found bool
	err   error
	calls int
}

func (f *fakeStateReader) GetWithLease(
	context.Context, uuid.UUID, presence.Category,
) (presence.ActivityState, bool, error) {
	f.calls++
	return f.state, f.found, f.err
}

func TestResolveSixBranches(t *testing.T) {
	now := time.Now()
	liveToken := uuid.New()
	live := presence.ActivityState{SourceToken: liveToken, SourceVersion: now.UnixMicro()}
	successor := presence.ActivityState{SourceToken: uuid.New(), SourceVersion: now.UnixMicro() + 9}
	sameTokenSuccessor := presence.ActivityState{
		SourceToken:   liveToken,
		SourceVersion: now.Add(time.Minute).UnixMicro(),
	}

	cases := []struct {
		name      string
		reader    fakeStateReader
		plan      Plan
		outcome   Outcome
		failure   FailureClass
		wantState presence.ActivityState
		wantReads int
	}{
		{
			name:   "B1 absent and past the level arm",
			reader: fakeStateReader{},
			plan: Plan{Resolution: ResolutionConservative,
				EventAt: now.Add(-2 * presence.ActivityStateTTL)},
			outcome:   OutcomeStateAbsent,
			failure:   FailureNone,
			wantReads: 1,
		},
		{
			name:      "B2 unexpiring",
			reader:    fakeStateReader{err: presence.ErrUnexpiringActivityState},
			plan:      Plan{Resolution: ResolutionConservative, EventAt: now},
			outcome:   OutcomeDisconnected,
			failure:   FailureStateUnexpiring,
			wantReads: 1,
		},
		{
			name:      "B3 malformed",
			reader:    fakeStateReader{err: presence.ErrMalformedActivityState},
			plan:      Plan{Resolution: ResolutionConservative, EventAt: now},
			outcome:   OutcomeDisconnected,
			failure:   FailureStateMalformed,
			wantReads: 1,
		},
		{
			name:   "B4 superseded by a later generation",
			reader: fakeStateReader{state: successor, found: true},
			plan: Plan{Resolution: ResolutionExact, LifecycleID: liveToken,
				EventAt: now, Attempts: 0},
			outcome: OutcomeSuperseded,
			failure: FailureNone,
			// A superseded plan hands the terminal NO generation: the only
			// generation in Redis belongs to the successor, and deleting it
			// would kill a live call.
			wantState: presence.ActivityState{},
			wantReads: 1,
		},
		{
			// The token arm alone decides this one: a DIFFERENT lifecycle whose
			// version does not postdate the plan's event time. It is reachable
			// because scope_event_at is ratcheted with GREATEST across a plan
			// collision, so it can outrun the version of a generation written
			// after it. Without this case the token arm is dead code that reads
			// correctly.
			name: "B4 superseded by a different lifecycle with an older version",
			reader: fakeStateReader{
				state: presence.ActivityState{
					SourceToken:   uuid.New(),
					SourceVersion: now.Add(-time.Minute).UnixMicro(),
				},
				found: true,
			},
			plan: Plan{Resolution: ResolutionExact, LifecycleID: liveToken,
				EventAt: now},
			outcome:   OutcomeSuperseded,
			failure:   FailureNone,
			wantState: presence.ActivityState{},
			wantReads: 1,
		},
		{
			name:   "B4 superseded within the same lifecycle by a later version",
			reader: fakeStateReader{state: sameTokenSuccessor, found: true},
			plan: Plan{Resolution: ResolutionExact, LifecycleID: liveToken,
				EventAt: now},
			outcome:   OutcomeSuperseded,
			failure:   FailureNone,
			wantState: presence.ActivityState{},
			wantReads: 1,
		},
		{
			name:      "B5 stale generation is still live",
			reader:    fakeStateReader{state: live, found: true},
			plan:      Plan{Resolution: ResolutionConservative, EventAt: now},
			outcome:   OutcomeCleared,
			failure:   FailureNone,
			wantState: live,
			wantReads: 1,
		},
		{
			name:   "B5 exact plan at its own generation",
			reader: fakeStateReader{state: live, found: true},
			plan: Plan{Resolution: ResolutionExact, LifecycleID: liveToken,
				EventAt: now},
			outcome:   OutcomeCleared,
			failure:   FailureNone,
			wantState: live,
			wantReads: 1,
		},
		{
			name:      "B5 absent but younger than the level arm",
			reader:    fakeStateReader{},
			plan:      Plan{Resolution: ResolutionConservative, EventAt: now},
			outcome:   OutcomeCleared,
			failure:   FailureNone,
			wantReads: 1,
		},
		{
			name:      "B6 read error",
			reader:    fakeStateReader{err: errors.New("redis is unreachable")},
			plan:      Plan{Resolution: ResolutionConservative, EventAt: now},
			outcome:   OutcomeRetained,
			failure:   FailureStateRead,
			wantReads: 1,
		},
		{
			name:   "B6 quarantine at the attempt bound",
			reader: fakeStateReader{err: errors.New("redis is unreachable")},
			plan: Plan{Resolution: ResolutionConservative, EventAt: now,
				Attempts: maxPlanAttempts - 1},
			outcome:   OutcomeQuarantined,
			failure:   FailureStateRead,
			wantReads: 1,
		},
		{
			name:   "B6 plan violating its own invariants",
			reader: fakeStateReader{state: live, found: true},
			plan: Plan{Resolution: ResolutionExact, LifecycleID: uuid.Nil,
				EventAt: now},
			outcome: OutcomeRetained,
			failure: FailurePlanInvalid,
			// An inconsistent plan must never reach Redis at all.
			wantReads: 0,
		},
		{
			name:   "B6 plan naming an unknown category",
			reader: fakeStateReader{state: live, found: true},
			plan: Plan{Resolution: ResolutionConservative, EventAt: now,
				Category: presence.Category("screen_share")},
			outcome:   OutcomeRetained,
			failure:   FailurePlanInvalid,
			wantReads: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := tc.plan
			plan.SubjectID = uuid.New()
			plan.OperationID = uuid.New()
			if plan.Category == "" {
				plan.Category = presence.CategoryPrivateCall
			}
			reader := tc.reader

			decision := Resolve(context.Background(), &reader, plan, now)

			require.Equal(t, tc.outcome, decision.Outcome)
			require.Equal(t, tc.failure, decision.Failure)
			require.Equal(t, tc.wantState, decision.State,
				"the terminal may only act on a generation the resolver proved")
			require.Equal(t, tc.wantReads, reader.calls,
				"branch reachability: the read must happen exactly where the branch table says")
		})
	}
}

// A missing reader is a wiring bug, and a wiring bug must not ack an
// obligation. It fails closed onto the retained arm.
func TestResolveWithoutReaderRetains(t *testing.T) {
	plan := Plan{
		SubjectID: uuid.New(), OperationID: uuid.New(),
		Category:   presence.CategoryPrivateCall,
		Resolution: ResolutionConservative, EventAt: time.Now(),
	}

	decision := Resolve(context.Background(), nil, plan, time.Now())

	require.Equal(t, OutcomeRetained, decision.Outcome)
	require.Equal(t, FailureStateRead, decision.Failure)
	require.Equal(t, presence.ActivityState{}, decision.State)
}

// exact may degrade to conservative; the reverse must be impossible. The schema
// CHECK catches the persisted form, and this catches an in-memory upgrade before
// it is ever written.
//
// The observable form of that invariant inside the resolver is the superseded
// branch: it is the one decision that requires lifecycle evidence, so a
// conservative plan -- which by construction has none -- must never reach it.
// Taking it would silently ack a privacy obligation nothing had discharged.
func TestResolveNeverUpgradesConservativeToExact(t *testing.T) {
	plan := Plan{
		SubjectID: uuid.New(), OperationID: uuid.New(),
		Category:   presence.CategoryPrivateCall,
		Resolution: ResolutionConservative, EventAt: time.Now(),
	}
	reader := &fakeStateReader{
		state: presence.ActivityState{SourceToken: uuid.New(), SourceVersion: 3},
		found: true,
	}

	decision := Resolve(context.Background(), reader, plan, time.Now())

	require.Equal(t, OutcomeCleared, decision.Outcome,
		"a conservative plan cannot prove supersession, so it must take the clear arm")
	require.Equal(t, ResolutionConservative, plan.Resolution,
		"Resolve must not mutate the plan's resolution")
}

// Zero Redis writes and zero delivery on the superseded branch. The resolver
// cannot write -- StateReader exposes only a read -- so what has to be asserted
// is the ABSENCE of the input the terminal would write with: a superseded
// decision carries no generation, so Task 7 has nothing to CompareAndDelete and
// HasGeneration gates the terminal shut.
func TestResolveSupersededHandsTheTerminalNothing(t *testing.T) {
	planned := uuid.New()
	eventAt := time.Now().Add(-time.Hour)
	reader := &fakeStateReader{
		state: presence.ActivityState{
			SourceToken:   uuid.New(),
			SourceVersion: time.Now().UnixMicro(),
		},
		found: true,
	}
	plan := Plan{
		SubjectID: uuid.New(), OperationID: uuid.New(),
		Category:    presence.CategoryPrivateCall,
		Resolution:  ResolutionExact,
		LifecycleID: planned,
		EventAt:     eventAt,
	}

	decision := Resolve(context.Background(), reader, plan, time.Now())

	require.Equal(t, OutcomeSuperseded, decision.Outcome)
	require.Equal(t, presence.ActivityState{}, decision.State)
	require.False(t, decision.HasGeneration(),
		"a superseded decision must never authorize a generation delete")
	require.Equal(t, 1, reader.calls, "exactly one read, and no second pass")
}

// The clear arm splits: a live generation authorizes the exact delete, while an
// absent-but-young state authorizes only the frame. Handing a zero generation to
// CompareAndDelete would be a malformed-key error on every young-absent plan.
func TestDecisionHasGeneration(t *testing.T) {
	live := Decision{
		Outcome: OutcomeCleared,
		State: presence.ActivityState{
			SourceToken: uuid.New(), SourceVersion: time.Now().UnixMicro(),
		},
	}
	require.True(t, live.HasGeneration())

	require.False(t, Decision{Outcome: OutcomeCleared}.HasGeneration(),
		"absent-but-young carries no generation")
	require.False(t, Decision{
		Outcome: OutcomeCleared,
		State:   presence.ActivityState{SourceToken: uuid.New()},
	}.HasGeneration(), "a zero version is not a generation")
	require.False(t, Decision{
		Outcome: OutcomeDisconnected,
		State: presence.ActivityState{
			SourceToken: uuid.New(), SourceVersion: time.Now().UnixMicro(),
		},
	}.HasGeneration(), "only the clear arm deletes a generation")
}
