package rbac_test

// Codex on #3508: past the epoch guard, ReorderRoles' transaction still holds
// nothing that keeps the server: the visibility advisory lock is not taken by
// DeleteServer or by account erasure, and nothing reads the servers or roles
// rows with a lock before the UPDATE. A server deleted there reaches two exits
// that both reported sql.ErrNoRows, which the handler answers as 404 "Role not
// found":
//
//   - the authoritative guard query, whose only row source is the servers row;
//   - the UPDATE, whose roles went with the server (ON DELETE CASCADE), so it
//     matched none of the named roles.
//
// A vanished server is the non-member 403 CreateRole and the earlier reorder
// windows give. TestReorderRoles_UnknownRoleID_NotFound keeps the 404 for a
// role ID that names nothing on a live server.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

const (
	// reorderGuardFragment is unique to reorderGuardQuery. The fast path runs
	// the same query on the pool before the transaction, so each sequence
	// starts at the epoch guard, which only the transaction runs.
	reorderGuardFragment = `SELECT COUNT(*) FROM named n WHERE n.position >= a.max_position`
	// reorderUpdateFragment is applyRolePositions' single UPDATE.
	reorderUpdateFragment = `SET position = u.new_position`
)

func TestReorderRoles_ServerVanishingAfterEpochGuard_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	windows := []struct {
		name     string
		sequence []string
	}{
		{name: "authoritative guard query", sequence: []string{epochGuardFragment, reorderGuardFragment}},
		{name: "reorder UPDATE", sequence: []string{epochGuardFragment, reorderUpdateFragment}},
	}
	for _, win := range windows {
		for _, sc := range stmthook.Scenarios() {
			t.Run(win.name+"/"+sc.Name, func(t *testing.T) {
				// A non-owner actor: the owner's erasure would otherwise wait on
				// the epoch guard's FOR SHARE lock on the actor's own users row.
				suffix := uuid.NewString()[:8]
				owner := ts.CreateTestUser(t, "rrvo"+suffix)
				actor := ts.CreateTestUser(t, "rrva"+suffix)
				serverID := ts.CreateTestServer(t, owner.ID, "Vanishing reorder server")
				ts.AddMemberToServer(t, serverID, actor.ID, "member")
				ts.AssignRoleToUser(t, serverID, actor.ID, ts.CreateTestRole(t, serverID, "roles", 20, int64(rbac.PermManageRoles)))
				roleA := ts.CreateTestRole(t, serverID, "a", 10, 0)
				roleB := ts.CreateTestRole(t, serverID, "b", 11, 0)
				hook.Arm(win.sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

				w := ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+serverID+"/roles/reorder",
					map[string]any{"role_ids": []string{roleB, roleA}}, testhelpers.AuthHeaders(actor.AccessToken))

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(win.sequence), serverID)
				assert.Equal(t, sc.Want, classifyReorderRoles(w.Code, w.Body.String()),
					"ReorderRoles, with the server gone after its epoch guard, must deny rather than report a "+
						"missing role; a real fault at that statement must still be reported")
			})
		}
	}
}
