package messages_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bodyFmtPlaceholder = "body=%s"

// insertDMMessageDirect writes a DM message row directly and returns its id.
// Tests avoid the DM send API because this package does not own those routes.
func insertDMMessageDirect(t *testing.T, ts *testhelpers.TestServer, convID, userID, content string) string {
	t.Helper()
	msgID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, type)
		 VALUES ($1, $2, $3, $4, 'user')`,
		msgID, convID, userID, content,
	)
	require.NoError(t, err, "failed to insert DM message")
	return msgID
}

// --- Pin DM Message Tests ---

func TestPinMessage_DM_HappyPath(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmpin_a")
	u2 := ts.CreateTestUser(t, "dmpin_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "please pin me")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, bodyFmtPlaceholder, w.Body.String())

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, msgID, resp["message_id"])
	assert.NotNil(t, resp["pinned_at"])
	assert.NotNil(t, resp["pinned_by"])
	assert.Equal(t, convID, resp["conversation_id"])

	// Verify row state in the database.
	var pinnedAt *time.Time
	var pinnedBy *string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, msgID,
	).Scan(&pinnedAt, &pinnedBy))
	assert.NotNil(t, pinnedAt)
	require.NotNil(t, pinnedBy)
	assert.Equal(t, u1.ID, *pinnedBy)
}

func TestPinMessage_DM_NonParticipant_404(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmpin_np_a")
	u2 := ts.CreateTestUser(t, "dmpin_np_b")
	outsider := ts.CreateTestUser(t, "dmpin_np_c")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "private")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	// lookupMessageContext returns 404 for non-participants to avoid leaking
	// that the message/conversation exists (privacy-preserving, same as reactions.go).
	assert.Equal(t, http.StatusNotFound, w.Code, bodyFmtPlaceholder, w.Body.String())

	// Ensure the row is still unpinned.
	var pinnedAt *time.Time
	require.NoError(t, ts.DB.QueryRow(
		`SELECT pinned_at FROM dm_messages WHERE id = $1`, msgID,
	).Scan(&pinnedAt))
	assert.Nil(t, pinnedAt)
}

func TestPinMessage_DM_AlreadyPinned_Idempotent(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmpin_idem_a")
	u2 := ts.CreateTestUser(t, "dmpin_idem_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "twice")

	// First pin
	w1 := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	require.Equal(t, http.StatusOK, w1.Code)

	// Pin again — idempotent
	w2 := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusOK, w2.Code)
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w2, &resp)
	assert.Equal(t, true, resp["already_pinned"])
}

func TestUnpinMessage_DM_HappyPath(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmunpin_a")
	u2 := ts.CreateTestUser(t, "dmunpin_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "pin then unpin")

	// Pin
	wp := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	require.Equal(t, http.StatusOK, wp.Code)

	// Unpin
	wu := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(u2.AccessToken))
	assert.Equal(t, http.StatusOK, wu.Code, bodyFmtPlaceholder, wu.Body.String())

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, wu, &resp)
	assert.Equal(t, msgID, resp["message_id"])
	assert.Nil(t, resp["already_unpinned"])

	// Row should be fully unpinned.
	var pinnedAt *time.Time
	var pinnedBy *string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, msgID,
	).Scan(&pinnedAt, &pinnedBy))
	assert.Nil(t, pinnedAt)
	assert.Nil(t, pinnedBy)
}

func TestUnpinMessage_DM_AlreadyUnpinned(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmunpin_idem_a")
	u2 := ts.CreateTestUser(t, "dmunpin_idem_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "never pinned")

	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp["already_unpinned"])
}

func TestUnpinMessage_DM_NonParticipant_404(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmunpin_np_a")
	u2 := ts.CreateTestUser(t, "dmunpin_np_b")
	outsider := ts.CreateTestUser(t, "dmunpin_np_c")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "x")

	// Pin as participant.
	require.Equal(t, http.StatusOK,
		ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
			testhelpers.AuthHeaders(u1.AccessToken)).Code)

	// Outsider attempts to unpin — lookupMessageContext returns 404 (not 403)
	// to avoid leaking message/conversation existence.
	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetChannelPins_DM_ReturnsPins(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmgetpins_a")
	u2 := ts.CreateTestUser(t, "dmgetpins_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)

	// Create three messages, pin two of them.
	msg1 := insertDMMessageDirect(t, ts, convID, u1.ID, "pinned one")
	_ = insertDMMessageDirect(t, ts, convID, u2.ID, "not pinned")
	msg3 := insertDMMessageDirect(t, ts, convID, u1.ID, "pinned three")

	require.Equal(t, http.StatusOK,
		ts.DoRequest("POST", pinAPIMsg+msg1+pinPath, nil,
			testhelpers.AuthHeaders(u1.AccessToken)).Code)
	require.Equal(t, http.StatusOK,
		ts.DoRequest("POST", pinAPIMsg+msg3+pinPath, nil,
			testhelpers.AuthHeaders(u2.AccessToken)).Code)

	w := ts.DoRequest("GET", pinAPICh+convID+pinsPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, bodyFmtPlaceholder, w.Body.String())

	var resp struct {
		PinnedMessages []map[string]interface{} `json:"pinned_messages"`
		Count          int                      `json:"count"`
		ConversationID string                   `json:"conversation_id"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 2, resp.Count)
	assert.Len(t, resp.PinnedMessages, 2)
	assert.Equal(t, convID, resp.ConversationID)

	// Verify every returned message has pinned_at populated and belongs to this conversation.
	for _, m := range resp.PinnedMessages {
		assert.NotNil(t, m["pinned_at"])
		assert.Equal(t, convID, m["conversation_id"])
	}
}

func TestGetChannelPins_DM_NonParticipant_404(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmgetpins_np_a")
	u2 := ts.CreateTestUser(t, "dmgetpins_np_b")
	outsider := ts.CreateTestUser(t, "dmgetpins_np_c")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)

	w := ts.DoRequest("GET", pinAPICh+convID+pinsPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	// Returns 404 to prevent non-participants from probing conversation IDs.
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetChannelPins_DM_Empty(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmgetpins_empty_a")
	u2 := ts.CreateTestUser(t, "dmgetpins_empty_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	_ = insertDMMessageDirect(t, ts, convID, u1.ID, "hi")

	w := ts.DoRequest("GET", pinAPICh+convID+pinsPath, nil,
		testhelpers.AuthHeaders(u2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 0, resp.Count)
}

// --- lookupMessageContext regression coverage ---

func TestLookupMessageContext_ServerStillWorks(t *testing.T) {
	// Regression: the existing server-channel path must still pin correctly
	// after the DM fallback was added.
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lookup_server_ok")
	serverID := ts.CreateTestServer(t, user.ID, "Regression Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "still works")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPinMessage_DM_LimitReached(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmpin_limit_a")
	u2 := ts.CreateTestUser(t, "dmpin_limit_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)

	// Seed 50 already-pinned DM messages directly.
	for i := 0; i < 50; i++ {
		_, err := ts.DB.Exec(
			`INSERT INTO dm_messages (id, conversation_id, user_id, content, type, pinned_at, pinned_by)
			 VALUES (gen_random_uuid(), $1, $2, 'seeded', 'user', NOW(), $2)`,
			convID, u1.ID,
		)
		require.NoError(t, err)
	}

	extra := insertDMMessageDirect(t, ts, convID, u1.ID, "the 51st pin")
	w := ts.DoRequest("POST", pinAPIMsg+extra+pinPath, nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestGetChannelPins_UnknownID_404(t *testing.T) {
	// A UUID that is neither a channel nor a DM conversation must 404
	// through the getDMConversationPins fallback.
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getpins_unknown")
	fakeID := uuid.New().String()
	w := ts.DoRequest("GET", pinAPICh+fakeID+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetChannelPins_InvalidUUID_400(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getpins_bad")
	w := ts.DoRequest("GET", pinAPICh+"not-a-uuid"+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReactions_DMMessage_Returns404(t *testing.T) {
	// Reactions on DM messages are not yet supported; the handler must
	// resolve the DM context and return 404 to non-owners of the feature.
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmreact_a")
	u2 := ts.CreateTestUser(t, "dmreact_b")
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessageDirect(t, ts, convID, u1.ID, "emoji me")

	body := map[string]interface{}{"emoji": "👍"}
	w := ts.DoRequest("PUT", pinAPIMsg+msgID+"/reactions", body,
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)

	wg := ts.DoRequest("GET", pinAPIMsg+msgID+"/reactions", nil,
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusNotFound, wg.Code)
}

func TestUnpinMessage_InvalidUUID_400(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpininvalid")
	_ = ts.CreateTestServer(t, user.ID, "UnpinInvalid")

	w := ts.DoRequest("DELETE", pinAPIMsg+"not-a-uuid"+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUnpinMessage_NotMember_403(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "unpinowner")
	outsider := ts.CreateTestUser(t, "unpinoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "UnpinPriv")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, owner, "hi")

	// Pin as owner.
	require.Equal(t, http.StatusOK,
		ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
			testhelpers.AuthHeaders(owner.AccessToken)).Code)

	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetChannelPins_DM_InvalidUUID_400_Covered(t *testing.T) {
	// Equivalent to TestGetChannelPins_InvalidUUID_400 but kept under the DM
	// test file to document that DM routing shares the same 400 path.
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getpins_bad_dm")
	w := ts.DoRequest("GET", pinAPICh+"xyz"+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLookupMessageContext_NotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lookup_nf")
	_ = ts.CreateTestServer(t, user.ID, "NF Server")

	fakeID := uuid.New().String()
	w := ts.DoRequest("POST", pinAPIMsg+fakeID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
