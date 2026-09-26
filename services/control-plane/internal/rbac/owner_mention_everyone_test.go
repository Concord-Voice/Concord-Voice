package rbac_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
)

// TestOwnerHasMentionEveryone_Issue3453RS9 pins the oracle: a server owner's
// @all/@here survives mention enforcement — HasPermission(owner,
// PermMentionEveryone) is true at both server and channel scope — while a
// plain member's @all/@here is stripped.
//
// regression for #3453 (RS9)
func TestOwnerHasMentionEveryone_Issue3453RS9(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "rs9owner")
	member := ts.CreateTestUser(t, "rs9member")
	serverID := ts.CreateTestServer(t, owner.ID, "RS9 Mention Everyone")
	channelID := ts.CreateTestChannel(t, serverID, "rs9-channel")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Vacuity guard: the owner must hold no role granting bit 24. CreateTestServer
	// assigns the owner only the @all role (rbac.BasePermissions), which excludes
	// PermMentionEveryone — assert that directly against the raw role bitfield so
	// a future fixture change can't silently grant the owner a role that would
	// make this test pass for the wrong reason (vacuity mode 3 / mode 6).
	var ownerRoleBits int64
	err := ts.DB.QueryRowContext(ctx, `
		SELECT COALESCE(BIT_OR(r.permissions), 0)
		FROM member_roles mr
		JOIN roles r ON r.id = mr.role_id AND r.server_id = mr.server_id
		WHERE mr.server_id = $1 AND mr.user_id = $2
	`, serverID, owner.ID).Scan(&ownerRoleBits)
	require.NoError(t, err, "reading owner's raw role BIT_OR")
	require.Zero(t, rbac.Permission(ownerRoleBits)&rbac.PermMentionEveryone,
		"fixture invalid: owner holds a role granting PermMentionEveryone (bit 24); "+
			"this would make the test pass without exercising OwnerPermissions at all")

	t.Run("owner server scope", func(t *testing.T) {
		has, err := resolver.HasPermission(ctx, serverID, owner.ID, "", rbac.PermMentionEveryone)
		require.NoError(t, err)
		assert.True(t, has,
			"server owner must hold PermMentionEveryone (@all/@here); OwnerPermissions omits bit 24")
	})

	t.Run("owner channel scope", func(t *testing.T) {
		has, err := resolver.HasPermission(ctx, serverID, owner.ID, channelID, rbac.PermMentionEveryone)
		require.NoError(t, err)
		assert.True(t, has,
			"server owner must hold PermMentionEveryone (@all/@here) at channel scope; OwnerPermissions omits bit 24")
	})

	// Discriminating negative: a plain member (only the default @all role, which
	// lacks bit 24) must NOT have PermMentionEveryone at either scope. Without
	// this, a broken fix that granted bit 24 to everyone would also make the
	// owner assertions above pass, proving nothing about OwnerPermissions
	// specifically.
	t.Run("member server scope denied", func(t *testing.T) {
		has, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermMentionEveryone)
		require.NoError(t, err)
		assert.False(t, has, "plain member must not hold PermMentionEveryone (@all/@here)")
	})

	t.Run("member channel scope denied", func(t *testing.T) {
		has, err := resolver.HasPermission(ctx, serverID, member.ID, channelID, rbac.PermMentionEveryone)
		require.NoError(t, err)
		assert.False(t, has, "plain member must not hold PermMentionEveryone (@all/@here) at channel scope")
	})
}
