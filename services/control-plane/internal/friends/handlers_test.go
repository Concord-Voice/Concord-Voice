package friends_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathFriendsPrefix  = "/api/v1/friends/"
	pathFriends        = "/api/v1/friends"
	pathFriendRequest  = "/api/v1/friends/request"
	pathFriendRequests = "/api/v1/friends/requests"
	pathFriendCodes    = "/api/v1/friends/codes"
	pathBlock          = "/block"
	statusAccepted     = "accepted"
	statusPending      = "pending"
	statusBlocked      = "blocked"

	pathFriendRequestSlash = "/api/v1/friends/request/"
	pathClaim              = "/claim"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// createPendingRequest inserts a pending friend request and returns its ID.
func createPendingRequest(t *testing.T, ts *testhelpers.TestServer, fromID, toID string) string {
	t.Helper()
	var requestID string
	err := ts.DB.QueryRow(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending') RETURNING id`,
		fromID, toID,
	).Scan(&requestID)
	require.NoError(t, err)
	return requestID
}

// createFriendCode inserts a friend code directly and returns the code string and its DB ID.
func createFriendCode(t *testing.T, ts *testhelpers.TestServer, userID, code string, maxUses *int, expiresAt *time.Time, autoAccept bool) string {
	t.Helper()
	var codeID string
	err := ts.DB.QueryRow(
		`INSERT INTO friend_codes (user_id, code, max_uses, expires_at, auto_accept)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		userID, code, maxUses, expiresAt, autoAccept,
	).Scan(&codeID)
	require.NoError(t, err)
	return codeID
}

// ============================================================
// BlockUser Tests (Security-Critical: E2EE key revocation)
// ============================================================

func TestBlockUser_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker")
	user2 := ts.CreateTestUser(t, "blocked")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify friendship status changed to blocked
	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusBlocked, status)
}

func TestBlockUser_RecordsRevocation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker2")
	user2 := ts.CreateTestUser(t, "blocked2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify revocation record
	var reason, revokedBy string
	var revokedEpoch, successorEpoch int
	err := ts.DB.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason, revoked_by FROM dm_key_revocations WHERE conversation_id = $1`,
		convID,
	).Scan(&revokedEpoch, &successorEpoch, &reason, &revokedBy)
	require.NoError(t, err)
	assert.Equal(t, 1, revokedEpoch)
	assert.Equal(t, 2, successorEpoch)
	assert.Equal(t, "user_blocked", reason)
	assert.Equal(t, user1.ID, revokedBy)
}

func TestBlockUser_DeletesBlockedUserKey(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker3")
	user2 := ts.CreateTestUser(t, "blocked3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Blocked user's key at version 1 should be deleted
	var keyCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user2.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 0, keyCount, "blocked user's key should be deleted")

	// Blocker's key should still exist
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user1.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 1, keyCount, "blocker's key should remain")
}

func TestBlockUser_NoDMConversation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker4")
	user2 := ts.CreateTestUser(t, "blocked4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	// No DM conversation — should still succeed
	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusBlocked, status)
}

func TestBlockUser_SelfBlock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "selfblocker")

	w := ts.DoRequest("POST", pathFriendsPrefix+user.ID+pathBlock, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBlockUser_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "blocker5")

	w := ts.DoRequest("POST", "/api/v1/friends/not-a-uuid/block", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBlockUser_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "blocker6")

	w := ts.DoRequest("POST", pathFriendsPrefix+user.ID+pathBlock, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBlockUser_NoExistingFriendship(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker7")
	user2 := ts.CreateTestUser(t, "blocked7")

	// No existing friendship — should create a blocked one
	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusBlocked, status)
}

func TestBlockUser_PendingRequest(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blockpend1")
	user2 := ts.CreateTestUser(t, "blockpend2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusPending)

	// Blocking while a pending request exists should work
	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusBlocked, status)
}

func TestBlockUser_ReverseDirectionFriendship(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blockrev1")
	user2 := ts.CreateTestUser(t, "blockrev2")
	// Friendship created with user2 as requester, user1 as addressee
	ts.CreateFriendship(t, user2.ID, user1.ID, statusAccepted)

	// user1 blocks user2 (reverse direction from friendship row)
	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusBlocked, status)
}

// ============================================================
// ListFriends Tests
// ============================================================

func TestListFriends_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listuser1")
	user2 := ts.CreateTestUser(t, "listuser2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 1)
	friend := friends[0].(map[string]interface{})
	assert.Equal(t, user2.ID, friend["user_id"])
}

func TestListFriends_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lonelyuser")

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 0)
}

func TestListFriends_ExcludesBlocked(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listblock1")
	user2 := ts.CreateTestUser(t, "listblock2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusBlocked)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 0, "blocked users should not appear in friends list")
}

func TestListFriends_ExcludesPending(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listpend1")
	user2 := ts.CreateTestUser(t, "listpend2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusPending)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 0, "pending requests should not appear in friends list")
}

func TestListFriends_BothDirections(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listboth1")
	user2 := ts.CreateTestUser(t, "listboth2")
	user3 := ts.CreateTestUser(t, "listboth3")
	// user1 is requester for user2
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	// user3 is requester for user1 (user1 is addressee)
	ts.CreateFriendship(t, user3.ID, user1.ID, statusAccepted)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 2, "should see friends regardless of request direction")

	// Collect friend user IDs
	friendIDs := make(map[string]bool)
	for _, f := range friends {
		friendIDs[f.(map[string]interface{})["user_id"].(string)] = true
	}
	assert.True(t, friendIDs[user2.ID], "user2 should be in friends list")
	assert.True(t, friendIDs[user3.ID], "user3 should be in friends list")
}

func TestListFriends_MultipleFriends(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listmulti1")
	user2 := ts.CreateTestUser(t, "listmulti2")
	user3 := ts.CreateTestUser(t, "listmulti3")
	user4 := ts.CreateTestUser(t, "listmulti4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateFriendship(t, user1.ID, user3.ID, statusAccepted)
	ts.CreateFriendship(t, user1.ID, user4.ID, statusAccepted)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 3)
}

func TestListFriends_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts // ensure server is running

	w := ts.DoRequest("GET", pathFriends, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestListFriends_ResponseFields(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listfields1")
	user2 := ts.CreateTestUser(t, "listfields2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	require.Len(t, friends, 1)
	friend := friends[0].(map[string]interface{})

	// Verify all expected fields are present
	assert.NotEmpty(t, friend["id"], "should have friendship id")
	assert.Equal(t, user2.ID, friend["user_id"])
	assert.Equal(t, user2.Username, friend["username"])
	assert.NotEmpty(t, friend["created_at"], "should have created_at")
}

// ============================================================
// SendRequest Tests
// ============================================================

func TestSendRequest_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender1")
	user2 := ts.CreateTestUser(t, "receiver1")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, statusPending, body["status"])
	assert.NotEmpty(t, body["id"])
	assert.NotEmpty(t, body["created_at"])
}

func TestSendRequest_ToSelf(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "selfsender")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user.ID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendRequest_AlreadyFriends(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender2")
	user2 := ts.CreateTestUser(t, "receiver2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Already friends")
}

func TestSendRequest_ByUsername(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender3")
	user2 := ts.CreateTestUser(t, "receiver3")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"username": user2.Username,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

// TestSendRequest_ByUsername_MixedCaseStored locks the #1931 friend-add half: a
// legacy SSO-style row stored with a mixed-case username must still be resolvable
// by username (lookup is case-insensitive). Pre-fix the raw `username = $1` match
// against the lowercased input returned "User not found".
func TestSendRequest_ByUsername_MixedCaseStored(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender3mixed")
	user2 := ts.CreateTestUser(t, "receiver3mixed")

	// Simulate legacy SSO storage (raw mixed-case), bypassing normalization.
	_, err := ts.DB.Exec(`UPDATE users SET username = 'Receiver3Mixed' WHERE id = $1`, user2.ID)
	require.NoError(t, err)

	// Send by ANY case — the handler lowercases the input and matches LOWER(username).
	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"username": "ReCeIvEr3MiXeD",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code, "mixed-case stored username must resolve, not 404")
}

func TestSendRequest_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendinvalid")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": "not-a-uuid",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendRequest_UserNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendnotfound")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": uuid.New().String(),
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSendRequest_UsernameNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendnouser")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"username": "nonexistentuser999",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSendRequest_NoBodyField(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendempty")

	// Neither user_id nor username provided
	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "user_id or username is required")
}

func TestSendRequest_PendingAlreadyExists(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "senddup1")
	user2 := ts.CreateTestUser(t, "senddup2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusPending)

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "already pending")
}

func TestSendRequest_BlockedUser(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sendblock1")
	user2 := ts.CreateTestUser(t, "sendblock2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusBlocked)

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestSendRequest_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendunauth")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user.ID,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSendRequest_CreatesDBRow(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "senddb1")
	user2 := ts.CreateTestUser(t, "senddb2")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Verify the database row
	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusPending, status)
}

// ============================================================
// RespondRequest Tests
// ============================================================

func TestRespondRequest_Accept(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqsender")
	user2 := ts.CreateTestUser(t, "reqreceiver")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify status changed
	var status string
	err := ts.DB.QueryRow(`SELECT status FROM friendships WHERE id = $1`, requestID).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusAccepted, status)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "accepted")
}

func TestRespondRequest_Decline(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "declsender")
	user2 := ts.CreateTestUser(t, "declreceiver")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "decline",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify friendship was deleted
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM friendships WHERE id = $1`, requestID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "declined")
}

func TestRespondRequest_InvalidRequestID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "respondinv")

	w := ts.DoRequest("PATCH", "/api/v1/friends/request/not-a-uuid", map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRespondRequest_InvalidAction(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "invact1")
	user2 := ts.CreateTestUser(t, "invact2")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "maybe",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "accept")
}

func TestRespondRequest_NotAddresseeCannotAccept(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "notaddr1")
	user2 := ts.CreateTestUser(t, "notaddr2")
	user3 := ts.CreateTestUser(t, "notaddr3")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	// user3 (not the addressee) tries to accept
	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user3.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRespondRequest_RequesterCannotAcceptOwnRequest(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "selfacc1")
	user2 := ts.CreateTestUser(t, "selfacc2")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	// The requester (user1) tries to accept their own request
	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRespondRequest_NonexistentRequest(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nonexreq")

	fakeID := uuid.New().String()
	w := ts.DoRequest("PATCH", pathFriendRequestSlash+fakeID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRespondRequest_AlreadyAccepted(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "alracc1")
	user2 := ts.CreateTestUser(t, "alracc2")

	// Create an accepted friendship (not pending)
	var requestID string
	err := ts.DB.QueryRow(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted') RETURNING id`,
		user1.ID, user2.ID,
	).Scan(&requestID)
	require.NoError(t, err)

	// Try to accept it again — should not find it as pending
	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRespondRequest_MissingActionField(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "missact1")
	user2 := ts.CreateTestUser(t, "missact2")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRespondRequest_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "respunauth1")
	user2 := ts.CreateTestUser(t, "respunauth2")

	requestID := createPendingRequest(t, ts, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "accept",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// RemoveFriend Tests
// ============================================================

func TestRemoveFriend_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover1")
	user2 := ts.CreateTestUser(t, "removee1")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "removed")
}

func TestRemoveFriend_NotFriends(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover2")
	user2 := ts.CreateTestUser(t, "removee2")

	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRemoveFriend_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "reminv")

	w := ts.DoRequest("DELETE", pathFriendsPrefix+"not-a-uuid", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRemoveFriend_CannotRemoveBlocked(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remblk1")
	user2 := ts.CreateTestUser(t, "remblk2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusBlocked)

	// RemoveFriend only deletes status='accepted', not blocked
	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRemoveFriend_CannotRemovePending(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rempend1")
	user2 := ts.CreateTestUser(t, "rempend2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusPending)

	// RemoveFriend only deletes status='accepted', not pending
	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRemoveFriend_ReverseDirection(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remrev1")
	user2 := ts.CreateTestUser(t, "remrev2")
	// Friendship with user2 as requester
	ts.CreateFriendship(t, user2.ID, user1.ID, statusAccepted)

	// user1 removes (they are the addressee)
	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestRemoveFriend_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "remunauth")

	w := ts.DoRequest("DELETE", pathFriendsPrefix+user.ID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// ListRequests Tests
// ============================================================

func TestListRequests_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqlist1")
	user2 := ts.CreateTestUser(t, "reqlist2")

	createPendingRequest(t, ts, user1.ID, user2.ID)

	// Addressee sees the request
	w := ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 1)
	req := requests[0].(map[string]interface{})
	assert.Equal(t, "received", req["direction"])
}

func TestListRequests_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "noreqs")

	w := ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 0)
}

func TestListRequests_SentDirection(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqsent1")
	user2 := ts.CreateTestUser(t, "reqsent2")

	createPendingRequest(t, ts, user1.ID, user2.ID)

	// Requester sees it as "sent"
	w := ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	require.Len(t, requests, 1)
	req := requests[0].(map[string]interface{})
	assert.Equal(t, "sent", req["direction"])
}

func TestListRequests_BothSentAndReceived(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqboth1")
	user2 := ts.CreateTestUser(t, "reqboth2")
	user3 := ts.CreateTestUser(t, "reqboth3")

	// user1 sent a request to user2
	createPendingRequest(t, ts, user1.ID, user2.ID)
	// user3 sent a request to user1
	createPendingRequest(t, ts, user3.ID, user1.ID)

	w := ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 2)

	directions := make(map[string]bool)
	for _, r := range requests {
		directions[r.(map[string]interface{})["direction"].(string)] = true
	}
	assert.True(t, directions["sent"], "should have a sent request")
	assert.True(t, directions["received"], "should have a received request")
}

func TestListRequests_ExcludesAccepted(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqexcl1")
	user2 := ts.CreateTestUser(t, "reqexcl2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 0, "accepted friendships should not appear in requests list")
}

func TestListRequests_ResponseFields(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqfields1")
	user2 := ts.CreateTestUser(t, "reqfields2")

	createPendingRequest(t, ts, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	require.Len(t, requests, 1)
	req := requests[0].(map[string]interface{})

	assert.NotEmpty(t, req["id"])
	assert.Equal(t, user1.ID, req["from_user_id"])
	assert.Equal(t, user1.Username, req["from_username"])
	assert.Equal(t, user2.ID, req["to_user_id"])
	assert.Equal(t, user2.Username, req["to_username"])
	assert.Equal(t, "received", req["direction"])
	assert.NotEmpty(t, req["created_at"])
}

func TestListRequests_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("GET", pathFriendRequests, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// CreateFriendCode Tests
// ============================================================

func TestCreateFriendCode_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codecreate1")

	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	fc := body["friend_code"].(map[string]interface{})
	assert.NotEmpty(t, fc["id"])
	assert.Equal(t, user.ID, fc["user_id"])
	assert.NotEmpty(t, fc["code"])
	assert.Len(t, fc["code"].(string), 8, "friend code should be 8 characters")
	assert.Equal(t, false, fc["is_revoked"])
	assert.NotEmpty(t, fc["created_at"])
}

func TestCreateFriendCode_WithOptions(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codecreate2")

	maxUses := 5
	expiresIn := 7200 // 2 hours
	autoAccept := true

	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"max_uses":    maxUses,
		"expires_in":  expiresIn,
		"auto_accept": autoAccept,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	fc := body["friend_code"].(map[string]interface{})
	assert.Equal(t, float64(maxUses), fc["max_uses"])
	assert.Equal(t, true, fc["auto_accept"])
}

func TestCreateFriendCode_MaxUsesClampedTo10(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codecreate3")

	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"max_uses": 50,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	fc := body["friend_code"].(map[string]interface{})
	assert.Equal(t, float64(10), fc["max_uses"], "max_uses should be clamped to 10")
}

func TestCreateFriendCode_DefaultMaxUses(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codecreate4")

	// No max_uses specified — should default to 1
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	fc := body["friend_code"].(map[string]interface{})
	assert.Equal(t, float64(1), fc["max_uses"], "default max_uses should be 1")
}

func TestCreateFriendCode_ExpiresInClampedMin(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codecreate5")

	// expires_in below minimum (300s)
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"expires_in": 60,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	fc := body["friend_code"].(map[string]interface{})
	// Verify that the code was created (expiry is clamped to 300s minimum)
	assert.NotEmpty(t, fc["expires_at"])
}

func TestCreateFriendCode_ExpiresInClampedMax(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codecreate6")

	// expires_in above maximum (86400s)
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"expires_in": 200000,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	fc := body["friend_code"].(map[string]interface{})
	assert.NotEmpty(t, fc["expires_at"])
}

func TestCreateFriendCode_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathFriendCodes, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// ListFriendCodes Tests
// ============================================================

func TestListFriendCodes_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codelist1")

	// Create a code first
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// List codes
	w = ts.DoRequest("GET", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	codes := body["friend_codes"].([]interface{})
	assert.Len(t, codes, 1)
}

func TestListFriendCodes_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codelist2")

	w := ts.DoRequest("GET", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	codes := body["friend_codes"].([]interface{})
	assert.Len(t, codes, 0)
}

func TestListFriendCodes_ExcludesRevoked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codelist3")

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	codeID := fc["id"].(string)

	// Revoke it
	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// List should be empty
	w = ts.DoRequest("GET", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	codes := body["friend_codes"].([]interface{})
	assert.Len(t, codes, 0, "revoked codes should not appear in list")
}

func TestListFriendCodes_DoesNotShowOtherUsersCodes(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "codelist4")
	user2 := ts.CreateTestUser(t, "codelist5")

	// Create code for user1
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// user2 should see no codes
	w = ts.DoRequest("GET", pathFriendCodes, nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	codes := body["friend_codes"].([]interface{})
	assert.Len(t, codes, 0)
}

func TestListFriendCodes_MultipleCodes(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codelist6")

	// Create 3 codes
	for i := 0; i < 3; i++ {
		w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
	}

	w := ts.DoRequest("GET", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	codes := body["friend_codes"].([]interface{})
	assert.Len(t, codes, 3)
}

func TestListFriendCodes_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("GET", pathFriendCodes, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// RevokeFriendCode Tests
// ============================================================

func TestRevokeFriendCode_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "coderevoke1")

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	codeID := fc["id"].(string)

	// Revoke it
	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "revoked")

	// Verify in DB
	var isRevoked bool
	err := ts.DB.QueryRow(`SELECT is_revoked FROM friend_codes WHERE id = $1`, codeID).Scan(&isRevoked)
	require.NoError(t, err)
	assert.True(t, isRevoked)
}

func TestRevokeFriendCode_InvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "coderevoke2")

	w := ts.DoRequest("DELETE", pathFriendCodes+"/not-a-uuid", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeFriendCode_NotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "coderevoke3")

	fakeID := uuid.New().String()
	w := ts.DoRequest("DELETE", pathFriendCodes+"/"+fakeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeFriendCode_AlreadyRevoked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "coderevoke4")

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	codeID := fc["id"].(string)

	// Revoke once
	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Revoke again — should be not found
	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeFriendCode_CannotRevokeOtherUsersCode(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "coderevoke5")
	user2 := ts.CreateTestUser(t, "coderevoke6")

	// Create code for user1
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	codeID := fc["id"].(string)

	// user2 tries to revoke user1's code
	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeFriendCode_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	fakeID := uuid.New().String()
	w := ts.DoRequest("DELETE", pathFriendCodes+"/"+fakeID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// PreviewFriendCode Tests
// ============================================================

func TestPreviewFriendCode_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codepreview1")

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Preview it (any authenticated user can preview)
	viewer := ts.CreateTestUser(t, "codeviewer1")
	w = ts.DoRequest("GET", pathFriendCodes+"/"+code, nil, testhelpers.AuthHeaders(viewer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, user.ID, body["user_id"])
	assert.Equal(t, user.Username, body["username"])
	assert.Equal(t, true, body["valid"])
}

func TestPreviewFriendCode_InvalidFormat(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codepreview2")

	// Code is not 8 characters
	w := ts.DoRequest("GET", pathFriendCodes+"/abc", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPreviewFriendCode_NotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codepreview3")

	w := ts.DoRequest("GET", pathFriendCodes+"/ABCD1234", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPreviewFriendCode_RevokedCodeShowsInvalid(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codepreview4")

	// Create and revoke a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	codeID := fc["id"].(string)
	code := fc["code"].(string)

	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Preview revoked code
	viewer := ts.CreateTestUser(t, "codeviewer4")
	w = ts.DoRequest("GET", pathFriendCodes+"/"+code, nil, testhelpers.AuthHeaders(viewer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["valid"], "revoked code should show valid=false")
}

func TestPreviewFriendCode_ExpiredCodeShowsInvalid(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codepreview5")

	// Insert an already-expired code directly
	expiredAt := time.Now().UTC().Add(-1 * time.Hour)
	createFriendCode(t, ts, user.ID, "EXPR1234", nil, &expiredAt, false)

	viewer := ts.CreateTestUser(t, "codeviewer5")
	w := ts.DoRequest("GET", pathFriendCodes+"/EXPR1234", nil, testhelpers.AuthHeaders(viewer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["valid"], "expired code should show valid=false")
}

func TestPreviewFriendCode_MaxUsesReachedShowsInvalid(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "codepreview6")

	// Insert a code with max_uses=1 and use_count=1
	maxUses := 1
	futureExpiry := time.Now().UTC().Add(1 * time.Hour)
	codeID := createFriendCode(t, ts, user.ID, "MAXU1234", &maxUses, &futureExpiry, false)

	// Manually set use_count to max
	_, err := ts.DB.Exec(`UPDATE friend_codes SET use_count = 1 WHERE id = $1`, codeID)
	require.NoError(t, err)

	viewer := ts.CreateTestUser(t, "codeviewer6")
	w := ts.DoRequest("GET", pathFriendCodes+"/MAXU1234", nil, testhelpers.AuthHeaders(viewer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["valid"], "exhausted code should show valid=false")
}

func TestPreviewFriendCode_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("GET", pathFriendCodes+"/ABCD1234", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// ClaimFriendCode Tests
// ============================================================

func TestClaimFriendCode_SuccessPending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "codeowner1")
	claimer := ts.CreateTestUser(t, "codeclaim1")

	// Create a code (auto_accept = false by default)
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Claim the code
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, statusPending, body["status"])
	assert.NotEmpty(t, body["friendship_id"])
	userInfo := body["user"].(map[string]interface{})
	assert.Equal(t, owner.ID, userInfo["user_id"])
	assert.Equal(t, owner.Username, userInfo["username"])

	// Verify friendship in DB
	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`,
		claimer.ID, owner.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusPending, status)
}

func TestClaimFriendCode_SuccessAutoAccept(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "codeowner2")
	claimer := ts.CreateTestUser(t, "codeclaim2")

	// Create a code with auto_accept
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"auto_accept": true,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Claim the code
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, statusAccepted, body["status"])

	// Verify friendship is directly accepted in DB
	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`,
		claimer.ID, owner.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, statusAccepted, status)
}

func TestClaimFriendCode_IncrementsUseCount(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "codeowner3")
	claimer := ts.CreateTestUser(t, "codeclaim3")

	// Create a code with max_uses=5
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)
	codeID := fc["id"].(string)

	// Claim the code
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify use_count incremented
	var useCount int
	err := ts.DB.QueryRow(`SELECT use_count FROM friend_codes WHERE id = $1`, codeID).Scan(&useCount)
	require.NoError(t, err)
	assert.Equal(t, 1, useCount)
}

func TestClaimFriendCode_InvalidFormat(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "claiminv1")

	w := ts.DoRequest("POST", pathFriendCodes+"/abc/claim", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestClaimFriendCode_NotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "claimnotf1")

	w := ts.DoRequest("POST", pathFriendCodes+"/ABCD1234/claim", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestClaimFriendCode_OwnCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimown1")

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Try to claim own code
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "own friend code")
}

func TestClaimFriendCode_AlreadyFriends(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimdup1")
	claimer := ts.CreateTestUser(t, "claimdup2")
	ts.CreateFriendship(t, owner.ID, claimer.ID, statusAccepted)

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Try to claim — already friends
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Already friends")
}

func TestClaimFriendCode_PendingRequestExists(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimpend1")
	claimer := ts.CreateTestUser(t, "claimpend2")
	ts.CreateFriendship(t, owner.ID, claimer.ID, statusPending)

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Try to claim — pending exists
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestClaimFriendCode_BlockedRelationship(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimblk1")
	claimer := ts.CreateTestUser(t, "claimblk2")
	ts.CreateFriendship(t, owner.ID, claimer.ID, statusBlocked)

	// Create a code
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Try to claim — blocked
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestClaimFriendCode_RevokedCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimrev1")
	claimer := ts.CreateTestUser(t, "claimrev2")

	// Create and revoke a code
	w := ts.DoRequest("POST", pathFriendCodes, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	codeID := fc["id"].(string)
	code := fc["code"].(string)

	w = ts.DoRequest("DELETE", pathFriendCodes+"/"+codeID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Try to claim revoked code
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusGone, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "revoked")
}

func TestClaimFriendCode_ExpiredCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimexp1")
	claimer := ts.CreateTestUser(t, "claimexp2")

	// Insert an already-expired code directly
	expiredAt := time.Now().UTC().Add(-1 * time.Hour)
	maxUses := 5
	createFriendCode(t, ts, owner.ID, "CLMX1234", &maxUses, &expiredAt, false)

	w := ts.DoRequest("POST", pathFriendCodes+"/CLMX1234/claim", nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusGone, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "expired")
}

func TestClaimFriendCode_MaxUsesReached(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "claimmax1")
	claimer := ts.CreateTestUser(t, "claimmax2")

	// Insert a code with max_uses=1 and use_count already at 1
	maxUses := 1
	futureExpiry := time.Now().UTC().Add(1 * time.Hour)
	codeID := createFriendCode(t, ts, owner.ID, "MAXC1234", &maxUses, &futureExpiry, false)

	_, err := ts.DB.Exec(`UPDATE friend_codes SET use_count = 1 WHERE id = $1`, codeID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathFriendCodes+"/MAXC1234/claim", nil, testhelpers.AuthHeaders(claimer.AccessToken))
	assert.Equal(t, http.StatusGone, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "maximum uses")
}

func TestClaimFriendCode_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathFriendCodes+"/ABCD1234/claim", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================
// End-to-End Friend Lifecycle Tests
// ============================================================

func TestFriendLifecycle_RequestAcceptRemove(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "lifecycle1")
	user2 := ts.CreateTestUser(t, "lifecycle2")

	// Step 1: Send request
	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var reqBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &reqBody)
	requestID := reqBody["id"].(string)

	// Step 2: Verify request appears in user2's requests
	w = ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var listBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &listBody)
	requests := listBody["requests"].([]interface{})
	require.Len(t, requests, 1)

	// Step 3: Accept request
	w = ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Step 4: Verify they are friends
	w = ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var friendsBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &friendsBody)
	friends := friendsBody["friends"].([]interface{})
	require.Len(t, friends, 1)
	assert.Equal(t, user2.ID, friends[0].(map[string]interface{})["user_id"])

	// Step 5: Remove friend
	w = ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Step 6: Verify no longer friends
	w = ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &friendsBody)
	friends = friendsBody["friends"].([]interface{})
	assert.Len(t, friends, 0)
}

func TestFriendLifecycle_RequestDecline(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "lifecycled1")
	user2 := ts.CreateTestUser(t, "lifecycled2")

	// Send request
	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var reqBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &reqBody)
	requestID := reqBody["id"].(string)

	// Decline
	w = ts.DoRequest("PATCH", pathFriendRequestSlash+requestID, map[string]interface{}{
		"action": "decline",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify they are NOT friends
	w = ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 0)

	// Verify no pending requests
	w = ts.DoRequest("GET", pathFriendRequests, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 0)
}

func TestFriendLifecycle_FriendCodeAutoAccept(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "lifecyclec1")
	claimer := ts.CreateTestUser(t, "lifecyclec2")

	// Create auto-accept code
	w := ts.DoRequest("POST", pathFriendCodes, map[string]interface{}{
		"auto_accept": true,
		"max_uses":    5,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	fc := createBody["friend_code"].(map[string]interface{})
	code := fc["code"].(string)

	// Claim it
	w = ts.DoRequest("POST", pathFriendCodes+"/"+code+pathClaim, nil, testhelpers.AuthHeaders(claimer.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify they are immediately friends
	w = ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	require.Len(t, friends, 1)
	assert.Equal(t, claimer.ID, friends[0].(map[string]interface{})["user_id"])
}

func TestFriendLifecycle_BlockAfterFriends(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "lifecycleb1")
	user2 := ts.CreateTestUser(t, "lifecycleb2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	// Verify they are friends
	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Len(t, body["friends"].([]interface{}), 1)

	// Block user2
	w = ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify no longer friends
	w = ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	assert.Len(t, body["friends"].([]interface{}), 0)

	// Verify user2 also cannot see user1 as friend
	w = ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	assert.Len(t, body["friends"].([]interface{}), 0)
}
