package activepresence

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// deadlineRecordingReader captures whether the context reaching the resolver
// carries a deadline. Asserting the deadline EXISTS is the right test: waiting
// for claimTimeout to actually fire would cost ten seconds and prove the same
// thing more slowly.
type deadlineRecordingReader struct {
	sawDeadline bool
	deadline    time.Time
	deadlines   []time.Time
	called      bool
}

func (d *deadlineRecordingReader) GetWithLease(
	ctx context.Context, _ uuid.UUID, _ presence.Category,
) (presence.ActivityState, bool, error) {
	d.called = true
	d.deadline, d.sawDeadline = ctx.Deadline()
	if d.sawDeadline {
		d.deadlines = append(d.deadlines, d.deadline)
	}
	return presence.ActivityState{}, false, nil
}

// The transactional segment must be time-bounded even when the caller's context
// has no deadline of its own.
//
// Review finding on the detachment fix: CompleteAlreadyGated calls
// context.WithoutCancel, which strips the deadline as well as cancellation. The
// claim and the resolver's Redis read then ran with no bound at all — while the
// caller held its sender gate, a buffer-1 channel. One hung dependency pinned
// that stripe indefinitely, stalling every sender that hashed to it. The blast
// radius is the stripe rather than the one request, which is what makes this a
// liveness bug and not a slow call.
func TestClaimAndResolveAreTimeBoundedWithoutACallerDeadline(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	// A real users row: the FK is ON DELETE RESTRICT, so an invented UUID is
	// rejected at insert rather than reaching the code under test.
	subject := dbtest.CreateUser(t, db)
	insert(t, db, conservativePlan(subject, presence.CategoryPrivateCall))

	reader := &deadlineRecordingReader{}
	r := NewReconciler(db, passthroughGate{}, reader, &recordingDeleter{}, &recordingDeliverer{}, nil)

	// A context with NO deadline — exactly what WithoutCancel produces.
	ctx := context.Background()
	_, hasDeadline := ctx.Deadline()
	require.False(t, hasDeadline, "fixture must start from an unbounded context")

	var pass passState
	_, err := r.resolveOneAlreadyGated(ctx, PlanKey{
		SubjectID: subject, Category: presence.CategoryPrivateCall,
	}, &pass)
	require.NoError(t, err)

	require.True(t, reader.called, "the resolver must have been reached")
	require.True(t, reader.sawDeadline,
		"the claim/resolve segment must carry its own deadline: an unbounded Redis read holds the sender gate")
	require.LessOrEqual(t, time.Until(reader.deadline), claimTimeout,
		"the deadline must come from claimTimeout, not from somewhere looser")
}
