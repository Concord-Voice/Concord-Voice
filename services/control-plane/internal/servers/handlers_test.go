package servers_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- List Servers ---

func TestListServersEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listuser")

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := body["servers"].([]interface{})
	assert.Empty(t, servers)
}

func TestListServersWithServers(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listuser2")
	ts.CreateTestServer(t, user.ID, "My Server")

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := body["servers"].([]interface{})
	assert.Len(t, servers, 1)
	server := servers[0].(map[string]interface{})
	assert.Equal(t, "My Server", server["name"])
	assert.Equal(t, "owner", server["role"])
}

// --- Create Server ---

func TestCreateServerSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createuser")

	w := ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{
		"name": "New Server",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, "New Server", server["name"])
	assert.Equal(t, "groundspeed", server["server_tier"])
	assert.Equal(t, "owner", body["role"])
}

func TestCreateServerNameTooShort(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "shortname")

	w := ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{
		"name": "ab",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestCreateServerResponseHasNoE2EEDefault locks the #1647 removal: the
// per-server e2ee_default opt-out is gone end-to-end (E2EE-everywhere is a
// structural invariant), so the create response must no longer echo the field.
func TestCreateServerResponseHasNoE2EEDefault(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "e2eeserver")

	w := ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{
		"name": "Encrypted Server",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	_, hasField := server["e2ee_default"]
	assert.False(t, hasField, "create response must not echo the removed e2ee_default field")
}

// --- Get Server ---

func TestGetServerSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getuser")
	serverID := ts.CreateTestServer(t, user.ID, "Get Test")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, "Get Test", server["name"])
}

func TestGetServerNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "owner1")
	outsider := ts.CreateTestUser(t, "outsider1")
	serverID := ts.CreateTestServer(t, owner.ID, "Private Server")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Update Server ---

func TestUpdateServerAsOwner(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updateowner")
	serverID := ts.CreateTestServer(t, user.ID, "Old Name")

	w := ts.DoRequest("PATCH", "/api/v1/servers/"+serverID, map[string]interface{}{
		"name": "New Name",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, "New Name", server["name"])
	assert.Equal(t, "groundspeed", server["server_tier"])
}

func TestUpdateServerNotOwnerOrAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updowner")
	member := ts.CreateTestUser(t, "updmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Restricted")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("PATCH", "/api/v1/servers/"+serverID, map[string]interface{}{
		"name": "Hacked",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Delete Server ---

func TestDeleteServerAsOwner(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delowner")
	serverID := ts.CreateTestServer(t, user.ID, "To Delete")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify it's gone
	w = ts.DoRequest("GET", "/api/v1/servers/"+serverID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestDeleteServerNotOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delnon1")
	member := ts.CreateTestUser(t, "delnon2")
	serverID := ts.CreateTestServer(t, owner.ID, "Protected")
	ts.AddMemberToServer(t, serverID, member.ID, "admin")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Server tier exposure (#179) ---

func TestGetServer_IncludesServerTier(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tier_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Tier Test Server")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, "groundspeed", server["server_tier"], "server_tier must be present and 'groundspeed'")
}

func TestListServers_IncludesServerTier(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tier_list_owner")
	ts.CreateTestServer(t, owner.ID, "Tier List Server")

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := body["servers"].([]interface{})
	assert.NotEmpty(t, servers)
	first := servers[0].(map[string]interface{})
	assert.Equal(t, "groundspeed", first["server_tier"], "server_tier must be present and 'groundspeed' in list")
}
