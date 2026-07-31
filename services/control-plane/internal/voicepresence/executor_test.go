package voicepresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLogger discards output so the fail-closed paths under test can log
// without polluting the suite.
func testLogger() *logger.Logger { return logger.NewWithWriter(io.Discard) }

type refresherStub struct {
	mu    sync.Mutex
	calls []uuid.UUID
	errs  map[uuid.UUID]error
	done  chan struct{}
}

func (r *refresherStub) RefreshServerVoiceRecheck(
	_ context.Context, senderID uuid.UUID, _ presence.Scope, _ map[uuid.UUID]bool,
) error {
	r.mu.Lock()
	r.calls = append(r.calls, senderID)
	err := r.errs[senderID]
	last := len(r.calls)
	r.mu.Unlock()
	if r.done != nil && last > 0 {
		select {
		case r.done <- struct{}{}:
		default:
		}
	}
	return err
}

type disconnectorStub struct {
	mu       sync.Mutex
	sets     []map[uuid.UUID]bool
	allCalls int
	err      error
}

func (d *disconnectorStub) DisconnectRichPresenceClients(
	_ context.Context, recipients map[uuid.UUID]bool,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	copied := make(map[uuid.UUID]bool, len(recipients))
	for k, v := range recipients {
		copied[k] = v
	}
	d.sets = append(d.sets, copied)
	return d.err
}

func (d *disconnectorStub) DisconnectAllRichPresenceClients(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allCalls++
	return nil
}

func newTestExecutor(
	refresher *refresherStub, disconnector *disconnectorStub,
) *Executor {
	return NewExecutor(nil, refresher, nil, nil, disconnector, testLogger())
}

func planWith(senderID uuid.UUID, viewers ...uuid.UUID) *Plan {
	audience := make(map[uuid.UUID]bool, len(viewers))
	for _, viewerID := range viewers {
		audience[viewerID] = true
	}
	channelID := uuid.New()
	return &Plan{Senders: []SenderCapture{{
		SenderID:    senderID,
		Scope:       scopeFor(channelID),
		OldAudience: audience,
	}}}
}

func TestExecutor_Execute_DispatchesOneRefreshPerSender(t *testing.T) {
	refresher := &refresherStub{done: make(chan struct{}, 4)}
	executor := newTestExecutor(refresher, &disconnectorStub{})
	defer executor.Close()
	senderID := uuid.New()

	executor.Execute(planWith(senderID, uuid.New()))

	select {
	case <-refresher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh was never dispatched")
	}
	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	assert.Equal(t, []uuid.UUID{senderID}, refresher.calls)
}

func TestExecutor_Execute_SenderNotCurrent_DisconnectsOnlyThatSendersViewers(t *testing.T) {
	senderID := uuid.New()
	lostViewer := uuid.New()
	refresher := &refresherStub{
		done: make(chan struct{}, 4),
		errs: map[uuid.UUID]error{
			senderID: fmt.Errorf("recheck: %w", presence.ErrRecheckSenderNotCurrent),
		},
	}
	disconnector := &disconnectorStub{}
	executor := newTestExecutor(refresher, disconnector)
	defer executor.Close()

	executor.Execute(planWith(senderID, lostViewer))

	select {
	case <-refresher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh was never dispatched")
	}
	require.Eventually(t, func() bool {
		disconnector.mu.Lock()
		defer disconnector.mu.Unlock()
		return len(disconnector.sets) == 1
	}, 2*time.Second, 10*time.Millisecond)

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	assert.Equal(t, map[uuid.UUID]bool{lostViewer: true}, disconnector.sets[0])
	assert.Zero(t, disconnector.allCalls,
		"F3: a stale sender is viewer-scoped, never a replica-wide disconnect")
}

func TestExecutor_Abandon_DisconnectsTheWholeCapturedAudience(t *testing.T) {
	disconnector := &disconnectorStub{}
	executor := newTestExecutor(&refresherStub{}, disconnector)
	defer executor.Close()
	viewerA, viewerB := uuid.New(), uuid.New()

	executor.Abandon(&Plan{Senders: []SenderCapture{
		{SenderID: uuid.New(), OldAudience: map[uuid.UUID]bool{viewerA: true}},
		{SenderID: uuid.New(), OldAudience: map[uuid.UUID]bool{viewerB: true}},
	}}, "ambiguous_commit")

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	require.Len(t, disconnector.sets, 1)
	assert.Equal(t, map[uuid.UUID]bool{viewerA: true, viewerB: true}, disconnector.sets[0])
}

func TestExecutor_Abandon_DisconnectFailure_EscalatesToGlobal(t *testing.T) {
	disconnector := &disconnectorStub{err: errors.New("hub unavailable")}
	executor := newTestExecutor(&refresherStub{}, disconnector)
	defer executor.Close()

	executor.Abandon(planWith(uuid.New(), uuid.New()), "dispatch_unavailable")

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	assert.Equal(t, 1, disconnector.allCalls,
		"a failed viewer-scoped disconnect escalates to the global terminal (class 10)")
}

func TestExecutor_Execute_AfterClose_AbandonsFailClosed(t *testing.T) {
	disconnector := &disconnectorStub{}
	executor := newTestExecutor(&refresherStub{}, disconnector)
	executor.Close()
	viewer := uuid.New()

	executor.Execute(planWith(uuid.New(), viewer))

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	require.Len(t, disconnector.sets, 1, "an unreachable dispatch fails closed (class 5)")
	assert.Equal(t, map[uuid.UUID]bool{viewer: true}, disconnector.sets[0])
}

func TestExecutor_Execute_EmptyPlan_IsANoOp(t *testing.T) {
	refresher := &refresherStub{}
	disconnector := &disconnectorStub{}
	executor := newTestExecutor(refresher, disconnector)
	defer executor.Close()

	executor.Execute(&Plan{})

	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	assert.Empty(t, refresher.calls)
	assert.Empty(t, disconnector.sets)
}

type visibilityStub struct {
	calls   []string
	visible []string
	err     error
}

func (v *visibilityStub) FilterVisibleUserIDsForChannelTx(
	_ context.Context, _ *sql.Tx, _, channelID string, _ []string,
) ([]string, error) {
	v.calls = append(v.calls, channelID)
	return v.visible, v.err
}

func TestExecutor_CaptureVisibility_NoCandidates_IssuesZeroVisibilityQueries(t *testing.T) {
	visibility := &visibilityStub{}
	executor := NewExecutor(
		nil, &refresherStub{}, visibility, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()
	channelID := uuid.New()

	// A channel with an active sender whose base presence is off: phase 1
	// produced a sender entry with an EMPTY candidate set.
	plan := &Plan{ServerID: uuid.New().String(), Senders: []SenderCapture{{
		SenderID:    uuid.New(),
		ChannelID:   channelID.String(),
		Scope:       scopeFor(channelID),
		Candidates:  map[uuid.UUID]bool{},
		OldAudience: map[uuid.UUID]bool{},
	}}}

	require.NoError(t, executor.CaptureVisibility(context.Background(), nil, plan))

	assert.Empty(t, visibility.calls, "phase 2 must issue zero queries with no candidates")
	assert.False(t, plan.HasWork(), "an empty capture is the benign terminal")
}

func TestExecutor_CaptureVisibility_OneQueryPerChannel_IntersectsIntoOldAudience(t *testing.T) {
	visible := uuid.New()
	invisible := uuid.New()
	visibility := &visibilityStub{visible: []string{visible.String()}}
	executor := NewExecutor(
		nil, &refresherStub{}, visibility, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()
	channelID := uuid.New()

	// Two senders in the SAME channel: advisory-lock hold time is O(#channels),
	// so they must share exactly one visibility query.
	plan := &Plan{ServerID: uuid.New().String(), Senders: []SenderCapture{
		{
			SenderID: uuid.New(), ChannelID: channelID.String(), Scope: scopeFor(channelID),
			Candidates:  map[uuid.UUID]bool{visible: true, invisible: true},
			OldAudience: map[uuid.UUID]bool{},
		},
		{
			SenderID: uuid.New(), ChannelID: channelID.String(), Scope: scopeFor(channelID),
			Candidates:  map[uuid.UUID]bool{visible: true},
			OldAudience: map[uuid.UUID]bool{},
		},
	}}

	require.NoError(t, executor.CaptureVisibility(context.Background(), nil, plan))

	assert.Equal(t, []string{channelID.String()}, visibility.calls,
		"exactly one FilterVisibleUserIDsForChannelTx per affected channel")
	assert.Equal(t, map[uuid.UUID]bool{visible: true}, plan.Senders[0].OldAudience)
	assert.Equal(t, map[uuid.UUID]bool{visible: true}, plan.Senders[1].OldAudience)
	assert.True(t, plan.HasWork())
}

func TestExecutor_CaptureVisibility_QueryError_IsReturned(t *testing.T) {
	visibility := &visibilityStub{err: errors.New("visibility read failed")}
	executor := NewExecutor(
		nil, &refresherStub{}, visibility, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()
	channelID := uuid.New()

	plan := &Plan{ServerID: uuid.New().String(), Senders: []SenderCapture{{
		SenderID: uuid.New(), ChannelID: channelID.String(), Scope: scopeFor(channelID),
		Candidates:  map[uuid.UUID]bool{uuid.New(): true},
		OldAudience: map[uuid.UUID]bool{},
	}}}

	err := executor.CaptureVisibility(context.Background(), nil, plan)

	require.Error(t, err, "a phase-2 failure must roll the caller's transaction back")
	assert.Contains(t, err.Error(), "visibility read failed")
}

func TestExecutor_PrepareCapture_InvalidServerID_FailsBeforeAnyRead(t *testing.T) {
	visibility := &visibilityStub{}
	executor := NewExecutor(
		nil, &refresherStub{}, visibility, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()

	_, err := executor.PrepareCapture(
		context.Background(), "not-a-uuid", []string{uuid.New().String()}, nil,
	)

	require.Error(t, err, "phase 1 fails before BeginTx, so nothing is written")
	assert.Empty(t, visibility.calls)
}

func TestExecutor_PrepareCapture_ChannelLimitExceeded_FailsClosed(t *testing.T) {
	executor := NewExecutor(
		nil, &refresherStub{}, &visibilityStub{}, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()

	channelIDs := make([]string, presenceCaptureMaxChannels+1)
	for i := range channelIDs {
		channelIDs[i] = uuid.New().String()
	}

	_, err := executor.PrepareCapture(
		context.Background(), uuid.New().String(), channelIDs, nil,
	)

	require.ErrorIs(t, err, ErrCaptureChannelLimit,
		"the fan-out bound is a safety bound checked before any transaction opens")
}

func TestExecutor_PrepareCapture_NoChannels_ProducesTheBenignEmptyPlan(t *testing.T) {
	executor := NewExecutor(
		nil, &refresherStub{}, &visibilityStub{}, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()

	plan, err := executor.PrepareCapture(
		context.Background(), uuid.New().String(), []string{}, nil,
	)

	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.False(t, plan.HasWork())
}

func TestCandidateIDStrings_OnlyUserID_BoundsTheFilterInput(t *testing.T) {
	inScope := uuid.New()
	other := uuid.New()
	candidates := map[uuid.UUID]bool{inScope: true, other: true}

	t.Run("affected user is a candidate", func(t *testing.T) {
		scoped := inScope.String()

		assert.Equal(t, []string{inScope.String()}, candidateIDStrings(candidates, &scoped))
	})

	t.Run("affected user is not a candidate", func(t *testing.T) {
		absent := uuid.New().String()

		assert.Empty(t, candidateIDStrings(candidates, &absent),
			"a user outside the candidate set needs no visibility query")
	})

	t.Run("unparseable affected user", func(t *testing.T) {
		invalid := "not-a-uuid"

		assert.Empty(t, candidateIDStrings(candidates, &invalid))
	})

	t.Run("unbounded", func(t *testing.T) {
		assert.Len(t, candidateIDStrings(candidates, nil), 2)
	})
}

// withAuthorityCapture classifies the fan-out bound in its logs via
// errors.Is(err, rbac.ErrPresenceCaptureLimited). internal/rbac cannot import
// this package (the zero-presence-imports invariant), so that classification
// works only because ErrCaptureChannelLimit WRAPS the consumer-side sentinel.
// Redeclaring it as a plain errors.New would compile, pass every other test,
// and silently take the operator's only signal that role management is refusing
// writes on a busy server.
func TestErrCaptureChannelLimit_WrapsTheConsumerSentinel(t *testing.T) {
	require.ErrorIs(t, ErrCaptureChannelLimit, rbac.ErrPresenceCaptureLimited,
		"the bound must stay classifiable from internal/rbac")

	wrapped := fmt.Errorf("%w: %d channels", ErrCaptureChannelLimit, 65)
	assert.ErrorIs(t, wrapped, rbac.ErrPresenceCaptureLimited,
		"PrepareCapture's own %w wrap must preserve the classification")
	assert.NotErrorIs(t, errors.New("some other capture failure"), rbac.ErrPresenceCaptureLimited,
		"an unrelated capture failure is not the bound")
}
