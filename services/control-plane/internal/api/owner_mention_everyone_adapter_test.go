package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// buildOwnerMentionEveryoneFixture builds a server, its owner, a plain member
// (holding only the default @all role) and one text channel — mirroring
// testhelpers.CreateTestServer / CreateTestChannel / AddMemberToServer by
// hand, since the top-level testhelpers package imports internal/api and an
// in-package (package api) test importing it would form an import cycle (see
// the note atop redemption_route_test.go for the same constraint from the
// other direction).
func buildOwnerMentionEveryoneFixture(t *testing.T, db *sql.DB) (serverID, ownerID, memberID, channelID string) {
	t.Helper()

	owner := dbtest.CreateUser(t, db)
	member := dbtest.CreateUser(t, db)

	server := uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`,
		server, "RS9 Adapter Mention Everyone", owner)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		server, owner)
	require.NoError(t, err)

	allRole := uuid.New()
	_, err = db.Exec(`
		INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, '@all', 0, $3, TRUE, TRUE)`,
		allRole, server, int64(rbac.BasePermissions))
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		server, owner, allRole)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
		server, member)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		server, member, allRole)
	require.NoError(t, err)

	channel := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`,
		channel, server, "rs9-adapter-channel")
	require.NoError(t, err)

	return server.String(), owner.String(), member.String(), channel.String()
}

// TestPermissionCheckerAdapter_OwnerHasMentionEveryone drives the actual
// PRODUCTION adapter wired to the Hub's mentionChecker — permissionCheckerAdapter,
// constructed at router.go:812 as &permissionCheckerAdapter{resolver: rbacResolver}
// — rather than the bare *rbac.Resolver or a mock. This is the "test the
// consumer" pairing for TestOwnerHasMentionEveryone_Issue3453RS9 in
// internal/rbac and stands in for the websocket-level consumer test, which
// cannot compile in internal/websocket (internal/rbac imports internal/websocket
// via authority_tx.go / handlers.go, so a package-websocket test importing
// internal/rbac is a genuine import cycle). internal/api is the composition
// root that wires the two together, so it is the layer that CAN exercise
// the real adapter.
//
// regression for #3453 (RS9)
func TestPermissionCheckerAdapter_OwnerHasMentionEveryone(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	redisClient := redistest.Client(t)

	resolver := rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), logger.New("test"))
	a := &permissionCheckerAdapter{resolver: resolver}

	serverID, ownerID, memberID, channelID := buildOwnerMentionEveryoneFixture(t, db)
	ctx := context.Background()

	t.Run("owner server scope", func(t *testing.T) {
		has, err := a.HasMentionPermission(ctx, serverID, ownerID, "", int64(rbac.PermMentionEveryone))
		require.NoError(t, err)
		assert.True(t, has,
			"permissionCheckerAdapter — what websocket enforceEveryonePerm consumes — must report the "+
				"owner has PermMentionEveryone (@all/@here); OwnerPermissions omits bit 24")
	})

	t.Run("owner channel scope", func(t *testing.T) {
		has, err := a.HasMentionPermission(ctx, serverID, ownerID, channelID, int64(rbac.PermMentionEveryone))
		require.NoError(t, err)
		assert.True(t, has,
			"permissionCheckerAdapter — what websocket enforceEveryonePerm consumes — must report the "+
				"owner has PermMentionEveryone (@all/@here) at channel scope; OwnerPermissions omits bit 24")
	})

	// Discriminating negative: the adapter must still report false for a plain
	// member at both scopes, so a fix that granted bit 24 unconditionally would
	// not pass this test.
	t.Run("member server scope denied", func(t *testing.T) {
		has, err := a.HasMentionPermission(ctx, serverID, memberID, "", int64(rbac.PermMentionEveryone))
		require.NoError(t, err)
		assert.False(t, has, "plain member must not report PermMentionEveryone (@all/@here) through the adapter")
	})

	t.Run("member channel scope denied", func(t *testing.T) {
		has, err := a.HasMentionPermission(ctx, serverID, memberID, channelID, int64(rbac.PermMentionEveryone))
		require.NoError(t, err)
		assert.False(t, has, "plain member must not report PermMentionEveryone (@all/@here) through the adapter at channel scope")
	})
}
