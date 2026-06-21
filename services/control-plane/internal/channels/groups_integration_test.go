package channels_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Additional Channel Group Tests for Coverage ---

func TestCreateChannelGroupAdminMember(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	admin := ts.CreateTestUser(t, "groupadmin1")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "Admin Created Group",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateChannelGroupMemberWithPermission(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	member := ts.CreateTestUser(t, "grouppermmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Grant PermManageChannels via custom role
	roleID := ts.CreateTestRole(t, serverID, "ChannelManager", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "Permission Created Group",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestUpdateChannelGroupNameAndPosition(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Original", user.AccessToken)

	newName := "Renamed"
	newPos := 10
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"name":     newName,
		"position": newPos,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := body["channel_group"].(map[string]interface{})
	assert.Equal(t, newName, group["name"])
	assert.Equal(t, float64(newPos), group["position"])
}

func TestUpdateChannelGroupInvalidBody(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "NeedUpdate", user.AccessToken)

	// Send invalid JSON (wrong type for name)
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), "not-json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteChannelGroupAdminMember(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "AdminDelete", user.AccessToken)

	admin := ts.CreateTestUser(t, "groupdeladmin")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Reorder Channels Additional Tests ---

func TestReorderChannelsMultiple(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Reorder Group", user.AccessToken)
	ch1 := ts.CreateTestChannel(t, serverID, "chan-a")
	ch2 := ts.CreateTestChannel(t, serverID, "chan-b")
	ch3 := ts.CreateTestChannel(t, serverID, "chan-c")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": ch1, "group_id": groupID, "position": 2},
			{"channel_id": ch2, "group_id": groupID, "position": 0},
			{"channel_id": ch3, "group_id": groupID, "position": 1},
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify positions were updated
	var pos1, pos2, pos3 int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, ch1).Scan(&pos1))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, ch2).Scan(&pos2))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, ch3).Scan(&pos3))
	assert.Equal(t, 2, pos1)
	assert.Equal(t, 0, pos2)
	assert.Equal(t, 1, pos3)
}

func TestReorderChannelsNilGroupID(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	ch := ts.CreateTestChannel(t, serverID, "ungrouped-chan")

	// Set group_id to nil (uncategorized)
	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": ch, "group_id": nil, "position": 0},
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReorderChannelsAdminMember(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	ch := ts.CreateTestChannel(t, serverID, "admin-reorder-chan")

	admin := ts.CreateTestUser(t, "reorderadmin")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": ch, "group_id": nil, "position": 5},
		},
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	_ = user // owner created server but admin performs the reorder
}

func TestListChannelGroupsMultipleOrdered(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	// Create groups in order
	g1 := createGroup(t, ts, serverID, "Alpha", user.AccessToken)
	g2 := createGroup(t, ts, serverID, "Bravo", user.AccessToken)
	g3 := createGroup(t, ts, serverID, "Charlie", user.AccessToken)

	// Reorder: Charlie first, Alpha second, Bravo third
	_, err := ts.DB.Exec(`UPDATE channel_groups SET position = 0 WHERE id = $1`, g3)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE channel_groups SET position = 1 WHERE id = $1`, g1)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE channel_groups SET position = 2 WHERE id = $1`, g2)
	require.NoError(t, err)

	w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	groups := body["channel_groups"].([]interface{})
	require.Len(t, groups, 3)

	assert.Equal(t, "Charlie", groups[0].(map[string]interface{})["name"])
	assert.Equal(t, "Alpha", groups[1].(map[string]interface{})["name"])
	assert.Equal(t, "Bravo", groups[2].(map[string]interface{})["name"])
}

func TestCreateChannelGroupNameTooLong(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	// Max is 100 characters per validation
	longName := ""
	for i := 0; i < 101; i++ {
		longName += "x"
	}

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": longName,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateChannelGroupAdminWithPermission(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "AdminUpdateTarget", user.AccessToken)

	member := ts.CreateTestUser(t, "updperm")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Grant PermManageChannels
	roleID := ts.CreateTestRole(t, serverID, "ChanManager", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)

	newName := "UpdatedByPerm"
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"name": newName,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteChannelGroupMemberWithPermission(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "PermDelete", user.AccessToken)

	member := ts.CreateTestUser(t, "delperm")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	roleID := ts.CreateTestRole(t, serverID, "ChanDeleter", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
