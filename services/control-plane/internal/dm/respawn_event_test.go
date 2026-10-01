package dm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connectVisibilityWS opens a WebSocket for userID and, when subscribe is set,
// subscribes it to convID. A hidden thread's client is NOT subscribed.
func connectVisibilityWS(t *testing.T, ts *testhelpers.TestServer, userID, convID string, subscribe bool) *gorillaWS.Conn {
	t.Helper()
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	ticket := "dm-respawn-" + uuid.NewString()
	require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":"+ticket, time.Minute).Err())
	conn, _, dialErr := gorillaWS.DefaultDialer.Dial("ws"+wsServer.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
	require.NoError(t, dialErr)
	t.Cleanup(func() { _ = conn.Close() })
	readVisibilityFrame(t, conn, "connected")
	if subscribe {
		require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_dm", "data": map[string]interface{}{"conversation_id": convID}}))
		readVisibilityFrame(t, conn, "dm_subscribed")
	}
	return conn
}

// hideForActor hides convID for actor and returns the actor's unsubscribed
// connection with the hide's own (non-null) frame already consumed.
func hideForActor(t *testing.T, ts *testhelpers.TestServer, actor testhelpers.TestUser, convID string) *gorillaWS.Conn {
	t.Helper()
	conn := connectVisibilityWS(t, ts, actor.ID, convID, false)
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/hide", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	hidden := readVisibilityFrame(t, conn, "dm_conversation_hidden")
	require.NotNil(t, hidden.Data["hidden_at"], "the hide itself publishes a non-null state")
	return conn
}

func requireRespawnFrame(t *testing.T, conn *gorillaWS.Conn, convID string) {
	t.Helper()
	respawn := readVisibilityFrame(t, conn, "dm_conversation_hidden")
	assert.Equal(t, convID, respawn.Data["conversation_id"])
	assert.Contains(t, respawn.Data, "hidden_at")
	assert.Nil(t, respawn.Data["hidden_at"], "a respawn publishes the visible (null) state")
}

// A hidden thread's client discards its local view and unsubscribes, so the
// only way it learns that a peer message respawned the thread is an explicit
// visibility event. Without one the thread stays invisible until the next full
// conversation refetch (#2822 walkthrough, 2026-09-26).
func TestDMVisibility_PeerMessageRespawnPublishesNullHiddenEventToActorOnly(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "respawn_event_actor")
	peer := ts.CreateTestUser(t, "respawn_event_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	peerConn := connectVisibilityWS(t, ts, peer.ID, convID, true)
	actorConn := hideForActor(t, ts, actor, convID)

	require.NoError(t, peerConn.WriteJSON(map[string]interface{}{
		"type": "dm_message",
		"data": map[string]interface{}{
			"conversation_id": convID,
			"content":         "respawn me",
			"key_version":     1,
			"nonce":           "respawn-nonce",
		},
	}))
	// The hub publishes the respawn before the ack, so a respawn wrongly sent
	// to the peer lands ahead of it; readVisibilityFrame would skip it unseen.
	readUntilWithoutVisibilityEvent(t, peerConn, "dm_message_ack")

	requireRespawnFrame(t, actorConn, convID)
	// The respawn signal is actor-only: the peer never learns the thread was hidden.
	assertPeerVisibilityEventAbsentBeforeBarrier(t, peerConn, convID)
}

// An expiration-policy change writes a system row, which respawns the thread
// for a participant who hid it.
func TestDMVisibility_PeerExpirationChangeRespawnPublishes(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "respawn_expiry_actor")
	peer := ts.CreateTestUser(t, "respawn_expiry_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	actorConn := hideForActor(t, ts, actor, convID)

	w := ts.DoRequest(http.MethodPatch, pathDMConversationsPrefix+convID+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"},
		testhelpers.AuthHeaders(peer.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	requireRespawnFrame(t, actorConn, convID)
}

// A call-event row (here a caller-cancelled ring) respawns the thread for a
// callee who hid it.
func TestDMVisibility_PeerCallEventRespawnPublishes(t *testing.T) {
	ts := setupTS(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	actor := ts.CreateTestUser(t, "respawn_call_actor")
	peer := ts.CreateTestUser(t, "respawn_call_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	actorConn := hideForActor(t, ts, actor, convID)

	ringForTest(t, ts, peer, convID)
	ts.Redis.Del(context.Background(), fmt.Sprintf("ratelimit:user:%s", peer.ID))
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceCancel, nil, testhelpers.AuthHeaders(peer.AccessToken))
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	requireRespawnFrame(t, actorConn, convID)
}

// A respawn rolls back with the message that caused it, so a failed write
// must publish nothing: the thread is still hidden in the database.
func TestDMVisibility_RolledBackMessagePublishesNoRespawn(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "respawn_rollback_actor")
	peer := ts.CreateTestUser(t, "respawn_rollback_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	peerConn := connectVisibilityWS(t, ts, peer.ID, convID, true)
	actorConn := hideForActor(t, ts, actor, convID)
	fileID := insertDMMediaFile(t, ts, peer.ID, convID, "file", "application/octet-stream", 1)

	// Fails linkDMAttachments, which runs after Respawn inside the same transaction.
	_, err := ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION test_reject_dm_respawn_attachment() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced attachment link failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_dm_respawn_attachment
		BEFORE INSERT ON dm_message_attachments
		FOR EACH ROW EXECUTE FUNCTION test_reject_dm_respawn_attachment()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_reject_dm_respawn_attachment ON dm_message_attachments;
			DROP FUNCTION IF EXISTS test_reject_dm_respawn_attachment()`)
		if cleanupErr != nil {
			t.Errorf("cleanup attachment trigger: %v", cleanupErr)
		}
	})

	require.NoError(t, peerConn.WriteJSON(map[string]interface{}{
		"type": "dm_message",
		"data": map[string]interface{}{
			"conversation_id": convID,
			"content":         "rolled back",
			"key_version":     1,
			"nonce":           "rollback-nonce",
			"attachment_ids":  []string{fileID},
		},
	}))
	readVisibilityFrame(t, peerConn, "error")

	require.NoError(t, actorConn.WriteJSON(map[string]interface{}{
		"type": "connection_ready_probe",
		"data": map[string]interface{}{"protocol_version": 2},
	}))
	readUntilWithoutVisibilityEvent(t, actorConn, "connection_ready")

	var stillHidden bool
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_at IS NOT NULL FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, actor.ID).Scan(&stillHidden))
	assert.True(t, stillHidden, "the rolled-back respawn left the thread hidden")
}

// readUntilWithoutVisibilityEvent reads up to want and fails on any visibility event seen
// on the way, which readVisibilityFrame would silently skip.
func readUntilWithoutVisibilityEvent(t *testing.T, conn *gorillaWS.Conn, want string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		var frame visibilityWSFrame
		require.NoError(t, conn.ReadJSON(&frame))
		if frame.Type == "dm_conversation_hidden" || frame.Type == "dm_conversation_cleared" {
			t.Fatalf("received visibility event %q before %q", frame.Type, want)
		}
		if frame.Type == want {
			return
		}
	}
}
