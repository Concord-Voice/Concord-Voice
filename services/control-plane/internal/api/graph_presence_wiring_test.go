package api

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/graphpresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
)

// nopCapture is a wired-but-inert presencecapture.GraphPresenceCapture. The
// guard must see a wired handler, not a working one.
type nopCapture struct{}

// WithGatedTx stays inert like the rest of this double, but returns a sentinel
// rather than nil. Nothing invokes it — these tests only ask the guard whether
// the handlers are wired, and graphpresence's own topology_test is the tree's
// only WithGatedTx caller — so a nil return would be an unexercised claim that
// work ran when it did not.
func (nopCapture) WithGatedTx(
	context.Context, presencecapture.Subject, func(*sql.Tx) error,
) error {
	return errors.New("nopCapture: WithGatedTx is inert and runs no work")
}

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

func TestGraphPresenceFamilyRegistryComplete(t *testing.T) {
	t.Run("the real registry covers every declared family", func(t *testing.T) {
		assert.True(t, graphPresenceFamilyRegistryComplete(),
			"every declared presencecapture.Family must carry a registry entry")
	})

	// The assertion above reads the REAL registry, which pins the guard to its
	// source but can only ever assert true — on its own it cannot tell a wired
	// predicate from a hardcoded `return true`, and the guard would go inert
	// with the test still green. That is the same fail-OPEN class the sibling
	// wiring guard drives false in TestGraphPresenceGuardDetectsUnwiredHandler.
	//
	// Swapping familyGaps drives the FALSE branch of the exact zero-arg
	// function requireGraphPresenceFamilyRegistry calls. presencecapture's own
	// registry_test cannot: its gap comes from swapping the unexported
	// familyRegistry, which this package cannot reach.
	t.Run("a gap in the lookup makes the guard fail", func(t *testing.T) {
		original := familyGaps
		t.Cleanup(func() { familyGaps = original })
		familyGaps = func() []presencecapture.Family {
			return []presencecapture.Family{presencecapture.FamilyBlock}
		}

		assert.False(t, graphPresenceFamilyRegistryComplete(),
			"the guard must follow its gap lookup, not a constant")
	})
}

// The cases above drive the zero-arg composition the boot guard calls. These
// drive the verdict underneath it over a supplied gap list, so a verdict that
// stopped reading its argument is caught even while the lookup is intact.
func TestFamilyRegistryCompleteVerdict(t *testing.T) {
	cases := []struct {
		name    string
		missing []presencecapture.Family
		want    bool
	}{
		{
			name:    "no gap is complete",
			missing: nil,
			want:    true,
		},
		{
			name:    "one unregistered family is incomplete",
			missing: []presencecapture.Family{presencecapture.FamilyBlock},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, familyRegistryComplete(tc.missing),
				"verdict must follow the gap list, not a constant")
		})
	}
}

// TestGraphPresenceRailWired drives both branches of the rail guard. No func-var
// seam is needed here: graphPresenceRailWired takes the reconciler as a
// parameter, so a test constructs one without the rail and then wires it.
func TestGraphPresenceRailWired(t *testing.T) {
	// graphpresence.New starts a dispatch goroutine, so every reconciler built
	// here is closed — an unclosed one is a -race report, not a leak the test
	// would otherwise notice.
	newReconciler := func(t *testing.T) *graphpresence.Reconciler {
		t.Helper()
		reconciler := graphpresence.New(nil, nil, nil, nil, nil)
		t.Cleanup(reconciler.Close)
		return reconciler
	}

	t.Run("an unwired rail is a boot failure", func(t *testing.T) {
		assert.False(t, graphPresenceRailWired(newReconciler(t)),
			"a reconciler with no rail must fail the guard")
	})

	t.Run("wiring the rail satisfies the guard", func(t *testing.T) {
		reconciler := newReconciler(t)
		reconciler.SetTopologyRail(
			presencehistory.NewService(nil, presencehistory.DisclosureState{}, false))

		assert.True(t, graphPresenceRailWired(reconciler),
			"a wired reconciler must pass the guard")
	})
}

// TestGraphPresenceRailIsWiredAtTheConstructionSite pins the one router line
// that arms the durable leg in a running process, and pins it BEFORE the guard
// that checks it.
//
// TestGraphPresenceRailWired proves the predicate reads the reconciler, but it
// wires its own reconciler, so deleting router.go's SetTopologyRail call leaves
// it green. The boot guard does catch that deletion — through log.Fatal's
// os.Exit(1), which aborts whichever SetupTestServer test ran first and names
// nothing. Reading the source is the same mechanism
// TestNewRouterActivityHistoryWiringOrderIsSingleAndFinal already uses for the
// sibling presenceHistoryService wiring lines, and it fails by name.
func TestGraphPresenceRailIsWiredAtTheConstructionSite(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)

	// require, not assert, and that is load-bearing rather than stylistic: if
	// the wiring line were absent, strings.Index below returns -1 and
	// require.Less(-1, ...) would PASS. Only this assertion halting first stops
	// the ordering check from reporting a clean run on a rail nobody wired. Do
	// not "tidy" it to assert.
	const wiring = "graphPresenceCapture.SetTopologyRail(presenceHistoryService)"
	require.Equal(t, 1, strings.Count(source, wiring),
		"the durable rail is wired at exactly one construction site")

	// LastIndex, not Index: requireGraphPresenceCaptureWired's own declaration
	// precedes the construction site, so Index would match the func keyword and
	// the ordering claim would invert.
	require.Less(t, strings.Index(source, wiring),
		strings.LastIndex(source, "requireGraphPresenceCaptureWired("),
		"the rail must be wired before the guard that interrogates it runs")
}
