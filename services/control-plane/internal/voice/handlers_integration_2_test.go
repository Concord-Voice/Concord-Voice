package voice_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// GetParticipants additional edge cases
// =============================================================================

func TestGetParticipants_WithVoiceParticipant(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vpwithpart")
	serverID := ts.CreateTestServer(t, owner.ID, "VP With Participant")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-with-part")

	// Insert a voice participant directly
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing, joined_at)
		VALUES ($1, $2, false, false, false, false, NOW())
	`, channelID, owner.ID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 1)

	p := participants[0].(map[string]interface{})
	assert.Equal(t, owner.ID, p["user_id"])
	assert.Equal(t, false, p["is_muted"])
	assert.Equal(t, false, p["is_deafened"])
	assert.Equal(t, false, p["is_video_on"])
	assert.Equal(t, false, p["is_screen_sharing"])
}

func TestGetParticipants_MultipleParticipants(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vpmultown")
	member := ts.CreateTestUser(t, "vpmultmem")
	serverID := ts.CreateTestServer(t, owner.ID, "VP Multi")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-multi")

	// Insert two participants
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing, joined_at)
		VALUES ($1, $2, true, false, false, false, NOW()),
		       ($1, $3, false, true, true, false, NOW())
	`, channelID, owner.ID, member.ID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 2)
}

func TestGetParticipants_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vpunauthown")
	serverID := ts.CreateTestServer(t, owner.ID, "VP Unauth")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-unauth")

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestGetParticipants_NonexistentChannel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "vpnonexist")
	fakeID := uuid.New().String()

	w := ts.DoRequest("GET", pathChannelsPrefix+fakeID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(user.AccessToken))
	// User is not a member of any server containing this channel
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetParticipants_MemberCanView(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vpmemown")
	member := ts.CreateTestUser(t, "vpmemview")
	serverID := ts.CreateTestServer(t, owner.ID, "VP MemberView")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-memview")

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// AuthorizeJoin additional edge cases
// =============================================================================

func TestAuthorizeJoin_InvalidChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "vjinvalidch")

	w := ts.DoRequest("POST", "/api/v1/channels/not-a-uuid/voice/join", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeJoin_NonexistentChannel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "vjnonexist")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathChannelsPrefix+fakeID+pathVoiceJoin, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeJoin_TextChannelRejected(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjtextowner")
	serverID := ts.CreateTestServer(t, owner.ID, "VJ TextChannel")
	// Create a text channel (not voice)
	textChannelID := ts.CreateTestChannel(t, serverID, "text-general")

	w := ts.DoRequest("POST", pathChannelsPrefix+textChannelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Not a voice channel", body["error"])
}

func TestAuthorizeJoin_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjunauthown")
	serverID := ts.CreateTestServer(t, owner.ID, "VJ Unauth")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-unauth-join")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthorizeJoin_ReturnsChannelInfo(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjinfoown")
	serverID := ts.CreateTestServer(t, owner.ID, "VJ Info Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-info")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
	assert.NotNil(t, body["permissions"])

	channel := body["channel"].(map[string]interface{})
	assert.Equal(t, channelID, channel["id"])
	assert.Equal(t, "voice-info", channel["name"])
	assert.Equal(t, serverID, channel["server_id"])
}

func TestAuthorizeJoin_OwnerJoinSucceeds(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjownerjoin")
	serverID := ts.CreateTestServer(t, owner.ID, "VJ Owner Join")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-owner")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthorizeJoin_ChannelOverrideDenyJoinVoice(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjdenyown2")
	member := ts.CreateTestUser(t, "vjdenymem2")
	serverID := ts.CreateTestServer(t, owner.ID, "VJ Deny2")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-deny2")

	// Deny JoinVoice on the channel for member (user-level override)
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermJoinVoice))

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =============================================================================
// AuthorizeVoiceAction additional edge cases
// =============================================================================

func TestAuthorizeVoiceAction_InvalidChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "vainvalidch")

	w := ts.DoRequest("POST", "/api/v1/channels/not-a-uuid/voice/authorize-action", map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: uuid.New().String(),
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceAction_MissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vanobody")
	serverID := ts.CreateTestServer(t, owner.ID, "VA NoBody")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-nobody")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceAction_MissingAction(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vanoaction")
	target := ts.CreateTestUser(t, "vanoactarget")
	serverID := ts.CreateTestServer(t, owner.ID, "VA NoAction")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-noaction")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceAction_MissingTargetUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vanotarget")
	serverID := ts.CreateTestServer(t, owner.ID, "VA NoTarget")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-notarget")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction: "mute",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceAction_NonMemberActor(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vanonmemown")
	outsider := ts.CreateTestUser(t, "vanonmemout")
	target := ts.CreateTestUser(t, "vanonmemtgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA NonMember Actor")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-nonmem")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_TargetNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vatgtnonmemown")
	outsider := ts.CreateTestUser(t, "vatgtnonmemout")
	serverID := ts.CreateTestServer(t, owner.ID, "VA TargetNotMember")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tgtnonmem")

	// Owner has MuteMembers (as owner), tries to mute a non-member
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: outsider.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceAction_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaunauthown")
	target := ts.CreateTestUser(t, "vaunauthtgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA Unauth")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-unauth-action")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: target.ID,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthorizeVoiceAction_DeafenAction(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vadeafenown")
	target := ts.CreateTestUser(t, "vadeafentgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA Deafen")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-deafen")

	// Owner should be able to deafen (owner has all permissions)
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "deafen",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
	assert.Equal(t, "deafen", body["action"])
}

func TestAuthorizeVoiceAction_MoveAction(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vamoveown")
	target := ts.CreateTestUser(t, "vamovetgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA Move")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-move")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "move",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
	assert.Equal(t, "move", body["action"])
}

func TestAuthorizeVoiceAction_MemberDeafenNoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vadeafnpown")
	actor := ts.CreateTestUser(t, "vadeafnpact")
	target := ts.CreateTestUser(t, "vadeafnptgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA DeafenNoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-deafnp")

	// Base member does not have DeafenMembers
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "deafen",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_MemberMoveNoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vamovnpown")
	actor := ts.CreateTestUser(t, "vamovnpact")
	target := ts.CreateTestUser(t, "vamovnptgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA MoveNoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-movnp")

	// Base member does not have MoveMembers
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "move",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_ModeratorCanDeafenMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vamoddeafown")
	moderator := ts.CreateTestUser(t, "vamoddeafmod")
	target := ts.CreateTestUser(t, "vamoddeaftgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA Mod Deafen")
	ts.AddMemberToServer(t, serverID, moderator.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-moddeaf")

	// Give moderator DeafenMembers at higher position
	modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermDeafenMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, modRoleID)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "deafen",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(moderator.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthorizeVoiceAction_ModeratorCanMoveMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vamodmoveown")
	moderator := ts.CreateTestUser(t, "vamodmovemod")
	target := ts.CreateTestUser(t, "vamodmovetgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA Mod Move")
	ts.AddMemberToServer(t, serverID, moderator.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-modmove")

	// Give moderator MoveMembers at higher position
	modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, modRoleID)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "move",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(moderator.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthorizeVoiceAction_NonexistentChannel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "vanonexch")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathChannelsPrefix+fakeID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: uuid.New().String(),
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_ResponseStructure(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "varespown")
	target := ts.CreateTestUser(t, "varesptgt")
	serverID := ts.CreateTestServer(t, owner.ID, "VA Response")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-resp")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: target.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
	assert.Equal(t, "mute", body["action"])
	assert.Equal(t, target.ID, body["target_id"])
	assert.Equal(t, channelID, body["channel_id"])
}

func TestAuthorizeVoiceAction_InvalidTargetUUID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vainvtarget")
	serverID := ts.CreateTestServer(t, owner.ID, "VA InvalidTarget")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-invtarget")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathVoiceAuthorizeAction, map[string]interface{}{
		keyAction:       "mute",
		keyTargetUserID: "not-a-uuid",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
