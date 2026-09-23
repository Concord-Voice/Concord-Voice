//go:build integration

package rbac_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test2907AuthorityLimitFailsClosed exercises the single bounded fan-out guard
// through every public writer that can reach it. The rows are checked after each
// refusal so a limit error cannot accidentally commit the durable mutation.
func Test2907AuthorityLimitFailsClosed(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	roleID := ts.CreateTestRole(t, serverID, "limit-role", 1, 0)
	assignedRoleID := ts.CreateTestRole(t, serverID, "assigned-role", 1, 0)
	ts.AssignRoleToUser(t, serverID, member.ID, assignedRoleID)
	categoryID := createTestCategory(t, ts, serverID, "limit-category")
	for i := 0; i < 501; i++ {
		_, err := ts.DB.Exec(`INSERT INTO channels (id, server_id, name, type, group_id, sync_permissions) VALUES ($1, $2, $3, 'text', $4, TRUE)`,
			uuid.NewString(), serverID, fmt.Sprintf("limit-%03d", i), categoryID)
		require.NoError(t, err)
	}
	overrideID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny) VALUES ($1, $2, 'role', $3, 0, 1)`, overrideID, categoryID, assignedRoleID)
	require.NoError(t, err)

	assertLimit := func(method, path string, body interface{}) {
		t.Helper()
		w := ts.DoRequest(method, path, body, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusConflict, w.Code, "%s %s", method, path)
	}

	assertLimit("PATCH", fmt.Sprintf("/api/v1/servers/%s/roles/%s", serverID, roleID), map[string]interface{}{"permissions": "1"})
	var roleName string
	require.NoError(t, ts.DB.QueryRow(`SELECT name FROM roles WHERE id = $1`, roleID).Scan(&roleName))
	assert.Contains(t, roleName, "limit-role")

	deleteRoleID := ts.CreateTestRole(t, serverID, "delete-limit-role", 1, 0)
	assertLimit("DELETE", fmt.Sprintf("/api/v1/servers/%s/roles/%s", serverID, deleteRoleID), nil)
	var roleCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM roles WHERE id = $1`, deleteRoleID).Scan(&roleCount))
	assert.Equal(t, 1, roleCount)

	assignRoleID := ts.CreateTestRole(t, serverID, "assign-limit-role", 1, 0)
	assertLimit("POST", fmt.Sprintf("/api/v1/servers/%s/members/%s/roles", serverID, member.ID), map[string]string{"role_id": assignRoleID})
	var assignmentCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, member.ID, assignRoleID).Scan(&assignmentCount))
	assert.Equal(t, 0, assignmentCount)

	assertLimit("DELETE", fmt.Sprintf("/api/v1/servers/%s/members/%s/roles/%s", serverID, member.ID, assignedRoleID), nil)
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, member.ID, assignedRoleID).Scan(&assignmentCount))
	assert.Equal(t, 1, assignmentCount)

	assertLimit("PUT", fmt.Sprintf("/api/v1/categories/%s/overrides", categoryID), map[string]interface{}{"target_type": "role", "target_id": assignedRoleID, "allow": 1, "deny": 0})
	var deny int64
	require.NoError(t, ts.DB.QueryRow(`SELECT deny FROM category_permission_overrides WHERE id = $1`, overrideID).Scan(&deny))
	assert.Equal(t, int64(1), deny)

	assertLimit("DELETE", fmt.Sprintf("/api/v1/categories/%s/overrides/%s", categoryID, overrideID), nil)
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM category_permission_overrides WHERE id = $1`, overrideID).Scan(&roleCount))
	assert.Equal(t, 1, roleCount)
}
