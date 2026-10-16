package users_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateMeProfileAudience(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "profileowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Profile audience")
	conversationID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO dm_conversations (id, created_by, is_group) VALUES ($1, $2, TRUE)`, conversationID, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, owner.ID)
	require.NoError(t, err)

	type observer struct {
		name    string
		userID  string
		conn    *gorillaWS.Conn
		allowed bool
	}
	cases := []struct {
		name    string
		friend  string
		server  bool
		dm      bool
		direct  bool
		hidden  bool
		allowed bool
	}{
		{name: "outgoing friend", friend: "accepted", allowed: true},
		{name: "incoming friend", friend: "reverse", allowed: true},
		{name: "server peer", server: true, allowed: true},
		{name: "group DM peer", dm: true, allowed: true},
		{name: "direct DM peer", dm: true, direct: true, allowed: true},
		{name: "hidden DM peer", dm: true, hidden: true, allowed: true},
		{name: "overlapping relationships", friend: "accepted", server: true, dm: true, allowed: true},
		{name: "unrelated"},
		{name: "pending friend", friend: "pending"},
		{name: "friend of friend", friend: "fof"},
		{name: "outgoing block", friend: "blocked", server: true, dm: true},
		{name: "incoming block", friend: "blocked_reverse", server: true, dm: true},
	}
	observers := make([]observer, 0, len(cases)+2)
	observers = append(observers,
		observer{name: "owner", userID: owner.ID, conn: dialProfileObserver(t, ts, owner.ID), allowed: true},
		observer{name: "owner second session", userID: owner.ID, conn: dialProfileObserver(t, ts, owner.ID), allowed: true},
	)
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(owner.ID)) == 2
	}, 5*time.Second, 10*time.Millisecond)
	var blocks [][2]string
	for i, tc := range cases {
		peer := ts.CreateTestUser(t, fmt.Sprintf("profilepeer%d", i))
		if tc.friend == "fof" {
			bridge := ts.CreateTestUser(t, "profilebridge")
			_, err = ts.DB.Exec(`INSERT INTO friendships (requester_id, addressee_id, status)
				VALUES ($1, $2, 'accepted'), ($2, $3, 'accepted')`, owner.ID, bridge.ID, peer.ID)
			require.NoError(t, err)
		} else if tc.friend == "blocked" {
			blocks = append(blocks, [2]string{owner.ID, peer.ID})
		} else if tc.friend == "blocked_reverse" {
			blocks = append(blocks, [2]string{peer.ID, owner.ID})
		} else if tc.friend != "" {
			from, to, status := owner.ID, peer.ID, tc.friend
			if tc.friend == "reverse" {
				from, to, status = peer.ID, owner.ID, "accepted"
			}
			_, err = ts.DB.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, $3)`, from, to, status)
			require.NoError(t, err)
		}
		if tc.server {
			ts.AddMemberToServer(t, serverID, peer.ID, "member")
		}
		if tc.dm {
			peerConversationID := conversationID
			if tc.direct {
				peerConversationID = uuid.NewString()
				_, err = ts.DB.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, peerConversationID, owner.ID)
				require.NoError(t, err)
				_, err = ts.DB.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, peerConversationID, owner.ID)
				require.NoError(t, err)
			}
			_, err = ts.DB.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, peerConversationID, peer.ID)
			require.NoError(t, err)
			if tc.hidden {
				_, err = ts.DB.Exec(`UPDATE dm_participants SET hidden_at = NOW() WHERE conversation_id = $1 AND user_id = $2`, peerConversationID, peer.ID)
				require.NoError(t, err)
			}
		}
		observers = append(observers, observer{tc.name, peer.ID, dialProfileObserver(t, ts, peer.ID), tc.allowed})
	}
	for _, pair := range blocks {
		tx, err := ts.DB.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'blocked')`, pair[0], pair[1])
		require.NoError(t, err)
		require.NoError(t, dmblock.RecordBlockTx(t.Context(), tx, pair[0], pair[1], uuid.NewString()))
		require.NoError(t, tx.Commit())
	}

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]string{"display_name": "Scoped profile"}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	// Reaching the owner's frame also drains the old global publisher before
	// the per-user barriers, so this regression fails on the vulnerable code.
	payload := readProfilePayload(t, observers[0].conn)
	assert.Equal(t, owner.ID, payload["user_id"])
	assert.Equal(t, owner.Username, payload["username"])
	assert.Equal(t, "Scoped profile", payload["display_name"])
	for _, field := range []string{"avatar_url", "header_image_url", "color_scheme"} {
		assert.Contains(t, payload, field)
		assert.Nil(t, payload[field])
	}
	for i, obs := range observers {
		ts.Hub.BroadcastToUser(uuid.MustParse(obs.userID), concordws.OutgoingMessage{Type: "profile_audience_barrier"})
		seen := readProfileWireUntil(t, obs.conn, "profile_audience_barrier")
		if i == 0 {
			assert.NotContains(t, seen, "profile_updated", "owner should receive one event")
		} else if obs.allowed {
			assert.Equal(t, 1, countProfileUpdates(seen), obs.name)
		} else {
			assert.NotContains(t, seen, "profile_updated", obs.name)
		}
	}

	// Revoking each relationship must take effect on the next edit, without
	// relying on a client unsubscribe or disconnect.
	for _, obs := range observers[2:] {
		if !obs.allowed {
			continue
		}
		testhelpers.RemoveFriendship(t, ts.DB, uuid.MustParse(owner.ID), uuid.MustParse(obs.userID))
		_, err = ts.DB.Exec(`DELETE FROM server_members WHERE user_id = $1`, obs.userID)
		require.NoError(t, err)
		_, err = ts.DB.Exec(`DELETE FROM dm_participants WHERE user_id = $1`, obs.userID)
		require.NoError(t, err)
	}
	w = ts.DoRequest(methodPatch, urlUsersMe, map[string]string{"display_name": ""}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	payload = readProfilePayload(t, observers[0].conn)
	assert.Contains(t, payload, "display_name")
	assert.Nil(t, payload["display_name"], "clears retain the nullable wire contract")
	for i, obs := range observers {
		ts.Hub.BroadcastToUser(uuid.MustParse(obs.userID), concordws.OutgoingMessage{Type: "profile_revocation_barrier"})
		seen := readProfileWireUntil(t, obs.conn, "profile_revocation_barrier")
		if i == 1 {
			assert.Equal(t, 1, countProfileUpdates(seen), "owner second session still receives clears")
		} else {
			assert.NotContains(t, seen, "profile_updated", obs.name)
		}
	}
}

func readProfilePayload(t *testing.T, conn *gorillaWS.Conn) map[string]interface{} {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		var frame struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		require.NoError(t, conn.ReadJSON(&frame))
		if frame.Type == "profile_updated" {
			return frame.Data
		}
	}
}

func countProfileUpdates(types []string) int {
	count := 0
	for _, typ := range types {
		if typ == "profile_updated" {
			count++
		}
	}
	return count
}

func TestUpdateMeProfileAudienceFailureDoesNotUndoSaveOrBroadcast(t *testing.T) {
	hook, db := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, db)
	owner := ts.CreateTestUser(t, "profilequeryowner")
	friend := ts.CreateTestUser(t, "profilequeryfriend")
	_, err := ts.DB.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, owner.ID, friend.ID)
	require.NoError(t, err)
	conn := dialProfileObserver(t, ts, friend.ID)
	fired := false
	hook.Arm([]string{"WITH profile_recipients AS"}, func() error { fired = true; return nil }, stmthook.ErrInjected)

	h := users.NewHandler(db, logger.New("test"), ts.Hub, nil, profileMediaTierResolver{}, nil, nil)
	w := invokeProfilePatch(t, h, owner.ID, map[string]string{"display_name": "Saved despite delivery failure"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, fired, "the audience read must fail")
	var saved string
	require.NoError(t, ts.DB.QueryRow(`SELECT display_name FROM users WHERE id = $1`, owner.ID).Scan(&saved))
	assert.Equal(t, "Saved despite delivery failure", saved)
	ts.Hub.BroadcastToUser(uuid.MustParse(friend.ID), concordws.OutgoingMessage{Type: "profile_failure_barrier"})
	assert.NotContains(t, readProfileWireUntil(t, conn, "profile_failure_barrier"), "profile_updated")
}

func TestUpdateMeProfileAudienceSurvivesRequestCancellationAfterCommit(t *testing.T) {
	hook, db := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, db)
	owner := ts.CreateTestUser(t, "profilecancelowner")
	friend := ts.CreateTestUser(t, "profilecancelfriend")
	_, err := ts.DB.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, owner.ID, friend.ID)
	require.NoError(t, err)
	conn := dialProfileObserver(t, ts, friend.ID)
	requestCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fired := false
	hook.Arm([]string{"WITH profile_recipients AS"}, func() error {
		fired = true
		cancel()
		return nil
	}, nil)

	h := users.NewHandler(db, logger.New("test"), ts.Hub, nil, profileMediaTierResolver{}, nil, nil)
	body, err := json.Marshal(map[string]string{"display_name": "Committed before disconnect"})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, urlUsersMe, bytes.NewReader(body)).WithContext(requestCtx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", owner.ID)
	h.UpdateMe(c)
	require.True(t, fired, "the request must cancel after the update commits")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, owner.ID, readProfilePayload(t, conn)["user_id"])
}

func TestUpdateMeProfileAudienceQueueSaturationIsBounded(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "profilefullqueue")
	stalledHub := concordws.NewHub(ts.DB, nil)
	for range 256 {
		require.True(t, stalledHub.BroadcastToUserContext(t.Context(), uuid.MustParse(owner.ID), concordws.OutgoingMessage{Type: "queue_filler"}))
	}
	h := users.NewHandler(ts.DB, logger.New("test"), stalledHub, nil, profileMediaTierResolver{}, nil, nil)
	body, err := json.Marshal(map[string]string{"display_name": "Saved despite full queue"})
	require.NoError(t, err)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPatch, urlUsersMe, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", owner.ID)
		h.UpdateMe(c)
		done <- w
	}()
	select {
	case w := <-done:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	case <-time.After(3 * time.Second):
		t.Fatal("a full hub queue held the HTTP handler after the profile edit committed")
	}
	var saved string
	require.NoError(t, ts.DB.QueryRow(`SELECT display_name FROM users WHERE id = $1`, owner.ID).Scan(&saved))
	assert.Equal(t, "Saved despite full queue", saved)
}
