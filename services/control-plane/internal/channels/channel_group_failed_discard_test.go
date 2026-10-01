package channels_test

// Codex's sixth review of #3508: both channel-group writes wrote their refusal
// inside the transaction, and the deferred rollback only logged a failure, so
// an unresolved transaction was reported as a clean 404. The hook ends the
// hooked transaction's own backend (idle after the epoch guard) and answers the
// server lock with no row; the control leaves the connection alive.

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

func TestChannelGroupWrites_FailedDiscardAfterServerGone_IsAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	writes := []struct {
		name     string
		method   string
		path     func(serverID, groupID string) string
		body     any
		goneBody string
		fault    string
	}{
		{
			name: "UpdateChannelGroup", method: http.MethodPatch,
			path: func(serverID, groupID string) string {
				return "/api/v1/servers/" + serverID + "/channel-groups/" + groupID
			},
			body:     map[string]any{"name": "renamed"},
			goneBody: `{"error":"Channel group not found"}`, fault: `{"error":"Failed to update channel group"}`,
		},
		{
			name: "CreateChannelGroup", method: http.MethodPost,
			path:     func(serverID, _ string) string { return "/api/v1/servers/" + serverID + "/channel-groups" },
			body:     map[string]any{"name": "fresh"},
			goneBody: `{"error":"Server not found"}`, fault: `{"error":"Failed to create channel group"}`,
		},
	}
	for _, write := range writes {
		for _, terminate := range []bool{false, true} {
			name, wantCode, wantBody := write.name+"/control: the discard succeeds", http.StatusNotFound, write.goneBody
			if terminate {
				name, wantCode, wantBody = write.name+"/the discard fails", http.StatusInternalServerError, write.fault
			}
			t.Run(name, func(t *testing.T) {
				owner := ts.CreateTestUser(t, "cgdo"+uuid.NewString()[:8])
				serverID := ts.CreateTestServer(t, owner.ID, "Discarding group server")
				groupID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'discarding', 0)`, groupID, serverID)
				require.NoError(t, err)
				terminated := 0
				var between func() error
				if terminate {
					between = stmthook.TerminateIdleBackend(ts.DB, groupEpochGuardFragment, &terminated)
				}
				hook.Arm([]string{groupEpochGuardFragment, groupServerLockFragment}, between, sql.ErrNoRows)

				w := ts.DoRequest(write.method, write.path(serverID, groupID), write.body, testhelpers.AuthHeaders(owner.AccessToken))

				seen, betweenErr := hook.Report()
				require.Equal(t, 2, seen, "the hook must fire at the server lock")
				require.NoError(t, betweenErr)
				if terminate {
					require.Equal(t, 1, terminated, "exactly the hooked backend must be ended")
				}
				assert.Equal(t, wantCode, w.Code)
				assert.Equal(t, wantBody, w.Body.String())
			})
		}
	}
}
