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

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/graphpresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
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

// inertActiveDeliverer is a wired-but-inert activepresence.Deliverer, the same
// shape and for the same reason as nopCapture above: the rail guard must see a
// terminal that was ATTACHED, not one that works.
type inertActiveDeliverer struct{}

func (inertActiveDeliverer) ClearSenderActiveCategory(uuid.UUID, presence.Category) {}

// DisconnectAllRichPresenceClients returns a sentinel rather than nil for
// nopCapture's reason: nothing here invokes it, so a nil return would be an
// unexercised claim that a fleet-wide disconnect ran when none did.
func (inertActiveDeliverer) DisconnectAllRichPresenceClients(context.Context) error {
	return errors.New("inertActiveDeliverer: the terminal is inert and delivers nothing")
}

// deliverableActivePlanRail builds a rail whose reconciler carries a terminal.
// Everything else is nil: the guard reads the terminal and nothing else.
func deliverableActivePlanRail() *activepresence.Rail {
	return activepresence.NewRail(nil, nil,
		activepresence.NewReconciler(nil, nil, nil, nil, inertActiveDeliverer{}, nil), nil)
}

// fullyWiredErasureService is the erasure consumer with all FOUR of its arms
// attached. It is a builder rather than four inline calls because the unwire
// table below drops one arm at a time, and a case that dropped an arm from a
// service missing another would prove nothing about the arm it names.
func fullyWiredErasureService() *users.AccountService {
	service := &users.AccountService{}
	service.SetGraphPresenceCapture(nopCapture{})
	service.SetNATS(&natsclient.Client{})
	service.SetActivePlanRail(deliverableActivePlanRail())
	service.SetAudienceFence(websocket.NewHub(nil, nil))
	return service
}

func fullyWiredGraphPresenceConsumers() graphPresenceConsumers {
	c := graphPresenceConsumers{
		friends: &friends.Handler{},
		users:   &users.Handler{},
		members: &members.Handler{},
		invites: &invites.Handler{},
		servers: &servers.Handler{},
		dm:      &dm.Handler{},
		erasure: fullyWiredErasureService(),
	}
	c.friends.SetGraphPresenceCapture(nopCapture{})
	c.users.SetGraphPresenceCapture(nopCapture{})
	c.members.SetGraphPresenceCapture(nopCapture{})
	c.invites.SetGraphPresenceCapture(nopCapture{})
	c.servers.SetGraphPresenceCapture(nopCapture{})
	c.servers.SetActivePlanRail(deliverableActivePlanRail())
	c.dm.SetActivePlanRail(deliverableActivePlanRail())
	return c
}

func (c graphPresenceConsumers) complete() bool {
	return graphPresenceWiringComplete(c)
}

// The guard must interrogate consumer state. Mirrors the #2445 review finding
// that a check on the constructed value is a tautology.
func TestGraphPresenceGuardDetectsUnwiredHandler(t *testing.T) {
	c := graphPresenceConsumers{friends: &friends.Handler{}, users: &users.Handler{},
		members: &members.Handler{}, invites: &invites.Handler{},
		servers: &servers.Handler{}, dm: &dm.Handler{}, erasure: &users.AccountService{}}

	require.False(t, c.complete(),
		"guard must report incomplete when no consumer is wired")

	c.friends.SetGraphPresenceCapture(nopCapture{})
	require.False(t, c.complete(),
		"guard must report incomplete while the other consumers are still unwired")
}

// Every one of the guard's clauses must be able to fail it ON ITS OWN. A guard
// that only notices the first missing arm is how #2447's four new consumers —
// and #2448's two — would ship unwired and silently skip reconciliation.
func TestGraphPresenceGuardDetectsEachUnwiredConsumer(t *testing.T) {
	unwire := map[string]func(*graphPresenceConsumers){
		"friends": func(c *graphPresenceConsumers) { c.friends = &friends.Handler{} },
		"users":   func(c *graphPresenceConsumers) { c.users = &users.Handler{} },
		"members": func(c *graphPresenceConsumers) { c.members = &members.Handler{} },
		"invites": func(c *graphPresenceConsumers) { c.invites = &invites.Handler{} },
		"servers": func(c *graphPresenceConsumers) { c.servers = &servers.Handler{} },
		"servers active-category rail": func(c *graphPresenceConsumers) {
			fresh := &servers.Handler{}
			fresh.SetGraphPresenceCapture(nopCapture{})
			c.servers = fresh
		},
		"erasure": func(c *graphPresenceConsumers) { c.erasure = &users.AccountService{} },
		"erasure clear publisher": func(c *graphPresenceConsumers) {
			// Capture and drain wired, SetNATS NEVER CALLED. Still fails closed,
			// and #2854 stage A sharpened WHY: a nil here is now a deleted wiring
			// line or an unparseable NATS_URL, never a transient outage.
			// RetryOnFailedConnect (pkg/nats.Connect) makes an unreachable bus
			// return a reconnecting client, so an outage no longer reaches this
			// predicate at all. An earlier revision of this comment said "a boot
			// without NATS must fail", which conflated the two.
			fresh := fullyWiredErasureService()
			fresh.SetNATS(nil)
			c.erasure = fresh
		},
		// #2448. An unwired dm rail is the worst of the set: DeleteGroup would
		// commit the cascade, record NO obligation, and destroy the C3 evidence
		// (dm_voice_participants and the Redis lease) in that same commit. There
		// is no later recovery and no error, because Reconciler.Run discards
		// every pass error by design — this guard is the only thing standing in
		// front of that.
		"dm active-category rail": func(c *graphPresenceConsumers) {
			c.dm = &dm.Handler{}
		},
		// #2448. Unwired, an erasure leaves the deleted user's plan rows behind
		// and hits migration 000111's RESTRICT: the erasure fails with an opaque
		// 23503 rather than draining.
		"erasure active-category drain": func(c *graphPresenceConsumers) {
			fresh := &users.AccountService{}
			fresh.SetGraphPresenceCapture(nopCapture{})
			fresh.SetNATS(&natsclient.Client{})
			fresh.SetAudienceFence(websocket.NewHub(nil, nil))
			c.erasure = fresh
		},
		// #2992. Unwired, the erasure still runs and still dispatches its
		// post-commit signal -- so this arm DEGRADES rather than breaking, unlike
		// the drain above. What is lost is the ordering: the signal is bounded by
		// dispatchQueueDepth times dispatchTimeout rather than by the write, which
		// is precisely the residual #2992 exists to close. Silent, which is why it
		// is guarded rather than warned about.
		"erasure audience fence": func(c *graphPresenceConsumers) {
			fresh := fullyWiredErasureService()
			fresh.SetAudienceFence(nil)
			c.erasure = fresh
		},
	}
	for name, drop := range unwire {
		t.Run(name, func(t *testing.T) {
			c := fullyWiredGraphPresenceConsumers()
			drop(&c)
			require.False(t, c.complete(),
				"an unwired %s must fail the boot guard, not silently skip reconciliation", name)
		})
	}
}

func TestGraphPresenceGuardPassesWhenEveryConsumerIsWired(t *testing.T) {
	require.True(t, fullyWiredGraphPresenceConsumers().complete(),
		"guard must pass when every consumer is wired")
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

// TestActivePlanRailWired drives every branch of the #2448 rail guard.
//
// The middle case is the load-bearing one. activepresence.NewRail and
// NewReconciler both always return a non-nil pointer, so `rail != nil` would be
// a tautology that boots happily with the terminal never attached — and an
// unattached terminal is silent, because Reconciler.Run drops every pass error.
func TestActivePlanRailWired(t *testing.T) {
	t.Run("a rail with no reconciler is a boot failure", func(t *testing.T) {
		assert.False(t, activePlanRailWired(activepresence.NewRail(nil, nil, nil, nil)),
			"a rail whose reconciler was never constructed must fail the guard")
	})

	t.Run("a reconciler with no terminal is a boot failure", func(t *testing.T) {
		assert.False(t, activePlanRailWired(activepresence.NewRail(nil, nil,
			activepresence.NewReconciler(nil, nil, nil, nil, nil, nil), nil)),
			"the guard must read the terminal, not the rail pointer")
	})

	t.Run("a deliverable rail satisfies the guard", func(t *testing.T) {
		assert.True(t, activePlanRailWired(deliverableActivePlanRail()),
			"a rail whose reconciler carries a terminal must pass the guard")
	})
}

// TestActivePlanRailIsWiredAtItsConstructionSites pins the three router lines
// that arm #2448 in a running process, for the reason
// TestGraphPresenceRailIsWiredAtTheConstructionSite gives below: the unwire
// table proves each guard CLAUSE, but every case builds its own consumers, so
// deleting a wiring line from router.go leaves all of them green. The boot guard
// does catch that deletion — through log.Fatal's os.Exit(1), which aborts
// whichever SetupTestServer test ran first and names nothing. This fails by name.
func TestActivePlanRailIsWiredAtItsConstructionSites(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)

	// ONE construction site for each. A second rail would be a second claim/ack
	// loop racing the first for the same rows, and a second gate array — which
	// would not be a gate.
	for _, wiring := range []string{
		"activePlanRail, activePlanReconciler := buildActivePlanRail(",
		"ActivePlans: activePlanRail,",
		"accountService.SetActivePlanRail(activePlanRail)",
		"serversHandler.SetActivePlanRail(activePlanRail)",
	} {
		// require, not assert: an absent line makes strings.Index return -1, and
		// the ordering assertion below would PASS on it.
		require.Equal(t, 1, strings.Count(source, wiring),
			"%s must appear at exactly one construction site", wiring)

		// LastIndex for the guard: requireGraphPresenceCaptureWired's own
		// declaration precedes every wiring line, so Index would match the func
		// keyword and invert the claim.
		require.Less(t, strings.Index(source, wiring),
			strings.LastIndex(source, "requireGraphPresenceCaptureWired("),
			"%s must run before the guard that interrogates it", wiring)
	}

	// And the guard must actually CONSUME the predicate. TestActivePlanRailWired
	// proves activePlanRailWired reads the terminal, but it calls the predicate
	// itself, so deleting this branch from requireGraphPresenceCaptureWired
	// leaves it green — and leaves the boot inert. Nothing else can drive this:
	// the branch ends in log.Fatal, whose os.Exit(1) a test cannot survive to
	// assert on.
	require.Equal(t, 1, strings.Count(source, "if !activePlanRailWired(activePlanRail) {"),
		"requireGraphPresenceCaptureWired must fatal-exit on an undeliverable rail")
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
