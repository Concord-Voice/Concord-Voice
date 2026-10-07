//go:build integration

package testhelpers

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddMemberToServer_ReusesAdminRole(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	ts := &TestServer{DB: db}

	ownerID := testdb.CreateUser(t, db).String()
	firstAdminID := testdb.CreateUser(t, db).String()
	secondAdminID := testdb.CreateUser(t, db).String()
	serverID := ts.CreateTestServer(t, ownerID, "two admins")

	ts.AddMemberToServer(t, serverID, firstAdminID, "admin")
	ts.AddMemberToServer(t, serverID, secondAdminID, "admin")

	var adminRoleID string
	var position int
	var permissions int64
	var isDefault, isManaged bool
	err := db.QueryRow(`
		SELECT id, position, permissions, is_default, is_managed
		FROM roles WHERE server_id = $1 AND name = 'admin'
	`, serverID).Scan(&adminRoleID, &position, &permissions, &isDefault, &isManaged)
	require.NoError(t, err)
	require.NotEmpty(t, adminRoleID)
	assert.Equal(t, 10, position)
	assert.Equal(t, int64(rbac.AdminPermissions), permissions)
	assert.False(t, isDefault)
	assert.True(t, isManaged)

	for _, userID := range []string{firstAdminID, secondAdminID} {
		var assignedRoleID string
		err := db.QueryRow(`
			SELECT mr.role_id FROM member_roles mr
			JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
			WHERE mr.server_id = $1 AND mr.user_id = $2 AND r.name = 'admin'
		`, serverID, userID).Scan(&assignedRoleID)
		require.NoError(t, err)
		assert.Equal(t, adminRoleID, assignedRoleID, "both admins must share the stored role")

		var hasDefaultRole bool
		err = db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM member_roles mr
				JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
				WHERE mr.server_id = $1 AND mr.user_id = $2 AND r.name = '@all'
			)
		`, serverID, userID).Scan(&hasDefaultRole)
		require.NoError(t, err)
		assert.True(t, hasDefaultRole, "each admin must retain the default role")
	}
}

func TestAddMemberToServer_RejectsIncompatibleAdminRole(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	ts := &TestServer{DB: db}

	ownerID := testdb.CreateUser(t, db).String()
	serverID := ts.CreateTestServer(t, ownerID, "conflicting admin")
	adminRoleID := ts.CreateTestRole(t, serverID, "admin", 10, 0)
	_, err := db.Exec(`UPDATE roles SET is_managed = TRUE WHERE id = $1`, adminRoleID)
	require.NoError(t, err)

	reusedRoleID, err := ts.getOrCreateAdminRole(serverID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Empty(t, reusedRoleID)

	var permissions int64
	require.NoError(t, db.QueryRow(`SELECT permissions FROM roles WHERE id = $1`, adminRoleID).Scan(&permissions))
	assert.Zero(t, permissions, "incompatible role must remain unchanged")
}
