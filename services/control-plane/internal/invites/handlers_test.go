package invites_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// Helper to create invite and return the code
func createInvite(t *testing.T, ts *testhelpers.TestServer, serverID, token string) string {
	t.Helper()
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses":   0,
		"expires_in": 86400,
	}, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	return invite["code"].(string)
}

// --- Create Invite ---

func TestCreateInviteSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Invite Server")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	assert.NotEmpty(t, invite["code"])
	assert.Equal(t, float64(5), invite["max_uses"])
}

func TestCreateInviteNotAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invown2")
	member := ts.CreateTestUser(t, "invmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Invite No Admin")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- List Invites ---

func TestListInvitesSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listinvown")
	serverID := ts.CreateTestServer(t, owner.ID, "List Invite Server")
	createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/invites", nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invites := body["invites"].([]interface{})
	assert.Len(t, invites, 1)
}

// --- Revoke Invite ---

func TestRevokeInviteSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revokeown")
	serverID := ts.CreateTestServer(t, owner.ID, "Revoke Server")

	// Create and get invite ID
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses": 1,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)

	w = ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/invites/"+inviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Join Server ---

func TestJoinServerSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinown")
	joiner := ts.CreateTestUser(t, "joiner")
	serverID := ts.CreateTestServer(t, owner.ID, "Join Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotNil(t, body["server"])
}

func TestJoinServerAlreadyMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinown2")
	serverID := ts.CreateTestServer(t, owner.ID, "Already In Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestJoinServerInvalidCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badinvite")

	w := ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": "ZZZZZZZZ",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Get Invite Info ---

func TestGetInviteInfoSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "infoown")
	serverID := ts.CreateTestServer(t, owner.ID, "Info Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("GET", "/api/v1/invites/"+code, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Info Server", body["server_name"])
	assert.Equal(t, true, body["valid"])
}

func TestGetInviteInfoInvalidCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "infobad")

	w := ts.DoRequest("GET", "/api/v1/invites/BADCODE1", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
