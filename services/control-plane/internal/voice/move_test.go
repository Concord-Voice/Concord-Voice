package voice_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pathMove = "/move"

// joinVoice inserts a voice_participants row so the target counts as in-voice.
func joinVoice(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

// hideVoiceChannel denies VIEW_VOICE for the @all role so a plain member cannot see
// (and thus cannot join) the channel — the precondition for a temp grant.
func hideVoiceChannel(t *testing.T, ts *testhelpers.TestServer, serverID, channelID string) {
	t.Helper()
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
}

func moveTempOverrideExists(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_permission_overrides
		   WHERE channel_id=$1 AND target_type='user' AND target_id=$2 AND is_temporary=true)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func auditMoveCount(t *testing.T, ts *testhelpers.TestServer, serverID, targetID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE server_id=$1 AND target_id=$2 AND action='voice_member_moved'`,
		serverID, targetID,
	).Scan(&n))
	return n
}

// TestServerMove_SelfMove: a user relocating themselves needs no permission, no
// hierarchy, and gets no temp grant.
func TestServerMove_SelfMove(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_self_owner")
	mover := ts.CreateTestUser(t, "mv_self_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Self")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-self-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-self-to")
	joinVoice(t, ts, from, mover.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, mover.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, moveTempOverrideExists(t, ts, to, mover.ID), "self-move must not insert a temp grant")
}

// TestServerMove_WithPermission_HiddenChannel_GrantsTemp: a MOVE_MEMBERS holder
// moving a lower-ranked member into a hidden channel → 200 + temp grant inserted.
func TestServerMove_WithPermission_HiddenChannel_GrantsTemp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_grant_owner")
	mover := ts.CreateTestUser(t, "mv_grant_mover")
	target := ts.CreateTestUser(t, "mv_grant_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Grant")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Mover (pos 5) has MOVE_MEMBERS; target stays base member (pos 0).
	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	from := ts.CreateVoiceChannel(t, serverID, "voice-grant-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-grant-to")
	hideVoiceChannel(t, ts, serverID, to) // target cannot see/join `to`
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, moveTempOverrideExists(t, ts, to, target.ID), "temp grant inserted when target lacked access")
	assert.Equal(t, 0, auditMoveCount(t, ts, serverID, target.ID), "no audit for a downward move")
}

// TestServerMove_HierarchyBypass_AuditsCrossing: a MOVE_MEMBERS holder moving a
// HIGHER-ranked member → 200 (no hierarchy block) AND an audit_log entry.
func TestServerMove_HierarchyBypass_AuditsCrossing(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_hier_owner")
	mover := ts.CreateTestUser(t, "mv_hier_mover")
	target := ts.CreateTestUser(t, "mv_hier_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Hierarchy")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Mover (pos 5) has MOVE_MEMBERS; target (pos 10) OUTRANKS the mover.
	moverRole := ts.CreateTestRole(t, serverID, "Mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	higherRole := ts.CreateTestRole(t, serverID, "Senior", 10, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, target.ID, higherRole)

	from := ts.CreateVoiceChannel(t, serverID, "voice-hier-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-hier-to")
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "MOVE_MEMBERS bypasses hierarchy (ADR-0023)")
	assert.Equal(t, 1, auditMoveCount(t, ts, serverID, target.ID), "hierarchy-crossing move must be audited")
}

// TestServerMove_NoPermission: a base member without MOVE_MEMBERS → 403.
func TestServerMove_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_np_owner")
	mover := ts.CreateTestUser(t, "mv_np_mover")
	target := ts.CreateTestUser(t, "mv_np_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move NoPerm")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-np-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-np-to")
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestServerMove_CrossServerTarget: target_channel_id in another server → 400.
func TestServerMove_CrossServerTarget(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_cross_owner")
	target := ts.CreateTestUser(t, "mv_cross_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Cross A")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-cross-from")
	joinVoice(t, ts, from, target.ID)

	// A voice channel in a DIFFERENT server.
	otherServer := ts.CreateTestServer(t, owner.ID, "Move Cross B")
	foreignChannel := ts.CreateVoiceChannel(t, otherServer, "voice-cross-foreign")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": foreignChannel}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_NonVoiceTarget: target is a text channel → 400.
func TestServerMove_NonVoiceTarget(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_text_owner")
	target := ts.CreateTestUser(t, "mv_text_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Text")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-text-from")
	textChannel := ts.CreateTestChannel(t, serverID, "text-target")
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": textChannel}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_TargetNotInVoice: target is not in any voice channel → 409.
func TestServerMove_TargetNotInVoice(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_niv_owner")
	target := ts.CreateTestUser(t, "mv_niv_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move NotInVoice")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	to := ts.CreateVoiceChannel(t, serverID, "voice-niv-to")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestServerMove_AlreadyInTarget: from == to → 400.
func TestServerMove_AlreadyInTarget(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_same_owner")
	target := ts.CreateTestUser(t, "mv_same_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Same")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	to := ts.CreateVoiceChannel(t, serverID, "voice-same")
	joinVoice(t, ts, to, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_MissingBody: no target_channel_id → 400.
func TestServerMove_MissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_mb_owner")
	target := ts.CreateTestUser(t, "mv_mb_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move MissingBody")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_InvalidServerID: a non-UUID :id path param → 400 before any
// permission/DB work (parseMoveRequest guard).
func TestServerMove_InvalidServerID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_badsrv_owner")
	target := ts.CreateTestUser(t, "mv_badsrv_target")

	w := ts.DoRequest("POST", voiceEnforcePath("not-a-uuid", target.ID, pathMove),
		map[string]interface{}{"target_channel_id": target.ID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_InvalidUserID: a non-UUID :userId path param → 400 before any
// permission/DB work (parseMoveRequest guard).
func TestServerMove_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_baduser_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Move BadUser")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, "not-a-uuid", pathMove),
		map[string]interface{}{"target_channel_id": serverID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_Unauthenticated: no token → 401.
func TestServerMove_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_unauth_owner")
	target := ts.CreateTestUser(t, "mv_unauth_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Unauth")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	to := ts.CreateVoiceChannel(t, serverID, "voice-unauth-move")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestServerMove_SelfMove_NoPermissionNeeded_HiddenStays: self-move into a hidden
// channel does NOT grant temp access (self-move skips the grant entirely). The
// client AuthorizeJoin will then legitimately reject if they truly can't join —
// but the move endpoint itself must not fabricate access for a self-move.
func TestServerMove_SelfMove_NoTempGrantEvenIfHidden(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_selfhide_owner")
	mover := ts.CreateTestUser(t, "mv_selfhide_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Move SelfHidden")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-selfhide-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-selfhide-to")
	hideVoiceChannel(t, ts, serverID, to)
	joinVoice(t, ts, from, mover.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, mover.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, moveTempOverrideExists(t, ts, to, mover.ID), "self-move never grants temp access")
}
