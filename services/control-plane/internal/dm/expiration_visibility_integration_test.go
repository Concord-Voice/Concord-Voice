package dm_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMExpirationChangedPolicyRespawnsHiddenParticipantsAndNoOpDoesNot(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "expiration_visibility_actor")
	peer := ts.CreateTestUser(t, "expiration_visibility_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	conversationPath := pathDMConversationsPrefix + conversationID

	hideBoth := func() {
		t.Helper()
		for _, user := range []testhelpers.TestUser{actor, peer} {
			response := ts.DoRequest(http.MethodPost, conversationPath+"/hide", nil, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		}
	}
	assertHidden := func(want bool) {
		t.Helper()
		var hidden int
		require.NoError(t, ts.DB.QueryRow(`
			SELECT count(*) FROM dm_participants
			WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, conversationID).Scan(&hidden))
		if want {
			assert.Equal(t, 2, hidden)
		} else {
			assert.Zero(t, hidden)
		}
	}

	// Establish the previous policy, then hide both participants again so the
	// changed-policy event is the operation that must restore visibility.
	response := ts.DoRequest(http.MethodPatch, conversationPath+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"},
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	hideBoth()
	assertHidden(true)

	response = ts.DoRequest(http.MethodPatch, conversationPath+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"},
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assertHidden(false)

	var eventCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM dm_messages
		WHERE conversation_id = $1 AND type = 'expiration_event'`, conversationID).Scan(&eventCount))
	assert.Equal(t, 2, eventCount, "initial set and changed policy each persist one event")

	history := func(user testhelpers.TestUser) []struct {
		ID                     string          `json:"id"`
		UserID                 string          `json:"user_id"`
		Type                   string          `json:"type"`
		CreatedAt              time.Time       `json:"created_at"`
		ExpirationEventPayload json.RawMessage `json:"expiration_event_payload"`
	} {
		t.Helper()
		response := ts.DoRequest(http.MethodGet, conversationPath+"/messages", nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var body struct {
			Messages []struct {
				ID                     string          `json:"id"`
				UserID                 string          `json:"user_id"`
				Type                   string          `json:"type"`
				CreatedAt              time.Time       `json:"created_at"`
				ExpirationEventPayload json.RawMessage `json:"expiration_event_payload"`
			} `json:"messages"`
		}
		testhelpers.ParseJSON(t, response, &body)
		return body.Messages
	}

	for _, user := range []testhelpers.TestUser{actor, peer} {
		messages := history(user)
		var events []struct {
			ID                     string
			UserID                 string
			CreatedAt              time.Time
			ExpirationEventPayload json.RawMessage
		}
		for _, message := range messages {
			if message.Type == "expiration_event" {
				events = append(events, struct {
					ID                     string
					UserID                 string
					CreatedAt              time.Time
					ExpirationEventPayload json.RawMessage
				}{message.ID, message.UserID, message.CreatedAt, message.ExpirationEventPayload})
			}
		}
		require.Len(t, events, 2, "history exposes the server-authored expiration rows to actor and peer")
		for _, event := range events {
			assert.Equal(t, actor.ID, event.UserID, "the actor is the plain message author")
			var payload struct {
				ActorUserID string    `json:"actor_user_id"`
				ChangedAt   time.Time `json:"changed_at"`
			}
			require.NoError(t, json.Unmarshal(event.ExpirationEventPayload, &payload))
			assert.Equal(t, actor.ID, payload.ActorUserID)
			assert.True(t, event.CreatedAt.Equal(payload.ChangedAt), "event row and payload retain the accepted change timestamp")
		}
	}

	hideBoth()
	assertHidden(true)
	response = ts.DoRequest(http.MethodPatch, conversationPath+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"},
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assertHidden(true)
	var unchangedCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM dm_messages
		WHERE conversation_id = $1 AND type = 'expiration_event'`, conversationID).Scan(&unchangedCount))
	assert.Equal(t, eventCount, unchangedCount, "a policy no-op emits no additional system row")
}

func TestDMExpirationHistoryHidesExpirationEventsInsideClearRange(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "expiration_history_clear_actor")
	peer := ts.CreateTestUser(t, "expiration_history_clear_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	conversationPath := pathDMConversationsPrefix + conversationID

	response := ts.DoRequest(http.MethodPatch, conversationPath+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"},
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	_, err := ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, false)
		ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = false`, actor.ID)
	require.NoError(t, err)

	response = ts.DoRequest(http.MethodPost, conversationPath+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	response = ts.DoRequest(http.MethodGet, conversationPath+"/messages", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var actorBody struct {
		Messages []struct {
			Type string `json:"type"`
		} `json:"messages"`
	}
	testhelpers.ParseJSON(t, response, &actorBody)
	for _, message := range actorBody.Messages {
		assert.NotEqual(t, "expiration_event", message.Type, "clear history filters expiration system rows for the clearing participant")
	}

	response = ts.DoRequest(http.MethodGet, conversationPath+"/messages", nil, testhelpers.AuthHeaders(peer.AccessToken))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var peerBody struct {
		Messages []struct {
			Type string `json:"type"`
		} `json:"messages"`
	}
	testhelpers.ParseJSON(t, response, &peerBody)
	var peerEvents int
	for _, message := range peerBody.Messages {
		if message.Type == "expiration_event" {
			peerEvents++
		}
	}
	assert.Equal(t, 1, peerEvents, "peer history retains the event and its plain system-message type")
}
