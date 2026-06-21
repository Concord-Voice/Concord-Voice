package members_test

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

// --- List Members ---

func TestListMembersSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listmemowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Members Server")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/members", nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	assert.Len(t, members, 1) // Just the owner
}

func TestListMembersNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listowner2")
	outsider := ts.CreateTestUser(t, "listoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Private Server")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/members", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Add Member ---

func TestAddMemberAsOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addowner")
	newMember := ts.CreateTestUser(t, "addmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Add Member Server")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/members", map[string]interface{}{
		"user_id": newMember.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestAddMemberAsAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addadminown")
	admin := ts.CreateTestUser(t, "addadmin")
	newMember := ts.CreateTestUser(t, "addadminmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Add Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/members", map[string]interface{}{
		"user_id": newMember.ID,
	}, testhelpers.AuthHeaders(admin.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestAddMemberAsMemberForbidden(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addmemown")
	member := ts.CreateTestUser(t, "addmem1")
	newMember := ts.CreateTestUser(t, "addmem2")
	serverID := ts.CreateTestServer(t, owner.ID, "Member Add Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/members", map[string]interface{}{
		"user_id": newMember.ID,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAddMemberAlreadyMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dupmemown")
	member := ts.CreateTestUser(t, "dupmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Dup Member Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/members", map[string]interface{}{
		"user_id": member.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusConflict, w.Code)
}

// --- Update Member Role ---

func TestUpdateMemberChangeRole(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "roleowner")
	member := ts.CreateTestUser(t, "rolemember")
	serverID := ts.CreateTestServer(t, owner.ID, "Role Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("PATCH", "/api/v1/servers/"+serverID+"/members/"+member.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMemberNotOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "roleown2")
	member := ts.CreateTestUser(t, "rolemem2")
	target := ts.CreateTestUser(t, "roletarget")
	serverID := ts.CreateTestServer(t, owner.ID, "Role Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	// Plain member (no PermManageRolesAssign) tries to change role — expect 403
	w := ts.DoRequest("PATCH", "/api/v1/servers/"+serverID+"/members/"+target.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Remove Member ---

func TestRemoveMemberKickByOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickowner")
	member := ts.CreateTestUser(t, "kickmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Kick Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/members/"+member.ID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRemoveMemberSelfLeave(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "leaveowner")
	member := ts.CreateTestUser(t, "leavemember")
	serverID := ts.CreateTestServer(t, owner.ID, "Leave Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/members/"+member.ID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRemoveMemberOwnerCannotLeave(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ownleave")
	serverID := ts.CreateTestServer(t, owner.ID, "Owner Leave Server")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/members/"+owner.ID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoveMemberMemberCannotKickOthers(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickown2")
	member1 := ts.CreateTestUser(t, "kickmem1")
	member2 := ts.CreateTestUser(t, "kickmem2")
	serverID := ts.CreateTestServer(t, owner.ID, "No Kick Server")
	ts.AddMemberToServer(t, serverID, member1.ID, "member")
	ts.AddMemberToServer(t, serverID, member2.ID, "member")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/members/"+member2.ID, nil, testhelpers.AuthHeaders(member1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
