package rbac_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- GetReadableChannelIDs / GetAllReadableChannelIDs Tests ---
//
// "Readable" is visibility PLUS PermReadMessageHistory. The two permissions are
// separately deniable and must stay separate: the view bit decides whether a
// channel appears at all, history decides whether anything derived from its
// message content (an unread count, most obviously) may be served. These tests
// pin both directions — the history denial narrows the readable set and leaves the
// visible set alone.

func TestGetReadableChannelIDsExcludesHistoryDeniedChannel(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "readowner1")
	member := ts.CreateTestUser(t, "readmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "Readable Server 1")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chOpen := ts.CreateTestChannel(t, serverID, "read-open")
	chSealed := ts.CreateTestChannel(t, serverID, "read-sealed")

	// View is untouched; only read_message_history is denied, on one channel.
	ts.CreateChannelOverride(t, chSealed, "user", member.ID, 0, int64(rbac.PermReadMessageHistory))

	visible, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, visible, chOpen)
	assert.Contains(t, visible, chSealed, "a history denial must not change VISIBILITY")

	readable, err := resolver.GetReadableChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, readable, chOpen)
	assert.NotContains(t, readable, chSealed, "history-denied channel must not be readable")

	allReadable, err := resolver.GetAllReadableChannelIDs(ctx, member.ID)
	require.NoError(t, err)
	assert.Contains(t, allReadable, chOpen)
	assert.NotContains(t, allReadable, chSealed,
		"the cross-server resolver must agree with the per-server one")
}

func TestGetReadableChannelIDsStillRequiresTheViewBit(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "readowner2")
	member := ts.CreateTestUser(t, "readmember2")
	serverID := ts.CreateTestServer(t, owner.ID, "Readable Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chOpen := ts.CreateTestChannel(t, serverID, "read-open-2")
	chHidden := ts.CreateTestChannel(t, serverID, "read-hidden-2")
	chVoiceHidden := ts.CreateVoiceChannel(t, serverID, "read-voice-hidden-2")

	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, chHidden, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, chVoiceHidden, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	readable, err := resolver.GetReadableChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, readable, chOpen)
	assert.NotContains(t, readable, chHidden, "a view denial still excludes the channel")
	assert.NotContains(t, readable, chVoiceHidden, "the voice view bit is checked for voice channels")

	allReadable, err := resolver.GetAllReadableChannelIDs(ctx, member.ID)
	require.NoError(t, err)
	assert.Contains(t, allReadable, chOpen)
	assert.NotContains(t, allReadable, chHidden)
	assert.NotContains(t, allReadable, chVoiceHidden)
}

// The owner and administrator fast paths bypass the history requirement exactly as
// they bypass the view bits, because the per-request check they stand in for does
// too: resolveServerPermissions grants an owner OwnerPermissions before SBAC is
// consulted, and applyChannelOverrides returns early for an administrator.
func TestGetReadableChannelIDsOwnerAndAdminIgnoreHistoryDeny(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "readowner3")
	admin := ts.CreateTestUser(t, "readadmin3")
	serverID := ts.CreateTestServer(t, owner.ID, "Readable Server 3")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin-read3", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	chSealed := ts.CreateTestChannel(t, serverID, "read-sealed-3")
	ts.CreateChannelOverride(t, chSealed, "user", owner.ID, 0, int64(rbac.PermReadMessageHistory))
	ts.CreateChannelOverride(t, chSealed, "user", admin.ID, 0, int64(rbac.PermReadMessageHistory))

	ownerReadable, err := resolver.GetReadableChannelIDs(ctx, serverID, owner.ID)
	require.NoError(t, err)
	assert.Contains(t, ownerReadable, chSealed, "SBAC cannot restrict the server owner")

	adminReadable, err := resolver.GetReadableChannelIDs(ctx, serverID, admin.ID)
	require.NoError(t, err)
	assert.Contains(t, adminReadable, chSealed, "SBAC cannot restrict an administrator")

	ownerAll, err := resolver.GetAllReadableChannelIDs(ctx, owner.ID)
	require.NoError(t, err)
	assert.Contains(t, ownerAll, chSealed, "the cross-server owner fast path agrees")

	adminAll, err := resolver.GetAllReadableChannelIDs(ctx, admin.ID)
	require.NoError(t, err)
	assert.Contains(t, adminAll, chSealed, "the cross-server administrator fast path agrees")
}

// A non-member reads nothing, and the empty result is a non-nil slice so callers
// can hand it straight to pq.Array.
func TestGetAllReadableChannelIDsEmptyForNonMember(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "readowner4")
	outsider := ts.CreateTestUser(t, "readoutsider4")
	serverID := ts.CreateTestServer(t, owner.ID, "Readable Server 4")
	ts.CreateTestChannel(t, serverID, "read-lonely-4")

	ids, err := resolver.GetAllReadableChannelIDs(ctx, outsider.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{}, ids)

	perServer, err := resolver.GetReadableChannelIDs(ctx, serverID, outsider.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{}, perServer)
}
