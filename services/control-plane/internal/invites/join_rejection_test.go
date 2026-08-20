package invites

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// joinRejection carries a non-5xx outcome out of the hooked transaction. Before
// the hook these paths wrote their response inline; now they must survive the
// round trip through error and back, or a 404 silently becomes a 500.
func TestJoinRejectionCarriesItsStatus(t *testing.T) {
	rejection := &joinRejection{status: http.StatusNotFound, msg: errMsgInvalidInviteCode}

	require.Equal(t, errMsgInvalidInviteCode, rejection.Error(),
		"the message must survive so the handler can echo it verbatim")
	require.Equal(t, http.StatusNotFound, rejection.status)
}

// The hydrate is post-commit and must never turn a committed join into a
// failure, so every branch of it degrades quietly.
func TestInviteHydrateDegradesQuietly(t *testing.T) {
	t.Run("no snapshot service wired", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.hydrateJoinerPresence(context.Background(), "11111111-1111-1111-1111-111111111111")
		})
	})

	t.Run("malformed viewer id", func(t *testing.T) {
		// snapshots MUST be non-nil, or hydrateJoinerPresence returns at its own
		// nil check and this subtest silently walks the same path as the one
		// above, never reaching uuid.Parse (CodeRabbit, PR #2840).
		h := &Handler{log: logger.New("test"), snapshots: &presence.ActivitySnapshotService{}}
		require.NotPanics(t, func() {
			h.hydrateJoinerPresence(context.Background(), "not-a-uuid")
		})
	})
}

// captureFailure is a GraphPresenceCapture double that runs the handler's
// closure and then fails the capture read, so the fail-closed branch inside the
// hooked transaction is reachable without a database. The closure receives a nil
// *sql.Tx, which is safe only because the branch returns before any statement.
type captureFailure struct{ err error }

func (c *captureFailure) WithGatedTx(
	_ context.Context, _ presencecapture.Subject, work func(*sql.Tx) error,
) error {
	return work(nil)
}

func (c *captureFailure) CaptureInTx(
	context.Context, *sql.Tx, presencecapture.Subject,
) (presencecapture.Plan, error) {
	return nil, c.err
}
func (*captureFailure) Complete(context.Context, *sql.Tx, presencecapture.Plan) error { return nil }
func (*captureFailure) Abandon(presencecapture.Plan, presencecapture.Cause)           {}

// addMemberToServer is the additive write, but the CAPTURE around it still fails
// closed: hydration failing open is a post-commit posture, not a licence to
// commit a membership row whose pre-mutation audience could not be read.
func TestJoinCaptureFailureIsSurfaced(t *testing.T) {
	forced := errors.New("forced capture read failure")
	capture := &captureFailure{err: forced}

	var captured error
	err := presencehook.WithGatedTx(context.Background(), capture, nil, logger.New("test"),
		presencehook.Spec{
			Family:      presencecapture.FamilyMemberJoin,
			Posture:     presencecapture.FailClosedBlockWrite,
			PrincipalID: "11111111-1111-1111-1111-111111111111",
		},
		func(tx *sql.Tx) error {
			_, captured = presencehook.Capture(context.Background(), capture, tx, presencehook.Spec{
				Family:      presencecapture.FamilyMemberJoin,
				Posture:     presencecapture.FailClosedBlockWrite,
				PrincipalID: "11111111-1111-1111-1111-111111111111",
			})
			return captured
		})

	require.ErrorIs(t, err, forced)
	require.ErrorIs(t, captured, forced,
		"the additive path still fails closed on a capture read failure")
}
