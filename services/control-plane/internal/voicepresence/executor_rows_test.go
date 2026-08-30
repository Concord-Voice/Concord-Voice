package voicepresence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 1 reads *sql.Rows on e.db, so its resolved paths need a real *sql.DB.
// This scripted database/sql driver supplies one without PostgreSQL, following
// the pattern already established in internal/presence/activity_builder_test.go.
// senderPresence stays nil throughout, so CaptureServerVoiceCandidates resolves
// to the empty candidate set without touching the database — which keeps these
// tests focused on the enumeration and row-error surface that belongs here.

const executorRowsDriverName = "voicepresence-executor-rows-test"

var executorRowsDriverOnce sync.Once

var (
	errExecutorChannelsQuery  = errors.New("forced voice-channel query failure")
	errExecutorChannelsIter   = errors.New("forced voice-channel iteration failure")
	errExecutorSendersQuery   = errors.New("forced active-sender query failure")
	errExecutorSendersIterate = errors.New("forced active-sender iteration failure")
)

const (
	executorRowsChannelID = "44444444-4444-4444-4444-444444444444"
	executorRowsSenderID  = "55555555-5555-5555-5555-555555555555"
)

var executorRowsEventAt = time.Unix(300, 0).UTC()

type executorRowsDriver struct{}

func (executorRowsDriver) Open(scenario string) (driver.Conn, error) {
	return &executorRowsConn{scenario: scenario}, nil
}

type executorRowsConn struct{ scenario string }

func (*executorRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (*executorRowsConn) Close() error { return nil }

func (*executorRowsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *executorRowsConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "SELECT DISTINCT c.id") {
		if c.scenario == "channels_query_error" {
			return nil, errExecutorChannelsQuery
		}
		return newExecutorChannelRows(c.scenario), nil
	}
	if c.scenario == "senders_query_error" {
		return nil, errExecutorSendersQuery
	}
	return newExecutorSenderRows(c.scenario), nil
}

type scriptedRows struct {
	columns     []string
	rows        [][]driver.Value
	index       int
	terminalErr error
}

func (r *scriptedRows) Columns() []string { return r.columns }

func (*scriptedRows) Close() error { return nil }

func (r *scriptedRows) Next(values []driver.Value) error {
	if r.index >= len(r.rows) {
		if r.terminalErr != nil {
			return r.terminalErr
		}
		return io.EOF
	}
	copy(values, r.rows[r.index])
	r.index++
	return nil
}

func newExecutorChannelRows(scenario string) *scriptedRows {
	rows := &scriptedRows{
		columns: []string{"id"},
		rows:    [][]driver.Value{{executorRowsChannelID}},
	}
	switch scenario {
	case "channels_scan_error":
		// database/sql refuses to convert a NULL into the scanned string.
		rows.rows = [][]driver.Value{{nil}}
	case "channels_iteration_error":
		rows.terminalErr = errExecutorChannelsIter
	case "no_active_channels":
		rows.rows = nil
	}
	return rows
}

func newExecutorSenderRows(scenario string) *scriptedRows {
	rows := &scriptedRows{
		columns: []string{"user_id", "channel_id", "lifecycle_event_at"},
		rows: [][]driver.Value{
			{executorRowsSenderID, executorRowsChannelID, executorRowsEventAt},
		},
	}
	switch scenario {
	case "senders_scan_error":
		rows.rows = [][]driver.Value{
			{"not-a-uuid", executorRowsChannelID, executorRowsEventAt},
		}
	case "senders_iteration_error":
		rows.terminalErr = errExecutorSendersIterate
	case "no_active_senders":
		rows.rows = nil
	}
	return rows
}

func openExecutorRowsDB(t *testing.T, scenario string) *sql.DB {
	t.Helper()
	executorRowsDriverOnce.Do(func() {
		sql.Register(executorRowsDriverName, executorRowsDriver{})
	})
	db, err := sql.Open(executorRowsDriverName, scenario)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func newRowsExecutor(t *testing.T, scenario string) *Executor {
	t.Helper()
	executor := NewExecutor(
		openExecutorRowsDB(t, scenario), &refresherStub{}, &visibilityStub{},
		nil, &disconnectorStub{}, testLogger(),
	)
	t.Cleanup(executor.Close)
	return executor
}

func TestExecutorRows_PrepareCapture_NilChannels_EnumeratesActiveVoiceChannels(t *testing.T) {
	executor := newRowsExecutor(t, "resolved")

	plan, err := executor.PrepareCapture(
		context.Background(), uuid.New().String(), nil, nil,
	)

	require.NoError(t, err)
	typed, ok := plan.(*Plan)
	require.True(t, ok)
	require.Len(t, typed.Senders, 1,
		"nil channelIDs means every active voice channel in the server")
	sender := typed.Senders[0]
	assert.Equal(t, uuid.MustParse(executorRowsSenderID), sender.SenderID)
	assert.Equal(t, executorRowsChannelID, sender.ChannelID)
	assert.Equal(t, uuid.MustParse(executorRowsChannelID), sender.Scope.RoomID)
	assert.True(t, sender.Scope.EventAt.Equal(executorRowsEventAt),
		"the scope carries the committed lifecycle_event_at")
	assert.Empty(t, sender.OldAudience,
		"phase 1 never fills the audience; only CaptureVisibility does")
	assert.False(t, plan.HasWork(),
		"phase-1 candidates alone are not work")
}

func TestExecutorRows_PrepareCapture_ExplicitChannels_SkipTheEnumerationRead(t *testing.T) {
	// The enumeration query would fail under this scenario, so a clean result
	// proves an explicit channel list never issues it.
	executor := newRowsExecutor(t, "channels_query_error")

	plan, err := executor.PrepareCapture(
		context.Background(), uuid.New().String(),
		[]string{executorRowsChannelID}, nil,
	)

	require.NoError(t, err)
	require.NotNil(t, plan)
}

func TestExecutorRows_PrepareCapture_ReadFailures_FailBeforeAnyTransaction(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   string
		channelIDs []string
		message    string
		wrapped    error
	}{
		{
			name:     "channel enumeration query",
			scenario: "channels_query_error",
			message:  "enumerate server voice channels",
			wrapped:  errExecutorChannelsQuery,
		},
		{
			name:     "channel enumeration scan",
			scenario: "channels_scan_error",
			message:  "scan server voice channel",
		},
		{
			name:     "channel enumeration iteration",
			scenario: "channels_iteration_error",
			message:  "iterate server voice channels",
			wrapped:  errExecutorChannelsIter,
		},
		{
			name:       "active-sender query",
			scenario:   "senders_query_error",
			channelIDs: []string{executorRowsChannelID},
			message:    "enumerate active voice senders",
			wrapped:    errExecutorSendersQuery,
		},
		{
			name:       "active-sender scan",
			scenario:   "senders_scan_error",
			channelIDs: []string{executorRowsChannelID},
			message:    "scan active voice sender",
		},
		{
			name:       "active-sender iteration",
			scenario:   "senders_iteration_error",
			channelIDs: []string{executorRowsChannelID},
			message:    "iterate active voice senders",
			wrapped:    errExecutorSendersIterate,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := newRowsExecutor(t, test.scenario)

			plan, err := executor.PrepareCapture(
				context.Background(), uuid.New().String(), test.channelIDs, nil,
			)

			require.Error(t, err,
				"a phase-1 failure must reach the caller before BeginTx, so nothing is written")
			assert.Nil(t, plan)
			assert.ErrorContains(t, err, test.message)
			if test.wrapped != nil {
				assert.ErrorIs(t, err, test.wrapped)
			}
		})
	}
}

func TestExecutorRows_PrepareCapture_NoActiveVoice_IsTheBenignEmptyPlan(t *testing.T) {
	t.Run("no active channels", func(t *testing.T) {
		executor := newRowsExecutor(t, "no_active_channels")

		plan, err := executor.PrepareCapture(
			context.Background(), uuid.New().String(), nil, nil,
		)

		require.NoError(t, err)
		assert.False(t, plan.HasWork())
	})

	t.Run("channel with no active senders", func(t *testing.T) {
		executor := newRowsExecutor(t, "no_active_senders")

		plan, err := executor.PrepareCapture(
			context.Background(), uuid.New().String(),
			[]string{executorRowsChannelID}, nil,
		)

		require.NoError(t, err)
		assert.False(t, plan.HasWork())
	})
}

// foreignPlan is a PresenceRecheckPlan this executor did not produce. Every
// entry point must treat it as a no-op rather than panicking on the type
// assertion.
type foreignPlan struct{}

func (foreignPlan) HasWork() bool { return true }

func TestExecutorRows_ForeignPlan_IsANoOpAtEveryEntryPoint(t *testing.T) {
	refresher := &refresherStub{}
	disconnector := &disconnectorStub{}
	visibility := &visibilityStub{}
	executor := NewExecutor(
		nil, refresher, visibility, nil, disconnector, testLogger(),
	)
	defer executor.Close()
	var plan rbac.PresenceRecheckPlan = foreignPlan{}

	require.NoError(t, executor.CaptureVisibility(context.Background(), nil, plan))
	executor.Execute(plan)
	executor.Abandon(plan, "ambiguous_commit")

	assert.Empty(t, visibility.calls)
	assert.Empty(t, disconnector.sets)
	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	assert.Empty(t, refresher.calls)
}

func TestExecutorRows_CaptureVisibility_OnlyUserOutsideCandidates_IssuesNoQuery(t *testing.T) {
	visibility := &visibilityStub{}
	executor := NewExecutor(
		nil, &refresherStub{}, visibility, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()
	channelID := uuid.New()
	unrelated := uuid.New().String()

	plan := &Plan{
		ServerID:   uuid.New().String(),
		OnlyUserID: &unrelated,
		Senders: []SenderCapture{{
			SenderID: uuid.New(), ChannelID: channelID.String(), Scope: scopeFor(channelID),
			Candidates:  map[uuid.UUID]bool{uuid.New(): true},
			OldAudience: map[uuid.UUID]bool{},
		}},
	}

	require.NoError(t, executor.CaptureVisibility(context.Background(), nil, plan))

	assert.Empty(t, visibility.calls,
		"a bounded filter input that is empty needs no visibility query at all")
	assert.True(t, plan.HasWork(), "candidate-bearing sender with empty captured audience still needs refresh")
}

func TestExecutorRows_CaptureVisibility_UnparseableVisibleID_IsSkipped(t *testing.T) {
	candidate := uuid.New()
	visibility := &visibilityStub{visible: []string{"not-a-uuid", candidate.String()}}
	executor := NewExecutor(
		nil, &refresherStub{}, visibility, nil, &disconnectorStub{}, testLogger(),
	)
	defer executor.Close()
	channelID := uuid.New()

	plan := &Plan{ServerID: uuid.New().String(), Senders: []SenderCapture{{
		SenderID: uuid.New(), ChannelID: channelID.String(), Scope: scopeFor(channelID),
		Candidates:  map[uuid.UUID]bool{candidate: true},
		OldAudience: map[uuid.UUID]bool{},
	}}}

	require.NoError(t, executor.CaptureVisibility(context.Background(), nil, plan))

	assert.Equal(t, map[uuid.UUID]bool{candidate: true}, plan.Senders[0].OldAudience,
		"an unparseable id is dropped without discarding the rest of the filter result")
}

// A generic refresher error must disconnect the captured audience.
//
// This test asserted the opposite until #2445 review: it used assert.Never to
// lock "logs and must NOT disconnect", on the claim that every
// non-ErrRecheckSenderNotCurrent terminal had already failed closed inside
// internal/presence. That claim is false for the two classes that return before
// refreshAlreadyGated runs — validateActivityServiceCall, and a
// coordinator.WithSender gate-acquisition ctx.Err() — so the branch was
// fail-OPEN and the test held it that way. Do not re-invert it.
func TestExecutorRows_Dispatch_GenericRefreshFailure_DisconnectsCapturedAudience(t *testing.T) {
	senderID := uuid.New()
	viewer := uuid.New()
	refresher := &refresherStub{
		done: make(chan struct{}, 2),
		errs: map[uuid.UUID]error{senderID: errors.New("audience rebuild failed")},
	}
	disconnector := &disconnectorStub{}
	executor := newTestExecutor(refresher, disconnector)
	defer executor.Close()

	executor.Execute(planWith(senderID, viewer))

	select {
	case <-refresher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh was never dispatched")
	}
	assert.Eventually(t, func() bool {
		disconnector.mu.Lock()
		defer disconnector.mu.Unlock()
		return len(disconnector.sets) == 1
	}, 2*time.Second, 20*time.Millisecond,
		"a generic refresh failure must disconnect the captured audience, not log and continue")

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	assert.Equal(t, map[uuid.UUID]bool{viewer: true}, disconnector.sets[0],
		"the disconnect is viewer-scoped to exactly the captured audience")
	assert.Zero(t, disconnector.allCalls,
		"a per-sender refresh failure never escalates to a global disconnect")
}

// A gate-acquisition timeout is the specific class that motivated the fix
// above: coordinator.WithSender returns ctx.Err() from its select, so work()
// never runs and nothing inside internal/presence has failed closed.
func TestExecutorRows_Dispatch_ContextDeadline_DisconnectsCapturedAudience(t *testing.T) {
	senderID := uuid.New()
	viewer := uuid.New()
	refresher := &refresherStub{
		done: make(chan struct{}, 2),
		errs: map[uuid.UUID]error{senderID: context.DeadlineExceeded},
	}
	disconnector := &disconnectorStub{}
	executor := newTestExecutor(refresher, disconnector)
	defer executor.Close()

	executor.Execute(planWith(senderID, viewer))

	select {
	case <-refresher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh was never dispatched")
	}
	assert.Eventually(t, func() bool {
		disconnector.mu.Lock()
		defer disconnector.mu.Unlock()
		return len(disconnector.sets) == 1
	}, 2*time.Second, 20*time.Millisecond,
		"a sender-gate deadline must disconnect the captured audience")
}

func TestExecutorRows_Dispatch_SkipsSendersWithNoCandidates(t *testing.T) {
	captured := uuid.New()
	skipped := uuid.New()
	refresher := &refresherStub{done: make(chan struct{}, 2)}
	executor := newTestExecutor(refresher, &disconnectorStub{})
	defer executor.Close()
	channelID := uuid.New()

	executor.Execute(&Plan{Senders: []SenderCapture{
		{SenderID: skipped, Scope: scopeFor(channelID), Candidates: map[uuid.UUID]bool{}, OldAudience: map[uuid.UUID]bool{}},
		{
			SenderID: captured, Scope: scopeFor(channelID),
			Candidates:  map[uuid.UUID]bool{uuid.New(): true},
			OldAudience: map[uuid.UUID]bool{uuid.New(): true},
		},
	}})

	select {
	case <-refresher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh was never dispatched")
	}
	require.Eventually(t, func() bool {
		refresher.mu.Lock()
		defer refresher.mu.Unlock()
		return len(refresher.calls) == 1
	}, 2*time.Second, 10*time.Millisecond)

	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	assert.Equal(t, []uuid.UUID{captured}, refresher.calls,
		"a sender with no candidates has nothing to reconcile")
}

func TestExecutorRows_Disconnect_EmptyRecipientSet_NeverTouchesTheHub(t *testing.T) {
	disconnector := &disconnectorStub{err: errors.New("hub unavailable")}
	executor := newTestExecutor(&refresherStub{}, disconnector)
	defer executor.Close()

	executor.disconnect(nil, "dispatch_unavailable")
	executor.disconnect(map[uuid.UUID]bool{}, "dispatch_unavailable")

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	assert.Empty(t, disconnector.sets,
		"an empty capture has nobody to clear, so it must not reach the hub")
	assert.Zero(t, disconnector.allCalls,
		"and it must never escalate to the replica-wide terminal")
}

// blockingRefresher parks the single lifecycle worker inside one dispatch so the
// queue's saturation and shutdown-drain behavior become deterministic.
type blockingRefresher struct {
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls map[uuid.UUID]bool
}

func newBlockingRefresher() *blockingRefresher {
	return &blockingRefresher{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		calls:   make(map[uuid.UUID]bool),
	}
}

func (b *blockingRefresher) RefreshServerVoiceRecheck(
	_ context.Context, senderID uuid.UUID, _ presence.Scope, _ map[uuid.UUID]bool,
) error {
	b.mu.Lock()
	first := len(b.calls) == 0
	b.calls[senderID] = true
	b.mu.Unlock()
	if first {
		b.entered <- struct{}{}
		<-b.release
	}
	return nil
}

func (b *blockingRefresher) sawSender(senderID uuid.UUID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[senderID]
}

func TestExecutorRows_Execute_SaturatedQueue_FailsClosed(t *testing.T) {
	refresher := newBlockingRefresher()
	disconnector := &disconnectorStub{}
	executor := NewExecutor(nil, refresher, nil, nil, disconnector, testLogger())
	defer func() {
		close(refresher.release)
		executor.Close()
	}()

	// Park the worker so it cannot drain, then fill the buffer exactly.
	executor.Execute(planWith(uuid.New(), uuid.New()))
	select {
	case <-refresher.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never entered a dispatch")
	}
	for i := 0; i < presenceDispatchQueue; i++ {
		executor.Execute(planWith(uuid.New(), uuid.New()))
	}
	disconnector.mu.Lock()
	require.Empty(t, disconnector.sets,
		"a buffered plan is a reachable dispatch and must not be abandoned")
	disconnector.mu.Unlock()

	overflowViewer := uuid.New()
	executor.Execute(planWith(uuid.New(), overflowViewer))

	disconnector.mu.Lock()
	defer disconnector.mu.Unlock()
	require.Len(t, disconnector.sets, 1,
		"an unreachable dispatch fails closed rather than dropping the capture (class 5)")
	assert.Equal(t, map[uuid.UUID]bool{overflowViewer: true}, disconnector.sets[0])
}

func TestExecutorRows_Close_NeverDropsAQueuedCapture(t *testing.T) {
	refresher := newBlockingRefresher()
	disconnector := &disconnectorStub{}
	executor := NewExecutor(nil, refresher, nil, nil, disconnector, testLogger())

	// Park the worker, queue further captures behind it, then close.
	executor.Execute(planWith(uuid.New(), uuid.New()))
	select {
	case <-refresher.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never entered a dispatch")
	}
	type queued struct {
		senderID uuid.UUID
		viewerID uuid.UUID
	}
	pending := make([]queued, 0, 5)
	for i := 0; i < 5; i++ {
		entry := queued{senderID: uuid.New(), viewerID: uuid.New()}
		pending = append(pending, entry)
		executor.Execute(planWith(entry.senderID, entry.viewerID))
	}

	executor.Close()
	executor.Close() // idempotent
	close(refresher.release)

	// Whichever way the worker interleaves shutdown against the buffer, every
	// queued capture must be either refreshed or disconnected. A silently dropped
	// plan is a captured audience that is never cleared — fail-open.
	for _, entry := range pending {
		require.Eventually(t, func() bool {
			if refresher.sawSender(entry.senderID) {
				return true
			}
			disconnector.mu.Lock()
			defer disconnector.mu.Unlock()
			for _, set := range disconnector.sets {
				if set[entry.viewerID] {
					return true
				}
			}
			return false
		}, 3*time.Second, 10*time.Millisecond,
			"a queued capture was neither refreshed nor abandoned")
	}
}
