package activepresence

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// A real buffer-1 gate, matching presencehistory's shape: acquire, and release
// only on the way out. Re-entry from inside the closure blocks forever.
type countingGate struct{ slot chan struct{} }

func newCountingGate() *countingGate {
	return &countingGate{slot: make(chan struct{}, 1)}
}

func (g *countingGate) WithSenders(
	ctx context.Context, _ []uuid.UUID, work func() error,
) error {
	select {
	case g.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.slot }()
	return work()
}

// blockingGate parks the caller at the gate until the test releases it, so a
// test can observe what the rail has ALREADY done by the time it waits. That is
// the only way to see the gates-before-BeginTx ordering from outside.
type blockingGate struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingGate() *blockingGate {
	return &blockingGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *blockingGate) WithSenders(_ context.Context, _ []uuid.UUID, work func() error) error {
	g.entered <- struct{}{}
	<-g.release
	return work()
}

var errWorkFailed = errors.New("work failed")

func wiredReconciler(db *sql.DB, deliverer Deliverer) *Reconciler {
	return NewReconciler(db, passthroughGate{}, &fakeStateReader{}, &recordingDeleter{}, deliverer, nil)
}

func TestWithGatedTxRejectsTooManySubjects(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	rail := NewRail(db, newCountingGate(), nil, nil)

	subjects := make([]uuid.UUID, maxActiveSubjects+1)
	for i := range subjects {
		subjects[i] = uuid.New()
	}

	err := rail.WithGatedTx(context.Background(), subjects, func(*sql.Tx) error {
		t.Error("work must not run once the bound is exceeded")
		return nil
	})
	require.ErrorIs(t, err, ErrTooManySubjects)
}

// WALL-CLOCK BOUNDED ON PURPOSE. Do not delete the timeout as noise.
//
// The failure this guards is a channel-gate re-entrancy deadlock, which is a
// LIVENESS failure with no data race in it. `go test -race` observes conflicting
// memory accesses and reports nothing here; without the bound the test would
// hang to the package timeout with no diagnostic. The bound turns a hang into a
// named failure.
func TestAlreadyGatedEntryPointsDoNotReEnterTheGate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	gate := newCountingGate()
	rail := NewRail(db, gate, nil, nil)

	done := make(chan error, 1)
	go func() {
		// Simulate the erasure path: already inside the subject's gate.
		done <- gate.WithSenders(context.Background(), []uuid.UUID{subject}, func() error {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := rail.DrainAlreadyGated(context.Background(), tx, subject); err != nil {
				return err
			}
			return tx.Commit()
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("DrainAlreadyGated re-entered the sender gate and deadlocked")
	}
}

// The other already-gated entry point, and the one internal/dm calls: it runs
// the full claim/resolve/deliver/ack terminal from INSIDE WithGatedTx's own
// closure, so a single WithSenders call in its path wedges group deletion
// permanently.
//
// WALL-CLOCK BOUNDED ON PURPOSE -- see the comment above; the race detector
// cannot see a liveness failure.
func TestCompleteAlreadyGatedDeliversWithoutReEnteringTheGate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	gate := newCountingGate()
	deliverer := &recordingDeliverer{}
	rail := NewRail(db, gate, wiredReconciler(db, deliverer), nil)
	plan := livePlan(subject)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- rail.WithGatedTx(ctx, []uuid.UUID{subject}, func(tx *sql.Tx) error {
			if err := rail.CapturePlansTx(ctx, tx, []Plan{plan}); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return rail.CompleteAlreadyGated(ctx, nil, []PlanKey{{
				SubjectID: subject, Category: plan.Category,
			}})
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("CompleteAlreadyGated re-entered the sender gate and deadlocked")
	}

	require.Equal(t, []PlanKey{{SubjectID: subject, Category: plan.Category}}, deliverer.clears)
	require.Zero(t, deliverer.disconnects,
		"the ordinary arm is proportional, never a fleet-wide disconnect")
	require.Zero(t, countPlans(t, db), "a delivered plan must be acknowledged")
}

func TestCompleteAlreadyGatedReportsARetainedPlanAsIncomplete(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))
	rail := NewRail(db, passthroughGate{}, NewReconciler(
		db, passthroughGate{}, &fakeStateReader{err: errors.New("state unavailable")},
		&recordingDeleter{}, &recordingDeliverer{}, nil,
	), nil)

	err := rail.CompleteAlreadyGated(context.Background(), nil, []PlanKey{{
		SubjectID: subject, Category: presence.CategoryPrivateCall,
	}})

	require.ErrorIs(t, err, ErrDeliveryIncomplete)
	require.Equal(t, 1, countPlans(t, db), "an incomplete plan must remain for retry")
}

func TestCompleteAlreadyGatedReportsAnAlreadyClaimedPlanAsIncomplete(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	insert(t, db, livePlan(subject))
	_, err := db.Exec(`
		UPDATE presence_active_pending_plans
		SET reconcile_after = clock_timestamp() + interval '1 minute'
		WHERE user_id = $1 AND category = 'private_call'`, subject)
	require.NoError(t, err)
	rail := NewRail(db, passthroughGate{}, wiredReconciler(db, &recordingDeliverer{}), nil)

	err = rail.CompleteAlreadyGated(context.Background(), nil, []PlanKey{{
		SubjectID: subject, Category: presence.CategoryPrivateCall,
	}})

	require.ErrorIs(t, err, ErrDeliveryIncomplete)
	require.Equal(t, 1, countPlans(t, db), "another replica's claim remains pending until acknowledged")
}

func TestWithGatedTxRollsBackWhenWorkFails(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	rail := NewRail(db, newCountingGate(), nil, nil)

	err := rail.WithGatedTx(context.Background(), []uuid.UUID{subject},
		func(tx *sql.Tx) error {
			require.NoError(t, rail.CapturePlansTx(context.Background(), tx, []Plan{{
				SubjectID: subject, Category: presence.CategoryPrivateCall,
				OperationID: uuid.New(), Resolution: ResolutionConservative,
				EventAt: time.Now(),
			}}))
			return errWorkFailed
		})
	require.ErrorIs(t, err, errWorkFailed)

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, subject,
	).Scan(&count))
	require.Zero(t, count, "a failed mutation must leave no plan behind")
}

// work must run with the gate HELD, not merely after the rail touched it. The
// probe is a non-blocking send: a free slot means the closure escaped the gate,
// which is the shape a "simplifying" refactor that calls work directly leaves.
func TestWithGatedTxRunsWorkInsideTheGate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	gate := newCountingGate()
	rail := NewRail(db, gate, nil, nil)

	err := rail.WithGatedTx(context.Background(), []uuid.UUID{uuid.New()},
		func(tx *sql.Tx) error {
			select {
			case gate.slot <- struct{}{}:
				<-gate.slot
				return errors.New("work ran with the sender gate free")
			default:
			}
			return tx.Commit()
		})
	require.NoError(t, err)
}

// Gates BEFORE BeginTx, observed rather than asserted by inspection.
//
// The rail is parked at the gate; if it had opened its transaction first,
// db.Stats().InUse would already be 1. Inverting the two lines is not a style
// regression -- it lets this path hold a row lock while waiting for a gate that
// a settings writer holds while waiting for that row, a cycle no database
// deadlock detector can break because half of it is a Go channel.
//
// WALL-CLOCK BOUNDED ON PURPOSE: every wait here is on a channel.
func TestWithGatedTxAcquiresTheGateBeforeOpeningTheTransaction(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	gate := newBlockingGate()
	rail := NewRail(db, gate, nil, nil)

	done := make(chan error, 1)
	go func() {
		done <- rail.WithGatedTx(context.Background(), []uuid.UUID{uuid.New()},
			func(tx *sql.Tx) error { return tx.Commit() })
	}()

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("WithGatedTx never reached the sender gate")
	}
	require.Zero(t, db.Stats().InUse,
		"a transaction was already open while the rail waited on the gate")

	close(gate.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("WithGatedTx never returned after the gate was released")
	}
}

type orderedGate struct {
	entered  chan struct{}
	release  chan struct{}
	released chan struct{}
}

func newOrderedGate() *orderedGate {
	return &orderedGate{
		entered:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		released: make(chan struct{}, 1),
	}
}

func (g *orderedGate) WithSenders(_ context.Context, _ []uuid.UUID, work func() error) error {
	g.entered <- struct{}{}
	<-g.release
	defer func() { g.released <- struct{}{} }()
	return work()
}

func TestWithGatedRevocationTxDoesNotOpenFenceWhileGateWaits(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	gate := newOrderedGate()
	rail := NewRail(db, gate, nil, nil)
	fenceOpened := make(chan struct{}, 1)

	done := make(chan error, 1)
	go func() {
		done <- rail.WithGatedRevocationTx(context.Background(), []uuid.UUID{uuid.New()}, func() func() {
			fenceOpened <- struct{}{}
			return func() {}
		}, func(tx *sql.Tx) error { return tx.Commit() })
	}()

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("WithGatedRevocationTx never reached the sender gate")
	}
	select {
	case <-fenceOpened:
		t.Fatal("revocation fence opened while waiting for the sender gate")
	default:
	}
	close(gate.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("WithGatedRevocationTx did not return after the gate was released")
	}
}

func TestWithGatedRevocationTxOrdersFenceTransactionAndGate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	gate := newOrderedGate()
	close(gate.release)
	rail := NewRail(db, gate, nil, nil)
	events := make(chan string, 3)

	err := rail.WithGatedRevocationTx(context.Background(), []uuid.UUID{uuid.New()}, func() func() {
		events <- "fence-open"
		return func() { events <- "fence-close" }
	}, func(tx *sql.Tx) error {
		events <- "work"
		require.Equal(t, 1, db.Stats().InUse, "work must run with the transaction open")
		return tx.Commit()
	})
	require.NoError(t, err)
	require.Equal(t, "fence-open", <-events)
	require.Equal(t, "work", <-events)
	require.Equal(t, "fence-close", <-events)
	select {
	case <-gate.released:
	case <-time.After(5 * time.Second):
		t.Fatal("sender gate was not released")
	}
}

func TestWithGatedRevocationTxRollsBackBeforeClosingFenceOnWorkError(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	gate := newOrderedGate()
	close(gate.release)
	rail := NewRail(db, gate, nil, nil)
	subject := dbtest.CreateUser(t, db)
	closedAfterRollback := make(chan bool, 1)

	err := rail.WithGatedRevocationTx(context.Background(), []uuid.UUID{subject}, func() func() {
		return func() {
			var count int
			err := db.QueryRow(`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`, subject).Scan(&count)
			closedAfterRollback <- err == nil && count == 0
		}
	}, func(tx *sql.Tx) error {
		require.NoError(t, rail.CapturePlansTx(context.Background(), tx, []Plan{livePlan(subject)}))
		return errWorkFailed
	})
	require.ErrorIs(t, err, errWorkFailed)
	require.True(t, <-closedAfterRollback, "fence must close after the failed transaction rolls back")
	select {
	case <-gate.released:
	case <-time.After(5 * time.Second):
		t.Fatal("sender gate was not released after work failed")
	}
}

func TestWithGatedRevocationTxRefusesNilFenceWithoutRunningWork(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	called := false
	err := NewRail(db, newCountingGate(), nil, nil).WithGatedRevocationTx(context.Background(), []uuid.UUID{uuid.New()}, nil,
		func(*sql.Tx) error {
			called = true
			return nil
		})
	require.ErrorContains(t, err, "revocation fence is unavailable")
	require.NotErrorIs(t, err, ErrRailNotWired)
	require.False(t, called, "nil fence must fail closed before work")
}

// Every claim in one completion batch must inherit the same deadline. Giving
// each key a fresh claimTimeout would let a held sender gate grow linearly with
// the batch size.
func TestCompleteAlreadyGatedSharesOneClaimDeadlineAcrossBatch(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	reader := &deadlineRecordingReader{}
	rail := NewRail(db, passthroughGate{}, NewReconciler(
		db, passthroughGate{}, reader, &recordingDeleter{}, &recordingDeliverer{}, nil,
	), nil)
	subjects := []uuid.UUID{dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)}
	keys := make([]PlanKey, 0, len(subjects))

	for _, subject := range subjects {
		insert(t, db, conservativePlan(subject, CategoryPrivateCall))
		keys = append(keys, PlanKey{SubjectID: subject, Category: CategoryPrivateCall})
	}

	require.NoError(t, rail.CompleteAlreadyGated(context.Background(), nil, keys))
	require.Len(t, reader.deadlines, len(keys))
	require.True(t, reader.deadlines[0].Equal(reader.deadlines[1]),
		"every key must inherit the one batch claim deadline")
}

func TestWithGatedTxRefusesAnUnwiredRail(t *testing.T) {
	refuse := func(*sql.Tx) error {
		t.Error("work must not run on an unwired rail")
		return nil
	}
	subjects := []uuid.UUID{uuid.New()}

	var absent *Rail
	require.Error(t, absent.WithGatedTx(context.Background(), subjects, refuse))
	require.Error(t, NewRail(nil, newCountingGate(), nil, nil).
		WithGatedTx(context.Background(), subjects, refuse))
	// new(sql.DB) is never dereferenced: the nil-gate clause returns first.
	require.Error(t, NewRail(new(sql.DB), nil, nil, nil).
		WithGatedTx(context.Background(), subjects, refuse))
}

// Two overlapping captures must agree on acquisition order, so the ordering has
// to be a function of the SET and not of the caller's argument order.
func TestCanonicalSubjectsIsDeterministicDedupedAndNilFree(t *testing.T) {
	first, second, third := uuid.New(), uuid.New(), uuid.New()

	forward := canonicalSubjects([]uuid.UUID{first, second, third})
	reversed := canonicalSubjects([]uuid.UUID{third, second, first})
	require.Equal(t, forward, reversed, "two argument orders must acquire in one order")
	require.Len(t, forward, 3)
	for i := 1; i < len(forward); i++ {
		require.Less(t, forward[i-1].String(), forward[i].String(), "acquisition order is sorted")
	}

	require.Equal(t, []uuid.UUID{first}, canonicalSubjects([]uuid.UUID{first, first, first}),
		"a repeated subject must be acquired once, not twice")
	require.Empty(t, canonicalSubjects([]uuid.UUID{uuid.Nil, uuid.Nil}),
		"uuid.Nil is not a subject and must never reach a gate")
}

func TestCapturePlansTxRejectsMoreThanTheBound(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	rail := NewRail(db, newCountingGate(), nil, nil)

	plans := make([]Plan, maxActiveSubjects+1)
	for i := range plans {
		plans[i] = conservativePlan(subject, presence.CategoryPrivateCall)
	}

	tx := beginTx(t, db)
	require.ErrorIs(t, rail.CapturePlansTx(context.Background(), tx, plans), ErrTooManySubjects)
	require.NoError(t, tx.Commit())
	require.Zero(t, countPlans(t, db), "an over-bound capture writes nothing")
}

func TestCapturePlansTxWritesEveryObligation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	rail := NewRail(db, newCountingGate(), nil, nil)

	tx := beginTx(t, db)
	require.NoError(t, rail.CapturePlansTx(context.Background(), tx, []Plan{
		conservativePlan(subject, presence.CategoryPrivateCall),
		conservativePlan(subject, presence.CategoryServerVoice),
	}))
	require.NoError(t, tx.Commit())
	require.Equal(t, 2, countPlans(t, db))
}

func TestCapturePlansTxRefusesAnInvalidPlan(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	rail := NewRail(db, newCountingGate(), nil, nil)

	tx := beginTx(t, db)
	require.ErrorIs(t, rail.CapturePlansTx(context.Background(), tx, []Plan{{}}), ErrInvalidPlan)
}

func TestPrivateCallAlreadyGatedCaptureWritesOneConservativePlanPerSubject(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subjects := []uuid.UUID{dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)}
	r := NewRail(db, newCountingGate(), nil, nil)

	tx := beginTx(t, db)
	require.NoError(t, r.CapturePrivateCallPlansAlreadyGated(context.Background(), tx, subjects))
	require.NoError(t, tx.Commit())

	require.Equal(t, 2, countPlans(t, db))
	for _, subject := range subjects {
		var category, resolution string
		var lifecycle *uuid.UUID
		err := db.QueryRow(`
			SELECT category, resolution, scope_lifecycle_id
			FROM presence_active_pending_plans WHERE user_id = $1`, subject).
			Scan(&category, &resolution, &lifecycle)
		require.NoError(t, err)
		require.Equal(t, "private_call", category)
		require.Equal(t, "conservative", resolution)
		require.Nil(t, lifecycle)
	}
}

func TestPrivateCallAlreadyGatedCaptureRejectsMoreThanTheBound(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subjects := make([]uuid.UUID, maxActiveSubjects+1)
	for i := range subjects {
		subjects[i] = uuid.New()
	}
	r := NewRail(db, newCountingGate(), nil, nil)

	tx := beginTx(t, db)
	require.ErrorIs(t,
		r.CapturePrivateCallPlansAlreadyGated(context.Background(), tx, subjects),
		ErrTooManySubjects)
	require.NoError(t, tx.Commit())
	require.Zero(t, countPlans(t, db), "an over-bound capture writes nothing")
}

func TestPrivateCallAlreadyGatedCompletionDoesNotReEnterTheGate(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	gate := newCountingGate()
	deliverer := &recordingDeliverer{}
	r := NewRail(db, gate, wiredReconciler(db, deliverer), nil)

	done := make(chan error, 1)
	go func() {
		done <- gate.WithSenders(context.Background(), []uuid.UUID{subject}, func() error {
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				return err
			}
			if err := r.CapturePrivateCallPlansAlreadyGated(context.Background(), tx, []uuid.UUID{subject}); err != nil {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					return errors.Join(err, rollbackErr)
				}
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return r.CompletePrivateCallPlansAlreadyGated(context.Background(), []uuid.UUID{subject})
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("CompletePrivateCallPlansAlreadyGated re-entered the sender gate and deadlocked")
	}
}

func TestPrivateCallAlreadyGatedCompletionAttemptsEverySubject(t *testing.T) {
	closed, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	r := NewRail(closed, passthroughGate{}, wiredReconciler(closed, &recordingDeliverer{}), nil)

	err = r.CompletePrivateCallPlansAlreadyGated(context.Background(), []uuid.UUID{uuid.New(), uuid.New()})
	require.Error(t, err)
	joined, ok := err.(interface{ Unwrap() []error })
	require.True(t, ok, "failures must be joined, not replaced")
	require.Len(t, joined.Unwrap(), 2, "every subject's failure is reported")
}

// An unwired rail must FAIL the mutation rather than report a discharged
// obligation it never delivered.
func TestCompleteAlreadyGatedRefusesAnUnwiredRail(t *testing.T) {
	var absent *Rail
	require.Error(t, absent.CompleteAlreadyGated(context.Background(), nil, nil))
	require.Error(t, NewRail(nil, passthroughGate{}, nil, nil).
		CompleteAlreadyGated(context.Background(), nil, nil))
}

// Every key is attempted and every failure is reported. An early return would
// hide the second subject's undelivered clear behind the first one's error.
func TestCompleteAlreadyGatedJoinsEveryKeysFailure(t *testing.T) {
	// Failure is injected with a CLOSED handle, not a cancelled context.
	//
	// This test previously cancelled the context to make both keys fail. That
	// stopped working once CompleteAlreadyGated started detaching via
	// context.WithoutCancel -- which is the point of the detachment: a request
	// cancelled after the caller's commit must NOT abandon the retraction it
	// owes. The cancellation was only ever the injector; the subject of this
	// test is that every key's failure is joined rather than the first one
	// returned. A closed handle fails BeginTx immediately for both keys and
	// keeps that subject intact.
	closed, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	rail := NewRail(closed, passthroughGate{}, wiredReconciler(closed, &recordingDeliverer{}), nil)

	err = rail.CompleteAlreadyGated(context.Background(), nil, []PlanKey{
		{SubjectID: uuid.New(), Category: presence.CategoryPrivateCall},
		{SubjectID: uuid.New(), Category: presence.CategoryServerVoice},
	})
	require.Error(t, err)
	joined, ok := err.(interface{ Unwrap() []error })
	require.True(t, ok, "failures must be joined, not replaced")
	require.Len(t, joined.Unwrap(), 2, "every key's failure is reported, not just the first")
}

func TestDrainAlreadyGatedReportsEveryCategory(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	rail := NewRail(db, newCountingGate(), nil, nil)
	ctx := context.Background()

	require.NoError(t, rail.WithGatedTx(ctx, []uuid.UUID{subject}, func(tx *sql.Tx) error {
		if err := rail.CapturePlansTx(ctx, tx, []Plan{
			conservativePlan(subject, presence.CategoryPrivateCall),
			conservativePlan(subject, presence.CategoryServerVoice),
		}); err != nil {
			return err
		}
		return tx.Commit()
	}))

	tx := beginTx(t, db)
	drained, err := rail.DrainAlreadyGated(ctx, tx, subject)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.ElementsMatch(t,
		[]presence.Category{presence.CategoryPrivateCall, presence.CategoryServerVoice}, drained)
	require.Zero(t, countPlans(t, db), "the drain must remove the erased subject's rows")
}

// The post-commit half of the erasure drain. One proportional frame per drained
// category -- never the disconnect sink, which erasure would otherwise turn
// into a fleet-wide outage primitive.
func TestClearDrainedEmitsOneProportionalFramePerCategory(t *testing.T) {
	deliverer := &recordingDeliverer{}
	rail := NewRail(nil, passthroughGate{}, wiredReconciler(nil, deliverer), nil)
	subject := uuid.New()

	rail.ClearDrained(context.Background(), subject, []presence.Category{
		presence.CategoryServerVoice, presence.CategoryPrivateCall,
	})

	require.Equal(t, []PlanKey{
		{SubjectID: subject, Category: presence.CategoryServerVoice},
		{SubjectID: subject, Category: presence.CategoryPrivateCall},
	}, deliverer.clears)
	require.Zero(t, deliverer.disconnects)
}

// Best effort by design: a presence terminal that is not wired must never fail
// an account erasure, and must never panic inside one.
func TestClearDrainedIsANoOpWhenTheTerminalIsAbsent(t *testing.T) {
	categories := []presence.Category{presence.CategoryPrivateCall}
	subject := uuid.New()

	var absent *Rail
	require.NotPanics(t, func() { absent.ClearDrained(context.Background(), subject, categories) })
	require.NotPanics(t, func() {
		NewRail(nil, passthroughGate{}, nil, nil).ClearDrained(context.Background(), subject, categories)
	})
	require.NotPanics(t, func() {
		NewRail(nil, passthroughGate{}, NewReconciler(nil, passthroughGate{}, nil, nil, nil, nil), nil).
			ClearDrained(context.Background(), subject, categories)
	})
}

// The boot guard's whole reason to exist. NewReconciler never returns nil, so a
// nil check on the constructed value is a tautology that still boots with the
// wiring line deleted -- this must report whether the TERMINAL actually landed.
func TestHasReconcilerReportsRealTerminalWiring(t *testing.T) {
	var absent *Rail
	require.False(t, absent.HasReconciler(), "a nil rail cannot deliver")
	require.False(t, NewRail(nil, passthroughGate{}, nil, nil).HasReconciler(),
		"a rail with no reconciler cannot deliver")

	unwired := NewReconciler(nil, passthroughGate{}, nil, nil, nil, nil)
	require.NotNil(t, unwired, "NewReconciler never returns nil -- that is the tautology to avoid")
	require.False(t, NewRail(nil, passthroughGate{}, unwired, nil).HasReconciler(),
		"a constructed reconciler with no terminal must not report itself wired")

	require.True(t, NewRail(nil, passthroughGate{}, wiredReconciler(nil, &recordingDeliverer{}), nil).
		HasReconciler())
}
