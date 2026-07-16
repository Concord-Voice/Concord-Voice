package members_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests lock the CV-CAN-007 review-P1 wiring end-to-end: kicking, leaving,
// or banning a member who is currently in a voice channel must evict them from
// the media plane, through the real router wiring
// (members.Handler.SetVoiceEnforcer -> voice.PermissionEnforcer -> NATS). The
// removed member is no longer a server member, so the enforcer's fresh resolve
// returns ErrNotMember and publishes voice.enforce.disconnect rather than a
// recomputed permissions push. They require a live NATS server (dev env / CI)
// and skip otherwise, mirroring internal/rbac/voice_enforcer_integration_test.go.

func membersNATSTestURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return "nats://localhost:4222"
}

// subscribeVoiceDisconnect returns a channel of decoded voice.enforce.disconnect
// payloads, skipping the test if NATS is down.
func subscribeVoiceDisconnect(t *testing.T) chan map[string]interface{} {
	t.Helper()
	obsClient, err := natsclient.Connect(membersNATSTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live enforcement test (runs in CI)", err)
	}
	t.Cleanup(obsClient.Close)

	msgs := make(chan map[string]interface{}, 8)
	sub, err := obsClient.Subscribe("voice.enforce.disconnect", func(data []byte) {
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

// waitDisconnectFor drains payloads until one matches the wanted user (async
// publishes from sibling tests share this subject), failing on timeout.
func waitDisconnectFor(t *testing.T, msgs chan map[string]interface{}, userID string) map[string]interface{} {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case p := <-msgs:
			if got, _ := p["userId"].(string); got == userID {
				return p
			}
		case <-deadline:
			t.Fatal("timed out waiting for voice.enforce.disconnect push")
			return nil
		}
	}
}

func seedVoiceParticipant(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

// TestRemoveMember_EvictsFromVoice: kicking a member who is in a voice channel
// must publish voice.enforce.disconnect so the media plane drops their producers
// instead of leaving them on their stale join-time snapshot.
func TestRemoveMember_EvictsFromVoice(t *testing.T) {
	msgs := subscribeVoiceDisconnect(t)
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vkickown")
	member := ts.CreateTestUser(t, "vkickmem")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceKickServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "kick-voice")
	seedVoiceParticipant(t, ts, channelID, member.ID)

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	payload := waitDisconnectFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
}

// TestRemoveMemberSelfLeave_EvictsFromVoice: a member leaving on their own must
// also be evicted from any voice channel they were in.
func TestRemoveMemberSelfLeave_EvictsFromVoice(t *testing.T) {
	msgs := subscribeVoiceDisconnect(t)
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vleaveown")
	member := ts.CreateTestUser(t, "vleavemem")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceLeaveServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "leave-voice")
	seedVoiceParticipant(t, ts, channelID, member.ID)

	w := ts.DoRequest("DELETE", memberPath(serverID, member.ID), nil,
		testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	payload := waitDisconnectFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
}

// TestBanMember_EvictsFromVoice: banning a member who is in a voice channel must
// publish voice.enforce.disconnect for the same reason as a kick.
func TestBanMember_EvictsFromVoice(t *testing.T) {
	msgs := subscribeVoiceDisconnect(t)
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vbanown")
	member := ts.CreateTestUser(t, "vbanmem")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceBanServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "ban-voice")
	seedVoiceParticipant(t, ts, channelID, member.ID)

	w := ts.DoRequest("POST", banPath(serverID, member.ID),
		map[string]interface{}{"reason": "voice eviction test"},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	payload := waitDisconnectFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
}

// TestTimeoutMember_EvictsFromVoice: timing out a member who is in a voice
// channel must publish voice.enforce.disconnect. Unlike a kick/ban the member
// remains in the server, so this goes through the unconditional DisconnectUser
// path (the join gate bars a timed-out member independently of the permission
// bitfield, so a recheck would leave them live in the room).
func TestTimeoutMember_EvictsFromVoice(t *testing.T) {
	msgs := subscribeVoiceDisconnect(t)
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vtoown")
	member := ts.CreateTestUser(t, "vtomem")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceTimeoutServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "timeout-voice")
	seedVoiceParticipant(t, ts, channelID, member.ID)

	w := ts.DoRequest("POST", timeoutPath(serverID, member.ID),
		map[string]interface{}{"duration_seconds": 300},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	payload := waitDisconnectFor(t, msgs, member.ID)
	assert.Equal(t, channelID, payload["channelId"])
}
