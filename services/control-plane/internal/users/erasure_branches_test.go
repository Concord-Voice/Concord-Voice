package users

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// captureErasureAlreadyGated must fail CLOSED on a malformed principal.
// Capturing against uuid.Nil would drop the principal from the focal set
// silently and reconcile nothing, which on an erasure path means the audience is
// never cleared and the account is erased anyway.
func TestCaptureErasureRejectsAMalformedPrincipal(t *testing.T) {
	s := &AccountService{log: logger.New("test")}
	s.SetGraphPresenceCapture(&noopErasureCapture{})

	plan, err := s.captureErasureAlreadyGated(context.Background(), nil, "not-a-uuid")

	require.Error(t, err, "a malformed principal must fail closed, not capture against uuid.Nil")
	require.Nil(t, plan)
}

// An unwired capture is the pre-hook shape and must stay a clean no-op, so a
// replica without presence wiring behaves exactly as it did before.
func TestCaptureErasureIsANoOpWhenUnwired(t *testing.T) {
	s := &AccountService{log: logger.New("test")}

	plan, err := s.captureErasureAlreadyGated(context.Background(), nil, "11111111-1111-1111-1111-111111111111")

	require.NoError(t, err)
	require.Nil(t, plan)
}

// publishErasureCleared is post-commit and best-effort: the account is already
// gone, so no branch of it may panic or return an error that fails the request.
//
// It takes NO context. An earlier version guarded on the inbound REQUEST
// context's ctx.Err(), so a client disconnecting between the commit and the
// publish silently skipped the only cross-replica clear there is — and the test
// that accompanied it asserted that skip was fine, pinning the bug rather than
// catching it (Gitar review, PR #2840). The signature change makes the mistake
// unrepresentable.
//
// A zero-value nats client is the vehicle for the failure case. Its conn is nil,
// and nats.go returns ErrInvalidConnection rather than panicking, so this
// exercises the real publish call and the real error branch. Passing nil for the
// client instead would return at the first guard and leave both untested.
func TestPublishErasureClearedIsBestEffort(t *testing.T) {
	const principal = "11111111-1111-1111-1111-111111111111"

	t.Run("no nats wired", func(t *testing.T) {
		s := &AccountService{log: logger.New("test")}
		require.NotPanics(t, func() { s.publishErasureCleared(principal) })
	})

	t.Run("publish failure is logged, never returned", func(t *testing.T) {
		s := &AccountService{log: logger.New("test")}
		s.SetNATS(&natsclient.Client{})
		require.NotPanics(t, func() { s.publishErasureCleared(principal) },
			"a failed cross-replica clear must not fail a request whose account is already erased")
	})
}

// noopErasureCapture satisfies the capture interface without touching a
// transaction. The malformed-principal test must fail BEFORE any capture call,
// so this double never being invoked is part of what that test asserts.
type noopErasureCapture struct{}

func (*noopErasureCapture) WithGatedTx(
	context.Context, presencecapture.Subject, func(*sql.Tx) error,
) error {
	panic("erasure must not call WithGatedTx; it is already gated")
}

func (*noopErasureCapture) CaptureInTx(
	context.Context, *sql.Tx, presencecapture.Subject,
) (presencecapture.Plan, error) {
	return nil, nil
}
func (*noopErasureCapture) Complete(context.Context, *sql.Tx, presencecapture.Plan) error { return nil }
func (*noopErasureCapture) Abandon(presencecapture.Plan, presencecapture.Cause)           {}
