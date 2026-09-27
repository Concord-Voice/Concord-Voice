package messages_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMMutationsRejectStaleCredentialEpoch(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dmstale_mutator")
	peer := ts.CreateTestUser(t, "dmstale_peer")
	conversationID := ts.CreateDMConversation(t, user.ID, peer.ID)
	messageID := insertDMMessageDirect(t, ts, conversationID, user.ID, "stale mutation")
	staleToken := ts.SimulateStaleEpochWindow(t, user.ID)
	auth := testhelpers.AuthHeaders(staleToken)

	t.Run("pin", func(t *testing.T) {
		w := ts.DoRequest("POST", pinAPIMsg+messageID+pinPath, nil, auth)
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	})
	t.Run("unpin", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pinAPIMsg+messageID+pinPath, nil, auth)
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	})
	t.Run("reaction", func(t *testing.T) {
		w := ts.DoRequest("PUT", reactURL(messageID), emojiBody("👍"), auth)
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	})

	var pins, reactions int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_messages WHERE id = $1 AND pinned_at IS NOT NULL`, messageID).Scan(&pins))
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_message_reactions WHERE message_id = $1`, messageID).Scan(&reactions))
	assert.Zero(t, pins)
	assert.Zero(t, reactions)
}
