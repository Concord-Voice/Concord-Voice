package users_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteAccount_HappyPath verifies the full erasure flow: the user
// row is deleted (cascading through FK-CASCADE tables), an audit row
// is inserted into account_deletions with user_id = NULL, and the
// transaction commits.
func TestDeleteAccount_HappyPath(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct1")

	svc := users.NewAccountService(ts.DB, logger.New("test"))

	err := svc.DeleteAccount(context.Background(), user.ID)
	require.NoError(t, err)

	var userRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID,
	).Scan(&userRows))
	assert.Equal(t, 0, userRows, "user row must be deleted")

	var auditRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM account_deletions WHERE user_id IS NULL AND deleted_at >= NOW() - INTERVAL '1 minute'`,
	).Scan(&auditRows))
	assert.GreaterOrEqual(t, auditRows, 1, "audit row must be inserted with NULL user_id")
}

// TestDeleteAccount_MissingUser verifies the sentinel-error contract:
// targeting a non-existent UUID returns ErrUserNotFound and writes no
// audit row.
func TestDeleteAccount_MissingUser(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	svc := users.NewAccountService(ts.DB, logger.New("test"))

	const ghostUUID = "00000000-0000-0000-0000-000000000001"

	var auditBefore int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&auditBefore))

	err := svc.DeleteAccount(context.Background(), ghostUUID)
	require.ErrorIs(t, err, users.ErrUserNotFound)

	var auditAfter int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&auditAfter))
	assert.Equal(t, auditBefore, auditAfter,
		"no audit row must be written when the user did not exist")
}

// TestDeleteAccount_IdempotentRetry verifies that calling DeleteAccount
// twice for the same user yields ErrUserNotFound on the second call and
// produces exactly one audit row total.
func TestDeleteAccount_IdempotentRetry(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct2")

	svc := users.NewAccountService(ts.DB, logger.New("test"))

	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))

	err := svc.DeleteAccount(context.Background(), user.ID)
	require.ErrorIs(t, err, users.ErrUserNotFound)

	var auditCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM account_deletions WHERE deleted_at >= NOW() - INTERVAL '1 minute'`,
	).Scan(&auditCount))
	assert.Equal(t, 1, auditCount, "exactly one audit row must exist after success+retry")
}

// TestDeleteAccount_AuditRowInsertWithoutSentryColumn pins the
// post-migration-000060 schema: the INSERT in DeleteAccount must NOT
// reference the dropped sentry_delete_attempted column. If a future
// edit accidentally re-adds the column reference, this test catches it
// (the INSERT would fail with column-not-found error).
func TestDeleteAccount_AuditRowInsertWithoutSentryColumn(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct3")

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	err := svc.DeleteAccount(context.Background(), user.ID)
	require.NoError(t, err, "INSERT must succeed without sentry_delete_attempted column")
}

func TestDeleteAccount_BroadcastsIncompleteChannelDeletion(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "erasechannelowner")
	creator := ts.CreateTestUser(t, "erasechannelcreator")
	serverID := ts.CreateTestServer(t, owner.ID, "Erase Channel Server")
	channelID := ts.CreateTestChannel(t, serverID, "incomplete-channel")
	ts.AddMemberToServer(t, serverID, creator.ID, "member")
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, creator.ID,
	)
	require.NoError(t, err)

	var broadcasts [][2]string
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetChannelDeletedBroadcaster(func(serverID, channelID string) {
		broadcasts = append(broadcasts, [2]string{serverID, channelID})
	})
	require.NoError(t, svc.DeleteAccount(context.Background(), creator.ID))

	assert.Equal(t, [][2]string{{serverID, channelID}}, broadcasts)
	var channelRows int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID).Scan(&channelRows))
	assert.Zero(t, channelRows)
}

func TestDeleteAccount_BroadcastsCascadedIncompleteLinkedChannelDeletion(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "erasevoiceowner")
	creator := ts.CreateTestUser(t, "erasevoicecreator")
	serverID := ts.CreateTestServer(t, owner.ID, "Erase Voice Channel Server")
	voiceID := ts.CreateTestChannel(t, serverID, "incomplete-voice")
	linkedTextID := ts.CreateTestChannel(t, serverID, "completed-linked-text")
	ts.AddMemberToServer(t, serverID, creator.ID, "member")
	_, err := ts.DB.Exec(
		`UPDATE channels SET linked_voice_channel_id = $2 WHERE id = $1`, linkedTextID, voiceID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, voiceID, creator.ID,
	)
	require.NoError(t, err)

	var broadcasts [][2]string
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetChannelDeletedBroadcaster(func(serverID, channelID string) {
		broadcasts = append(broadcasts, [2]string{serverID, channelID})
	})
	require.NoError(t, svc.DeleteAccount(context.Background(), creator.ID))

	assert.ElementsMatch(t, [][2]string{{serverID, voiceID}, {serverID, linkedTextID}}, broadcasts)
	var channelRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channels WHERE id IN ($1, $2)`, voiceID, linkedTextID,
	).Scan(&channelRows))
	assert.Zero(t, channelRows)
}

// TestDeleteAccount_CascadeDeletesUserRelatedData verifies that the
// ON DELETE CASCADE behavior correctly cleans up all user-associated
// data (servers, channels, messages, roles, etc.) when the user row
// is deleted. This ensures account erasure is complete.
func TestDeleteAccount_CascadeDeletesUserRelatedData(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct4")

	// Create a server and channel owned by the user.
	serverID := ts.CreateTestServer(t, user.ID, "test_server")
	channelID := ts.CreateTestChannel(t, serverID, "test_channel")

	// Add a role and assign it to the user.
	roleID := ts.CreateTestRole(t, serverID, "test_role", 1, 1)
	ts.AssignRoleToUser(t, serverID, user.ID, roleID)

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))

	// Verify user is gone.
	var userExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID,
	).Scan(&userExists))
	assert.Equal(t, 0, userExists)

	// Verify server is cascaded (owner deletion cascades).
	var serverExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM servers WHERE id = $1`, serverID,
	).Scan(&serverExists))
	assert.Equal(t, 0, serverExists, "owned server must be cascaded when user is deleted")

	// Verify channel is also cascaded transitively.
	var channelExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID,
	).Scan(&channelExists))
	assert.Equal(t, 0, channelExists, "channel must be cascaded when server is deleted")

	// Verify role is cascaded.
	var roleExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM roles WHERE id = $1`, roleID,
	).Scan(&roleExists))
	assert.Equal(t, 0, roleExists, "role must be cascaded when server is deleted")
}
