package rbac_test

// Codex on #3508: ReorderRoles' transaction runs credepoch.GuardTx before
// anything that can see the server is gone. When the OWNER reorders and their
// erasure commits after admission, their users row and (through
// servers.owner_id's ON DELETE CASCADE) the server are both gone, so the
// guard's read found no row. That sql.ErrNoRows then reached the handler's
// "Role not found" arm, which exists for the reorder UPDATE's shortfall, so an
// owner acting mid-erasure was told a role was missing rather than given the
// 403 CreateRole gives in the same window.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

func classifyReorderRoles(code int, body string) string {
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == http.StatusForbidden && body == `{"error":"Insufficient permissions"}`:
		return stmthook.Denied
	case code == http.StatusInternalServerError && body == `{"error":"Failed to reorder roles"}`:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, body)
}

func TestReorderRoles_OwnerErasedBeforeEpochGuard_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{epochGuardFragment}

	scenarios := append(stmthook.Scenarios(), faultAfterOwnerErasure)
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "rrgo"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Vanishing reorder guard server")
			roleA := ts.CreateTestRole(t, serverID, "a", 10, 0)
			roleB := ts.CreateTestRole(t, serverID, "b", 11, 0)
			hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+serverID+"/roles/reorder",
				map[string]any{"role_ids": []string{roleB, roleA}}, testhelpers.AuthHeaders(owner.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
			assert.Equal(t, sc.Want, classifyReorderRoles(w.Code, w.Body.String()),
				"ReorderRoles by the owner, with the server gone before its epoch guard, must deny rather than "+
					"report a missing role; a real fault at that read must still be reported")
		})
	}
}

// Control for the case above: an actor whose row is gone while the server
// still exists is not a vanished server. The guard's failure is a fault, and
// its missing row must not read as the reorder's "Role not found" either.
func TestReorderRoles_ActorErasedOnALiveServer_StaysAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	suffix := uuid.NewString()[:8]
	owner := ts.CreateTestUser(t, "rrlo"+suffix)
	actor := ts.CreateTestUser(t, "rrla"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "Live reorder guard server")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	ts.AssignRoleToUser(t, serverID, actor.ID, ts.CreateTestRole(t, serverID, "roles", 20, int64(rbac.PermManageRoles)))
	roleA := ts.CreateTestRole(t, serverID, "a", 10, 0)
	roleB := ts.CreateTestRole(t, serverID, "b", 11, 0)
	hook.Arm([]string{epochGuardFragment}, func() error {
		_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, actor.ID)
		return err
	}, nil)

	w := ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+serverID+"/roles/reorder",
		map[string]any{"role_ids": []string{roleB, roleA}}, testhelpers.AuthHeaders(actor.AccessToken))

	seen, betweenErr := hook.Report()
	require.Equal(t, 1, seen, "the hook must fire at the epoch guard")
	require.NoError(t, betweenErr)
	assert.Equal(t, stmthook.Fault, classifyReorderRoles(w.Code, w.Body.String()))
}
