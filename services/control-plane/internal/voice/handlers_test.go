package voice_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathChannelsPrefix       = "/api/v1/channels/"
	roleMember               = "member"
	pathVoiceJoin            = "/voice/join"
	pathVoiceAuthorizeAction = "/voice/authorize-action"
	pathVoiceParticipants    = "/voice/participants"
	keyAction                = "action"
	keyTargetUserID          = "target_user_id"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- GetParticipants Tests ---

func TestGetParticipants(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "vpowner")
		serverID := ts.CreateTestServer(t, owner.ID, "Voice Test Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-general")

		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		participants := body["participants"].([]interface{})
		assert.Len(t, participants, 0, "no participants initially")
	})

	t.Run("NotMember", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "vpowner2")
		outsider := ts.CreateTestUser(t, "vpoutsider")
		serverID := ts.CreateTestServer(t, owner.ID, "Voice NotMember Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-private")

		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("InvalidChannel", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "vpinvalid")

		w := ts.DoRequest("GET", "/api/v1/channels/not-a-uuid/voice/participants", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// --- AuthorizeJoin Tests ---

func TestAuthorizeJoin(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "vjowner")
		member := ts.CreateTestUser(t, "vjmember")
		serverID := ts.CreateTestServer(t, owner.ID, "VoiceJoin Test Server")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-join")

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, true, body["allowed"])
	})

	t.Run("NotMember", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "vjowner2")
		outsider := ts.CreateTestUser(t, "vjoutsider")
		serverID := ts.CreateTestServer(t, owner.ID, "VoiceJoin NonMember")
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-restricted")

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("NoJoinVoicePermission", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "vjowner3")
		member := ts.CreateTestUser(t, "vjnoperm")
		serverID := ts.CreateTestServer(t, owner.ID, "VoiceJoin NoPerm")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-noperm")

		// Get @all role and create channel deny override for JoinVoice
		var allRoleID string
		err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
		require.NoError(t, err)
		ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermJoinVoice))

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// --- AuthorizeVoiceAction Tests (table-driven to reduce duplication) ---

func TestAuthorizeVoiceAction_ValidActions(t *testing.T) {
	actions := []struct {
		action     string
		expectCode int
	}{
		{"mute", http.StatusOK},
		{"deafen", http.StatusOK},
		{"move", http.StatusOK},
		{"explode", http.StatusBadRequest},
	}

	for _, tc := range actions {
		t.Run(tc.action, func(t *testing.T) {
			ts := setupTS(t)
			owner := ts.CreateTestUser(t, "va_"+tc.action+"_owner")
			target := ts.CreateTestUser(t, "va_"+tc.action+"_target")
			serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction "+tc.action)
			ts.AddMemberToServer(t, serverID, target.ID, roleMember)
			channelID := ts.CreateVoiceChannel(t, serverID, "voice-"+tc.action)

			w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
				keyAction:       tc.action,
				keyTargetUserID: target.ID,
			}, testhelpers.AuthHeaders(owner.AccessToken))
			assert.Equal(t, tc.expectCode, w.Code)
		})
	}
}

func TestAuthorizeVoiceAction_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaowner5")
	actor := ts.CreateTestUser(t, "vaactor5")
	target := ts.CreateTestUser(t, "vatarget5")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction NoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-noperm")

	// Base member doesn't have MuteMembers
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_HierarchyViolation(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaowner6")
	moderator := ts.CreateTestUser(t, "vamod6")
	admin := ts.CreateTestUser(t, "vaadmin6")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction Hierarchy")
	ts.AddMemberToServer(t, serverID, moderator.ID, roleMember)
	ts.AddMemberToServer(t, serverID, admin.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hierarchy")

	// Mod (pos 5) has MuteMembers
	modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermMuteMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, modRoleID)

	// Admin (pos 10) outranks mod
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.AdminPermissions))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	// Mod tries to mute admin → hierarchy violation
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: admin.ID,
	}, testhelpers.AuthHeaders(moderator.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_WithCustomPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaowner7")
	actor := ts.CreateTestUser(t, "vaactor7")
	target := ts.CreateTestUser(t, "vatarget7")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction CustomPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-customperm")

	// Give actor exactly MuteMembers at a higher position than target
	muterRoleID := ts.CreateTestRole(t, serverID, "Muter", 5, int64(rbac.PermMuteMembers))
	ts.AssignRoleToUser(t, serverID, actor.ID, muterRoleID)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
}

// --- Server-Enforced Mute/Deafen Tests ---

const (
	pathServersPrefix = "/api/v1/servers/"
	pathVoice         = "/voice/"
	pathMute          = "/mute"
	pathDeafen        = "/deafen"
	pathUserMute      = "/user-mute"
	pathUserDeafen    = "/user-deafen"
)

// voiceEnforcePath builds /api/v1/servers/{serverID}/voice/{userID}{suffix}
func voiceEnforcePath(serverID, userID, suffix string) string {
	return pathServersPrefix + serverID + pathVoice + userID + suffix
}

func TestServerMute(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "smuteowner")
		target := ts.CreateTestUser(t, "smutetarget")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerMute OK")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		// Insert target as voice participant so NATS path is exercised
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-smute")
		_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
		require.NoError(t, err)

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMute), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify flag persisted on server_members
		var serverMuted bool
		err = ts.DB.QueryRow(`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&serverMuted)
		require.NoError(t, err)
		assert.True(t, serverMuted)
	})

	t.Run("InsufficientPermission", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "smutepermown")
		actor := ts.CreateTestUser(t, "smutepermact")
		target := ts.CreateTestUser(t, "smutepermtgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerMute NoPerm")
		ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		// actor has only base permissions (no MuteMembers)
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMute), nil, testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("HierarchyViolation", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "smutehierown")
		modA := ts.CreateTestUser(t, "smutehiermod")
		admin := ts.CreateTestUser(t, "smutehieradm")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerMute Hierarchy")
		ts.AddMemberToServer(t, serverID, modA.ID, roleMember)
		ts.AddMemberToServer(t, serverID, admin.ID, roleMember)

		// Mod (pos 5) has MuteMembers
		modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermMuteMembers))
		ts.AssignRoleToUser(t, serverID, modA.ID, modRoleID)

		// Admin (pos 10) outranks mod
		adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.AdminPermissions))
		ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

		// Mod tries to server-mute Admin → hierarchy violation
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, admin.ID, pathMute), nil, testhelpers.AuthHeaders(modA.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("TargetNotMember", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "smutenomown")
		outsider := ts.CreateTestUser(t, "smutenomout")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerMute NoMember")

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, outsider.ID, pathMute), nil, testhelpers.AuthHeaders(owner.AccessToken))
		// target not a member → RowsAffected == 0 → 404
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("InvalidUUID", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "smuteinvown")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerMute Invalid")

		w := ts.DoRequest("POST", pathServersPrefix+"not-a-uuid"+pathVoice+owner.ID+pathMute, nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)

		w = ts.DoRequest("POST", pathServersPrefix+serverID+pathVoice+"not-a-uuid"+pathMute, nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("PersistsFlag", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "smutepersown")
		target := ts.CreateTestUser(t, "smuteperstgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerMute Persist")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-persist")

		// Insert voice participant
		_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
		require.NoError(t, err)

		// Server-mute the target
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMute), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Remove the voice_participants row (simulating leave)
		_, err = ts.DB.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, channelID, target.ID)
		require.NoError(t, err)

		// server_members flag should still be true
		var serverMuted bool
		err = ts.DB.QueryRow(`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&serverMuted)
		require.NoError(t, err)
		assert.True(t, serverMuted, "server_muted should persist after leaving voice")
	})
}

func TestServerUnmute(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "sunmuteown")
		target := ts.CreateTestUser(t, "sunmutetgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerUnmute OK")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		// Pre-set server_muted = true
		_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`, serverID, target.ID)
		require.NoError(t, err)

		w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathMute), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var serverMuted bool
		err = ts.DB.QueryRow(`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&serverMuted)
		require.NoError(t, err)
		assert.False(t, serverMuted)
	})

	t.Run("WhenDeafened", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "sunmdeafown")
		target := ts.CreateTestUser(t, "sunmdeaftgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerUnmute Deaf")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		// Set both flags
		_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true, server_deafened = true WHERE server_id = $1 AND user_id = $2`, serverID, target.ID)
		require.NoError(t, err)

		// Try to unmute while deafened → should fail
		w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathMute), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)

		var respBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &respBody)
		assert.Contains(t, respBody["error"], "undeafen first")
	})
}

func TestServerDeafen(t *testing.T) {
	t.Run("ImpliesMute", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "sdeafown")
		target := ts.CreateTestUser(t, "sdeaftgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerDeafen Implies")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathDeafen), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var serverMuted, serverDeafened bool
		err := ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&serverMuted, &serverDeafened)
		require.NoError(t, err)
		assert.True(t, serverMuted, "deafen should also set server_muted")
		assert.True(t, serverDeafened)
	})

	t.Run("InsufficientPermission", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "sdeafpermown")
		actor := ts.CreateTestUser(t, "sdeafpermact")
		target := ts.CreateTestUser(t, "sdeafpermtgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerDeafen NoPerm")
		ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathDeafen), nil, testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestServerUndeafen(t *testing.T) {
	t.Run("ClearsBoth", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "sundeafown")
		target := ts.CreateTestUser(t, "sundeaftgt")
		serverID := ts.CreateTestServer(t, owner.ID, "ServerUndeafen Both")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		// Deafen first (sets both flags)
		_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true, server_deafened = true WHERE server_id = $1 AND user_id = $2`, serverID, target.ID)
		require.NoError(t, err)

		w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathDeafen), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var serverMuted, serverDeafened bool
		err = ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&serverMuted, &serverDeafened)
		require.NoError(t, err)
		assert.False(t, serverMuted, "undeafen should also clear server_muted")
		assert.False(t, serverDeafened)
	})
}

func TestUserMute(t *testing.T) {
	t.Run("NoHierarchyRequired", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "umutehierown")
		mod := ts.CreateTestUser(t, "umutehiermod")
		admin := ts.CreateTestUser(t, "umutehieradm")
		serverID := ts.CreateTestServer(t, owner.ID, "UserMute NoHierarchy")
		ts.AddMemberToServer(t, serverID, mod.ID, roleMember)
		ts.AddMemberToServer(t, serverID, admin.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-umute")

		// Mod (pos 5) has MuteMembers
		modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermMuteMembers))
		ts.AssignRoleToUser(t, serverID, mod.ID, modRoleID)

		// Admin (pos 10) outranks mod
		adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.AdminPermissions))
		ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

		// Insert admin as voice participant
		_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, admin.ID)
		require.NoError(t, err)

		// Mod user-mutes higher-ranked admin → should succeed (no hierarchy check)
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, admin.ID, pathUserMute), nil, testhelpers.AuthHeaders(mod.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("TargetNotInVoice", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "umutenovown")
		target := ts.CreateTestUser(t, "umutenovtgt")
		serverID := ts.CreateTestServer(t, owner.ID, "UserMute NoVoice")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		// Target is a server member but NOT in voice_participants
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathUserMute), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)

		var respBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &respBody)
		assert.Contains(t, respBody["error"], "not in a voice channel")
	})

	t.Run("InsufficientPermission", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "umutepermown")
		actor := ts.CreateTestUser(t, "umutepermact")
		target := ts.CreateTestUser(t, "umutepermtgt")
		serverID := ts.CreateTestServer(t, owner.ID, "UserMute NoPerm")
		ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-umuteperm")

		_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
		require.NoError(t, err)

		// actor has only base permissions (no MuteMembers)
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathUserMute), nil, testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestUserDeafen(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "udeafown")
		target := ts.CreateTestUser(t, "udeaftgt")
		serverID := ts.CreateTestServer(t, owner.ID, "UserDeafen OK")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-udeaf")

		_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
		require.NoError(t, err)

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathUserDeafen), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("TargetNotInVoice", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "udeafnovown")
		target := ts.CreateTestUser(t, "udeafnovtgt")
		serverID := ts.CreateTestServer(t, owner.ID, "UserDeafen NoVoice")
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathUserDeafen), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("InsufficientPermission", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "udeafpermown")
		actor := ts.CreateTestUser(t, "udeafpermact")
		target := ts.CreateTestUser(t, "udeafpermtgt")
		serverID := ts.CreateTestServer(t, owner.ID, "UserDeafen NoPerm")
		ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
		ts.AddMemberToServer(t, serverID, target.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-udeafperm")

		_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
		require.NoError(t, err)

		w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathUserDeafen), nil, testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// --- AuthorizeJoin media_entitlements Tests (#1300) ---

// insertSubscription inserts an active subscription row for a user so that the
// entitlement cache's read-through (ResolveTier) resolves the given tier. Mirrors
// internal/entitlements/resolver_test.go insertSub.
func insertSubscription(t *testing.T, ts *testhelpers.TestServer, userID, tier string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, $2, 'active', 'code')`,
		userID, tier,
	)
	require.NoError(t, err)
}

// assertMediaEntitlements asserts the media_entitlements object on a join-authorize
// response body matches the entitlements.MediaFor(tier) source of truth.
func assertMediaEntitlements(t *testing.T, body map[string]interface{}, tier string) {
	t.Helper()
	me, ok := body["media_entitlements"].(map[string]interface{})
	require.True(t, ok, "media_entitlements present and is an object")

	want := entitlements.MediaFor(tier)
	assert.Equal(t, want.Tier, me["tier"], "tier")
	assert.EqualValues(t, want.MinPtimeMs, me["min_ptime_ms"], "min_ptime_ms")
	assert.EqualValues(t, want.MaxManualBitrateBps, me["max_manual_bitrate_bps"], "max_manual_bitrate_bps")

	rawTiers, ok := me["allowed_audio_tiers"].([]interface{})
	require.True(t, ok, "allowed_audio_tiers is an array")
	gotTiers := make([]string, len(rawTiers))
	for i, v := range rawTiers {
		gotTiers[i] = v.(string)
	}
	assert.Equal(t, want.AllowedAudioTiers, gotTiers, "allowed_audio_tiers")
}

func TestAuthorizeJoin_MediaEntitlements(t *testing.T) {
	t.Run("FreeUserGetsFreeCaps", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "me_free_owner")
		member := ts.CreateTestUser(t, "me_free_member")
		serverID := ts.CreateTestServer(t, owner.ID, "ME Free Server")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-me-free")

		// No subscription row → fail-closed to free.
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assertMediaEntitlements(t, body, entitlements.TierFree)

		// Spot-check the exact free floor the media-plane depends on.
		me := body["media_entitlements"].(map[string]interface{})
		assert.Equal(t, "free", me["tier"])
		assert.EqualValues(t, 20, me["min_ptime_ms"])
		assert.EqualValues(t, 5000000, me["max_manual_bitrate_bps"])
		rawTiers := me["allowed_audio_tiers"].([]interface{})
		require.NotEmpty(t, rawTiers)
		assert.Equal(t, "minimum", rawTiers[0])
		assert.Equal(t, "standard", rawTiers[len(rawTiers)-1])
	})

	t.Run("PremiumUserGetsPremiumCaps", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "me_prem_owner")
		member := ts.CreateTestUser(t, "me_prem_member")
		serverID := ts.CreateTestServer(t, owner.ID, "ME Premium Server")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-me-prem")

		insertSubscription(t, ts, member.ID, entitlements.TierPremium)

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assertMediaEntitlements(t, body, entitlements.TierPremium)

		me := body["media_entitlements"].(map[string]interface{})
		assert.Equal(t, "premium", me["tier"])
		assert.EqualValues(t, 10, me["min_ptime_ms"])
		assert.EqualValues(t, 10000000, me["max_manual_bitrate_bps"])
	})

	t.Run("NoActiveSubscriptionFailsClosedToFree", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "me_canceled_owner")
		member := ts.CreateTestUser(t, "me_canceled_member")
		serverID := ts.CreateTestServer(t, owner.ID, "ME Canceled Server")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-me-canceled")

		// A canceled premium subscription is NOT active → ResolveTier returns free.
		_, err := ts.DB.Exec(
			`INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, 'premium', 'canceled', 'code')`,
			member.ID,
		)
		require.NoError(t, err)

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assertMediaEntitlements(t, body, entitlements.TierFree)
	})

	t.Run("TierResolvedFromAuthenticatedUserNotBody", func(t *testing.T) {
		// The premium subscription belongs to OWNER. MEMBER (a free user) joins and
		// supplies a body claiming owner's id + tier=premium. The handler must resolve
		// the tier from the authenticated member (free), ignoring any body value.
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "me_auth_owner")
		member := ts.CreateTestUser(t, "me_auth_member")
		serverID := ts.CreateTestServer(t, owner.ID, "ME Auth Server")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		channelID := ts.CreateVoiceChannel(t, serverID, "voice-me-auth")

		insertSubscription(t, ts, owner.ID, entitlements.TierPremium)

		// Hostile body: claim the premium owner's id and a premium tier directly.
		body := map[string]interface{}{
			"user_id":            owner.ID,
			"tier":               "premium",
			"media_entitlements": map[string]interface{}{"tier": "premium", "max_manual_bitrate_bps": 99999999},
		}
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, body, testhelpers.AuthHeaders(member.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		var resp map[string]interface{}
		testhelpers.ParseJSON(t, w, &resp)
		// Resolved from the authenticated member (free), NOT the body-claimed premium.
		assertMediaEntitlements(t, resp, entitlements.TierFree)
		me := resp["media_entitlements"].(map[string]interface{})
		assert.Equal(t, "free", me["tier"], "tier must come from the JWT user, not the request body")
		assert.EqualValues(t, 5000000, me["max_manual_bitrate_bps"], "body cannot raise the bitrate cap")
	})
}

func TestAuthorizeJoin_ChannelStandard_ShapesMediaEntitlements(t *testing.T) {
	// A free member joins a voice channel whose audio_quality_tier='standard'
	// on a Groundspeed (default) server. The channel standard uplifts the member
	// so "standard" appears in allowed_audio_tiers.  "studio" must NOT appear
	// (Groundspeed ceiling is standard, not studio).
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ch_std_owner")
	member := ts.CreateTestUser(t, "ch_std_member")
	serverID := ts.CreateTestServer(t, owner.ID, "ChannelStd Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ch-std")

	// Set audio_quality_tier on the channel directly via SQL.
	_, err := ts.DB.Exec("UPDATE channels SET audio_quality_tier=$1 WHERE id=$2", "standard", channelID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	me, ok := body["media_entitlements"].(map[string]interface{})
	require.True(t, ok, "media_entitlements present")

	rawTiers := me["allowed_audio_tiers"].([]interface{})
	gotTiers := make([]string, len(rawTiers))
	for i, v := range rawTiers {
		gotTiers[i] = v.(string)
	}
	assert.Contains(t, gotTiers, "standard", "standard tier must be granted by the channel")
	assert.NotContains(t, gotTiers, "studio", "studio must not be granted on a Groundspeed server")
}

func TestAuthorizeJoin_MachChannelStandardUsesServerTierCache(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ch_mach_owner")
	member := ts.CreateTestUser(t, "ch_mach_member")
	serverID := ts.CreateTestServer(t, owner.ID, "Mach ChannelStd Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ch-mach")
	require.NoError(t, entitlements.NewServerCache(ts.Redis, ts.DB).
		SetServerTier(context.Background(), serverID, entitlements.TierMach))

	_, err := ts.DB.Exec("UPDATE channels SET audio_quality_tier=$1 WHERE id=$2", "studio", channelID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	me, ok := body["media_entitlements"].(map[string]interface{})
	require.True(t, ok, "media_entitlements present")

	rawTiers := me["allowed_audio_tiers"].([]interface{})
	gotTiers := make([]string, len(rawTiers))
	for i, v := range rawTiers {
		gotTiers[i] = v.(string)
	}
	assert.Contains(t, gotTiers, "studio", "Studio must be granted by a Mach-bounded channel standard")
	assert.EqualValues(t, 10, me["min_ptime_ms"], "Studio channel standard must allow 10ms ptime")
}
