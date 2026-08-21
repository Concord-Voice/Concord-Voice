package rbac_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Cross-language reorder contract — the Go half of the shared oracle (#2359 T2)
//
// testdata/role_reorder_contract.json is ONE table read by two languages. The
// TypeScript half (T3) asserts what the client WOULD send for each declared
// world. This half seeds the identical world, PATCHes that same payload as that
// same actor, and asks the REAL server for the verdict.
//
// That split is the whole point. PR #2839 shipped an owner-only reorder with a
// fully green suite because the production code and the Vitest suite encoded
// the same wrong belief from the same docstring and asserted it against a
// vi.fn() stub, while every green Go test for a non-owner sent a hand-written
// partial payload that happened to sit inside every budget. A wrong belief
// cannot be green on both sides of THIS table, because one side is not asking
// a mock.
//
// Consequently: NOTHING in this file may re-derive a guard. It resolves keys to
// ids, sends what the fixture declares, and compares the response and the rows
// to what the fixture declares. A helper here that recomputed the band, the
// position offset or the budget would reintroduce #2839's defect in a new
// costume — the server is the only authority this file consults.
// ─────────────────────────────────────────────────────────────────────────────

const roleReorderContractFile = "role_reorder_contract.json"

type contractRole struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Position    int      `json:"position"`
	IsDefault   bool     `json:"is_default"`
	IsManaged   bool     `json:"is_managed"`
	Permissions []string `json:"permissions"`
}

type contractViewer struct {
	Kind            string `json:"kind"`
	MaxRolePosition *int   `json:"max_role_position"`
}

type contractActor struct {
	HeldRoleKeys []string       `json:"held_role_keys"`
	Viewer       contractViewer `json:"viewer"`
}

type contractWorld struct {
	OwnerIsActor bool           `json:"owner_is_actor"`
	Roles        []contractRole `json:"roles"`
	Actor        contractActor  `json:"actor"`
}

type contractHierarchy struct {
	AboveCeiling []string `json:"above_ceiling"`
	Band         []string `json:"band"`
	Managed      []string `json:"managed"`
	Pinned       []string `json:"pinned"`
}

type contractCollision struct {
	Position int      `json:"position"`
	RoleKeys []string `json:"role_keys"`
}

type contractVerdict struct {
	Status         int                 `json:"status"`
	ErrorContains  *string             `json:"error_contains"`
	PositionsAfter map[string]int      `json:"positions_after"`
	Collisions     []contractCollision `json:"collisions"`
}

type contractProbe struct {
	ID           string          `json:"id"`
	Purpose      string          `json:"purpose"`
	WrongBuilder string          `json:"wrong_builder"`
	RoleIDs      []string        `json:"role_ids"`
	Expected     contractVerdict `json:"expected"`
	Note         string          `json:"note"`
}

type contractPayload struct {
	RoleIDs []string `json:"role_ids"`
}

type contractScenario struct {
	ID             string            `json:"id"`
	SpecScenario   int               `json:"spec_scenario"`
	Title          string            `json:"title"`
	World          contractWorld     `json:"world"`
	ClientSnapshot *contractWorld    `json:"client_snapshot"`
	Hierarchy      contractHierarchy `json:"expected_hierarchy"`
	Precondition   string            `json:"expected_precondition"`
	Payload        *contractPayload  `json:"expected_payload"`
	Server         *contractVerdict  `json:"server"`
	Probes         []contractProbe   `json:"probes"`
	GoExercisable  bool              `json:"go_exercisable"`
	ClientOnly     json.RawMessage   `json:"client_only"`
}

type contractFile struct {
	Version   int                `json:"version"`
	Scenarios []contractScenario `json:"scenarios"`
}

// clientView is the world the client built its payload from: the snapshot when
// one is declared (staleness), otherwise server truth.
func (s contractScenario) clientView() contractWorld {
	if s.ClientSnapshot != nil {
		return *s.ClientSnapshot
	}
	return s.World
}

func (w contractWorld) byKey() map[string]contractRole {
	out := make(map[string]contractRole, len(w.Roles))
	for _, r := range w.Roles {
		out[r.Key] = r
	}
	return out
}

func loadRoleReorderContract(t *testing.T) contractFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", roleReorderContractFile))
	require.NoError(t, err, "the shared contract fixture must be readable from the Go package directory")

	var fixture contractFile
	require.NoError(t, json.Unmarshal(raw, &fixture), "the shared contract fixture must be valid JSON")
	require.Equal(t, 1, fixture.Version,
		"contract fixture schema version changed; both language consumers must be updated together")
	return fixture
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture integrity — pure, no database
//
// These are structural checks on the declared table, NOT semantic ones: nothing
// here decides what the server would do, or what the band ought to contain. Its
// job is to stop the table quietly rotting — a scenario dropped, a key renamed
// on one side of a group, a payload edited out of step with its band.
// ─────────────────────────────────────────────────────────────────────────────

func TestRoleReorderContract_FixtureIntegrity(t *testing.T) {
	fixture := loadRoleReorderContract(t)

	t.Run("all eight spec scenarios are present exactly once", func(t *testing.T) {
		seenSpec := map[int]string{}
		seenID := map[string]bool{}
		for _, sc := range fixture.Scenarios {
			require.NotContains(t, seenSpec, sc.SpecScenario,
				"spec §6 scenario %d declared twice: %q and %q", sc.SpecScenario, seenSpec[sc.SpecScenario], sc.ID)
			require.False(t, seenID[sc.ID], "duplicate scenario id %q", sc.ID)
			seenSpec[sc.SpecScenario] = sc.ID
			seenID[sc.ID] = true
		}
		for n := 1; n <= 8; n++ {
			assert.Contains(t, seenSpec, n, "spec §6 scenario %d is mandatory and is missing", n)
		}
	})

	for _, sc := range fixture.Scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			assertWorldWellFormed(t, "world", sc.World)
			if sc.ClientSnapshot != nil {
				assertWorldWellFormed(t, "client_snapshot", *sc.ClientSnapshot)
			}
			assertHierarchyPartitionsRoles(t, sc)
			assertPayloadIsTheCompleteBand(t, sc)
			assertServerBlockMatchesPayload(t, sc)
			assertProbesResolve(t, sc)
			assertClientOnlyPresentWhereGoCannotReach(t, sc)
		})
	}
}

func assertWorldWellFormed(t *testing.T, label string, w contractWorld) {
	t.Helper()

	defaults := 0
	seen := map[string]bool{}
	for _, r := range w.Roles {
		require.False(t, seen[r.Key], "%s: duplicate role key %q", label, r.Key)
		seen[r.Key] = true
		if r.IsDefault {
			defaults++
		}
	}
	require.Equal(t, 1, defaults, "%s: a server has exactly one default role", label)

	// The declared ceiling must be the one the server will compute from the
	// declared held roles. This checks the fixture against itself, not the
	// server: getting it wrong would silently seed a world that does not test
	// what the scenario claims to test.
	ceiling := 0
	byKey := w.byKey()
	for _, k := range w.Actor.HeldRoleKeys {
		role, ok := byKey[k]
		require.True(t, ok, "%s: actor holds unknown role key %q", label, k)
		if role.Position > ceiling {
			ceiling = role.Position
		}
	}

	switch w.Actor.Viewer.Kind {
	case "owner":
		require.True(t, w.OwnerIsActor, "%s: viewer kind 'owner' but owner_is_actor is false", label)
		require.Nil(t, w.Actor.Viewer.MaxRolePosition,
			"%s: the owner has no ceiling — guards 4 and 5 are bypassed, so declaring one would be a fiction", label)
	case "bounded":
		require.False(t, w.OwnerIsActor, "%s: viewer kind 'bounded' but owner_is_actor is true", label)
		require.NotNil(t, w.Actor.Viewer.MaxRolePosition, "%s: a bounded viewer must declare max_role_position", label)
		assert.Equal(t, ceiling, *w.Actor.Viewer.MaxRolePosition,
			"%s: declared ceiling disagrees with MAX(position) over the actor's held roles", label)
	default:
		t.Fatalf("%s: unknown viewer kind %q", label, w.Actor.Viewer.Kind)
	}
}

// assertHierarchyPartitionsRoles enforces that the four groups of spec §5.1 are
// disjoint and together cover every role — the property that makes "the payload
// projects exactly band" a meaningful statement.
func assertHierarchyPartitionsRoles(t *testing.T, sc contractScenario) {
	t.Helper()
	view := sc.clientView()
	byKey := view.byKey()

	groups := map[string][]string{
		"above_ceiling": sc.Hierarchy.AboveCeiling,
		"band":          sc.Hierarchy.Band,
		"managed":       sc.Hierarchy.Managed,
		"pinned":        sc.Hierarchy.Pinned,
	}

	placed := map[string]string{}
	for name, keys := range groups {
		assertGroupDisplayOrder(t, name, keys, byKey)
		for _, k := range keys {
			require.Contains(t, byKey, k, "group %s names unknown role key %q", name, k)
			prev, dup := placed[k]
			require.False(t, dup, "role %q appears in both %s and %s; the four groups are disjoint", k, prev, name)
			placed[k] = name
		}
	}
	for _, r := range view.Roles {
		assert.Contains(t, placed, r.Key,
			"role %q is in no group; every role is rendered, so the partition must be total", r.Key)
	}
}

// assertGroupDisplayOrder pins the tie-break declared in the fixture's
// ordering_rules: position DESC, then name ASC, then key ASC. It matters only
// where positions duplicate — a state the server really can produce (s6-p1).
func assertGroupDisplayOrder(t *testing.T, group string, keys []string, byKey map[string]contractRole) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		prev, cur := byKey[keys[i-1]], byKey[keys[i]]
		ordered := prev.Position > cur.Position ||
			(prev.Position == cur.Position && prev.Name < cur.Name) ||
			(prev.Position == cur.Position && prev.Name == cur.Name && prev.Key < cur.Key)
		assert.True(t, ordered,
			"group %s: %q (pos %d) must not precede %q (pos %d) under position DESC, name ASC, key ASC",
			group, prev.Key, prev.Position, cur.Key, cur.Position)
	}
}

// assertPayloadIsTheCompleteBand is the subset-collision guard expressed as a
// TABLE-WIDE law rather than a single scenario. Spec §6 scenario 6 is the case
// that dramatises it; this makes every scenario carry it, so a future payload
// edited down to "just the roles that moved" fails here as well as against the
// server.
func assertPayloadIsTheCompleteBand(t *testing.T, sc contractScenario) {
	t.Helper()
	if sc.Payload == nil {
		assert.NotEqual(t, "ok", sc.Precondition,
			"a scenario with no payload must declare the read-only reason it sends nothing")
		return
	}
	assert.Equal(t, "ok", sc.Precondition, "a scenario that sends a payload must have a passing precondition")
	assert.NotEmpty(t, sc.Payload.RoleIDs,
		"the client must never send []; an empty band means send NOTHING (the server rejects [] at the binding)")

	want := append([]string(nil), sc.Hierarchy.Band...)
	got := append([]string(nil), sc.Payload.RoleIDs...)
	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got,
		"the payload must be a permutation of the COMPLETE band — nothing more (a 403), nothing less (a silent collision)")
}

func assertServerBlockMatchesPayload(t *testing.T, sc contractScenario) {
	t.Helper()
	if sc.Payload == nil {
		assert.Nil(t, sc.Server, "there is no server verdict to declare when the client sends nothing")
		return
	}
	require.NotNil(t, sc.Server, "a scenario with a payload must declare the verdict the real server returns")
	assert.NotEmpty(t, sc.Server.PositionsAfter, "declare every role's position after the request, including unchanged ones")
	if sc.Server.Status == 200 {
		assert.Nil(t, sc.Server.ErrorContains, "a 200 carries no error text")
	} else {
		assert.NotNil(t, sc.Server.ErrorContains, "a refusal must pin the reason text the client surfaces verbatim")
	}
}

func assertProbesResolve(t *testing.T, sc contractScenario) {
	t.Helper()
	byKey := sc.World.byKey()
	for _, p := range sc.Probes {
		assert.Contains(t, []string{"falsification", "negative", "tolerance"}, p.Purpose,
			"probe %s: unknown purpose %q", p.ID, p.Purpose)
		assert.NotEmpty(t, p.WrongBuilder, "probe %s: name the wrong builder it stands for", p.ID)
		for _, k := range p.RoleIDs {
			assert.Contains(t, byKey, k, "probe %s names unknown role key %q", p.ID, k)
		}
		assert.NotEmpty(t, p.Expected.PositionsAfter, "probe %s: declare the resulting positions", p.ID)
	}
}

func assertClientOnlyPresentWhereGoCannotReach(t *testing.T, sc contractScenario) {
	t.Helper()
	if !sc.GoExercisable {
		assert.NotEmpty(t, sc.ClientOnly,
			"a scenario Go cannot exercise must say what the client asserts instead, or it is untested everywhere")
	}
}

// TestRoleReorderContract_Scenario8IsRepresented keeps the client-only half of
// spec §6 scenario 8 from being quietly deleted because no Go assertion reads
// it. Go exercises that scenario's PATCH, but the behaviour that DEFINES it —
// a successful write followed by a failed refetch — has no server-side
// expression at all.
func TestRoleReorderContract_Scenario8IsRepresented(t *testing.T) {
	fixture := loadRoleReorderContract(t)
	for _, sc := range fixture.Scenarios {
		if sc.SpecScenario != 8 {
			continue
		}
		require.NotEmpty(t, sc.ClientOnly, "scenario 8 must carry a client_only block for the T3 consumer")
		var clientOnly map[string]any
		require.NoError(t, json.Unmarshal(sc.ClientOnly, &clientOnly))
		require.Contains(t, clientOnly, "expected_outcome")
		outcome, ok := clientOnly["expected_outcome"].(map[string]any)
		require.True(t, ok, "expected_outcome must be an object")
		assert.Equal(t, true, outcome["ok"], "the PATCH landed, so the outcome is ok")
		assert.Equal(t, false, outcome["reconciled"],
			"the refetch failed, so the store must report reconciled:false and never a bare true")
		return
	}
	t.Fatal("spec §6 scenario 8 is missing from the contract fixture")
}

// ─────────────────────────────────────────────────────────────────────────────
// The oracle — seed the declared world, send the declared payload, ask the
// REAL server
// ─────────────────────────────────────────────────────────────────────────────

type seededWorld struct {
	serverID string
	actor    testhelpers.TestUser
	idByKey  map[string]string
	keyByID  map[string]string
}

func contractPermissionBits(t *testing.T, names []string) int64 {
	t.Helper()
	var bits int64
	for _, n := range names {
		if n != "manage_roles" {
			t.Fatalf("contract fixture names a permission this consumer cannot map: %q", n)
		}
		bits |= int64(rbac.PermManageRoles)
	}
	return bits
}

// seedContractWorld materialises one declared world on a FRESH server with a
// fresh actor, so every payload sent by this file starts from the declared
// positions and no earlier request can have moved them.
func seedContractWorld(t *testing.T, ts *testhelpers.TestServer, world contractWorld) seededWorld {
	t.Helper()

	owner := ts.CreateTestUser(t, "rcown"+uuid.New().String()[:6])
	serverID := ts.CreateTestServer(t, owner.ID, "Reorder Contract Server")
	actor := owner
	if !world.OwnerIsActor {
		actor = ts.CreateTestUser(t, "rcact"+uuid.New().String()[:6])
		ts.AddMemberToServer(t, serverID, actor.ID, "member")
	}

	var defaultID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&defaultID))

	seeded := seededWorld{
		serverID: serverID,
		actor:    actor,
		idByKey:  make(map[string]string, len(world.Roles)),
		keyByID:  make(map[string]string, len(world.Roles)),
	}

	for _, r := range world.Roles {
		bits := contractPermissionBits(t, r.Permissions)
		roleID := defaultID
		if r.IsDefault {
			// The default role already exists — CreateTestServer makes it. Move
			// it to the declared shape rather than inserting a second one.
			_, err := ts.DB.Exec(
				`UPDATE roles SET name = $2, position = $3, is_managed = $4, permissions = permissions | $5
				 WHERE id = $1`,
				defaultID, r.Name, r.Position, r.IsManaged, bits)
			require.NoError(t, err, "seed default role %q", r.Key)
		} else {
			roleID = uuid.New().String()
			_, err := ts.DB.Exec(
				`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
				 VALUES ($1, $2, $3, $4, $5, FALSE, $6)`,
				roleID, serverID, r.Name, r.Position, bits, r.IsManaged)
			require.NoError(t, err, "seed role %q", r.Key)
		}
		seeded.idByKey[r.Key] = roleID
		seeded.keyByID[roleID] = r.Key
	}

	for _, k := range world.Actor.HeldRoleKeys {
		roleID, ok := seeded.idByKey[k]
		require.True(t, ok, "actor holds unknown role key %q", k)
		ts.AssignRoleToUser(t, serverID, actor.ID, roleID)
	}
	// Roles were assigned by direct INSERT, which bypasses handler-level cache
	// invalidation; without this the permission gate reads a pre-seed answer.
	invalidatePermCache(t, ts, serverID, actor.ID)

	return seeded
}

func (w seededWorld) resolve(t *testing.T, keys []string) []string {
	t.Helper()
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		id, ok := w.idByKey[k]
		require.True(t, ok, "payload names unknown role key %q", k)
		ids = append(ids, id)
	}
	return ids
}

func (w seededWorld) positions(t *testing.T, ts *testhelpers.TestServer) map[string]int {
	t.Helper()
	rows, err := ts.DB.Query(`SELECT id, position FROM roles WHERE server_id = $1`, w.serverID)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // read-only query in a test

	out := map[string]int{}
	for rows.Next() {
		var id string
		var position int
		require.NoError(t, rows.Scan(&id, &position))
		key, ok := w.keyByID[id]
		require.True(t, ok, "unexpected role %s on the seeded server", id)
		out[key] = position
	}
	require.NoError(t, rows.Err())
	return out
}

// sendAndAssert is the only place this file talks to the server, and it makes
// no decision of its own: it sends the declared ids and compares the response
// and the resulting rows to the declared verdict.
func (w seededWorld) sendAndAssert(t *testing.T, ts *testhelpers.TestServer, keys []string, want contractVerdict) {
	t.Helper()

	res := ts.DoRequest("PATCH", reorderRolesPath(w.serverID),
		map[string]interface{}{"role_ids": w.resolve(t, keys)},
		testhelpers.AuthHeaders(w.actor.AccessToken))
	require.Equal(t, want.Status, res.Code, "server response body: %s", res.Body.String())

	if want.ErrorContains != nil {
		var body map[string]interface{}
		testhelpers.ParseJSON(t, res, &body)
		assert.Contains(t, fmt.Sprint(body["error"]), *want.ErrorContains,
			"the client surfaces this text verbatim, so it is part of the contract")
	}

	got := w.positions(t, ts)
	assert.Equal(t, want.PositionsAfter, got, "role positions after the request")

	for _, collision := range want.Collisions {
		var sharing []string
		for key, position := range got {
			if position == collision.Position {
				sharing = append(sharing, key)
			}
		}
		sort.Strings(sharing)
		expected := append([]string(nil), collision.RoleKeys...)
		sort.Strings(expected)
		assert.Equal(t, expected, sharing,
			"roles sharing position %d — a collision the server committed under HTTP 200", collision.Position)
	}
}

func TestRoleReorderContract(t *testing.T) {
	fixture := loadRoleReorderContract(t)
	ts := testhelpers.SetupTestServer(t)

	for _, sc := range fixture.Scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			if !sc.GoExercisable {
				t.Skipf("scenario %d is client-only: %s", sc.SpecScenario, sc.Title)
			}

			if sc.Payload != nil {
				require.NotNil(t, sc.Server)
				t.Run("declared-payload", func(t *testing.T) {
					// Each request gets its own freshly seeded world: probes
					// that succeed MUTATE positions, and a shared world would
					// make the later assertions depend on execution order.
					seedContractWorld(t, ts, sc.World).sendAndAssert(t, ts, sc.Payload.RoleIDs, *sc.Server)
				})
			}

			for _, probe := range sc.Probes {
				t.Run(probe.ID, func(t *testing.T) {
					t.Logf("probe purpose=%s wrong builder: %s", probe.Purpose, probe.WrongBuilder)
					seedContractWorld(t, ts, sc.World).sendAndAssert(t, ts, probe.RoleIDs, probe.Expected)
				})
			}
		})
	}
}

// TestRoleReorderContract_FalsificationProbesAreDeclared refuses to let the two
// mandatory red demonstrations be deleted. Spec §6: scenario 1 must be shown
// red against a full-display-list builder (#2839's actual behaviour) and
// scenario 6 against a dragged-roles-only builder. A regression test never seen
// failing for the right reason is a doc-pin test, not a logic test — and a
// falsification probe that quietly disappears leaves exactly that behind.
func TestRoleReorderContract_FalsificationProbesAreDeclared(t *testing.T) {
	fixture := loadRoleReorderContract(t)

	required := map[int]string{
		1: "full-display-list",
		6: "dragged-roles-only",
	}
	for specNum, builder := range required {
		found := false
		for _, sc := range fixture.Scenarios {
			if sc.SpecScenario != specNum {
				continue
			}
			for _, probe := range sc.Probes {
				if probe.Purpose != "falsification" {
					continue
				}
				found = true
				assert.Contains(t, probe.WrongBuilder, builder,
					"scenario %d's falsification probe must stand for the %s builder", specNum, builder)

				// The probe must actually disagree with the correct payload,
				// otherwise it falsifies nothing.
				require.NotNil(t, sc.Payload, "scenario %d declares a payload to be falsified against", specNum)
				assert.NotEqual(t, sc.Payload.RoleIDs, probe.RoleIDs,
					"scenario %d: the wrong builder must emit a DIFFERENT payload from the right one", specNum)
				assert.NotEqual(t, *sc.Server, probe.Expected,
					"scenario %d: the wrong payload must produce a different outcome, or the contract does not bite", specNum)
			}
		}
		assert.True(t, found, "spec §6 mandates a falsification probe on scenario %d", specNum)
	}
}
