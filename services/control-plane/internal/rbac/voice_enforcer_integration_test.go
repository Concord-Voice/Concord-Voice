package rbac_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests lock the CV-CAN-007 review-P1 wiring end-to-end: an RBAC
// mutation over HTTP must push a voice.enforce.permissions recheck for a
// voice-connected member, through the real router wiring
// (rbac.Handler.SetVoiceEnforcer -> voice.PermissionEnforcer -> NATS). They
// require a live NATS server (dev env / CI) and skip otherwise, mirroring
// internal/voice/nats_disconnect_test.go.

func rbacNATSTestURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return "nats://localhost:4222"
}

// subscribeVoicePermissions returns a channel of decoded
// voice.enforce.permissions payloads, skipping the test if NATS is down.
func subscribeVoicePermissions(t *testing.T) chan map[string]interface{} {
	t.Helper()
	obsClient, err := natsclient.Connect(rbacNATSTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live enforcement test (runs in CI)", err)
	}
	t.Cleanup(obsClient.Close)

	msgs := make(chan map[string]interface{}, 8)
	sub, err := obsClient.Subscribe("voice.enforce.permissions", func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) == nil {
			msgs <- payload
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, obsClient.Flush())
	return msgs
}

// waitRecheckFor drains payloads until one matches the wanted user (async
// publishes from sibling tests share these subjects), failing on timeout.
func waitRecheckFor(t *testing.T, msgs chan map[string]interface{}, userID string) map[string]interface{} {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case p := <-msgs:
			if got, _ := p["userId"].(string); got == userID {
				return p
			}
		case <-deadline:
			t.Fatal("timed out waiting for voice.enforce.permissions push")
			return nil
		}
	}
}

func addVoiceParticipantRow(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

// TestAssignRole_PushesVoicePermissionRecheck: assigning a role to a member who
// is currently in a voice channel must push their recomputed bitfield.
func TestAssignRole_PushesVoicePermissionRecheck(t *testing.T) {
	msgs := subscribeVoicePermissions(t)
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateVoiceChannel(t, serverID, "enforce-voice")
	addVoiceParticipantRow(t, ts, channelID, member.ID)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "VoicePush", 0)
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID),
		map[string]interface{}{"role_id": roleID}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	payload := waitRecheckFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
	permStr, ok := payload["permissions"].(string)
	assert.True(t, ok, "permissions must ride as the decimal-string wire format")
	assert.Regexp(t, `^\d+$`, permStr)
}

// TestUpdateRole_PushesVoicePermissionRecheck: editing a role's permissions
// must push a recheck to every voice-connected member of the server.
func TestUpdateRole_PushesVoicePermissionRecheck(t *testing.T) {
	msgs := subscribeVoicePermissions(t)
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateVoiceChannel(t, serverID, "enforce-voice")
	addVoiceParticipantRow(t, ts, channelID, member.ID)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "VoiceEdit", 0)
	w := ts.DoRequest("PATCH", rolePath(serverID, roleID),
		map[string]interface{}{"name": "VoiceEdited"}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	payload := waitRecheckFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
}

// mustParseBitfield parses the decimal-string permissions payload field.
func mustParseBitfield(t *testing.T, payload map[string]interface{}) int64 {
	t.Helper()
	str, ok := payload["permissions"].(string)
	require.True(t, ok, "permissions must be a decimal string, got %T", payload["permissions"])
	v, err := strconv.ParseInt(str, 10, 64)
	require.NoError(t, err)
	return v
}

// TestUpsertChannelOverride_PushesVoicePermissionRecheck: a channel permission
// override must push a recheck to members currently in that voice channel.
func TestUpsertChannelOverride_PushesVoicePermissionRecheck(t *testing.T) {
	msgs := subscribeVoicePermissions(t)
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateVoiceChannel(t, serverID, "enforce-voice")
	addVoiceParticipantRow(t, ts, channelID, member.ID)

	// Deny-Speak user override on the voice channel for the member.
	body := map[string]interface{}{
		"target_type": "user",
		"target_id":   member.ID,
		"allow":       0,
		"deny":        int64(rbac.PermSpeak),
	}
	w := ts.DoRequest("PUT", channelOverridesPath(channelID), body,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "override upsert should succeed: %s", w.Body.String())

	payload := waitRecheckFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
	// The pushed bitfield must REFLECT the mutation: Speak was just denied.
	assert.Zero(t, mustParseBitfield(t, payload)&int64(rbac.PermSpeak),
		"denied Speak bit must be cleared in the pushed bitfield")
}

// TestUnassignRole_PushesRevokedBitfield locks the revocation direction on the
// user-scope call site: unassigning a role must push a bitfield WITHOUT the
// role's distinctive bit.
func TestUnassignRole_PushesRevokedBitfield(t *testing.T) {
	msgs := subscribeVoicePermissions(t)
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateVoiceChannel(t, serverID, "enforce-voice")
	addVoiceParticipantRow(t, ts, channelID, member.ID)

	// MuteMembers is not in the default member grant — a distinctive marker bit.
	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "VoiceMuteRole", int64(rbac.PermMuteMembers))
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID),
		map[string]interface{}{"role_id": roleID}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	granted := waitRecheckFor(t, msgs, member.ID)
	require.NotZero(t, mustParseBitfield(t, granted)&int64(rbac.PermMuteMembers),
		"assign push must carry the granted bit")

	w = ts.DoRequest("DELETE", unassignRolePath(serverID, member.ID, roleID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	revoked := waitRecheckFor(t, msgs, member.ID)
	assert.Zero(t, mustParseBitfield(t, revoked)&int64(rbac.PermMuteMembers),
		"unassign push must clear the revoked bit")
}

// TestDeleteRole_PushesRevokedBitfield locks the revocation direction on the
// server-scope call site: deleting an assigned role must push the shrunken
// bitfield to voice-connected members.
func TestDeleteRole_PushesRevokedBitfield(t *testing.T) {
	msgs := subscribeVoicePermissions(t)
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateVoiceChannel(t, serverID, "enforce-voice")
	addVoiceParticipantRow(t, ts, channelID, member.ID)

	roleID := createRoleViaAPI(t, ts, serverID, owner.AccessToken, "VoiceDeleteRole", int64(rbac.PermMuteMembers))
	w := ts.DoRequest("POST", assignRolePath(serverID, member.ID),
		map[string]interface{}{"role_id": roleID}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	granted := waitRecheckFor(t, msgs, member.ID)
	require.NotZero(t, mustParseBitfield(t, granted)&int64(rbac.PermMuteMembers))

	w = ts.DoRequest("DELETE", rolePath(serverID, roleID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	revoked := waitRecheckFor(t, msgs, member.ID)
	assert.Zero(t, mustParseBitfield(t, revoked)&int64(rbac.PermMuteMembers),
		"delete-role push must clear the role's bit")
}
