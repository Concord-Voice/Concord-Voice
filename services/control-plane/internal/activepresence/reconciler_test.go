package activepresence

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// recordingDeliverer separates the two terminals so a test can prove which one
// ran. Counting only "was something delivered" would let the ordinary path be
// re-routed through the disconnect sink without a single assertion noticing.
type recordingDeliverer struct {
	mu             sync.Mutex
	clears         []PlanKey
	disconnects    int
	disconnectFail error
}

func (d *recordingDeliverer) ClearSenderActiveCategory(id uuid.UUID, c presence.Category) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clears = append(d.clears, PlanKey{SubjectID: id, Category: c})
}

func (d *recordingDeliverer) DisconnectAllRichPresenceClients(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnects++
	return d.disconnectFail
}

// generationDelete records one step-(a) call ARGUMENT-BY-ARGUMENT. A bare call
// count would not catch a terminal that deletes the wrong generation, which is
// the one mistake that destroys a live successor's presence.
type generationDelete struct {
	subject  uuid.UUID
	category presence.Category
	token    uuid.UUID
	version  int64
}

type recordingDeleter struct {
	mu    sync.Mutex
	calls []generationDelete
	err   error
}

func (d *recordingDeleter) CompareAndDelete(
	_ context.Context, subject uuid.UUID, category presence.Category,
	token uuid.UUID, version int64,
) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, generationDelete{subject, category, token, version})
	return d.err == nil, d.err
}

func (d *recordingDeleter) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

type passthroughGate struct{}

func (passthroughGate) WithSenders(_ context.Context, _ []uuid.UUID, work func() error) error {
	return work()
}

// serializingGate reproduces presencehistory's real gate shape -- a buffer-1
// channel released by a deferred receive, with NO timeout -- so a reconciler
// that re-enters it deadlocks here exactly as it would in production.
type serializingGate struct {
	slot    chan struct{}
	mu      sync.Mutex
	entries int
}

func newSerializingGate() *serializingGate {
	return &serializingGate{slot: make(chan struct{}, 1)}
}

func (g *serializingGate) WithSenders(
	ctx context.Context, _ []uuid.UUID, work func() error,
) error {
	select {
	case g.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.slot }()
	g.mu.Lock()
	g.entries++
	g.mu.Unlock()
	return work()
}

func (g *serializingGate) entryCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entries
}

// countPlans reads the WHOLE table on purpose. SetupTestDB holds a per-database
// advisory lock and truncates on cleanup, so an unscoped count is safe here --
// and it catches an acknowledgement that deletes the wrong subject's row, which
// a WHERE user_id = $1 count would silently pass.
func countPlans(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans`).Scan(&count))
	return count
}

// livePlan is the ordinary producer shape: a conservative plan with no
// lifecycle evidence, which is what group deletion cuts.
func livePlan(subject uuid.UUID) Plan {
	return Plan{
		SubjectID:   subject,
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		Resolution:  ResolutionConservative,
		EventAt:     time.Now(),
	}
}

// THE SECURITY-CRITICAL TEST. The ordinary path -- a live stale generation
// belonging to a user-reachable deletion -- must clear exactly one subject and
// disconnect NOBODY.
//
// How it fails if someone routes the ordinary arm to the disconnect sink:
// deliverer.disconnects becomes 1 and deliverer.clears becomes empty, so both
// the Zero and the Len assertion trip. That is the #2840 posture enforced one
// layer above the hub, where DELETE /api/v1/dm/conversations/:id -- a route
// bounded only by RateLimitByUser (5/min/user) -- would otherwise hand any
// group-DM admin a repeatable fleet-wide
// denial-of-service primitive.
func TestReconcilePassClearsWithoutDisconnecting(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	token := uuid.New()
	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	reader := &fakeStateReader{
		state: presence.ActivityState{SourceToken: token, SourceVersion: 4},
		found: true,
	}
	r := NewReconciler(db, passthroughGate{}, reader, deleter, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Cleared)
	require.Zero(t, deliverer.disconnects, "the ordinary path must never disconnect")
	require.Len(t, deliverer.clears, 1)
	require.Equal(t, subject, deliverer.clears[0].SubjectID)
	require.Equal(t, presence.CategoryPrivateCall, deliverer.clears[0].Category)

	// Step (a) ran, and against the EXACT generation the resolver read.
	require.Len(t, deleter.calls, 1)
	require.Equal(t, generationDelete{
		subject: subject, category: presence.CategoryPrivateCall,
		token: token, version: 4,
	}, deleter.calls[0])

	require.Zero(t, countPlans(t, db), "a delivered plan is acknowledged by deletion")
}

// B5, absent-but-young: the clear frame still ships, but there is NO generation
// to delete. Step (a) must be skipped -- activityGenerationKey rejects
// uuid.Nil, so an ungated call reports a spurious generation_delete failure on
// every young-absent plan.
func TestReconcilePassClearsAYoungAbsentPlanWithoutAGenerationDelete(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	r := NewReconciler(db, passthroughGate{}, &fakeStateReader{}, deleter, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Cleared)
	require.Len(t, deliverer.clears, 1)
	require.Zero(t, deleter.callCount(),
		"a decision with no generation must not reach CompareAndDelete")
	require.Zero(t, deliverer.disconnects)
	require.Zero(t, countPlans(t, db))
}

// The step-(a) gate is Decision.HasGeneration(), NOT a bare token != uuid.Nil
// check. This state carries a token and a ZERO version, which the naive check
// admits and activityGenerationKey then rejects (sourceVersion <= 0).
func TestReconcilePassSkipsTheGenerationDeleteWithoutAVersion(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	reader := &fakeStateReader{
		state: presence.ActivityState{SourceToken: uuid.New(), SourceVersion: 0},
		found: true,
	}
	r := NewReconciler(db, passthroughGate{}, reader, deleter, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Cleared)
	require.Zero(t, deleter.callCount(),
		"a zero source version is not a generation the terminal may delete")
	require.Len(t, deliverer.clears, 1, "the clear frame ships regardless of step (a)")
	require.Zero(t, countPlans(t, db))
}

// The anomaly arm is coalesced: N unexpiring plans in one pass produce ONE
// disconnect, and every one of them is acknowledged off that single success.
func TestReconcilePassCoalescesTheDisconnectArm(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	for i := 0; i < 4; i++ {
		insert(t, db, livePlan(dbtest.CreateUser(t, db)))
	}

	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	reader := &fakeStateReader{err: presence.ErrUnexpiringActivityState}
	r := NewReconciler(db, passthroughGate{}, reader, deleter, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 4, stats.Disconnected)
	require.Equal(t, 1, deliverer.disconnects, "one disconnect per pass, never one per plan")
	require.Empty(t, deliverer.clears, "the anomaly arm does not also emit clear frames")
	require.Zero(t, deleter.callCount(),
		"B2 returns before decoding, so it carries no generation to delete")
	require.Zero(t, countPlans(t, db))
}

// The malformed arm reaches the same coalescer. Two anomaly classes in one
// pass still buy exactly one disconnect.
func TestReconcilePassCoalescesTheMalformedArmToo(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	insert(t, db, livePlan(dbtest.CreateUser(t, db)))
	insert(t, db, livePlan(dbtest.CreateUser(t, db)))

	deliverer := &recordingDeliverer{}
	reader := &fakeStateReader{err: presence.ErrMalformedActivityState}
	r := NewReconciler(db, passthroughGate{}, reader, &recordingDeleter{}, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Disconnected)
	require.Equal(t, 1, deliverer.disconnects)
	require.Zero(t, countPlans(t, db))
}

// A failed disconnect must NOT acknowledge, and must not be retried once per
// plan -- the coalescer latches the outcome, not just the attempt.
func TestReconcilePassDoesNotAckWhenTheDisconnectFails(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	for i := 0; i < 3; i++ {
		insert(t, db, livePlan(dbtest.CreateUser(t, db)))
	}

	deliverer := &recordingDeliverer{disconnectFail: errors.New("hub unavailable")}
	reader := &fakeStateReader{err: presence.ErrUnexpiringActivityState}
	r := NewReconciler(db, passthroughGate{}, reader, &recordingDeleter{}, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Zero(t, stats.Disconnected, "a failed terminal is never an acknowledgement")
	require.Equal(t, 1, deliverer.disconnects, "the failure is latched, not retried per plan")
	require.Equal(t, 3, countPlans(t, db), "every undelivered obligation survives")
}

// A superseded plan is acknowledged with NO delivery at all: no clear frame, no
// disconnect, and ZERO Redis writes. The generation in Redis belongs to a LIVE
// successor, and deleting it would kill a live call's presence.
func TestReconcilePassAcksSupersededWithoutDelivery(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := livePlan(subject)
	plan.Resolution = ResolutionExact
	plan.LifecycleID = uuid.New()
	insert(t, db, plan)

	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	reader := &fakeStateReader{
		state: presence.ActivityState{SourceToken: uuid.New(), SourceVersion: 12},
		found: true,
	}
	r := NewReconciler(db, passthroughGate{}, reader, deleter, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Acked)
	require.Zero(t, stats.Cleared)
	require.Empty(t, deliverer.clears)
	require.Zero(t, deliverer.disconnects)
	require.Zero(t, deleter.callCount(), "supersession performs zero Redis writes")
	require.Zero(t, countPlans(t, db))
}

// B1: absent AND past the 90-second level arm. Every viewer's copy has already
// expired, so the obligation is discharged by deletion alone.
func TestReconcilePassAcksAnExpiredAbsentPlanWithoutDelivery(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := livePlan(subject)
	plan.EventAt = time.Now().Add(-2 * presence.ActivityStateTTL)
	insert(t, db, plan)

	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	r := NewReconciler(db, passthroughGate{}, &fakeStateReader{}, deleter, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Acked)
	require.Empty(t, deliverer.clears)
	require.Zero(t, deliverer.disconnects)
	require.Zero(t, deleter.callCount())
	require.Zero(t, countPlans(t, db))
}

// B6: a read failure is RETAINED, never acknowledged. The row survives with its
// attempt counted and its class recorded, and nothing is delivered.
func TestReconcilePassRetainsAFailedReadWithoutAcking(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	deliverer := &recordingDeliverer{}
	reader := &fakeStateReader{err: errors.New("redis is unreachable")}
	r := NewReconciler(db, passthroughGate{}, reader, &recordingDeleter{}, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Retained)
	require.Zero(t, stats.Acked)
	require.Empty(t, deliverer.clears)
	require.Zero(t, deliverer.disconnects)

	var attempts int
	var class *string
	require.NoError(t, db.QueryRow(
		`SELECT attempts, failure_class FROM presence_active_pending_plans
		 WHERE user_id = $1`, subject).Scan(&attempts, &class))
	require.Equal(t, 1, attempts)
	require.NotNil(t, class)
	require.Equal(t, "state_read", *class)
}

// Quarantine is a SLOW RETRY, never a delete: the row is the evidence that an
// obligation is undischarged, and dropping it fails open.
func TestReconcilePassQuarantinesAtTheAttemptCeilingWithoutDeleting(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := livePlan(subject)
	plan.Attempts = maxPlanAttempts - 1
	insert(t, db, plan)

	deliverer := &recordingDeliverer{}
	reader := &fakeStateReader{err: errors.New("redis is unreachable")}
	r := NewReconciler(db, passthroughGate{}, reader, &recordingDeleter{}, deliverer, nil)

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Quarantined)
	require.Zero(t, stats.Retained)
	require.Empty(t, deliverer.clears)
	require.Equal(t, 1, countPlans(t, db), "a quarantined plan is never deleted")
}

// An unwired terminal fails CLOSED. It must not silently discover, claim and
// acknowledge plans it can never deliver.
func TestReconcilePassRefusesAnUnwiredTerminal(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	r := NewReconciler(db, passthroughGate{}, &fakeStateReader{}, &recordingDeleter{}, nil, nil)
	require.False(t, r.HasDeliverer())

	_, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.Error(t, err)
	require.Equal(t, 1, countPlans(t, db), "an unwired pass discharges nothing")
}

// The pass enters the sender gate ONCE per plan and never re-enters it.
//
// WALL-CLOCK BOUNDED ON PURPOSE. Do not delete the timeout as noise: the
// failure this guards is a channel-gate re-entrancy deadlock, a LIVENESS
// failure with no data race in it. `go test -race` observes conflicting memory
// accesses and would report nothing here; without the bound the test would hang
// to the package timeout with no diagnostic. The bound turns a hang into a
// named failure.
func TestReconcilePassDoesNotReEnterTheSenderGate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	gate := newSerializingGate()
	deliverer := &recordingDeliverer{}
	r := NewReconciler(db, gate, &fakeStateReader{}, &recordingDeleter{}, deliverer, nil)

	// The pass context is bounded so a re-entrant acquisition UNWINDS rather
	// than wedging the fixture. presencehistory's real gate selects on
	// ctx.Done() (operations.go:186-189), so this models it faithfully -- and
	// without the bound a deadlocked reconciler would keep its FOR UPDATE row
	// lock past t.Fatal, block the fixture's truncate cleanup, and swallow the
	// very diagnostic the wall-clock bound exists to print.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan PassStats, 1)
	go func() {
		stats, _ := r.ReconcilePass(ctx, maxPlanBatch)
		done <- stats
	}()

	select {
	case stats := <-done:
		require.Equal(t, 1, gate.entryCount(), "one gate acquisition per plan, never two")
		require.Equal(t, 1, stats.Cleared,
			"the pass never completed: a re-entrant gate acquisition blocked "+
				"until the context expired")
	case <-time.After(5 * time.Second):
		t.Fatal("the reconciliation pass re-entered the sender gate and deadlocked")
	}
}

// The ticker must stop on context cancellation and must not leak a goroutine.
//
// WALL-CLOCK BOUNDED ON PURPOSE. Do not delete the timeout as noise: a
// reconciler that never returns is a liveness bug, and `go test -race` cannot
// see a liveness bug -- the race detector observes memory races, so a blocked
// goroutine reports nothing and the test would simply hang to the package
// timeout with no diagnostic.
func TestRunStopsOnContextCancellation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	r := NewReconciler(db, passthroughGate{}, &fakeStateReader{}, &recordingDeleter{},
		&recordingDeliverer{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(stopped)
	}()
	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciler did not stop within its wall-clock bound")
	}
}

// The delivery switch's fail-closed default, exercised directly because Resolve
// cannot produce a value outside the closed vocabulary. A corrupted outcome
// must deliver NOTHING and must not authorize an acknowledgement -- the
// obligation survives for the next pass.
func TestDeliverRefusesAnOutcomeOutsideTheVocabulary(t *testing.T) {
	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{}
	r := NewReconciler(nil, passthroughGate{}, &fakeStateReader{}, deleter, deliverer, nil)

	pass := &passState{}
	acked := r.deliver(context.Background(), livePlan(uuid.New()),
		Decision{Outcome: Outcome(200)}, pass)

	require.False(t, acked, "an unrecognized outcome is never an acknowledgement")
	require.Empty(t, deliverer.clears)
	require.Zero(t, deliverer.disconnects)
	require.Zero(t, deleter.callCount())
	require.Equal(t, PassStats{}, pass.stats)
}

// Both log surfaces carry FIXED enums and aggregate counts and nothing else.
// The two NotContains assertions are the point: an untyped class field is
// exactly how a wrapped database error -- and therefore data -- ends up in a
// log line, and a per-plan line would carry the subject id.
func TestReconcilePassLogsAggregatesWithoutSubjectsOrErrors(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))

	var buf bytes.Buffer
	deliverer := &recordingDeliverer{}
	deleter := &recordingDeleter{err: errors.New("redis refused the compare-and-delete")}
	reader := &fakeStateReader{
		state: presence.ActivityState{SourceToken: uuid.New(), SourceVersion: 7},
		found: true,
	}
	r := NewReconciler(db, passthroughGate{}, reader, deleter, deliverer,
		logger.NewWithWriter(&buf))

	stats, err := r.ReconcilePass(context.Background(), maxPlanBatch)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Cleared, "a failed step (a) must not block the clear frame")

	out := buf.String()
	require.Contains(t, out, "cleared_count=1")
	require.Contains(t, out, "failure_class=generation_delete")
	require.NotContains(t, out, subject.String(),
		"a reconciliation log line must never carry a subject id")
	require.NotContains(t, out, "redis refused",
		"a wrapped database error must never reach a fixed class field")
}
