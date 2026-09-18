package channels_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// CV-CAN-002 / CWE-863: an unread COUNT is message-content disclosure, not a
// directory entry. PermReadMessageHistory is separately deniable and is already
// enforced on every path that returns message bodies (messages.checkChannelAccess,
// reactions, media), so a member holding only the view bit could previously poll
// /servers/unread-status for a live side channel on volume and timing in a channel
// they are forbidden to read. These tests pin both halves of the fix: the count
// requires history, and the channel LISTING still requires only the view bit.

// insertUnreadMessage writes one message from authorID into channelID, newer than
// any read state, so it counts as unread for every other member.
func insertUnreadMessage(t *testing.T, ts *testhelpers.TestServer, channelID, authorID string) {
	t.Helper()

	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		 VALUES ($1, $2, $3, 'unread', 1, false, NOW(), NOW())`,
		uuid.New().String(), channelID, authorID)
	require.NoError(t, err)
}

// unreadChannelIDs extracts the channel ids from GET /servers/{id}/unread.
func unreadChannelIDs(t *testing.T, ts *testhelpers.TestServer, serverID string, user testhelpers.TestUser) []string {
	t.Helper()

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		Unreads []struct {
			ChannelID string `json:"channel_id"`
		} `json:"unreads"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	ids := make([]string, 0, len(body.Unreads))
	for _, entry := range body.Unreads {
		ids = append(ids, entry.ChannelID)
	}
	return ids
}

// serverUnreadStatus extracts the per-channel ids and the server ids from
// GET /servers/unread-status.
func serverUnreadStatus(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) (channelIDs, serverIDs []string) {
	t.Helper()

	w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		ServerIDs []string `json:"server_ids"`
		Channels  []struct {
			ChannelID string `json:"channel_id"`
		} `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	channelIDs = make([]string, 0, len(body.Channels))
	for _, entry := range body.Channels {
		channelIDs = append(channelIDs, entry.ChannelID)
	}
	return channelIDs, body.ServerIDs
}

// listedChannelIDs extracts the channel ids from GET /servers/{id}/channels.
func listedChannelIDs(t *testing.T, ts *testhelpers.TestServer, serverID string, user testhelpers.TestUser) []string {
	t.Helper()

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/channels", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		Channels []struct {
			ID string `json:"id"`
		} `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	ids := make([]string, 0, len(body.Channels))
	for _, ch := range body.Channels {
		ids = append(ids, ch.ID)
	}
	return ids
}

// A member who can SEE a channel but is denied read_message_history in it gets no
// unread count for it — from either endpoint — while the channel stays in the
// channel list and the member's other channel is unaffected.
func TestUnreadCountsRequireReadMessageHistory(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "rmhowner")
	member := ts.CreateTestUser(t, "rmhmember")
	serverID := ts.CreateTestServer(t, owner.ID, "RMH Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	openChannel := ts.CreateTestChannel(t, serverID, "rmh-open")
	sealedChannel := ts.CreateTestChannel(t, serverID, "rmh-leadership")

	// The view bit is untouched on both channels; only read_message_history is
	// denied, and only on the sealed one, and only for this member.
	ts.CreateChannelOverride(t, sealedChannel, "user", member.ID, 0, int64(rbac.PermReadMessageHistory))

	insertUnreadMessage(t, ts, openChannel, owner.ID)
	insertUnreadMessage(t, ts, sealedChannel, owner.ID)

	// The sidebar is a view-bit question: a history denial must hide nothing.
	listed := listedChannelIDs(t, ts, serverID, member)
	assert.Contains(t, listed, openChannel)
	assert.Contains(t, listed, sealedChannel,
		"denying read_message_history must not remove the channel from the channel list")

	unreads := unreadChannelIDs(t, ts, serverID, member)
	assert.Contains(t, unreads, openChannel, "the readable channel keeps its unread count")
	assert.NotContains(t, unreads, sealedChannel,
		"GetUnreadCounts must not report activity in a channel the member may not read")

	statusChannels, statusServers := serverUnreadStatus(t, ts, member)
	assert.Contains(t, statusChannels, openChannel, "the readable channel keeps its unread count")
	assert.NotContains(t, statusChannels, sealedChannel,
		"GetServerUnreadStatus must not report activity in a channel the member may not read")
	assert.Contains(t, statusServers, serverID,
		"the readable channel still raises the server's unread dot")
}

// When the ONLY unread channel is history-denied, the server drops out of
// server_ids entirely — with no other unread, the dot itself is the side channel.
func TestServerUnreadStatusDropsServerWhenOnlyChannelIsHistoryDenied(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "rmhsoloowner")
	member := ts.CreateTestUser(t, "rmhsolomember")
	serverID := ts.CreateTestServer(t, owner.ID, "RMH Solo Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	sealedChannel := ts.CreateTestChannel(t, serverID, "rmh-solo-sealed")
	ts.CreateChannelOverride(t, sealedChannel, "user", member.ID, 0, int64(rbac.PermReadMessageHistory))
	insertUnreadMessage(t, ts, sealedChannel, owner.ID)

	statusChannels, statusServers := serverUnreadStatus(t, ts, member)
	assert.NotContains(t, statusChannels, sealedChannel)
	assert.NotContains(t, statusServers, serverID,
		"a history-denied channel must not raise the server's unread dot")
}

// The owner and administrator fast paths deliberately keep bypassing the history
// requirement, because the enforcement path they mirror does the same: the owner
// short-circuits to OwnerPermissions before SBAC is consulted, and
// applyChannelOverrides refuses to restrict an administrator. Counting what those
// two can already read discloses nothing new.
func TestUnreadCountsIgnoreHistoryDenyForOwnerAndAdministrator(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "rmhfpowner")
	admin := ts.CreateTestUser(t, "rmhfpadmin")
	author := ts.CreateTestUser(t, "rmhfpauthor")
	serverID := ts.CreateTestServer(t, owner.ID, "RMH Fastpath Server")
	ts.AddMemberToServer(t, serverID, admin.ID, roleMember)
	ts.AddMemberToServer(t, serverID, author.ID, roleMember)

	adminRoleID := ts.CreateTestRole(t, serverID, "Admin-rmh", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	sealedChannel := ts.CreateTestChannel(t, serverID, "rmh-fp-sealed")
	ts.CreateChannelOverride(t, sealedChannel, "user", owner.ID, 0, int64(rbac.PermReadMessageHistory))
	ts.CreateChannelOverride(t, sealedChannel, "user", admin.ID, 0, int64(rbac.PermReadMessageHistory))
	insertUnreadMessage(t, ts, sealedChannel, author.ID)

	ownerUnreads := unreadChannelIDs(t, ts, serverID, owner)
	assert.Contains(t, ownerUnreads, sealedChannel, "SBAC cannot restrict the server owner")

	adminUnreads := unreadChannelIDs(t, ts, serverID, admin)
	assert.Contains(t, adminUnreads, sealedChannel, "SBAC cannot restrict an administrator")
}
