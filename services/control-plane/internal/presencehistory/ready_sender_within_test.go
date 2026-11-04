package presencehistory

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testhelpers "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// WithReadySenderModeWithin (#3680 PR3, RT-8): the wait for the sender gate is bounded by
// wait as well as ctx. The gate is one of senderGateStripes stripes, so it may be held by
// another sender or by the caller. Only the acquire is bounded.

const (
	withinWait = 200 * time.Millisecond
	// holderLifetime caps how long a test's gate holder can live.
	holderLifetime = 30 * time.Second
	// withinSlack is how far above the wait a call's own context deadline sits. A call that
	// ignores the wait then returns the context error and fails an assertion in seconds,
	// instead of hanging to the package timeout.
	withinSlack = 5 * time.Second
)

// boundedCtx is a context that ends withinSlack after wait.
func boundedCtx(t *testing.T, wait time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait+withinSlack)
	t.Cleanup(cancel)
	return ctx
}

type withinFixture struct {
	service *Service
	sender  uuid.UUID
}

// newWithinFixture returns a service with delivery bound (so readiness can pass) and a real
// sender row.
func newWithinFixture(t *testing.T) withinFixture {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	return withinFixture{service: service, sender: testhelpers.CreateUser(t, db)}
}

// stripeMate returns a sender, not senderID, whose gate is senderID's stripe.
func stripeMate(senderID uuid.UUID) uuid.UUID {
	for {
		if c := uuid.New(); c != senderID && senderGateIndex(c) == senderGateIndex(senderID) {
			return c
		}
	}
}

// holdGate holds senderID's gate until the returned release is called, and returns once held.
func holdGate(t *testing.T, s *Service, senderID uuid.UUID) (release func()) {
	t.Helper()
	held := make(chan struct{})
	free := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), holderLifetime)
		defer cancel()
		_ = s.WithSender(ctx, senderID, func() error {
			close(held)
			// Bounded: a test that dies before releasing cannot wedge the gate or its cleanup.
			select {
			case <-free:
			case <-time.After(holderLifetime):
			}
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate holder never entered")
	}
	var once bool
	release = func() {
		if !once {
			once = true
			close(free)
			<-done
		}
	}
	t.Cleanup(release)
	return release
}

func TestWithReadySenderModeWithin_RefusesANonPositiveWait(t *testing.T) {
	f := newWithinFixture(t)
	for _, wait := range []time.Duration{0, -time.Second} {
		called := false
		err := f.service.WithReadySenderModeWithin(boundedCtx(t, 0), f.sender, ForcedSecurityClear, wait, func() error {
			called = true
			return nil
		})
		require.Error(t, err, "wait %v", wait)
		assert.NotErrorIs(t, err, ErrSenderGateBusy)
		assert.False(t, called, "wait %v: the work never runs", wait)
	}
	// The refusal took no gate.
	require.NoError(t, f.service.WithSender(boundedCtx(t, 0), f.sender, func() error { return nil }))
}

func TestWithReadySenderModeWithin_HeldGateIsBusyWithinTheBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		held func(sender uuid.UUID) uuid.UUID
	}{
		{"another sender on the same stripe", stripeMate},
		{"the sender's own gate", func(sender uuid.UUID) uuid.UUID { return sender }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWithinFixture(t)
			// No delivery is bound on this service, so readiness would fail with its own 503 if the
			// gate were ever acquired: ErrSenderGateBusy proves nothing ran past the acquire.
			bare := NewService(f.service.db, DisclosureState{}, false)
			holder := tc.held(f.sender)
			release := holdGate(t, bare, holder)
			defer release()

			called := false
			start := time.Now()
			err := bare.WithReadySenderModeWithin(boundedCtx(t, withinWait), f.sender, ForcedSecurityClear, withinWait, func() error {
				called = true
				return nil
			})
			took := time.Since(start)

			require.ErrorIs(t, err, ErrSenderGateBusy, "an unbounded acquire would return the context error instead")
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			assert.Less(t, took, withinWait+withinSlack, "returned before the call's own deadline")
			assert.False(t, called, "the work never runs when the gate is busy")
			assert.GreaterOrEqual(t, took, withinWait-50*time.Millisecond, "it waited for the bound")
			assert.Less(t, took, withinWait+2*time.Second, "and no longer")
		})
	}
}

func TestWithReadySenderModeWithin_GateThatFreesInsideTheBoundRunsTheWork(t *testing.T) {
	f := newWithinFixture(t)
	release := holdGate(t, f.service, stripeMate(f.sender))
	time.AfterFunc(withinWait/4, release)

	called := false
	err := f.service.WithReadySenderModeWithin(boundedCtx(t, 10*time.Second), f.sender, ForcedSecurityClear, 10*time.Second, func() error {
		called = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, called)
}

func TestWithReadySenderModeWithin_BoundCoversOnlyTheWait(t *testing.T) {
	t.Run("work longer than the wait still completes", func(t *testing.T) {
		f := newWithinFixture(t)
		called := false
		err := f.service.WithReadySenderModeWithin(boundedCtx(t, 10*time.Second), f.sender, ForcedSecurityClear, withinWait, func() error {
			time.Sleep(3 * withinWait)
			called = true
			return nil
		})
		require.NoError(t, err)
		assert.True(t, called)
	})

	t.Run("readiness longer than the wait still completes and the work runs", func(t *testing.T) {
		f := newWithinFixture(t)
		// Readiness opens a transaction; stall that open past the wait.
		begins := 0
		restore := f.service.SetTransactionTestHooks(TransactionTestHooks{
			Begin: func(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
				begins++
				time.Sleep(3 * withinWait)
				return f.service.db.BeginTx(ctx, opts)
			},
		})
		t.Cleanup(restore)

		called := false
		err := f.service.WithReadySenderModeWithin(boundedCtx(t, 10*time.Second), f.sender, ForcedSecurityClear, withinWait, func() error {
			called = true
			return nil
		})
		require.NoError(t, err)
		assert.Positive(t, begins, "fixture: readiness did stall past the wait")
		assert.True(t, called)
	})
}

func TestWithReadySenderModeWithin_CallerContextIsStillHonoured(t *testing.T) {
	t.Run("cancelled while waiting", func(t *testing.T) {
		f := newWithinFixture(t)
		release := holdGate(t, f.service, f.sender)
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(withinWait/4, cancel)

		called := false
		start := time.Now()
		err := f.service.WithReadySenderModeWithin(ctx, f.sender, ForcedSecurityClear, 10*time.Second, func() error {
			called = true
			return nil
		})

		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, ErrSenderGateBusy)
		assert.False(t, called)
		assert.Less(t, time.Since(start), 5*time.Second, "it did not wait out the bound")
	})

	t.Run("deadline shorter than the wait", func(t *testing.T) {
		f := newWithinFixture(t)
		release := holdGate(t, f.service, f.sender)
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), withinWait/4)
		defer cancel()

		err := f.service.WithReadySenderModeWithin(ctx, f.sender, ForcedSecurityClear, 10*time.Second, func() error { return nil })

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, ErrSenderGateBusy)
	})

	t.Run("already cancelled with a free gate", func(t *testing.T) {
		f := newWithinFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		called := false
		err := f.service.WithReadySenderModeWithin(ctx, f.sender, ForcedSecurityClear, time.Second, func() error {
			called = true
			return nil
		})

		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, called)
	})
}
