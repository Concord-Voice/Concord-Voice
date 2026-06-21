package rbac_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers and constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	pathRolesPrefix      = "/api/v1/servers/"
	pathChannelsPrefix   = "/api/v1/channels/"
	pathCategoriesPrefix = "/api/v1/categories/"
	invalidUUID          = "not-a-uuid" //nolint:gosec // test constant, not a credential
	malformedJSON        = "{bad"
)

func rolesPath(serverID string) string {
	return pathRolesPrefix + serverID + "/roles"
}

func rolePath(serverID, roleID string) string {
	return pathRolesPrefix + serverID + "/roles/" + roleID
}

func reorderRolesPath(serverID string) string {
	return pathRolesPrefix + serverID + "/roles/reorder"
}

func assignRolePath(serverID, userID string) string {
	return pathRolesPrefix + serverID + "/members/" + userID + "/roles"
}

func unassignRolePath(serverID, userID, roleID string) string {
	return pathRolesPrefix + serverID + "/members/" + userID + "/roles/" + roleID
}

func serverPermissionsPath(serverID string) string {
	return pathRolesPrefix + serverID + "/permissions"
}

func auditLogPath(serverID string) string {
	return pathRolesPrefix + serverID + "/audit-log"
}

func channelOverridesPath(channelID string) string {
	return pathChannelsPrefix + channelID + "/overrides"
}

func channelOverridePath(channelID, overrideID string) string {
	return pathChannelsPrefix + channelID + "/overrides/" + overrideID
}

func channelPermissionsPath(channelID string) string {
	return pathChannelsPrefix + channelID + "/permissions"
}

func channelPermSyncPath(channelID string) string {
	return pathChannelsPrefix + channelID + "/permission-sync"
}

func categoryOverridesPath(categoryID string) string {
	return pathCategoriesPrefix + categoryID + "/overrides"
}

func categoryOverridePath(categoryID, overrideID string) string {
	return pathCategoriesPrefix + categoryID + "/overrides/" + overrideID
}

// setupOwnerAndMember creates a test server with an owner and a base member.
func setupOwnerAndMember(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, testhelpers.TestUser, string) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "howner"+uuid.New().String()[:6])
	member := ts.CreateTestUser(t, "hmember"+uuid.New().String()[:6])
	serverID := ts.CreateTestServer(t, owner.ID, "Handler Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	return ts, owner, member, serverID
}

// grantPermToUser creates a role with given permissions and assigns it to the user.
func grantPermToUser(t *testing.T, ts *testhelpers.TestServer, serverID, userID string, position int, perms int64) string {
	t.Helper()
	roleID := ts.CreateTestRole(t, serverID, "grant_"+uuid.New().String()[:8], position, perms)
	ts.AssignRoleToUser(t, serverID, userID, roleID)
	invalidatePermCache(t, ts, serverID, userID)
	return roleID
}

// createRoleViaAPI is a helper that creates a role through the API and returns the role ID.
func createRoleViaAPI(t *testing.T, ts *testhelpers.TestServer, serverID string, token string, name string, perms int64) string {
	t.Helper()
	body := map[string]interface{}{
		"name":        name,
		"permissions": fmt.Sprintf("%d", perms),
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusCreated, w.Code, "createRoleViaAPI: expected 201, body: %s", w.Body.String())

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	return role["id"].(string)
}

// ─────────────────────────────────────────────────────────────────────────────
// ListRoles
// ─────────────────────────────────────────────────────────────────────────────

func TestListRoles_Success(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	roles := body["roles"].([]interface{})
	// At least the @all default role should exist
	assert.GreaterOrEqual(t, len(roles), 1, "should have at least the @all role")
}

func TestListRoles_NonMember_Forbidden(t *testing.T) {
	ts, _, _, serverID := setupOwnerAndMember(t)
	outsider := ts.CreateTestUser(t, "listoutsider"+uuid.New().String()[:6])

	w := ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListRoles_InvalidServerID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", rolesPath(invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestListRoles_Unauthenticated(t *testing.T) {
	ts, _, _, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", rolesPath(serverID), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// CreateRole
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateRole_Success_Owner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"name":        "Moderator",
		"permissions": "0",
		"color":       "#FF5733",
		"mentionable": true,
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	assert.Equal(t, "Moderator", role["name"])
	assert.Equal(t, "#FF5733", role["color"])
	assert.Equal(t, true, role["mentionable"])
	assert.NotEmpty(t, role["id"])
	assert.NotEmpty(t, role["created_at"])
}

func TestCreateRole_Success_MemberWithPermission(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	// Give member ManageRoles permission
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageRoles))

	body := map[string]interface{}{
		"name":        "CustomRole",
		"permissions": "0",
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateRole_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"name":        "Hacker",
		"permissions": "0",
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCreateRole_InvalidBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Missing required "name" field
	body := map[string]interface{}{
		"permissions": "0",
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateRole_InvalidColor(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"name":        "BadColor",
		"permissions": "0",
		"color":       "red",
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "color")
}

func TestCreateRole_InvalidServerID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"name":        "Test",
		"permissions": "0",
	}
	w := ts.DoRequest("POST", rolesPath(invalidUUID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateRole_PrivilegeEscalation_Blocked(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	// Give member ManageRoles only
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageRoles))

	// Try to create a role with PermBan (which member doesn't have)
	body := map[string]interface{}{
		"name":        "Escalated",
		"permissions": fmt.Sprintf("%d", int64(rbac.PermBan)),
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "permissions you do not have")
}

func TestCreateRole_DuplicateName_Conflict(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"name":        "UniqueRole",
		"permissions": "0",
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Create again with same name
	w = ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdateRole
// ─────────────────────────────────────────────────────────────────────────────

func TestUpdateRole_Success_Owner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Create a role via API
	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Editable", 0)

	body := map[string]interface{}{
		"name": "Renamed",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	assert.Equal(t, "Renamed", role["name"])
}

func TestUpdateRole_UpdatePermissions(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "PermUpdate", 0)

	newPerms := int64(rbac.PermKick | rbac.PermBan)
	body := map[string]interface{}{
		"permissions": fmt.Sprintf("%d", newPerms),
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	// permissions comes back as a string (json:"permissions,string")
	permStr := role["permissions"].(string)
	assert.Equal(t, fmt.Sprintf("%d", newPerms), permStr)
}

func TestUpdateRole_UpdateColor(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ColorTest", 0)

	body := map[string]interface{}{
		"color": "#AABBCC",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	assert.Equal(t, "#AABBCC", role["color"])
}

func TestUpdateRole_InvalidColor(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "BadColUpdate", 0)

	body := map[string]interface{}{
		"color": "blue",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateRole_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "NoTouch", 0)

	body := map[string]interface{}{
		"name": "Hacked",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateRole_ManagedRole_Forbidden(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// The @all role is managed — get its ID
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_managed = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	body := map[string]interface{}{
		"name": "Renamed @all",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, allRoleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "managed")
}

func TestUpdateRole_NotFound(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	fakeRoleID := uuid.New().String()
	body := map[string]interface{}{
		"name": "Ghost",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, fakeRoleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateRole_NoFieldsToUpdate(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "EmptyUpdate", 0)

	// Send an empty object
	body := map[string]interface{}{}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateRole_HierarchyViolation(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Give member ManageRoles at position 3
	grantPermToUser(t, ts, serverID, member.ID, 3, int64(rbac.PermManageRoles))

	// Create a high-position role (position will be max+1)
	highRoleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighRole", 0)
	// Manually set its position above the member's role
	_, err := ts.DB.Exec(`UPDATE roles SET position = 10 WHERE id = $1`, highRoleID)
	require.NoError(t, err)

	body := map[string]interface{}{
		"name": "Tampered",
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, highRoleID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "above your own position")
}

func TestUpdateRole_PrivilegeEscalation_Blocked(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Create role first so its position is low
	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "LowRole", 0)

	// Give member ManageRoles at position ABOVE the created role
	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	// Try to escalate permissions beyond what the member has
	body := map[string]interface{}{
		"permissions": fmt.Sprintf("%d", int64(rbac.PermAdministrator)),
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateRole_InvalidIDs(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{"name": "Test"}

	// Invalid server ID
	w := ts.DoRequest("PATCH", rolePath(invalidUUID, uuid.New().String()), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Invalid role ID (valid server)
	ts2, owner2, _, serverID2 := setupOwnerAndMember(t)
	_ = ts2
	w = ts.DoRequest("PATCH", rolePath(serverID2, invalidUUID), body, testhelpers.AuthHeaders(owner2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteRole
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteRole_Success_Owner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Deletable", 0)

	w := ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Role deleted", resp["message"])

	// Verify it's actually gone
	w = ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &resp)
	roles := resp["roles"].([]interface{})
	for _, r := range roles {
		role := r.(map[string]interface{})
		assert.NotEqual(t, roleID, role["id"], "deleted role should not appear in list")
	}
}

func TestDeleteRole_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Protected", 0)

	w := ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteRole_ManagedRole_Forbidden(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_managed = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", rolePath(serverID, allRoleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "managed")
}

func TestDeleteRole_DefaultRole_Forbidden(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// @all is both managed and default — test the default check separately
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", rolePath(serverID, allRoleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteRole_NotFound(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("DELETE", rolePath(serverID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteRole_HierarchyViolation(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Give member ManageRoles at position 3
	grantPermToUser(t, ts, serverID, member.ID, 3, int64(rbac.PermManageRoles))

	// Create a role at position above the member's highest
	highRoleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighDel", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 10 WHERE id = $1`, highRoleID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", rolePath(serverID, highRoleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "above your own position")
}

func TestDeleteRole_InvalidIDs(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("DELETE", rolePath(invalidUUID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	_, owner2, _, serverID2 := setupOwnerAndMember(t)
	w = ts.DoRequest("DELETE", rolePath(serverID2, invalidUUID), nil, testhelpers.AuthHeaders(owner2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// ReorderRoles
// ─────────────────────────────────────────────────────────────────────────────

func TestReorderRoles_Success_Owner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "RoleA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "RoleB", 0)

	body := map[string]interface{}{
		"role_ids": []string{roleB, roleA},
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Roles reordered", resp["message"])
}

func TestReorderRoles_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ReorderA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ReorderB", 0)

	body := map[string]interface{}{
		"role_ids": []string{roleB, roleA},
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestReorderRoles_InvalidServerID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"role_ids": []string{uuid.New().String()},
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(invalidUUID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderRoles_InvalidBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Missing role_ids
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderRoles_HierarchyViolation_CannotReorderAbove(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Give member ManageRoles at position 3
	grantPermToUser(t, ts, serverID, member.ID, 3, int64(rbac.PermManageRoles))

	// Create a role above member's position
	highRole := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighReorder", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 10 WHERE id = $1`, highRole)
	require.NoError(t, err)

	body := map[string]interface{}{
		"role_ids": []string{highRole},
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// AssignRole
// ─────────────────────────────────────────────────────────────────────────────

func TestAssignRole_Success_Owner(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Assignable", 0)

	body := map[string]interface{}{
		"role_id": roleID,
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Role assigned", resp["message"])
}

func TestAssignRole_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "assigntarget"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "NoAssign", 0)

	body := map[string]interface{}{
		"role_id": roleID,
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, target.ID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAssignRole_HierarchyViolation(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "assignhtarget"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	// Give member ManageRolesAssign at position 3
	grantPermToUser(t, ts, serverID, member.ID, 3, int64(rbac.PermManageRolesAssign))

	// Create a role at position 10 (above member's highest)
	highRoleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighAssign", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 10 WHERE id = $1`, highRoleID)
	require.NoError(t, err)

	body := map[string]interface{}{
		"role_id": highRoleID,
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, target.ID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "equal or higher position")
}

func TestAssignRole_TargetNotMember(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "NoTarget", 0)
	nonMember := ts.CreateTestUser(t, "nonmember"+uuid.New().String()[:6])

	body := map[string]interface{}{
		"role_id": roleID,
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, nonMember.ID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "not a member")
}

func TestAssignRole_RoleNotFound(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"role_id": uuid.New().String(),
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAssignRole_InvalidIDs(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"role_id": uuid.New().String(),
	}
	// Invalid server ID
	w := ts.DoRequest("POST", assignRolePath(invalidUUID, uuid.New().String()), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAssignRole_InvalidBody(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Missing role_id
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAssignRole_Idempotent(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Idempotent", 0)

	body := map[string]interface{}{
		"role_id": roleID,
	}
	// Assign once
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Assign again — ON CONFLICT DO NOTHING, should still return 200
	w = ts.DoRequest("POST", assignRolePath(serverID, member.ID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// UnassignRole
// ─────────────────────────────────────────────────────────────────────────────

func TestUnassignRole_Success_Owner(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Removable", 0)

	// Assign first
	assignBody := map[string]interface{}{"role_id": roleID}
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Unassign
	w = ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, roleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Role unassigned", resp["message"])
}

func TestUnassignRole_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "unassigntarget"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "NoUnassign", 0)
	assignBody := map[string]interface{}{"role_id": roleID}
	ts.DoRequest("POST", assignRolePath(serverID, target.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))

	w := ts.DoRequest("DELETE", unassignRolePath(serverID, target.ID, roleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUnassignRole_DefaultRole_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, allRoleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "default")
}

func TestUnassignRole_NotFound(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Try to unassign a role that was never assigned
	fakeRoleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "NeverAssigned", 0)
	w := ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, fakeRoleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUnassignRole_RoleNotInServer(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUnassignRole_HierarchyViolation(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "unassignhtarget"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	// Give member ManageRolesAssign at position 3
	grantPermToUser(t, ts, serverID, member.ID, 3, int64(rbac.PermManageRolesAssign))

	// Create a role at position 10 and assign it to target
	highRoleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighUnassign", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 10 WHERE id = $1`, highRoleID)
	require.NoError(t, err)
	assignBody := map[string]interface{}{"role_id": highRoleID}
	ts.DoRequest("POST", assignRolePath(serverID, target.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))

	// Member tries to unassign a role above their position
	w := ts.DoRequest("DELETE", unassignRolePath(serverID, target.ID, highRoleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUnassignRole_InvalidIDs(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	// Invalid server ID
	w := ts.DoRequest("DELETE", unassignRolePath(invalidUUID, uuid.New().String(), uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Invalid user ID
	_, owner2, _, sid2 := setupOwnerAndMember(t)
	w = ts.DoRequest("DELETE", unassignRolePath(sid2, invalidUUID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// GetMyServerPermissions
// ─────────────────────────────────────────────────────────────────────────────

func TestGetMyServerPermissions_Success(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	perms, ok := resp["permissions"]
	assert.True(t, ok, "response should contain permissions field")
	// Owner should have OwnerPermissions (a non-zero value)
	permVal := int64(perms.(float64))
	assert.NotZero(t, permVal, "owner should have non-zero permissions")
}

func TestGetMyServerPermissions_BaseMember(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	perms := int64(resp["permissions"].(float64))
	assert.Equal(t, int64(rbac.BasePermissions), perms, "base member should have BasePermissions")
}

func TestGetMyServerPermissions_NonMember_Forbidden(t *testing.T) {
	ts, _, _, serverID := setupOwnerAndMember(t)
	outsider := ts.CreateTestUser(t, "permoutsider"+uuid.New().String()[:6])

	w := ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMyServerPermissions_InvalidServerID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", serverPermissionsPath(invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// GetAuditLog
// ─────────────────────────────────────────────────────────────────────────────

func TestGetAuditLog_Success_Owner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Trigger an audit log entry by creating a role
	createRoleViaAPI(t, ts, serverID, owner.AccessToken, "AuditTest", 0)

	w := ts.DoRequest("GET", auditLogPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	entries := resp["entries"].([]interface{})
	assert.GreaterOrEqual(t, len(entries), 1, "should have at least one audit entry")

	// Verify entry structure
	entry := entries[0].(map[string]interface{})
	assert.NotEmpty(t, entry["id"])
	assert.NotEmpty(t, entry["action"])
	assert.NotEmpty(t, entry["created_at"])
}

func TestGetAuditLog_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", auditLogPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetAuditLog_Pagination(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Create several roles to generate audit entries
	for i := 0; i < 3; i++ {
		createRoleViaAPI(t, ts, serverID, owner.AccessToken, fmt.Sprintf("AuditPage%d", i), 0)
	}

	// Fetch with limit=2
	w := ts.DoRequest("GET", auditLogPath(serverID)+"?limit=2&offset=0", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	entries := resp["entries"].([]interface{})
	assert.LessOrEqual(t, len(entries), 2, "pagination limit should be respected")
	assert.Equal(t, float64(2), resp["limit"])
	assert.Equal(t, float64(0), resp["offset"])
}

func TestGetAuditLog_InvalidServerID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", auditLogPath(invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Channel Permission Overrides (SBAC)
// ─────────────────────────────────────────────────────────────────────────────

func TestListChannelOverrides_Success(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "override-test")

	w := ts.DoRequest("GET", channelOverridesPath(channelID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	overrides := resp["overrides"].([]interface{})
	assert.Equal(t, 0, len(overrides), "new channel should have no overrides")
}

func TestListChannelOverrides_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "override-forbidden")

	w := ts.DoRequest("GET", channelOverridesPath(channelID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListChannelOverrides_ChannelNotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", channelOverridesPath(uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestListChannelOverrides_InvalidChannelID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", channelOverridesPath(invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpsertChannelOverride_Success(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "upsert-test")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	override := resp["override"].(map[string]interface{})
	assert.Equal(t, "user", override["target_type"])
	assert.Equal(t, member.ID, override["target_id"])
}

func TestUpsertChannelOverride_RoleTarget(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "upsert-role-test")

	// Get @all role ID
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	body := map[string]interface{}{
		"target_type": "role",
		"target_id":   allRoleID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpsertChannelOverride_UpsertUpdatesExisting(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "upsert-update")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	// First upsert (insert)
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Second upsert (update)
	body["deny"] = int64(rbac.PermAttachFiles)
	w = ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify only one override exists
	w = ts.DoRequest("GET", channelOverridesPath(channelID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var listResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &listResp)
	overrides := listResp["overrides"].([]interface{})
	assert.Equal(t, 1, len(overrides), "upsert should not create duplicates")
}

func TestUpsertChannelOverride_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "upsert-forbidden")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpsertChannelOverride_PrivilegeEscalation_Blocked(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "upsert-escalation")

	// Give member ManageChannels (required for override) but NOT PermBan
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermBan), // member doesn't have PermBan
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "permissions you do not have")
}

func TestUpsertChannelOverride_DenyBitsNoEscalationCheck(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "deny-no-escalation")

	// Give member ManageChannels only
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	// Deny bits should be allowed even for permissions the actor doesn't have
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermBan | rbac.PermAdministrator), // deny does not escalate
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpsertChannelOverride_InvalidBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "invalid-body")

	tests := []struct {
		name string
		body map[string]interface{}
	}{
		{
			name: "missing target_type",
			body: map[string]interface{}{
				"target_id": uuid.New().String(),
				"allow":     0,
				"deny":      0,
			},
		},
		{
			name: "invalid target_type",
			body: map[string]interface{}{
				"target_type": "invalid",
				"target_id":   uuid.New().String(),
				"allow":       0,
				"deny":        0,
			},
		},
		{
			name: "missing target_id",
			body: map[string]interface{}{
				"target_type": "user",
				"allow":       0,
				"deny":        0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := ts.DoRequest("PUT", channelOverridesPath(channelID), tc.body, testhelpers.AuthHeaders(owner.AccessToken))
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestUpsertChannelOverride_ChannelNotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   uuid.New().String(),
		"allow":       0,
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(uuid.New().String()), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteChannelOverride_Success(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "delete-override")

	// Create an override first
	upsertBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), upsertBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	override := upsertResp["override"].(map[string]interface{})
	overrideID := override["id"].(string)

	// Delete it
	w = ts.DoRequest("DELETE", channelOverridePath(channelID, overrideID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Override deleted", resp["message"])
}

func TestDeleteChannelOverride_NotFound(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "del-override-nf")

	w := ts.DoRequest("DELETE", channelOverridePath(channelID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteChannelOverride_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "del-override-403")

	// Create override as owner
	upsertBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), upsertBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	// Member tries to delete
	w = ts.DoRequest("DELETE", channelOverridePath(channelID, overrideID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteChannelOverride_InvalidIDs(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	// Invalid channel ID
	w := ts.DoRequest("DELETE", channelOverridePath(invalidUUID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Invalid override ID
	_, owner2, _, sid := setupOwnerAndMember(t)
	chID := ts.CreateTestChannel(t, sid, "del-inv-override")
	w = ts.DoRequest("DELETE", channelOverridePath(chID, invalidUUID), nil, testhelpers.AuthHeaders(owner2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteChannelOverride_ChannelNotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("DELETE", channelOverridePath(uuid.New().String(), uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// GetMyChannelPermissions
// ─────────────────────────────────────────────────────────────────────────────

func TestGetMyChannelPermissions_Success(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "perms-test")

	w := ts.DoRequest("GET", channelPermissionsPath(channelID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	_, ok := resp["permissions"]
	assert.True(t, ok, "response should contain permissions field")
}

func TestGetMyChannelPermissions_WithOverride(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "perms-override")

	// Deny SendMessages for member in this channel
	upsertBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), upsertBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Invalidate cache
	invalidatePermCache(t, ts, serverID, member.ID)

	// Check member's channel permissions — should not have SendMessages
	w = ts.DoRequest("GET", channelPermissionsPath(channelID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	perms := rbac.Permission(int64(resp["permissions"].(float64)))
	assert.False(t, perms.Has(rbac.PermSendMessages), "member should be denied SendMessages in this channel")
}

func TestGetMyChannelPermissions_ChannelNotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", channelPermissionsPath(uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetMyChannelPermissions_InvalidChannelID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", channelPermissionsPath(invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Category Permission Overrides
// ─────────────────────────────────────────────────────────────────────────────

// createTestCategory creates a category (channel_groups row) in the database.
func createTestCategory(t *testing.T, ts *testhelpers.TestServer, serverID, name string) string {
	t.Helper()
	catID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, $3, 0)`,
		catID, serverID, name,
	)
	require.NoError(t, err)
	return catID
}

// assignChannelToCategory assigns a channel to a category and optionally enables sync.
func assignChannelToCategory(t *testing.T, ts *testhelpers.TestServer, channelID, categoryID string, syncPerms bool) {
	t.Helper()
	_, err := ts.DB.Exec(
		`UPDATE channels SET group_id = $1, sync_permissions = $2 WHERE id = $3`,
		categoryID, syncPerms, channelID,
	)
	require.NoError(t, err)
}

func TestListCategoryOverrides_Success(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "test-category")

	w := ts.DoRequest("GET", categoryOverridesPath(catID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	overrides := resp["overrides"].([]interface{})
	assert.Equal(t, 0, len(overrides))
}

func TestListCategoryOverrides_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-forbidden")

	w := ts.DoRequest("GET", categoryOverridesPath(catID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListCategoryOverrides_NotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", categoryOverridesPath(uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestListCategoryOverrides_InvalidID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	w := ts.DoRequest("GET", categoryOverridesPath(invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpsertCategoryOverride_Success(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-upsert")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	override := resp["override"].(map[string]interface{})
	assert.Equal(t, "user", override["target_type"])
	assert.Equal(t, member.ID, override["target_id"])
}

func TestUpsertCategoryOverride_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-upsert-403")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpsertCategoryOverride_PrivilegeEscalation_Blocked(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-escalation")

	// Give member ManageChannels but NOT PermBan
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermBan),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpsertCategoryOverride_NotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   uuid.New().String(),
		"allow":       0,
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(uuid.New().String()), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpsertCategoryOverride_SyncsToChlid(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-sync")
	channelID := ts.CreateTestChannel(t, serverID, "synced-channel")
	assignChannelToCategory(t, ts, channelID, catID, true)

	// Create a category override
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify the override was synced to the child channel
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, member.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "category override should sync to child channel")
}

func TestDeleteCategoryOverride_Success(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-del")

	// Create override
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	// Delete
	w = ts.DoRequest("DELETE", categoryOverridePath(catID, overrideID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Override deleted", resp["message"])
}

func TestDeleteCategoryOverride_CascadesToSyncedChannels(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-cascade-del")
	channelID := ts.CreateTestChannel(t, serverID, "cascaded-channel")
	assignChannelToCategory(t, ts, channelID, catID, true)

	// Create category override (which syncs to child)
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	// Verify child channel has the override
	var beforeCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID,
	).Scan(&beforeCount)
	require.NoError(t, err)
	require.Equal(t, 1, beforeCount)

	// Delete the category override
	w = ts.DoRequest("DELETE", categoryOverridePath(catID, overrideID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify cascade deleted from child channel
	var afterCount int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID,
	).Scan(&afterCount)
	require.NoError(t, err)
	assert.Equal(t, 0, afterCount, "synced channel overrides should be cascade-deleted")
}

func TestDeleteCategoryOverride_NotFound(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-del-nf")

	w := ts.DoRequest("DELETE", categoryOverridePath(catID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteCategoryOverride_BaseMember_Forbidden(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-del-403")

	// Create override as owner
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	// Member tries to delete
	w = ts.DoRequest("DELETE", categoryOverridePath(catID, overrideID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteCategoryOverride_InvalidIDs(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	// Invalid category ID
	w := ts.DoRequest("DELETE", categoryOverridePath(invalidUUID, uuid.New().String()), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Invalid override ID
	_, owner2, _, sid := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, sid, "cat-inv-id")
	w = ts.DoRequest("DELETE", categoryOverridePath(catID, invalidUUID), nil, testhelpers.AuthHeaders(owner2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// SetChannelPermissionSync
// ─────────────────────────────────────────────────────────────────────────────

func TestSetChannelPermSync_EnableSync(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-enable")
	channelID := ts.CreateTestChannel(t, serverID, "sync-test")
	assignChannelToCategory(t, ts, channelID, catID, false)

	// Create a category override first
	catBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), catBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Enable sync
	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w = ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp["sync_permissions"])

	// Verify category overrides were copied to channel
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "enabling sync should copy category overrides to channel")
}

func TestSetChannelPermSync_DisableSync(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-disable")
	channelID := ts.CreateTestChannel(t, serverID, "sync-off")
	assignChannelToCategory(t, ts, channelID, catID, true)

	body := map[string]interface{}{
		"sync_permissions": false,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, false, resp["sync_permissions"])
}

func TestSetChannelPermSync_NoCategoryReject(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "no-category")

	// Channel not in any category — enabling sync should fail
	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "not in a category")
}

func TestSetChannelPermSync_BaseMember_Forbidden(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-forbidden")
	channelID := ts.CreateTestChannel(t, serverID, "sync-403")
	assignChannelToCategory(t, ts, channelID, catID, false)

	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestSetChannelPermSync_ChannelNotFound(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(uuid.New().String()), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSetChannelPermSync_InvalidChannelID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(invalidUUID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// End-to-end RBAC flow test
// ─────────────────────────────────────────────────────────────────────────────

func TestRBACFlow_CreateAssignVerifyPermissions(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// 1. Member should start with BasePermissions only
	w := ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var permResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &permResp)
	startPerms := int64(permResp["permissions"].(float64))
	assert.Equal(t, int64(rbac.BasePermissions), startPerms, "member starts with base permissions")

	// 2. Owner creates a moderator role
	modPerms := int64(rbac.PermManageAllMessages | rbac.PermKick | rbac.PermMuteMembers)
	modRoleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "Moderator", modPerms)

	// 3. Owner assigns moderator role to member
	assignBody := map[string]interface{}{"role_id": modRoleID}
	w = ts.DoRequest("POST", assignRolePath(serverID, member.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// 4. Verify member now has elevated permissions
	w = ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &permResp)
	newPerms := rbac.Permission(int64(permResp["permissions"].(float64)))
	assert.True(t, newPerms.Has(rbac.PermManageAllMessages), "member should now have ManageAllMessages")
	assert.True(t, newPerms.Has(rbac.PermKick), "member should now have Kick")
	assert.True(t, newPerms.Has(rbac.PermMuteMembers), "member should now have MuteMembers")

	// 5. Owner unassigns the role
	w = ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, modRoleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// 6. Verify member is back to base permissions
	w = ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &permResp)
	finalPerms := int64(permResp["permissions"].(float64))
	assert.Equal(t, int64(rbac.BasePermissions), finalPerms, "member should be back to base permissions after unassign")
}

func TestRBACFlow_ChannelOverrideAffectsPermissions(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "flow-override")

	// 1. Member should have SendMessages in this channel (from BasePermissions)
	w := ts.DoRequest("GET", channelPermissionsPath(channelID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var permResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &permResp)
	perms := rbac.Permission(int64(permResp["permissions"].(float64)))
	assert.True(t, perms.Has(rbac.PermSendMessages), "member should have SendMessages initially")

	// 2. Owner creates a channel override denying SendMessages for member
	overrideBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w = ts.DoRequest("PUT", channelOverridesPath(channelID), overrideBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Invalidate cache to pick up the new override
	invalidatePermCache(t, ts, serverID, member.ID)

	// 3. Member should now be denied SendMessages in this channel
	w = ts.DoRequest("GET", channelPermissionsPath(channelID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &permResp)
	perms = rbac.Permission(int64(permResp["permissions"].(float64)))
	assert.False(t, perms.Has(rbac.PermSendMessages), "member should be denied SendMessages after override")

	// 4. But server-level permissions should still include SendMessages
	w = ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &permResp)
	serverPerms := rbac.Permission(int64(permResp["permissions"].(float64)))
	assert.True(t, serverPerms.Has(rbac.PermSendMessages), "server-level permissions should still include SendMessages")
}

// ─────────────────────────────────────────────────────────────────────────────
// Audit log entries verification
// ─────────────────────────────────────────────────────────────────────────────

func TestAuditLog_RecordsRoleLifecycle(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Create role
	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "AuditLifecycle", 0)

	// Assign role
	assignBody := map[string]interface{}{"role_id": roleID}
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Unassign role
	w = ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, roleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Delete role
	w = ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Fetch audit log
	w = ts.DoRequest("GET", auditLogPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	entries := resp["entries"].([]interface{})

	// Collect action types
	actions := make(map[string]bool)
	for _, e := range entries {
		entry := e.(map[string]interface{})
		actions[entry["action"].(string)] = true
	}

	assert.True(t, actions["role_created"], "audit log should contain role_created")
	assert.True(t, actions["role_assigned"], "audit log should contain role_assigned")
	assert.True(t, actions["role_unassigned"], "audit log should contain role_unassigned")
	assert.True(t, actions["role_deleted"], "audit log should contain role_deleted")
}

func TestAuditLog_RecordsChannelOverrideLifecycle(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "audit-override")

	// Create override
	overrideBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), overrideBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	// Update override (same target_type+target_id triggers upsert update path)
	overrideBody["deny"] = int64(rbac.PermAttachFiles)
	w = ts.DoRequest("PUT", channelOverridesPath(channelID), overrideBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Delete override
	w = ts.DoRequest("DELETE", channelOverridePath(channelID, overrideID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Fetch audit log
	w = ts.DoRequest("GET", auditLogPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	entries := resp["entries"].([]interface{})

	actions := make(map[string]bool)
	for _, e := range entries {
		entry := e.(map[string]interface{})
		actions[entry["action"].(string)] = true
	}

	assert.True(t, actions["channel_override_created"], "should log channel_override_created")
	assert.True(t, actions["channel_override_updated"], "should log channel_override_updated")
	assert.True(t, actions["channel_override_deleted"], "should log channel_override_deleted")
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge case: permissions serialization
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateRole_LargePermissionBitfield(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Owner has all OwnerPermissions, so they can create a role with all those bits
	body := map[string]interface{}{
		"name":        "SuperRole",
		"permissions": fmt.Sprintf("%d", int64(rbac.OwnerPermissions)),
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})

	// Verify the permissions round-trip correctly
	permStr := role["permissions"].(string)
	assert.Equal(t, fmt.Sprintf("%d", int64(rbac.OwnerPermissions)), permStr)
}

func TestGetMyServerPermissions_ReturnsCorrectBitmask(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)

	// Give member a specific custom permission set
	customPerms := int64(rbac.BasePermissions | rbac.PermKick | rbac.PermMuteMembers)
	grantPermToUser(t, ts, serverID, member.ID, 5, customPerms)

	w := ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	perms := rbac.Permission(int64(resp["permissions"].(float64)))

	assert.True(t, perms.Has(rbac.PermKick), "should have Kick")
	assert.True(t, perms.Has(rbac.PermMuteMembers), "should have MuteMembers")
	assert.True(t, perms.Has(rbac.PermSendMessages), "should still have base SendMessages")
	assert.False(t, perms.Has(rbac.PermBan), "should not have Ban")
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdateRole response body verification (issue #249)
// ─────────────────────────────────────────────────────────────────────────────

func TestUpdateRole_ReturnsFullRoleBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "BodyCheck", 0)

	// Verify the JSON response structure
	body := map[string]interface{}{
		"name":               "BodyChecked",
		"mentionable":        true,
		"display_separately": true,
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Parse raw JSON to verify field presence
	var raw json.RawMessage
	err := json.Unmarshal(w.Body.Bytes(), &raw)
	require.NoError(t, err)

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Contains(t, resp, "role", "response should contain 'role' key")

	var role map[string]interface{}
	require.NoError(t, json.Unmarshal(resp["role"], &role))

	// Verify all expected fields are present
	expectedFields := []string{
		"id", "server_id", "name", "position", "permissions",
		"is_default", "is_managed", "mentionable", "display_separately",
		"created_at", "updated_at",
	}
	for _, field := range expectedFields {
		assert.Contains(t, role, field, "role response should contain field: %s", field)
	}

	assert.Equal(t, "BodyChecked", role["name"])
	assert.Equal(t, true, role["mentionable"])
	assert.Equal(t, true, role["display_separately"])
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional coverage tests
// ─────────────────────────────────────────────────────────────────────────────

func TestReorderRolesMemberWithPermissionSuccess(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ReorderMemberA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ReorderMemberB", 0)

	// Grant member a role at position well above the created roles
	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	body := map[string]interface{}{
		"role_ids": []string{roleB, roleA},
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReorderRolesPositionExceedsActor(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	grantPermToUser(t, ts, serverID, member.ID, 2, int64(rbac.PermManageRoles))

	// Create 3 roles — reordering 3 roles means positions 0,1,2 assigned
	// member's max position is 2 so max new position (2) >= actorMaxPosition (2)
	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "TooManyA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "TooManyB", 0)
	roleC := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "TooManyC", 0)
	// Keep them below member's position
	_, err := ts.DB.Exec(`UPDATE roles SET position = 0 WHERE id IN ($1, $2, $3)`, roleA, roleB, roleC)
	require.NoError(t, err)

	body := map[string]interface{}{
		"role_ids": []string{roleA, roleB, roleC},
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Contains(t, resp["error"], "above your position")
}

func TestAssignRoleInvalidUserID(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"role_id": uuid.New().String(),
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, invalidUUID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAssignRoleMemberWithPermissionSuccess(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "assignperm"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "LowAssignable", 0)

	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRolesAssign))

	body := map[string]interface{}{
		"role_id": roleID,
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, target.ID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUnassignRoleInvalidRoleID(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUnassignRoleMemberWithPermissionSuccess(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "unassignperm"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "UnassignLow", 0)

	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRolesAssign))
	assignBody := map[string]interface{}{"role_id": roleID}
	w := ts.DoRequest("POST", assignRolePath(serverID, target.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("DELETE", unassignRolePath(serverID, target.ID, roleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteRoleMemberWithPermissionSuccess(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "MemberDeletes", 0)

	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	w := ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateRoleMemberWithPermissionUpdatesLowerRole(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "MemberEditable", 0)

	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	body := map[string]interface{}{
		"name":        "MemberEdited",
		"mentionable": true,
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	assert.Equal(t, "MemberEdited", role["name"])
	assert.Equal(t, true, role["mentionable"])
}

func TestUpdateRoleInvalidBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "InvalidBody", 0)

	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), malformedJSON, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateRoleEmojiBooleanFields(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "EmojiTest", 0)

	emoji := "🎮"
	body := map[string]interface{}{
		"emoji":              emoji,
		"display_separately": true,
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	assert.Equal(t, emoji, role["emoji"])
	assert.Equal(t, true, role["display_separately"])
}

func TestGetMyChannelPermissionsNonMember(t *testing.T) {
	ts, _, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "perms-outsider")
	outsider := ts.CreateTestUser(t, "chpermout"+uuid.New().String()[:6])

	w := ts.DoRequest("GET", channelPermissionsPath(channelID), nil, testhelpers.AuthHeaders(outsider.AccessToken))
	// Non-member hits the resolver error path (no membership → resolver returns error → 500)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestUpsertChannelOverrideInvalidChannelID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   uuid.New().String(),
		"allow":       0,
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(invalidUUID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestListChannelOverridesWithExistingOverrides(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "list-with-overrides")

	upsertBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), upsertBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("GET", channelOverridesPath(channelID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	overrides := resp["overrides"].([]interface{})
	assert.Equal(t, 1, len(overrides))

	override := overrides[0].(map[string]interface{})
	assert.Equal(t, "user", override["target_type"])
	assert.Equal(t, member.ID, override["target_id"])
	assert.NotEmpty(t, override["created_at"])
	assert.NotEmpty(t, override["updated_at"])
}

func TestListCategoryOverridesWithExistingOverrides(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "list-cat-overrides")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("GET", categoryOverridesPath(catID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	overrides := resp["overrides"].([]interface{})
	assert.Equal(t, 1, len(overrides))

	override := overrides[0].(map[string]interface{})
	assert.Equal(t, "user", override["target_type"])
	assert.NotEmpty(t, override["created_at"])
}

func TestUpsertCategoryOverrideInvalidBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-bad-body")

	w := ts.DoRequest("PUT", categoryOverridesPath(catID), malformedJSON, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpsertCategoryOverrideInvalidCategoryID(t *testing.T) {
	ts, owner, _, _ := setupOwnerAndMember(t)

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   uuid.New().String(),
		"allow":       0,
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(invalidUUID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpsertCategoryOverrideUpsertUpdatesExisting(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-upsert-update")

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	body["deny"] = int64(rbac.PermAttachFiles)
	w = ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("GET", categoryOverridesPath(catID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var listResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &listResp)
	overrides := listResp["overrides"].([]interface{})
	assert.Equal(t, 1, len(overrides), "upsert should not create duplicates")
}

func TestSetChannelPermSyncInvalidBody(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "sync-bad-body")

	w := ts.DoRequest("PUT", channelPermSyncPath(channelID), malformedJSON, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSetChannelPermSyncMemberWithPermissionSuccess(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-member-perm")
	channelID := ts.CreateTestChannel(t, serverID, "sync-member-ch")
	assignChannelToCategory(t, ts, channelID, catID, false)

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteCategoryOverrideCascadeNoSyncChannels(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-no-sync-cascade")
	channelID := ts.CreateTestChannel(t, serverID, "unsync-channel")
	assignChannelToCategory(t, ts, channelID, catID, false) // sync=false

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	w = ts.DoRequest("DELETE", categoryOverridePath(catID, overrideID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListChannelOverridesMemberWithManageChannels(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "list-perm")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	w := ts.DoRequest("GET", channelOverridesPath(channelID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteChannelOverrideMemberWithManageChannels(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "del-member-perm")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	upsertBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), upsertBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	w = ts.DoRequest("DELETE", channelOverridePath(channelID, overrideID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpsertChannelOverrideMemberWithManageChannels(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "upsert-member-perm")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels|rbac.PermSendMessages))

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListCategoryOverridesMemberWithManageChannels(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-list-perm")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	w := ts.DoRequest("GET", categoryOverridesPath(catID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpsertCategoryOverrideMemberWithManageChannels(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-upsert-perm")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels|rbac.PermSendMessages))

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteCategoryOverrideMemberWithManageChannels(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-del-perm")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermSendMessages),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var upsertResp map[string]interface{}
	testhelpers.ParseJSON(t, w, &upsertResp)
	overrideID := upsertResp["override"].(map[string]interface{})["id"].(string)

	w = ts.DoRequest("DELETE", categoryOverridePath(catID, overrideID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateRoleWithEmoji(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	emoji := "🎯"
	body := map[string]interface{}{
		"name":               "EmojiRole",
		"permissions":        "0",
		"emoji":              emoji,
		"display_separately": true,
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	assert.Equal(t, emoji, role["emoji"])
	assert.Equal(t, true, role["display_separately"])
}

func TestGetAuditLogCustomPagination(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	for i := 0; i < 5; i++ {
		createRoleViaAPI(t, ts, serverID, owner.AccessToken, fmt.Sprintf("AuditCustom%d", i), 0)
	}

	w := ts.DoRequest("GET", auditLogPath(serverID)+"?limit=3&offset=1", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, float64(3), resp["limit"])
	assert.Equal(t, float64(1), resp["offset"])
	entries := resp["entries"].([]interface{})
	assert.LessOrEqual(t, len(entries), 3)
}

func TestGetAuditLogInvalidPaginationUsesDefaults(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	createRoleViaAPI(t, ts, serverID, owner.AccessToken, "AuditDefault", 0)

	w := ts.DoRequest("GET", auditLogPath(serverID)+"?limit=invalid&offset=bad", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, float64(50), resp["limit"])
	assert.Equal(t, float64(0), resp["offset"])
}

func TestSetChannelPermSyncDisableSyncWithExistingOverrides(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-disable-existing")
	channelID := ts.CreateTestChannel(t, serverID, "sync-disable-ch")
	assignChannelToCategory(t, ts, channelID, catID, false)

	catBody := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSendMessages),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), catBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	syncBody := map[string]interface{}{"sync_permissions": true}
	w = ts.DoRequest("PUT", channelPermSyncPath(channelID), syncBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Now disable sync
	syncBody["sync_permissions"] = false
	w = ts.DoRequest("PUT", channelPermSyncPath(channelID), syncBody, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, false, resp["sync_permissions"])
}

func TestUpdateRoleMemberUpdatesPermissionsWithinAllowed(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "PermUpdateable", 0)

	// Give member ManageRoles AND Kick — they can grant Kick to the role
	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles|rbac.PermKick))

	newPerms := int64(rbac.PermKick)
	body := map[string]interface{}{
		"permissions": fmt.Sprintf("%d", newPerms),
	}
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	role := resp["role"].(map[string]interface{})
	permStr := role["permissions"].(string)
	assert.Equal(t, fmt.Sprintf("%d", newPerms), permStr)
}

func TestGetMyServerPermissionsNonMember(t *testing.T) {
	ts, _, _, serverID := setupOwnerAndMember(t)
	outsider := ts.CreateTestUser(t, "srvpermout"+uuid.New().String()[:6])

	w := ts.DoRequest("GET", serverPermissionsPath(serverID), nil, testhelpers.AuthHeaders(outsider.AccessToken))
	// Non-member is blocked by membership middleware → 403
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListRolesMemberCanList(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Create multiple roles to trigger iteration
	createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ListA", 0)
	createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ListB", 0)

	w := ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	roles := body["roles"].([]interface{})
	// @all + admin (from AddMemberToServer) + 2 new roles = at least 3
	assert.GreaterOrEqual(t, len(roles), 3)

	// Verify structure of returned roles
	for _, r := range roles {
		role := r.(map[string]interface{})
		assert.NotEmpty(t, role["id"])
		assert.NotEmpty(t, role["server_id"])
		assert.NotEmpty(t, role["name"])
	}
}

func TestDeleteRoleMemberDeletesLowerRoleVerifyPosition(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Create role first so its position is lower
	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "DelByMember", 0)

	// Then give member high position with ManageRoles
	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	// Member should be able to delete a role below their position
	w := ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Role deleted", resp["message"])
}

func TestUpsertCategoryOverrideDenyBitsNoEscalation(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-deny-bits")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageChannels))

	// Deny bits should be allowed even for permissions the actor doesn't have
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermBan | rbac.PermAdministrator),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestSetChannelPermSyncEnableSyncNoCategoryOverrides(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-empty-cat")
	channelID := ts.CreateTestChannel(t, serverID, "sync-empty-ch")
	assignChannelToCategory(t, ts, channelID, catID, false)

	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w := ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// No category overrides to copy — channel should have no overrides
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestSetChannelPermSyncEnableSyncWithExistingChannelOverrides(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "sync-replace")
	channelID := ts.CreateTestChannel(t, serverID, "sync-replace-ch")
	assignChannelToCategory(t, ts, channelID, catID, false)

	// Create a channel override directly
	ts.CreateChannelOverride(t, channelID, "user", member.ID, int64(rbac.PermSendMessages), 0)

	// Create a different category override
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	catBody := map[string]interface{}{
		"target_type": "role",
		"target_id":   allRoleID,
		"allow":       0,
		"deny":        int64(rbac.PermAttachFiles),
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), catBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Enable sync — should replace channel overrides with category overrides
	body := map[string]interface{}{
		"sync_permissions": true,
	}
	w = ts.DoRequest("PUT", channelPermSyncPath(channelID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify: old user override removed, new role override from category present
	var count int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'user'`, channelID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "old user override should be removed by sync")

	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'role'`, channelID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "category role override should be synced")
}

func TestAssignRoleMemberWithPermissionOwnerBypass(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// Create a high-position role
	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighAssignOwner", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 100 WHERE id = $1`, roleID)
	require.NoError(t, err)

	// Owner can assign any role regardless of position
	body := map[string]interface{}{
		"role_id": roleID,
	}
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUnassignRoleOwnerBypassHierarchy(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "HighUnassignOwner", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 100 WHERE id = $1`, roleID)
	require.NoError(t, err)

	// Assign it first
	assignBody := map[string]interface{}{"role_id": roleID}
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), assignBody, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Owner can unassign regardless of position
	w = ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, roleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteRoleSuccessMemberThenVerifyGone(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "MemberWillDelete", 0)

	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	w := ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify gone via list
	w = ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	roles := resp["roles"].([]interface{})
	for _, r := range roles {
		role := r.(map[string]interface{})
		assert.NotEqual(t, roleID, role["id"])
	}
}

func TestListRolesReturnsAllFields(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	// Create roles with all optional fields
	color := "#112233"
	emoji := "🔥"
	body := map[string]interface{}{
		"name":               "FullFieldRole",
		"permissions":        fmt.Sprintf("%d", int64(rbac.PermKick)),
		"color":              color,
		"emoji":              emoji,
		"mentionable":        true,
		"display_separately": true,
	}
	w := ts.DoRequest("POST", rolesPath(serverID), body, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// List roles and verify fields
	w = ts.DoRequest("GET", rolesPath(serverID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	roles := resp["roles"].([]interface{})

	var foundCustom bool
	for _, r := range roles {
		role := r.(map[string]interface{})
		if role["name"] == "FullFieldRole" {
			foundCustom = true
			assert.Equal(t, color, role["color"])
			assert.Equal(t, emoji, role["emoji"])
			assert.Equal(t, true, role["mentionable"])
			assert.Equal(t, true, role["display_separately"])
			assert.Equal(t, false, role["is_default"])
			assert.Equal(t, false, role["is_managed"])
			assert.NotEmpty(t, role["id"])
			assert.NotEmpty(t, role["server_id"])
			assert.NotEmpty(t, role["created_at"])
			assert.NotEmpty(t, role["updated_at"])
		}
	}
	assert.True(t, foundCustom, "should find the custom role in list")
}

func TestGetAuditLogLimitBounds(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	createRoleViaAPI(t, ts, serverID, owner.AccessToken, "AuditBounds", 0)

	// Limit > 100 should be clamped to default 50
	w := ts.DoRequest("GET", auditLogPath(serverID)+"?limit=200", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, float64(50), resp["limit"])

	// Limit = 0 should be clamped to default 50
	w = ts.DoRequest("GET", auditLogPath(serverID)+"?limit=0", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, float64(50), resp["limit"])

	// Negative offset should use default 0
	w = ts.DoRequest("GET", auditLogPath(serverID)+"?offset=-1", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, float64(0), resp["offset"])
}

func TestUpsertChannelOverrideAdminBypassesEscalation(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "admin-bypass")

	// Give member Administrator
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermAdministrator))

	// Admin can grant any permission via channel override, even ones they don't have on role
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermBan | rbac.PermKick),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpsertCategoryOverrideAdminBypassesEscalation(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-admin-bypass")

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermAdministrator))

	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       int64(rbac.PermBan | rbac.PermKick),
		"deny":        0,
	}
	w := ts.DoRequest("PUT", categoryOverridesPath(catID), body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteCategoryOverrideInvalidOverrideID(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)
	catID := createTestCategory(t, ts, serverID, "cat-del-inv")

	w := ts.DoRequest("DELETE", categoryOverridePath(catID, invalidUUID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderRolesInvalidBodyNotJSON(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID), "{bad", testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAssignRoleInvalidBodyNotJSON(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID), "{bad", testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteRoleOwnerDeletesHighRole(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "OwnerHighDel", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 99 WHERE id = $1`, roleID)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", rolePath(serverID, roleID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetAuditLogMemberWithViewAuditLog(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermViewAuditLog))

	createRoleViaAPI(t, ts, serverID, owner.AccessToken, "AuditVisible", 0)

	w := ts.DoRequest("GET", auditLogPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	entries := resp["entries"].([]interface{})
	assert.GreaterOrEqual(t, len(entries), 1)
}
