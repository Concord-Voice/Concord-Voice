package voice_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

const pathDisconnect = "/disconnect"

// TestServerDisconnect_WithPermission: a MOVE_MEMBERS holder disconnecting a
// lower-ranked member who IS in voice → 200 {disconnected:true}.
func TestServerDisconnect_WithPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dc_perm_owner")
	mover := ts.CreateTestUser(t, "dc_perm_mover")
	target := ts.CreateTestUser(t, "dc_perm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Disconnect Perm")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice-dc-perm")
	joinVoice(t, ts, channelID, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathDisconnect),
		nil, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["disconnected"])
}

// TestServerDisconnect_NoPermission: a base member without MOVE_MEMBERS → 403.
func TestServerDisconnect_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dc_np_owner")
	actor := ts.CreateTestUser(t, "dc_np_actor")
	target := ts.CreateTestUser(t, "dc_np_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Disconnect NoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-dc-np")
	joinVoice(t, ts, channelID, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathDisconnect),
		nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestServerDisconnect_HierarchyBlocked: disconnect RESPECTS hierarchy (unlike
// /move). A MOVE_MEMBERS holder disconnecting a HIGHER-ranked member → 403.
func TestServerDisconnect_HierarchyBlocked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dc_hier_owner")
	mover := ts.CreateTestUser(t, "dc_hier_mover")
	target := ts.CreateTestUser(t, "dc_hier_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Disconnect Hierarchy")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Mover (pos 5) has MOVE_MEMBERS; target (pos 10) OUTRANKS the mover.
	moverRole := ts.CreateTestRole(t, serverID, "Mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	higherRole := ts.CreateTestRole(t, serverID, "Senior", 10, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, target.ID, higherRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice-dc-hier")
	joinVoice(t, ts, channelID, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathDisconnect),
		nil, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "disconnect enforces hierarchy (no ADR-0023 exception)")
}

// TestServerDisconnect_TargetNotInVoice: target is a member but not in any voice
// channel → 409.
func TestServerDisconnect_TargetNotInVoice(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dc_niv_owner")
	target := ts.CreateTestUser(t, "dc_niv_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Disconnect NotInVoice")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathDisconnect),
		nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestServerDisconnect_Self: the self-target guard does NOT apply to disconnect
// (requireHierarchy=true path), but the owner disconnecting themselves still hits
// the hierarchy check against themselves (CheckHierarchy passes for self) and a
// real in-voice row, so the owner CAN disconnect themselves → 200. The relevant
// security property is that a non-self moderator cannot exceed hierarchy; for self
// the action is harmless. This documents the self path returns 200 when in voice.
func TestServerDisconnect_Self(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dc_self_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Disconnect Self")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-dc-self")
	joinVoice(t, ts, channelID, owner.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, owner.ID, pathDisconnect),
		nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "owner may disconnect self (in voice)")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["disconnected"])
}

// TestServerDisconnect_InvalidUUID: non-UUID path params → 400 before any work.
func TestServerDisconnect_InvalidUUID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dc_uuid_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Disconnect BadUUID")

	w := ts.DoRequest("POST", pathServersPrefix+"not-a-uuid"+pathVoice+owner.ID+pathDisconnect,
		nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathVoice+"not-a-uuid"+pathDisconnect,
		nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
