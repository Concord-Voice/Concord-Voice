package activepresence

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
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
	mu        sync.Mutex
	calls     int
	sawCancel bool
}

func (d *ctxSensitiveDeleter) CompareAndDelete(
	ctx context.Context, _ uuid.UUID, _ presence.Category, _ uuid.UUID, _ int64,
) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if err := ctx.Err(); err != nil {
		d.sawCancel = true
		return false, err
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
	ok := r.deliver(ctx, plan, decision, &pass)

	require.Equal(t, 1, deleter.calls, "step (a) must run")
	require.False(t, deleter.sawCancel,
		"the generation delete must receive a LIVE context: reconciliation outlives the request that triggered it")
	require.True(t, ok, "delivery must succeed despite the cancelled request context")
	require.Len(t, deliverer.clears, 1, "the clear frame must still be emitted")
	require.Zero(t, deliverer.disconnects, "the ordinary path must never disconnect")
}
