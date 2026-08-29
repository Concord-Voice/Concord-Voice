package servers_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #2992 wiring lock for the first hand-wired seam, and the arm has to be a
// REFUSED delete for the test to mean anything.
//
// A successful delete already advances the epoch without any bracket at all:
// disconnectServerAudience runs post-commit and calls
// DisconnectRichPresenceClients, which bumps it. An assertion on the successful
// path is therefore satisfied by machinery that shipped in #2975 and says
// nothing about whether this handler took a bracket -- it passes identically
// with the defer deleted. (Observed: the first version of this test was green
// before the production change existed.)
//
// A refused delete is the discriminator. The ownership check runs INSIDE the
// transaction (handlers.go, `SELECT owner_id ... FOR UPDATE` then the 403), and
// the 403 returns long before disconnectServerAudience. So on this path the
// bracket -- taken before BeginTx -- is the ONLY thing that can move the epoch.
func TestDeleteServerBracketsEvenWhenTheDeleteIsRefused(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delfenceowner")
	other := ts.CreateTestUser(t, "delfenceother")
	serverID := ts.CreateTestServer(t, owner.ID, "DelFenceServer")

	beforeEpoch := ts.Hub.PresenceAuthzEpochForTest()
	require.Zero(t, ts.Hub.PresenceAuthzOpenForTest(),
		"precondition: no revocation may be in flight before the request")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
		testhelpers.AuthHeaders(other.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code)

	assert.Greater(t, ts.Hub.PresenceAuthzEpochForTest(), beforeEpoch,
		"the bracket is taken before BeginTx, so it must have advanced the epoch even "+
			"though this request was refused inside the transaction and never reached "+
			"the post-commit disconnect -- if this is equal, the defer is missing")
	assert.Zero(t, ts.Hub.PresenceAuthzOpenForTest(),
		"a refused request must still RELEASE the bracket: a leaked open count "+
			"suppresses base presence hub-wide, permanently, and self-heals never")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM servers WHERE id = $1`, serverID).Scan(&rows))
	require.Equal(t, 1, rows, "control: the refused delete must not have deleted anything")
}

// The success path. The epoch is deliberately NOT asserted here -- it would pass
// on the pre-existing post-commit bump alone (see above). What this locks is the
// RELEASE across a path that commits, cascades and then disconnects, which is
// the longest-lived bracket this handler takes.
func TestDeleteServerReleasesTheBracketOnASuccessfulDelete(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delfenceowner2")
	member := ts.CreateTestUser(t, "delfencemember2")
	serverID := ts.CreateTestServer(t, owner.ID, "DelFenceServer2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	assert.Zero(t, ts.Hub.PresenceAuthzOpenForTest(),
		"the bracket must be released after commit, cascade and disconnect")
}

// The negative control. An invalid path parameter returns before BeginTx is
// reached, so no bracket is taken and NEITHER half moves. Without this, a
// permanently-stuck fence would satisfy both arms above.
func TestDeleteServerTakesNoBracketWhenItRejectsTheIDOutright(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delfenceowner3")

	before := ts.Hub.PresenceAuthzStateForTest()
	w := ts.DoRequest("DELETE", "/api/v1/servers/not-a-uuid", nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code)

	assert.Equal(t, before, ts.Hub.PresenceAuthzStateForTest(),
		"a request rejected on its path parameter never opens a transaction, so it "+
			"must not disturb the fence in either half")
}
