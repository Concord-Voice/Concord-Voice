package expiration

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeExpiryRunner struct {
	mu      sync.Mutex
	plans   []purge.ExpiryPlan
	ctxErrs []error
	run     func(context.Context, purge.ExpiryPlan) (purge.Result, error)
}

func (f *fakeExpiryRunner) RunExpiryBatch(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
	f.mu.Lock()
	f.plans = append(f.plans, plan)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.mu.Unlock()
	if f.run == nil {
		return purge.Result{DeletedCount: len(plan.CandidateIDs)}, nil
	}
	return f.run(ctx, plan)
}

type purgeNotice struct {
	kind   string
	id     string
	cutoff time.Time
}

type fakePurgeNotifier struct {
	mu      sync.Mutex
	notices []purgeNotice
}

func (f *fakePurgeNotifier) EmitServerPurged(_ context.Context, serverID string, cutoff time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, purgeNotice{kind: "server", id: serverID, cutoff: cutoff})
}

func (f *fakePurgeNotifier) EmitDMPurged(_ context.Context, conversationID string, cutoff time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, purgeNotice{kind: "dm", id: conversationID, cutoff: cutoff})
}

const (
	expirerChannelID      = "11111111-1111-4111-8111-111111111111"
	expirerServerID       = "22222222-2222-4222-8222-222222222222"
	expirerConversationID = "33333333-3333-4333-8333-333333333333"
	expirerMessageID      = "44444444-4444-4444-8444-444444444444"
)

func dueTransition(cutoff time.Time, ids ...string) Transition {
	return Transition{Current: Policy{UpdatedAt: &cutoff}, DuePinnedIDs: ids}
}

func newTestExpirer(lifetime context.Context, t *testing.T, runner ExpiryBatchRunner, notifier PurgeNotifier) (*ContextExpirer, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	expirer, err := NewContextExpirer(lifetime, runner, notifier, logger.NewWithWriter(&logs))
	require.NoError(t, err)
	return expirer, &logs
}

func TestNewContextExpirer_RequiresItsDependencies(t *testing.T) {
	log := logger.NewWithWriter(&bytes.Buffer{})
	_, err := NewContextExpirer(context.Background(), nil, &fakePurgeNotifier{}, log)
	assert.Error(t, err)
	_, err = NewContextExpirer(context.Background(), &fakeExpiryRunner{}, nil, log)
	assert.Error(t, err)
	_, err = NewContextExpirer(context.Background(), &fakeExpiryRunner{}, &fakePurgeNotifier{}, nil)
	assert.Error(t, err)
	var noLifetime context.Context // the documented "never shut down" input
	expirer, err := NewContextExpirer(noLifetime, &fakeExpiryRunner{}, &fakePurgeNotifier{}, log)
	require.NoError(t, err)
	assert.False(t, expirer.shuttingDown(), "a nil lifetime never shuts the expirer down")
}

func TestContextExpirer_NoDueIDsRunsNothing(t *testing.T) {
	runner, notifier := &fakeExpiryRunner{}, &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)
	assert.Zero(t, expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID, dueTransition(time.Now())))
	assert.Zero(t, expirer.ExpireConversationPins(context.Background(), expirerConversationID, false, Transition{DuePinnedIDs: []string{expirerMessageID}}))
	assert.Empty(t, runner.plans)
	assert.Empty(t, notifier.notices)
	assert.Empty(t, logs.String())
	assert.Zero(t, (*ContextExpirer)(nil).ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID, dueTransition(time.Now(), expirerMessageID)))
}

func TestContextExpirer_ChannelRunsRestrictedPlanAtAcceptedCutoffAndAnnounces(t *testing.T) {
	runner, notifier := &fakeExpiryRunner{}, &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 123000, time.UTC)

	// A request context that is already gone must not cancel the deletion: the
	// policy has committed, and a client hanging up is not a reason to leave
	// due rows for the sweeper.
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	deleted := expirer.ExpireChannelPins(requestCtx, expirerChannelID, expirerServerID, dueTransition(cutoff, expirerMessageID))

	assert.Equal(t, 1, deleted)
	require.Len(t, runner.plans, 1)
	plan := runner.plans[0]
	assert.Equal(t, purge.ContextChannel, plan.ContextType)
	assert.Equal(t, expirerChannelID, plan.ContextID)
	require.NotNil(t, plan.ServerID)
	assert.Equal(t, expirerServerID, *plan.ServerID)
	assert.Equal(t, []string{expirerMessageID}, plan.CandidateIDs)
	assert.True(t, plan.ExpiresBefore.Equal(cutoff), "the engine rechecks at the accepted cutoff C, never now()")
	assert.NoError(t, runner.ctxErrs[0], "the run context is detached from the request")
	assert.Equal(t, []purgeNotice{{kind: "server", id: expirerServerID, cutoff: cutoff}}, notifier.notices)
	assert.Empty(t, logs.String())
}

func TestContextExpirer_ConversationContextTypeFollowsIsGroup(t *testing.T) {
	cutoff := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		isGroup bool
		want    purge.ContextType
	}{
		{"one to one", false, purge.ContextDM},
		{"group", true, purge.ContextGroup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, notifier := &fakeExpiryRunner{}, &fakePurgeNotifier{}
			expirer, _ := newTestExpirer(context.Background(), t, runner, notifier)
			assert.Equal(t, 1, expirer.ExpireConversationPins(context.Background(), expirerConversationID, tc.isGroup, dueTransition(cutoff, expirerMessageID)))
			require.Len(t, runner.plans, 1)
			assert.Equal(t, tc.want, runner.plans[0].ContextType)
			assert.Nil(t, runner.plans[0].ServerID, "a DM plan carries no server")
			assert.Equal(t, []purgeNotice{{kind: "dm", id: expirerConversationID, cutoff: cutoff}}, notifier.notices)
		})
	}
}

func TestContextExpirer_NothingDeletedAnnouncesNothing(t *testing.T) {
	runner := &fakeExpiryRunner{run: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		return purge.Result{PurgeID: "audit", DeletedCount: 0}, nil
	}}
	notifier := &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)
	assert.Zero(t, expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID, dueTransition(time.Now(), expirerMessageID)))
	assert.Empty(t, notifier.notices)
	assert.Empty(t, logs.String())
}

// TestContextExpirer_RunFailureLogsFixedClassAndCountsOnly pins C-OBS: one
// line, a fixed failure_class, the two counts, and nothing that names the
// context, a message, or the driver's error text.
func TestContextExpirer_RunFailureLogsFixedClassAndCountsOnly(t *testing.T) {
	runner := &fakeExpiryRunner{run: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		return purge.Result{DeletedCount: 1}, errors.New("pq: finalize failed for id " + expirerMessageID)
	}}
	notifier := &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)

	deleted := expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID, dueTransition(time.Now(), expirerMessageID, expirerChannelID))

	assert.Equal(t, 1, deleted)
	assert.Len(t, notifier.notices, 1, "rows that were deleted are still announced")
	line := logs.String()
	assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("\n")), "exactly one log line")
	assert.Contains(t, line, `msg="Pinned expiry did not complete"`)
	assert.Contains(t, line, "failure_class=pinned_expiry_run_failed")
	assert.Contains(t, line, "due=2")
	assert.Contains(t, line, "deleted=1")
	for _, forbidden := range []string{expirerChannelID, expirerServerID, expirerMessageID, "pq:", "error="} {
		assert.NotContains(t, line, forbidden)
	}
}

func TestContextExpirer_DeadlineLogsDeadlineClass(t *testing.T) {
	runner := &fakeExpiryRunner{run: func(ctx context.Context, _ purge.ExpiryPlan) (purge.Result, error) {
		<-ctx.Done()
		return purge.Result{}, ctx.Err()
	}}
	expirer, logs := newTestExpirer(context.Background(), t, runner, &fakePurgeNotifier{})
	expirer.timeout = 5 * time.Millisecond

	assert.Zero(t, expirer.ExpireConversationPins(context.Background(), expirerConversationID, false, dueTransition(time.Now(), expirerMessageID)))
	assert.Contains(t, logs.String(), "failure_class=pinned_expiry_deadline")
	assert.Contains(t, logs.String(), "due=1")
	assert.Contains(t, logs.String(), "deleted=0")
}

// TestContextExpirer_ShutdownIsNotAFailure pins §8: a run the process
// lifecycle cancels is not logged as a failure.
func TestContextExpirer_ShutdownIsNotAFailure(t *testing.T) {
	lifetime, shutdown := context.WithCancel(context.Background())
	runner := &fakeExpiryRunner{run: func(ctx context.Context, _ purge.ExpiryPlan) (purge.Result, error) {
		shutdown()
		<-ctx.Done()
		return purge.Result{}, ctx.Err()
	}}
	expirer, logs := newTestExpirer(lifetime, t, runner, &fakePurgeNotifier{})

	assert.Zero(t, expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID, dueTransition(time.Now(), expirerMessageID)))
	require.Len(t, runner.plans, 1, "the run started before shutdown")
	assert.Empty(t, logs.String())
}

// A batch deletes at most the engine's stride, which can be smaller than the
// due set. The expirer runs batches until every due id is gone and announces
// once (#3459 Codex review). With one batch it left the rest to the periodic
// sweep and still reported success.
func TestContextExpirer_DrainsDueSetLargerThanTheStride(t *testing.T) {
	const stride = 2
	ids := []string{
		"55555555-5555-4555-8555-555555555551",
		"55555555-5555-4555-8555-555555555552",
		"55555555-5555-4555-8555-555555555553",
		"55555555-5555-4555-8555-555555555554",
		"55555555-5555-4555-8555-555555555555",
	}
	remaining := len(ids)
	runner := &fakeExpiryRunner{run: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		n := min(stride, remaining)
		remaining -= n
		return purge.Result{DeletedCount: n}, nil
	}}
	notifier := &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)

	deleted := expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID, dueTransition(time.Now(), ids...))

	assert.Equal(t, len(ids), deleted)
	assert.Len(t, runner.plans, 3, "ceil(5/2) batches, and none after the set is drained")
	assert.Len(t, notifier.notices, 1, "one announcement covers every batch")
	assert.Empty(t, logs.String())
}

// A batch that deletes nothing ends the loop: what is left is no longer due or
// no longer there, and another batch would only write an empty audit row.
func TestContextExpirer_StopsWhenABatchDeletesNothing(t *testing.T) {
	calls := 0
	runner := &fakeExpiryRunner{run: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		calls++
		if calls == 1 {
			return purge.Result{DeletedCount: 1}, nil
		}
		return purge.Result{}, nil
	}}
	notifier := &fakePurgeNotifier{}
	expirer, _ := newTestExpirer(context.Background(), t, runner, notifier)

	deleted := expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID,
		dueTransition(time.Now(), expirerMessageID, "55555555-5555-4555-8555-555555555551", "55555555-5555-4555-8555-555555555552"))

	assert.Equal(t, 1, deleted)
	assert.Len(t, runner.plans, 2)
	assert.Len(t, notifier.notices, 1)
}

// A failing batch stops the loop, keeps what earlier batches deleted, announces
// it, and reports the failure once.
func TestContextExpirer_StopsOnABatchError(t *testing.T) {
	calls := 0
	runner := &fakeExpiryRunner{run: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		calls++
		if calls == 1 {
			return purge.Result{DeletedCount: 1}, nil
		}
		return purge.Result{}, errors.New("forced batch failure")
	}}
	notifier := &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)

	deleted := expirer.ExpireChannelPins(context.Background(), expirerChannelID, expirerServerID,
		dueTransition(time.Now(), expirerMessageID, "55555555-5555-4555-8555-555555555551"))

	assert.Equal(t, 1, deleted)
	assert.Len(t, runner.plans, 2)
	assert.Len(t, notifier.notices, 1)
	assert.Contains(t, logs.String(), failureClassPinnedExpiryRunFailed)
	assert.Contains(t, logs.String(), "deleted=1")
}

// TestContextExpirer_CallerDeadlineCutsTheRunBudget pins the response budget
// (#3459 Codex review): a caller's deadline shortens the run to what is left of
// it after the announcement's second, so the PATCH answers inside the server's
// write timeout. The expirer's own detached context drops the caller's
// deadline, so without the cut the run would keep its full 10 seconds.
func TestContextExpirer_CallerDeadlineCutsTheRunBudget(t *testing.T) {
	var runDeadline time.Time
	runner := &fakeExpiryRunner{run: func(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
		runDeadline, _ = ctx.Deadline()
		return purge.Result{DeletedCount: len(plan.CandidateIDs)}, nil
	}}
	notifier := &fakePurgeNotifier{}
	expirer, _ := newTestExpirer(context.Background(), t, runner, notifier)

	callerDeadline := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	assert.Equal(t, 1, expirer.ExpireChannelPins(ctx, expirerChannelID, expirerServerID, dueTransition(time.Now(), expirerMessageID)))
	require.False(t, runDeadline.IsZero())
	assert.WithinDuration(t, callerDeadline.Add(-pinnedExpiryNotifyTimeout), runDeadline, 200*time.Millisecond,
		"the run ends a notification's length before the caller's deadline, not after its own 10 seconds")
	assert.Len(t, notifier.notices, 1)
}

// TestContextExpirer_SpentDeadlineSkipsTheRun pins the other half: with no
// time left, nothing runs, the deadline class is logged, and the sweeper is
// left to delete the rows.
func TestContextExpirer_SpentDeadlineSkipsTheRun(t *testing.T) {
	runner := &fakeExpiryRunner{}
	notifier := &fakePurgeNotifier{}
	expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(pinnedExpiryNotifyTimeout/2))
	defer cancel()

	assert.Zero(t, expirer.ExpireChannelPins(ctx, expirerChannelID, expirerServerID, dueTransition(time.Now(), expirerMessageID)))
	assert.Empty(t, runner.plans, "no batch starts without time to finish")
	assert.Empty(t, notifier.notices)
	assert.Contains(t, logs.String(), "failure_class=pinned_expiry_deadline")
	assert.Contains(t, logs.String(), "due=1")
	assert.Contains(t, logs.String(), "deleted=0")
}

func TestResponseBudget_LeavesRoomToWrite(t *testing.T) {
	assert.Less(t, ResponseBudget, 15*time.Second, "the budget leaves room inside the server's 15 s WriteTimeout")
}
