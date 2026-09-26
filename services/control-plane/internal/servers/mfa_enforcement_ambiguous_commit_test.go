package servers_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression for the #3453 Phase-4 red-team finding F2. A Commit() error does
// not prove a rollback: the server can apply the COMMIT and lose only its
// acknowledgement (spec correction C-3). The MFA factor hooks bump the user's
// permission generation whatever Commit returns; the toggle returned on the
// commit error before bumping the server's, so an ON that had in fact applied
// left every member's cached, unmasked value in service for the cache TTL,
// and an unenrolled Administrator could still rename the enforcing server.
//
// The commit seam below commits for real and then reports an error: the lost
// acknowledgement, exactly.
func TestMFAEnforcement_LostCommitAckStillRefreshesPermissions(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfaack", false)
	enrollMFATOTP(t, env, f.owner.ID)

	perms := func() rbac.Permission {
		t.Helper()
		w := env.ts.DoRequest(http.MethodGet, "/api/v1/servers/"+f.serverID+"/permissions", nil,
			testhelpers.AuthHeaders(f.admin.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			Permissions int64 `json:"permissions"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return rbac.Permission(body.Permissions)
	}

	// Cache the unenrolled Administrator's unmasked value.
	require.True(t, perms().Has(rbac.PermManageServer), "setup: unmasked before the flip")
	require.True(t, perms().Has(rbac.PermManageServer), "setup: the cached value is served")
	env.events.take()

	servers.SetMFAEnforcementCommitForTest(t, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("simulated lost commit acknowledgement")
	})
	on := putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusInternalServerError, on.Code, on.Body.String())
	require.True(t, readMFAFlag(t, env, f.serverID), "setup: the commit applied, so the server now enforces")

	assert.False(t, perms().Has(rbac.PermManageServer),
		"after a commit that applied ON, the next read must be masked even though Commit reported an error")
	patch := env.ts.DoRequest(http.MethodPatch, "/api/v1/servers/"+f.serverID,
		map[string]any{"name": "renamed-after-lost-ack"}, testhelpers.AuthHeaders(f.admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, patch.Code,
		"an unenrolled Administrator must not act on a server whose ON committed: %s", patch.Body.String())

	// The refresh is safe in both outcomes; a success event is not. The route
	// answered 500, so nothing may report the change as having happened.
	for _, ev := range env.events.take() {
		assert.NotEqual(t, securityevent.OutcomeSuccess, ev.Outcome, "no success event without a known commit: %+v", ev)
	}
}
