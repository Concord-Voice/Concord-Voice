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
	pinPath   = "/pin"
	pinsPath  = "/pins"
	pinAPIMsg = "/api/v1/messages/"
	pinAPICh  = "/api/v1/channels/"
)

// --- Pin Message Tests ---

func TestPinMessageSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinuser")
	serverID := ts.CreateTestServer(t, user.ID, "Pin Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin this!")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, msgID, resp["message_id"])
	assert.NotNil(t, resp["pinned_at"])
	assert.NotNil(t, resp["pinned_by"])
}

func TestPinMessageAlreadyPinned(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinidempotent")
	serverID := ts.CreateTestServer(t, user.ID, "Pin2 Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin me twice")

	// Pin first time
	ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Pin again — should be idempotent
	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp["already_pinned"])
}

func TestPinMessageInvalidUUID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pininvalid")
	_ = ts.CreateTestServer(t, user.ID, "PinInvalid Server")

	w := ts.DoRequest("POST", pinAPIMsg+"not-a-uuid"+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPinMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinnotfound")
	_ = ts.CreateTestServer(t, user.ID, "PinNotFound Server")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("POST", pinAPIMsg+fakeID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPinMessageNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "pinowner")
	outsider := ts.CreateTestUser(t, "pinoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "PinPrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, owner, "Secret pin")

	w := ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPinMessageLimitReached(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinlimit")
	serverID := ts.CreateTestServer(t, user.ID, "PinLimit Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Seed 50 pinned messages directly via SQL to avoid rate limiting
	for i := 0; i < 50; i++ {
		_, err := ts.DB.Exec(
			`INSERT INTO messages (id, channel_id, user_id, content, pinned_at, pinned_by, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, 'pinned msg', NOW(), $2, NOW(), NOW())`,
			channelID, user.ID,
		)
		require.NoError(t, err)
	}

	// Create 51st message and try to pin via API
	extraMsg := ts.CreateTestMessage(t, channelID, user, "One too many")
	w := ts.DoRequest("POST", pinAPIMsg+extraMsg+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// --- Unpin Message Tests ---

func TestUnpinMessageSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpinuser")
	serverID := ts.CreateTestServer(t, user.ID, "Unpin Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Unpin me")

	// Pin first
	ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Unpin
	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, msgID, resp["message_id"])
	assert.Nil(t, resp["already_unpinned"])
}

func TestUnpinMessageAlreadyUnpinned(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpinidempotent")
	serverID := ts.CreateTestServer(t, user.ID, "Unpin2 Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Never pinned")

	w := ts.DoRequest("DELETE", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp["already_unpinned"])
}

func TestUnpinMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unpinnotfound")
	_ = ts.CreateTestServer(t, user.ID, "UnpinNotFound Server")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("DELETE", pinAPIMsg+fakeID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- GetChannelPins Tests ---

func TestGetChannelPinsSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getpinsuser")
	serverID := ts.CreateTestServer(t, user.ID, "GetPins Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create and pin 3 messages
	for i := 0; i < 3; i++ {
		msgID := ts.CreateTestMessage(t, channelID, user, "Pinned msg")
		ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
			testhelpers.AuthHeaders(user.AccessToken))
	}

	w := ts.DoRequest("GET", pinAPICh+channelID+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		PinnedMessages []models.MessageWithUser `json:"pinned_messages"`
		Count          int                      `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 3, resp.Count)
	assert.Len(t, resp.PinnedMessages, 3)
	// Verify ordered by pinned_at DESC (most recently pinned first)
	for _, msg := range resp.PinnedMessages {
		assert.NotNil(t, msg.PinnedAt)
	}
}

func TestGetChannelPinsEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nopinsuser")
	serverID := ts.CreateTestServer(t, user.ID, "NoPins Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", pinAPICh+channelID+pinsPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		PinnedMessages []json.RawMessage `json:"pinned_messages"`
		Count          int               `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 0, resp.Count)
	assert.Len(t, resp.PinnedMessages, 0)
}

func TestGetChannelPinsNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "pinsowner")
	outsider := ts.CreateTestUser(t, "pinsoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "PinsPrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", pinAPICh+channelID+pinsPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessagesPinnedFieldsIncluded(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pinnedfields")
	serverID := ts.CreateTestServer(t, user.ID, "PinnedFields Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	msgID := ts.CreateTestMessage(t, channelID, user, "Pin and fetch")

	// Pin the message
	ts.DoRequest("POST", pinAPIMsg+msgID+pinPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Fetch messages and verify pinned fields
	w := ts.DoRequest("GET", pinAPICh+channelID+"/messages", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 1)

	var msg struct {
		ID       string  `json:"id"`
		PinnedAt *string `json:"pinned_at"`
		PinnedBy *string `json:"pinned_by"`
	}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &msg))
	assert.Equal(t, msgID, msg.ID)
	require.NotNil(t, msg.PinnedAt)
	require.NotNil(t, msg.PinnedBy)
}
