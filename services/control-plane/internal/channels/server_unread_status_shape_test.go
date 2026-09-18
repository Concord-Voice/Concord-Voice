package channels_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Shape of GET /servers/unread-status (#2403). The history-permission half lives
// in unread_history_permission_test.go; these four pin the branches that decide
// what the response CONTAINS rather than who may see it. Each one covers a branch
// the badge derives from directly, so a regression here is a wrong number on a
// user's Dock rather than a failed request.

// A channel the member cannot SEE contributes no unread row.
//
// Distinct from the history-denied case next door, and both are needed: that one
// denies read_message_history with the view bit intact and asserts the channel
// STAYS in the channel list, while this one denies the view bit and asserts it
// leaves. They exercise opposite arms of `effective & mask = mask`, so a mask
// built from the wrong bit passes one and fails the other.
func TestServerUnreadStatusExcludesNonVisibleChannel(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "shapevisowner")
	member := ts.CreateTestUser(t, "shapevismember")
	serverID := ts.CreateTestServer(t, owner.ID, "Shape Visibility")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	visible := ts.CreateTestChannel(t, serverID, "shape-visible")
	hidden := ts.CreateTestChannel(t, serverID, "shape-hidden")

	// Deny the VIEW bit this time, not read_message_history.
	ts.CreateChannelOverride(t, hidden, "user", member.ID, 0, int64(rbac.PermViewTextChannels))

	insertUnreadMessage(t, ts, visible, owner.ID)
	insertUnreadMessage(t, ts, hidden, owner.ID)

	statusChannels, statusServers := serverUnreadStatus(t, ts, member)
	assert.Contains(t, statusChannels, visible)
	assert.NotContains(t, statusChannels, hidden,
		"a channel the member cannot view must not appear in the unread response")
	assert.Contains(t, statusServers, serverID,
		"the visible channel still raises the server")

	// The view denial is what removes it, so the channel list must agree.
	listed := listedChannelIDs(t, ts, serverID, member)
	assert.Contains(t, listed, visible)
	assert.NotContains(t, listed, hidden)
}

// Both empty responses emit `[]` for BOTH arrays, never `null`.
//
// Asserted against the RAW JSON on purpose. Go unmarshals `[]` and `null` into
// the same nil slice, so a struct-level `assert.Empty` passes either way — and
// the client iterates both fields, where `null` is a runtime error rather than an
// empty loop. Nothing short of a raw-bytes check can tell the two apart.
//
// TWO subtests because there are TWO empty responses, produced by different code
// with different literals, and a mutation control is what proved they need
// separate coverage: mutating the accumulator declarations left the no-servers
// case green, because that case never reaches them. The early return builds its
// own `[]string{}` / `[]channelUnread{}` inline; the query path builds
// `channels := []channelUnread{}` and `serverIDs := []string{}` and then adds
// nothing. Breaking either one alone is invisible to a test covering the other.
func TestServerUnreadStatusEmptyArraysAreNotNull(t *testing.T) {
	assertEmptyArrays := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))

		require.Contains(t, raw, "channels")
		require.Contains(t, raw, "server_ids")
		assert.JSONEq(t, `[]`, string(raw["channels"]),
			"channels must serialize as [] — the client iterates it and null throws")
		assert.JSONEq(t, `[]`, string(raw["server_ids"]),
			"server_ids must serialize as [] — same reason")
	}

	// No readable channels at all: the early return, before the query runs.
	t.Run("NoServers", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "shapeemptynone")

		assertEmptyArrays(t, ts.DoRequest("GET", "/api/v1/servers/unread-status", nil,
			testhelpers.AuthHeaders(user.AccessToken)))
	})

	// Readable channels exist and none of them is unread: the query path, which
	// runs to completion and appends nothing. This is the branch the early return
	// cannot stand in for.
	t.Run("ServerWithNothingUnread", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "shapeemptyowner")
		member := ts.CreateTestUser(t, "shapeemptymember")
		serverID := ts.CreateTestServer(t, owner.ID, "Shape Empty")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		ts.CreateTestChannel(t, serverID, "shape-empty-quiet") // readable, silent

		assertEmptyArrays(t, ts.DoRequest("GET", "/api/v1/servers/unread-status", nil,
			testhelpers.AuthHeaders(member.AccessToken)))
	})
}

// Two unread channels in ONE server yield two rows and ONE server id.
//
// This is the `seenServers` guard's FALSE arm: the true arm (a server seen for
// the first time) runs in every other test in this file, and nothing reached the
// arm that skips a duplicate. Without it, server_ids would carry the server once
// per unread channel and the sidebar would count the same server repeatedly.
func TestServerUnreadStatusDeduplicatesServerAcrossChannels(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "shapededupeowner")
	member := ts.CreateTestUser(t, "shapededupemember")
	serverID := ts.CreateTestServer(t, owner.ID, "Shape Dedupe")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	first := ts.CreateTestChannel(t, serverID, "shape-dedupe-a")
	second := ts.CreateTestChannel(t, serverID, "shape-dedupe-b")
	insertUnreadMessage(t, ts, first, owner.ID)
	insertUnreadMessage(t, ts, second, owner.ID)

	statusChannels, statusServers := serverUnreadStatus(t, ts, member)
	assert.ElementsMatch(t, []string{first, second}, statusChannels,
		"both unread channels report their own row")
	assert.Equal(t, []string{serverID}, statusServers,
		"one server, named once, however many of its channels are unread")
}

// A channel with nothing unread is ABSENT, never present with a count of 0.
//
// Two ways to have nothing unread, and they are different predicates: no messages
// at all (the INNER JOIN on messages drops the row) and messages the member wrote
// themselves (`m.user_id != $1`). Both must be absent, because the client sums
// whatever rows arrive and a zero row that leaked in as a one would be a Dock
// badge for a conversation with nothing new in it.
func TestServerUnreadStatusOmitsChannelsWithNothingUnread(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "shapezeroowner")
	member := ts.CreateTestUser(t, "shapezeromember")
	serverID := ts.CreateTestServer(t, owner.ID, "Shape Zero")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	unread := ts.CreateTestChannel(t, serverID, "shape-zero-unread")
	silent := ts.CreateTestChannel(t, serverID, "shape-zero-silent")
	ownMessagesOnly := ts.CreateTestChannel(t, serverID, "shape-zero-self")

	insertUnreadMessage(t, ts, unread, owner.ID)
	insertUnreadMessage(t, ts, ownMessagesOnly, member.ID) // authored by the reader

	statusChannels, statusServers := serverUnreadStatus(t, ts, member)
	assert.Equal(t, []string{unread}, statusChannels,
		"only the genuinely-unread channel reports a row")
	assert.NotContains(t, statusChannels, silent,
		"a channel with no messages is absent, not present with unread_count 0")
	assert.NotContains(t, statusChannels, ownMessagesOnly,
		"the member's own messages are not unread for the member")
	assert.Equal(t, []string{serverID}, statusServers)
}
