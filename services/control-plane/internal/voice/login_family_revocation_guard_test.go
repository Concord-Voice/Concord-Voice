package voice_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #2272: a revoke-one committed after admission, with the epoch unchanged,
// must stop the in-flight voice moderation write at its family guard.
func TestServerMuteRejectsRevokedSessionFamily(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	owner := ts.CreateTestUser(t, "family_voice_moderator")
	target := ts.CreateTestUser(t, "family_voice_target")
	serverID := ts.CreateTestServer(t, owner.ID, "family voice authority")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	require.NoError(t, boundary.ArmRevocation(owner.AccessToken))
	sequence := []string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`, serverMediaAdmissionFamilyGuardSQL}
	boundary.Hook.ArmArg(sequence, owner.ID, nil, nil)
	w := ts.DoRequest(http.MethodPost,
		"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/mute",
		nil, testhelpers.AuthHeaders(owner.AccessToken))
	boundary.RequireRevoked(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, len(sequence), seen, "the write's epoch guard then family guard must run after the revocation committed")
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var muted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, target.ID,
	).Scan(&muted))
	assert.False(t, muted, "a write in flight when its session family was revoked must not commit")
	requireNoServerOutboxCommands(t, ts.DB)
}
