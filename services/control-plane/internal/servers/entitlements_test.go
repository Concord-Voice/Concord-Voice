package servers_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
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
	assert.Equal(t, float64(15), ent["MaxServerSoundboards"], "founder entitlement matrix baseline")
	assert.Equal(t, float64(33_554_432), ent["MaxServerUploadBytes"], "Groundspeed public 32 MB per-file baseline")
	assert.Equal(t, float64(-1), ent["MaxServerStoragePoolBytes"], "storage sentinel (A11-pending)")
	assert.Equal(t, false, ent["UnlockServerAudioQualityCaps"])
	assert.Equal(t, float64(0), ent["ServerVideoFloorHeight"], "no server video floor on Groundspeed")
}

// Self-hosted deployments resolve to the dedicated selfhost row (ADR-0028):
// uncapped cosmetics/uploads (marketing: hardware-limited), audio unlocked.
func TestGetServerEntitlements_SelfHostedMemberGetsSelfHost(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "serventselfhost")
	serverID := ts.CreateTestServer(t, owner.ID, "Self Hosted Entitlement Server")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/entitlements", nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ent := body["entitlement"].(map[string]interface{})
	assert.Equal(t, "selfhost", ent["Tier"])
	assert.Equal(t, true, ent["UnlockServerAudioQualityCaps"])
	assert.Equal(t, float64(-1), ent["MaxServerCustomEmoji"], "selfhost is uncapped")
	assert.Equal(t, float64(-1), ent["MaxServerUploadBytes"], "selfhost is uncapped")
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
