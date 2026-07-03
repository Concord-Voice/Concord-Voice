package messages_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
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
			`INSERT INTO messages (id, channel_id, user_id, content, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, 'bulk msg', NOW(), NOW())`,
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

// --- Search-depth bound (#1555 Gate 2, bulk backfill path only) ---

// grantPremium inserts an active premium subscription and clears the cached tier
// (login during CreateTestUser may have cached the then-free tier). Server
// enforcement reads the subscriptions table via the entitlements cache (#1298).
func grantPremium(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, 'premium', 'active', 'code')`,
		userID,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Del(context.Background(), "ent:"+userID).Err())
}

// seedMessageAgedDays inserts a message directly (bypassing the send rate limit)
// with a created_at the given number of days in the past.
func seedMessageAgedDays(t *testing.T, ts *testhelpers.TestServer, channelID, userID, content string, ageDays int) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, NOW() - make_interval(days => $4), NOW())`,
		channelID, userID, content, ageDays,
	)
	require.NoError(t, err)
}

type boundedBulkResp struct {
	Messages        []models.MessageWithUser `json:"messages"`
	Count           int                      `json:"count"`
	SearchDepthDays *int                     `json:"search_depth_days"`
}

func messageContents(messages []models.MessageWithUser) []string {
	contents := make([]string, 0, len(messages))
	for _, m := range messages {
		contents = append(contents, m.Content)
	}
	return contents
}

// A free member's bulk backfill is bounded to 90 days: an 89-day-old message is
// returned, a 91-day-old one is not, and the response self-describes the bound.
func TestGetMessagesBulkFreeUserBoundedTo90Days(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "depthfree")
	serverID := ts.CreateTestServer(t, user.ID, "Depth Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	seedMessageAgedDays(t, ts, channelID, user.ID, "fresh-89d", 89)
	seedMessageAgedDays(t, ts, channelID, user.ID, "stale-91d", 91)

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp boundedBulkResp
	testhelpers.ParseJSON(t, w, &resp)
	contents := messageContents(resp.Messages)
	assert.Contains(t, contents, "fresh-89d")
	assert.NotContains(t, contents, "stale-91d", "free backfill must not reach past 90 days")
	require.NotNil(t, resp.SearchDepthDays, "bounded response must carry search_depth_days")
	assert.Equal(t, 90, *resp.SearchDepthDays)
}

// A premium member's bulk backfill reaches 180 days: a 91-day-old message is
// returned, a 181-day-old one is not, and the response carries the 180 bound.
func TestGetMessagesBulkPremiumUserBoundedTo180Days(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "depthprem")
	grantPremium(t, ts, user.ID)
	serverID := ts.CreateTestServer(t, user.ID, "Depth Premium Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	seedMessageAgedDays(t, ts, channelID, user.ID, "mid-91d", 91)
	seedMessageAgedDays(t, ts, channelID, user.ID, "ancient-181d", 181)

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp boundedBulkResp
	testhelpers.ParseJSON(t, w, &resp)
	contents := messageContents(resp.Messages)
	assert.Contains(t, contents, "mid-91d", "premium backfill reaches past 90 days")
	assert.NotContains(t, contents, "ancient-181d", "premium backfill must not reach past 180 days")
	require.NotNil(t, resp.SearchDepthDays)
	assert.Equal(t, 180, *resp.SearchDepthDays)
}

// Privacy regression lock: history ACCESS is never gated. The non-bulk
// GetMessages path returns arbitrarily old messages for BOTH tiers and never
// carries a search_depth_days bound.
func TestGetMessagesNonBulkUnboundedForBothTiers(t *testing.T) {
	ts := setupTS(t)

	for _, tc := range []struct {
		name    string
		premium bool
	}{
		{name: "free", premium: false},
		{name: "premium", premium: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := ts.CreateTestUser(t, "nodepth"+tc.name)
			if tc.premium {
				grantPremium(t, ts, user.ID)
			}
			serverID := ts.CreateTestServer(t, user.ID, "NoDepth Server "+tc.name)
			channelID := ts.CreateTestChannel(t, serverID, "general")

			seedMessageAgedDays(t, ts, channelID, user.ID, "ancient-181d", 181)

			w := ts.DoRequest("GET", channelsPrefix+channelID+"/messages", nil,
				testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code)

			var resp boundedBulkResp
			testhelpers.ParseJSON(t, w, &resp)
			assert.Contains(t, messageContents(resp.Messages), "ancient-181d",
				"non-bulk history access must never be depth-gated (privacy stance)")
			assert.Nil(t, resp.SearchDepthDays, "non-bulk response must not carry search_depth_days")
		})
	}
}
