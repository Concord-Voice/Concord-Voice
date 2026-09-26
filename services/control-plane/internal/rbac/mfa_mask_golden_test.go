package rbac_test

import (
	"context"
	"math/bits"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// T0 of #3453 ([internal]plans/2026-09-25-3453-mfa-enforcement-core.md).
// This file captures a GOLDEN permission table from the UNMODIFIED resolver at
// the current HEAD, before any MFA-enforcement mask exists. With enforcement
// OFF (there is no mask yet, so every read below is definitionally "off"),
// every entry point must return exactly the values hard-coded below. A later
// task (R9) adds bit 24 (PermMentionEveryone) to OwnerPermissions -- do not
// fold that in here; this table is what the code returns TODAY.
//
// Statement-count probe: SKIPPED, marked UNVERIFIED. internal/testhelpers and
// internal/rbac's own tests were searched for an existing SQL statement
// counter; the only one found is channelViewerCountingConnection in
// resolver_visibility_test.go, a fully-mocked driver.Conn that fabricates its
// own result rows for exactly one query shape (FilterVisibleUserIDsForChannel's
// unnest-array query). It is not a general pass-through counter: every entry
// point exercised here issues a different mix of real queries against real
// Postgres (membership check, owner check, role BIT_OR, batched SBAC override
// reads), and building a driver.Conn/Stmt/Rows/Tx proxy that transparently
// forwards to the real lib/pq driver while counting is a correctness-risky
// undertaking of its own -- exactly the kind of thing this task says not to
// attempt against production code or with weak confidence in the harness
// itself. Per the task's own escape hatch, this is skipped rather than built.

// goldenOwnerAt3421022 is OwnerPermissions exactly as recorded at 3421022,
// before any #3453 change. It stays a literal so the one intended delta below
// is visible as a delta rather than folded into a re-captured baseline.
const goldenOwnerAt3421022 = rbac.Permission(0x3effffff)

// rs9OwnerMentionEveryone is the only intended change to any value in this
// table when enforcement is off (spec I2, C-2): R9 gives the owner
// PermMentionEveryone, which OwnerPermissions omitted, so owners could not
// use @all/@here. TestMFAMaskGolden_RS9DeltaIsOneNewBit pins it.
const rs9OwnerMentionEveryone = rbac.PermMentionEveryone

// goldenPersonaOrder is the fixed persona iteration order asserted below.
var goldenPersonaOrder = []string{"owner", "admin", "adminDeny", "moderator", "member", "memberAllow", "memberDeny"}

// goldenHasPermChecks is the fixed HasPermission bit list from the T0 task.
var goldenHasPermChecks = []struct {
	name string
	perm rbac.Permission
}{
	{"PermManageServer", rbac.PermManageServer},
	{"PermManageRoles", rbac.PermManageRoles},
	{"PermManageChannels", rbac.PermManageChannels},
	{"PermManageCryptoRotation", rbac.PermManageCryptoRotation},
	{"PermKick", rbac.PermKick},
	{"PermBan", rbac.PermBan},
	{"PermManageAllMessages", rbac.PermManageAllMessages},
	{"PermManageDevResources", rbac.PermManageDevResources},
	{"PermViewTextChannels", rbac.PermViewTextChannels},
	{"PermSendMessages", rbac.PermSendMessages},
	{"PermJoinVoice", rbac.PermJoinVoice},
	{"PermSpeak", rbac.PermSpeak},
	{"PermMentionEveryone", rbac.PermMentionEveryone},
}

// goldenFixture names the one server, one channel and seven personas T0 asks
// for. It is deliberately just IDs, not a *rbac.Resolver or *testhelpers.TestServer,
// so a later task (off / on+enrolled / on+unenrolled) can build one fixture and
// observe it under several configurations without rebuilding the data.
type goldenFixture struct {
	serverID  string
	channelID string
	personas  map[string]string // persona name -> userID
}

// buildGoldenFixture builds the one-server/one-channel/seven-persona fixture:
//   - owner: the server owner.
//   - admin: holds a role whose permissions include PermAdministrator.
//   - adminDeny: an Administrator with a channel USER-override DENY of
//     PermManageAllMessages, proving the admin bypass ignores overrides.
//   - moderator: a role with ModeratorPermissions.
//   - member: only the default @all role (BasePermissions).
//   - memberAllow: a member whose role has a channel ROLE-override ALLOW of
//     PermManageAllMessages.
//   - memberDeny: a member with a channel USER-override DENY of
//     PermSendMessages.
func buildGoldenFixture(t *testing.T, ts *testhelpers.TestServer) goldenFixture {
	t.Helper()

	owner := ts.CreateTestUser(t, "goldowner")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA Mask Golden")
	channelID := ts.CreateTestChannel(t, serverID, "golden-channel")

	admin := ts.CreateTestUser(t, "goldadmin")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	adminRole := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRole)

	adminDeny := ts.CreateTestUser(t, "goldadmindeny")
	ts.AddMemberToServer(t, serverID, adminDeny.ID, "member")
	ts.AssignRoleToUser(t, serverID, adminDeny.ID, adminRole)
	ts.CreateChannelOverride(t, channelID, "user", adminDeny.ID, 0, int64(rbac.PermManageAllMessages))

	moderator := ts.CreateTestUser(t, "goldmod")
	ts.AddMemberToServer(t, serverID, moderator.ID, "member")
	modRole := ts.CreateTestRole(t, serverID, "Moderator", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, moderator.ID, modRole)

	member := ts.CreateTestUser(t, "goldmember")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	memberAllow := ts.CreateTestUser(t, "goldmallow")
	ts.AddMemberToServer(t, serverID, memberAllow.ID, "member")
	allowRole := ts.CreateTestRole(t, serverID, "AllowRole", 1, 0)
	ts.AssignRoleToUser(t, serverID, memberAllow.ID, allowRole)
	ts.CreateChannelOverride(t, channelID, "role", allowRole, int64(rbac.PermManageAllMessages), 0)

	memberDeny := ts.CreateTestUser(t, "goldmdeny")
	ts.AddMemberToServer(t, serverID, memberDeny.ID, "member")
	ts.CreateChannelOverride(t, channelID, "user", memberDeny.ID, 0, int64(rbac.PermSendMessages))

	return goldenFixture{
		serverID:  serverID,
		channelID: channelID,
		personas: map[string]string{
			"owner":       owner.ID,
			"admin":       admin.ID,
			"adminDeny":   adminDeny.ID,
			"moderator":   moderator.ID,
			"member":      member.ID,
			"memberAllow": memberAllow.ID,
			"memberDeny":  memberDeny.ID,
		},
	}
}

// observed is one persona's recorded values across every entry point.
type observed struct {
	perms   map[string]rbac.Permission // entry point key -> value
	hasPerm map[string]bool            // "<scope>/<PermName>" -> value
}

// observeAll calls every entry point named in the T0 task for one persona and
// records what it returns. For every entry point backed by the Redis cache
// (GetEffectivePermissions, ResolveEffectivePermissionsFresh, HasPermission)
// it flushes the cache first and then calls twice, asserting the cold
// (compute) and warm (cache-hit) reads agree -- satisfying "record through
// both a cold and warm read" without hand-duplicating every call site.
func observeAll(t *testing.T, r *rbac.Resolver, ts *testhelpers.TestServer, f goldenFixture, userID string) observed {
	t.Helper()
	ctx := context.Background()
	obs := observed{
		perms:   make(map[string]rbac.Permission),
		hasPerm: make(map[string]bool),
	}

	record := func(key string, call func() (rbac.Permission, error)) {
		invalidatePermCache(t, ts, f.serverID, userID)
		cold, err := call()
		require.NoError(t, err, "%s: cold read", key)
		warm, err := call()
		require.NoError(t, err, "%s: warm read", key)
		assert.Equal(t, cold, warm, "%s: cold and warm reads must agree", key)
		obs.perms[key] = cold
	}

	record("GetEffectivePermissions/server", func() (rbac.Permission, error) {
		return r.GetEffectivePermissions(ctx, f.serverID, userID, "")
	})
	record("GetEffectivePermissions/channel", func() (rbac.Permission, error) {
		return r.GetEffectivePermissions(ctx, f.serverID, userID, f.channelID)
	})
	record("ResolveEffectivePermissionsFresh/server", func() (rbac.Permission, error) {
		return r.ResolveEffectivePermissionsFresh(ctx, f.serverID, userID, "")
	})
	record("ResolveEffectivePermissionsFresh/channel", func() (rbac.Permission, error) {
		return r.ResolveEffectivePermissionsFresh(ctx, f.serverID, userID, f.channelID)
	})
	record("ResolveEffectivePermissionsUncached/server", func() (rbac.Permission, error) {
		return r.ResolveEffectivePermissionsUncached(ctx, f.serverID, userID, "")
	})
	record("ResolveEffectivePermissionsUncached/channel", func() (rbac.Permission, error) {
		return r.ResolveEffectivePermissionsUncached(ctx, f.serverID, userID, f.channelID)
	})

	invalidatePermCache(t, ts, f.serverID, userID)

	txServer, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	v, err := r.ResolveServerPermissionsTx(ctx, txServer, f.serverID, userID)
	require.NoError(t, err, "ResolveServerPermissionsTx")
	require.NoError(t, txServer.Rollback())
	obs.perms["ResolveServerPermissionsTx"] = v

	txChannel, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	v, err = r.ResolveChannelPermissionsTx(ctx, txChannel, f.serverID, userID, f.channelID)
	require.NoError(t, err, "ResolveChannelPermissionsTx")
	require.NoError(t, txChannel.Rollback())
	obs.perms["ResolveChannelPermissionsTx"] = v

	byChannel, err := r.ResolveEffectivePermissionsForChannelsFresh(ctx, f.serverID, userID, []string{f.channelID})
	require.NoError(t, err, "ResolveEffectivePermissionsForChannelsFresh")
	obs.perms["ResolveEffectivePermissionsForChannelsFresh"] = byChannel[f.channelID]

	invalidatePermCache(t, ts, f.serverID, userID)
	for _, p := range goldenHasPermChecks {
		hasServer, err := r.HasPermission(ctx, f.serverID, userID, "", p.perm)
		require.NoError(t, err, "HasPermission server/%s", p.name)
		hasChannel, err := r.HasPermission(ctx, f.serverID, userID, f.channelID, p.perm)
		require.NoError(t, err, "HasPermission channel/%s", p.name)
		obs.hasPerm["server/"+p.name] = hasServer
		obs.hasPerm["channel/"+p.name] = hasChannel
	}

	return obs
}

// goldenPerms is the T0 golden literal, captured from the unmodified resolver
// at HEAD 56254e4 (migration 000157, then numbered 000155, added; the enforce_mfa_dangerous_actions
// column exists but nothing reads it yet, so every read below is
// unconditionally the "off" configuration). Keyed persona -> entry point.
var goldenPerms = map[string]map[string]rbac.Permission{
	"owner": { // OwnerPermissions at every entry point -- the owner
		// short-circuit in resolveServerPermissions returns it before SBAC is
		// ever consulted, so server and channel scope agree. The value is the
		// baseline recorded at 3421022 plus the one named RS9 delta.
		"GetEffectivePermissions/server":              goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"GetEffectivePermissions/channel":             goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveEffectivePermissionsFresh/server":     goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveEffectivePermissionsFresh/channel":    goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveEffectivePermissionsUncached/server":  goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveEffectivePermissionsUncached/channel": goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveServerPermissionsTx":                  goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveChannelPermissionsTx":                 goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
		"ResolveEffectivePermissionsForChannelsFresh": goldenOwnerAt3421022 | rs9OwnerMentionEveryone,
	},
	"admin": { // BasePermissions|PermAdministrator (0x400000001ce3be00): the
		// RAW role bitfield (default @all role OR'd with the Admin role,
		// which holds only bit 62). GetEffectivePermissions/ResolveXxx never
		// expand PermAdministrator into "every bit" -- only Permission.Has
		// applies that bypass at check time -- so this is the literal OR of
		// the two roles' permissions columns, not AdminPermissions.
		"GetEffectivePermissions/server":              rbac.Permission(0x400000001ce3be00),
		"GetEffectivePermissions/channel":             rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsFresh/server":     rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsFresh/channel":    rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsUncached/server":  rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsUncached/channel": rbac.Permission(0x400000001ce3be00),
		"ResolveServerPermissionsTx":                  rbac.Permission(0x400000001ce3be00),
		"ResolveChannelPermissionsTx":                 rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsForChannelsFresh": rbac.Permission(0x400000001ce3be00),
	},
	"adminDeny": { // Identical to "admin" at every entry point, including
		// channel scope, despite the channel USER-DENY override on
		// PermManageAllMessages: applyChannelOverrides short-circuits on
		// basePerms.Has(PermAdministrator) and returns basePerms unchanged,
		// before the override rows are even read. This is the admin-bypass
		// proof the fixture exists to pin.
		"GetEffectivePermissions/server":              rbac.Permission(0x400000001ce3be00),
		"GetEffectivePermissions/channel":             rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsFresh/server":     rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsFresh/channel":    rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsUncached/server":  rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsUncached/channel": rbac.Permission(0x400000001ce3be00),
		"ResolveServerPermissionsTx":                  rbac.Permission(0x400000001ce3be00),
		"ResolveChannelPermissionsTx":                 rbac.Permission(0x400000001ce3be00),
		"ResolveEffectivePermissionsForChannelsFresh": rbac.Permission(0x400000001ce3be00),
	},
	"moderator": { // ModeratorPermissions (0x3cfffe80), same at both scopes:
		// no channel override targets this persona or its role.
		"GetEffectivePermissions/server":              rbac.Permission(0x3cfffe80),
		"GetEffectivePermissions/channel":             rbac.Permission(0x3cfffe80),
		"ResolveEffectivePermissionsFresh/server":     rbac.Permission(0x3cfffe80),
		"ResolveEffectivePermissionsFresh/channel":    rbac.Permission(0x3cfffe80),
		"ResolveEffectivePermissionsUncached/server":  rbac.Permission(0x3cfffe80),
		"ResolveEffectivePermissionsUncached/channel": rbac.Permission(0x3cfffe80),
		"ResolveServerPermissionsTx":                  rbac.Permission(0x3cfffe80),
		"ResolveChannelPermissionsTx":                 rbac.Permission(0x3cfffe80),
		"ResolveEffectivePermissionsForChannelsFresh": rbac.Permission(0x3cfffe80),
	},
	"member": { // BasePermissions (0x1ce3be00) at every entry point -- only
		// the default @all role, no override touches this persona.
		"GetEffectivePermissions/server":              rbac.Permission(0x1ce3be00),
		"GetEffectivePermissions/channel":             rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsFresh/server":     rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsFresh/channel":    rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsUncached/server":  rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsUncached/channel": rbac.Permission(0x1ce3be00),
		"ResolveServerPermissionsTx":                  rbac.Permission(0x1ce3be00),
		"ResolveChannelPermissionsTx":                 rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsForChannelsFresh": rbac.Permission(0x1ce3be00),
	},
	"memberAllow": { // Server scope stays BasePermissions (0x1ce3be00); every
		// channel-scoped entry point picks up the role-ALLOW override and
		// reads BasePermissions|PermManageAllMessages (0x1ce3fe00).
		"GetEffectivePermissions/server":              rbac.Permission(0x1ce3be00),
		"GetEffectivePermissions/channel":             rbac.Permission(0x1ce3fe00),
		"ResolveEffectivePermissionsFresh/server":     rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsFresh/channel":    rbac.Permission(0x1ce3fe00),
		"ResolveEffectivePermissionsUncached/server":  rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsUncached/channel": rbac.Permission(0x1ce3fe00),
		"ResolveServerPermissionsTx":                  rbac.Permission(0x1ce3be00),
		"ResolveChannelPermissionsTx":                 rbac.Permission(0x1ce3fe00),
		"ResolveEffectivePermissionsForChannelsFresh": rbac.Permission(0x1ce3fe00),
	},
	"memberDeny": { // Server scope stays BasePermissions (0x1ce3be00); every
		// channel-scoped entry point picks up the user-DENY override and
		// reads BasePermissions&^PermSendMessages (0x1ce3b600).
		"GetEffectivePermissions/server":              rbac.Permission(0x1ce3be00),
		"GetEffectivePermissions/channel":             rbac.Permission(0x1ce3b600),
		"ResolveEffectivePermissionsFresh/server":     rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsFresh/channel":    rbac.Permission(0x1ce3b600),
		"ResolveEffectivePermissionsUncached/server":  rbac.Permission(0x1ce3be00),
		"ResolveEffectivePermissionsUncached/channel": rbac.Permission(0x1ce3b600),
		"ResolveServerPermissionsTx":                  rbac.Permission(0x1ce3be00),
		"ResolveChannelPermissionsTx":                 rbac.Permission(0x1ce3b600),
		"ResolveEffectivePermissionsForChannelsFresh": rbac.Permission(0x1ce3b600),
	},
}

// goldenHasPerm is the T0 golden HasPermission table, keyed persona ->
// "<scope>/<PermName>". true/false mirrors goldenPerms bit-for-bit through
// Permission.Has (including the PermAdministrator bypass for admin/adminDeny).
var goldenHasPerm = map[string]map[string]bool{
	"owner": {
		"server/PermManageServer": true, "channel/PermManageServer": true,
		"server/PermManageRoles": true, "channel/PermManageRoles": true,
		"server/PermManageChannels": true, "channel/PermManageChannels": true,
		"server/PermManageCryptoRotation": true, "channel/PermManageCryptoRotation": true,
		"server/PermKick": true, "channel/PermKick": true,
		"server/PermBan": true, "channel/PermBan": true,
		"server/PermManageAllMessages": true, "channel/PermManageAllMessages": true,
		"server/PermManageDevResources": true, "channel/PermManageDevResources": true,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": true,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		// RS9 (C-2 delta): the owner holds PermMentionEveryone. At 3421022 both were false.
		"server/PermMentionEveryone": true, "channel/PermMentionEveryone": true,
	},
	"admin": {
		// PermAdministrator makes Permission.Has report true for every bit
		// checked, regardless of whether AdminPermissions itself carries it.
		"server/PermManageServer": true, "channel/PermManageServer": true,
		"server/PermManageRoles": true, "channel/PermManageRoles": true,
		"server/PermManageChannels": true, "channel/PermManageChannels": true,
		"server/PermManageCryptoRotation": true, "channel/PermManageCryptoRotation": true,
		"server/PermKick": true, "channel/PermKick": true,
		"server/PermBan": true, "channel/PermBan": true,
		"server/PermManageAllMessages": true, "channel/PermManageAllMessages": true,
		"server/PermManageDevResources": true, "channel/PermManageDevResources": true,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": true,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		"server/PermMentionEveryone": true, "channel/PermMentionEveryone": true,
	},
	"adminDeny": { // Byte-identical to "admin": the channel user-deny override
		// is never consulted for an administrator.
		"server/PermManageServer": true, "channel/PermManageServer": true,
		"server/PermManageRoles": true, "channel/PermManageRoles": true,
		"server/PermManageChannels": true, "channel/PermManageChannels": true,
		"server/PermManageCryptoRotation": true, "channel/PermManageCryptoRotation": true,
		"server/PermKick": true, "channel/PermKick": true,
		"server/PermBan": true, "channel/PermBan": true,
		"server/PermManageAllMessages": true, "channel/PermManageAllMessages": true,
		"server/PermManageDevResources": true, "channel/PermManageDevResources": true,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": true,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		"server/PermMentionEveryone": true, "channel/PermMentionEveryone": true,
	},
	"moderator": { // ModeratorPermissions: gains Kick and ManageAllMessages
		// over base, nothing else in the checked list.
		"server/PermManageServer": false, "channel/PermManageServer": false,
		"server/PermManageRoles": false, "channel/PermManageRoles": false,
		"server/PermManageChannels": false, "channel/PermManageChannels": false,
		"server/PermManageCryptoRotation": false, "channel/PermManageCryptoRotation": false,
		"server/PermKick": true, "channel/PermKick": true,
		"server/PermBan": false, "channel/PermBan": false,
		"server/PermManageAllMessages": true, "channel/PermManageAllMessages": true,
		"server/PermManageDevResources": false, "channel/PermManageDevResources": false,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": true,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		"server/PermMentionEveryone": false, "channel/PermMentionEveryone": false,
	},
	"member": { // BasePermissions only.
		"server/PermManageServer": false, "channel/PermManageServer": false,
		"server/PermManageRoles": false, "channel/PermManageRoles": false,
		"server/PermManageChannels": false, "channel/PermManageChannels": false,
		"server/PermManageCryptoRotation": false, "channel/PermManageCryptoRotation": false,
		"server/PermKick": false, "channel/PermKick": false,
		"server/PermBan": false, "channel/PermBan": false,
		"server/PermManageAllMessages": false, "channel/PermManageAllMessages": false,
		"server/PermManageDevResources": false, "channel/PermManageDevResources": false,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": true,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		"server/PermMentionEveryone": false, "channel/PermMentionEveryone": false,
	},
	"memberAllow": { // Identical to "member" except channel/PermManageAllMessages,
		// granted by the role-ALLOW override.
		"server/PermManageServer": false, "channel/PermManageServer": false,
		"server/PermManageRoles": false, "channel/PermManageRoles": false,
		"server/PermManageChannels": false, "channel/PermManageChannels": false,
		"server/PermManageCryptoRotation": false, "channel/PermManageCryptoRotation": false,
		"server/PermKick": false, "channel/PermKick": false,
		"server/PermBan": false, "channel/PermBan": false,
		"server/PermManageAllMessages": false, "channel/PermManageAllMessages": true,
		"server/PermManageDevResources": false, "channel/PermManageDevResources": false,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": true,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		"server/PermMentionEveryone": false, "channel/PermMentionEveryone": false,
	},
	"memberDeny": { // Identical to "member" except channel/PermSendMessages,
		// withdrawn by the user-DENY override.
		"server/PermManageServer": false, "channel/PermManageServer": false,
		"server/PermManageRoles": false, "channel/PermManageRoles": false,
		"server/PermManageChannels": false, "channel/PermManageChannels": false,
		"server/PermManageCryptoRotation": false, "channel/PermManageCryptoRotation": false,
		"server/PermKick": false, "channel/PermKick": false,
		"server/PermBan": false, "channel/PermBan": false,
		"server/PermManageAllMessages": false, "channel/PermManageAllMessages": false,
		"server/PermManageDevResources": false, "channel/PermManageDevResources": false,
		"server/PermViewTextChannels": true, "channel/PermViewTextChannels": true,
		"server/PermSendMessages": true, "channel/PermSendMessages": false,
		"server/PermJoinVoice": true, "channel/PermJoinVoice": true,
		"server/PermSpeak": true, "channel/PermSpeak": true,
		"server/PermMentionEveryone": false, "channel/PermMentionEveryone": false,
	},
}

// TestMFAMaskGolden_Off asserts the golden table above against the live
// resolver, one subtest per persona. Every entry point is exercised for
// every persona, so a failure names both without needing to reconstruct
// which call produced it.
func TestMFAMaskGolden_Off(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	f := buildGoldenFixture(t, ts)

	for _, persona := range goldenPersonaOrder {
		persona := persona
		t.Run(persona, func(t *testing.T) {
			userID, ok := f.personas[persona]
			require.True(t, ok, "fixture is missing persona %s", persona)

			obs := observeAll(t, r, ts, f, userID)

			for entryPoint, want := range goldenPerms[persona] {
				got, ok := obs.perms[entryPoint]
				require.True(t, ok, "persona %s: observeAll did not record entry point %s", persona, entryPoint)
				assert.Equal(t, want, got,
					"persona %s, entry point %s: got %#x want %#x", persona, entryPoint, int64(got), int64(want))
			}

			for key, want := range goldenHasPerm[persona] {
				got, ok := obs.hasPerm[key]
				require.True(t, ok, "persona %s: observeAll did not record HasPermission %s", persona, key)
				assert.Equal(t, want, got, "persona %s, HasPermission %s", persona, key)
			}
		})
	}
}

// TestGoldenProbeDump is not part of the golden assertions and asserts
// nothing. It exists so a later task (adding the on+enrolled / on+unenrolled
// configurations) can re-run this exact fixture and harness against whatever
// the resolver returns at that point, without hand-deriving hex values a
// second time. Skipped unless CONCORD_GOLDEN_PROBE_DUMP is set.
func TestGoldenProbeDump(t *testing.T) {
	if os.Getenv("CONCORD_GOLDEN_PROBE_DUMP") == "" {
		t.Skip("set CONCORD_GOLDEN_PROBE_DUMP=1 to dump observed values for every persona/entry point")
	}
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	f := buildGoldenFixture(t, ts)

	for _, persona := range goldenPersonaOrder {
		obs := observeAll(t, r, ts, f, f.personas[persona])
		t.Logf("=== %s ===", persona)
		for _, entryPoint := range []string{
			"GetEffectivePermissions/server", "GetEffectivePermissions/channel",
			"ResolveEffectivePermissionsFresh/server", "ResolveEffectivePermissionsFresh/channel",
			"ResolveEffectivePermissionsUncached/server", "ResolveEffectivePermissionsUncached/channel",
			"ResolveServerPermissionsTx", "ResolveChannelPermissionsTx",
			"ResolveEffectivePermissionsForChannelsFresh",
		} {
			t.Logf("%s = 0x%x", entryPoint, int64(obs.perms[entryPoint]))
		}
		for _, p := range goldenHasPermChecks {
			t.Logf("HasPermission server/%s = %v  channel/%s = %v",
				p.name, obs.hasPerm["server/"+p.name], p.name, obs.hasPerm["channel/"+p.name])
		}
	}
}

// TestMFAMaskGolden_RS9DeltaIsOneNewBit keeps the owner rows honest: the
// RS9 delta must be exactly one bit, absent from the recorded baseline, and
// the resulting value must be what OwnerPermissions now is. Without it, a
// later edit could widen the delta and re-baseline the owner silently.
func TestMFAMaskGolden_RS9DeltaIsOneNewBit(t *testing.T) {
	require.Equal(t, rbac.Permission(0), goldenOwnerAt3421022&rs9OwnerMentionEveryone,
		"the RS9 delta must be a bit the 3421022 baseline did not have")
	require.Equal(t, 1, bits.OnesCount64(uint64(rs9OwnerMentionEveryone)),
		"the RS9 delta must be exactly one bit")
	require.Equal(t, rbac.OwnerPermissions, goldenOwnerAt3421022|rs9OwnerMentionEveryone,
		"OwnerPermissions must be the 3421022 baseline plus the RS9 delta, and nothing else")
}
