package messages_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reactionsPath = "/reactions"
	messagesPath  = "/api/v1/messages/"
	testContent   = "Hello!"
)

// reactURL builds a PUT/GET URL for a message's reactions endpoint.
func reactURL(msgID string) string {
	return messagesPath + msgID + reactionsPath
}

// emojiBody returns a request body for toggling a reaction.
func emojiBody(emoji string) map[string]interface{} {
	return map[string]interface{}{"emoji": emoji}
}

// --- Toggle Reaction Tests ---

func TestToggleReactionAddSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "reactuser")
	serverID := ts.CreateTestServer(t, user.ID, "Reaction Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, testContent)

	w := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "added", resp["action"])

	reaction := resp["reaction"].(map[string]interface{})
	assert.Equal(t, "👍", reaction["emoji"])
	assert.Equal(t, float64(1), reaction["count"])
	assert.Equal(t, true, reaction["me"])
}

func TestToggleReactionRemoveSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "toggleuser")
	serverID := ts.CreateTestServer(t, user.ID, "Toggle Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, testContent)

	// Add reaction
	ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))

	// Toggle off
	w := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "removed", resp["action"])
	assert.Nil(t, resp["reaction"])
}

func TestToggleReactionMultipleEmoji(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "multiemoji")
	serverID := ts.CreateTestServer(t, user.ID, "Multi Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, testContent)

	w1 := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := ts.DoRequest("PUT", reactURL(msgID), emojiBody("❤️"),
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w2.Code)

	w := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Reactions []models.ReactionSummary `json:"reactions"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Len(t, resp.Reactions, 2)
}

func TestToggleReactionMultipleUsers(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reactor1")
	user2 := ts.CreateTestUser(t, "reactor2")
	serverID := ts.CreateTestServer(t, user1.ID, "Multi User Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, user2.ID, "member")
	msgID := ts.CreateTestMessage(t, channelID, user1, "React to me!")

	ts.DoRequest("PUT", reactURL(msgID), emojiBody("🎉"),
		testhelpers.AuthHeaders(user1.AccessToken))

	w := ts.DoRequest("PUT", reactURL(msgID), emojiBody("🎉"),
		testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	reaction := resp["reaction"].(map[string]interface{})
	assert.Equal(t, float64(2), reaction["count"])
	users := reaction["users"].([]interface{})
	assert.Len(t, users, 2)
}

func TestToggleReactionInvalidEmoji(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bademojiuser")
	serverID := ts.CreateTestServer(t, user.ID, "Bad Emoji Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, testContent)

	w := ts.DoRequest("PUT", reactURL(msgID), emojiBody("hello"),
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestToggleReactionEmptyEmoji(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "emptyemoji")
	serverID := ts.CreateTestServer(t, user.ID, "Empty Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, testContent)

	w := ts.DoRequest("PUT", reactURL(msgID), emojiBody(""),
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestToggleReactionInvalidMessageID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "invalidmsg")
	_ = ts.CreateTestServer(t, user.ID, "Invalid Server")

	w := ts.DoRequest("PUT", messagesPath+"not-a-uuid"+reactionsPath,
		emojiBody("👍"), testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestToggleReactionMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "notfounduser")
	_ = ts.CreateTestServer(t, user.ID, "NotFound Server")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("PUT", reactURL(fakeID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestToggleReactionNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "reactowner")
	outsider := ts.CreateTestUser(t, "reactoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Private Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, owner, testContent)

	w := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Get Reactions Tests ---

func TestGetReactionsSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getreactuser")
	serverID := ts.CreateTestServer(t, user.ID, "GetReact Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, testContent)

	ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))

	w := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Reactions []models.ReactionSummary `json:"reactions"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.Len(t, resp.Reactions, 1)
	assert.Equal(t, "👍", resp.Reactions[0].Emoji)
	assert.Equal(t, 1, resp.Reactions[0].Count)
	assert.True(t, resp.Reactions[0].Me)
	assert.Len(t, resp.Reactions[0].Users, 1)
}

func TestGetReactionsEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "noreactuser")
	serverID := ts.CreateTestServer(t, user.ID, "Empty React Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "No reactions here")

	w := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Reactions []models.ReactionSummary `json:"reactions"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Len(t, resp.Reactions, 0)
}

func TestGetReactionsNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "getreactowner")
	outsider := ts.CreateTestUser(t, "getreactoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Private React Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, owner, testContent)

	w := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- GetMessages Integration ---

func TestGetMessagesIncludesReactions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "msgreactuser")
	serverID := ts.CreateTestServer(t, user.ID, "MsgReact Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "React to this!")

	ts.DoRequest("PUT", reactURL(msgID), emojiBody("🎉"),
		testhelpers.AuthHeaders(user.AccessToken))

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 1)

	var msg struct {
		ID        string                   `json:"id"`
		Reactions []models.ReactionSummary `json:"reactions"`
	}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &msg))
	assert.Equal(t, msgID, msg.ID)
	require.Len(t, msg.Reactions, 1)
	assert.Equal(t, "🎉", msg.Reactions[0].Emoji)
	assert.Equal(t, 1, msg.Reactions[0].Count)
}

func TestGetMessagesNoReactionsOmitsField(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "noreactmsguser")
	serverID := ts.CreateTestServer(t, user.ID, "NoReact Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.CreateTestMessage(t, channelID, user, "Plain message")

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 1)

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &raw))
	_, hasReactions := raw["reactions"]
	assert.False(t, hasReactions, "reactions field should be omitted when empty")
}

// --- DM message reaction tests ---

// TestReactions_DMMessage_Participant_Returns404 verifies that a valid DM
// participant receives 404 (not 200) when attempting to toggle or fetch
// reactions on a DM message. DM reactions are intentionally not yet supported;
// the handler resolves the DM context successfully but then returns 404 because
// the feature gate is not open. This is distinct from the non-participant case.
func TestReactions_DMMessage_Participant_Returns404(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmreact_part_a")
	u2 := ts.CreateTestUser(t, "dmreact_part_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "react me")

	wPut := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusNotFound, wPut.Code,
		"participant should get 404 (DM reactions not supported): %s", wPut.Body.String())

	wGet := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusNotFound, wGet.Code,
		"participant should get 404 on GET reactions (DM reactions not supported): %s", wGet.Body.String())
}

// TestReactions_DMMessage_NonParticipant_Returns404 verifies the new DM
// participation check in lookupMessageContext: a user who is not a participant
// of the DM conversation receives 404 (not 403) to avoid leaking that the
// conversation or message exists.
func TestReactions_DMMessage_NonParticipant_Returns404(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmreact_np_a")
	u2 := ts.CreateTestUser(t, "dmreact_np_b")
	outsider := ts.CreateTestUser(t, "dmreact_np_c")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "secret")

	wPut := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusNotFound, wPut.Code,
		"non-participant should get 404, not 403 (privacy): %s", wPut.Body.String())

	wGet := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusNotFound, wGet.Code,
		"non-participant should get 404 on GET reactions (privacy): %s", wGet.Body.String())
}

func TestToggleReactionCascadeOnDelete(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cascadeuser")
	serverID := ts.CreateTestServer(t, user.ID, "Cascade Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Delete me")

	ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(user.AccessToken))

	w := ts.DoRequest("DELETE", messagesPath+msgID, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	w2 := ts.DoRequest("GET", reactURL(msgID), nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w2.Code)
}
