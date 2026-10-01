package channels_test

// Regression: UpdateChannelGroup answered 500 when its server was deleted after
// the preflight read of the group and before the transaction's server lock
// (`SELECT id FROM servers WHERE id = $1 FOR UPDATE`, authorizeChannelGroupUpdateTx).
// Only the visibility advisory lock and the credential-epoch FOR SHARE precede
// that lock, and neither holds the servers row, so DeleteServer or the owner's
// erasure could commit in between. Every error at the lock was a 500.
//
// The server's deletion cascades to its channel groups, so the handler now
// gives the answer it already gives when the group is gone at the preflight
// read: 404 "Channel group not found".

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

const groupServerLockFragment = `SELECT id FROM servers WHERE id = $1 FOR UPDATE`

// groupEpochGuardFragment is credepoch.GuardTx's read of the acting user's row,
// which both channel-group transactions take before their server lock.
const groupEpochGuardFragment = `SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`

func classifyGroupCreate(code int, body string) string {
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == http.StatusNotFound && body == `{"error":"Server not found"}`:
		return stmthook.Denied
	case code == http.StatusInternalServerError && body == `{"error":"Failed to create channel group"}`:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, body)
}

func classifyGroupUpdate(code int, body string) string {
	switch {
	case code >= 200 && code < 300:
		return stmthook.Allowed
	case code == http.StatusNotFound && body == `{"error":"Channel group not found"}`:
		return stmthook.Denied
	case code == http.StatusInternalServerError && body == `{"error":"Failed to update channel group"}`:
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected %d %s", code, body)
}

// TestUpdateChannelGroup_ServerVanishingBeforeServerLock_IsNotFound pins the
// oracle: a server deleted before UpdateChannelGroup locks it answers the
// handler's own "group gone" 404, while a real fault at that lock stays a 500.
func TestUpdateChannelGroup_ServerVanishingBeforeServerLock_IsNotFound(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{groupServerLockFragment}

	for _, sc := range stmthook.Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			suffix := uuid.NewString()[:8]
			owner := ts.CreateTestUser(t, "cgvo"+suffix)
			actor := ts.CreateTestUser(t, "cgva"+suffix)
			serverID := ts.CreateTestServer(t, owner.ID, "Vanishing group server")
			ts.AddMemberToServer(t, serverID, actor.ID, "member")
			ts.AssignRoleToUser(t, serverID, actor.ID, ts.CreateTestRole(t, serverID, "channels", 10, int64(rbac.PermManageChannels)))
			groupID := uuid.NewString()
			_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'vanishing', 0)`, groupID, serverID)
			require.NoError(t, err)
			hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+serverID+"/channel-groups/"+groupID,
				map[string]any{"name": "renamed"}, testhelpers.AuthHeaders(actor.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
			assert.Equal(t, sc.Want, classifyGroupUpdate(w.Code, w.Body.String()),
				"UpdateChannelGroup, with the server deleted before its server lock, must answer not-found rather than "+
					"report a fault; a real fault at that lock must still be reported")
		})
	}
}

// Codex on #3508: both channel-group transactions reach their vanished-server
// answer only after credepoch.GuardTx, which reads the acting user's row first.
// When the OWNER acts and their erasure commits after the preflight, their row
// and (through servers.owner_id's ON DELETE CASCADE) the server and its groups
// are all gone, so the guard found no row and both handlers answered 500. The
// case above acts as a non-owner. Here the owner acts and the hook fires at the
// guard.
func TestChannelGroupWrites_OwnerErasedBeforeEpochGuard_IsNotFound(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sequence := []string{groupEpochGuardFragment}

	writes := []struct {
		name     string
		method   string
		path     func(serverID, groupID string) string
		body     any
		classify func(int, string) string
	}{
		{
			name: "UpdateChannelGroup", method: http.MethodPatch,
			path: func(serverID, groupID string) string {
				return "/api/v1/servers/" + serverID + "/channel-groups/" + groupID
			},
			body: map[string]any{"name": "renamed"}, classify: classifyGroupUpdate,
		},
		{
			name: "CreateChannelGroup", method: http.MethodPost,
			path: func(serverID, _ string) string { return "/api/v1/servers/" + serverID + "/channel-groups" },
			body: map[string]any{"name": "fresh"}, classify: classifyGroupCreate,
		},
	}
	for _, write := range writes {
		for _, sc := range stmthook.Scenarios() {
			t.Run(write.name+"/"+sc.Name, func(t *testing.T) {
				owner := ts.CreateTestUser(t, "cgeo"+uuid.NewString()[:8])
				serverID := ts.CreateTestServer(t, owner.ID, "Vanishing group guard server")
				groupID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'vanishing', 0)`, groupID, serverID)
				require.NoError(t, err)
				hook.Arm(sequence, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

				w := ts.DoRequest(write.method, write.path(serverID, groupID), write.body, testhelpers.AuthHeaders(owner.AccessToken))

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), serverID)
				assert.Equal(t, sc.Want, write.classify(w.Code, w.Body.String()),
					"%s by the owner, with the server gone before its epoch guard, must answer not-found rather "+
						"than report a fault; a real fault at that read must still be reported", write.name)
			})
		}
	}
}

// Control for TestChannelGroupWrites_OwnerErasedBeforeEpochGuard_IsNotFound:
// the owner's erasure took the server, but the guard failed with a real fault
// rather than a missing row. Only the missing row may read as the vanished
// server; a fault that coincides with the erasure must still be reported.
func TestChannelGroupWrites_FaultAtEpochGuardAfterOwnerErasure_StaysAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)
	sc := stmthook.Scenario{
		Name:        "control: a real fault at the guard after the owner's erasure",
		DeleteSQL:   `DELETE FROM users WHERE id = $1`,
		DeleteOwner: true,
		Fault:       stmthook.ErrInjected,
		Want:        stmthook.Fault,
	}

	writes := []struct {
		name     string
		method   string
		path     func(serverID, groupID string) string
		body     any
		classify func(int, string) string
	}{
		{
			name: "UpdateChannelGroup", method: http.MethodPatch,
			path: func(serverID, groupID string) string {
				return "/api/v1/servers/" + serverID + "/channel-groups/" + groupID
			},
			body: map[string]any{"name": "renamed"}, classify: classifyGroupUpdate,
		},
		{
			name: "CreateChannelGroup", method: http.MethodPost,
			path: func(serverID, _ string) string { return "/api/v1/servers/" + serverID + "/channel-groups" },
			body: map[string]any{"name": "fresh"}, classify: classifyGroupCreate,
		},
	}
	for _, write := range writes {
		t.Run(write.name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "cgfo"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Faulting group guard server")
			groupID := uuid.NewString()
			_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'faulting', 0)`, groupID, serverID)
			require.NoError(t, err)
			hook.Arm([]string{groupEpochGuardFragment}, sc.Between(ts.DB, owner.ID, serverID), sc.Fault)

			w := ts.DoRequest(write.method, write.path(serverID, groupID), write.body, testhelpers.AuthHeaders(owner.AccessToken))

			stmthook.RequireInterleaved(t, ts.DB, hook, sc, 1, serverID)
			assert.Equal(t, sc.Want, write.classify(w.Code, w.Body.String()), write.name)
		})
	}
}
