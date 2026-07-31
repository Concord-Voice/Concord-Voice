package rbac

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// presenceRecheckStub records the ORDER of every seam call. Ordering is the
// property being locked: PrepareCapture must run before BeginTx (outside the
// advisory lock) and CaptureVisibility after it.
type presenceRecheckStub struct {
	sequence      []string
	prepareCalls  []prepareCall
	visibilityTxs []*sql.Tx
	executes      int
	abandons      []string
	plan          PresenceRecheckPlan
	prepareErr    error
	visibilityErr error
	// prepareProbe and visibilityProbe let the ordering lock observe the
	// advisory-lock state at each phase boundary. Phase 1 must observe the
	// lock UNHELD (it runs before BeginTx); phase 2 must observe it HELD.
	prepareProbe    func()
	visibilityProbe func(*sql.Tx)
}

type prepareCall struct {
	serverID   string
	channelIDs []string
	onlyUserID *string
}

type presenceRecheckPlanStub struct{ work bool }

func (p *presenceRecheckPlanStub) HasWork() bool { return p.work }

func (s *presenceRecheckStub) PrepareCapture(
	_ context.Context, serverID string, channelIDs []string, onlyUserID *string,
) (PresenceRecheckPlan, error) {
	s.sequence = append(s.sequence, "PrepareCapture")
	if s.prepareProbe != nil {
		s.prepareProbe()
	}
	s.prepareCalls = append(s.prepareCalls, prepareCall{serverID, channelIDs, onlyUserID})
	return s.plan, s.prepareErr
}

func (s *presenceRecheckStub) CaptureVisibility(
	_ context.Context, tx *sql.Tx, _ PresenceRecheckPlan,
) error {
	s.sequence = append(s.sequence, "CaptureVisibility")
	if s.visibilityProbe != nil {
		s.visibilityProbe(tx)
	}
	s.visibilityTxs = append(s.visibilityTxs, tx)
	return s.visibilityErr
}

func (s *presenceRecheckStub) Execute(PresenceRecheckPlan) {
	s.sequence = append(s.sequence, "Execute")
	s.executes++
}

func (s *presenceRecheckStub) Abandon(_ PresenceRecheckPlan, cause string) {
	s.sequence = append(s.sequence, "Abandon")
	s.abandons = append(s.abandons, cause)
}

func TestPresenceHelpers_NilRecheck_AreNoOps(t *testing.T) {
	h := &Handler{}

	plan, err := h.preparePresenceCapture(context.Background(), "server", []string{"channel"}, nil)
	require.NoError(t, err)
	assert.Nil(t, plan)

	require.NoError(t, h.capturePresenceVisibility(context.Background(), nil, nil))

	assert.NotPanics(t, func() { h.presenceExecute(nil) })
	assert.NotPanics(t, func() { h.presenceAbandon(nil, "dispatch_unavailable") })
}

func TestPresenceHelpers_WiredRecheck_Delegate(t *testing.T) {
	stub := &presenceRecheckStub{plan: &presenceRecheckPlanStub{work: true}}
	h := &Handler{}
	h.SetPresenceRecheck(stub)

	plan, err := h.preparePresenceCapture(context.Background(), "server", []string{"a", "b"}, nil)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.Len(t, stub.prepareCalls, 1)
	assert.Equal(t, "server", stub.prepareCalls[0].serverID)
	assert.Equal(t, []string{"a", "b"}, stub.prepareCalls[0].channelIDs)
	assert.Nil(t, stub.prepareCalls[0].onlyUserID)

	userID := "affected-user"
	_, err = h.preparePresenceCapture(context.Background(), "server", nil, &userID)
	require.NoError(t, err)
	require.Len(t, stub.prepareCalls, 2)
	require.NotNil(t, stub.prepareCalls[1].onlyUserID)
	assert.Equal(t, userID, *stub.prepareCalls[1].onlyUserID,
		"user-scope bounding survives the collapse of CaptureUserScope into PrepareCapture")

	require.NoError(t, h.capturePresenceVisibility(context.Background(), nil, plan))

	h.presenceExecute(plan)
	assert.Equal(t, 1, stub.executes)

	h.presenceAbandon(plan, "ambiguous_commit")
	assert.Equal(t, []string{"ambiguous_commit"}, stub.abandons)
}

func TestPreparePresenceCapture_Error_Propagates(t *testing.T) {
	stub := &presenceRecheckStub{prepareErr: errors.New("candidate read failed")}
	h := &Handler{}
	h.SetPresenceRecheck(stub)

	_, err := h.preparePresenceCapture(context.Background(), "server", []string{"a"}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "candidate read failed")
}

func TestCapturePresenceVisibility_Error_Propagates(t *testing.T) {
	stub := &presenceRecheckStub{
		plan:          &presenceRecheckPlanStub{work: true},
		visibilityErr: errors.New("visibility read failed"),
	}
	h := &Handler{}
	h.SetPresenceRecheck(stub)

	err := h.capturePresenceVisibility(context.Background(), nil, stub.plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "visibility read failed")
}

func TestPresenceExecute_PlanWithoutWork_SkipsDispatch(t *testing.T) {
	stub := &presenceRecheckStub{}
	h := &Handler{}
	h.SetPresenceRecheck(stub)

	h.presenceExecute(&presenceRecheckPlanStub{work: false})

	assert.Zero(t, stub.executes, "an empty plan is the benign terminal, not a dispatch")
}
