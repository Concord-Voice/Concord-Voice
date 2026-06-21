package servers_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server member receives the server-axis entitlement set. Every server resolves
// to Groundspeed (free) today via the inert Mach hook (#1521).
func TestGetServerEntitlements_MemberGetsGroundspeed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "serventowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Entitlement Server")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/entitlements", nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ent := body["entitlement"].(map[string]interface{})
	assert.Equal(t, "groundspeed", ent["Tier"])
	assert.Equal(t, float64(75), ent["MaxServerCustomEmoji"])
	assert.Equal(t, float64(10), ent["MaxServerStickers"])
	assert.Equal(t, float64(-1), ent["MaxServerStoragePoolBytes"], "storage sentinel (A11-pending)")
	assert.Equal(t, false, ent["UnlockServerAudioQualityCaps"])
	assert.Equal(t, false, ent["UnlockServerVideoQualityCaps"])
}

// A non-member is forbidden — the endpoint mirrors GetServer's membership gate.
func TestGetServerEntitlements_NonMemberForbidden(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "serventowner2")
	serverID := ts.CreateTestServer(t, owner.ID, "Entitlement Server 2")
	outsider := ts.CreateTestUser(t, "serventoutsider")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/entitlements", nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// An invalid (non-UUID) server id is a 400 before any membership lookup.
func TestGetServerEntitlements_InvalidServerID(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "serventbadid")

	w := ts.DoRequest("GET", "/api/v1/servers/not-a-uuid/entitlements", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
