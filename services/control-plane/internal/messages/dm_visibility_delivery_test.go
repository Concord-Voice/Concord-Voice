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

func TestDMPinVisibility_ClearRangeHidesActorButPreservesPeer(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "dmvisibility_pin_actor")
	peer := ts.CreateTestUser(t, "dmvisibility_pin_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	msgID := insertDMMessageDirect(t, ts, convID, peer.ID, "private pin target")

	require.Equal(t, http.StatusOK, ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(peer.AccessToken)).Code)
	markDMHistoryCleared(t, ts, actor.ID, convID)

	t.Run("cleared actor cannot read or mutate hidden pin target", func(t *testing.T) {
		read := ts.DoRequest("GET", pinAPICh+convID+pinsPath, nil,
			testhelpers.AuthHeaders(actor.AccessToken))
		require.Equal(t, http.StatusOK, read.Code, read.Body.String())
		var body struct {
			Count int `json:"count"`
		}
		testhelpers.ParseJSON(t, read, &body)
		assert.Equal(t, 0, body.Count)

		mutate := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
			testhelpers.AuthHeaders(actor.AccessToken))
		assert.Equal(t, http.StatusNotFound, mutate.Code, mutate.Body.String())
	})

	t.Run("peer still reads the pin", func(t *testing.T) {
		read := ts.DoRequest("GET", pinAPICh+convID+pinsPath, nil,
			testhelpers.AuthHeaders(peer.AccessToken))
		require.Equal(t, http.StatusOK, read.Code, read.Body.String())
		var body struct {
			Count int `json:"count"`
		}
		testhelpers.ParseJSON(t, read, &body)
		assert.Equal(t, 1, body.Count)
	})
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
