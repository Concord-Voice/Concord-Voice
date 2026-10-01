package rbac_test

// Regression: applyRolePositions, ReorderRoles' transaction, discarded its
// deferred rollback's error, so a denial the transaction reached stood even
// when the rollback then failed. A failed discard of an interrupted roles
// change is a fault, as it is for CreateRole and withAuthorityCapture.
//
// The hook answers the in-transaction permission check's server read with no
// row, which the resolver reads as ErrNotMember (a 403), after ending the
// hooked transaction's own backend; the control leaves the connection alive.

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

const (
	// resolverOwnerRead is resolveServerPermissions' server read; after the
	// epoch guard it is the in-transaction permission check's.
	resolverOwnerRead = `SELECT owner_id, enforce_mfa_dangerous_actions FROM servers WHERE id = $1`
	// resolverMemberRead precedes it, so it is the hooked backend's last query.
	resolverMemberRead = `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`
)

func TestReorderRoles_FailedDiscardAfterDenial_IsAFault(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	cases := []struct {
		name      string
		terminate bool
		wantCode  int
		wantBody  string
	}{
		{name: "control: the discard succeeds", wantCode: http.StatusForbidden, wantBody: `{"error":"Insufficient permissions"}`},
		{name: "the discard fails", terminate: true, wantCode: http.StatusInternalServerError, wantBody: `{"error":"Failed to reorder roles"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := ts.CreateTestUser(t, "rrdo"+uuid.NewString()[:8])
			serverID := ts.CreateTestServer(t, owner.ID, "Discarding reorder server")
			roleA := ts.CreateTestRole(t, serverID, "a", 10, 0)
			roleB := ts.CreateTestRole(t, serverID, "b", 11, 0)
			terminated := 0
			var between func() error
			if tc.terminate {
				between = stmthook.TerminateIdleBackend(ts.DB, resolverMemberRead, &terminated)
			}
			hook.Arm([]string{epochGuardFragment, resolverOwnerRead}, between, sql.ErrNoRows)

			w := ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+serverID+"/roles/reorder",
				map[string]any{"role_ids": []string{roleB, roleA}}, testhelpers.AuthHeaders(owner.AccessToken))

			seen, betweenErr := hook.Report()
			require.Equal(t, 2, seen, "the hook must fire at the in-transaction server read")
			require.NoError(t, betweenErr)
			if tc.terminate {
				require.Equal(t, 1, terminated, "exactly the hooked backend must be ended")
			}
			assert.Equal(t, tc.wantCode, w.Code)
			assert.Equal(t, tc.wantBody, w.Body.String())
		})
	}
}
