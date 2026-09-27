package messages_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A timeout can be applied after the request-time permission preflight. The
// mutation transaction must re-check the member row before deleting.
func TestDeleteMessage_TimedOutAuthorIsRejectedByMutationFence(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mut2_timeout_owner")
	author := ts.CreateTestUser(t, "mut2_timeout_author")
	serverID := ts.CreateTestServer(t, owner.ID, "mutation timeout fence")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, author.ID, "member")
	messageID := ts.CreateTestMessage(t, channelID, author, testhelpers.ValidCiphertext())

	_, err := ts.DB.Exec(`UPDATE server_members
		SET timed_out_until = clock_timestamp() + interval '10 minutes'
		WHERE server_id = $1 AND user_id = $2`, serverID, author.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, pathAPIMsgSlash+messageID, nil,
		testhelpers.AuthHeaders(author.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, messageID).Scan(&count))
	assert.Equal(t, 1, count, "timed-out author must not delete a message")
}

// Removing a participant after a DM message was created must close the
// mutation path as well as the lookup path; pinning must not resurrect access.
func TestPinMessage_DMRemovedParticipantCannotMutate(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "mut2_dm_removed")
	peer := ts.CreateTestUser(t, "mut2_dm_peer")
	conversationID := ts.CreateDMConversation(t, user.ID, peer.ID)
	messageID := insertDMMessageDirect(t, ts, conversationID, user.ID, "membership fence")

	_, err := ts.DB.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPost, pinAPIMsg+messageID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	var pinnedAt *string
	require.NoError(t, ts.DB.QueryRow(`SELECT pinned_at::text FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt))
	assert.Nil(t, pinnedAt)
}
