package voice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type voiceWireEnvelope struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

func connectVoiceWireClient(
	t *testing.T,
	ts *testhelpers.TestServer,
	user testhelpers.TestUser,
) *gorillaWS.Conn {
	t.Helper()
	httpServer := httptest.NewServer(ts.Router)
	t.Cleanup(httpServer.Close)
	return connectVoiceWireClientAtURL(t, ts.Redis, ts.Hub, httpServer.URL, user)
}

func connectVoiceWireClientAtURL(
	t *testing.T,
	redisClient *redis.Client,
	hub *concordws.Hub,
	baseURL string,
	user testhelpers.TestUser,
) *gorillaWS.Conn {
	t.Helper()
	ticket := "voice-rich-presence-" + uuid.NewString()
	require.NoError(t, redisClient.Set(
		context.Background(), "ws_ticket:"+ticket, user.ID+":test-session", 30*time.Second,
	).Err())
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(baseURL, "http")+
			"/api/v1/ws?ticket="+ticket+"&activity_rich_presence=1",
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return hub.GetUserClientCount(uuid.MustParse(user.ID)) == 1
	}, time.Second, 10*time.Millisecond)
	return conn
}

func newVoiceReplicaHub(
	t *testing.T,
	ts *testhelpers.TestServer,
) (*concordws.Hub, string) {
	t.Helper()
	hub := concordws.NewHub(ts.DB, ts.Redis)
	resolver := rbac.NewResolver(
		ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test-replica"),
	)
	activityStore := presence.NewActivityStore(ts.Redis)
	activityBuilder := presence.NewActivityBuilder(
		ts.DB, testCallLeaseVerifier{redis: ts.Redis}, activityStore,
	)
	hub.SetActivitySnapshotService(presence.NewActivitySnapshotService(
		ts.DB,
		activityBuilder,
		activityStore,
		resolver,
		ts.PresenceHistory,
		permitAllPresence{},
	))
	go hub.Run()
	handler := concordws.NewHandler(
		hub, ts.DB, ts.Redis, testhelpers.TestJWTSecret, []string{"*"}, nil, nil,
	)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/ws", handler.HandleWebSocket)
	server := httptest.NewServer(http.Handler(router))
	t.Cleanup(func() { hub.Shutdown() })
	t.Cleanup(server.Close)
	return hub, server.URL
}

func waitForVoiceWireType(
	t *testing.T,
	conn *gorillaWS.Conn,
	wantType string,
) voiceWireEnvelope {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		var envelope voiceWireEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		if envelope.Type == wantType {
			return envelope
		}
	}
}

func synchronizeVoiceWireClient(t *testing.T, conn *gorillaWS.Conn) {
	t.Helper()
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "connection_ready_probe",
		"data": map[string]interface{}{"protocol_version": 2},
	}))
	waitForVoiceWireType(t, conn, "connection_ready")
}

func TestServerVoiceMissingLifecycleRejectsStaleCrossScopeEventBeforeClaim(t *testing.T) {
	for _, eventKind := range []string{"join", "heartbeat"} {
		t.Run(eventKind, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			sender := ts.CreateTestUser(t, "rp_server_durable_fence_"+eventKind)
			serverID := ts.CreateTestServer(t, sender.ID, "RP Durable Fence")
			staleChannelID := ts.CreateVoiceChannel(t, serverID, "rp-stale-scope")
			currentChannelID := ts.CreateVoiceChannel(t, serverID, "rp-current-scope")
			currentAt := time.Date(2026, 7, 16, 1, 3, 0, 0, time.UTC)
			staleAt := currentAt.Add(-time.Second)
			_, err := ts.DB.Exec(`
				INSERT INTO voice_participants
					(channel_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, currentChannelID, sender.ID, currentAt)
			require.NoError(t, err)
			lifecycleKey, err := presence.VoiceLifecycleKey(
				uuid.MustParse(sender.ID), presence.CategoryServerVoice,
			)
			require.NoError(t, err)
			require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val())

			if eventKind == "join" {
				sub.HandleJoined(mustJSON(t, map[string]interface{}{
					"channelId": staleChannelID,
					"userId":    sender.ID, "username": sender.Username,
					"timestamp": staleAt.Format(time.RFC3339Nano),
				}))
			} else {
				sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
					"channelId": staleChannelID,
					"userIds":   []string{sender.ID},
					"timestamp": staleAt.Format(time.RFC3339Nano),
				}))
			}

			require.True(t, voiceParticipantExists(
				t, ts.DB, currentChannelID, sender.ID,
			))
			require.False(t, voiceParticipantExists(
				t, ts.DB, staleChannelID, sender.ID,
			))
			require.Zero(t, ts.Redis.Exists(context.Background(), lifecycleKey).Val(),
				"the durable row must reject before recreating a stale lifecycle watermark")
		})
	}
}

func TestPrivateCallCrossScopeMoveRefreshesOldScopeImmediately(t *testing.T) {
	for _, eventKind := range []string{"join", "grouped heartbeat"} {
		t.Run(eventKind, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			name := strings.ReplaceAll(eventKind, " ", "_")
			mover := ts.CreateTestUser(t, "rp_old_scope_mover_"+name)
			oldPeer := ts.CreateTestUser(t, "rp_old_scope_peer_"+name)
			newPeer := ts.CreateTestUser(t, "rp_new_scope_peer_"+name)
			oldViewer := ts.CreateTestUser(t, "rp_old_scope_viewer_"+name)
			oldConversationID := uuid.MustParse(
				ts.CreateDMConversation(t, mover.ID, oldPeer.ID),
			)
			newConversationID := uuid.MustParse(
				ts.CreateDMConversation(t, mover.ID, newPeer.ID),
			)
			oldCallID := uuid.New()
			newCallID := uuid.New()
			oldAt := time.Date(2026, 7, 16, 1, 4, 0, 0, time.UTC)
			moveAt := oldAt.Add(time.Second)
			moverID := uuid.MustParse(mover.ID)
			oldPeerID := uuid.MustParse(oldPeer.ID)
			newPeerID := uuid.MustParse(newPeer.ID)

			_, err := ts.DB.Exec(`
				INSERT INTO user_presence_settings
					(user_id, master_enabled, private_call_tier,
					 private_call_show_details)
				VALUES ($1, TRUE, $2, TRUE)
			`, oldPeerID, presence.TierFriends)
			require.NoError(t, err)
			_, err = ts.DB.Exec(`
				INSERT INTO friendships (requester_id, addressee_id, status)
				VALUES ($1, $2, 'accepted')
			`, oldPeerID, oldViewer.ID)
			require.NoError(t, err)
			for _, lease := range []dm.VoiceCallLease{
				{
					ConversationID: oldConversationID,
					CallID:         oldCallID, CallerUserID: moverID,
				},
				{
					ConversationID: newConversationID,
					CallID:         newCallID, CallerUserID: moverID,
				},
			} {
				require.NoError(t, dm.RefreshDMVoiceCallLease(
					context.Background(), ts.Redis, lease, dm.DMVoiceCallLeaseTTL, true,
				))
			}
			_, err = ts.DB.Exec(`
				INSERT INTO dm_voice_participants
					(conversation_id, user_id, joined_at, lifecycle_event_at)
				VALUES
					($1, $3, $5, $5), ($1, $4, $5, $5),
					($2, $6, $5, $5)
			`, oldConversationID, newConversationID, moverID, oldPeerID, oldAt, newPeerID)
			require.NoError(t, err)
			seedPrivateVoiceGeneration(t, ts, moverID, oldCallID, oldAt)
			seedPrivateVoiceGeneration(t, ts, oldPeerID, oldCallID, oldAt)
			seedPrivateVoiceGeneration(t, ts, newPeerID, newCallID, oldAt)
			stored, err := presence.NewActivityStore(ts.Redis).CompareAndSetActive(
				context.Background(), oldPeerID, presence.CategoryPrivateCall,
				presence.ActivityState{
					SourceToken: oldCallID, SourceVersion: oldAt.UnixMicro(),
					Payload: json.RawMessage(
						`{"call_type":"dm","participant_count":2}`,
					),
					UpdatedAt: oldAt.Unix(),
				},
			)
			require.NoError(t, err)
			require.True(t, stored)

			moverConn := connectVoiceWireClient(t, ts, mover)
			oldViewerConn := connectVoiceWireClient(t, ts, oldViewer)
			synchronizeVoiceWireClient(t, moverConn)
			synchronizeVoiceWireClient(t, oldViewerConn)

			if eventKind == "join" {
				sub.HandleJoined(mustJSON(t, map[string]interface{}{
					"channelId": newConversationID.String(), "callId": newCallID.String(),
					"userId": mover.ID, "username": mover.Username,
					"timestamp": moveAt.Format(time.RFC3339Nano),
				}))
			} else {
				sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
					"channelId": newConversationID.String(), "callId": newCallID.String(),
					"callerUserId": mover.ID, "userIds": []string{mover.ID, newPeer.ID},
					"timestamp": moveAt.Format(time.RFC3339Nano),
				}))
			}

			var oldPeerLifecycleAt time.Time
			require.NoError(t, ts.DB.QueryRow(`
				SELECT lifecycle_event_at
				FROM dm_voice_participants
				WHERE conversation_id = $1 AND user_id = $2
			`, oldConversationID, oldPeerID).Scan(&oldPeerLifecycleAt))
			require.Equal(t, moveAt.UnixMicro(), oldPeerLifecycleAt.UnixMicro())
			lifecycleKey, err := presence.VoiceLifecycleKey(
				oldPeerID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			lifecycle, err := ts.Redis.HGetAll(context.Background(), lifecycleKey).Result()
			require.NoError(t, err)
			require.Equal(t, oldCallID.String(), lifecycle["token"])
			require.Equal(t, fmt.Sprintf("%d", moveAt.UnixMicro()), lifecycle["version"])
			require.Equal(t, "1", lifecycle["active"])

			state, found, err := presence.NewActivityStore(ts.Redis).Get(
				context.Background(), oldPeerID, presence.CategoryPrivateCall,
			)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, oldCallID, state.SourceToken)
			require.Equal(t, moveAt.UnixMicro(), state.SourceVersion,
				"the old-scope peer must be republished at its committed set revision")
			var payload presence.PrivateCallPayload
			require.NoError(t, json.Unmarshal(state.Payload, &payload))
			require.Equal(t, 1, payload.ParticipantCount)

			update := waitForVoiceWireType(t, oldViewerConn, "rich_presence_update")
			require.Equal(t, oldPeer.ID, update.Data["user_id"])
			updatePayload, ok := update.Data["payload"].(map[string]interface{})
			require.True(t, ok)
			require.Equal(t, float64(1), updatePayload["participant_count"])

			clearFrame := waitForVoiceWireType(t, moverConn, "rich_presence_clear")
			require.Equal(t, oldPeer.ID, clearFrame.Data["user_id"])
			require.Equal(t, string(presence.CategoryPrivateCall), clearFrame.Data["category"])
		})
	}
}

func TestVoiceLifecycle_ComposedServerRichPresenceWireJoinMoveLeave(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_wire_server_sender")
	oldViewer := ts.CreateTestUser(t, "rp_wire_server_old_viewer")
	newViewer := ts.CreateTestUser(t, "rp_wire_server_new_viewer")
	outsider := ts.CreateTestUser(t, "rp_wire_server_outsider")
	oldServerID := ts.CreateTestServer(t, sender.ID, "RP Wire Old Server")
	newServerID := ts.CreateTestServer(t, sender.ID, "RP Wire New Server")
	ts.AddMemberToServer(t, oldServerID, oldViewer.ID, "member")
	ts.AddMemberToServer(t, newServerID, newViewer.ID, "member")
	oldChannelID := ts.CreateVoiceChannel(t, oldServerID, "rp-wire-old")
	newChannelID := ts.CreateVoiceChannel(t, newServerID, "rp-wire-new")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)

	oldConn := connectVoiceWireClient(t, ts, oldViewer)
	newConn := connectVoiceWireClient(t, ts, newViewer)
	outsiderConn := connectVoiceWireClient(t, ts, outsider)
	for _, conn := range []*gorillaWS.Conn{oldConn, newConn, outsiderConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	joinedAt := time.Date(2026, 7, 16, 1, 0, 0, 123456000, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": oldChannelID, "userId": sender.ID,
		"username": sender.Username, "displayName": "Wire Sender",
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	oldUpdate := waitForVoiceWireType(t, oldConn, "rich_presence_update")
	require.Equal(t, sender.ID, oldUpdate.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), oldUpdate.Data["category"])
	require.Equal(t, false, oldUpdate.Data["minimized"])
	oldPayload, ok := oldUpdate.Data["payload"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, oldChannelID, oldPayload["channel_id"])
	require.Equal(t, oldServerID, oldPayload["server_id"])

	movedAt := joinedAt.Add(time.Microsecond)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": newChannelID, "userId": sender.ID,
		"username": sender.Username, "displayName": "Wire Sender",
		"timestamp": movedAt.Format(time.RFC3339Nano),
	}))
	oldClear := waitForVoiceWireType(t, oldConn, "rich_presence_clear")
	require.Equal(t, sender.ID, oldClear.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), oldClear.Data["category"])
	newUpdate := waitForVoiceWireType(t, newConn, "rich_presence_update")
	require.Equal(t, sender.ID, newUpdate.Data["user_id"])
	require.Equal(t, false, newUpdate.Data["minimized"])
	newPayload, ok := newUpdate.Data["payload"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, newChannelID, newPayload["channel_id"])
	require.Equal(t, newServerID, newPayload["server_id"])

	leftAt := movedAt.Add(time.Microsecond)
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": newChannelID, "userId": sender.ID,
		"timestamp": leftAt.Format(time.RFC3339Nano),
	}))
	newClear := waitForVoiceWireType(t, newConn, "rich_presence_clear")
	require.Equal(t, sender.ID, newClear.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), newClear.Data["category"])
	requireNoVoiceWireType(t, outsiderConn, "rich_presence_update")
}

// Regression for #2666: ownership is an authority input to channel visibility,
// so confirming a transfer must reconcile already-delivered Server Voice state.
func TestOwnershipTransfer_ReconcilesServerVoiceRecipients(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sender := ts.CreateTestUser(t, "rp_owner_sender")
	oldOwner := ts.CreateTestUser(t, "rp_owner_old")
	newOwner := ts.CreateTestUser(t, "rp_owner_new")
	retained := ts.CreateTestUser(t, "rp_owner_retained")
	serverID := ts.CreateTestServer(t, oldOwner.ID, "RP Ownership Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-owner")
	for _, user := range []testhelpers.TestUser{sender, newOwner, retained} {
		ts.AddMemberToServer(t, serverID, user.ID, "member")
	}
	ts.CreateChannelOverride(t, channelID, "user", newOwner.ID, 0,
		int64(rbac.PermViewVoiceChannels))
	ts.CreateChannelOverride(t, channelID, "user", oldOwner.ID, 0,
		int64(rbac.PermViewVoiceChannels))
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, FALSE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)

	oldConn := connectVoiceWireClient(t, ts, oldOwner)
	retainedConn := connectVoiceWireClient(t, ts, retained)
	senderConn := connectVoiceWireClient(t, ts, sender)
	for _, conn := range []*gorillaWS.Conn{oldConn, retainedConn, senderConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	sub := newTestSubscriber(ts)
	joinedAt := time.Date(2026, 8, 27, 12, 0, 0, 123456000, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": sender.ID, "username": sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	state, found, stateErr := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice)
	require.NoError(t, stateErr)
	require.True(t, found, "initial generation must exist before ownership reconciliation")
	require.Equal(t, uuid.MustParse(channelID), state.SourceToken)
	lifecycleKey, keyErr := presence.VoiceLifecycleKey(uuid.MustParse(sender.ID), presence.CategoryServerVoice)
	require.NoError(t, keyErr)
	lifecycle, lifecycleErr := ts.Redis.HGetAll(context.Background(), lifecycleKey).Result()
	require.NoError(t, lifecycleErr)
	require.Len(t, lifecycle, 3, "voice lifecycle watermark must have exactly token, version, and active fields")
	require.Equal(t, channelID, lifecycle["token"])
	require.Equal(t, strconv.FormatInt(joinedAt.UnixMicro(), 10), lifecycle["version"])
	require.Equal(t, "1", lifecycle["active"])
	pttl, pttlErr := ts.Redis.PTTL(context.Background(), lifecycleKey).Result()
	require.NoError(t, pttlErr)
	require.Greater(t, pttl, time.Duration(0))
	require.LessOrEqual(t, pttl, presence.ActivityStateTTL)
	waitForVoiceWireType(t, oldConn, "rich_presence_update")
	waitForVoiceWireType(t, retainedConn, "rich_presence_update")
	newConn := connectVoiceWireClient(t, ts, newOwner)
	synchronizeVoiceWireClient(t, newConn)

	initiate := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership",
		map[string]interface{}{"target_user_id": newOwner.ID, "password": testhelpers.TestAuthPlaintext},
		testhelpers.AuthHeaders(oldOwner.AccessToken))
	require.Equal(t, http.StatusCreated, initiate.Code)
	confirm := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm",
		nil, testhelpers.AuthHeaders(oldOwner.AccessToken))
	require.Equal(t, http.StatusOK, confirm.Code)
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test-ownership-repro"))
	oldVisible, visibilityErr := resolver.HasPermission(context.Background(), serverID, oldOwner.ID, channelID, rbac.PermViewVoiceChannels)
	require.NoError(t, visibilityErr)
	require.False(t, oldVisible, "fresh ownership visibility must revoke the outgoing owner's channel access")

	clearFrame, clearErr := receiveVoiceWireType(oldConn, sender.ID, "rich_presence_clear")
	require.NoError(t, clearErr, "ownership loss must clear the outgoing owner's view")
	require.Equal(t, sender.ID, clearFrame.Data["user_id"], "ownership loss must clear the outgoing owner's view")
	require.Equal(t, string(presence.CategoryServerVoice), clearFrame.Data["category"])
	update, updateErr := receiveVoiceWireType(newConn, sender.ID, "rich_presence_update")
	require.NoError(t, updateErr, "new owner must receive the current minimized update")
	require.Equal(t, sender.ID, update.Data["user_id"], "new owner must receive the current minimized update")
	require.Equal(t, string(presence.CategoryServerVoice), update.Data["category"])
	require.Equal(t, true, update.Data["minimized"], "new owner must receive a minimized current update")
	assertMinimalServerVoicePayload(t, update.Data["payload"], channelID, serverID, joinedAt)
	requireNoVoiceWireType(t, retainedConn, "rich_presence_clear")
}

// Reversal must produce the exact inverse delta: the restored owner receives
// the current state and the former owner is cleared; an unchanged viewer is
// never cleared.
func TestOwnershipReversal_ReconcilesInverseRecipients(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sender := ts.CreateTestUser(t, "rp_reverse_sender")
	oldOwner := ts.CreateTestUser(t, "rp_reverse_old")
	newOwner := ts.CreateTestUser(t, "rp_reverse_new")
	retained := ts.CreateTestUser(t, "rp_reverse_retained")
	serverID := ts.CreateTestServer(t, oldOwner.ID, "RP Reversal Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-reversal")
	for _, user := range []testhelpers.TestUser{sender, newOwner, retained} {
		ts.AddMemberToServer(t, serverID, user.ID, "member")
	}
	// Owner bypass is the only initial visibility for oldOwner/newOwner.
	ts.CreateChannelOverride(t, channelID, "user", oldOwner.ID, 0, int64(rbac.PermViewVoiceChannels))
	ts.CreateChannelOverride(t, channelID, "user", newOwner.ID, 0, int64(rbac.PermViewVoiceChannels))
	// Retained visibility is explicit and survives both ownership writes.
	ts.CreateChannelOverride(t, channelID, "user", retained.ID, int64(rbac.PermViewVoiceChannels), 0)
	_, err := ts.DB.Exec(`INSERT INTO user_presence_settings
		(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, FALSE)`, sender.ID, presence.TierServers)
	require.NoError(t, err)

	oldConn := connectVoiceWireClient(t, ts, oldOwner)
	newConn := connectVoiceWireClient(t, ts, newOwner)
	retainedConn := connectVoiceWireClient(t, ts, retained)
	senderConn := connectVoiceWireClient(t, ts, sender)
	for _, conn := range []*gorillaWS.Conn{oldConn, newConn, retainedConn, senderConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	sub := newTestSubscriber(ts)
	joinedAt := time.Date(2026, 8, 27, 12, 30, 0, 123456000, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": sender.ID, "username": sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	waitForVoiceWireType(t, oldConn, "rich_presence_update")
	waitForVoiceWireType(t, retainedConn, "rich_presence_update")

	initiate := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership",
		map[string]interface{}{"target_user_id": newOwner.ID, "password": testhelpers.TestAuthPlaintext},
		testhelpers.AuthHeaders(oldOwner.AccessToken))
	require.Equal(t, http.StatusCreated, initiate.Code)
	confirm := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil,
		testhelpers.AuthHeaders(oldOwner.AccessToken))
	require.Equal(t, http.StatusOK, confirm.Code)
	_, err = receiveVoiceWireType(oldConn, sender.ID, "rich_presence_clear")
	require.NoError(t, err)
	_, err = receiveVoiceWireType(newConn, sender.ID, "rich_presence_update")
	require.NoError(t, err)

	var reversalToken string
	require.NoError(t, ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken))
	reverse := ts.DoRequest("POST", "/api/v1/ownership/reverse/"+reversalToken,
		map[string]interface{}{"password": testhelpers.TestAuthPlaintext}, testhelpers.AuthHeaders(oldOwner.AccessToken))
	require.Equal(t, http.StatusOK, reverse.Code)
	clearFrame, clearErr := receiveVoiceWireType(newConn, sender.ID, "rich_presence_clear")
	require.NoError(t, clearErr)
	require.Equal(t, sender.ID, clearFrame.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), clearFrame.Data["category"])
	update, updateErr := receiveVoiceWireType(oldConn, sender.ID, "rich_presence_update")
	require.NoError(t, updateErr)
	require.Equal(t, sender.ID, update.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), update.Data["category"])
	require.Equal(t, true, update.Data["minimized"])
	assertMinimalServerVoicePayload(t, update.Data["payload"], channelID, serverID, joinedAt)
	requireNoVoiceWireType(t, retainedConn, "rich_presence_clear")
}

func TestExpiredOwnershipTransfer_ReconcilesServerVoiceRecipients(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sender := ts.CreateTestUser(t, "rp_expiry_sender")
	oldOwner := ts.CreateTestUser(t, "rp_expiry_old")
	newOwner := ts.CreateTestUser(t, "rp_expiry_new")
	retained := ts.CreateTestUser(t, "rp_expiry_retained")
	serverID := ts.CreateTestServer(t, oldOwner.ID, "RP Expiry Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-expiry")
	for _, user := range []testhelpers.TestUser{sender, newOwner, retained} {
		ts.AddMemberToServer(t, serverID, user.ID, "member")
	}
	ts.CreateChannelOverride(t, channelID, "user", newOwner.ID, 0, int64(rbac.PermViewVoiceChannels))
	ts.CreateChannelOverride(t, channelID, "user", oldOwner.ID, 0, int64(rbac.PermViewVoiceChannels))
	ts.CreateChannelOverride(t, channelID, "user", retained.ID, int64(rbac.PermViewVoiceChannels), 0)
	_, err := ts.DB.Exec(`INSERT INTO user_presence_settings (user_id, master_enabled, server_voice_tier, server_voice_show_details) VALUES ($1, TRUE, $2, FALSE)`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	oldConn := connectVoiceWireClient(t, ts, oldOwner)
	retainedConn := connectVoiceWireClient(t, ts, retained)
	senderConn := connectVoiceWireClient(t, ts, sender)
	for _, conn := range []*gorillaWS.Conn{oldConn, retainedConn, senderConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	sub := newTestSubscriber(ts)
	joinedAt := time.Date(2026, 8, 27, 13, 0, 0, 123456000, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{"channelId": channelID, "userId": sender.ID, "username": sender.Username, "timestamp": joinedAt.Format(time.RFC3339Nano)}))
	waitForVoiceWireType(t, oldConn, "rich_presence_update")
	waitForVoiceWireType(t, retainedConn, "rich_presence_update")
	newConn := connectVoiceWireClient(t, ts, newOwner)
	synchronizeVoiceWireClient(t, newConn)
	initiate := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{"target_user_id": newOwner.ID, "password": testhelpers.TestAuthPlaintext}, testhelpers.AuthHeaders(oldOwner.AccessToken))
	require.Equal(t, http.StatusCreated, initiate.Code)
	_, err = ts.DB.Exec(`UPDATE ownership_transfers SET expires_at = NOW() - INTERVAL '1 hour' WHERE server_id = $1 AND status = 'pending'`, serverID)
	require.NoError(t, err)
	require.NotNil(t, ts.CompleteExpiredTransfers)
	ts.CompleteExpiredTransfers(context.Background())
	clearFrame, clearErr := receiveVoiceWireType(oldConn, sender.ID, "rich_presence_clear")
	require.NoError(t, clearErr)
	require.Equal(t, sender.ID, clearFrame.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), clearFrame.Data["category"])
	update, updateErr := receiveVoiceWireType(newConn, sender.ID, "rich_presence_update")
	require.NoError(t, updateErr)
	require.Equal(t, sender.ID, update.Data["user_id"])
	require.Equal(t, string(presence.CategoryServerVoice), update.Data["category"])
	require.Equal(t, true, update.Data["minimized"])
	assertMinimalServerVoicePayload(t, update.Data["payload"], channelID, serverID, joinedAt)
	requireNoVoiceWireType(t, retainedConn, "rich_presence_clear")
}

func receiveVoiceWireType(conn *gorillaWS.Conn, senderID, wantType string) (voiceWireEnvelope, error) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return voiceWireEnvelope{}, err
	}
	for {
		var envelope voiceWireEnvelope
		if err := conn.ReadJSON(&envelope); err != nil {
			return voiceWireEnvelope{}, err
		}
		if envelope.Type == wantType {
			return envelope, nil
		}
		if (envelope.Type == "rich_presence_clear" || envelope.Type == "rich_presence_update") &&
			envelope.Data["user_id"] == senderID && envelope.Data["category"] == string(presence.CategoryServerVoice) {
			return voiceWireEnvelope{}, fmt.Errorf("unexpected %s for sender %s while waiting for %s", envelope.Type, senderID, wantType)
		}
	}
}

func assertMinimalServerVoicePayload(t *testing.T, raw interface{}, channelID, serverID string, joinedAt time.Time) {
	t.Helper()
	payload, ok := raw.(map[string]interface{})
	require.True(t, ok)
	require.Len(t, payload, 3)
	for _, key := range []string{"channel_id", "server_id", "started_at"} {
		require.Contains(t, payload, key)
	}
	require.Equal(t, channelID, payload["channel_id"])
	require.Equal(t, serverID, payload["server_id"])
	require.Equal(t, float64(joinedAt.Unix()), payload["started_at"])
	require.NotContains(t, payload, "channel_name")
	require.NotContains(t, payload, "server_name")
}

func TestVoiceLifecycle_ComposedPrivateRichPresenceWireJoinLeaveEnd(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_wire_dm_caller")
	peer := ts.CreateTestUser(t, "rp_wire_dm_peer")
	outsider := ts.CreateTestUser(t, "rp_wire_dm_outsider")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	callID := uuid.New()
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $2, FALSE)
	`, caller.ID, presence.TierOff)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $2, FALSE)
	`, peer.ID, presence.TierOff)
	require.NoError(t, err)
	callerJoinedAt := time.Date(2026, 7, 16, 1, 5, 0, 123456000, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": callerJoinedAt.Format(time.RFC3339Nano),
	}))

	callerConn := connectVoiceWireClient(t, ts, caller)
	peerConn := connectVoiceWireClient(t, ts, peer)
	outsiderConn := connectVoiceWireClient(t, ts, outsider)
	for _, conn := range []*gorillaWS.Conn{callerConn, peerConn, outsiderConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	peerJoinedAt := callerJoinedAt.Add(time.Microsecond)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": peer.ID, "username": peer.Username,
		"timestamp": peerJoinedAt.Format(time.RFC3339Nano),
	}))
	callerUpdate := waitForVoiceWireType(t, peerConn, "rich_presence_update")
	require.Equal(t, caller.ID, callerUpdate.Data["user_id"])
	require.Equal(t, string(presence.CategoryPrivateCall), callerUpdate.Data["category"])
	require.Equal(t, true, callerUpdate.Data["minimized"])
	callerPayload, ok := callerUpdate.Data["payload"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "dm", callerPayload["call_type"])
	require.NotContains(t, callerPayload, "participant_count")
	peerUpdate := waitForVoiceWireType(t, callerConn, "rich_presence_update")
	require.Equal(t, peer.ID, peerUpdate.Data["user_id"])
	require.Equal(t, true, peerUpdate.Data["minimized"])
	requireNoVoiceWireType(t, outsiderConn, "rich_presence_update")

	callerLeftAt := peerJoinedAt.Add(time.Microsecond)
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": caller.ID, "timestamp": callerLeftAt.Format(time.RFC3339Nano),
	}))
	callerClear := waitForVoiceWireType(t, peerConn, "rich_presence_clear")
	require.Equal(t, caller.ID, callerClear.Data["user_id"])
	require.Equal(t, string(presence.CategoryPrivateCall), callerClear.Data["category"])

	endedAt := callerLeftAt.Add(time.Microsecond)
	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"ringId": callID.String(), "callerUserId": caller.ID,
		"participantUserIds": []string{caller.ID, peer.ID},
		"startedAt":          callerJoinedAt.Format(time.RFC3339Nano),
		"timestamp":          endedAt.Format(time.RFC3339Nano),
	}))
	require.Equal(t, 0, countDMVoiceParticipants(t, ts.DB, conversationID))
	for _, participant := range []testhelpers.TestUser{caller, peer} {
		state, found, getErr := presence.NewActivityStore(ts.Redis).Get(
			context.Background(), uuid.MustParse(participant.ID), presence.CategoryPrivateCall,
		)
		require.NoError(t, getErr)
		require.Falsef(t, found, "terminal retained participant %s state %+v", participant.ID, state)
	}
}

func TestVoiceLifecycle_TwoReplicaHubsConvergeServerMoveAndTerminal(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winner := newTestSubscriber(ts)
	replicaHub, replicaURL := newVoiceReplicaHub(t, ts)
	loser := newTestSubscriberWithHub(ts, replicaHub)
	sender := ts.CreateTestUser(t, "rp_replica_server_sender")
	oldViewer := ts.CreateTestUser(t, "rp_replica_server_old_viewer")
	newViewer := ts.CreateTestUser(t, "rp_replica_server_new_viewer")
	oldServerID := ts.CreateTestServer(t, sender.ID, "RP Replica Old")
	newServerID := ts.CreateTestServer(t, sender.ID, "RP Replica New")
	ts.AddMemberToServer(t, oldServerID, oldViewer.ID, "member")
	ts.AddMemberToServer(t, newServerID, newViewer.ID, "member")
	oldChannelID := ts.CreateVoiceChannel(t, oldServerID, "rp-replica-old")
	newChannelID := ts.CreateVoiceChannel(t, newServerID, "rp-replica-new")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)

	oldConn := connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, oldViewer)
	newConn := connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, newViewer)
	for _, conn := range []*gorillaWS.Conn{oldConn, newConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	joinedAt := time.Date(2026, 7, 16, 1, 10, 0, 123456000, time.UTC)
	joinedOld := mustJSON(t, map[string]interface{}{
		"channelId": oldChannelID, "userId": sender.ID,
		"username": sender.Username, "timestamp": joinedAt.Format(time.RFC3339Nano),
	})
	winner.HandleJoined(joinedOld)
	loser.HandleJoined(joinedOld)
	oldUpdate := waitForVoiceWireType(t, oldConn, "rich_presence_update")
	require.Equal(t, sender.ID, oldUpdate.Data["user_id"])

	movedAt := joinedAt.Add(time.Microsecond)
	joinedNew := mustJSON(t, map[string]interface{}{
		"channelId": newChannelID, "userId": sender.ID,
		"username": sender.Username, "timestamp": movedAt.Format(time.RFC3339Nano),
	})
	winner.HandleJoined(joinedNew)
	loser.HandleJoined(joinedNew)
	require.Eventually(t, func() bool {
		return replicaHub.GetUserClientCount(uuid.MustParse(oldViewer.ID)) == 0 &&
			replicaHub.GetUserClientCount(uuid.MustParse(newViewer.ID)) == 0
	}, time.Second, 10*time.Millisecond,
		"the losing replica must reconnect its unknown old audience after replaying a move")

	reconnectedNew := connectVoiceWireClientAtURL(
		t, ts.Redis, replicaHub, replicaURL, newViewer,
	)
	newSnapshot := waitForVoiceWireType(t, reconnectedNew, "presence_snapshot")
	replayedNew, found := voiceSnapshotRichPresenceEntry(
		t, newSnapshot, sender.ID, presence.CategoryServerVoice,
	)
	require.True(t, found)
	replayedPayload, ok := replayedNew["payload"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, newChannelID, replayedPayload["channel_id"])
	reconnectedOld := connectVoiceWireClientAtURL(
		t, ts.Redis, replicaHub, replicaURL, oldViewer,
	)
	oldSnapshot := waitForVoiceWireType(t, reconnectedOld, "presence_snapshot")
	_, found = voiceSnapshotRichPresenceEntry(
		t, oldSnapshot, sender.ID, presence.CategoryServerVoice,
	)
	require.False(t, found)

	leftAt := movedAt.Add(time.Microsecond)
	left := mustJSON(t, map[string]interface{}{
		"channelId": newChannelID, "userId": sender.ID,
		"timestamp": leftAt.Format(time.RFC3339Nano),
	})
	winner.HandleLeft(left)
	loser.HandleLeft(left)
	require.Eventually(t, func() bool {
		return replicaHub.GetUserClientCount(uuid.MustParse(newViewer.ID)) == 0
	}, time.Second, 10*time.Millisecond,
		"an exact terminal duplicate must converge the losing replica locally")
	terminalReconnect := connectVoiceWireClientAtURL(
		t, ts.Redis, replicaHub, replicaURL, newViewer,
	)
	terminalSnapshot := waitForVoiceWireType(t, terminalReconnect, "presence_snapshot")
	_, found = voiceSnapshotRichPresenceEntry(
		t, terminalSnapshot, sender.ID, presence.CategoryServerVoice,
	)
	require.False(t, found)
}

func TestVoiceLifecycle_TwoReplicaHubsConvergePrivateRoomTerminal(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winner := newTestSubscriber(ts)
	replicaHub, replicaURL := newVoiceReplicaHub(t, ts)
	loser := newTestSubscriberWithHub(ts, replicaHub)
	caller := ts.CreateTestUser(t, "rp_replica_dm_caller")
	peer := ts.CreateTestUser(t, "rp_replica_dm_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	callID := uuid.New()
	for _, participant := range []testhelpers.TestUser{caller, peer} {
		_, err := ts.DB.Exec(`
			INSERT INTO user_presence_settings
				(user_id, master_enabled, private_call_tier, private_call_show_details)
			VALUES ($1, TRUE, $2, FALSE)
		`, participant.ID, presence.TierOff)
		require.NoError(t, err)
	}
	callerAt := time.Date(2026, 7, 16, 1, 15, 0, 123456000, time.UTC)
	callerJoined := mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": callerAt.Format(time.RFC3339Nano),
	})
	winner.HandleJoined(callerJoined)
	loser.HandleJoined(callerJoined)

	callerConn := connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, caller)
	peerConn := connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, peer)
	for _, conn := range []*gorillaWS.Conn{callerConn, peerConn} {
		synchronizeVoiceWireClient(t, conn)
	}
	peerAt := callerAt.Add(time.Microsecond)
	peerJoined := mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": peer.ID, "username": peer.Username,
		"timestamp": peerAt.Format(time.RFC3339Nano),
	})
	loser.HandleJoined(peerJoined)
	callerUpdate := waitForVoiceWireType(t, peerConn, "rich_presence_update")
	require.Equal(t, caller.ID, callerUpdate.Data["user_id"])
	peerUpdate := waitForVoiceWireType(t, callerConn, "rich_presence_update")
	require.Equal(t, peer.ID, peerUpdate.Data["user_id"])
	winner.HandleJoined(peerJoined)

	endedAt := peerAt.Add(time.Microsecond)
	roomEmpty := mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"ringId": callID.String(), "callerUserId": caller.ID,
		"participantUserIds": []string{caller.ID, peer.ID},
		"startedAt":          callerAt.Format(time.RFC3339Nano),
		"timestamp":          endedAt.Format(time.RFC3339Nano),
	})
	winner.HandleRoomEmpty(roomEmpty)
	loser.HandleRoomEmpty(roomEmpty)
	require.Eventually(t, func() bool {
		return replicaHub.GetUserClientCount(uuid.MustParse(caller.ID)) == 0 &&
			replicaHub.GetUserClientCount(uuid.MustParse(peer.ID)) == 0
	}, time.Second, 10*time.Millisecond,
		"the replica that finds terminal rows already removed must reconnect local clients")
	require.Equal(t, 0, countDMVoiceParticipants(t, ts.DB, conversationID))

	reconnectedPeer := connectVoiceWireClientAtURL(
		t, ts.Redis, replicaHub, replicaURL, peer,
	)
	snapshot := waitForVoiceWireType(t, reconnectedPeer, "presence_snapshot")
	for _, senderID := range []string{caller.ID, peer.ID} {
		_, found := voiceSnapshotRichPresenceEntry(
			t, snapshot, senderID, presence.CategoryPrivateCall,
		)
		require.False(t, found)
	}
}

func TestVoiceLifecycle_TwoReplicaHubsConvergeServerRoomEmpty(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winner := newTestSubscriber(ts)
	replicaHub, replicaURL := newVoiceReplicaHub(t, ts)
	loser := newTestSubscriberWithHub(ts, replicaHub)
	sender := ts.CreateTestUser(t, "rp_replica_empty_sender")
	viewer := ts.CreateTestUser(t, "rp_replica_empty_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Replica Empty")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-replica-empty")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	conn := connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, viewer)
	synchronizeVoiceWireClient(t, conn)
	joinedAt := time.Date(2026, 7, 16, 1, 20, 0, 123456000, time.UTC)
	joined := mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": sender.ID,
		"username": sender.Username, "timestamp": joinedAt.Format(time.RFC3339Nano),
	})
	winner.HandleJoined(joined)
	loser.HandleJoined(joined)
	waitForVoiceWireType(t, conn, "rich_presence_update")
	roomEmpty := mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"timestamp": joinedAt.Add(time.Microsecond).Format(time.RFC3339Nano),
	})
	winner.HandleRoomEmpty(roomEmpty)
	loser.HandleRoomEmpty(roomEmpty)
	require.Eventually(t, func() bool {
		return replicaHub.GetUserClientCount(uuid.MustParse(viewer.ID)) == 0
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, 0, countVoiceParticipants(t, ts.DB, channelID))
	reconnected := connectVoiceWireClientAtURL(t, ts.Redis, replicaHub, replicaURL, viewer)
	snapshot := waitForVoiceWireType(t, reconnected, "presence_snapshot")
	_, found := voiceSnapshotRichPresenceEntry(
		t, snapshot, sender.ID, presence.CategoryServerVoice,
	)
	require.False(t, found)
}

func requireNoVoiceWireType(
	t *testing.T,
	conn *gorillaWS.Conn,
	wantType string,
) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	for {
		var envelope voiceWireEnvelope
		err := conn.ReadJSON(&envelope)
		if err == nil {
			require.NotEqual(t, wantType, envelope.Type)
			continue
		}
		var networkErr net.Error
		require.True(t, errors.As(err, &networkErr) && networkErr.Timeout(),
			"expected read timeout while proving %s non-delivery, got %v", wantType, err)
		return
	}
}

func voiceSnapshotRichPresenceEntry(
	t *testing.T,
	envelope voiceWireEnvelope,
	senderID string,
	category presence.Category,
) (map[string]interface{}, bool) {
	t.Helper()
	require.Equal(t, "presence_snapshot", envelope.Type)
	users, ok := envelope.Data["users"].([]interface{})
	require.True(t, ok)
	for _, rawUser := range users {
		user, userOK := rawUser.(map[string]interface{})
		if !userOK || user["user_id"] != senderID {
			continue
		}
		richPresence, richOK := user["rich_presence"].(map[string]interface{})
		if !richOK {
			return nil, false
		}
		entry, found := richPresence[string(category)].(map[string]interface{})
		return entry, found
	}
	return nil, false
}

func TestHandleJoined_ServerPreservesStartAndAdvancesLifecycleWatermark(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	user := ts.CreateTestUser(t, "rp_watermark_join")
	serverID := ts.CreateTestServer(t, user.ID, "RP Watermark Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-watermark")
	joinedAt := time.Date(2026, 7, 15, 12, 0, 0, 123456000, time.UTC)
	refreshedAt := joinedAt.Add(30 * time.Second)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    user.ID,
		"username":  user.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    user.ID,
		"username":  user.Username,
		"timestamp": refreshedAt.Format(time.RFC3339Nano),
	}))

	var gotJoinedAt, gotLifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT joined_at, lifecycle_event_at
		FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, user.ID).Scan(&gotJoinedAt, &gotLifecycleAt))
	require.Equal(t, joinedAt, gotJoinedAt)
	require.Equal(t, refreshedAt, gotLifecycleAt)
}

func TestHandleJoined_FailsClosedWithoutRichPresenceDependency(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := voice.NewNATSSubscriber(
		ts.DB, logger.New("test"), ts.Hub, nil, ts.Redis, nil, nil,
	)

	sender := ts.CreateTestUser(t, "rp_missing_dependency_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Missing Dependency Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-missing-dependency")
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": time.Date(2026, 7, 15, 11, 30, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}))

	require.False(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
}

func TestHandleJoined_ServerPersistsAuthoritativeRichPresence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sender := ts.CreateTestUser(t, "rp_join_sender")
	viewer := ts.CreateTestUser(t, "rp_join_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Join Server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-join")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	eventAt := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": eventAt.Format(time.RFC3339Nano),
	}))

	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, uuid.MustParse(channelID), state.SourceToken)
	require.Equal(t, eventAt.UnixMicro(), state.SourceVersion)
}

func TestHandleLeft_ServerStaleEventPreservesNewerParticipantAndActivity(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sender := ts.CreateTestUser(t, "rp_stale_leave_sender")
	viewer := ts.CreateTestUser(t, "rp_stale_leave_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Stale Leave Server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-stale-leave")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	joinedAt := time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"timestamp": joinedAt.Add(-time.Second).Format(time.RFC3339Nano),
	}))

	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, joinedAt.UnixMicro(), state.SourceVersion)
}

func TestHandleJoined_PrivateCallPreservesStartAndAdvancesLifecycleWatermark(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_dm_watermark_caller")
	callee := ts.CreateTestUser(t, "rp_dm_watermark_callee")
	conversationID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 14, 0, 0, 654321000, time.UTC)
	refreshedAt := joinedAt.Add(30 * time.Second)

	for _, eventAt := range []time.Time{joinedAt, refreshedAt} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID,
			"callId":    callID.String(),
			"userId":    caller.ID,
			"username":  caller.Username,
			"timestamp": eventAt.Format(time.RFC3339Nano),
		}))
	}

	var gotJoinedAt, gotLifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT joined_at, lifecycle_event_at
		FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, caller.ID).Scan(&gotJoinedAt, &gotLifecycleAt))
	require.Equal(t, joinedAt, gotJoinedAt)
	require.Equal(t, refreshedAt, gotLifecycleAt)
}

func TestHandleJoined_PrivateCallRefreshesEveryCurrentParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_dm_join_caller")
	callee := ts.CreateTestUser(t, "rp_dm_join_callee")
	conversationID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $3, TRUE), ($2, TRUE, $3, TRUE)
	`, caller.ID, callee.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC)
	for index, user := range []struct {
		id       string
		username string
	}{{caller.ID, caller.Username}, {callee.ID, callee.Username}} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID,
			"callId":    callID.String(),
			"userId":    user.id,
			"username":  user.username,
			"timestamp": joinedAt.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		}))
	}

	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(caller.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	var payload presence.PrivateCallPayload
	require.NoError(t, json.Unmarshal(state.Payload, &payload))
	require.Equal(t, 2, payload.ParticipantCount)
}

func TestHandleLeft_PrivateCallStaleEventPreservesNewerParticipantAndActivity(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_dm_stale_leave_caller")
	callee := ts.CreateTestUser(t, "rp_dm_stale_leave_callee")
	conversationID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $3, TRUE), ($2, TRUE, $3, TRUE)
	`, caller.ID, callee.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	for index, user := range []struct {
		id       string
		username string
	}{{caller.ID, caller.Username}, {callee.ID, callee.Username}} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID,
			"callId":    callID.String(),
			"userId":    user.id,
			"username":  user.username,
			"timestamp": joinedAt.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		}))
	}

	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID,
		"callId":    callID.String(),
		"userId":    caller.ID,
		"timestamp": joinedAt.Add(-time.Second).Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))
	_, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(caller.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
}

func TestHandleHeartbeat_ServerAdvancesActiveWatermarkAndActivity(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sender := ts.CreateTestUser(t, "rp_hb_sender")
	viewer := ts.CreateTestUser(t, "rp_hb_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Heartbeat Server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-heartbeat")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	joinedAt := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	heartbeatAt := joinedAt.Add(30 * time.Second)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{sender.ID},
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	}))

	var gotJoinedAt, gotLifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT joined_at, lifecycle_event_at
		FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, sender.ID).Scan(&gotJoinedAt, &gotLifecycleAt))
	require.Equal(t, joinedAt, gotJoinedAt)
	require.Equal(t, heartbeatAt, gotLifecycleAt)
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, heartbeatAt.UnixMicro(), state.SourceVersion)
}

func TestHandleHeartbeat_ServerStaleOmissionPreservesNewerParticipantAndActivity(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sender := ts.CreateTestUser(t, "rp_hb_stale_sender")
	viewer := ts.CreateTestUser(t, "rp_hb_stale_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Stale Heartbeat Server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-stale-heartbeat")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	joinedAt := time.Date(2026, 7, 15, 16, 30, 0, 0, time.UTC)

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{},
		"timestamp": joinedAt.Add(-time.Second).Format(time.RFC3339Nano),
	}))

	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, joinedAt.UnixMicro(), state.SourceVersion)
}

func TestHandleHeartbeat_ServerDeduplicates256RawParticipants(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sender := ts.CreateTestUser(t, "rp_hb_raw_overflow_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Heartbeat Raw Overflow")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-heartbeat-raw-overflow")
	joinedAt := time.Date(2026, 7, 15, 16, 45, 0, 0, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))

	// Server rooms support more participants than the bounded Private Call path.
	// A 256-entry server heartbeat must therefore reach parsing and safely
	// deduplicate repeated IDs instead of being rejected by the private limit.
	userIDs := make([]string, 256)
	for index := range userIDs {
		userIDs[index] = sender.ID
	}
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   userIDs,
		"timestamp": joinedAt.Add(time.Minute).Format(time.RFC3339Nano),
	}))

	var lifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT lifecycle_event_at
		FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, sender.ID).Scan(&lifecycleAt))
	require.Equal(t, joinedAt.Add(time.Minute), lifecycleAt)
}

func TestHandleHeartbeat_ServerAccepts256Participants(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "rp_hb_db_overflow_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "RP Heartbeat DB Overflow")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-heartbeat-db-overflow")
	rowAt := time.Date(2026, 7, 15, 16, 50, 0, 0, time.UTC)
	prefix := "rpovf_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	_, err := ts.DB.Exec(`
		INSERT INTO users
			(id, email, username, password_hash, age_verified, email_verified)
		SELECT
			gen_random_uuid(),
			$1 || '_' || series || '@test.concord.chat',
			$1 || '_' || series,
			'overflow-test-hash', TRUE, TRUE
		FROM generate_series(1, 256) AS series
	`, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO server_members (server_id, user_id)
		SELECT $1, id
		FROM users
		WHERE LEFT(username, LENGTH($2)) = $2
	`, serverID, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		SELECT $1, id, $3, $3
		FROM users
		WHERE LEFT(username, LENGTH($2)) = $2
	`, channelID, prefix, rowAt)
	require.NoError(t, err)
	rows, err := ts.DB.Query(`
		SELECT user_id::text
		FROM voice_participants
		WHERE channel_id = $1
		ORDER BY user_id
	`, channelID)
	require.NoError(t, err)
	userIDs := make([]string, 0, 256)
	for rows.Next() {
		var userID string
		require.NoError(t, rows.Scan(&userID))
		userIDs = append(userIDs, userID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, userIDs, 256)

	heartbeatAt := rowAt.Add(time.Minute)
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   userIDs,
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	}))

	var participantCount, refreshedCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE lifecycle_event_at = $2)
		FROM voice_participants
		WHERE channel_id = $1
	`, channelID, heartbeatAt).Scan(&participantCount, &refreshedCount))
	require.Equal(t, 256, participantCount)
	require.Equal(t, 256, refreshedCount,
		"every admitted server participant must advance on the authoritative heartbeat")
}

func TestHandleHeartbeat_ServerReconcilesFullStaleSetAt1000ParticipantLimit(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "rp_hb_server_limit_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "RP Heartbeat Server Limit")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-heartbeat-server-limit")
	rowAt := time.Date(2026, 7, 15, 16, 51, 0, 0, time.UTC)
	prefix := "rplim_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	_, err := ts.DB.Exec(`
		INSERT INTO users
			(id, email, username, password_hash, age_verified, email_verified)
		SELECT
			gen_random_uuid(),
			$1 || '_' || series || '@test.concord.chat',
			$1 || '_' || series,
			'limit-test-hash', TRUE, TRUE
		FROM generate_series(1, 2000) AS series
	`, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO server_members (server_id, user_id)
		SELECT $1, id
		FROM users
		WHERE LEFT(username, LENGTH($2)) = $2
	`, serverID, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		SELECT $1, id, $3, $3
		FROM users
		WHERE LEFT(username, LENGTH($2)) = $2
		ORDER BY id
		LIMIT 1000
	`, channelID, prefix, rowAt)
	require.NoError(t, err)

	rows, err := ts.DB.Query(`
		SELECT id::text
		FROM users
		WHERE LEFT(username, LENGTH($1)) = $1
		ORDER BY id
	`, prefix)
	require.NoError(t, err)
	allUserIDs := make([]string, 0, 2000)
	for rows.Next() {
		var userID string
		require.NoError(t, rows.Scan(&userID))
		allUserIDs = append(allUserIDs, userID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, allUserIDs, 2000)
	staleUserID := allUserIDs[0]
	replacementUserID := allUserIDs[1000]
	mediaUserIDs := allUserIDs[1000:]
	deferredJoinAt := rowAt.Add(30 * time.Second)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    replacementUserID,
		"username":  prefix + "_replacement",
		"timestamp": deferredJoinAt.Format(time.RFC3339Nano),
	}))
	var participantCountAfterDeferredJoin int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*)
		FROM voice_participants
		WHERE channel_id = $1
	`, channelID).Scan(&participantCountAfterDeferredJoin))
	require.Equal(t, 1000, participantCountAfterDeferredJoin,
		"a joined event must defer a new row while stale rows occupy the room ceiling")
	require.False(t, voiceParticipantExists(t, ts.DB, channelID, replacementUserID))
	var firstReplacementOnce sync.Once
	var firstReplacementObserved bool
	var participantCountBeforeFirstReplacement int
	var firstReplacementProbeErr error
	sub.SetServerVoiceScopeObservedHookForTest(func(
		_ uuid.UUID,
		_ uuid.UUID,
		_ time.Time,
	) {
		firstReplacementOnce.Do(func() {
			firstReplacementObserved = true
			firstReplacementProbeErr = ts.DB.QueryRow(`
				SELECT COUNT(*)
				FROM voice_participants
				WHERE channel_id = $1
			`, channelID).Scan(&participantCountBeforeFirstReplacement)
		})
	})

	var participantCount, refreshedCount int
	heartbeatAt := rowAt.Add(time.Minute)
	// Retry one logical heartbeat so deadline-limited partial work accumulates.
	for range 15 {
		sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
			"channelId": channelID,
			"userIds":   mediaUserIDs,
			"timestamp": heartbeatAt.Format(time.RFC3339Nano),
		}))
		require.NoError(t, ts.DB.QueryRow(`
			SELECT COUNT(*), COUNT(*) FILTER (WHERE lifecycle_event_at = $2)
			FROM voice_participants
			WHERE channel_id = $1
		`, channelID, heartbeatAt).Scan(&participantCount, &refreshedCount))
		require.LessOrEqual(t, participantCount, 1000,
			"an interrupted heartbeat must never leave an over-cap persisted set")
		if participantCount == 1000 && refreshedCount == 1000 {
			break
		}
	}
	require.True(t, firstReplacementObserved)
	require.NoError(t, firstReplacementProbeErr)
	require.Zero(t, participantCountBeforeFirstReplacement,
		"a full stale set must drain before any authoritative replacement is admitted")
	require.Equal(t, 1000, participantCount)
	require.Equal(t, 1000, refreshedCount)
	require.False(t, voiceParticipantExists(t, ts.DB, channelID, staleUserID))
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, replacementUserID),
		"a full room must admit the authoritative replacement after clearing stale state")
}

func TestHandleHeartbeat_PrivateCallRejects256RawParticipants(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_hb_private_overflow_caller")
	peer := ts.CreateTestUser(t, "rp_hb_private_overflow_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	callID := uuid.New()
	rowAt := time.Date(2026, 7, 15, 16, 52, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, conversationID, caller.ID, rowAt)
	require.NoError(t, err)

	userIDs := make([]string, 256)
	for index := range userIDs {
		userIDs[index] = caller.ID
	}
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    conversationID,
		"callId":       callID.String(),
		"callerUserId": caller.ID,
		"userIds":      userIDs,
		"timestamp":    rowAt.Add(time.Minute).Format(time.RFC3339Nano),
	}))

	var lifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT lifecycle_event_at
		FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, caller.ID).Scan(&lifecycleAt))
	require.Equal(t, rowAt, lifecycleAt,
		"the Private Call participant bound must remain fail-closed")
}

func TestHandleRoomEmpty_RejectsParticipantSummaryOverflowBeforeMutation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_room_empty_overflow_caller")
	peer := ts.CreateTestUser(t, "rp_room_empty_overflow_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 16, 55, 0, 0, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID,
		"callId":    callID.String(),
		"userId":    caller.ID,
		"username":  caller.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))
	participantIDs := make([]string, 256)
	for index := range participantIDs {
		participantIDs[index] = caller.ID
	}

	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          conversationID,
		"callId":             callID.String(),
		"callerUserId":       caller.ID,
		"participantUserIds": participantIDs,
		"startedAt":          joinedAt.Format(time.RFC3339Nano),
		"timestamp":          joinedAt.Add(time.Minute).Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))
	var completedCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*)
		FROM dm_messages
		WHERE conversation_id = $1 AND id = $2 AND type = 'call_event'
	`, conversationID, callID).Scan(&completedCount))
	require.Zero(t, completedCount)
}

func TestHandleHeartbeat_PrivateCallReconcilesAllParticipantActivity(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_dm_hb_caller")
	callee := ts.CreateTestUser(t, "rp_dm_hb_callee")
	departed := ts.CreateTestUser(t, "rp_dm_hb_departed")
	conversationID := ts.CreateGroupDMConversation(t, caller.ID, callee.ID, departed.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $4, TRUE), ($2, TRUE, $4, TRUE), ($3, TRUE, $4, TRUE)
	`, caller.ID, callee.ID, departed.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC)
	for index, user := range []struct {
		id       string
		username string
	}{{caller.ID, caller.Username}, {callee.ID, callee.Username}, {departed.ID, departed.Username}} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID,
			"callId":    callID.String(),
			"userId":    user.id,
			"username":  user.username,
			"timestamp": joinedAt.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		}))
	}
	heartbeatAt := joinedAt.Add(30 * time.Second)
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID,
		"callId":    callID.String(),
		"userIds":   []string{caller.ID, callee.ID},
		"timestamp": heartbeatAt.Format(time.RFC3339Nano),
	}))

	var gotJoinedAt, gotLifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT joined_at, lifecycle_event_at
		FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, caller.ID).Scan(&gotJoinedAt, &gotLifecycleAt))
	require.Equal(t, joinedAt, gotJoinedAt)
	require.Equal(t, heartbeatAt, gotLifecycleAt)
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID, departed.ID))

	store := presence.NewActivityStore(ts.Redis)
	_, departedFound, err := store.Get(
		context.Background(), uuid.MustParse(departed.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.False(t, departedFound)
	callerState, callerFound, err := store.Get(
		context.Background(), uuid.MustParse(caller.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, callerFound)
	require.Equal(t, heartbeatAt.UnixMicro(), callerState.SourceVersion)
	var payload presence.PrivateCallPayload
	require.NoError(t, json.Unmarshal(callerState.Payload, &payload))
	require.Equal(t, 2, payload.ParticipantCount)
}

func TestHandleHeartbeat_PrivateCallStaleOmissionPreservesNewerParticipant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_dm_stale_hb_caller")
	callee := ts.CreateTestUser(t, "rp_dm_stale_hb_callee")
	conversationID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $3, TRUE), ($2, TRUE, $3, TRUE)
	`, caller.ID, callee.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 17, 30, 0, 0, time.UTC)
	for index, user := range []struct {
		id       string
		username string
	}{{caller.ID, caller.Username}, {callee.ID, callee.Username}} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID,
			"callId":    callID.String(),
			"userId":    user.id,
			"username":  user.username,
			"timestamp": joinedAt.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		}))
	}

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID,
		"callId":    callID.String(),
		"userIds":   []string{caller.ID},
		"timestamp": joinedAt.Add(-time.Second).Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID, callee.ID))
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(callee.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, joinedAt.Add(time.Second).UnixMicro(), state.SourceVersion)
}

func TestHandleRoomEmpty_PrivateCallClearsActivityBeforeLeaseCleanup(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	caller := ts.CreateTestUser(t, "rp_dm_terminal_caller")
	callee := ts.CreateTestUser(t, "rp_dm_terminal_callee")
	conversationID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $3, TRUE), ($2, TRUE, $3, TRUE)
	`, caller.ID, callee.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	startedAt := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	for index, user := range []struct {
		id       string
		username string
	}{{caller.ID, caller.Username}, {callee.ID, callee.Username}} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID,
			"callId":    callID.String(),
			"userId":    user.id,
			"username":  user.username,
			"timestamp": startedAt.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		}))
	}
	store := presence.NewActivityStore(ts.Redis)
	for _, userID := range []string{caller.ID, callee.ID} {
		_, found, getErr := store.Get(
			context.Background(), uuid.MustParse(userID), presence.CategoryPrivateCall,
		)
		require.NoError(t, getErr)
		require.True(t, found)
	}

	endedAt := startedAt.Add(time.Minute)
	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId":          conversationID,
		"callId":             callID.String(),
		"callerUserId":       caller.ID,
		"participantUserIds": []string{caller.ID, callee.ID},
		"startedAt":          startedAt.Format(time.RFC3339Nano),
		"timestamp":          endedAt.Format(time.RFC3339Nano),
	}))

	require.Zero(t, countDMVoiceParticipants(t, ts.DB, conversationID))
	for _, userID := range []string{caller.ID, callee.ID} {
		_, found, getErr := store.Get(
			context.Background(), uuid.MustParse(userID), presence.CategoryPrivateCall,
		)
		require.NoError(t, getErr)
		require.False(t, found)
	}
	_, found, err := dm.LookupDMVoiceCallLease(
		context.Background(), ts.Redis, uuid.MustParse(conversationID),
	)
	require.NoError(t, err)
	require.False(t, found)
}

func TestHandleRoomEmpty_ServerStaleEventPreservesNewerParticipantAndActivity(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	sender := ts.CreateTestUser(t, "rp_server_terminal_sender")
	viewer := ts.CreateTestUser(t, "rp_server_terminal_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Terminal Server")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-terminal")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	joinedAt := time.Date(2026, 7, 15, 19, 0, 0, 0, time.UTC)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userId":    sender.ID,
		"username":  sender.Username,
		"timestamp": joinedAt.Format(time.RFC3339Nano),
	}))

	sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"timestamp": joinedAt.Add(-time.Second).Format(time.RFC3339Nano),
	}))

	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, joinedAt.UnixMicro(), state.SourceVersion)
}

func TestHandleJoined_ServerMoveConvergesForEitherArrivalOrder(t *testing.T) {
	for _, test := range []struct {
		name       string
		leftBefore bool
	}{
		{name: "join before delayed left"},
		{name: "left before join", leftBefore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			sender := ts.CreateTestUser(t, "rp_move_sender_"+test.name[:4])
			oldViewer := ts.CreateTestUser(t, "rp_move_old_"+test.name[:4])
			newViewer := ts.CreateTestUser(t, "rp_move_new_"+test.name[:4])
			oldServerID := ts.CreateTestServer(t, sender.ID, "RP Move Old")
			newServerID := ts.CreateTestServer(t, sender.ID, "RP Move New")
			ts.AddMemberToServer(t, oldServerID, oldViewer.ID, "member")
			ts.AddMemberToServer(t, newServerID, newViewer.ID, "member")
			oldChannelID := ts.CreateVoiceChannel(t, oldServerID, "rp-move-old")
			newChannelID := ts.CreateVoiceChannel(t, newServerID, "rp-move-new")
			_, err := ts.DB.Exec(`
				INSERT INTO user_presence_settings
					(user_id, master_enabled, server_voice_tier, server_voice_show_details)
				VALUES ($1, TRUE, $2, TRUE)
			`, sender.ID, presence.TierServers)
			require.NoError(t, err)

			joinedOldAt := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
			leftOldAt := joinedOldAt.Add(time.Second)
			joinedNewAt := joinedOldAt.Add(2 * time.Second)
			sub.HandleJoined(mustJSON(t, map[string]interface{}{
				"channelId": oldChannelID, "userId": sender.ID,
				"username": sender.Username, "timestamp": joinedOldAt.Format(time.RFC3339Nano),
			}))
			left := mustJSON(t, map[string]interface{}{
				"channelId": oldChannelID, "userId": sender.ID,
				"timestamp": leftOldAt.Format(time.RFC3339Nano),
			})
			joined := mustJSON(t, map[string]interface{}{
				"channelId": newChannelID, "userId": sender.ID,
				"username": sender.Username, "timestamp": joinedNewAt.Format(time.RFC3339Nano),
			})
			if test.leftBefore {
				sub.HandleLeft(left)
				sub.HandleJoined(joined)
			} else {
				sub.HandleJoined(joined)
				sub.HandleLeft(left)
			}

			var (
				rowCount int
				gotRoom  uuid.UUID
				gotAt    time.Time
			)
			require.NoError(t, ts.DB.QueryRow(`
				SELECT COUNT(*) FROM voice_participants WHERE user_id = $1
			`, sender.ID).Scan(&rowCount))
			require.Equal(t, 1, rowCount)
			require.NoError(t, ts.DB.QueryRow(`
				SELECT channel_id, lifecycle_event_at
				FROM voice_participants WHERE user_id = $1
			`, sender.ID).Scan(&gotRoom, &gotAt))
			require.Equal(t, uuid.MustParse(newChannelID), gotRoom)
			require.Equal(t, joinedNewAt, gotAt)
			state, found, getErr := presence.NewActivityStore(ts.Redis).Get(
				context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
			)
			require.NoError(t, getErr)
			require.True(t, found)
			require.Equal(t, uuid.MustParse(newChannelID), state.SourceToken)
			require.Equal(t, joinedNewAt.UnixMicro(), state.SourceVersion)
		})
	}
}

func TestHandleJoined_PrivateCallDeletesExpiredPriorCallRowsWithoutRebinding(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_dm_fresh_call_caller")
	stalePeer := ts.CreateTestUser(t, "rp_dm_fresh_call_stale_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, stalePeer.ID)
	oldCallID := uuid.New()
	newCallID := uuid.New()
	oldAt := time.Date(2026, 7, 15, 20, 20, 0, 0, time.UTC)
	newAt := oldAt.Add(time.Minute)
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, conversationID, stalePeer.ID, oldAt)
	require.NoError(t, err)
	staleLifecycleKey := "voice:lifecycle:private:" + stalePeer.ID
	require.NoError(t, ts.Redis.HSet(
		context.Background(), staleLifecycleKey,
		"token", oldCallID.String(), "version", oldAt.UnixMicro(), "active", "1",
	).Err())
	require.NoError(t, ts.Redis.PExpire(
		context.Background(), staleLifecycleKey, presence.ActivityStateTTL,
	).Err())
	activityStore := presence.NewActivityStore(ts.Redis)
	stored, err := activityStore.CompareAndSet(
		context.Background(), uuid.MustParse(stalePeer.ID), presence.CategoryPrivateCall,
		presence.ActivityState{
			SourceToken: oldCallID, SourceVersion: oldAt.UnixMicro(),
			Payload:   json.RawMessage(`{"call_type":"dm","participant_count":2}`),
			UpdatedAt: oldAt.Unix(),
		},
	)
	require.NoError(t, err)
	require.True(t, stored)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": newCallID.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": newAt.Format(time.RFC3339Nano),
	}))

	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID, stalePeer.ID),
		"an expired prior-call row must not be rebound into the replacement call")
	require.Equal(t, 1, countDMVoiceParticipants(t, ts.DB, conversationID))
	staleLifecycle, err := ts.Redis.HGetAll(
		context.Background(), staleLifecycleKey,
	).Result()
	require.NoError(t, err)
	require.Equal(t, oldCallID.String(), staleLifecycle["token"],
		"cleanup must not claim the stale participant for the new call")
	state, found, err := activityStore.Get(
		context.Background(), uuid.MustParse(stalePeer.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found,
		"the joining sender cannot distinguish an orphan from the peer's active successor")
	require.Equal(t, oldCallID, state.SourceToken)
	require.Equal(t, oldAt.UnixMicro(), state.SourceVersion)
}

func TestHandleJoined_PrivateCallHealsSenderRowFromExpiredOtherConversation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_dm_cross_scope_sender")
	oldPeer := ts.CreateTestUser(t, "rp_dm_cross_scope_old_peer")
	newPeer := ts.CreateTestUser(t, "rp_dm_cross_scope_new_peer")
	oldConversationID := ts.CreateDMConversation(t, sender.ID, oldPeer.ID)
	newConversationID := ts.CreateDMConversation(t, sender.ID, newPeer.ID)
	oldCallID := uuid.New()
	newCallID := uuid.New()
	oldAt := time.Date(2026, 7, 15, 20, 25, 0, 0, time.UTC)
	newAt := oldAt.Add(time.Minute)
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, oldConversationID, sender.ID, oldAt)
	require.NoError(t, err)
	lifecycleKey := "voice:lifecycle:private:" + sender.ID
	require.NoError(t, ts.Redis.HSet(
		context.Background(), lifecycleKey,
		"token", oldCallID.String(), "version", oldAt.UnixMicro(), "active", "1",
	).Err())
	require.NoError(t, ts.Redis.PExpire(
		context.Background(), lifecycleKey, presence.ActivityStateTTL,
	).Err())
	activityStore := presence.NewActivityStore(ts.Redis)
	stored, err := activityStore.CompareAndSet(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryPrivateCall,
		presence.ActivityState{
			SourceToken: oldCallID, SourceVersion: oldAt.UnixMicro(),
			Payload:   json.RawMessage(`{"call_type":"dm","participant_count":2}`),
			UpdatedAt: oldAt.Unix(),
		},
	)
	require.NoError(t, err)
	require.True(t, stored)
	peerAt := newAt.Add(-time.Microsecond)
	_, err = ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, newConversationID, newPeer.ID, peerAt)
	require.NoError(t, err)
	peerLifecycleKey := "voice:lifecycle:private:" + newPeer.ID
	require.NoError(t, ts.Redis.HSet(
		context.Background(), peerLifecycleKey,
		"token", newCallID.String(), "version", peerAt.UnixMicro(), "active", "1",
	).Err())
	require.NoError(t, ts.Redis.PExpire(
		context.Background(), peerLifecycleKey, presence.ActivityStateTTL,
	).Err())

	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": newConversationID, "callId": newCallID.String(),
		"userId": sender.ID, "username": sender.Username,
		"timestamp": newAt.Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(t, ts.DB, oldConversationID, sender.ID))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, newConversationID, sender.ID))
	var rowCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_voice_participants WHERE user_id = $1
	`, sender.ID).Scan(&rowCount))
	require.Equal(t, 1, rowCount)
	lifecycle, err := ts.Redis.HGetAll(context.Background(), lifecycleKey).Result()
	require.NoError(t, err)
	require.Equal(t, newCallID.String(), lifecycle["token"])
	state, found, err := activityStore.Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, newCallID, state.SourceToken,
		"successor-safe cleanup must preserve the freshly published call generation")
}

func TestHandleJoined_PrivateCallLeaseRotationRollsBackPausedOldCallInsert(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_dm_lease_rotate_caller")
	peer := ts.CreateTestUser(t, "rp_dm_lease_rotate_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	callA := uuid.New()
	callB := uuid.New()
	eventA := time.Date(2026, 7, 15, 20, 28, 0, 0, time.UTC)
	eventB := eventA.Add(time.Microsecond)
	claimed := make(chan struct{})
	release := make(chan struct{})
	sub.SetVoiceLifecycleClaimedHookForTest(func(
		category presence.Category,
		senderID uuid.UUID,
		_ time.Time,
	) {
		if category == presence.CategoryPrivateCall && senderID == uuid.MustParse(caller.ID) {
			close(claimed)
			<-release
		}
	})
	eventPayloadA := mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callA.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": eventA.Format(time.RFC3339Nano),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub.HandleJoined(eventPayloadA)
	}()
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for old-call lifecycle claim")
	}
	require.NoError(t, dm.DeleteDMVoiceCallLease(
		context.Background(), ts.Redis, conversationID, callA,
	))
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis,
		dm.VoiceCallLease{
			ConversationID: conversationID,
			CallID:         callB,
			CallerUserID:   uuid.MustParse(caller.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for paused old-call join")
	}
	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), caller.ID,
	), "a lease rotated before commit must roll back the delayed old-call insert")

	sub.SetVoiceLifecycleClaimedHookForTest(nil)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": callB.String(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": eventB.Format(time.RFC3339Nano),
	}))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
	lifecycle, err := ts.Redis.HGetAll(
		context.Background(), "voice:lifecycle:private:"+caller.ID,
	).Result()
	require.NoError(t, err)
	require.Equal(t, callB.String(), lifecycle["token"])
}

func TestUpsertPrivateVoiceParticipant_ConcurrentJoinsCannotExceedParticipantCap(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winner := newTestSubscriber(ts)
	waiter := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_private_cap_caller")
	peer := ts.CreateTestUser(t, "rp_private_cap_peer")
	firstJoiner := ts.CreateTestUser(t, "rp_private_cap_first")
	secondJoiner := ts.CreateTestUser(t, "rp_private_cap_second")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	_, err := ts.DB.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2), ($1, $3)
	`, conversationID, firstJoiner.ID, secondJoiner.ID)
	require.NoError(t, err)
	callID := uuid.New()
	eventAt := time.Date(2026, 7, 15, 20, 29, 0, 123456000, time.UTC)
	prefix := "rpcap_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	_, err = ts.DB.Exec(`
		INSERT INTO users
			(id, email, username, password_hash, age_verified, email_verified)
		SELECT
			gen_random_uuid(),
			$1 || '_' || series || '@test.concord.chat',
			$1 || '_' || series,
			'cap-test-hash', TRUE, TRUE
		FROM generate_series(1, 254) AS series
	`, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO dm_voice_participants
			(conversation_id, user_id, joined_at, lifecycle_event_at)
		SELECT $1, id, $3, $3
		FROM users
		WHERE LEFT(username, LENGTH($2)) = $2
	`, conversationID, prefix, eventAt.Add(-time.Second))
	require.NoError(t, err)
	rows, err := ts.DB.Query(`
		SELECT user_id
		FROM dm_voice_participants
		WHERE conversation_id = $1
		ORDER BY user_id
	`, conversationID)
	require.NoError(t, err)
	existingIDs := make([]uuid.UUID, 0, 254)
	for rows.Next() {
		var participantID uuid.UUID
		require.NoError(t, rows.Scan(&participantID))
		existingIDs = append(existingIDs, participantID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, existingIDs, 254)

	redisPipe := ts.Redis.Pipeline()
	for _, participantID := range existingIDs {
		key, keyErr := presence.VoiceLifecycleKey(
			participantID, presence.CategoryPrivateCall,
		)
		require.NoError(t, keyErr)
		redisPipe.HSet(context.Background(), key,
			"token", callID.String(),
			"version", eventAt.Add(-time.Second).UnixMicro(),
			"active", "1",
		)
		redisPipe.PExpire(context.Background(), key, presence.ActivityStateTTL)
	}
	_, err = redisPipe.Exec(context.Background())
	require.NoError(t, err)
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis, dm.VoiceCallLease{
			ConversationID:  conversationID,
			CallID:          callID,
			CallerUserID:    uuid.MustParse(caller.ID),
			MediaAuthorized: true,
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))

	firstClaimed := make(chan struct{})
	releaseFirst := make(chan struct{})
	winner.SetVoiceLifecycleClaimedHookForTest(func(
		category presence.Category, senderID uuid.UUID, _ time.Time,
	) {
		if category == presence.CategoryPrivateCall && senderID == uuid.MustParse(firstJoiner.ID) {
			close(firstClaimed)
			<-releaseFirst
		}
	})
	type result struct {
		applied bool
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		applied, joinErr := winner.UpsertPrivateVoiceParticipantForTest(
			context.Background(), conversationID, uuid.MustParse(firstJoiner.ID), callID, eventAt,
		)
		firstDone <- result{applied: applied, err: joinErr}
	}()
	select {
	case <-firstClaimed:
	case <-time.After(2 * time.Second):
		t.Fatal("first join did not pause while holding the conversation participant-set lock")
	}
	secondDone := make(chan result, 1)
	go func() {
		applied, joinErr := waiter.UpsertPrivateVoiceParticipantForTest(
			context.Background(), conversationID, uuid.MustParse(secondJoiner.ID), callID,
			eventAt.Add(time.Microsecond),
		)
		secondDone <- result{applied: applied, err: joinErr}
	}()
	select {
	case premature := <-secondDone:
		t.Fatalf("second join bypassed participant-set serialization: %+v", premature)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	first := <-firstDone
	second := <-secondDone
	require.NoError(t, first.err)
	require.True(t, first.applied)
	require.ErrorContains(t, second.err, "participant limit exceeded")
	require.False(t, second.applied)
	require.Equal(t, 255, countDMVoiceParticipants(t, ts.DB, conversationID.String()))
	require.True(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), firstJoiner.ID,
	))
	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), secondJoiner.ID,
	))
	secondLifecycleKey, err := presence.VoiceLifecycleKey(
		uuid.MustParse(secondJoiner.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, err)
	require.Zero(t, ts.Redis.Exists(context.Background(), secondLifecycleKey).Val(),
		"the over-cap waiter must fail before claiming a Redis lifecycle generation")
}

func TestUpsertServerVoiceParticipant_AtomicallyMovesCurrentRow(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_atomic_move_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Atomic Move")
	oldChannelID := ts.CreateVoiceChannel(t, serverID, "rp-atomic-old")
	newChannelID := ts.CreateVoiceChannel(t, serverID, "rp-atomic-new")
	joinedAt := time.Date(2026, 7, 15, 20, 30, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, oldChannelID, sender.ID, joinedAt)
	require.NoError(t, err)

	result, err := sub.UpsertServerVoiceParticipantForTest(
		context.Background(), uuid.MustParse(newChannelID), uuid.MustParse(sender.ID),
		joinedAt.Add(time.Second),
	)
	require.NoError(t, err)
	require.True(t, result.Applied)
}

func TestUpsertServerVoiceParticipant_ReplaysMoveResultAcrossReplicas(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winner := newTestSubscriber(ts)
	loser := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_replay_move_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Replay Move")
	oldChannelID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-replay-old"))
	newChannelID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-replay-new"))
	joinedAt := time.Date(2026, 7, 15, 20, 35, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, oldChannelID, sender.ID, joinedAt)
	require.NoError(t, err)
	eventAt := joinedAt.Add(time.Second)

	winnerResult, err := winner.UpsertServerVoiceParticipantForTest(
		context.Background(), newChannelID, uuid.MustParse(sender.ID), eventAt,
	)
	require.NoError(t, err)
	loserResult, err := loser.UpsertServerVoiceParticipantForTest(
		context.Background(), newChannelID, uuid.MustParse(sender.ID), eventAt,
	)
	require.NoError(t, err)

	require.True(t, winnerResult.Applied)
	require.True(t, winnerResult.Added)
	require.Equal(t, []uuid.UUID{oldChannelID}, winnerResult.RemovedRoomIDs)
	require.True(t, loserResult.Applied)
	require.True(t, loserResult.Duplicate)
	require.False(t, loserResult.ReplayMissing)
	require.True(t, loserResult.Added)
	require.Equal(t, []uuid.UUID{oldChannelID}, loserResult.RemovedRoomIDs)
	replayKey := fmt.Sprintf(
		"voice:result:server:%s:%d", sender.ID, eventAt.UnixMicro(),
	)
	ttl, err := ts.Redis.PTTL(context.Background(), replayKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, presence.ActivityStateTTL-time.Second)
	require.LessOrEqual(t, ttl, presence.ActivityStateTTL)
}

func TestUpsertServerVoiceParticipant_RejectsAndHealsPoisonedReplay(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload func(target, removed string) string
	}{
		{
			name: "missing required added field",
			payload: func(target, removed string) string {
				return fmt.Sprintf(
					`{"target_room_id":%q,"removed_room_ids":[%q]}`,
					target, removed,
				)
			},
		},
		{
			name: "null removal list",
			payload: func(target, _ string) string {
				return fmt.Sprintf(
					`{"target_room_id":%q,"added":true,"removed_room_ids":null}`,
					target,
				)
			},
		},
		{
			name: "duplicate required field",
			payload: func(target, removed string) string {
				return fmt.Sprintf(
					`{"target_room_id":%q,"added":true,"added":true,"removed_room_ids":[%q]}`,
					target, removed,
				)
			},
		},
		{
			name: "oversized replay",
			payload: func(_, _ string) string {
				return strings.Repeat("x", 16*1024+1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			winner := newTestSubscriber(ts)
			loser := newTestSubscriber(ts)
			sender := ts.CreateTestUser(t, "rp_replay_poison_sender")
			serverID := ts.CreateTestServer(t, sender.ID, "RP Replay Poison")
			oldRoom := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-poison-old"))
			targetRoom := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-poison-new"))
			joinedAt := time.Date(2026, 7, 15, 20, 38, 0, 0, time.UTC)
			_, err := ts.DB.Exec(`
				INSERT INTO voice_participants
					(channel_id, user_id, joined_at, lifecycle_event_at)
				VALUES ($1, $2, $3, $3)
			`, oldRoom, sender.ID, joinedAt)
			require.NoError(t, err)
			eventAt := joinedAt.Add(time.Microsecond)
			_, err = winner.UpsertServerVoiceParticipantForTest(
				context.Background(), targetRoom, uuid.MustParse(sender.ID), eventAt,
			)
			require.NoError(t, err)
			replayKey := fmt.Sprintf(
				"voice:result:server:%s:%d", sender.ID, eventAt.UnixMicro(),
			)
			require.NoError(t, ts.Redis.Set(
				context.Background(), replayKey,
				test.payload(targetRoom.String(), oldRoom.String()),
				presence.ActivityStateTTL,
			).Err())

			_, err = loser.UpsertServerVoiceParticipantForTest(
				context.Background(), targetRoom, uuid.MustParse(sender.ID), eventAt,
			)
			require.Error(t, err)
			require.Zero(t, ts.Redis.Exists(context.Background(), replayKey).Val())
			recovered, err := loser.UpsertServerVoiceParticipantForTest(
				context.Background(), targetRoom, uuid.MustParse(sender.ID), eventAt,
			)
			require.NoError(t, err)
			require.True(t, recovered.Duplicate)
			require.True(t, recovered.ReplayMissing)
		})
	}
}

func TestUpsertServerVoiceParticipant_ReplayRequiresBoundedStringTTL(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	winner := newTestSubscriber(ts)
	loser := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_replay_ttl_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Replay TTL")
	oldRoom := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-replay-ttl-old"))
	targetRoom := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-replay-ttl-new"))
	joinedAt := time.Date(2026, 7, 15, 20, 39, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, oldRoom, sender.ID, joinedAt)
	require.NoError(t, err)
	eventAt := joinedAt.Add(time.Microsecond)
	_, err = winner.UpsertServerVoiceParticipantForTest(
		context.Background(), targetRoom, uuid.MustParse(sender.ID), eventAt,
	)
	require.NoError(t, err)
	replayKey := fmt.Sprintf(
		"voice:result:server:%s:%d", sender.ID, eventAt.UnixMicro(),
	)
	validReplay := fmt.Sprintf(
		`{"target_room_id":%q,"added":true,"removed_room_ids":[%q]}`,
		targetRoom.String(), oldRoom.String(),
	)
	for _, test := range []struct {
		name  string
		setup func()
	}{
		{
			name: "wrong type",
			setup: func() {
				require.NoError(t, ts.Redis.Del(context.Background(), replayKey).Err())
				require.NoError(t, ts.Redis.HSet(
					context.Background(), replayKey, "poison", "value",
				).Err())
				require.NoError(t, ts.Redis.PExpire(
					context.Background(), replayKey, presence.ActivityStateTTL,
				).Err())
			},
		},
		{
			name: "persistent",
			setup: func() {
				require.NoError(t, ts.Redis.Set(
					context.Background(), replayKey, validReplay, 0,
				).Err())
			},
		},
		{
			name: "overlong",
			setup: func() {
				require.NoError(t, ts.Redis.Set(
					context.Background(), replayKey, validReplay,
					presence.ActivityStateTTL+time.Second,
				).Err())
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.setup()
			_, replayErr := loser.UpsertServerVoiceParticipantForTest(
				context.Background(), targetRoom, uuid.MustParse(sender.ID), eventAt,
			)
			require.Error(t, replayErr)
			require.Zero(t, ts.Redis.Exists(context.Background(), replayKey).Val())
		})
	}
}

func TestUpsertServerVoiceParticipant_DoesNotPublishReplayBeforeCommit(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_replay_commit_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Replay Commit")
	oldRoom := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-commit-old"))
	targetRoom := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-commit-new"))
	joinedAt := time.Date(2026, 7, 15, 20, 40, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, oldRoom, sender.ID, joinedAt)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_fail_voice_move_commit() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'simulated deferred voice move commit failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER test_fail_voice_move_commit
			AFTER INSERT OR UPDATE ON voice_participants
			DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION test_fail_voice_move_commit();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_voice_move_commit ON voice_participants;
			DROP FUNCTION IF EXISTS test_fail_voice_move_commit();
		`)
	})
	eventAt := joinedAt.Add(time.Microsecond)
	_, err = sub.UpsertServerVoiceParticipantForTest(
		context.Background(), targetRoom, uuid.MustParse(sender.ID), eventAt,
	)
	require.Error(t, err)
	replayKey := fmt.Sprintf(
		"voice:result:server:%s:%d", sender.ID, eventAt.UnixMicro(),
	)
	require.Zero(t, ts.Redis.Exists(context.Background(), replayKey).Val(),
		"a rolled-back PostgreSQL mutation must not leave a committed replay result")
	require.True(t, voiceParticipantExists(t, ts.DB, oldRoom.String(), sender.ID))
	require.False(t, voiceParticipantExists(t, ts.DB, targetRoom.String(), sender.ID))
}

func TestServerLifecycleWatermarkRejectsEqualTokenConflictAndOldRoomTerminal(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_watermark_successor_sender")
	viewer := ts.CreateTestUser(t, "rp_watermark_successor_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Watermark Successor")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	oldChannelID := ts.CreateVoiceChannel(t, serverID, "rp-successor-old")
	currentChannelID := ts.CreateVoiceChannel(t, serverID, "rp-successor-current")
	conflictChannelID := ts.CreateVoiceChannel(t, serverID, "rp-successor-conflict")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	oldAt := time.Date(2026, 7, 15, 20, 45, 0, 0, time.UTC)
	currentAt := oldAt.Add(time.Second)
	for _, event := range []struct {
		channelID string
		eventAt   time.Time
	}{
		{channelID: oldChannelID, eventAt: oldAt},
		{channelID: currentChannelID, eventAt: currentAt},
		{channelID: conflictChannelID, eventAt: currentAt},
	} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": event.channelID, "userId": sender.ID,
			"username": sender.Username, "timestamp": event.eventAt.Format(time.RFC3339Nano),
		}))
	}
	// A later terminal from the old room must not tombstone the current token.
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": oldChannelID, "userId": sender.ID,
		"timestamp": currentAt.Add(time.Second).Format(time.RFC3339Nano),
	}))

	var channelID uuid.UUID
	require.NoError(t, ts.DB.QueryRow(`
		SELECT channel_id FROM voice_participants WHERE user_id = $1
	`, sender.ID).Scan(&channelID))
	require.Equal(t, uuid.MustParse(currentChannelID), channelID)
	state, found, getErr := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, getErr)
	require.True(t, found)
	require.Equal(t, uuid.MustParse(currentChannelID), state.SourceToken)
	require.Equal(t, currentAt.UnixMicro(), state.SourceVersion)
}

func TestHandleJoined_ServerReceiverRejectsEqualConflictThenAcceptsNextMicrosecond(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_receiver_micro_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Receiver Microsecond")
	firstChannelID := ts.CreateVoiceChannel(t, serverID, "rp-receiver-first")
	conflictChannelID := ts.CreateVoiceChannel(t, serverID, "rp-receiver-conflict")
	baseAt := time.Date(2026, 7, 15, 20, 47, 0, 123456000, time.UTC)

	sendJoined := func(channelID string, eventAt time.Time) {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": channelID,
			"userId":    sender.ID,
			"username":  sender.Username,
			"timestamp": eventAt.Format(time.RFC3339Nano),
		}))
	}
	sendJoined(firstChannelID, baseAt)
	sendJoined(conflictChannelID, baseAt)

	assertCurrent := func(channelID string, eventAt time.Time) {
		t.Helper()
		var gotChannelID uuid.UUID
		var gotLifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRow(`
			SELECT channel_id, lifecycle_event_at
			FROM voice_participants
			WHERE user_id = $1
		`, sender.ID).Scan(&gotChannelID, &gotLifecycleAt))
		require.Equal(t, uuid.MustParse(channelID), gotChannelID)
		require.Equal(t, eventAt, gotLifecycleAt)
		watermark, err := ts.Redis.HGetAll(
			context.Background(),
			"voice:lifecycle:server:"+sender.ID,
		).Result()
		require.NoError(t, err)
		require.Equal(t, channelID, watermark["token"])
		require.Equal(t, fmt.Sprintf("%d", eventAt.UnixMicro()), watermark["version"])
		require.Equal(t, "1", watermark["active"])
	}
	assertCurrent(firstChannelID, baseAt)

	nextAt := baseAt.Add(time.Microsecond)
	sendJoined(conflictChannelID, nextAt)
	assertCurrent(conflictChannelID, nextAt)
}

func TestVoiceLifecycleClaimCAS(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	ctx := context.Background()
	baseAt := time.Date(2026, 7, 15, 20, 50, 0, 0, time.UTC)

	t.Run("equal-version terminal dominates active resurrection", func(t *testing.T) {
		senderID := uuid.New()
		token := uuid.New()
		claimed, err := sub.ClaimVoiceLifecycleForTest(
			ctx, presence.CategoryPrivateCall, senderID, token, baseAt, true,
		)
		require.NoError(t, err)
		require.True(t, claimed)

		claimed, err = sub.ClaimVoiceLifecycleForTest(
			ctx, presence.CategoryPrivateCall, senderID, token, baseAt, false,
		)
		require.NoError(t, err)
		require.True(t, claimed)

		claimed, err = sub.ClaimVoiceLifecycleForTest(
			ctx, presence.CategoryPrivateCall, senderID, token, baseAt, true,
		)
		require.NoError(t, err)
		require.False(t, claimed)

		watermark, err := ts.Redis.HGetAll(
			ctx, "voice:lifecycle:private:"+senderID.String(),
		).Result()
		require.NoError(t, err)
		require.Equal(t, token.String(), watermark["token"])
		require.Equal(t, "0", watermark["active"])
		require.Equal(t, fmt.Sprintf("%d", baseAt.UnixMicro()), watermark["version"])
	})

	t.Run("token-mismatched terminal preserves successor", func(t *testing.T) {
		senderID := uuid.New()
		oldToken := uuid.New()
		successorToken := uuid.New()
		claimed, err := sub.ClaimVoiceLifecycleForTest(
			ctx, presence.CategoryServerVoice, senderID, oldToken, baseAt, true,
		)
		require.NoError(t, err)
		require.True(t, claimed)
		claimed, err = sub.ClaimVoiceLifecycleForTest(
			ctx, presence.CategoryServerVoice, senderID, successorToken, baseAt.Add(time.Second), true,
		)
		require.NoError(t, err)
		require.True(t, claimed)

		claimed, err = sub.ClaimVoiceLifecycleForTest(
			ctx, presence.CategoryServerVoice, senderID, oldToken, baseAt.Add(2*time.Second), false,
		)
		require.NoError(t, err)
		require.False(t, claimed)

		watermark, err := ts.Redis.HGetAll(
			ctx, "voice:lifecycle:server:"+senderID.String(),
		).Result()
		require.NoError(t, err)
		require.Equal(t, successorToken.String(), watermark["token"])
		require.Equal(t, "1", watermark["active"])
		require.Equal(
			t, fmt.Sprintf("%d", baseAt.Add(time.Second).UnixMicro()), watermark["version"],
		)
	})

	t.Run("both categories use the exact activity-state TTL", func(t *testing.T) {
		require.Equal(t, 90*time.Second, presence.ActivityStateTTL)
		for _, category := range []presence.Category{
			presence.CategoryServerVoice,
			presence.CategoryPrivateCall,
		} {
			senderID := uuid.New()
			claimed, err := sub.ClaimVoiceLifecycleForTest(
				ctx, category, senderID, uuid.New(), baseAt, true,
			)
			require.NoError(t, err)
			require.True(t, claimed)
			prefix := "voice:lifecycle:server:"
			if category == presence.CategoryPrivateCall {
				prefix = "voice:lifecycle:private:"
			}
			ttl, err := ts.Redis.PTTL(ctx, prefix+senderID.String()).Result()
			require.NoError(t, err)
			require.Greater(t, ttl, presence.ActivityStateTTL-time.Second)
			require.LessOrEqual(t, ttl, presence.ActivityStateTTL)
		}
	})

	t.Run("strict malformed envelopes fail once then self-heal", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			category presence.Category
			prefix   string
			seed     func(string)
		}{
			{
				name: "noncanonical token", category: presence.CategoryServerVoice,
				prefix: "voice:lifecycle:server:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.HSet(ctx, key,
						"token", strings.ToUpper(uuid.NewString()),
						"version", baseAt.UnixMicro(), "active", "1",
					).Err())
				},
			},
			{
				name: "extra field", category: presence.CategoryPrivateCall,
				prefix: "voice:lifecycle:private:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.HSet(ctx, key,
						"token", uuid.NewString(), "version", baseAt.UnixMicro(),
						"active", "1", "unexpected", "field",
					).Err())
				},
			},
			{
				name: "noncanonical version", category: presence.CategoryServerVoice,
				prefix: "voice:lifecycle:server:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.HSet(ctx, key,
						"token", uuid.NewString(),
						"version", "0"+fmt.Sprintf("%d", baseAt.UnixMicro()), "active", "1",
					).Err())
				},
			},
			{
				name: "invalid active flag", category: presence.CategoryPrivateCall,
				prefix: "voice:lifecycle:private:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.HSet(ctx, key,
						"token", uuid.NewString(), "version", baseAt.UnixMicro(), "active", "true",
					).Err())
				},
			},
			{
				name: "wrong redis type", category: presence.CategoryPrivateCall,
				prefix: "voice:lifecycle:private:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.Set(ctx, key, "corrupt", 0).Err())
				},
			},
			{
				name: "persistent otherwise-valid hash", category: presence.CategoryServerVoice,
				prefix: "voice:lifecycle:server:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.HSet(ctx, key,
						"token", uuid.NewString(), "version", baseAt.UnixMicro(), "active", "1",
					).Err())
				},
			},
			{
				name: "overlong otherwise-valid ttl", category: presence.CategoryPrivateCall,
				prefix: "voice:lifecycle:private:",
				seed: func(key string) {
					require.NoError(t, ts.Redis.HSet(ctx, key,
						"token", uuid.NewString(), "version", baseAt.UnixMicro(), "active", "1",
					).Err())
					require.NoError(t, ts.Redis.PExpire(
						ctx, key, 2*presence.ActivityStateTTL,
					).Err())
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				senderID := uuid.New()
				key := test.prefix + senderID.String()
				test.seed(key)
				claimed, err := sub.ClaimVoiceLifecycleForTest(
					ctx, test.category, senderID, uuid.New(), baseAt.Add(time.Second), true,
				)
				require.ErrorContains(t, err, "malformed voice lifecycle watermark")
				require.False(t, claimed)
				require.Zero(t, ts.Redis.Exists(ctx, key).Val(),
					"corrupt lifecycle state must not become an immortal retry poison")

				recoveryToken := uuid.New()
				claimed, err = sub.ClaimVoiceLifecycleForTest(
					ctx, test.category, senderID, recoveryToken, baseAt.Add(2*time.Second), true,
				)
				require.NoError(t, err)
				require.True(t, claimed)
				watermark, readErr := ts.Redis.HGetAll(ctx, key).Result()
				require.NoError(t, readErr)
				require.Equal(t, map[string]string{
					"token":   recoveryToken.String(),
					"version": fmt.Sprintf("%d", baseAt.Add(2*time.Second).UnixMicro()),
					"active":  "1",
				}, watermark)
				ttl, ttlErr := ts.Redis.PTTL(ctx, key).Result()
				require.NoError(t, ttlErr)
				require.Greater(t, ttl, presence.ActivityStateTTL-time.Second)
				require.LessOrEqual(t, ttl, presence.ActivityStateTTL)
			})
		}
	})
}

func TestVoiceLifecycleMutationFenceRejectsPausedOldInsertAfterNewerTerminal(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	oldReplica := newTestSubscriber(ts)
	newReplica := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_fence_terminal_caller")
	peer := ts.CreateTestUser(t, "rp_fence_terminal_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	senderID := uuid.MustParse(caller.ID)
	callID := uuid.New()
	oldAt := time.Date(2026, 7, 15, 20, 55, 0, 0, time.UTC)
	terminalAt := oldAt.Add(time.Second)
	oldClaimed := make(chan struct{})
	releaseOld := make(chan struct{})
	oldReplica.SetVoiceLifecycleClaimedHookForTest(
		func(_ presence.Category, _ uuid.UUID, eventAt time.Time) {
			if eventAt.Equal(oldAt) {
				close(oldClaimed)
				<-releaseOld
			}
		},
	)

	oldDone := make(chan error, 1)
	go func() {
		_, err := oldReplica.RunVoiceLifecycleMutationForTest(
			context.Background(), presence.CategoryPrivateCall,
			senderID, callID, conversationID, oldAt, true,
			func(ctx context.Context, tx *sql.Tx) (bool, error) {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO dm_voice_participants
						(conversation_id, user_id, joined_at, lifecycle_event_at)
					VALUES ($1, $2, $3, $3)
				`, conversationID, senderID, oldAt)
				return err == nil, err
			},
		)
		oldDone <- err
	}()
	select {
	case <-oldClaimed:
	case <-time.After(2 * time.Second):
		t.Fatal("old lifecycle mutation did not reach the post-claim pause")
	}

	newDone := make(chan error, 1)
	go func() {
		_, err := newReplica.RunVoiceLifecycleMutationForTest(
			context.Background(), presence.CategoryPrivateCall,
			senderID, callID, conversationID, terminalAt, false,
			func(ctx context.Context, tx *sql.Tx) (bool, error) {
				result, err := tx.ExecContext(ctx, `
					DELETE FROM dm_voice_participants
					WHERE conversation_id = $1 AND user_id = $2
				`, conversationID, senderID)
				if err != nil {
					return false, err
				}
				rows, err := result.RowsAffected()
				return rows == 1, err
			},
		)
		newDone <- err
	}()

	newFinishedBeforeRelease := false
	select {
	case err := <-newDone:
		require.NoError(t, err)
		newFinishedBeforeRelease = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseOld)
	require.NoError(t, <-oldDone)
	if !newFinishedBeforeRelease {
		require.NoError(t, <-newDone)
	}
	require.False(t, dmVoiceParticipantExists(
		t, ts.DB, conversationID.String(), senderID.String(),
	), "newer terminal must run after the accepted older mutation and remove it")
}

func TestVoiceLifecycleMutationFenceSerializesConcurrentCrossChannelMoves(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	oldReplica := newTestSubscriber(ts)
	newReplica := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_fence_move_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Fence Move")
	oldChannelID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-fence-old"))
	newChannelID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "rp-fence-new"))
	senderID := uuid.MustParse(sender.ID)
	oldAt := time.Date(2026, 7, 15, 20, 56, 0, 0, time.UTC)
	newAt := oldAt.Add(time.Second)
	oldClaimed := make(chan struct{})
	releaseOld := make(chan struct{})
	oldReplica.SetVoiceLifecycleClaimedHookForTest(
		func(_ presence.Category, _ uuid.UUID, eventAt time.Time) {
			if eventAt.Equal(oldAt) {
				close(oldClaimed)
				<-releaseOld
			}
		},
	)

	oldDone := make(chan error, 1)
	go func() {
		_, err := oldReplica.RunVoiceLifecycleMutationForTest(
			context.Background(), presence.CategoryServerVoice,
			senderID, oldChannelID, uuid.Nil, oldAt, true,
			func(ctx context.Context, tx *sql.Tx) (bool, error) {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO voice_participants
						(channel_id, user_id, joined_at, lifecycle_event_at)
					VALUES ($1, $2, $3, $3)
				`, oldChannelID, senderID, oldAt)
				return err == nil, err
			},
		)
		oldDone <- err
	}()
	select {
	case <-oldClaimed:
	case <-time.After(2 * time.Second):
		t.Fatal("old server move did not reach the post-claim pause")
	}

	newDone := make(chan error, 1)
	go func() {
		_, err := newReplica.RunVoiceLifecycleMutationForTest(
			context.Background(), presence.CategoryServerVoice,
			senderID, newChannelID, uuid.Nil, newAt, true,
			func(ctx context.Context, tx *sql.Tx) (bool, error) {
				_, err := tx.ExecContext(ctx, `
					WITH removed AS (
						DELETE FROM voice_participants
						WHERE user_id = $2 AND channel_id <> $1
					)
					INSERT INTO voice_participants
						(channel_id, user_id, joined_at, lifecycle_event_at)
					VALUES ($1, $2, $3, $3)
				`, newChannelID, senderID, newAt)
				return err == nil, err
			},
		)
		newDone <- err
	}()
	newFinishedBeforeRelease := false
	select {
	case err := <-newDone:
		require.NoError(t, err)
		newFinishedBeforeRelease = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseOld)
	require.NoError(t, <-oldDone)
	if !newFinishedBeforeRelease {
		require.NoError(t, <-newDone)
	}

	var rowCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_participants WHERE user_id = $1
	`, senderID).Scan(&rowCount))
	require.Equal(t, 1, rowCount)
	var gotChannelID uuid.UUID
	require.NoError(t, ts.DB.QueryRow(`
		SELECT channel_id FROM voice_participants WHERE user_id = $1
	`, senderID).Scan(&gotChannelID))
	require.Equal(t, newChannelID, gotChannelID)
}

func TestHandleServerTerminalWatermarkRejectsDelayedRestoration(t *testing.T) {
	for _, delayed := range []string{"join", "heartbeat"} {
		t.Run(delayed, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			sender := ts.CreateTestUser(t, "rp_tombstone_"+delayed)
			viewer := ts.CreateTestUser(t, "rp_tombstone_viewer_"+delayed)
			serverID := ts.CreateTestServer(t, sender.ID, "RP Tombstone")
			ts.AddMemberToServer(t, serverID, viewer.ID, "member")
			channelID := ts.CreateVoiceChannel(t, serverID, "rp-tombstone")
			_, err := ts.DB.Exec(`
				INSERT INTO user_presence_settings
					(user_id, master_enabled, server_voice_tier, server_voice_show_details)
				VALUES ($1, TRUE, $2, TRUE)
			`, sender.ID, presence.TierServers)
			require.NoError(t, err)
			joinedAt := time.Date(2026, 7, 15, 21, 0, 0, 0, time.UTC)
			terminalAt := joinedAt.Add(2 * time.Second)
			delayedAt := joinedAt.Add(time.Second)
			sub.HandleJoined(mustJSON(t, map[string]interface{}{
				"channelId": channelID, "userId": sender.ID,
				"username": sender.Username, "timestamp": joinedAt.Format(time.RFC3339Nano),
			}))
			sub.HandleLeft(mustJSON(t, map[string]interface{}{
				"channelId": channelID, "userId": sender.ID,
				"timestamp": terminalAt.Format(time.RFC3339Nano),
			}))

			watermarkKey := "voice:lifecycle:server:" + sender.ID
			ttl, ttlErr := ts.Redis.PTTL(context.Background(), watermarkKey).Result()
			require.NoError(t, ttlErr)
			require.Greater(t, ttl, 85*time.Second)
			require.LessOrEqual(t, ttl, 90*time.Second)
			if delayed == "join" {
				sub.HandleJoined(mustJSON(t, map[string]interface{}{
					"channelId": channelID, "userId": sender.ID,
					"username": sender.Username, "timestamp": delayedAt.Format(time.RFC3339Nano),
				}))
			} else {
				sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
					"channelId": channelID, "userIds": []string{sender.ID},
					"timestamp": delayedAt.Format(time.RFC3339Nano),
				}))
			}

			require.False(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
			_, found, getErr := presence.NewActivityStore(ts.Redis).Get(
				context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
			)
			require.NoError(t, getErr)
			require.False(t, found)
		})
	}
}

func TestHandleHeartbeat_PrivateCallDoesNotResurrectParticipantAfterNewerLeft(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_dm_tombstone_caller")
	peer := ts.CreateTestUser(t, "rp_dm_tombstone_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, private_call_tier, private_call_show_details)
		VALUES ($1, TRUE, $3, TRUE), ($2, TRUE, $3, TRUE)
	`, caller.ID, peer.ID, presence.TierOff)
	require.NoError(t, err)
	callID := uuid.New()
	joinedAt := time.Date(2026, 7, 15, 22, 0, 0, 0, time.UTC)
	for index, participant := range []testhelpers.TestUser{caller, peer} {
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID, "callId": callID.String(),
			"userId": participant.ID, "username": participant.Username,
			"timestamp": joinedAt.Add(time.Duration(index) * time.Millisecond).Format(time.RFC3339Nano),
		}))
	}
	terminalAt := joinedAt.Add(2 * time.Second)
	sub.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": caller.ID, "timestamp": terminalAt.Format(time.RFC3339Nano),
	}))
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"callerUserId": caller.ID, "userIds": []string{caller.ID, peer.ID},
		"timestamp": joinedAt.Add(time.Second).Format(time.RFC3339Nano),
	}))

	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID, peer.ID))
	_, found, getErr := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(caller.ID), presence.CategoryPrivateCall,
	)
	require.NoError(t, getErr)
	require.False(t, found)
}

func TestHandleHeartbeat_ServerRestorationBroadcastsAuthoritativeBaseAndCount(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_server_restore_sender")
	viewer := ts.CreateTestUser(t, "rp_server_restore_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Server Restore")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-server-restore")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)

	conn := connectVoiceWireClient(t, ts, viewer)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server",
		"data": map[string]interface{}{"server_id": serverID},
	}))
	synchronizeVoiceWireClient(t, conn)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{sender.ID},
		"timestamp": time.Date(2026, 7, 15, 22, 30, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	gotJoined := false
	gotCount := false
	for !gotJoined || !gotCount {
		var envelope voiceWireEnvelope
		require.NoError(t, conn.ReadJSON(&envelope))
		switch envelope.Type {
		case "voice_state_update":
			if envelope.Data["action"] != "joined" || envelope.Data["user_id"] != sender.ID {
				continue
			}
			require.Equal(t, channelID, envelope.Data["channel_id"])
			require.Equal(t, serverID, envelope.Data["server_id"])
			_, hasUsername := envelope.Data["username"]
			_, hasDisplayName := envelope.Data["display_name"]
			require.False(t, hasUsername, "heartbeat recovery must not trust event username metadata")
			require.False(t, hasDisplayName, "heartbeat recovery must not trust event display metadata")
			gotJoined = true
		case "server_voice_counts":
			counts, ok := envelope.Data["counts"].(map[string]interface{})
			if !ok || counts[serverID] != float64(1) {
				continue
			}
			gotCount = true
		}
	}
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
}

func TestHandleHeartbeat_PrivateCallRestorationBroadcastsAuthoritativeBase(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_dm_restore_caller")
	peer := ts.CreateTestUser(t, "rp_dm_restore_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	conversationUUID := uuid.MustParse(conversationID)
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis,
		dm.VoiceCallLease{
			ConversationID: conversationUUID,
			CallID:         callID,
			CallerUserID:   uuid.MustParse(caller.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))

	conn := connectVoiceWireClient(t, ts, peer)
	synchronizeVoiceWireClient(t, conn)
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId":    conversationID,
		"callId":       callID.String(),
		"callerUserId": caller.ID,
		"userIds":      []string{caller.ID},
		"timestamp":    time.Date(2026, 7, 15, 23, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}))

	envelope := waitForVoiceWireType(t, conn, "dm_voice_state_update")
	require.Equal(t, "joined", envelope.Data["action"])
	require.Equal(t, caller.ID, envelope.Data["user_id"])
	require.Equal(t, conversationID, envelope.Data["conversation_id"])
	_, hasUsername := envelope.Data["username"]
	_, hasDisplayName := envelope.Data["display_name"]
	require.False(t, hasUsername, "heartbeat recovery must not trust event username metadata")
	require.False(t, hasDisplayName, "heartbeat recovery must not trust event display metadata")
	require.True(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))
}

func TestHandleHeartbeat_LifecycleWatermarkErrorDisconnectsAllClients(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_claim_error_sender")
	viewer := ts.CreateTestUser(t, "rp_claim_error_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Claim Error")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-claim-error")
	connectVoiceWireClient(t, ts, viewer)
	eventAt := time.Date(2026, 7, 15, 23, 30, 0, 0, time.UTC)

	watermarkKey := "voice:lifecycle:server:" + sender.ID
	require.NoError(t, ts.Redis.Set(
		context.Background(), watermarkKey, "malformed-watermark", 0,
	).Err())
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{sender.ID},
		"timestamp": eventAt.Format(time.RFC3339Nano),
	}))

	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(viewer.ID)) == 0
	}, time.Second, 10*time.Millisecond)
	require.False(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
	require.Zero(t, ts.Redis.Exists(context.Background(), watermarkKey).Val(),
		"malformed lifecycle state must be deleted after the event fails closed")

	recoveryAt := eventAt.Add(time.Microsecond)
	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{sender.ID},
		"timestamp": recoveryAt.Format(time.RFC3339Nano),
	}))
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
	watermark, err := ts.Redis.HGetAll(context.Background(), watermarkKey).Result()
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"token":   channelID,
		"version": fmt.Sprintf("%d", recoveryAt.UnixMicro()),
		"active":  "1",
	}, watermark)
	ttl, err := ts.Redis.PTTL(context.Background(), watermarkKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, presence.ActivityStateTTL-time.Second)
	require.LessOrEqual(t, ttl, presence.ActivityStateTTL)
}

func TestHandleHeartbeat_ReconciliationErrorDisconnectsAllClients(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_reconcile_error_sender")
	viewer := ts.CreateTestUser(t, "rp_reconcile_error_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Reconcile Error")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-reconcile-error")
	eventAt := time.Date(2026, 7, 15, 23, 45, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, channelID, sender.ID, eventAt)
	require.NoError(t, err)
	claimed, err := sub.ClaimVoiceLifecycleForTest(
		context.Background(), presence.CategoryServerVoice,
		uuid.MustParse(sender.ID), uuid.MustParse(channelID), eventAt, true,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_fail_server_voice_delete() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'simulated server voice participant delete failure';
			RETURN OLD;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_server_voice_delete
			BEFORE DELETE ON voice_participants
			FOR EACH ROW EXECUTE FUNCTION test_fail_server_voice_delete();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_server_voice_delete ON voice_participants;
			DROP FUNCTION IF EXISTS test_fail_server_voice_delete();
		`)
	})
	connectVoiceWireClient(t, ts, viewer)

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID,
		"userIds":   []string{},
		"timestamp": eventAt.Add(time.Second).Format(time.RFC3339Nano),
	}))

	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(viewer.ID)) == 0
	}, time.Second, 10*time.Millisecond)
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID),
		"failed reconciliation must not pretend the participant was removed")
}

func TestVoiceJoinedRejectsUnsafeSourceVersionBeforeAnyMutation(t *testing.T) {
	unsafeTimestamp := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)

	t.Run("server", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		sender := ts.CreateTestUser(t, "rp_unsafe_server_sender")
		serverID := ts.CreateTestServer(t, sender.ID, "RP Unsafe Server")
		channelID := ts.CreateVoiceChannel(t, serverID, "rp-unsafe-server")
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": channelID, "userId": sender.ID,
			"username": sender.Username, "timestamp": unsafeTimestamp,
		}))
		require.False(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID))
		require.Zero(t, ts.Redis.Exists(
			context.Background(), "voice:lifecycle:server:"+sender.ID,
		).Val())
	})

	t.Run("private call", func(t *testing.T) {
		ts := testhelpers.SetupTestServer(t)
		sub := newTestSubscriber(ts)
		caller := ts.CreateTestUser(t, "rp_unsafe_dm_caller")
		peer := ts.CreateTestUser(t, "rp_unsafe_dm_peer")
		conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
		callID := uuid.New()
		sub.HandleJoined(mustJSON(t, map[string]interface{}{
			"channelId": conversationID, "callId": callID.String(),
			"userId": caller.ID, "username": caller.Username, "timestamp": unsafeTimestamp,
		}))
		require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID, caller.ID))
		_, found, err := dm.LookupDMVoiceCallLease(
			context.Background(), ts.Redis, uuid.MustParse(conversationID),
		)
		require.NoError(t, err)
		require.False(t, found)
		require.Zero(t, ts.Redis.Exists(
			context.Background(), "voice:lifecycle:private:"+caller.ID,
		).Val())
	})
}

func TestDMVoiceDependencyErrorsDisconnectExistingRichPresenceClients(t *testing.T) {
	for _, eventType := range []string{"join", "left", "room_empty", "empty_heartbeat", "heartbeat"} {
		t.Run(eventType, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			sub := newTestSubscriber(ts)
			caller := ts.CreateTestUser(t, "rp_dependency_caller_"+eventType)
			peer := ts.CreateTestUser(t, "rp_dependency_peer_"+eventType)
			conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
			callID := uuid.New()
			connectVoiceWireClient(t, ts, peer)
			require.NoError(t, ts.Redis.Close())
			eventAt := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
			switch eventType {
			case "join":
				sub.HandleJoined(mustJSON(t, map[string]interface{}{
					"channelId": conversationID, "callId": callID.String(),
					"userId": caller.ID, "username": caller.Username, "timestamp": eventAt,
				}))
			case "left":
				sub.HandleLeft(mustJSON(t, map[string]interface{}{
					"channelId": conversationID, "callId": callID.String(),
					"userId": caller.ID, "timestamp": eventAt,
				}))
			case "room_empty":
				sub.HandleRoomEmpty(mustJSON(t, map[string]interface{}{
					"channelId": conversationID, "callId": callID.String(), "timestamp": eventAt,
				}))
			case "empty_heartbeat":
				sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
					"channelId": conversationID, "callId": callID.String(),
					"userIds": []string{}, "timestamp": eventAt,
				}))
			case "heartbeat":
				sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
					"channelId": conversationID, "callId": callID.String(),
					"callerUserId": caller.ID, "userIds": []string{caller.ID},
					"timestamp": eventAt,
				}))
			}
			require.Eventually(t, func() bool {
				return ts.Hub.GetUserClientCount(uuid.MustParse(peer.ID)) == 0
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestDMVoiceLeaseConflictDoesNotDisconnectCurrentClients(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	caller := ts.CreateTestUser(t, "rp_lease_conflict_caller")
	peer := ts.CreateTestUser(t, "rp_lease_conflict_peer")
	conversationID := uuid.MustParse(ts.CreateDMConversation(t, caller.ID, peer.ID))
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), ts.Redis,
		dm.VoiceCallLease{
			ConversationID: conversationID,
			CallID:         uuid.New(),
			CallerUserID:   uuid.MustParse(caller.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))
	connectVoiceWireClient(t, ts, peer)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": conversationID.String(), "callId": uuid.NewString(),
		"userId": caller.ID, "username": caller.Username,
		"timestamp": time.Date(2026, 7, 16, 0, 5, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}))
	require.Equal(t, 1, ts.Hub.GetUserClientCount(uuid.MustParse(peer.ID)))
	require.False(t, dmVoiceParticipantExists(t, ts.DB, conversationID.String(), caller.ID))
}

func TestHandleJoined_ServerAmbiguousRowsHealConservatively(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_ambiguous_heal_sender")
	viewer := ts.CreateTestUser(t, "rp_ambiguous_heal_viewer")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Ambiguous Heal")
	ts.AddMemberToServer(t, serverID, viewer.ID, "member")
	oldA := ts.CreateVoiceChannel(t, serverID, "rp-ambiguous-a")
	oldB := ts.CreateVoiceChannel(t, serverID, "rp-ambiguous-b")
	target := ts.CreateVoiceChannel(t, serverID, "rp-ambiguous-target")
	baseAt := time.Date(2026, 7, 16, 0, 10, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $3, $4, $4), ($2, $3, $4, $4)
	`, oldA, oldB, sender.ID, baseAt)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO user_presence_settings
			(user_id, master_enabled, server_voice_tier, server_voice_show_details)
		VALUES ($1, TRUE, $2, TRUE)
	`, sender.ID, presence.TierServers)
	require.NoError(t, err)
	connectVoiceWireClient(t, ts, viewer)
	sub.HandleJoined(mustJSON(t, map[string]interface{}{
		"channelId": target, "userId": sender.ID, "username": sender.Username,
		"timestamp": baseAt.Add(time.Second).Format(time.RFC3339Nano),
	}))

	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(viewer.ID)) == 0
	}, time.Second, 10*time.Millisecond)
	var rowCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_participants WHERE user_id = $1
	`, sender.ID).Scan(&rowCount))
	require.Equal(t, 1, rowCount)
	require.True(t, voiceParticipantExists(t, ts.DB, target, sender.ID))
	state, found, err := presence.NewActivityStore(ts.Redis).Get(
		context.Background(), uuid.MustParse(sender.ID), presence.CategoryServerVoice,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, uuid.MustParse(target), state.SourceToken)
}

func TestHandleHeartbeat_ParticipantFailureDoesNotBecomeAuthoritativeLeave(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	sender := ts.CreateTestUser(t, "rp_heartbeat_keep_sender")
	serverID := ts.CreateTestServer(t, sender.ID, "RP Heartbeat Keep")
	channelID := ts.CreateVoiceChannel(t, serverID, "rp-heartbeat-keep")
	joinedAt := time.Date(2026, 7, 16, 0, 15, 0, 0, time.UTC)
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants
			(channel_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, channelID, sender.ID, joinedAt)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(
		context.Background(), "voice:lifecycle:server:"+sender.ID, "corrupt", 0,
	).Err())

	sub.HandleHeartbeat(mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userIds": []string{sender.ID},
		"timestamp": joinedAt.Add(time.Second).Format(time.RFC3339Nano),
	}))
	require.True(t, voiceParticipantExists(t, ts.DB, channelID, sender.ID),
		"a participant reported by the authoritative heartbeat must remain in the reconcile keep-set")
}
