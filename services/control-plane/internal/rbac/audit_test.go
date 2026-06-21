package rbac_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupAudit(t *testing.T) (*rbac.AuditWriter, *testhelpers.TestServer) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	log := logger.New("test")
	audit := rbac.NewAuditWriter(ts.DB, log)
	return audit, ts
}

func TestAuditLogCreate(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner1")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server")

	actorID := owner.ID
	err := audit.Log(ctx, serverID, &actorID, "role_created", "role", nil, map[string]interface{}{
		"role_name": "Moderator",
	})
	require.NoError(t, err)

	// Verify entry exists
	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "role_created", entries[0].Action)
	assert.Equal(t, "role", entries[0].TargetType)
	assert.Equal(t, &actorID, entries[0].ActorID)
	assert.Equal(t, "Moderator", entries[0].Metadata["role_name"])
}

func TestAuditLogCreateWithNilActor(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner2")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 2")

	// System action (nil actor)
	err := audit.Log(ctx, serverID, nil, "system_action", "server", nil, map[string]interface{}{
		"reason": "automated cleanup",
	})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Nil(t, entries[0].ActorID)
	assert.Equal(t, "system_action", entries[0].Action)
}

func TestAuditLogCreateWithTargetID(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner3")
	member := ts.CreateTestUser(t, "auditmember3")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 3")

	actorID := owner.ID
	targetID := member.ID
	err := audit.Log(ctx, serverID, &actorID, "member_kicked", "member", &targetID, map[string]interface{}{
		"reason": "rule violation",
	})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, &targetID, entries[0].TargetID)
	assert.Equal(t, "member_kicked", entries[0].Action)
}

func TestAuditLogMultipleEntries(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner4")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 4")

	actorID := owner.ID
	for i := 0; i < 5; i++ {
		err := audit.Log(ctx, serverID, &actorID, "test_action", "role", nil, map[string]interface{}{
			"index": i,
		})
		require.NoError(t, err)
	}

	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 5)
}

func TestAuditLogPagination(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner5")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 5")

	actorID := owner.ID
	for i := 0; i < 5; i++ {
		err := audit.Log(ctx, serverID, &actorID, "paginated_action", "role", nil, map[string]interface{}{
			"index": i,
		})
		require.NoError(t, err)
	}

	// Page 1: limit 2, offset 0
	entries, err := audit.GetAuditLog(ctx, serverID, 2, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	// Page 2: limit 2, offset 2
	entries, err = audit.GetAuditLog(ctx, serverID, 2, 2)
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	// Page 3: limit 2, offset 4
	entries, err = audit.GetAuditLog(ctx, serverID, 2, 4)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestAuditLogEmptyMetadata(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner6")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 6")

	actorID := owner.ID
	err := audit.Log(ctx, serverID, &actorID, "simple_action", "server", nil, map[string]interface{}{})
	require.NoError(t, err)

	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestAuditLogEmptyServer(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner7")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 7")

	// No audit entries written
	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 0)
}

func TestAuditLogOrderDescByCreatedAt(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner8")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server 8")

	actorID := owner.ID
	require.NoError(t, audit.Log(ctx, serverID, &actorID, "first_action", "role", nil, map[string]interface{}{}))
	require.NoError(t, audit.Log(ctx, serverID, &actorID, "second_action", "role", nil, map[string]interface{}{}))

	entries, err := audit.GetAuditLog(ctx, serverID, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Most recent first (DESC)
	assert.Equal(t, "second_action", entries[0].Action)
	assert.Equal(t, "first_action", entries[1].Action)
}

func TestAuditLogIsolatedPerServer(t *testing.T) {
	audit, ts := setupAudit(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "auditowner9")
	server1 := ts.CreateTestServer(t, owner.ID, "Audit Server A")
	server2 := ts.CreateTestServer(t, owner.ID, "Audit Server B")

	actorID := owner.ID
	require.NoError(t, audit.Log(ctx, server1, &actorID, "action_a", "role", nil, map[string]interface{}{}))
	require.NoError(t, audit.Log(ctx, server2, &actorID, "action_b", "role", nil, map[string]interface{}{}))

	entries1, err := audit.GetAuditLog(ctx, server1, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries1, 1)
	assert.Equal(t, "action_a", entries1[0].Action)

	entries2, err := audit.GetAuditLog(ctx, server2, 10, 0)
	require.NoError(t, err)
	assert.Len(t, entries2, 1)
	assert.Equal(t, "action_b", entries2[0].Action)
}
