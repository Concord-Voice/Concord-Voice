package messages_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/require"
)

func seedPurgeAttachment(t *testing.T, ts *testhelpers.TestServer, channelID string, user testhelpers.TestUser) (string, string) {
	t.Helper()
	messageID := ts.CreateTestMessage(t, channelID, user, "must survive purge guard drift")
	fileID := insertMediaFile(t, ts, user.ID, channelID, "file", "application/octet-stream", 7)
	insertMessageAttachment(t, ts, messageID, fileID, 0)
	return messageID, fileID
}

func assignPurgeManager(t *testing.T, ts *testhelpers.TestServer, serverID string, actor testhelpers.TestUser) {
	t.Helper()
	role := ts.CreateTestRole(t, serverID, "purge manager", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, actor.ID, role)
}

// Channel purge preflight is intentionally outside the engine transaction.
// Hold the channel row so the real Guard closure waits, remove the actor's
// membership during that wait, then verify the failed purge retained all data.
func TestPurgeChannelRejectsMembershipLossAtGuard(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "purge_guard_owner")
	actor := ts.CreateTestUser(t, "purge_guard_actor")
	serverID := ts.CreateTestServer(t, owner.ID, "purge guard channel server")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	assignPurgeManager(t, ts, serverID, actor)
	channelID := ts.CreateTestChannel(t, serverID, "guarded")
	messageID, fileID := seedPurgeAttachment(t, ts, channelID, actor)

	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var xid int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&xid))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID).Scan(&locked))

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID), map[string]any{"range": "all"}, testhelpers.AuthHeaders(actor.AccessToken))
		result <- w.Code
	}()
	dbtest.WaitForRowLockWaiter(t, ts.DB, xid)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, actor.ID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	select {
	case status := <-result:
		require.Equal(t, http.StatusInternalServerError, status)
	case <-time.After(time.Second):
		t.Fatal("channel purge did not resume after releasing the channel fence")
	}
	assertPurgeRowsIntact(t, ts, channelID, messageID, fileID)
	var auditCount int
	var auditStatus string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*), COALESCE(max(status), '') FROM message_purges WHERE context_id = $1`, channelID,
	).Scan(&auditCount, &auditStatus))
	require.Equal(t, 1, auditCount, "the failed channel purge retains its recovery audit")
	require.NotEqual(t, "completed", auditStatus, "a rejected channel purge must not be marked completed")
}

// The server plan has one Guard closure per channel. This proves the closure
// revalidates membership after server-scope preflight rather than relying on
// the cached permission result used to build the plan.
func TestPurgeServerRejectsMembershipLossAtChannelGuard(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "purge_server_guard_owner")
	actor := ts.CreateTestUser(t, "purge_server_guard_actor")
	serverID := ts.CreateTestServer(t, owner.ID, "purge guard server")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	assignPurgeManager(t, ts, serverID, actor)
	channelID := ts.CreateTestChannel(t, serverID, "guarded server channel")
	messageID, fileID := seedPurgeAttachment(t, ts, channelID, actor)

	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var xid int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&xid))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID).Scan(&locked))

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID), map[string]any{"range": "all"}, testhelpers.AuthHeaders(actor.AccessToken))
		result <- w.Code
	}()
	dbtest.WaitForRowLockWaiter(t, ts.DB, xid)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, actor.ID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	select {
	case status := <-result:
		require.Equal(t, http.StatusInternalServerError, status)
	case <-time.After(time.Second):
		t.Fatal("server purge did not resume after releasing the channel fence")
	}
	assertPurgeRowsIntact(t, ts, channelID, messageID, fileID)
}

// Server-channel pinning performs its own request-time lookup before the
// users-before-channel mutation fence. Remove membership while that fence is
// held to prove the real lockChannelMessageMutationTx path rejects the stale
// authorization and leaves the message unpinned.
func TestPinChannelRejectsMembershipLossAtMutationFence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "pin_guard_owner")
	actor := ts.CreateTestUser(t, "pin_guard_actor")
	serverID := ts.CreateTestServer(t, owner.ID, "pin guard server")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	assignPurgeManager(t, ts, serverID, actor)
	channelID := ts.CreateTestChannel(t, serverID, "pin guarded")
	messageID := ts.CreateTestMessage(t, channelID, actor, "must remain unpinned")

	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var xid int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&xid))
	var locked string
	require.NoError(t, barrier.QueryRow(
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, actor.ID).Scan(&locked))

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, "/api/v1/messages/"+messageID+"/pin", nil,
			testhelpers.AuthHeaders(actor.AccessToken))
		result <- w.Code
	}()
	dbtest.WaitForRowLockWaiter(t, ts.DB, xid)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, actor.ID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	select {
	case status := <-result:
		require.Equal(t, http.StatusForbidden, status)
	case <-time.After(time.Second):
		t.Fatal("channel pin did not resume after releasing the users fence")
	}
	var pinnedAt *time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT pinned_at FROM messages WHERE id = $1`, messageID).Scan(&pinnedAt))
	require.Nil(t, pinnedAt, "membership loss at the mutation fence must not pin the message")
}

func assertPurgeRowsIntact(t *testing.T, ts *testhelpers.TestServer, channelID, messageID, fileID string) {
	t.Helper()
	require.Equal(t, 1, countChannelMessages(t, ts, channelID), "guard rejection must preserve messages")
	require.Equal(t, 1, countRowsByQuery(t, ts,
		`SELECT count(*) FROM message_attachments WHERE message_id = $1 AND file_id = $2`, messageID, fileID),
		"guard rejection must preserve attachment metadata")
	require.Equal(t, 1, countRowsByQuery(t, ts,
		`SELECT count(*) FROM media_files WHERE id = $1 AND deleted_at IS NULL`, fileID),
		"guard rejection must not enqueue a committed blob retirement")
}

func countRowsByQuery(t *testing.T, ts *testhelpers.TestServer, query string, args ...any) int {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(query, args...).Scan(&count))
	return count
}
