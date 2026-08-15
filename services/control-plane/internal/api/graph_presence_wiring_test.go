package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
)

// nopCapture is a wired-but-inert presencecapture.GraphPresenceCapture. The
// guard must see a wired handler, not a working one.
type nopCapture struct{}

func (nopCapture) CaptureInTx(
	context.Context, *sql.Tx, presencecapture.Subject,
) (presencecapture.Plan, error) {
	return nil, nil
}
func (nopCapture) Complete(context.Context, *sql.Tx, presencecapture.Plan) error { return nil }
func (nopCapture) Abandon(presencecapture.Plan, presencecapture.Cause)           {}

// The guard must interrogate handler state. Mirrors the #2445 review finding
// that a check on the constructed value is a tautology.
func TestGraphPresenceGuardDetectsUnwiredHandler(t *testing.T) {
	f := &friends.Handler{}
	u := &users.Handler{}

	require.False(t, graphPresenceWiringComplete(f, u),
		"guard must report incomplete when neither handler is wired")

	f.SetGraphPresenceCapture(nopCapture{})
	require.False(t, graphPresenceWiringComplete(f, u),
		"guard must report incomplete while the users handler is still unwired")
}

func TestGraphPresenceGuardPassesWhenBothWired(t *testing.T) {
	f := &friends.Handler{}
	u := &users.Handler{}
	f.SetGraphPresenceCapture(nopCapture{})
	u.SetGraphPresenceCapture(nopCapture{})

	require.True(t, graphPresenceWiringComplete(f, u),
		"guard must pass when both handlers are wired")
}
