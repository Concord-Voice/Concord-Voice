package activepresence

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// ctxSensitiveDeleter fails when handed a cancelled context, which is what the
// real ActivityStore does — the Redis round trip carries the context.
//
// It exists because recordingDeleter takes `_ context.Context` and therefore
// CANNOT observe cancellation. A detachment test written against that fake
// passes whether or not the detachment exists: the first version of this test
// did exactly that, and a mutant removing context.WithoutCancel survived it.
// The fake has to be sensitive to the thing under test, or the assertion is
// decoration.
type ctxSensitiveDeleter struct {
	mu          sync.Mutex
	calls       int
	err         error
	deadline    time.Time
	sawDeadline bool
}

func (d *ctxSensitiveDeleter) CompareAndDelete(
	ctx context.Context, _ uuid.UUID, _ presence.Category, _ uuid.UUID, _ int64,
) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.deadline, d.sawDeadline = ctx.Deadline()
	d.err = ctx.Err()
	if d.err != nil {
		return false, d.err
	}
	return true, nil
}

// Cancelling the request must not cancel the reconciliation it already owes.
//
// This is an acceptance criterion of #2448, not a nicety. By the time delivery
// runs the mutation has COMMITTED: the conversation is gone, and the evidence a
// resolver would need to rebuild the obligation was destroyed by that same
// commit. A client that disconnects or times out mid-request would otherwise
// take the retraction down with it, leaving stale presence until the TTL.
//
// The mechanism is context.WithoutCancel in deliver. That is one call, trivially
// deleted by someone tidying context plumbing, so it gets a test that dies with it.
func TestDeliverDetachesFromACancelledRequestContext(t *testing.T) {
	deleter := &ctxSensitiveDeleter{}
	deliverer := &recordingDeliverer{}
	r := NewReconciler(nil, passthroughGate{}, &fakeStateReader{}, deleter, deliverer, nil)

	subject := uuid.New()
	plan := Plan{
		SubjectID:   subject,
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		Resolution:  ResolutionExact,
	}
	// A generation is required to reach step (a) at all — HasGeneration() gates it.
	decision := Decision{
		Outcome: OutcomeCleared,
		State:   presence.ActivityState{SourceToken: uuid.New(), SourceVersion: 7},
	}
	require.True(t, decision.HasGeneration(), "fixture must reach the generation delete")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the worst case a real request produces: already dead on entry

	var pass passState
	delivered := r.deliver(ctx, plan, decision, &pass)

	require.Equal(t, 1, deleter.calls, "step (a) must run")
	require.NotErrorIs(t, deleter.err, context.Canceled,
		"the generation delete must not inherit request cancellation")
	require.True(t, deleter.sawDeadline, "the detached terminal must remain time bounded")
	require.True(t, delivered, "delivery must succeed despite the cancelled request context")
	require.Len(t, deliverer.clears, 1, "the clear frame must still be emitted")
	require.Zero(t, deliverer.disconnects, "the ordinary path must never disconnect")
}

func TestDeliverPreservesAnExpiredBatchDeadline(t *testing.T) {
	deleter := &ctxSensitiveDeleter{}
	deliverer := &recordingDeliverer{}
	r := NewReconciler(nil, passthroughGate{}, &fakeStateReader{}, deleter, deliverer, nil)
	plan := Plan{
		SubjectID:   uuid.New(),
		Category:    presence.CategoryPrivateCall,
		OperationID: uuid.New(),
		Resolution:  ResolutionExact,
	}
	decision := Decision{
		Outcome: OutcomeCleared,
		State:   presence.ActivityState{SourceToken: uuid.New(), SourceVersion: 7},
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	wantDeadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	cancel()

	var pass passState
	delivered := r.deliver(ctx, plan, decision, &pass)

	require.Equal(t, 1, deleter.calls, "step (a) must run")
	require.ErrorIs(t, deleter.err, context.DeadlineExceeded,
		"detaching cancellation must preserve an already-expired batch deadline")
	require.True(t, deleter.sawDeadline)
	require.True(t, deleter.deadline.Equal(wantDeadline))
	require.True(t, delivered, "generation deletion is best effort; the clear frame must still ship")
	require.Len(t, deliverer.clears, 1)
}

func TestAcknowledgeDoesNotRefreshAnExpiredBatchDeadline(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	plan := livePlan(subject)
	insert(t, db, plan)
	r := NewReconciler(db, passthroughGate{}, &fakeStateReader{}, &recordingDeleter{},
		&recordingDeliverer{}, nil)

	batchCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := r.acknowledge(batchCtx, plan)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, countPlans(t, db),
		"an expired batch may not receive a fresh acknowledgement budget")
}
