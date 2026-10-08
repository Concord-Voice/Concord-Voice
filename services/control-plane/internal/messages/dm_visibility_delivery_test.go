package messages_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markDMHistoryCleared seeds the persisted consumer state produced by Clear.
// Keeping the setup at the database boundary makes these tests exercise the
// actual reaction and pin readers rather than a visibility helper in isolation.
func markDMHistoryCleared(t *testing.T, ts *testhelpers.TestServer, userID, convID string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO dm_message_hidden_ranges
			(user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', NOW(), TRUE)`, userID, convID)
	require.NoError(t, err)
}

func TestDMReactionVisibility_ClearRangeHidesActorButPreservesPeer(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dmvisibility_reaction_actor")
	peer := ts.CreateTestUser(t, "dmvisibility_reaction_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessageDirect(t, ts, convID, actor.ID, "private reaction target")

	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", reactURL(msgID),
		emojiBody("👍"), testhelpers.AuthHeaders(peer.AccessToken)).Code)
	markDMHistoryCleared(t, ts, actor.ID, convID)

	t.Run("cleared actor cannot read or mutate hidden message", func(t *testing.T) {
		read := ts.DoRequest("GET", reactURL(msgID), nil, testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusNotFound, read.Code, read.Body.String())

		mutate := ts.DoRequest("PUT", reactURL(msgID), emojiBody("❤️"),
			testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusNotFound, mutate.Code, mutate.Body.String())
	})

	t.Run("peer still reads the reaction", func(t *testing.T) {
		read := ts.DoRequest("GET", reactURL(msgID), nil, testhelpers.AuthHeaders(peer.AccessToken))
		require.Equal(t, http.StatusOK, read.Code, read.Body.String())
		var body struct {
			Reactions []struct {
				Emoji string `json:"emoji"`
			} `json:"reactions"`
		}
		testhelpers.ParseJSON(t, read, &body)
		require.Len(t, body.Reactions, 1)
		assert.Equal(t, "👍", body.Reactions[0].Emoji)
	})
}

func TestDMReactionVisibility_LegacyRangeKeepsActorMessagesVisible(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dmvisibility_legacy_actor")
	peer := ts.CreateTestUser(t, "dmvisibility_legacy_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessageDirect(t, ts, convID, actor.ID, "legacy target")

	require.Equal(t, http.StatusOK, ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(peer.AccessToken)).Code)
	_, err := ts.DB.Exec(`
		INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', NOW(), FALSE)`, actor.ID, convID)
	require.NoError(t, err)

	actorRead := ts.DoRequest("GET", reactURL(msgID), nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, actorRead.Code, actorRead.Body.String())
	var actorBody struct {
		Reactions []struct {
			Emoji string `json:"emoji"`
		} `json:"reactions"`
	}
	testhelpers.ParseJSON(t, actorRead, &actorBody)
	require.Len(t, actorBody.Reactions, 1)
	assert.Equal(t, "👍", actorBody.Reactions[0].Emoji)
}

// dmPinCount returns the pin count the token's user reads for convID.
func dmPinCount(t *testing.T, ts *testhelpers.TestServer, convID, token string) int {
	t.Helper()
	read := ts.DoRequest("GET", pinAPICh+convID+pinsPath, nil, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	var body struct {
		Count int `json:"count"`
	}
	testhelpers.ParseJSON(t, read, &body)
	return body.Count
}

// dmVisibleMessageIDs returns the message IDs the token's user reads from the
// conversation's history, through the DM fetch's hidden-range filter.
func dmVisibleMessageIDs(t *testing.T, ts *testhelpers.TestServer, convID, token string) []string {
	t.Helper()
	read := ts.DoRequest("GET", "/api/v1/dm/conversations/"+convID+"/messages", nil, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	var body struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	testhelpers.ParseJSON(t, read, &body)
	ids := make([]string, 0, len(body.Messages))
	for _, message := range body.Messages {
		ids = append(ids, message.ID)
	}
	return ids
}

// A pinned message is never hidden (#3458 §18.1): Clear leaves the pin and
// the message visible to the actor, who can still unpin it, and the unpin
// returns the message to the history the actor cleared.
func TestDMPinVisibility_ClearRangeKeepsPinVisible(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dmvisibility_pin_actor")
	peer := ts.CreateTestUser(t, "dmvisibility_pin_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessageDirect(t, ts, convID, peer.ID, "private pin target")

	require.Equal(t, http.StatusOK, ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(peer.AccessToken)).Code)
	markDMHistoryCleared(t, ts, actor.ID, convID)

	t.Run("cleared actor still reads the pin and the message", func(t *testing.T) {
		assert.Equal(t, 1, dmPinCount(t, ts, convID, actor.AccessToken))
		assert.Contains(t, dmVisibleMessageIDs(t, ts, convID, actor.AccessToken), msgID)
	})

	t.Run("actor unpins it and the cleared range hides it again", func(t *testing.T) {
		unpin := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
			testhelpers.AuthHeaders(actor.AccessToken))
		require.Equal(t, http.StatusOK, unpin.Code, unpin.Body.String())
		assert.NotContains(t, dmVisibleMessageIDs(t, ts, convID, actor.AccessToken), msgID)
		assert.Equal(t, 0, dmPinCount(t, ts, convID, actor.AccessToken))
	})

	t.Run("peer still reads the unpinned message", func(t *testing.T) {
		assert.Contains(t, dmVisibleMessageIDs(t, ts, convID, peer.AccessToken), msgID)
	})
}

// A pinned message under the actor's Clear range stays reactable (#3458
// §18.1), and once it is unpinned the range hides it from the reaction reader
// and writer again.
func TestDMReactionVisibility_PinnedMessageUnderClearRange(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dmvisibility_pinreact_actor")
	peer := ts.CreateTestUser(t, "dmvisibility_pinreact_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessageDirect(t, ts, convID, actor.ID, "pinned reaction target")

	require.Equal(t, http.StatusOK, ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(peer.AccessToken)).Code)
	markDMHistoryCleared(t, ts, actor.ID, convID)

	react := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"), testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, react.Code, react.Body.String())
	read := ts.DoRequest("GET", reactURL(msgID), nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	var body struct {
		Reactions []struct {
			Emoji string `json:"emoji"`
		} `json:"reactions"`
	}
	testhelpers.ParseJSON(t, read, &body)
	require.Len(t, body.Reactions, 1)
	assert.Equal(t, "👍", body.Reactions[0].Emoji)

	unpin := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil, testhelpers.AuthHeaders(peer.AccessToken))
	require.Equal(t, http.StatusOK, unpin.Code, unpin.Body.String())

	hiddenRead := ts.DoRequest("GET", reactURL(msgID), nil, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusNotFound, hiddenRead.Code, hiddenRead.Body.String())
	hiddenReact := ts.DoRequest("PUT", reactURL(msgID), emojiBody("❤️"), testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusNotFound, hiddenReact.Code, hiddenReact.Body.String())
	var hearts int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_message_reactions WHERE message_id = $1 AND emoji = $2`, msgID, "❤️",
	).Scan(&hearts))
	assert.Zero(t, hearts, "a reaction to a message the range hides again must not be written")
}

// TestDMReactionVisibility_ClearFirstDoesNotMutate proves the hidden-range
// predicate is inside the reaction transaction: a Clear that commits before
// the toggle makes the toggle a 404 and leaves no shared reaction row behind.
func TestDMReactionVisibility_ClearFirstDoesNotMutate(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dmvisibility_reaction_clear_first_actor")
	peer := ts.CreateTestUser(t, "dmvisibility_reaction_clear_first_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessageDirect(t, ts, convID, peer.ID, "hidden before reaction")
	markDMHistoryCleared(t, ts, actor.ID, convID)

	response := ts.DoRequest("PUT", reactURL(msgID), emojiBody("👍"),
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_message_reactions WHERE message_id = $1 AND user_id = $2`,
		msgID, actor.ID,
	).Scan(&count))
	assert.Zero(t, count, "Clear-first reaction rejection must not mutate shared state")

	peerRead := ts.DoRequest("GET", reactURL(msgID), nil, testhelpers.AuthHeaders(peer.AccessToken))
	require.Equal(t, http.StatusOK, peerRead.Code, peerRead.Body.String())
	var body struct {
		Reactions []struct{} `json:"reactions"`
	}
	testhelpers.ParseJSON(t, peerRead, &body)
	assert.Empty(t, body.Reactions)
}
