package rbac_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

// waitForBlockedSession polls pg_stat_activity until a session is parked on a
// lock, so the interleaving below is deterministic rather than a timing race.
// Post-fix the parked statement is the advisory lock; pre-fix it is the
// position UPDATE. Either one proves the handler has passed its authorization
// reads and is already committed to writing.
func waitForBlockedSession(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := db.QueryRow(`
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND (query ILIKE '%pg_advisory_xact_lock%' OR query ILIKE '%UPDATE roles SET position%')`).Scan(&n)
		require.NoError(t, err)
		if n > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no session became lock-blocked within 10s; the interleaving did not set up")
}

// TestReorderRoles_TOCTOU_PromotedTargetIsRefused is the inverted PoC from
// issue #2851 (TestPoC_H4a_ReorderTOCTOU). It PASSED pre-fix by observing 200:
// an actor whose highest role sat at position 5 demoted a role that had been
// promoted to 19 after the handler's authorization reads had already passed.
// It now asserts 403. If this starts failing, CWE-367 is back on this handler.
func TestReorderRoles_TOCTOU_PromotedTargetIsRefused(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageRoles))

	target := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "TOCTOUTarget", 0)
	filler := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "TOCTOUFiller", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 2 WHERE id IN ($1, $2)`, target, filler)
	require.NoError(t, err)

	// The helper stands in for the concurrent promoter the issue describes —
	// "the owner or another admin issuing their own reorder" — so it holds the
	// SAME per-server advisory lock every role-authority writer takes, and a row
	// lock on the target as the original proof of concept does.
	//
	// Both locks matter, and for different halves of this test. Pre-fix the
	// handler took no advisory lock at all, so it parked on the ROW lock
	// mid-UPDATE with its authorization already passed, and the promotion then
	// landed underneath it. Post-fix it parks on the ADVISORY lock before
	// reading anything authoritative, so the promotion is visible to the
	// re-evaluation. Holding only the row lock would model a promoter that
	// bypasses the advisory order, which no writer in the control plane does:
	// applyRolePositions is the sole UPDATE of roles.position.
	lockKey, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)

	holder, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()

	_, err = holder.Exec(`SELECT pg_advisory_xact_lock($1)`, lockKey)
	require.NoError(t, err)

	var parked string
	require.NoError(t, holder.QueryRow(`SELECT id FROM roles WHERE id = $1 FOR UPDATE`, target).Scan(&parked))

	codes := make(chan int, 1)
	go func() {
		rec := ts.DoRequest("PATCH", reorderRolesPath(serverID),
			map[string]interface{}{"role_ids": []string{target, filler}},
			testhelpers.AuthHeaders(member.AccessToken))
		codes <- rec.Code
	}()

	waitForBlockedSession(t, ts.DB)

	// Promote the target far above the actor, then commit.
	_, err = holder.Exec(`UPDATE roles SET position = 19 WHERE id = $1`, target)
	require.NoError(t, err)
	require.NoError(t, holder.Commit())

	select {
	case code := <-codes:
		assert.Equal(t, http.StatusForbidden, code,
			"an actor at position 5 must not reorder a role promoted to 19; 200 here is CWE-367")
	case <-time.After(20 * time.Second):
		t.Fatal("handler did not return within 20s")
	}

	var finalPosition int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, target).Scan(&finalPosition))
	assert.Equal(t, 19, finalPosition, "the refused reorder must not have written a position")
}

// clearReorderRateLimit deletes a user's rate-limit counters so a contention
// test can run more rounds than the route's 5/min budget allows. It is scoped
// to the one user's keys — never a FLUSHDB, which would destroy session and
// token state this suite depends on.
func clearReorderRateLimit(t *testing.T, ts *testhelpers.TestServer, userIDs ...string) {
	t.Helper()
	for _, id := range userIDs {
		keys, err := ts.Redis.Keys(context.Background(), "ratelimit:user:"+id+":*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, ts.Redis.Del(context.Background(), keys...).Err())
		}
	}
}

// TestReorderRoles_ConcurrentOppositeOrders_Serialize locks the live 40P01
// deadlock reported on issue #2851. applyRolePositions iterates the
// CLIENT-SUPPLIED role order, so pre-fix two concurrent reorders submitting
// opposing orderings took row locks in opposite sequences —
// "T1 holds A wants B / T2 holds B wants A" — and the losing side got a
// generic 500 with nothing written to the audit log.
//
// It uses TWO admins deliberately, which is both the reported scenario ("two
// users reordering roles on the same server at the same time") and the case
// the route's own 5/min-per-USER rate limit does not throttle at all.
//
// Self-validating, following the #2721 occupancy-test precedent: it first
// asserts an uncontended reorder succeeds, so a failure below is genuine
// contention rather than a mis-seeded fixture producing a green test that
// proves nothing.
//
// WHAT THIS TEST DOES AND DOES NOT PIN — stated because it changed during
// review. When written, the write was a per-role loop and this test failed
// with 40P01 whenever the advisory lock was removed, so it pinned the LOCK.
// The write is now a single batched UPDATE, and two concurrent single
// statements of the same shape cannot deadlock — so this test passes with the
// lock removed and no longer isolates it. It still pins the OUTCOME (no
// deadlock reaches a caller) via either mechanism, which is what users care
// about. The advisory lock's own regression pin is
// TestReorderRoles_TOCTOU_PromotedTargetIsRefused, which was re-verified red
// with the lock disabled AFTER the batched rewrite.
func TestReorderRoles_ConcurrentOppositeOrders_Serialize(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// A second admin, high enough that every role below is fair game.
	grantPermToUser(t, ts, serverID, member.ID, 20, int64(rbac.PermManageRoles))

	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "DeadlockA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "DeadlockB", 0)
	roleC := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "DeadlockC", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET position = 0 WHERE id IN ($1, $2, $3)`, roleA, roleB, roleC)
	require.NoError(t, err)

	forward := []string{roleA, roleB, roleC}
	reverse := []string{roleC, roleB, roleA}

	warm := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": forward}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, warm.Code, "uncontended reorder must succeed; the fixture is wrong")

	type attempt struct {
		token string
		order []string
	}

	const rounds = 10
	var (
		mu     sync.Mutex
		codes  []int
		wg     sync.WaitGroup
		actors = []string{owner.AccessToken, member.AccessToken}
	)

	for r := 0; r < rounds; r++ {
		// Stay inside the 5/min-per-user budget: two requests per user per
		// round, counters cleared between rounds. A 429 here would mask the
		// deadlock this test exists to catch.
		clearReorderRateLimit(t, ts, owner.ID, member.ID)

		attempts := []attempt{
			{actors[0], forward}, {actors[1], reverse},
			{actors[0], reverse}, {actors[1], forward},
		}
		for _, a := range attempts {
			wg.Add(1)
			go func(a attempt) {
				defer wg.Done()
				rec := ts.DoRequest("PATCH", reorderRolesPath(serverID),
					map[string]interface{}{"role_ids": a.order},
					testhelpers.AuthHeaders(a.token))
				mu.Lock()
				codes = append(codes, rec.Code)
				mu.Unlock()
			}(a)
		}
		wg.Wait()
	}

	require.Len(t, codes, rounds*4)
	for idx, code := range codes {
		assert.Equal(t, http.StatusOK, code,
			"request %d returned %d; a 500 here is the 40P01 deadlock the advisory lock exists to prevent", idx, code)
	}
}

// TestReorderRoles_ManagedRole_Forbidden pins the F4-analogue fix: a managed
// role named in a reorder is refused by the guard, rather than passing it and
// being filtered out of the UPDATE afterwards.
//
// Originally that filter made the write a SILENT no-op — every other role
// moved, the managed role kept its position, two roles ended up sharing one
// position value, and the caller got 200 OK for a write that never happened.
// execRequiringRow already turned that into a hard failure, but it surfaced as
// 404 "Role not found", which is a lie about a role that plainly exists. The
// guard now gives the real answer.
func TestReorderRoles_ManagedRole_Forbidden(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	managed := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "ManagedReorder", 0)
	plain := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "PlainReorder", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET is_managed = TRUE WHERE id = $1`, managed)
	require.NoError(t, err)

	var before int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, managed).Scan(&before))

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{managed, plain}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Cannot reorder managed roles", resp["error"])

	var after int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, managed).Scan(&after))
	assert.Equal(t, before, after, "a refused reorder must write nothing")
}

// TestReorderRoles_NullIsManaged_Reorderable pins the COALESCE interpretation.
// roles.is_managed is NULLABLE — 000035_rbac_system.up.sql declares it
// BOOLEAN DEFAULT FALSE with no NOT NULL, and no later migration tightens it —
// so the original `AND is_managed = FALSE` write filter evaluated to NULL for
// such a row and skipped it. That is the same silent-skip defect as an
// explicitly managed role, reached by a second and even less visible route.
//
// NULL means not-managed, which is what the column's own DEFAULT intends. The
// guard and the write filter both COALESCE so they cannot disagree.
func TestReorderRoles_NullIsManaged_Reorderable(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	nullFlag := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "NullManaged", 0)
	other := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "OtherManaged", 0)
	_, err := ts.DB.Exec(`UPDATE roles SET is_managed = NULL WHERE id = $1`, nullFlag)
	require.NoError(t, err)

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{nullFlag, other}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var position int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, nullFlag).Scan(&position))
	// Position 2, not 1: @all is not named here, so positionOffset reserves 0
	// for it and the two named roles take 2 and 1.
	assert.Equal(t, 2, position, "a NULL is_managed role must be reordered, not skipped")
}

// TestReorderRoles_UnknownRoleID_NotFound pins a deliberate behaviour change.
// The write loop previously used a bare tx.Exec, so a role_ids entry naming a
// role that does not exist on this server matched no rows, was skipped without
// comment, and the caller still got 200 OK — while every other named role was
// repositioned by list index, silently producing an order the caller never
// asked for. execRequiringRow now reports sql.ErrNoRows and the whole
// transaction rolls back.
//
// 404 is deliberate over 500: the role genuinely is not found, and the answer
// discloses nothing an actor with ManageRoles cannot already read from
// ListRoles on the same server. Input SHAPE (bounds, duplicates) is #2841.
func TestReorderRoles_UnknownRoleID_NotFound(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	real1 := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "RealReorder1", 0)
	real2 := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "RealReorder2", 0)

	var before1, before2 int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, real1).Scan(&before1))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, real2).Scan(&before2))

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{real1, uuid.New().String(), real2}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "Role not found", resp["error"])

	// The whole transaction rolls back — no partial reorder survives.
	var after1, after2 int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, real1).Scan(&after1))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, real2).Scan(&after2))
	assert.Equal(t, before1, after1, "a rejected reorder must not have repositioned real1")
	assert.Equal(t, before2, after2, "a rejected reorder must not have repositioned real2")
}

// TestReorderRoles_MalformedRoleID_Rejected closes CodeQL go/log-injection
// (alert 1262, medium) on this handler.
//
// ReorderRolesRequest.RoleIDs was `binding:"required"` with NO element
// validation, so an entry could be any string. A non-UUID entry reaches
// PostgreSQL as `roles.id = ANY($3)`, which fails with
// `invalid input syntax for type uuid: "<attacker string>"`. That error is
// neither sql.ErrNoRows nor a guard denial, so it lands in the default branch
// and is written to the log — carrying the attacker's string, newlines and
// all, into the log stream. A caller with ManageRoles could forge log entries.
//
// Validating the elements kills the class at the boundary rather than
// scrubbing it at the sink: no malformed value ever reaches the database, the
// log, or the 500 path. Input BOUNDS (max length, duplicates, @everyone) stay
// with #2841; this is only the element type.
func TestReorderRoles_MalformedRoleID_Rejected(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	real1 := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "MalformedPeer", 0)

	// Assert the SECURITY property, not just the status code. A 400 proves the
	// request was refused; only the log buffer proves the probe never reached a
	// sink, which is what go/log-injection is actually about.
	logs := ts.CaptureLogs(t)

	for _, probe := range []string{
		"not-a-uuid",
		"\n2026-01-01T00:00:00Z level=ERROR msg=\"forged log line\"",
		"'; DROP TABLE roles; --",
	} {
		w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
			map[string]interface{}{"role_ids": []string{real1, probe}},
			testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code,
			"malformed role_id %q must be rejected at the binding boundary, never reach the DB or the log", probe)
		assert.NotContains(t, logs.String(), probe,
			"the rejected probe %q must not appear in any log line", probe)
	}
}

// TestReorderRoles_EmptyRoleIDs_Rejected pins `min=1`. The `required` tag alone
// does NOT reject a non-nil empty slice — it only rejects nil — so
// `{"role_ids": []}` bound successfully and committed a no-op reorder reported
// as 200 OK. Verified by probe before min=1 was added.
func TestReorderRoles_EmptyRoleIDs_Rejected(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"an empty role_ids array must be rejected, not silently accepted as a no-op")
}

// TestReorderRoles_OmittingDefault_KeepsItStrictlyLowest covers the second way
// the @all invariant can break: leaving it OUT of the payload entirely. The
// last named role would otherwise take position 0 alongside @all — a tie, so
// @all is no longer STRICTLY lowest — and RowsAffected still matches, so the
// write commits. positionOffset reserves 0.
func TestReorderRoles_OmittingDefault_KeepsItStrictlyLowest(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	allRoleID := defaultRoleID(t, ts, serverID)
	a := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "OmitA", 0)
	b := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "OmitB", 0)

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{a, b}},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "a partial reorder omitting @all is legitimate and must succeed")

	var pAll, pA, pB int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id=$1`, allRoleID).Scan(&pAll))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id=$1`, a).Scan(&pA))
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id=$1`, b).Scan(&pB))

	assert.Equal(t, 0, pAll, "@all stays at position 0")
	assert.Greater(t, pB, pAll, "the lowest NAMED role must sit strictly above @all, not tie with it")
	assert.Greater(t, pA, pB, "relative order of the named roles is preserved")
}

// TestReorderRoles_FullListIncludingDefault_Succeeds is the natural payload the
// incoming reorder UI sends (PR #2839): the ENTIRE display-ordered role list,
// which necessarily contains @all.
//
// @all is created `is_default = TRUE, is_managed = TRUE` on every server
// (internal/servers/handlers.go:191), so a managed-role refusal that does not
// exempt the default role rejects every full-list reorder — for the owner too.
// #2839's roleOrder.ts builds its payload from the whole displayed list and
// only marks @all non-draggable, so it does send it.
func TestReorderRoles_FullListIncludingDefault_Succeeds(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	allRoleID := defaultRoleID(t, ts, serverID)

	var allManaged bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COALESCE(is_managed, FALSE) FROM roles WHERE id = $1`, allRoleID).Scan(&allManaged))
	require.True(t, allManaged, "premise: @all is created is_managed = TRUE")

	top := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "FullListTop", 0)
	mid := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "FullListMid", 0)

	// Display order: highest authority first, @all last — exactly what
	// roleOrder.ts produces, and position = len-i-1 lands @all back at 0.
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{top, mid, allRoleID}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code,
		"a full-list reorder naming @all must succeed; 403 here breaks the #2839 reorder UI")

	var allPos int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, allRoleID).Scan(&allPos))
	assert.Equal(t, 0, allPos, "@all must remain at the lowest position")
}

// defaultRoleID returns the server's @all role, which every member holds.
func defaultRoleID(t *testing.T, ts *testhelpers.TestServer, serverID string) string {
	t.Helper()
	var id string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&id))
	return id
}

// TestReorderRoles_DefaultRoleCannotBePromoted_EvenByOwner is the hard
// invariant: @all is ALWAYS the lowest role in the hierarchy, and nobody —
// not even the server owner — can promote it above anything.
//
// This is a security rule, not a cosmetic one. Every member of the server
// holds @all, so its position sets the floor of COALESCE(MAX(r.position), 0)
// for the entire membership. Promoting it by one reorder would raise EVERY
// member's hierarchy ceiling at once, handing the whole server authority over
// every role beneath the new position — a mass privilege change wearing the
// costume of a drag-and-drop.
func TestReorderRoles_DefaultRoleCannotBePromoted_EvenByOwner(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	allRoleID := defaultRoleID(t, ts, serverID)
	top := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "PromoteTop", 0)
	mid := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "PromoteMid", 0)

	var before int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, allRoleID).Scan(&before))

	// @all first in display order == highest authority == position len-1.
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{allRoleID, top, mid}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code,
		"not even the owner may promote the default role above other roles")

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "The default role must remain the lowest role", resp["error"])

	var after int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, allRoleID).Scan(&after))
	assert.Equal(t, before, after, "the refused reorder must not have moved @all")
	assert.Equal(t, 0, after, "@all remains the lowest role")
}

// TestReorderRoles_DefaultRoleMidList_Forbidden covers the subtler case: @all
// neither first nor last still resolves to a position above 0.
func TestReorderRoles_DefaultRoleMidList_Forbidden(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	allRoleID := defaultRoleID(t, ts, serverID)
	top := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "MidTop", 0)
	low := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "MidLow", 0)

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{top, allRoleID, low}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "@all anywhere but last resolves above position 0")

	var after int
	require.NoError(t, ts.DB.QueryRow(`SELECT position FROM roles WHERE id = $1`, allRoleID).Scan(&after))
	assert.Equal(t, 0, after)
}

// TestReorderRoles_DuplicateRoleIDs_Rejected pins the `unique` binding tag.
// Duplicates are not merely untidy: the batched UPDATE verifies
// RowsAffected == len(RoleIDs), and a repeated id matches one row while
// counting twice, so the all-or-nothing check would misfire.
func TestReorderRoles_DuplicateRoleIDs_Rejected(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleA := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "DupA", 0)
	roleB := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "DupB", 0)

	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": []string{roleA, roleB, roleA}},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestReorderRoles_OversizedPayload_Rejected pins the `max=500` bound. The
// advisory lock is held until COMMIT, so an unbounded list is a lock-hold
// question, not a formatting one.
func TestReorderRoles_OversizedPayload_Rejected(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	ids := make([]string, 501)
	for i := range ids {
		ids[i] = uuid.New().String()
	}
	w := ts.DoRequest("PATCH", reorderRolesPath(serverID),
		map[string]interface{}{"role_ids": ids},
		testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"an oversized reorder must be refused before it can hold the server-wide lock")
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

// TestAssignRole_PrivilegeEscalation locks the permission-subset guard (#2350).
//
// Only the "escalation_blocked" case is failing-first: against the pre-fix
// position-only handler it returns 200 and writes a member_roles row. The other
// two cases pass both before and after the fix — they are anti-over-block
// regression locks, one pinning the actor-vs-target orientation of the guard and
// one proving the owner bypass survives.
func TestAssignRole_PrivilegeEscalation(t *testing.T) {
	testCases := []struct {
		name            string
		actorIsOwner    bool
		actorPerms      int64
		actorPosition   int
		rolePerms       int64
		rolePosition    int
		selfAssign      bool
		wantStatus      int
		wantErrContains string
		wantRow         bool
	}{
		{
			// Actor holds the capability to assign roles but NOT PermBan, and
			// PermBan is not in BasePermissions, so the target role genuinely
			// confers a bit the actor lacks. Its position (2) is BELOW the
			// actor's (5), so the hierarchy check passes and only the subset
			// guard can stop it.
			name:            "escalation_blocked",
			actorPerms:      int64(rbac.PermManageRolesAssign),
			actorPosition:   5,
			rolePerms:       int64(rbac.PermBan),
			rolePosition:    2,
			selfAssign:      true,
			wantStatus:      http.StatusForbidden,
			wantErrContains: "Cannot grant permissions you do not have",
			wantRow:         false,
		},
		{
			// Same shape, except the actor already holds PermBan. Targets a
			// THIRD member, not the actor: if the guard is wired to
			// targetUserID instead of actorID it checks a base member who
			// lacks PermBan, and this case flips to 403.
			name:          "subset_allowed",
			actorPerms:    int64(rbac.PermManageRolesAssign | rbac.PermBan),
			actorPosition: 5,
			rolePerms:     int64(rbac.PermBan),
			rolePosition:  2,
			selfAssign:    false,
			wantStatus:    http.StatusOK,
			wantRow:       true,
		},
		{
			// OwnerPermissions deliberately excludes PermAdministrator
			// (types.go:85), so without the owner bypass the subset check
			// would reject this and the case would 403.
			name:         "owner_bypass",
			actorIsOwner: true,
			rolePerms:    int64(rbac.AdminPermissions | rbac.PermAdministrator),
			rolePosition: 50,
			selfAssign:   false,
			wantStatus:   http.StatusOK,
			wantRow:      true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts, owner, member, serverID := setupOwnerAndMember(t)

			actorToken := owner.AccessToken
			actorID := owner.ID
			if !tc.actorIsOwner {
				actorToken = member.AccessToken
				actorID = member.ID
				grantPermToUser(t, ts, serverID, member.ID, tc.actorPosition, tc.actorPerms)
			}

			targetID := actorID
			if !tc.selfAssign {
				target := ts.CreateTestUser(t, "esctgt"+uuid.New().String()[:6])
				ts.AddMemberToServer(t, serverID, target.ID, "member")
				targetID = target.ID
			}

			roleID := ts.CreateTestRole(t, serverID, "esc_"+uuid.New().String()[:8], tc.rolePosition, tc.rolePerms)

			body := map[string]interface{}{"role_id": roleID}
			w := ts.DoRequest("POST", assignRolePath(serverID, targetID), body, testhelpers.AuthHeaders(actorToken))

			require.Equal(t, tc.wantStatus, w.Code, "unexpected status; body: %s", w.Body.String())

			if tc.wantErrContains != "" {
				// Asserting the STRING matters: the RequirePermission middleware
				// also returns 403, so a bare status assertion would pass against
				// the vulnerable handler for the wrong reason.
				var resp map[string]interface{}
				testhelpers.ParseJSON(t, w, &resp)
				assert.Contains(t, resp["error"], tc.wantErrContains)
			}

			var exists bool
			require.NoError(t, ts.DB.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3)`,
				serverID, targetID, roleID,
			).Scan(&exists))
			assert.Equal(t, tc.wantRow, exists, "member_roles row presence")
		})
	}
}

// TestAssignRole_GuardError_FailsClosed covers the guard's 500 branch
// (#2350, retargeted by #2721).
//
// Two things are asserted that nothing else in the suite reaches:
//
//  1. The 500 body is errMsgFailedAssignRole ("Failed to assign role"), not
//     errMsgFailedUpdateRole. That string IS the entire reason the failureMsg
//     parameter exists — without this case the argument could be wired to the
//     wrong constant and every other test still passes.
//  2. No member_roles row is written. The guard must fail CLOSED: a guard
//     failure denies the assignment rather than falling through.
//
// WHY THE INJECTION SEAM MOVED (#2721). This test previously injected via
// testhelpers.BrokenResolver — a *rbac.Resolver over a CLOSED database — because
// the escalation guard called ResolveEffectivePermissionsFresh, which read
// through the resolver's OWN db handle. #2721 moved the actor resolve inside the
// write transaction via ResolveServerPermissionsTx, which reads through the
// CALLER'S transaction and never touches r.db. That is the entire point of the
// change, and it makes a closed-db resolver inert here: every read succeeds on
// the handler's working transaction, so the old seam injected nothing and the
// assignment completed.
//
// The property under test did not weaken — it strengthened. The resolve now runs
// in the same transaction as the INSERT, so ANY guard error rolls the write back,
// rather than relying on an early return happening before the write. Fail-closed
// is now structural rather than control-flow.
//
// So the injection moves to a failure the new path can actually take: a
// conflicting row lock on the target role, held from a second connection, makes
// the guard's `FOR SHARE OF r` block until its `SET LOCAL lock_timeout = '3s'`
// fires (PostgreSQL 55P03) — the guard_lock_timeout branch #2721 introduced.
// BrokenResolver remains the right seam for the five other call sites that still
// resolve on the pool; it is only THIS guard that stopped reading r.db.
func TestAssignRole_GuardError_FailsClosed(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)

	// A role the actor could legitimately assign if the guard succeeded:
	// permissions 0 is a subset of anything, and position 2 is below the actor's 5.
	roleID := ts.CreateTestRole(t, serverID, "failclosed_"+uuid.New().String()[:8], 2, 0)
	grantPermToUser(t, ts, serverID, member.ID, 5, int64(rbac.PermManageRolesAssign))

	// Hold a conflicting lock on the target role from a SEPARATE POOL — never
	// ts.DB, which is the handler's own 5-connection pool. Borrowing one of those
	// to hold the barrier starves the request under test, and the starvation would
	// present as a lock-timeout flake indistinguishable from the behaviour this
	// test asserts. openLockProbePool exists for exactly this (#2721).
	//
	// FOR NO KEY UPDATE conflicts with the guard's FOR SHARE, so the guard blocks
	// and its lock_timeout fires. Rolled back by the defer, never committed, so
	// the row itself is unchanged.
	//
	// COST: this test spends a hard 3s of wall clock by construction — it waits
	// out `SET LOCAL lock_timeout = '3s'` (role_guard.go). That is the assertion,
	// not overhead. Do not "optimise" the timeout constant to speed this up.
	blocker, blockErr := openLockProbePool(t).Begin()
	require.NoError(t, blockErr)
	defer func() { _ = blocker.Rollback() }()
	var blockedPosition int
	require.NoError(t, blocker.QueryRow(
		`SELECT position FROM roles WHERE id = $1 FOR NO KEY UPDATE`, roleID,
	).Scan(&blockedPosition))

	handler := rbac.NewHandler(
		ts.DB,              // working DB: pre-checks and BeginTx succeed
		logger.New("test"), //
		ts.Redis,           //
		nil,                // hub: unreached, the 500 returns before any broadcast
		rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test")),
		rbac.NewPermissionCache(ts.Redis), //
		nil,                               // audit: unreached, and nil-checked regardless
	)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: serverID},
		{Key: "user_id", Value: owner.ID},
	}
	c.Set("user_id", member.ID) // non-owner actor, so the guard is not bypassed
	body := `{"role_id":"` + roleID + `"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AssignRole(c)

	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Failed to assign role", resp["error"],
		"500 body must name the assign operation, not update — this is what failureMsg exists for")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3)`,
		serverID, owner.ID, roleID,
	).Scan(&exists))
	assert.False(t, exists, "a guard failure must not write a member_roles row (fail closed)")
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
