package rbac_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Exact family share-lock statement credepoch.GuardFamilyTx issues after GuardTx.
const familyGuardSQL = `SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2 FOR SHARE`

// #2272: a revoke-one that commits after the request was admitted, while the
// epoch stays current, must stop the in-flight RBAC write at its family guard.
// The pre-family GuardTx admitted this write (same epoch) and persisted it.
func TestUpsertChannelOverrideRejectsRevokedSessionFamily(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	owner := ts.CreateTestUser(t, "family-override-owner")
	member := ts.CreateTestUser(t, "family-override-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Family override server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "family-revoked-override")

	require.NoError(t, boundary.ArmRevocation(owner.AccessToken))
	boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`, familyGuardSQL}, owner.ID, nil, nil)
	w := ts.DoRequest(http.MethodPut, channelOverridesPath(channelID), rbac.UpsertOverrideRequest{
		TargetType: "user",
		TargetID:   member.ID,
		Allow:      int64(rbac.PermSendMessages),
	}, testhelpers.AuthHeaders(owner.AccessToken))
	boundary.RequireRevoked(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, 2, seen, "the write's epoch guard then family guard must run after the revocation committed")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "a write in flight when its session family was revoked must not commit")
}
