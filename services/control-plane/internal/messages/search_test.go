package messages_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bulkPath       = "/messages/bulk"
	channelsPrefix = "/api/v1/channels/"
)

func TestGetMessagesBulkSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkuser")
	serverID := ts.CreateTestServer(t, user.ID, "Bulk Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create 5 messages
	for i := 0; i < 5; i++ {
		ts.CreateTestMessage(t, channelID, user, "Bulk message")
	}

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []models.MessageWithUser `json:"messages"`
		Count    int                      `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 5, resp.Count)
	assert.Len(t, resp.Messages, 5)
}

func TestGetMessagesBulkPagination(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkpaginate")
	serverID := ts.CreateTestServer(t, user.ID, "BulkPage Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create 3 messages
	for i := 0; i < 3; i++ {
		ts.CreateTestMessage(t, channelID, user, "Msg")
	}

	// Fetch with limit=2
	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+"?limit=2", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
		Count    int               `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 2, resp.Count)
}

func TestGetMessagesBulkNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bulkowner")
	outsider := ts.CreateTestUser(t, "bulkoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "BulkPrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessagesBulkDefaultLimit200(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkdefault")
	serverID := ts.CreateTestServer(t, user.ID, "BulkDefault Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Seed 5 messages via SQL to avoid rate limit
	for i := 0; i < 5; i++ {
		_, err := ts.DB.Exec(
			`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, 'bulk msg', 1, NOW(), NOW())`,
			channelID, user.ID,
		)
		require.NoError(t, err)
	}

	// Fetch without limit param — should default to 200 (returns all 5)
	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 5, resp.Count)
}

// --- No search-depth bound (founder un-gating 2026-10-15) ---

// History is never depth-limited: a free member's bulk backfill AND plain
// history both return a message older than the retired #1555 premium bound
// (180d), and neither response carries the retired search_depth_days field.
func TestGetMessagesUnboundedForFreeUser(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nodepthfree")
	serverID := ts.CreateTestServer(t, user.ID, "NoDepth Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, $2, 'ancient-400d', 1, NOW() - make_interval(days => 400), NOW())`,
		channelID, user.ID,
	)
	require.NoError(t, err)

	for _, path := range []string{bulkPath, "/messages"} {
		t.Run(path, func(t *testing.T) {
			w := ts.DoRequest("GET", channelsPrefix+channelID+path, nil,
				testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code)

			var resp struct {
				Messages        []models.MessageWithUser `json:"messages"`
				SearchDepthDays *int                     `json:"search_depth_days"`
			}
			testhelpers.ParseJSON(t, w, &resp)
			require.Len(t, resp.Messages, 1)
			assert.Equal(t, "ancient-400d", resp.Messages[0].Content)
			assert.Nil(t, resp.SearchDepthDays)
		})
	}
}
