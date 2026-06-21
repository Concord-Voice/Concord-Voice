package voice_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tgActorSystem is the actor string passed by system-triggered temp-SBAC cleanup
// (presence/heartbeat/sweep). It is the EMPTY string — there is no human actor — so
// the resulting key_revocations row stores revoked_by as SQL NULL (the column is
// nullable, REFERENCES users(id) ON DELETE SET NULL). Passing a non-empty,
// non-existent UUID here would trip key_revocations_revoked_by_fkey.
const tgActorSystem = ""

// newTempGrantManager builds a tempGrantManager backed by the test DB/Redis/Hub.
func newTempGrantManager(t *testing.T, ts *testhelpers.TestServer) *voice.TestTempGrantManager {
	t.Helper()
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	return voice.NewTestTempGrantManager(ts.DB, log, ts.Hub, resolver, nil)
}

// tgOverride reads the (allow, deny, is_temporary, temporary_reason) of a user
// override row, plus whether the row exists.
func tgOverride(t *testing.T, db *sql.DB, channelID, userID string) (exists bool, allow, deny int64, isTemp bool, reason sql.NullString) {
	t.Helper()
	err := db.QueryRow(
		`SELECT allow, deny, is_temporary, temporary_reason
		 FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, userID,
	).Scan(&allow, &deny, &isTemp, &reason)
	if err == sql.ErrNoRows {
		return false, 0, 0, false, sql.NullString{}
	}
	require.NoError(t, err)
	return true, allow, deny, isTemp, reason
}

func tgSeedChannelKey(t *testing.T, db *sql.DB, channelID, userID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'wk', 1)`,
		channelID, userID,
	)
	require.NoError(t, err)
}

func tgSeedPendingKeyRequest(t *testing.T, db *sql.DB, channelID, userID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2)`,
		channelID, userID,
	)
	require.NoError(t, err)
}

func tgChannelKeyExists(t *testing.T, db *sql.DB, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_keys WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func tgPendingKeyRequestExists(t *testing.T, db *sql.DB, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func tgKeyRevocationCount(t *testing.T, db *sql.DB, channelID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, channelID).Scan(&n))
	return n
}

// tgLatestRevokedBy returns the revoked_by of the most recent key_revocations row
// for the channel (NullString.Valid == false means SQL NULL).
func tgLatestRevokedBy(t *testing.T, db *sql.DB, channelID string) sql.NullString {
	t.Helper()
	var revokedBy sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT revoked_by FROM key_revocations WHERE channel_id = $1 ORDER BY revoked_epoch DESC LIMIT 1`,
		channelID,
	).Scan(&revokedBy))
	return revokedBy
}

// --- Grant tests (#487 Scope C grant / T5) ---

func TestGrantTemporaryChannelAccess_InsertsTempOverride(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_grant_owner")
	mover := ts.CreateTestUser(t, "tg_grant_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Insert")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-grant")

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.NoError(t, err)

	exists, allow, deny, isTemp, reason := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists, "a temp override row should be inserted")
	assert.Equal(t, int64(voice.TempGrantAllow), allow, "allow mask must be exactly VIEW|CONNECT|SPEAK")
	assert.Equal(t, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), allow, "allow mask must be the exact three bits")
	assert.Equal(t, int64(0), deny, "deny must be 0")
	assert.True(t, isTemp, "is_temporary must be true")
	assert.True(t, reason.Valid)
	assert.Equal(t, "move_granted", reason.String)
}

func TestGrantTemporaryChannelAccess_AllowMaskExcludesManagementAndMessaging(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_mask_owner")
	mover := ts.CreateTestUser(t, "tg_mask_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Mask")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-mask")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))

	_, allow, _, _, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	// SEND_MESSAGES and every management/moderation bit must be absent.
	assert.Equal(t, int64(0), allow&int64(rbac.PermSendMessages), "must NOT grant SEND_MESSAGES")
	assert.Equal(t, int64(0), allow&int64(rbac.PermMoveMembers), "must NOT grant MOVE_MEMBERS")
	assert.Equal(t, int64(0), allow&int64(rbac.PermMuteMembers), "must NOT grant MUTE_MEMBERS")
	assert.Equal(t, int64(0), allow&int64(rbac.PermManageRoles), "must NOT grant MANAGE_ROLES")
	assert.Equal(t, int64(0), allow&int64(rbac.PermAdministrator), "must NOT grant ADMINISTRATOR")
}

func TestGrantTemporaryChannelAccess_DoesNotDowngradePermanent(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_perm_owner")
	mover := ts.CreateTestUser(t, "tg_perm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Permanent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-perm")

	// Pre-existing PERMANENT override (is_temporary defaults to false) with a wider mask.
	permAllow := int64(rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak | rbac.PermSendMessages)
	ts.CreateChannelOverride(t, channelID, "user", mover.ID, permAllow, 0)

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.NoError(t, err)

	exists, allow, _, isTemp, reason := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists)
	assert.False(t, isTemp, "permanent grant must NOT be flipped to temporary")
	assert.Equal(t, permAllow, allow, "permanent allow mask must be untouched (not downgraded)")
	assert.False(t, reason.Valid, "temporary_reason must remain NULL on the permanent row")
}

func TestGrantTemporaryChannelAccess_IdempotentOnExistingTemp(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_idem_owner")
	mover := ts.CreateTestUser(t, "tg_idem_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Idempotent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-idem")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, mover.ID,
	).Scan(&count))
	assert.Equal(t, 1, count, "repeat grant must remain a single row (idempotent)")

	_, allow, _, isTemp, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	assert.Equal(t, int64(voice.TempGrantAllow), allow)
	assert.True(t, isTemp)
}

// TestGrantTemporaryChannelAccess_InsertError verifies the INSERT-failure branch:
// granting against a channel_id that does not exist violates the
// channel_permission_overrides FK (channel_id REFERENCES channels(id)). The grant
// must surface a wrapped "temp grant insert" error and write no override row.
func TestGrantTemporaryChannelAccess_InsertError(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_inserr_owner")
	mover := ts.CreateTestUser(t, "tg_inserr_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant InsertErr")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)

	// A well-formed UUID that is NOT a real channel → FK violation on INSERT.
	orphanChannel := "33333333-3333-3333-3333-333333333333"
	err := mgr.Grant(context.Background(), serverID, orphanChannel, mover.ID)
	require.Error(t, err, "grant against a non-existent channel must fail on the FK")
	assert.Contains(t, err.Error(), "temp grant insert")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_id = $2`,
		orphanChannel, mover.ID,
	).Scan(&count))
	assert.Equal(t, 0, count, "no override row should persist when the INSERT fails")
}

// --- Revoke tests (#487 P1 / T6) ---

func TestRevokeTemporaryChannelAccess_DeletesTempAndPurges(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_rev_owner")
	mover := ts.CreateTestUser(t, "tg_rev_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Purge")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-rev")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)

	err := mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.NoError(t, err)

	exists, _, _, _, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	assert.False(t, exists, "temp override must be deleted")
	assert.False(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "channel_keys must be purged")
	assert.False(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID), "pending_key_requests must be purged")
	assert.Equal(t, 1, tgKeyRevocationCount(t, ts.DB, channelID), "CSK must be rotated (one key_revocations row)")

	// System-triggered revoke (actorID == "") must store revoked_by as SQL NULL,
	// not the literal empty string — otherwise the FK to users(id) is violated.
	revokedBy := tgLatestRevokedBy(t, ts.DB, channelID)
	assert.False(t, revokedBy.Valid, "actorless system revoke must store revoked_by as NULL")
}

func TestRevokeTemporaryChannelAccess_DeletesOnlyTemporary(t *testing.T) {
	// SECURITY-CRITICAL: revoke must NOT touch a permanent override for the same
	// (channel, user). With ONLY a permanent grant present, revoke is a total NO-OP.
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_revperm_owner")
	mover := ts.CreateTestUser(t, "tg_revperm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Permanent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-revperm")

	permAllow := int64(rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak)
	ts.CreateChannelOverride(t, channelID, "user", mover.ID, permAllow, 0)
	// Seed key material that must SURVIVE because no temp grant exists.
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)

	err := mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.NoError(t, err)

	exists, allow, _, isTemp, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists, "permanent override must NOT be deleted")
	assert.False(t, isTemp)
	assert.Equal(t, permAllow, allow)
	// No purge, no rotation when only a permanent grant exists.
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "channel_keys must NOT be purged on no-op")
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID), "pending_key_requests must NOT be purged on no-op")
	assert.Equal(t, 0, tgKeyRevocationCount(t, ts.DB, channelID), "no CSK rotation on a permanent-only no-op")
}

func TestRevokeTemporaryChannelAccess_NoGrantIsNoOp(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_revnone_owner")
	mover := ts.CreateTestUser(t, "tg_revnone_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke None")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-revnone")
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)

	err := mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.NoError(t, err)

	// No temp grant present → no purge, no rotation.
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "channel_keys must survive when there is no temp grant")
	assert.Equal(t, 0, tgKeyRevocationCount(t, ts.DB, channelID))
}

// TestRevokeTemporaryChannelAccess_PermanentSupersedesTemp verifies the
// "permanent grant supersedes" lifecycle row: if a temp grant was flipped to
// permanent (is_temporary cleared), the revoke's is_temporary guard finds nothing
// to delete → total no-op (no purge, no rotation). Correct by construction.
func TestRevokeTemporaryChannelAccess_PermanentSupersedesTemp(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_super_owner")
	mover := ts.CreateTestUser(t, "tg_super_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Supersede")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-super")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	// Simulate the temp grant being promoted to permanent.
	_, err := ts.DB.Exec(
		`UPDATE channel_permission_overrides SET is_temporary = false, temporary_reason = NULL
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, mover.ID)
	require.NoError(t, err)
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)

	require.NoError(t, mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem))

	exists, _, _, isTemp, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists, "promoted permanent override must survive")
	assert.False(t, isTemp)
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "no purge on promoted-permanent no-op")
	assert.Equal(t, 0, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestHasTemporaryGrant(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_has_owner")
	mover := ts.CreateTestUser(t, "tg_has_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Has")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-has")

	has, err := mgr.HasTemporaryGrant(context.Background(), channelID, mover.ID)
	require.NoError(t, err)
	assert.False(t, has, "no grant yet")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	has, err = mgr.HasTemporaryGrant(context.Background(), channelID, mover.ID)
	require.NoError(t, err)
	assert.True(t, has, "temp grant should be detected")

	// A permanent override is not a temporary grant.
	other := ts.CreateTestUser(t, "tg_has_perm")
	ts.AddMemberToServer(t, serverID, other.ID, roleMember)
	ts.CreateChannelOverride(t, channelID, "user", other.ID, int64(rbac.PermViewVoiceChannels), 0)
	has, err = mgr.HasTemporaryGrant(context.Background(), channelID, other.ID)
	require.NoError(t, err)
	assert.False(t, has, "permanent override must NOT count as a temporary grant")
}

// TestRevokeThenVisible verifies the integration with GetVisibleChannelIDs (T7
// premise): a temp-granted hidden voice channel becomes visible, and after revoke
// it disappears again.
func TestGrantMakesHiddenVoiceChannelVisible(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)

	owner := ts.CreateTestUser(t, "tg_vis_owner")
	mover := ts.CreateTestUser(t, "tg_vis_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Visible")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)

	// Hidden voice channel: deny VIEW_VOICE for the @all role so the member can't see it.
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hidden")
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	visible, err := resolver.GetVisibleChannelIDs(context.Background(), serverID, mover.ID)
	require.NoError(t, err)
	assert.NotContains(t, visible, channelID, "channel should be hidden before grant")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	visible, err = resolver.GetVisibleChannelIDs(context.Background(), serverID, mover.ID)
	require.NoError(t, err)
	assert.Contains(t, visible, channelID, "temp grant (user-allow VIEW_VOICE) must surface the hidden channel")

	require.NoError(t, mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem))
	visible, err = resolver.GetVisibleChannelIDs(context.Background(), serverID, mover.ID)
	require.NoError(t, err)
	assert.NotContains(t, visible, channelID, "channel should be hidden again after revoke")
}
