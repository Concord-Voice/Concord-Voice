package api_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ownership"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
)

// stubPresenceRecheck is the smallest thing that satisfies the seam; the guard
// cares only that SetPresenceRecheck ran, never what it stored.
type stubPresenceRecheck struct{}

func (stubPresenceRecheck) PrepareCapture(
	context.Context, string, []string, *string,
) (rbac.PresenceRecheckPlan, error) {
	return nil, nil
}
func (s stubPresenceRecheck) PrepareCaptureStrict(
	ctx context.Context, serverID string, channelIDs []string, affectedUserID *string,
) (rbac.PresenceRecheckPlan, error) {
	return s.PrepareCapture(ctx, serverID, channelIDs, affectedUserID)
}
func (stubPresenceRecheck) CaptureVisibility(
	context.Context, *sql.Tx, rbac.PresenceRecheckPlan,
) error {
	return nil
}
func (stubPresenceRecheck) Execute(rbac.PresenceRecheckPlan)         {}
func (stubPresenceRecheck) Abandon(rbac.PresenceRecheckPlan, string) {}

// requirePresenceRecheckWired's predicate must reflect WIRING, not executor
// construction.
//
// The guard previously took the rbac.PresenceRecheck value the caller held and
// tested it for nil. voicepresence.NewExecutor always returns a non-nil
// *Executor, and boxing a non-nil pointer into an interface always yields a
// non-nil interface, so the condition was unreachable — and because it read a
// local variable rather than handler state, deleting the SetPresenceRecheck
// call still booted cleanly. That call is the one fail-OPEN path the guard is
// documented to catch: without it, RBAC mutations commit with no capture and no
// clear, silently restoring the #2445 disclosure.
//
// This covers the predicate rather than the log.Fatal wrapper, which cannot be
// exercised without process isolation.
func TestPresenceRecheckGuardPredicate_ReflectsWiring(t *testing.T) {
	t.Run("unwired handler reports false", func(t *testing.T) {
		assert.False(t, (&rbac.Handler{}).HasPresenceRecheck(),
			"a handler that never had SetPresenceRecheck called must fail the guard")
	})

	t.Run("wired handler reports true", func(t *testing.T) {
		h := &rbac.Handler{}
		h.SetPresenceRecheck(stubPresenceRecheck{})
		assert.True(t, h.HasPresenceRecheck())
	})

	t.Run("explicitly nil wiring still reports false", func(t *testing.T) {
		h := &rbac.Handler{}
		h.SetPresenceRecheck(nil)
		assert.False(t, h.HasPresenceRecheck(),
			"SetPresenceRecheck(nil) is not wiring")
	})

	t.Run("ownership handler has the same fail-closed wiring states", func(t *testing.T) {
		unwired := &ownership.Handler{}
		assert.False(t, unwired.HasPresenceRecheck())

		wired := &ownership.Handler{}
		wired.SetPresenceRecheck(stubPresenceRecheck{})
		assert.True(t, wired.HasPresenceRecheck())

		nilWiring := &ownership.Handler{}
		nilWiring.SetPresenceRecheck(nil)
		assert.False(t, nilWiring.HasPresenceRecheck())
	})

	// requirePresenceRecheckWired exits the process on failure, so keep this
	// wiring assertion source-level and exercise the handler states above.
	source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	assert.NoError(t, err)
	if err == nil {
		contents := string(source)
		setter := "ownershipHandler.SetPresenceRecheck(presenceRecheckExecutor)"
		guard := "requirePresenceRecheckWired(log, activityService, rbacHandler, ownershipHandler)"
		assert.Contains(t, contents, setter)
		assert.Contains(t, contents, guard)
		assert.Less(t, strings.Index(contents, setter), strings.Index(contents, guard),
			"ownership presence wiring must precede the startup guard")
		assert.Contains(t, contents, "ownershipHandler == nil || !ownershipHandler.HasPresenceRecheck()",
			"the startup guard must retain the ownership consumer arm")
		assert.Equal(t, 1, strings.Count(contents, "ownershipHandler.HasPresenceRecheck()"))
	}
}
