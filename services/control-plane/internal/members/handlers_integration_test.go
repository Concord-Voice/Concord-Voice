package members_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	notAUUID   = "not-a-uuid"
	fakeUUID1  = "00000000-0000-0000-0000-000000000001"
	fakeUUID2  = "00000000-0000-0000-0000-000000000002"
	fakeUUID99 = "00000000-0000-0000-0000-000000000099"
)

// Note: setupTS is defined in handlers_test.go.

func membersPath(serverID string) string {
	return fmt.Sprintf("/api/v1/servers/%s/members", serverID)
}

func memberPath(serverID, userID string) string {
	return fmt.Sprintf("/api/v1/servers/%s/members/%s", serverID, userID)
}

func timeoutPath(serverID, userID string) string {
	return fmt.Sprintf("/api/v1/servers/%s/members/%s/timeout", serverID, userID)
}

func banPath(serverID, userID string) string {
	return fmt.Sprintf("/api/v1/servers/%s/bans/%s", serverID, userID)
}

func bansPath(serverID string) string {
	return fmt.Sprintf("/api/v1/servers/%s/bans", serverID)
}

// ── ListMembers (extended) ───────────────────────────────────────────────────

func TestListMembersReturnsUserDetails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "detailowner")

	serverID := ts.CreateTestServer(t, owner.ID, "DetailServer")

	w := ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	require.Len(t, members, 1)

	m := members[0].(map[string]interface{})
	assert.Equal(t, owner.ID, m["user_id"])
	assert.NotEmpty(t, m["username"])
	assert.NotEmpty(t, m["role"])
	assert.NotEmpty(t, m["joined_at"])
	assert.NotNil(t, m["roles"])
}

func TestListMembersInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lminvalid")

	w := ts.DoRequest("GET", membersPath(notAUUID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestListMembersUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", membersPath(fakeUUID1), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestListMembersOwnerRoleMaskedForNonOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "maskowner")
	member := ts.CreateTestUser(t, "maskmember")

	serverID := ts.CreateTestServer(t, owner.ID, "MaskServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	for _, m := range members {
		mem := m.(map[string]interface{})
		if mem["user_id"] == owner.ID {
			assert.NotEqual(t, "owner", mem["role"], "owner role should be masked for non-owner viewer")
		}
	}
}

func TestListMembersOwnerSeesOwnRole(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ownerself")

	serverID := ts.CreateTestServer(t, owner.ID, "OwnerViewServer")

	w := ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	for _, m := range members {
		mem := m.(map[string]interface{})
		if mem["user_id"] == owner.ID {
			assert.Equal(t, "owner", mem["role"], "owner should see their own 'owner' role")
		}
	}
}

func TestListMembersRolesArrayNeverNil(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "rolesnilown")
	member := ts.CreateTestUser(t, "rolesnilmem")

	serverID := ts.CreateTestServer(t, owner.ID, "RolesNilServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	for _, m := range members {
		mem := m.(map[string]interface{})
		assert.NotNil(t, mem["roles"], "roles should never be nil (should be empty array)")
	}
}

func TestListMembersMultipleMembers(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "multiown")
	m1 := ts.CreateTestUser(t, "multimem1")
	m2 := ts.CreateTestUser(t, "multimem2")

	serverID := ts.CreateTestServer(t, owner.ID, "MultiServer")
	ts.AddMemberToServer(t, serverID, m1.ID, "member")
	ts.AddMemberToServer(t, serverID, m2.ID, "member")

	w := ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	assert.Len(t, members, 3) // owner + 2 members
}

// ── AddMember (extended) ─────────────────────────────────────────────────────

func TestAddMemberUserNotFound(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addnotfound")

	serverID := ts.CreateTestServer(t, owner.ID, "NotFoundServer")

	payload := map[string]interface{}{
		"user_id": fakeUUID99,
	}
	w := ts.DoRequest("POST", membersPath(serverID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAddMemberInvalidBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addinvbody")

	serverID := ts.CreateTestServer(t, owner.ID, "InvBodyServer")

	w := ts.DoRequest("POST", membersPath(serverID), map[string]interface{}{"user_id": "not-uuid"}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAddMemberInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "addinvsrv")

	w := ts.DoRequest("POST", membersPath(notAUUID), map[string]interface{}{"user_id": user.ID}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAddMemberBannedUser(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addbanown")
	banned := ts.CreateTestUser(t, "addbanned")

	serverID := ts.CreateTestServer(t, owner.ID, "BannedAddServer")

	_, err := ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by) VALUES ($1, $2, $3)`,
		serverID, banned.ID, owner.ID,
	)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"user_id": banned.ID,
	}
	w := ts.DoRequest("POST", membersPath(serverID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAddMemberUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", membersPath(fakeUUID1), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAddMemberReturnsCorrectRole(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addrolown")
	newUser := ts.CreateTestUser(t, "addrolnew")

	serverID := ts.CreateTestServer(t, owner.ID, "AddRoleServer")

	payload := map[string]interface{}{
		"user_id": newUser.ID,
	}
	w := ts.DoRequest("POST", membersPath(serverID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	member := body["member"].(map[string]interface{})
	assert.Equal(t, newUser.ID, member["user_id"])
	assert.Equal(t, "member", member["role"])
}

// ── UpdateMember (extended) ──────────────────────────────────────────────────

func TestUpdateMemberCannotChangeOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updownerown")
	admin := ts.CreateTestUser(t, "updowneradm")

	serverID := ts.CreateTestServer(t, owner.ID, "UpdOwnerServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	payload := map[string]interface{}{
		"role": "member",
	}
	w := ts.DoRequest("PATCH", memberPath(serverID, owner.ID), payload, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateMemberTargetNotFound(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updnotmemown")

	serverID := ts.CreateTestServer(t, owner.ID, "UpdNotMemServer")
	fakeUserID := fakeUUID99

	payload := map[string]interface{}{
		"role": "admin",
	}
	w := ts.DoRequest("PATCH", memberPath(serverID, fakeUserID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateMemberInvalidRole(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updinvrolown")
	member := ts.CreateTestUser(t, "updinvrolmem")

	serverID := ts.CreateTestServer(t, owner.ID, "InvRoleServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	payload := map[string]interface{}{
		"role": "superadmin",
	}
	w := ts.DoRequest("PATCH", memberPath(serverID, member.ID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMemberInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvsrv")

	payload := map[string]interface{}{
		"role": "admin",
	}
	w := ts.DoRequest("PATCH", memberPath(notAUUID, user.ID), payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMemberInvalidUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updinvuid")
	serverID := ts.CreateTestServer(t, owner.ID, "InvUIDServer")

	payload := map[string]interface{}{
		"role": "admin",
	}
	w := ts.DoRequest("PATCH", memberPath(serverID, notAUUID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMemberInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updpermown")
	member := ts.CreateTestUser(t, "updpermmem")
	target := ts.CreateTestUser(t, "updpermtgt")

	serverID := ts.CreateTestServer(t, owner.ID, "PermUpdServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	// Remove permissions from the @all role
	_, err := ts.DB.Exec(`UPDATE roles SET permissions = 0 WHERE server_id = $1 AND is_default = TRUE`, serverID)
	require.NoError(t, err)

	payload := map[string]interface{}{"role": "admin"}
	w := ts.DoRequest("PATCH", memberPath(serverID, target.ID), payload, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateMemberUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("PATCH", memberPath(fakeUUID1, fakeUUID2), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── RemoveMember (extended) ──────────────────────────────────────────────────

func TestRemoveMemberCannotKickOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickownown")
	admin := ts.CreateTestUser(t, "kickownadm")

	serverID := ts.CreateTestServer(t, owner.ID, "KickOwnerServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("DELETE", memberPath(serverID, owner.ID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoveMemberTargetNotAMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kicknotmem")

	serverID := ts.CreateTestServer(t, owner.ID, "KickNotMemServer")
	fakeUserID := fakeUUID99

	w := ts.DoRequest("DELETE", memberPath(serverID, fakeUserID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRemoveMemberRequesterNotAMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickreqout")
	outsider := ts.CreateTestUser(t, "kickoutsid")
	member := ts.CreateTestUser(t, "kickoutvic")

	serverID := ts.CreateTestServer(t, owner.ID, "ReqOutServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoveMemberInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "kickinvsrv")

	w := ts.DoRequest("DELETE", memberPath(notAUUID, user.ID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRemoveMemberInvalidUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickinvuid")
	serverID := ts.CreateTestServer(t, owner.ID, "InvUIDKickServer")

	w := ts.DoRequest("DELETE", memberPath(serverID, notAUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRemoveMemberCleansUpChannelKeys(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "keycleanown")
	member := ts.CreateTestUser(t, "keycleanmem")

	serverID := ts.CreateTestServer(t, owner.ID, "KeyCleanServer")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted-ch")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
		channelID, member.ID, []byte("test-wrapped-key"),
	)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var keyCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`,
		channelID, member.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 0, keyCount)
}

func TestRemoveMemberUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("DELETE", memberPath(fakeUUID1, fakeUUID2), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRemoveMemberSelfLeaveBroadcastsMessage(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bcastleaveown")
	member := ts.CreateTestUser(t, "bcastleavemem")

	serverID := ts.CreateTestServer(t, owner.ID, "BcastLeaveServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "left")
}

// ── BanMember ────────────────────────────────────────────────────────────────

func TestBanMemberSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banowner")
	member := ts.CreateTestUser(t, "bantarget")

	serverID := ts.CreateTestServer(t, owner.ID, "BanServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	payload := map[string]interface{}{
		"reason": "Spam",
	}
	w := ts.DoRequest("POST", banPath(serverID, member.ID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "banned")

	// Verify membership is removed
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, member.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// Verify ban exists
	var banExists bool
	err = ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_bans WHERE server_id = $1 AND user_id = $2)`,
		serverID, member.ID,
	).Scan(&banExists)
	require.NoError(t, err)
	assert.True(t, banExists)
}

func TestBanMemberCannotBanOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banownown")
	admin := ts.CreateTestUser(t, "banownadm")

	serverID := ts.CreateTestServer(t, owner.ID, "BanOwnerServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("POST", banPath(serverID, owner.ID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBanMemberCannotBanSelf(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banself")
	nonOwner := ts.CreateTestUser(t, "banselfnon")

	serverID := ts.CreateTestServer(t, owner.ID, "BanSelfServer")
	ts.AddMemberToServer(t, serverID, nonOwner.ID, "admin")

	// Owner banning self returns 403 "Cannot ban the server owner" (owner check fires first)
	w := ts.DoRequest("POST", banPath(serverID, owner.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Non-owner admin banning self returns 400 "Cannot ban yourself"
	w = ts.DoRequest("POST", banPath(serverID, nonOwner.ID), nil, testhelpers.AuthHeaders(nonOwner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBanMemberInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banpermown")
	member := ts.CreateTestUser(t, "banpermmem")
	target := ts.CreateTestUser(t, "banpermtgt")

	serverID := ts.CreateTestServer(t, owner.ID, "BanPermServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	_, err := ts.DB.Exec(`UPDATE roles SET permissions = 0 WHERE server_id = $1 AND is_default = TRUE`, serverID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", banPath(serverID, target.ID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBanMemberInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "baninvsrv")

	w := ts.DoRequest("POST", banPath(notAUUID, user.ID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBanMemberInvalidUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "baninvuid")
	serverID := ts.CreateTestServer(t, owner.ID, "InvUIDBanServer")

	w := ts.DoRequest("POST", banPath(serverID, notAUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBanMemberWithoutReason(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bannoreason")
	member := ts.CreateTestUser(t, "bannoreasonmem")

	serverID := ts.CreateTestServer(t, owner.ID, "BanNoReasonServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", banPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestBanMemberUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", banPath(fakeUUID1, fakeUUID2), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── UnbanMember ──────────────────────────────────────────────────────────────

func TestUnbanMemberSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "unbanowner")
	banned := ts.CreateTestUser(t, "unbantarget")

	serverID := ts.CreateTestServer(t, owner.ID, "UnbanServer")

	_, err := ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by) VALUES ($1, $2, $3)`,
		serverID, banned.ID, owner.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", banPath(serverID, banned.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "unbanned")
}

func TestUnbanMemberNotBanned(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "unbannone")
	notBanned := ts.CreateTestUser(t, "unbannotban")

	serverID := ts.CreateTestServer(t, owner.ID, "UnbanNoneServer")

	w := ts.DoRequest("DELETE", banPath(serverID, notBanned.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUnbanMemberInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "unbanpermown")
	member := ts.CreateTestUser(t, "unbanpermmem")
	banned := ts.CreateTestUser(t, "unbanpermban")

	serverID := ts.CreateTestServer(t, owner.ID, "UnbanPermServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	_, err := ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by) VALUES ($1, $2, $3)`,
		serverID, banned.ID, owner.ID,
	)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`UPDATE roles SET permissions = 0 WHERE server_id = $1 AND is_default = TRUE`, serverID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", banPath(serverID, banned.ID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUnbanMemberInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unbaninvsrv")

	w := ts.DoRequest("DELETE", banPath(notAUUID, user.ID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUnbanMemberInvalidUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "unbaninvuid")
	serverID := ts.CreateTestServer(t, owner.ID, "InvUIDUnbanServer")

	w := ts.DoRequest("DELETE", banPath(serverID, notAUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUnbanMemberUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("DELETE", banPath(fakeUUID1, fakeUUID2), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── ListBans ─────────────────────────────────────────────────────────────────

func TestListBansSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listbanown")
	banned1 := ts.CreateTestUser(t, "listban1")
	banned2 := ts.CreateTestUser(t, "listban2")

	serverID := ts.CreateTestServer(t, owner.ID, "ListBanServer")

	_, err := ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by, reason) VALUES ($1, $2, $3, 'Reason 1')`,
		serverID, banned1.ID, owner.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by, reason) VALUES ($1, $2, $3, 'Reason 2')`,
		serverID, banned2.ID, owner.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", bansPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var bans []interface{}
	testhelpers.ParseJSON(t, w, &bans)
	assert.Len(t, bans, 2)

	ban := bans[0].(map[string]interface{})
	assert.NotEmpty(t, ban["id"])
	assert.NotEmpty(t, ban["user_id"])
	assert.NotEmpty(t, ban["username"])
	assert.NotEmpty(t, ban["created_at"])
}

func TestListBansEmpty(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listbanempty")

	serverID := ts.CreateTestServer(t, owner.ID, "EmptyBanServer")

	w := ts.DoRequest("GET", bansPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var bans []interface{}
	testhelpers.ParseJSON(t, w, &bans)
	assert.Empty(t, bans)
}

func TestListBansInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listbanpermown")
	member := ts.CreateTestUser(t, "listbanpermmem")

	serverID := ts.CreateTestServer(t, owner.ID, "BanPermListServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	_, err := ts.DB.Exec(`UPDATE roles SET permissions = 0 WHERE server_id = $1 AND is_default = TRUE`, serverID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", bansPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListBansInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listbaninv")

	w := ts.DoRequest("GET", bansPath(notAUUID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestListBansUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", bansPath(fakeUUID1), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── Hierarchy enforcement ────────────────────────────────────────────────────

func TestRemoveMemberHierarchyCheck(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "hierown")
	admin := ts.CreateTestUser(t, "hieradmin")
	otherAdmin := ts.CreateTestUser(t, "hierother")

	serverID := ts.CreateTestServer(t, owner.ID, "HierServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	// Add second admin as member, then manually assign the existing admin role
	ts.AddMemberToServer(t, serverID, otherAdmin.ID, "member")
	var adminRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND name = 'admin'`, serverID).Scan(&adminRoleID)
	require.NoError(t, err)
	ts.AssignRoleToUser(t, serverID, otherAdmin.ID, adminRoleID)

	// Admin trying to kick another admin with equal rank should fail hierarchy check
	w := ts.DoRequest("DELETE", memberPath(serverID, otherAdmin.ID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBanMemberHierarchyCheck(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banhierown")
	admin := ts.CreateTestUser(t, "banhieradm")
	otherAdmin := ts.CreateTestUser(t, "banhieroth")

	serverID := ts.CreateTestServer(t, owner.ID, "BanHierServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	// Add second admin as member, then manually assign the existing admin role
	ts.AddMemberToServer(t, serverID, otherAdmin.ID, "member")
	var adminRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND name = 'admin'`, serverID).Scan(&adminRoleID)
	require.NoError(t, err)
	ts.AssignRoleToUser(t, serverID, otherAdmin.ID, adminRoleID)

	// Admin trying to ban another admin with equal rank should fail hierarchy check
	w := ts.DoRequest("POST", banPath(serverID, otherAdmin.ID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ── populateLastSeen coverage ─────────────────────────────────────────────────

func TestListMembersWithLastSeen(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "lastseeown")
	member := ts.CreateTestUser(t, "lastseemem")

	serverID := ts.CreateTestServer(t, owner.ID, "LastSeenServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Seed last_seen in Redis for the member
	ctx := context.Background()
	ts.Redis.Set(ctx, fmt.Sprintf("last_seen:%s", member.ID), "1711000000", 0)

	w := ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	assert.GreaterOrEqual(t, len(members), 2)
}

// ── RemoveMember cleans up read states ───────────────────────────────────────

func TestRemoveMemberCleansUpReadStates(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "readcleanown")
	member := ts.CreateTestUser(t, "readcleanmem")

	serverID := ts.CreateTestServer(t, owner.ID, "ReadCleanServer")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Seed a read state
	_, err := ts.DB.Exec(
		`INSERT INTO channel_read_states (channel_id, user_id, last_read_at) VALUES ($1, $2, NOW())`,
		channelID, member.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var rsCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_read_states WHERE channel_id = $1 AND user_id = $2`,
		channelID, member.ID,
	).Scan(&rsCount)
	require.NoError(t, err)
	assert.Equal(t, 0, rsCount)
}

// ── Ban cleans up member roles ───────────────────────────────────────────────

func TestBanMemberCleansUpMemberRoles(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bancleanown")
	member := ts.CreateTestUser(t, "bancleanmem")

	serverID := ts.CreateTestServer(t, owner.ID, "BanCleanServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Verify member roles exist
	var roleCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2`,
		serverID, member.ID,
	).Scan(&roleCount)
	require.NoError(t, err)
	assert.Greater(t, roleCount, 0)

	// Ban the member
	w := ts.DoRequest("POST", banPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify member roles are cleaned up
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2`,
		serverID, member.ID,
	).Scan(&roleCount)
	require.NoError(t, err)
	assert.Equal(t, 0, roleCount)
}

// ── ListBans returns banner info ─────────────────────────────────────────────

func TestListBansReturnsBannerInfo(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listbaninfo")
	banned := ts.CreateTestUser(t, "listbaninfo2")

	serverID := ts.CreateTestServer(t, owner.ID, "BanInfoServer")

	_, err := ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by, reason) VALUES ($1, $2, $3, 'Test reason')`,
		serverID, banned.ID, owner.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", bansPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var bans []interface{}
	testhelpers.ParseJSON(t, w, &bans)
	require.Len(t, bans, 1)

	ban := bans[0].(map[string]interface{})
	assert.Equal(t, banned.ID, ban["user_id"])
	assert.NotNil(t, ban["banned_by"])
	assert.NotNil(t, ban["banned_by_name"])
	assert.Equal(t, "Test reason", ban["reason"])
}

// ── Admin kicks member (permission path) ─────────────────────────────────────

func TestRemoveMemberAdminKicksMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "admkickown")
	admin := ts.CreateTestUser(t, "admkickadm")
	member := ts.CreateTestUser(t, "admkicktgt")

	serverID := ts.CreateTestServer(t, owner.ID, "AdminKickServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "removed")
}

// ── Ban triggers key revocation for E2EE channels ────────────────────────────

func TestBanMemberTriggersKeyRevocation(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bankeyown")
	member := ts.CreateTestUser(t, "bankeymem")

	serverID := ts.CreateTestServer(t, owner.ID, "BanKeyServer")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Seed a channel key for the member
	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
		channelID, member.ID, []byte("test-key"),
	)
	require.NoError(t, err)

	w := ts.DoRequest("POST", banPath(serverID, member.ID), map[string]interface{}{
		"reason": "Key revocation test",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify channel keys are cleaned up
	var keyCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`,
		channelID, member.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 0, keyCount)
}

// ── Kick triggers key revocation for E2EE channels ───────────────────────────

func TestRemoveMemberTriggersKeyRevocation(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickkeyown")
	member := ts.CreateTestUser(t, "kickkeymem")

	serverID := ts.CreateTestServer(t, owner.ID, "KickKeyServer")
	channelID := ts.CreateTestChannel(t, serverID, "e2ee-ch")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Seed a channel key
	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
		channelID, member.ID, []byte("test-key-2"),
	)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify key_revocations was created
	var revCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`,
		channelID,
	).Scan(&revCount)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, revCount, 1)
}

// ── AddMember missing body ───────────────────────────────────────────────────

func TestAddMemberMissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addmissbody")
	serverID := ts.CreateTestServer(t, owner.ID, "MissBodyServer")

	w := ts.DoRequest("POST", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── UpdateMember missing body ────────────────────────────────────────────────

func TestUpdateMemberMissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updmissbody")
	member := ts.CreateTestUser(t, "updmissmem")
	serverID := ts.CreateTestServer(t, owner.ID, "UpdMissBodyServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("PATCH", memberPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- TimeoutMember -------------------------------------------------------------

func TestTimeoutMemberSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutown")
	member := ts.CreateTestUser(t, "timeoutmem")

	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", timeoutPath(serverID, member.ID), map[string]interface{}{
		"duration_seconds": 300,
		"reason":           "cool down",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Member timed out", body["message"])
	assert.Equal(t, member.ID, body["user_id"])
	assert.NotEmpty(t, body["timed_out_until"])

	w = ts.DoRequest("GET", membersPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})
	found := false
	for _, raw := range members {
		m := raw.(map[string]interface{})
		if m["user_id"] == member.ID {
			found = true
			assert.NotEmpty(t, m["timed_out_until"])
		}
	}
	assert.True(t, found, "timed-out member should be in member list")
}

func TestRemoveTimeoutSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "untimeoutown")
	member := ts.CreateTestUser(t, "untimeoutmem")

	serverID := ts.CreateTestServer(t, owner.ID, "RemoveTimeoutServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	_, err := ts.DB.Exec("UPDATE server_members SET timed_out_until = NOW() + INTERVAL '1 hour' WHERE server_id = $1 AND user_id = $2", serverID, member.ID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", timeoutPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var cleared bool
	err = ts.DB.QueryRow("SELECT timed_out_until IS NULL FROM server_members WHERE server_id = $1 AND user_id = $2", serverID, member.ID).Scan(&cleared)
	require.NoError(t, err)
	assert.True(t, cleared)
}

func TestTimeoutMemberDurationBounds(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutboundsown")
	member := ts.CreateTestUser(t, "timeoutboundsmem")

	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutBoundsServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", timeoutPath(serverID, member.ID), map[string]interface{}{
		"duration_seconds": 59,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTimeoutMemberInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutpermown")
	actor := ts.CreateTestUser(t, "timeoutpermactor")
	target := ts.CreateTestUser(t, "timeoutpermtarget")

	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutPermServer")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	w := ts.DoRequest("POST", timeoutPath(serverID, target.ID), map[string]interface{}{
		"duration_seconds": 300,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestTimeoutMemberInvalidIDs(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutinvown")
	member := ts.CreateTestUser(t, "timeoutinvmem")
	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutInvalidIDsServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	payload := map[string]interface{}{"duration_seconds": 300}
	w := ts.DoRequest("POST", timeoutPath(notAUUID, member.ID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = ts.DoRequest("POST", timeoutPath(serverID, notAUUID), payload, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTimeoutMemberMissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutbodyown")
	member := ts.CreateTestUser(t, "timeoutbodymem")
	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutMissingBodyServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", timeoutPath(serverID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTimeoutMemberCannotTargetSelf(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutselfown")
	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutSelfServer")

	w := ts.DoRequest("POST", timeoutPath(serverID, owner.ID), map[string]interface{}{
		"duration_seconds": 300,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTimeoutMemberCannotTargetOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutownerown")
	admin := ts.CreateTestUser(t, "timeoutowneradm")
	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutOwnerServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("POST", timeoutPath(serverID, owner.ID), map[string]interface{}{
		"duration_seconds": 300,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestTimeoutMemberTargetNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeoutmissingown")
	target := ts.CreateTestUser(t, "timeoutmissingtarget")
	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutTargetMissingServer")

	w := ts.DoRequest("POST", timeoutPath(serverID, target.ID), map[string]interface{}{
		"duration_seconds": 300,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTimeoutMemberHierarchyCheck(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "timeouthierown")
	admin := ts.CreateTestUser(t, "timeouthieradm")
	otherAdmin := ts.CreateTestUser(t, "timeouthieroth")

	serverID := ts.CreateTestServer(t, owner.ID, "TimeoutHierarchyServer")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	ts.AddMemberToServer(t, serverID, otherAdmin.ID, "member")

	var adminRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND name = 'admin'`, serverID).Scan(&adminRoleID)
	require.NoError(t, err)
	ts.AssignRoleToUser(t, serverID, otherAdmin.ID, adminRoleID)

	w := ts.DoRequest("POST", timeoutPath(serverID, otherAdmin.ID), map[string]interface{}{
		"duration_seconds": 300,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoveTimeoutInvalidIDs(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "untimeoutinvown")
	member := ts.CreateTestUser(t, "untimeoutinvmem")
	serverID := ts.CreateTestServer(t, owner.ID, "RemoveTimeoutInvalidIDsServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", timeoutPath(notAUUID, member.ID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = ts.DoRequest("DELETE", timeoutPath(serverID, notAUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRemoveTimeoutInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "untimeoutpermown")
	actor := ts.CreateTestUser(t, "untimeoutpermactor")
	target := ts.CreateTestUser(t, "untimeoutpermtarget")

	serverID := ts.CreateTestServer(t, owner.ID, "RemoveTimeoutPermServer")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	w := ts.DoRequest("DELETE", timeoutPath(serverID, target.ID), nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ── Unverified email blocks member routes ────────────────────────────────────

func TestMembersUnverifiedEmailBlocked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "unverifiedmem")

	w := ts.DoRequest("GET", membersPath(fakeUUID1), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
