package channels_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #2420 recipient key-freshness guard: a distribution wrapped against a
// recipient's OLD public-key version must not re-create the wrapped-key row a
// concurrent ReplaceMyKeys/RecoveryResetAccount purged. Availability-only.
// (recipientKeyFresh unit coverage lives in keydist_freshness_internal_test.go.)

const jsonWrappedKeyVersions = "wrapped_key_versions"

func distributePost(ts *testhelpers.TestServer, token, contextID string, wrapped map[string]string, versions map[string]int) *httptest.ResponseRecorder {
	body := map[string]interface{}{"wrapped_keys": wrapped}
	if versions != nil {
		body[jsonWrappedKeyVersions] = versions
	}
	return ts.DoRequest("POST", pathE2EEKeys+contextID, body, testhelpers.AuthHeaders(token))
}

func setRecipientPublicKeyVersion(t *testing.T, ts *testhelpers.TestServer, userID string, version int) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO public_keys (user_id, public_key, key_version) VALUES ($1, $2, $3)`,
		userID, []byte("test-public-key"), version)
	require.NoError(t, err)
}

func rowCount(t *testing.T, ts *testhelpers.TestServer, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(query, args...).Scan(&n))
	return n
}

// --- channel path ---

func TestDistributeChannelKeys_StaleRecipient_SkippedAndEnqueued(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "stalechan")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	// Recipient's current public key is v6 (rotated); distributor wrapped against v5.
	setRecipientPublicKeyVersion(t, ts, member.ID, 6)

	w := distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member.ID: testhelpers.ValidCiphertext()},
		map[string]int{member.ID: 5})
	assert.Equal(t, http.StatusOK, w.Code)
	// The public response contract surfaces the stale skip distinctly (not folded
	// into the transient-retry `skipped`/503 bucket).
	assert.JSONEq(t, `{"distributed":0,"duplicates":0,"skipped":0,"skipped_stale":1}`,
		w.Body.String(), "the OK body reports the stale skip in skipped_stale")

	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"stale recipient must not receive a wrapped-key row")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"stale recipient must be self-heal-enqueued")
}

func TestDistributeChannelKeys_FreshRecipient_Delivered(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "freshchan")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	setRecipientPublicKeyVersion(t, ts, member.ID, 5)

	w := distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member.ID: testhelpers.ValidCiphertext()},
		map[string]int{member.ID: 5})
	assert.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"fresh recipient receives the key")
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"no self-heal for a fresh recipient")
}

func TestDistributeChannelKeys_NoVersion_FailOpen(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "failopenchan")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	setRecipientPublicKeyVersion(t, ts, member.ID, 6) // rotated, but distributor omits versions

	w := distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member.ID: testhelpers.ValidCiphertext()}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"old client (no version) falls open to the legacy insert")
}

// --- DM path ---

func TestDistributeDMKeys_StaleRecipient_SkippedAndEnqueued(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmdist1")
	user2 := ts.CreateTestUser(t, "dmdist2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	setRecipientPublicKeyVersion(t, ts, user2.ID, 6) // recipient rotated

	w := distributePost(ts, user1.AccessToken, convID,
		map[string]string{user2.ID: testhelpers.ValidCiphertext()},
		map[string]int{user2.ID: 5})
	assert.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`, convID, user2.ID),
		"stale DM recipient must not receive a wrapped-key row")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`, convID, user2.ID),
		"stale DM recipient must be self-heal-enqueued")
}

// --- concurrency: prove FOR SHARE serializes against a concurrent reset ---

// TestDistributeChannelKeys_ConcurrentReset_ForShareSerializes proves the guard's
// FOR SHARE freshness read actually serializes against a concurrent
// ReplaceMyKeys/RecoveryResetAccount (which locks public_keys FOR NO KEY UPDATE),
// not merely that the stale-detection logic works. The recipient starts at v5
// (matching the wrapped version), so a bare snapshot SELECT would read v5, pass
// the freshness check, and insert a stale row that survives the reset's purge.
// This test holds the reset's row lock while the distribution runs: the
// distribution MUST block on the FOR SHARE read until the reset commits, then
// observe the rotated v6 and skip. Deleting FOR SHARE makes the distribution
// NOT block and insert a row — failing this test.
func TestDistributeChannelKeys_ConcurrentReset_ForShareSerializes(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "racereset")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	setRecipientPublicKeyVersion(t, ts, member.ID, 5) // recipient currently at v5 (== wrapped version)

	// A concurrent reset holds FOR NO KEY UPDATE on the recipient's public_keys
	// row and rotates it to v6, WITHOUT committing yet — the lock is held open.
	resetTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = resetTx.Rollback() }()
	_, err = resetTx.Exec(
		`SELECT key_version FROM public_keys WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1 FOR NO KEY UPDATE`,
		member.ID)
	require.NoError(t, err)
	_, err = resetTx.Exec(`UPDATE public_keys SET key_version = 6 WHERE user_id = $1`, member.ID)
	require.NoError(t, err)

	// The distribution (wrapped against v5) runs concurrently; its FOR SHARE
	// freshness read must block on the reset's lock.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- distributePost(ts, owner.AccessToken, channelID,
			map[string]string{member.ID: testhelpers.ValidCiphertext()},
			map[string]int{member.ID: 5})
	}()

	select {
	case <-done:
		t.Fatal("distribution completed without blocking on the reset lock — FOR SHARE is not serializing")
	case <-time.After(750 * time.Millisecond):
		// Expected: the distribution is blocked on the FOR SHARE read.
	}

	// Release the reset (v6 now committed, lock dropped).
	require.NoError(t, resetTx.Commit())

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("distribution did not complete after the reset committed")
	}
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"a wrap that raced a concurrent reset must not create a stale row")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"the raced recipient must be self-heal-enqueued")
}

// --- channel-creation path (#2420 review — the initial-member wrap) ---

// TestCreateChannel_StaleInitialMember_SkippedAndEnqueued proves CreateChannel's
// initial-member key wrap (storeWrappedKeys) is guarded identically to the
// distribution path: a member wrapped against a superseded public-key version is
// skipped and self-heal-enqueued, while a member with no supplied version falls
// open to the legacy insert.
func TestCreateChannel_StaleInitialMember_SkippedAndEnqueued(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "createchanowner2")
	serverID := ts.CreateTestServer(t, owner.ID, "Create Chan Guard Server")
	member := ts.CreateTestUser(t, "createchanstale")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	setRecipientPublicKeyVersion(t, ts, member.ID, 6) // member rotated to v6

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "guarded-create-channel",
		"type":      "text",
		"wrapped_keys": map[string]string{
			owner.ID:  testhelpers.ValidCiphertext(),
			member.ID: testhelpers.ValidCiphertext(),
		},
		"wrapped_key_versions": map[string]int{member.ID: 5}, // wrapped against the OLD v5
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var channelID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM channels WHERE server_id = $1 AND name = 'guarded-create-channel'`, serverID).Scan(&channelID))

	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"stale initial member must not receive a wrapped-key row at channel creation")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"stale initial member must be self-heal-enqueued")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, owner.ID),
		"creator (no version supplied) falls open to the legacy insert")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID),
		"a stale persisted wrap must retain the creator fence")
}
