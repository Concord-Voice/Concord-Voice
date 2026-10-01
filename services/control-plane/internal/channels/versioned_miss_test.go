package channels_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/e2eekeys"
)

// A pending request is only ever fulfilled with the newest epoch, so a
// versioned miss for an epoch strictly older than one that already has wraps
// can never be delivered. It must not enroll (and page holders) or tell the
// caller to wait: a member who joined after epoch 1 would otherwise hold every
// pre-join row "pending" forever (#2822).

func versionedKeyMiss(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, contextID, version string) e2eekeys.ErrorResponse {
	t.Helper()
	w := ts.DoRequest(http.MethodGet, pathE2EEKeys+contextID+"?version="+version, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var body e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, e2eekeys.CodeNoKeyYet, body.Code)
	return body
}

const (
	dmPendingRowsQuery      = `SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`
	channelPendingRowsQuery = `SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`
)

func pendingRows(t *testing.T, ts *testhelpers.TestServer, query, contextID, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(query, contextID, userID).Scan(&n))
	return n
}

func TestGetDMKey_VersionedMissForAnOlderEpochIsFinalAndDoesNotEnroll(t *testing.T) {
	ts := setupTS(t)
	founder := ts.CreateTestUser(t, "vmiss-dm-founder")
	joiner := ts.CreateTestUser(t, "vmiss-dm-joiner")
	convID := ts.CreateDMConversation(t, founder.ID, joiner.ID)
	ts.SeedDMKey(t, convID, founder.ID, 1)
	ts.SeedDMKey(t, convID, founder.ID, 2)
	ts.SeedDMKey(t, convID, joiner.ID, 2)

	body := versionedKeyMiss(t, ts, joiner, convID, "1")
	assert.False(t, body.Pending, "no pending request can ever deliver epoch 1")
	assert.Zero(t, pendingRows(t, ts, dmPendingRowsQuery, convID, joiner.ID))
}

func TestGetDMKey_VersionedMissForTheNewestEpochStillEnrolls(t *testing.T) {
	ts := setupTS(t)
	founder := ts.CreateTestUser(t, "vmiss-dm-holder")
	missing := ts.CreateTestUser(t, "vmiss-dm-missing")
	convID := ts.CreateDMConversation(t, founder.ID, missing.ID)
	ts.SeedDMKey(t, convID, founder.ID, 1)
	ts.SeedDMKey(t, convID, founder.ID, 2)

	body := versionedKeyMiss(t, ts, missing, convID, "2")
	assert.True(t, body.Pending, "the newest epoch is what a holder delivers")
	assert.Equal(t, 1, pendingRows(t, ts, dmPendingRowsQuery, convID, missing.ID))
}

func TestGetChannelKey_VersionedMissForAnOlderEpochIsFinalAndDoesNotEnroll(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	joiner := ts.CreateTestUser(t, "vmiss-chan-joiner")
	ts.AddMemberToServer(t, serverID, joiner.ID, roleMember)

	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
			rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
		 ) VALUES ($1, 1, 2, 'member_removal', $2, $2, TRUE, $3)`,
		channelID, owner.ID, rotationFingerprint,
	)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, owner.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, rotationFingerprint)
	require.NoError(t, err)
	_, err = tx.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'owner-epoch-two', 2), ($1, $3, 'joiner-epoch-two', 2)`,
		channelID, owner.ID, joiner.ID,
	)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	body := versionedKeyMiss(t, ts, joiner, channelID, "1")
	assert.False(t, body.Pending)
	assert.Zero(t, pendingRows(t, ts, channelPendingRowsQuery, channelID, joiner.ID))
}

func TestGetChannelKey_VersionedMissForTheCurrentEpochStillEnrolls(t *testing.T) {
	ts, _, serverID, channelID := setupEncryptedChannel(t)
	missing := ts.CreateTestUser(t, "vmiss-chan-missing")
	ts.AddMemberToServer(t, serverID, missing.ID, roleMember)

	body := versionedKeyMiss(t, ts, missing, channelID, "1")
	assert.True(t, body.Pending)
	assert.Equal(t, 1, pendingRows(t, ts, channelPendingRowsQuery, channelID, missing.ID))
}

// A label above the newest issued epoch names a key no holder has, so it is
// as undeliverable as a superseded one (Codex #3472 review).
func TestGetDMKey_VersionedMissAboveTheIssuedEpochIsFinal(t *testing.T) {
	ts := setupTS(t)
	founder := ts.CreateTestUser(t, "vmiss-dm-future-founder")
	joiner := ts.CreateTestUser(t, "vmiss-dm-future-joiner")
	convID := ts.CreateDMConversation(t, founder.ID, joiner.ID)
	ts.SeedDMKey(t, convID, founder.ID, 1)

	body := versionedKeyMiss(t, ts, joiner, convID, "7")
	assert.False(t, body.Pending)
	assert.Zero(t, pendingRows(t, ts, dmPendingRowsQuery, convID, joiner.ID))
}

func TestGetChannelKey_VersionedMissAboveTheIssuedEpochIsFinal(t *testing.T) {
	ts, _, serverID, channelID := setupEncryptedChannel(t)
	joiner := ts.CreateTestUser(t, "vmiss-chan-future")
	ts.AddMemberToServer(t, serverID, joiner.ID, roleMember)

	body := versionedKeyMiss(t, ts, joiner, channelID, "7")
	assert.False(t, body.Pending)
	assert.Zero(t, pendingRows(t, ts, channelPendingRowsQuery, channelID, joiner.ID))
}

// An issued successor whose wraps are still being distributed is deliverable.
func TestGetChannelKey_VersionedMissForAnIssuedUnwrappedSuccessorStillEnrolls(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	missing := ts.CreateTestUser(t, "vmiss-chan-successor")
	ts.AddMemberToServer(t, serverID, missing.ID, roleMember)
	_, err := ts.DB.Exec(`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'member_removal', $2)`, channelID, owner.ID)
	require.NoError(t, err)

	body := versionedKeyMiss(t, ts, missing, channelID, "2")
	assert.True(t, body.Pending)
	assert.Equal(t, 1, pendingRows(t, ts, channelPendingRowsQuery, channelID, missing.ID))
}

// Once a rotation has issued epoch 2, pending requests are served at 2 even
// before its wraps exist, so a miss for epoch 1 can never be delivered
// (Codex #3472 review).
func TestGetChannelKey_VersionedMissBelowAnIssuedSuccessorIsFinal(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	joiner := ts.CreateTestUser(t, "vmiss-chan-superseded")
	ts.AddMemberToServer(t, serverID, joiner.ID, roleMember)
	_, err := ts.DB.Exec(`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'member_removal', $2)`, channelID, owner.ID)
	require.NoError(t, err)

	body := versionedKeyMiss(t, ts, joiner, channelID, "1")
	assert.False(t, body.Pending)
	assert.Zero(t, pendingRows(t, ts, channelPendingRowsQuery, channelID, joiner.ID))
}
