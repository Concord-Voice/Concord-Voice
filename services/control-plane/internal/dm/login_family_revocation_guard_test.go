package dm_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const dmFamilyGuardSQL = `SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2 FOR SHARE`

// #2272: a revoke-one committed after admission, with the epoch unchanged,
// must stop the in-flight DM write at its family guard.
func TestOpenConversationRejectsRevokedSessionFamily(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	caller := ts.CreateTestUser(t, "familyopencreator")
	target := ts.CreateTestUser(t, "familyopentarget")
	ts.CreateFriendship(t, caller.ID, target.ID, "accepted")

	require.NoError(t, boundary.ArmRevocation(caller.AccessToken))
	boundary.Hook.ArmArg([]string{dmEpochGuardSQL, dmFamilyGuardSQL}, caller.ID, nil, nil)
	w := ts.DoRequest(http.MethodPost, pathDMConversations, map[string]string{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(caller.AccessToken))
	boundary.RequireRevoked(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, 2, seen, "the write's epoch guard then family guard must run after the revocation committed")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_conversations WHERE created_by = $1`, caller.ID,
	).Scan(&count))
	assert.Zero(t, count, "a write in flight when its session family was revoked must not commit")
}
