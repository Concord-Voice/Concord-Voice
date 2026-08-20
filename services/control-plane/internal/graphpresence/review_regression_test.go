package graphpresence

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

// countingDisconnector records every targeted call separately so a test can
// assert on how many times a recipient was asked for, not merely the union.
type countingDisconnector struct {
	calls    []map[uuid.UUID]bool
	allCalls int
	err      error
}

func (c *countingDisconnector) DisconnectRichPresenceClients(
	_ context.Context, recipients map[uuid.UUID]bool,
) error {
	snapshot := make(map[uuid.UUID]bool, len(recipients))
	for id, included := range recipients {
		snapshot[id] = included
	}
	c.calls = append(c.calls, snapshot)
	return c.err
}

func (c *countingDisconnector) DisconnectAllRichPresenceClients(_ context.Context) error {
	c.allCalls++
	return nil
}

func (c *countingDisconnector) timesAsked(id uuid.UUID) int {
	n := 0
	for _, call := range c.calls {
		if call[id] {
			n++
		}
	}
	return n
}

// A benign already-closed socket must NOT promote a two-user disconnect into a
// whole-node teardown. The escalation is the fail-closed terminal for a genuine
// unknown; a recipient whose socket was already gone WAS reached.
//
// Guards the HIGH from PR #2738 review (@security-reviewer). The other half of
// that fix lives in websocket.disconnectPrivacyCriticalClient, which no longer
// reports an already-closed conn as an error at all.
func TestSlowButSuccessfulDisconnectDoesNotEscalate(t *testing.T) {
	d := &countingDisconnector{err: context.DeadlineExceeded}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	r.disconnect(map[uuid.UUID]bool{uuid.New(): true}, "peripheral_disconnect")

	assert.Equal(t, 0, d.allCalls,
		"a bare context deadline means the loop was slow, not that a client failed; "+
			"escalating disconnects every client on the node exactly when it is already slow")
}

// A genuine per-client failure still escalates — the guard must not swallow the
// case the escalation exists for.
func TestGenuineDisconnectFailureStillEscalates(t *testing.T) {
	d := &countingDisconnector{err: errors.New("hub refused the disconnect")}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	r.disconnect(map[uuid.UUID]bool{uuid.New(): true}, "peripheral_disconnect")

	assert.Equal(t, 1, d.allCalls, "an unknown per-client failure must still fail closed")
}

// errors.Join mixes a real failure with a deadline; "any leaf is a context
// error" is the wrong question, so the mixed case must still escalate.
func TestMixedErrorEscalates(t *testing.T) {
	joined := errors.Join(errors.New("client 3 close failed"), context.DeadlineExceeded)
	assert.False(t, onlyContextError(joined),
		"a real per-client failure alongside a deadline must not be classified as benign")

	assert.True(t, onlyContextError(errors.Join(nil, context.DeadlineExceeded)))
	assert.True(t, onlyContextError(fmt.Errorf("wrapped: %w", context.Canceled)))
	assert.False(t, onlyContextError(nil))
	assert.False(t, onlyContextError(net.ErrClosed))
}

// dispatch disconnects leg.captured and then p.viewers, which overlap whenever
// the principals share a server. Asking twice is what manufactured the
// already-closed error that used to escalate.
func TestOverlappingRecipientIsDisconnectedOnce(t *testing.T) {
	d := &countingDisconnector{}
	r := New(nil, &fakeRefresher{err: presence.ErrRecheckSenderNotCurrent}, d, nil, nil)
	defer r.Close()

	shared := uuid.New()
	plan := &Plan{
		active:  []activeLeg{{senderID: uuid.New(), captured: map[uuid.UUID]bool{shared: true}}},
		viewers: map[uuid.UUID]bool{shared: true},
	}

	r.dispatch(plan)

	assert.Equal(t, 1, d.timesAsked(shared),
		"a recipient in both the leg and the peripheral set must be asked for exactly once")
}

// The bound-degrade must honour the same CanRevokeVisibility gate as the
// peripheral seed. Without it, accepting a friend request while in voice on a
// large server tore down every device of BOTH principals for a mutation that
// revokes nothing — walking around the gate this PR added to prevent exactly
// that (PR #2738 review, @code-reviewer).
func TestBoundExceededOnAnAdditiveFamilyDoesNotDisconnect(t *testing.T) {
	subject := presencecapture.Subject{
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
		Family:      presencecapture.FamilyFriendshipAccept,
	}
	// Read the policy from the registry, exactly as CaptureInTx's step 1 does,
	// so this test cannot pass by handing planForBoundExceeded an answer the
	// registry does not actually declare.
	policy, err := presencecapture.PolicyFor(subject.Family)
	require.NoError(t, err)
	require.False(t, policy.CanRevokeVisibility, "precondition: accept is additive")

	r := New(nil, &fakeRefresher{}, &countingDisconnector{}, nil, nil)
	defer r.Close()

	// Exercise the DECISION, not a hand-built Plan. Asserting on a struct this
	// test constructed itself would pass with the gate deleted.
	plan := r.planForBoundExceeded(subject, policy)

	require.False(t, plan.Degraded(),
		"an additive family over the bound must not degrade")
	require.False(t, plan.HasWork(),
		"an additive family over the bound is the benign empty terminal, not a disconnect")

	// The teardown is what actually hurt: degradePlan seeds both principals and
	// dispatch resolves a degraded plan straight to a disconnect.
	d := &countingDisconnector{}
	r2 := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r2.Close()
	r2.dispatch(r2.planForBoundExceeded(subject, policy))
	assert.Equal(t, 0, d.timesAsked(subject.Principal),
		"accepting a friend request must never disconnect the principal")
	assert.Equal(t, 0, d.timesAsked(subject.Counterpart),
		"accepting a friend request must never disconnect the counterpart")
}

// The revoking direction must still degrade — the gate narrows the fix, it does
// not disable the bound.
func TestBoundExceededOnARevokingFamilyStillDegrades(t *testing.T) {
	subject := presencecapture.Subject{
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
		Family:      presencecapture.FamilyFriendshipRemove,
	}
	policy, err := presencecapture.PolicyFor(subject.Family)
	require.NoError(t, err)
	require.True(t, policy.CanRevokeVisibility, "precondition: removal can revoke")

	d := &countingDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	degraded := r.planForBoundExceeded(subject, policy)
	require.True(t, degraded.Degraded(), "the gate must narrow the fix, not disable the bound")
	require.True(t, degraded.HasWork())

	r.dispatch(degraded)
	assert.Equal(t, 1, d.timesAsked(subject.Principal),
		"a revoking family over the bound still clears the principals")
}

// Abandon is the fail-CLOSED terminal. A plan it cannot read is precisely the
// case where it does not know who to clear, so returning silently chose the
// open direction in the one place that exists to choose the closed one.
func TestAbandonEscalatesOnAForeignPlan(t *testing.T) {
	d := &countingDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	r.Abandon(foreignPlan{}, presencecapture.CauseCommitUnresolved)

	assert.Equal(t, 1, d.allCalls,
		"an unreadable plan under a cause that does not prove no-commit must fail closed")
}

// A cause that PROVES no commit must still be a no-op even for a foreign plan —
// nothing was written, so nothing may be torn down.
func TestAbandonStaysANoOpForProvenNoCommitEvenWhenForeign(t *testing.T) {
	d := &countingDisconnector{}
	r := New(nil, &fakeRefresher{}, d, nil, nil)
	defer r.Close()

	r.Abandon(foreignPlan{}, presencecapture.CauseWriteFailed)

	assert.Equal(t, 0, d.allCalls, "a proven-no-commit cause disconnects nobody")
	assert.Empty(t, d.calls)
}

// foreignPlan is declared in reconciler_test.go — reused here deliberately so
// both files exercise the same "not our concrete type" shape.

// refreshLeg guards a nil refresher rather than panicking. The sink worker has
// no recover(), so a panic there takes the control plane down instead of one
// plan. Hygiene rather than a live crash — router.go wires a concrete service.
func TestRefreshLegFailsClosedWhenActivityIsUnwired(t *testing.T) {
	r := New(nil, nil, &countingDisconnector{}, nil, nil)
	defer r.Close()

	err := r.refreshLeg(activeLeg{senderID: uuid.New()})
	require.Error(t, err, "an unwired refresher must return an error, not panic")
}
