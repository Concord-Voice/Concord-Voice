package channels_test

// Codex on #3508: when the channel-group epoch guard found no row and the
// server-existence probe after it failed, the write answered its 500 with no
// cause logged anywhere, so the failed authorization read could not be
// diagnosed. The hook ends the hooked transaction's own backend (idle after
// the visibility lock) and answers the guard with no row, so the probe that
// follows really fails.

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

// groupVisibilityLockFragment is the channel-group transaction's first
// statement, so it is the hooked backend's last query before the guard.
const groupVisibilityLockFragment = `pg_advisory_xact_lock`

func TestChannelGroupWrites_FailedServerProbeIsLogged(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	writes := []struct {
		name   string
		method string
		path   func(serverID, groupID string) string
		body   any
		fault  string
	}{
		{
			name: "UpdateChannelGroup", method: http.MethodPatch,
			path: func(serverID, groupID string) string {
				return "/api/v1/servers/" + serverID + "/channel-groups/" + groupID
			},
			body: map[string]any{"name": "renamed"}, fault: `{"error":"Failed to update channel group"}`,
		},
		{
			name: "CreateChannelGroup", method: http.MethodPost,
			path:  func(serverID, _ string) string { return "/api/v1/servers/" + serverID + "/channel-groups" },
			body:  map[string]any{"name": "fresh"},
			fault: `{"error":"Failed to create channel group"}`,
		},
	}
	for _, write := range writes {
		t.Run(write.name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "cgpo"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Probing group server")
			groupID := uuid.NewString()
			_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'probing', 0)`, groupID, serverID)
			require.NoError(t, err)
			terminated := 0
			hook.Arm([]string{groupEpochGuardFragment},
				stmthook.TerminateIdleBackend(ts.DB, groupVisibilityLockFragment, &terminated), sql.ErrNoRows)
			logs := ts.CaptureLogs(t)

			w := ts.DoRequest(write.method, write.path(serverID, groupID), write.body, testhelpers.AuthHeaders(owner.AccessToken))

			seen, betweenErr := hook.Report()
			require.Equal(t, 1, seen, "the hook must fire at the epoch guard")
			require.NoError(t, betweenErr)
			require.Equal(t, 1, terminated, "exactly the hooked backend must be ended")
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.Equal(t, write.fault, w.Body.String())
			assert.Contains(t, logs.String(), "confirm server after a missing epoch-guard row",
				"the failed probe's cause must be logged with the 500")
		})
	}
}
