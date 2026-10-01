package rbac_test

// Regression: CreateRole answered 500 when its server was deleted after the
// transaction's permission resolve and before its position snapshot. The
// snapshot reads the owner through a scalar subquery, so an absent servers row
// does not surface as sql.ErrNoRows: it comes back as a NULL owner_id, which
// failed the Scan into a string. servers.owner_id is NOT NULL, so that NULL
// can only mean "no servers row" — the actor is no longer a member, which is
// the 403 CreateRole already gives a non-member.
//
// Nothing in CreateRole's transaction holds the servers row: the visibility
// advisory lock, the credential-epoch FOR SHARE on the actor's users row, and
// the resolver's unlocked reads all leave DeleteServer and the owner's erasure
// free to commit in between.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

// createRoleSnapshotFragment is unique to resolveNewRolePosition's snapshot.
const createRoleSnapshotFragment = `(SELECT owner_id FROM servers WHERE id = $1),`

// createRoleInsertFragment is CreateRole's INSERT, and roleServerFK the FK it
// fails when its server is gone.
const (
	createRoleInsertFragment = `INSERT INTO roles (id, server_id, name`
	roleServerFK             = "roles_server_id_fkey"
)

// epochGuardFragment is credepoch.GuardTx's read of the actor's users row.
const epochGuardFragment = `SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`

func classifyCreateRole(code int, body string) string {
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == http.StatusForbidden && body == `{"error":"Insufficient permissions"}`:
		return stmthook.Denied
	case code == http.StatusInternalServerError && body == `{"error":"Failed to create role"}`:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, body)
}

// TestCreateRole_ServerVanishingBeforePositionSnapshot_IsADenial pins the
// oracle: a server deleted before CreateRole's position snapshot denies the
// actor as a non-member (403), while a real fault at that read stays a 500.
func TestCreateRole_ServerVanishingBeforePositionSnapshot_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{createRoleSnapshotFragment}

	for _, sc := range stmthook.Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			suffix := uuid.NewString()[:8]
			owner := ts.CreateTestUser(t, "crvo"+suffix)
			actor := ts.CreateTestUser(t, "crva"+suffix)
			serverID := ts.CreateTestServer(t, owner.ID, "Vanishing role server")
			ts.AddMemberToServer(t, serverID, actor.ID, "member")
			ts.AssignRoleToUser(t, serverID, actor.ID, ts.CreateTestRole(t, serverID, "roles", 10, int64(rbac.PermManageRoles)))
			hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/roles",
				map[string]any{"name": "fresh", "permissions": "0"}, testhelpers.AuthHeaders(actor.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
			assert.Equal(t, sc.Want, classifyCreateRole(w.Code, w.Body.String()),
				"CreateRole, with the server deleted before its position snapshot, must deny rather than report a fault; "+
					"a real fault at that read must still be reported")
		})
	}
}

// TestCreateRole_ServerVanishingBeforeInsert_IsADenial is the snapshot repro's
// second window: the server survives the position snapshot and is deleted
// before the INSERT, which then fails roles.server_id's foreign key (23503).
// That is the same fact the snapshot path answers with a 403, so it gets the
// same answer; a 23503 on any other constraint, and any other error, stays 500.
//
// Only the OWNER reaches this window. A non-owner's slot shifts every role at
// or above theirs (`UPDATE roles SET position = position + 1 …`), and those row
// locks make a server delete's cascade into roles wait for this transaction —
// measured: the interleaved DELETE times out with 55P03. And the owner's own
// erasure cannot land here either, because GuardTx holds the owner's users row
// FOR SHARE. What remains is the owner's server being deleted by another of the
// owner's requests (DeleteServer) between the snapshot and the INSERT.
func TestCreateRole_ServerVanishingBeforeInsert_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{createRoleSnapshotFragment, createRoleInsertFragment}

	for _, sc := range stmthook.FKScenarios(roleServerFK) {
		if sc.DeleteOwner {
			continue // unreachable in this window; see above
		}
		t.Run(sc.Name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "crio"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Vanishing role insert server")
			hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/roles",
				map[string]any{"name": "fresh", "permissions": "0"}, testhelpers.AuthHeaders(owner.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
			assert.Equal(t, sc.Want, classifyCreateRole(w.Code, w.Body.String()),
				"CreateRole, with the server deleted before its INSERT, must deny rather than report a fault; "+
					"any other error at that INSERT must still be reported")
		})
	}
}

// Codex on #3508: the vanished-server answers above come after
// credepoch.GuardTx, which reads the acting user's row first. When the OWNER
// creates a role and their erasure commits after admission, their row and
// (through servers.owner_id's ON DELETE CASCADE) the server are both gone, so
// the guard's read found no row and CreateRole answered 500 without reaching
// the 403. The cases above act as a non-owner or hook later.
func TestCreateRole_OwnerErasedBeforeEpochGuard_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{epochGuardFragment}

	for _, sc := range stmthook.Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "crgo"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Vanishing role guard server")
			hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/roles",
				map[string]any{"name": "fresh", "permissions": "0"}, testhelpers.AuthHeaders(owner.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
			assert.Equal(t, sc.Want, classifyCreateRole(w.Code, w.Body.String()),
				"CreateRole by the owner, with the server gone before its epoch guard, must deny rather than report "+
					"a fault; a real fault at that read must still be reported")
		})
	}
}

// Control for the case above: an actor whose row is gone while the server
// still exists is not a vanished server, and the guard's failure stays a 500.
// A fix that read every missing guard row as a denial would answer 403.
func TestCreateRole_ActorErasedOnALiveServer_StaysAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "crlo"+suffix)
	actor := ts.CreateTestUser(t, "crla"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "Live role guard server")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	ts.AssignRoleToUser(t, serverID, actor.ID, ts.CreateTestRole(t, serverID, "roles", 10, int64(rbac.PermManageRoles)))
	hook.Arm([]string{epochGuardFragment}, func() error {
		_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, actor.ID)
		return err
	}, nil)

	w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/roles",
		map[string]any{"name": "fresh", "permissions": "0"}, testhelpers.AuthHeaders(actor.AccessToken))

	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "the hook must fire at the epoch guard")
	require.NoError(t, betweenErr)
	assert.Equal(t, stmthook.Fault, classifyCreateRole(w.Code, w.Body.String()))
}

// faultAfterOwnerErasure erases the owner, which takes the server with it, and
// then fails the hooked statement with a real fault. Only a missing row at the
// epoch guard may read as the vanished server; a fault that merely coincides
// with the erasure must still be reported, or an outage on a deleted server
// would answer as a denial.
var faultAfterOwnerErasure = stmthook.Scenario{
	Name:        "control: a real fault at the guard after the owner's erasure",
	DeleteSQL:   `DELETE FROM users WHERE id = $1`,
	DeleteOwner: true,
	Fault:       stmthook.ErrInjected,
	Want:        stmthook.Fault,
}

// Control for TestCreateRole_OwnerErasedBeforeEpochGuard_IsADenial: the server
// is gone, but the guard failed with a fault rather than a missing row.
func TestCreateRole_FaultAtEpochGuardAfterOwnerErasure_StaysAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sc := faultAfterOwnerErasure
	owner := ts.CreateTestUser(t, "crfo"+uuid.NewString()[:8])
	serverID := ts.CreateTestServer(t, owner.ID, "Faulting role guard server")
	hook.Arm([]string{epochGuardFragment}, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

	w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/roles",
		map[string]any{"name": "fresh", "permissions": "0"}, testhelpers.AuthHeaders(owner.AccessToken))

	stmthook.RequireInterleaved(t, ts.DB, hook, sc, 1, serverID)
	assert.Equal(t, sc.Want, classifyCreateRole(w.Code, w.Body.String()))
}

// Codex on #3508: CreateRole's deferred rollback discarded its error, so when
// the INSERT failed on roles.server_id's FK and the rollback then failed too,
// the request still answered the clean 403. A failed discard of an interrupted
// roles change is a fault, as it is for every gated graph mutation (d2e424e).
//
// The hook ends the hooked transaction's own backend and then answers the
// INSERT with that FK violation, so the classified denial is followed by a
// rollback on a dead connection. The control injects the same violation and
// leaves the connection alive, so the rollback succeeds and the 403 stands.
func TestCreateRole_FailedDiscardAfterServerFK_IsAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	fkViolation := &pq.Error{Code: "23503", Constraint: roleServerFK}

	cases := []struct {
		name      string
		terminate bool
		want      string
	}{
		{name: "control: the discard succeeds", want: stmthook.Denied},
		{name: "the discard fails", terminate: true, want: stmthook.Fault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "crdo"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Discarding role server")
			var terminated int
			var between func() error
			if tc.terminate {
				// The hooked connection sits idle in its transaction with the
				// position snapshot as its last statement.
				between = stmthook.TerminateIdleBackend(ts.DB, createRoleSnapshotFragment, &terminated)
			}
			hook.Arm([]string{createRoleSnapshotFragment, createRoleInsertFragment}, between, fkViolation)

			w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/roles",
				map[string]any{"name": "fresh", "permissions": "0"}, testhelpers.AuthHeaders(owner.AccessToken))

			seen, betweenErr := hook.Report()
			require.Equal(t, 2, seen, "the hook must fire at the INSERT")
			require.NoError(t, betweenErr)
			if tc.terminate {
				require.Equal(t, 1, terminated, "exactly the hooked backend must be ended")
			}
			assert.Equal(t, tc.want, classifyCreateRole(w.Code, w.Body.String()))
		})
	}
}
