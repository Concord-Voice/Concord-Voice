package graphpresence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type fakeRefresher struct {
	calls int
	err   error
}

func (f *fakeRefresher) RefreshServerVoiceRecheck(
	_ context.Context, _ uuid.UUID, _ presence.Scope, _ map[uuid.UUID]bool,
) error {
	f.calls++
	return f.err
}

type fakeDisconnector struct {
	recipients map[uuid.UUID]bool
	allCalls   int
}

func (f *fakeDisconnector) DisconnectRichPresenceClients(
	_ context.Context, recipients map[uuid.UUID]bool,
) error {
	if f.recipients == nil {
		f.recipients = make(map[uuid.UUID]bool)
	}
	for id := range recipients {
		f.recipients[id] = true
	}
	return nil
}

func (f *fakeDisconnector) DisconnectAllRichPresenceClients(_ context.Context) error {
	f.allCalls++
	return nil
}

// BeginAudienceRevocation satisfies Disconnector. This fake exists to observe
// DISCONNECTS, and the #2992 bracket is orthogonal to every assertion it
// carries, so the closer is inert. The fence's own behaviour is covered by
// topology_test.go's fenceStub, which records into the ordering trace.
func (f *fakeDisconnector) BeginAudienceRevocation() func() { return func() {} }

func TestAbandonDisconnectsCapturedAudience(t *testing.T) {
	d := &fakeDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	v1, v2 := uuid.New(), uuid.New()
	plan := &Plan{
		active:  []activeLeg{{senderID: uuid.New(), captured: map[uuid.UUID]bool{v1: true}}},
		viewers: map[uuid.UUID]bool{v2: true},
	}
	r.Abandon(plan, "test")

	assert.Equal(t, map[uuid.UUID]bool{v1: true, v2: true}, d.recipients,
		"Abandon must disconnect the whole captured audience")
}

func TestAbandonToleratesNilPlan(t *testing.T) {
	d := &fakeDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	r.Abandon(nil, "test") // must not panic

	assert.Empty(t, d.recipients, "a nil plan must disconnect nothing")
}

func TestDegradeBuildsPrincipalSupersetWithFixedCause(t *testing.T) {
	r := New(nil, &fakeRefresher{}, &fakeDisconnector{}, nil, nil)
	defer r.Close()

	a, b := uuid.New(), uuid.New()
	subject := presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	}
	plan := r.degradePlan(subject, causeAudienceRead)

	assert.True(t, plan.Degraded(), "degradePlan must produce a degraded plan")
	assert.Equal(t, causeAudienceRead, plan.cause, "the fixed-enum cause must be carried through")
	assert.Equal(t, map[uuid.UUID]bool{a: true, b: true}, plan.viewers,
		"a degraded plan's viewers are the two principals")
	assert.Empty(t, plan.active, "a degraded plan must carry no exact legs")
}

// uuid.Nil is not a user. A family with no counterpart must not disconnect it.
func TestDegradeOmitsNilCounterpart(t *testing.T) {
	r := New(nil, &fakeRefresher{}, &fakeDisconnector{}, nil, nil)
	defer r.Close()

	a := uuid.New()
	plan := r.degradePlan(presencecapture.Subject{
		Family:      presencecapture.FamilyFriendsOfFriendsToggle,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
	}, causeAudienceRead)

	assert.Equal(t, map[uuid.UUID]bool{a: true}, plan.viewers,
		"a nil counterpart is not a user and must not be captured")
}

// The bound fails CLOSED even under the degrade posture: exceeding it means the
// focal-set derivation is wrong, which is a bug rather than load.
func TestBoundExceededFailsClosedUnderDegradePosture(t *testing.T) {
	r := New(nil, &fakeRefresher{}, &fakeDisconnector{}, nil, nil)
	defer r.Close()

	focal := make([]uuid.UUID, maxFocalSenders+1)
	for i := range focal {
		focal[i] = uuid.New()
	}
	err := r.checkFocalBound(focal)
	require.ErrorIs(t, err, presencecapture.ErrCaptureBound,
		"an oversized focal set must classify as ErrCaptureBound")
}

// The happy half of the same bound: a focal set at the limit is admitted.
func TestFocalBoundAdmitsSetAtTheLimit(t *testing.T) {
	r := New(nil, &fakeRefresher{}, &fakeDisconnector{}, nil, nil)
	defer r.Close()

	focal := make([]uuid.UUID, maxFocalSenders)
	for i := range focal {
		focal[i] = uuid.New()
	}
	assert.NoError(t, r.checkFocalBound(focal), "a focal set at the bound must be admitted")
}

func TestCompleteRejectsNilTx(t *testing.T) {
	r := New(nil, &fakeRefresher{}, &fakeDisconnector{}, nil, nil)
	defer r.Close()

	require.Error(t, r.Complete(context.Background(), nil, &Plan{}),
		"Complete with a nil tx must error, not silently succeed")
}

// The exact path: every active leg is refreshed, and the peripheral viewer set
// is cleared afterwards.
func TestDispatchRefreshesEveryLegThenClearsPeripheralViewers(t *testing.T) {
	refresher := &fakeRefresher{}
	d := &fakeDisconnector{}
	r := New(nil, refresher, d, nil, nil)
	defer r.Close()

	captured, peripheral := uuid.New(), uuid.New()
	r.dispatch(&Plan{
		active: []activeLeg{{
			senderID: uuid.New(),
			scope:    presence.Scope{Category: presence.CategoryServerVoice},
			captured: map[uuid.UUID]bool{captured: true},
		}},
		viewers: map[uuid.UUID]bool{peripheral: true},
	})

	assert.Equal(t, 1, refresher.calls, "every active leg must be refreshed exactly once")
	assert.Equal(t, map[uuid.UUID]bool{peripheral: true}, d.recipients,
		"the exact path clears only the peripheral viewers; the leg audience is reconciled")
}

// A failed refresh leaves reconciliation unresolved, so the whole captured
// audience is disconnected instead.
func TestDispatchFailsClosedWhenRefreshFails(t *testing.T) {
	refresher := &fakeRefresher{err: errors.New("refresh unavailable")}
	d := &fakeDisconnector{}
	r := New(nil, refresher, d, nil, nil)
	defer r.Close()

	captured, peripheral := uuid.New(), uuid.New()
	r.dispatch(&Plan{
		active: []activeLeg{{
			senderID: uuid.New(),
			captured: map[uuid.UUID]bool{captured: true},
		}},
		viewers: map[uuid.UUID]bool{peripheral: true},
	})

	assert.Equal(t, map[uuid.UUID]bool{captured: true, peripheral: true}, d.recipients,
		"a failed refresh must fall back to disconnecting the captured audience")
}

// A degraded plan has no exact legs, so dispatch resolves it to the
// conservative disconnect its viewer set already describes.
func TestDispatchDegradedPlanDisconnectsConservativeSuperset(t *testing.T) {
	refresher := &fakeRefresher{}
	d := &fakeDisconnector{}
	r := New(nil, refresher, d, nil, nil)
	defer r.Close()

	a, b := uuid.New(), uuid.New()
	r.dispatch(r.degradePlan(presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	}, causeAudienceRead))

	assert.Zero(t, refresher.calls, "a degraded plan has no exact leg to refresh")
	assert.Equal(t, map[uuid.UUID]bool{a: true, b: true}, d.recipients,
		"a degraded plan disconnects its conservative superset")
}

// ─── PR #2738 review: MAJOR-row regressions ───────────────────────────────────

// perLegRefresher fails a chosen sender and succeeds for every other.
type perLegRefresher struct {
	failFor uuid.UUID
	err     error
	seen    []uuid.UUID
}

func (f *perLegRefresher) RefreshServerVoiceRecheck(
	_ context.Context, senderID uuid.UUID, _ presence.Scope, _ map[uuid.UUID]bool,
) error {
	f.seen = append(f.seen, senderID)
	if senderID == f.failFor {
		return f.err
	}
	return nil
}

// failingDisconnector fails the targeted disconnect so the escalation path runs.
type failingDisconnector struct {
	targeted int
	global   int
}

func (f *failingDisconnector) DisconnectRichPresenceClients(
	_ context.Context, _ map[uuid.UUID]bool,
) error {
	f.targeted++
	return errors.New("hub unavailable")
}

func (f *failingDisconnector) DisconnectAllRichPresenceClients(_ context.Context) error {
	f.global++
	return nil
}

// BeginAudienceRevocation satisfies Disconnector. This fake exists to observe
// DISCONNECTS, and the #2992 bracket is orthogonal to every assertion it
// carries, so the closer is inert. The fence's own behaviour is covered by
// topology_test.go's fenceStub, which records into the ordering trace.
func (f *failingDisconnector) BeginAudienceRevocation() func() { return func() {} }

// ErrRecheckSenderNotCurrent is a documented BENIGN per-sender terminal: the
// caller disconnects only that sender's captured viewers. Aborting the loop and
// tearing down the union punished the healthy sender for the other's routine
// channel-leave, which happens on ordinary requests because capture is at write
// time and dispatch is post-commit.
func TestDispatchDisconnectsOnlyTheFailingLegAndKeepsGoing(t *testing.T) {
	senderA, senderB := uuid.New(), uuid.New()
	viewerA, viewerB := uuid.New(), uuid.New()

	refresher := &perLegRefresher{
		failFor: senderA,
		err:     fmt.Errorf("%w: left the channel", presence.ErrRecheckSenderNotCurrent),
	}
	disc := &fakeDisconnector{}
	r := New(nil, refresher, disc, nil, nil)
	defer r.Close()

	r.dispatch(&Plan{active: []activeLeg{
		{senderID: senderA, captured: map[uuid.UUID]bool{viewerA: true}},
		{senderID: senderB, captured: map[uuid.UUID]bool{viewerB: true}},
	}})

	assert.Equal(t, []uuid.UUID{senderA, senderB}, refresher.seen,
		"a benign per-sender terminal must not skip the remaining sender's refresh")
	assert.True(t, disc.recipients[viewerA], "the failing leg's viewers must be disconnected")
	assert.False(t, disc.recipients[viewerB],
		"the healthy leg's viewers must NOT be disconnected — its refresh succeeded")
}

// A non-benign refresh error is still leg-scoped: unresolved for that sender,
// but the other principal's reconciliation must still run.
func TestDispatchKeepsReconcilingAfterANonBenignLegError(t *testing.T) {
	senderA, senderB := uuid.New(), uuid.New()
	refresher := &perLegRefresher{failFor: senderA, err: errors.New("redis down")}
	disc := &fakeDisconnector{}
	r := New(nil, refresher, disc, nil, nil)
	defer r.Close()

	r.dispatch(&Plan{active: []activeLeg{
		{senderID: senderA, captured: map[uuid.UUID]bool{uuid.New(): true}},
		{senderID: senderB, captured: map[uuid.UUID]bool{uuid.New(): true}},
	}})

	assert.Len(t, refresher.seen, 2, "one leg's failure must not abort the loop")
}

// The fail-closed terminal must not fail OPEN. voicepresence escalates to a
// global disconnect when the targeted one fails; this logged and gave up, so
// the recipients kept holding presence the committed graph no longer authorizes
// and DisconnectAllRichPresenceClients sat on the interface, never called.
func TestDisconnectEscalatesToGlobalWhenTargetedFails(t *testing.T) {
	disc := &failingDisconnector{}
	r := New(nil, &fakeRefresher{}, disc, nil, nil)
	defer r.Close()

	r.disconnect(map[uuid.UUID]bool{uuid.New(): true}, "test")

	assert.Equal(t, 1, disc.targeted, "the targeted disconnect is attempted first")
	assert.Equal(t, 1, disc.global,
		"a failed targeted disconnect must escalate — otherwise the fail-closed terminal fails open")
}

// An empty recipient set must not reach the hub at all.
func TestDisconnectSkipsAnEmptyRecipientSet(t *testing.T) {
	disc := &failingDisconnector{}
	r := New(nil, &fakeRefresher{}, disc, nil, nil)
	defer r.Close()

	r.disconnect(nil, "test")

	assert.Zero(t, disc.targeted)
	assert.Zero(t, disc.global)
}

// ─── fail-closed and degrade paths ───────────────────────────────────────────

// ctxCapturingDisconnector records the context each leg of the fail-closed sink
// was handed, so a test can prove they are not the same one.
type ctxCapturingDisconnector struct {
	targetedCtx   context.Context
	escalationCtx context.Context
}

func (d *ctxCapturingDisconnector) DisconnectRichPresenceClients(
	ctx context.Context, _ map[uuid.UUID]bool,
) error {
	d.targetedCtx = ctx
	// Stands in for the stall-under-load case: the targeted call is the one
	// that consumed the deadline.
	return errors.New("targeted disconnect timed out")
}

func (d *ctxCapturingDisconnector) DisconnectAllRichPresenceClients(ctx context.Context) error {
	d.escalationCtx = ctx
	return nil
}

// BeginAudienceRevocation satisfies Disconnector. This fake exists to observe
// DISCONNECTS, and the #2992 bracket is orthogonal to every assertion it
// carries, so the closer is inert. The fence's own behaviour is covered by
// topology_test.go's fenceStub, which records into the ordering trace.
func (d *ctxCapturingDisconnector) BeginAudienceRevocation() func() { return func() {} }

// The escalation must NOT reuse the context the targeted disconnect exhausted.
//
// Sharing it made the fail-closed terminal fail OPEN in the exact scenario it
// exists for: when the targeted call fails BECAUSE it hit dispatchTimeout, that
// context is already deadline-exceeded, so DisconnectAllRichPresenceClients
// returned instantly with a context error and only logged (PR #2738, Gitar).
//
// The signal is context IDENTITY. An earlier version compared deadlines, which
// works only if the two WithTimeout calls land on different clock ticks — a
// resolution-dependent discriminator. It also asserted the escalation context
// was unexpired, which proves nothing here: the fake returns its error without
// consuming the deadline, so a REUSED context would be unexpired too. Identity
// is exact — reuse yields the same context value, two WithTimeout calls never
// do (PR #2738 review, CodeRabbit).
func TestEscalationDoesNotReuseTheExhaustedContext(t *testing.T) {
	d := &ctxCapturingDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	r.disconnect(map[uuid.UUID]bool{uuid.New(): true}, "test")

	require.NotNil(t, d.targetedCtx, "the targeted disconnect must run first")
	require.NotNil(t, d.escalationCtx, "a failed targeted disconnect must escalate")

	_, ok := d.targetedCtx.Deadline()
	require.True(t, ok, "the targeted call must be bounded")
	_, ok = d.escalationCtx.Deadline()
	require.True(t, ok, "the escalation must be bounded too")

	assert.NotSame(t, d.targetedCtx, d.escalationCtx,
		"the escalation must get its OWN context; reusing the one the targeted "+
			"call exhausted makes the fail-closed terminal fail open under the exact "+
			"timeout it exists to bound")
}

// A cause that PROVES the transaction never committed must disconnect NOBODY:
// the handler's deferred rollback discarded the write, so no viewer's
// authorization changed and there is nothing stale to clear.
func TestAbandonIsANoOpForCausesThatProveNoCommit(t *testing.T) {
	for _, cause := range []presencecapture.Cause{
		presencecapture.CauseWriteFailed,
		presencecapture.CauseRowsAffected,
	} {
		d := &fakeDisconnector{}
		r := New(nil, &fakeRefresher{}, d, nil, nil)
		plan := &Plan{viewers: map[uuid.UUID]bool{uuid.New(): true}}

		r.Abandon(plan, cause)
		r.Close()

		assert.Empty(t, d.recipients,
			"%s proves no commit, so it must disconnect nobody", cause)
		assert.Zero(t, d.allCalls, "and must not escalate either")
	}
}

// An unresolved commit is UNKNOWN state, which is the one cause that must still
// fan out. Without this the split above would be indistinguishable from
// disabling Abandon entirely.
func TestAbandonStillFansOutForAnUnresolvedCommit(t *testing.T) {
	d := &fakeDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()
	viewer := uuid.New()

	r.Abandon(&Plan{viewers: map[uuid.UUID]bool{viewer: true}},
		presencecapture.CauseCommitUnresolved)

	assert.Equal(t, map[uuid.UUID]bool{viewer: true}, d.recipients,
		"unknown state must fail closed")
}

// foreignPlan satisfies presencecapture.Plan without being this bridge's Plan.
type foreignPlan struct{}

func (foreignPlan) HasWork() bool  { return true }
func (foreignPlan) Degraded() bool { return false }

// Complete's foreign-plan refusal is exercised against a REAL transaction by
// TestCompleteRejectsAForeignPlanWithoutCommitting in integration_test.go. It
// lives there because the property worth locking is that the caller's write is
// not committed, and a zero-value &sql.Tx{} can neither be committed nor
// observed — the version that stood here asserted only on the error string and
// would have panicked, rather than failed, if the guard moved after the commit
// (PR #2738 review, @code-reviewer).
//
// Complete's nil-tx guard needs no transaction to observe and stays in this
// file, as TestCompleteRejectsNilTx above.

func TestAbandonRejectsAForeignPlan(t *testing.T) {
	d := &fakeDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, logger.New("test"))
	defer r.Close()

	r.Abandon(foreignPlan{}, presencecapture.CauseCommitUnresolved)

	assert.Empty(t, d.recipients, "a foreign plan carries no audience this bridge can clear")
}

// degradePlan logs a FIXED enum and a count — never the underlying error and
// never a user ID. Exercised with a real logger because the nil-logger path
// skips the block entirely.
func TestDegradePlanLogsFixedEnumOnly(t *testing.T) {
	// Capture the log. The previous version asserted only on the returned plan
	// while its name promised the log was checked — so the one thing it claimed
	// to verify, that a degrade emits a fixed enum and no user identifier, was
	// the one thing it did not (PR #2738 review, CodeRabbit).
	var logBuf bytes.Buffer
	r := New(nil, &fakeRefresher{}, &fakeDisconnector{}, nil, logger.NewWithWriter(&logBuf))
	defer r.Close()
	principal, counterpart := uuid.New(), uuid.New()

	plan := r.degradePlan(presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   principal,
		Counterpart: counterpart,
	}, causeActiveScopeRead)

	require.True(t, plan.Degraded())
	assert.Equal(t, map[uuid.UUID]bool{principal: true, counterpart: true}, plan.viewers,
		"the conservative superset is the principals themselves")

	logged := logBuf.String()
	assert.Contains(t, logged, "active_scope_read", "the fixed failure_class enum is logged")
	assert.Contains(t, logged, "viewer_count", "and the count, which is not identifying")
	assert.NotContains(t, logged, principal.String(),
		"no user ID may reach the log ([internal]rules/observability.md)")
	assert.NotContains(t, logged, counterpart.String(), "and neither may the counterpart")
}

func TestDispatchToleratesANilPlan(t *testing.T) {
	d := &fakeDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	assert.NotPanics(t, func() { r.dispatch(nil) })
	assert.Empty(t, d.recipients)
}
