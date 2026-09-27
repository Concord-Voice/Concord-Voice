package voice_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A request admitted by the stale epoch cache must not receive server voice
// authority. The same authorizeVoiceMod seam protects mute, deafen, move, and
// temporary-grant revoke; this representative mutation also proves that no
// durable enforcement was applied.
func TestStaleCredential_ServerMuteDoesNotIssueVoiceAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_moderator")
	target := ts.CreateTestUser(t, "stale_voice_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice authority")
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodPost,
		"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/mute",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var muted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, target.ID,
	).Scan(&muted))
	assert.False(t, muted, "stale credentials must not apply server mute")
}

// demoteVoiceModerator holds the same authority lock used by role mutation
// endpoints. Requests that passed an optimistic preflight must wait here and
// then observe the committed demotion in their in-effect authorization check.
func demoteVoiceModerator(t *testing.T, ts *testhelpers.TestServer, serverID, userID, roleID string) func() {
	t.Helper()
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, userID, roleID)
	require.NoError(t, err)
	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	return func() {
		dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, key)
		require.NoError(t, tx.Commit())
	}
}

func TestVoiceEffectRejectsModeratorDemotedAfterPreflight(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "rbac_voice_effect_owner")
	moderator := ts.CreateTestUser(t, "rbac_voice_effect_moderator")
	target := ts.CreateTestUser(t, "rbac_voice_effect_target")
	serverID := ts.CreateTestServer(t, owner.ID, "rbac voice effect")
	ts.AddMemberToServer(t, serverID, moderator.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "muter", 5, int64(rbac.PermMuteMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, roleID)

	commitDemotion := demoteVoiceModerator(t, ts, serverID, moderator.ID, roleID)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/voice/"+target.ID+"/mute", nil, testhelpers.AuthHeaders(moderator.AccessToken))
	}()
	commitDemotion()
	w := <-response
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var muted bool
	require.NoError(t, ts.DB.QueryRow(`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&muted))
	assert.False(t, muted)
}

func TestVoiceNATSEffectRejectsModeratorDemotedAfterPreflight(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "rbac_voice_nats_owner")
	moderator := ts.CreateTestUser(t, "rbac_voice_nats_moderator")
	target := ts.CreateTestUser(t, "rbac_voice_nats_target")
	serverID := ts.CreateTestServer(t, owner.ID, "rbac voice nats")
	ts.AddMemberToServer(t, serverID, moderator.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "muter", 5, int64(rbac.PermMuteMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, roleID)
	channelID := ts.CreateVoiceChannel(t, serverID, "room")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
	require.NoError(t, err)
	observer, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	delivered := make(chan struct{}, 1)
	_, err = observer.Subscribe("voice.user_mute", func([]byte) { delivered <- struct{}{} })
	require.NoError(t, err)
	require.NoError(t, observer.Flush())

	commitDemotion := demoteVoiceModerator(t, ts, serverID, moderator.ID, roleID)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/voice/"+target.ID+"/user-mute", nil, testhelpers.AuthHeaders(moderator.AccessToken))
	}()
	commitDemotion()
	w := <-response
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	select {
	case <-delivered:
		t.Fatal("demoted moderator published voice.user_mute")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestVoiceMoveGrantRejectsModeratorDemotedAfterPreflight(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "rbac_voice_move_owner")
	moderator := ts.CreateTestUser(t, "rbac_voice_move_moderator")
	target := ts.CreateTestUser(t, "rbac_voice_move_target")
	serverID := ts.CreateTestServer(t, owner.ID, "rbac voice move")
	ts.AddMemberToServer(t, serverID, moderator.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, roleID)
	fromID := ts.CreateVoiceChannel(t, serverID, "from")
	toID := ts.CreateVoiceChannel(t, serverID, "to")
	hideVoiceChannel(t, ts, serverID, toID)
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, fromID, target.ID)
	require.NoError(t, err)

	commitDemotion := demoteVoiceModerator(t, ts, serverID, moderator.ID, roleID)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest(http.MethodPost, "/api/v1/servers/"+serverID+"/voice/"+target.ID+"/move", map[string]interface{}{"target_channel_id": toID}, testhelpers.AuthHeaders(moderator.AccessToken))
	}()
	commitDemotion()
	w := <-response
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.False(t, moveTempOverrideExists(t, ts, toID, target.ID))
}

func TestVoiceTempRevokeRejectsModeratorDemotedAfterPreflight(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "rbac_voice_revoke_owner")
	moderator := ts.CreateTestUser(t, "rbac_voice_revoke_moderator")
	target := ts.CreateTestUser(t, "rbac_voice_revoke_target")
	serverID := ts.CreateTestServer(t, owner.ID, "rbac voice revoke")
	ts.AddMemberToServer(t, serverID, moderator.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, roleID)
	channelID := ts.CreateVoiceChannel(t, serverID, "room")
	seedTempGrant(t, ts, serverID, channelID, target.ID)

	commitDemotion := demoteVoiceModerator(t, ts, serverID, moderator.ID, roleID)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+serverID+"/voice/"+target.ID+"/temp-access", map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(moderator.AccessToken))
	}()
	commitDemotion()
	w := <-response
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID))
}

// Voice join is a media-admission endpoint: a stale bearer must fail before
// the response can authorize a media-plane connection.
func TestStaleCredential_ServerVoiceJoinDoesNotAuthorizeMedia(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_join_owner")
	member := ts.CreateTestUser(t, "stale_voice_join_member")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice join")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "stale voice room")

	staleToken := ts.SimulateStaleEpochWindow(t, member.ID)
	w := ts.DoRequest(http.MethodPost,
		"/api/v1/channels/"+channelID+"/voice/join",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), `"allowed":true`)
}

func TestStaleCredential_ServerMoveDoesNotIssueTemporaryGrant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_move_owner")
	target := ts.CreateTestUser(t, "stale_voice_move_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice move")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	from := ts.CreateVoiceChannel(t, serverID, "stale voice source")
	to := ts.CreateVoiceChannel(t, serverID, "stale voice destination")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, from, target.ID)
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodPost,
		"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/move",
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// Self moves take no temporary-grant path, so they exercise the separate held
// credential fence around the directed voice_move effect itself.
func TestStaleCredential_ServerSelfMoveDoesNotSignalVoiceMove(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	member := ts.CreateTestUser(t, "stale_voice_self_move_member")
	owner := ts.CreateTestUser(t, "stale_voice_self_move_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice self move")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	from := ts.CreateVoiceChannel(t, serverID, "stale self move source")
	to := ts.CreateVoiceChannel(t, serverID, "stale self move destination")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, from, member.ID)
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, member.ID)
	w := ts.DoRequest(http.MethodPost,
		"/api/v1/servers/"+serverID+"/voice/"+member.ID+"/move",
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestStaleCredential_ServerDisconnectDoesNotIssueNATSEnforcement(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_disconnect_owner")
	target := ts.CreateTestUser(t, "stale_voice_disconnect_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice disconnect")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "stale disconnect room")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodPost,
		"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/disconnect",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestStaleCredential_RevokeTempAccessDoesNotIssueAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_revoke_owner")
	target := ts.CreateTestUser(t, "stale_voice_revoke_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice revoke")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "stale revoke room")
	seedTempGrant(t, ts, serverID, channelID, target.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID))

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodDelete,
		"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/temp-access",
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID), "stale credentials must not revoke the temporary grant")
}

func TestStaleCredential_ServerUnmuteDoesNotClearEnforcement(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_unmute_owner")
	target := ts.CreateTestUser(t, "stale_voice_unmute_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice unmute")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`, serverID, target.ID)
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodDelete,
		"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/mute",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var muted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID,
	).Scan(&muted))
	assert.True(t, muted, "stale credentials must not clear server mute")
}

func TestCredentialEpoch_ServerUnmuteLinearizesBeforeConcurrentReset(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_unmute_race_owner")
	target := ts.CreateTestUser(t, "stale_voice_unmute_race_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice unmute race")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	_, err := ts.DB.Exec(`UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`, serverID, target.ID)
	require.NoError(t, err)

	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var xid int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&xid))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT user_id FROM server_members WHERE server_id = $1 AND user_id = $2 FOR UPDATE`, serverID, target.ID).Scan(&locked))

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- ts.DoRequest(http.MethodDelete,
			"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/mute", nil,
			testhelpers.AuthHeaders(owner.AccessToken))
	}()
	dbtest.WaitForRowLockWaiter(t, ts.DB, xid)
	resetStarted := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		close(resetStarted)
		defer close(resetDone)
		ts.SimulateStaleEpochWindow(t, owner.ID)
	}()
	<-resetStarted
	// GuardTx owns the user row while the request waits on the target row, so a
	// concurrent reset cannot commit ahead of this already-linearized unmute.
	require.Never(t, func() bool {
		select {
		case <-resetDone:
			return true
		default:
			return false
		}
	}, 100*time.Millisecond, 10*time.Millisecond)
	require.NoError(t, barrier.Commit())
	w := <-result
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	<-resetDone
	var muted bool
	require.NoError(t, ts.DB.QueryRow(`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target.ID).Scan(&muted))
	assert.False(t, muted, "the unmute linearized before the reset")
}

func TestCredentialEpoch_UserMuteLinearizesBeforeConcurrentReset(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale_voice_user_mute_race_owner")
	target := ts.CreateTestUser(t, "stale_voice_user_mute_race_target")
	serverID := ts.CreateTestServer(t, owner.ID, "stale voice user mute race")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "stale user mute race room")
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, target.ID)
	require.NoError(t, err)

	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	_, err = barrier.Exec(`LOCK TABLE voice_participants IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- ts.DoRequest(http.MethodPost,
			"/api/v1/servers/"+serverID+"/voice/"+target.ID+"/user-mute", nil,
			testhelpers.AuthHeaders(owner.AccessToken))
	}()
	for attempt := 0; attempt < 1000; attempt++ {
		var waiting bool
		err = ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity a JOIN pg_locks l ON l.pid = a.pid JOIN pg_class c ON c.oid = l.relation WHERE a.wait_event_type = 'Lock' AND NOT l.granted AND c.relname = 'voice_participants')`).Scan(&waiting)
		require.NoError(t, err)
		if waiting {
			break
		}
		if attempt == 999 {
			t.Fatal("user mute request did not wait on voice_participants table lock")
		}
		time.Sleep(time.Millisecond)
	}
	resetStarted := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		close(resetStarted)
		defer close(resetDone)
		ts.SimulateStaleEpochWindow(t, owner.ID)
	}()
	<-resetStarted
	require.Never(t, func() bool {
		select {
		case <-resetDone:
			return true
		default:
			return false
		}
	}, 100*time.Millisecond, 10*time.Millisecond)
	require.NoError(t, barrier.Commit())
	w := <-result
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	<-resetDone
}
