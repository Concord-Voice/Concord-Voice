package members_test

// Regression: AddMember answered 500 when its server was deleted after the
// request's permission check and before its INSERT. The transaction locks only
// the added user's row, so nothing holds the servers row; the INSERT then
// failed server_members.server_id's foreign key (23503). The actor is no longer
// a member of a server that no longer exists, so AddMember now gives the 403 it
// already gives an actor without permission. A 23503 on any other constraint,
// and any other error at that INSERT, stays a 500.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

const (
	addMemberInsertFragment = `INSERT INTO server_members (server_id, user_id, role, joined_at)`
	serverMembersServerFK   = "server_members_server_id_fkey"
)

func classifyAddMember(code int, body string) string {
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == http.StatusForbidden && body == bodyInsufficient:
		return stmthook.Denied
	case code == http.StatusInternalServerError && body == `{"error":"Failed to add member"}`:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, body)
}

// TestAddMember_ServerVanishingBeforeInsert_IsADenial pins the oracle: a server
// deleted before AddMember's INSERT denies the actor (403), while a 23503 on
// another constraint and any other error at that INSERT stay a 500.
func TestAddMember_ServerVanishingBeforeInsert_IsADenial(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{addMemberInsertFragment}

	for _, sc := range stmthook.FKScenarios(serverMembersServerFK) {
		t.Run(sc.Name, func(t *testing.T) {
			suffix := uuid.NewString()[:8]
			owner := ts.CreateTestUser(t, "amvo"+suffix)
			actor := ts.CreateTestUser(t, "amva"+suffix)
			joiner := ts.CreateTestUser(t, "amvj"+suffix)
			serverID := ts.CreateTestServer(t, owner.ID, "Vanishing add-member server")
			ts.AddMemberToServer(t, serverID, actor.ID, "member")
			ts.AssignRoleToUser(t, serverID, actor.ID, ts.CreateTestRole(t, serverID, "inviter", 10, int64(rbac.PermInvite)))
			hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/members",
				map[string]string{"user_id": joiner.ID}, testhelpers.AuthHeaders(actor.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
			assert.Equal(t, sc.Want, classifyAddMember(w.Code, w.Body.String()),
				"AddMember, with the server deleted before its INSERT, must deny rather than report a fault; "+
					"any other error at that INSERT must still be reported")
		})
	}
}
