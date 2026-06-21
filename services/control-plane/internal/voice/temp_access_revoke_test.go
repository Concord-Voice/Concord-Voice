package voice_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pathTempAccess = "/temp-access"

// TestRevokeTempAccess_RevokesGrant: a MOVE_MEMBERS holder revoking a target's
// move-granted temp SBAC override → 200 {revoked:true}, the override is deleted, a
// key_revocations row is inserted (CSK rotated). The force-disconnect publish path
// is exercised via revokeTemporaryChannelAccess.
func TestRevokeTempAccess_RevokesGrant(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_grant_owner")
	mover := ts.CreateTestUser(t, "tar_grant_mover")
	target := ts.CreateTestUser(t, "tar_grant_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Grant")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-grant")
	seedTempGrant(t, ts, serverID, channelID, target.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID))

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["revoked"])
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, target.ID), "temp override deleted")
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID), "CSK rotated on revoke")
}

// TestRevokeTempAccess_PermanentGrantNoOp: when only a PERMANENT override exists
// for (user, channel), the revoke is a no-op → 200 {revoked:false}, the permanent
// override survives, and no CSK rotation happens.
func TestRevokeTempAccess_PermanentGrantNoOp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_perm_owner")
	target := ts.CreateTestUser(t, "tar_perm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Perm")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-perm")

	// Permanent (is_temporary defaults false) user override.
	ts.CreateChannelOverride(t, channelID, "user", target.ID,
		int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice), 0)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["revoked"], "no temp grant → no-op")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_permission_overrides WHERE channel_id=$1 AND target_id=$2)`,
		channelID, target.ID,
	).Scan(&exists))
	assert.True(t, exists, "permanent override must survive a temp-access revoke")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no rotation when nothing revoked")
}

// TestRevokeTempAccess_NoGrantNoOp: no override at all → 200 {revoked:false}.
func TestRevokeTempAccess_NoGrantNoOp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_none_owner")
	target := ts.CreateTestUser(t, "tar_none_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp None")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-none")

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["revoked"])
}

// TestRevokeTempAccess_NoPermission: a base member without MOVE_MEMBERS → 403.
func TestRevokeTempAccess_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_np_owner")
	actor := ts.CreateTestUser(t, "tar_np_actor")
	target := ts.CreateTestUser(t, "tar_np_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp NoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-np")
	seedTempGrant(t, ts, serverID, channelID, target.ID)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	// Override must be untouched on an unauthorized attempt.
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID), "denied revoke must not delete the grant")
}

// TestRevokeTempAccess_HierarchyBlocked: revoke RESPECTS hierarchy — a
// MOVE_MEMBERS holder cannot revoke a higher-ranked member's grant → 403.
func TestRevokeTempAccess_HierarchyBlocked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_hier_owner")
	mover := ts.CreateTestUser(t, "tar_hier_mover")
	target := ts.CreateTestUser(t, "tar_hier_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Hierarchy")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	higherRole := ts.CreateTestRole(t, serverID, "Senior", 10, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, target.ID, higherRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-hier")
	seedTempGrant(t, ts, serverID, channelID, target.ID)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "temp-access revoke enforces hierarchy")
}

// TestRevokeTempAccess_CrossServerIDOR: a moderator with MOVE_MEMBERS in server A
// cannot revoke a temp grant on a voice channel in server B by passing server B's
// channel_id on server A's path. authorizeVoiceMod only authorized the actor for
// server A (:id), so the body channel_id MUST be scoped to that server. Expect 400
// AND that server B's temp override is STILL PRESENT (not deleted) and NO
// key_revocations row was inserted for server B's channel (CSK not rotated).
// Regression lock for the cross-server IDOR G6 fix (#487).
func TestRevokeTempAccess_CrossServerIDOR(t *testing.T) {
	ts := setupTS(t)
	// Server A: actor holds MOVE_MEMBERS.
	ownerA := ts.CreateTestUser(t, "tar_idor_ownerA")
	mover := ts.CreateTestUser(t, "tar_idor_mover")
	target := ts.CreateTestUser(t, "tar_idor_target")
	serverA := ts.CreateTestServer(t, ownerA.ID, "RevokeTemp IDOR A")
	ts.AddMemberToServer(t, serverA, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverA, target.ID, roleMember)
	moverRole := ts.CreateTestRole(t, serverA, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverA, mover.ID, moverRole)

	// Server B: a DIFFERENT server with a temp grant for target on a voice channel.
	ownerB := ts.CreateTestUser(t, "tar_idor_ownerB")
	serverB := ts.CreateTestServer(t, ownerB.ID, "RevokeTemp IDOR B")
	ts.AddMemberToServer(t, serverB, target.ID, roleMember)
	channelB := ts.CreateVoiceChannel(t, serverB, "voice-idor-b")
	seedTempGrant(t, ts, serverB, channelB, target.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelB, target.ID), "precondition: server-B temp grant exists")

	// Actor (authorized for server A only) tries to revoke server B's grant by
	// passing channel B's id on server A's path.
	w := ts.DoRequest("DELETE", voiceEnforcePath(serverA, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelB}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code, "cross-server channel_id is rejected")

	// The cross-server IDOR guard must leave server B's state untouched.
	assert.True(t, tempOverrideExists(t, ts.DB, channelB, target.ID),
		"cross-server attempt must NOT delete server B's temp override")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelB),
		"cross-server attempt must NOT rotate server B's channel CSK")
}

// TestRevokeTempAccess_MissingBody: no channel_id in body → 400.
func TestRevokeTempAccess_MissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_mb_owner")
	target := ts.CreateTestUser(t, "tar_mb_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp MissingBody")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
