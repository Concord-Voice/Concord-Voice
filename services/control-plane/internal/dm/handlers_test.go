package dm_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mimeImagePNG = "image/png"

const (
	pathDMConversationsPrefix = "/api/v1/dm/conversations/"
	pathDMConversations       = "/api/v1/dm/conversations"
	pathRotateKey             = "/rotate-key"
	statusAccepted            = "accepted"
	statusPending             = "pending"

	notAUUID       = "not-a-uuid"
	fmtExpected2xx = "expected 200 or 201, got %d"
	pathGroup      = "/group"
	pathMessages   = "/messages"
	wrappedKey     = "wrapped-key"
	pathVoiceJoin  = "/voice/join"
	pathVoiceParts = "/voice/participants"
	pathUserMute   = "/user-mute"
	pathMute       = "/mute"
	pathDeafen     = "/deafen"
	pathVoiceSlash = "/voice/"
	pathPersonal   = "/personal"
	pathMsgSlash   = "/messages/"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// insertDMMessage inserts a DM message directly into the database and returns its ID.
func insertDMMessage(t *testing.T, ts *testhelpers.TestServer, convID, userID, content string) string {
	t.Helper()
	msgID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, type) VALUES ($1, $2, $3, $4, 'text')`,
		msgID, convID, userID, content,
	)
	require.NoError(t, err, "failed to insert DM message")
	return msgID
}

// setDMPrivacy inserts or updates a user's DM privacy settings.
func setDMPrivacy(t *testing.T, ts *testhelpers.TestServer, userID string, level int, fof bool) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, dm_privacy_level, dm_friends_of_friends)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET dm_privacy_level = $2, dm_friends_of_friends = $3
	`, userID, level, fof)
	require.NoError(t, err, "failed to set DM privacy")
}

// TestUpdateMessage_RejectsOverCiphertextCap locks the max=65536 binding (#1298):
// a DM-edit body over the single hard ciphertext ceiling is rejected at bind time.
func TestUpdateMessage_RejectsOverCiphertextCap(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmcapauthor")
	u2 := ts.CreateTestUser(t, "dmcappeer")
	ts.CreateFriendship(t, u1.ID, u2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessage(t, ts, convID, u1.ID, "original")

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID,
		map[string]interface{}{"content": strings.Repeat("x", 65537)},
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestUpdateMessage_AcceptsWithinCap confirms the max=65536 binding does not
// over-reject a legitimate within-cap edit (valid ciphertext shape).
func TestUpdateMessage_AcceptsWithinCap(t *testing.T) {
	ts := setupTS(t)
	u1 := ts.CreateTestUser(t, "dmokauthor")
	u2 := ts.CreateTestUser(t, "dmokpeer")
	ts.CreateFriendship(t, u1.ID, u2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, u1.ID, u2.ID)
	msgID := insertDMMessage(t, ts, convID, u1.ID, "original")

	// Valid ciphertext shape (base64 decoding to >= 28 bytes), well within the cap.
	content := base64.StdEncoding.EncodeToString(make([]byte, 48))
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID,
		map[string]interface{}{"content": content},
		testhelpers.AuthHeaders(u1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ============================================================================
// RotateKey Tests (Security-Critical: E2EE forward secrecy)
// ============================================================================

func TestRotateKey_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator1")
	user2 := ts.CreateTestUser(t, "rotator2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["new_key_version"])
}

func TestRotateKey_RecordsRevocation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator3")
	user2 := ts.CreateTestUser(t, "rotator4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

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
	assert.Equal(t, "manual_rotation", reason)
	assert.Equal(t, user1.ID, revokedBy)
}

func TestRotateKey_NoExistingKeys(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator5")
	user2 := ts.CreateTestUser(t, "rotator6")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	// No keys seeded — maxVersion = 0

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["new_key_version"])

	// No revocation should be recorded (guard: if maxVersion > 0)
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revocation should be recorded when maxVersion=0")
}

func TestRotateKey_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator7")
	user2 := ts.CreateTestUser(t, "rotator8")
	outsider := ts.CreateTestUser(t, "outsider1")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRotateKey_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rotator9")

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/not-a-uuid/rotate-key", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRotateKey_MultipleRotations(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "multirot1")
	user2 := ts.CreateTestUser(t, "multirot2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)

	// First rotation
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Seed the new key version so second rotation has something to revoke
	ts.SeedDMKey(t, convID, user1.ID, 2)

	// Second rotation
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(3), body["new_key_version"])

	// Should have 2 revocation records
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestRotateKey_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	convID := uuid.New().String()

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// ListConversations Tests
// ============================================================================

func TestListConversations_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listdm1")
	user2 := ts.CreateTestUser(t, "listdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.GreaterOrEqual(t, len(conversations), 1)
}

func TestListConversations_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lonelydm")

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.Len(t, conversations, 0)
}

func TestListConversations_IncludesParticipants(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listpart1")
	user2 := ts.CreateTestUser(t, "listpart2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.Len(t, conversations, 1)

	conv := conversations[0].(map[string]interface{})
	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 2)
}

func TestListConversations_WithLastMessage(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listlm1")
	user2 := ts.CreateTestUser(t, "listlm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	insertDMMessage(t, ts, convID, user1.ID, "hello there")

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.Len(t, conversations, 1)

	conv := conversations[0].(map[string]interface{})
	lm := conv["last_message"].(map[string]interface{})
	assert.Equal(t, "hello there", lm["content"])
	assert.Equal(t, user1.ID, lm["user_id"])
}

func TestListConversations_UnreadCount(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "unread1")
	user2 := ts.CreateTestUser(t, "unread2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// user2 sends 3 messages (unread for user1)
	insertDMMessage(t, ts, convID, user2.ID, "msg1")
	insertDMMessage(t, ts, convID, user2.ID, "msg2")
	insertDMMessage(t, ts, convID, user2.ID, "msg3")

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.Len(t, conversations, 1)
	conv := conversations[0].(map[string]interface{})
	assert.Equal(t, float64(3), conv["unread_count"])
}

func TestListConversations_MultipleConversations(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listmulti1")
	user2 := ts.CreateTestUser(t, "listmulti2")
	user3 := ts.CreateTestUser(t, "listmulti3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateFriendship(t, user1.ID, user3.ID, statusAccepted)
	ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.CreateDMConversation(t, user1.ID, user3.ID)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.Len(t, conversations, 2)
}

func TestListConversations_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts // ensure server is up

	w := ts.DoRequest("GET", pathDMConversations, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// GetConversation Tests
// ============================================================================

func TestGetConversation_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdm1")
	user2 := ts.CreateTestUser(t, "getdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, convID, conv["id"])
	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 2)
}

func TestGetConversation_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdm3")
	user2 := ts.CreateTestUser(t, "getdm4")
	outsider := ts.CreateTestUser(t, "getdm5")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetConversation_InvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getdminv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetConversation_NonexistentID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getdmnone")
	fakeID := uuid.New().String()

	w := ts.DoRequest("GET", pathDMConversationsPrefix+fakeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code) // Not participant of nonexistent conv
}

func TestGetConversation_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdmunauth1")
	user2 := ts.CreateTestUser(t, "getdmunauth2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// OpenConversation Tests
// ============================================================================

func TestOpenConversation_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opendm1")
	user2 := ts.CreateTestUser(t, "opendm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))

	// Should be 200 or 201
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
}

func TestOpenConversation_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opendm3")
	user2 := ts.CreateTestUser(t, "opendm4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	// Open twice — should return same conversation
	w1 := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.True(t, w1.Code == http.StatusOK || w1.Code == http.StatusCreated)

	var body1 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	conv1 := body1["conversation"].(map[string]interface{})

	w2 := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.True(t, w2.Code == http.StatusOK || w2.Code == http.StatusCreated)

	var body2 map[string]interface{}
	testhelpers.ParseJSON(t, w2, &body2)
	conv2 := body2["conversation"].(map[string]interface{})

	assert.Equal(t, conv1["id"], conv2["id"], "same conversation should be returned")
}

func TestOpenConversation_MissingUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openmissing")

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestOpenConversation_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openinvalid")

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": notAUUID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestOpenConversation_SelfDM(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openself")

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user.ID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "yourself")
}

func TestOpenConversation_UserNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openghost")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": fakeID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestOpenConversation_PrivacyBlocked_DMDisabled(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openprivdis1")
	user2 := ts.CreateTestUser(t, "openprivdis2")
	// No friendship. Target has DMs disabled.
	setDMPrivacy(t, ts, user2.ID, 0, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "dm_disabled", body["error"])
}

func TestOpenConversation_PrivacyBlocked_FriendsOnly(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openprivfr1")
	user2 := ts.CreateTestUser(t, "openprivfr2")
	// No friendship. Target allows friends only.
	setDMPrivacy(t, ts, user2.ID, 1, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "privacy_blocked", body["error"])
}

func TestOpenConversation_FriendsAllowed(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openfriendok1")
	user2 := ts.CreateTestUser(t, "openfriendok2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	// Target has friends only, but they are friends
	setDMPrivacy(t, ts, user2.ID, 1, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

func TestOpenConversation_SharedServerAllowed(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opensrvok1")
	user2 := ts.CreateTestUser(t, "opensrvok2")
	// No friendship, but share a server. Target allows friends + server members (level 2, default).
	serverID := ts.CreateTestServer(t, user1.ID, "Shared Server")
	ts.AddMemberToServer(t, serverID, user2.ID, "member")
	setDMPrivacy(t, ts, user2.ID, 2, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

func TestOpenConversation_OpenToAll(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openallok1")
	user2 := ts.CreateTestUser(t, "openallok2")
	// No friendship, no shared servers, but target allows all.
	setDMPrivacy(t, ts, user2.ID, 3, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

// TestOpenConversation_FoFAllowed verifies that the friends-of-friends privacy
// path correctly allows a DM when sender and target share a mutual friend.
// Reproduction test for #1142 — FAILS against the broken FoF query that
// references non-existent user_id/friend_id columns; PASSES after the fix.
func TestOpenConversation_FoFAllowed(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "fofsender1")
	mutual := ts.CreateTestUser(t, "fofmutual1")
	target := ts.CreateTestUser(t, "foftarget1")
	// sender ↔ mutual and mutual ↔ target are both accepted friendships.
	ts.CreateFriendship(t, sender.ID, mutual.ID, statusAccepted)
	ts.CreateFriendship(t, mutual.ID, target.ID, statusAccepted)
	// Target accepts friends + server members AND friends-of-friends.
	setDMPrivacy(t, ts, target.ID, 2, true)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

// TestOpenConversation_FoFBlocked_NoMutualFriend verifies that when no mutual
// friend exists, a friends-only-with-FoF target correctly blocks the DM.
// Guards against an overly-permissive FoF query regression.
func TestOpenConversation_FoFBlocked_NoMutualFriend(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "fofnoblock1")
	target := ts.CreateTestUser(t, "fofnotarget1")
	// No friendship and no mutual friend between sender and target.
	// Target allows friends + FoF only, no server fallback (level 1).
	setDMPrivacy(t, ts, target.ID, 1, true)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "privacy_blocked", body["error"])
}

// TestOpenConversation_FoFDisabled_SharedServerFallback verifies that when FoF
// is disabled, the shared-server fallback path still allows the DM. Guards
// against a refactor that entangles FoF and server-share paths.
func TestOpenConversation_FoFDisabled_SharedServerFallback(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "foffb1")
	target := ts.CreateTestUser(t, "foffb2")
	// No friendship, no mutual friend; share a server.
	serverID := ts.CreateTestServer(t, sender.ID, "FoF Fallback Server")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	// Target allows friends + server members, no FoF (level 2).
	setDMPrivacy(t, ts, target.ID, 2, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

// TestOpenConversation_FoFPendingStatusIgnored verifies that a friendship row
// with status='pending' does NOT count as an accepted FoF connection. Guards
// against a future refactor that drops the status='accepted' filter from
// the FoF query.
func TestOpenConversation_FoFPendingStatusIgnored(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "fofpend1")
	mutual := ts.CreateTestUser(t, "fofpend2")
	target := ts.CreateTestUser(t, "fofpend3")
	// sender ↔ mutual is accepted, but mutual ↔ target is only pending.
	ts.CreateFriendship(t, sender.ID, mutual.ID, statusAccepted)
	ts.CreateFriendship(t, mutual.ID, target.ID, statusPending)
	// Target allows friends + FoF, no server fallback (level 1).
	setDMPrivacy(t, ts, target.ID, 1, true)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "privacy_blocked", body["error"])
}

// TestOpenConversation_FoFAllowed_Level1 verifies that the FoF path runs at
// privacy level 1 (friends-only, no server fallback) when fofEnabled=true.
// Guards against a future regression that gates FoF behind privacyLevel>=2.
// The sole existing positive-FoF coverage (TestOpenConversation_FoFAllowed)
// uses level=2; without this test, breaking FoF at level=1 would go unnoticed.
func TestOpenConversation_FoFAllowed_Level1(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "foflv1sender")
	mutual := ts.CreateTestUser(t, "foflv1mutual")
	target := ts.CreateTestUser(t, "foflv1target")
	// sender ↔ mutual and mutual ↔ target are both accepted friendships.
	ts.CreateFriendship(t, sender.ID, mutual.ID, statusAccepted)
	ts.CreateFriendship(t, mutual.ID, target.ID, statusAccepted)
	// Target allows friends-only + FoF (level 1, no server fallback).
	setDMPrivacy(t, ts, target.ID, 1, true)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

// TestOpenConversation_FoFSenderSidePendingIgnored is the symmetric companion
// to TestOpenConversation_FoFPendingStatusIgnored. The latter covers the
// target-side status filter; this one covers the sender-side filter. If the
// status='accepted' predicate is dropped from EITHER CTE in the FoF query,
// only the test pinning that side catches the regression.
func TestOpenConversation_FoFSenderSidePendingIgnored(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "fofspend1")
	mutual := ts.CreateTestUser(t, "fofspend2")
	target := ts.CreateTestUser(t, "fofspend3")
	// sender ↔ mutual is pending (sender-side edge not yet accepted),
	// mutual ↔ target is accepted. Without status='accepted' filter on the
	// sender-side CTE, this would false-positively grant FoF.
	ts.CreateFriendship(t, sender.ID, mutual.ID, statusPending)
	ts.CreateFriendship(t, mutual.ID, target.ID, statusAccepted)
	// Target allows friends + FoF, no server fallback (level 1).
	setDMPrivacy(t, ts, target.ID, 1, true)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "privacy_blocked", body["error"])
}

// TestOpenConversation_FriendshipsQueryError_FailsClosed verifies that when
// the `friendships` query inside enforceDMPrivacy errors (simulated here by
// renaming the table so the query returns "relation does not exist"), the
// handler responds with HTTP 500 — i.e., fail-CLOSED — instead of silently
// swallowing the error and falling through to "not a friend." This is the
// behavioral regression guard for the line-325 errcheck fix that closed
// the same class of fail-OPEN bug as the original #1142.
//
// t.Cleanup runs in LIFO order, so the rename-back runs BEFORE the harness's
// TruncateAllTables cleanup — TruncateAllTables sees the table back at its
// original name and runs normally.
func TestOpenConversation_FriendshipsQueryError_FailsClosed(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "frienderr1")
	target := ts.CreateTestUser(t, "frienderr2")
	// Privacy level is irrelevant — the friendship pre-check runs first
	// and triggers the error path before any privacy logic.
	setDMPrivacy(t, ts, target.ID, 1, false)

	// Rename the friendships table so the EXISTS query returns
	// "relation \"friendships\" does not exist" — a non-nil error from Scan.
	_, err := ts.DB.Exec("ALTER TABLE friendships RENAME TO friendships_dberrtest")
	require.NoError(t, err, "failed to rename friendships table for fault injection")
	t.Cleanup(func() {
		// Surface rename-back failures explicitly — silently swallowing them
		// would leave the table renamed for the rest of the package run, and
		// every subsequent test would fail with confusing "relation does not
		// exist" errors that don't point at this cleanup. t.Errorf (not
		// t.Fatalf) marks the test failed but allows other cleanups to run.
		if _, err := ts.DB.Exec("ALTER TABLE friendships_dberrtest RENAME TO friendships"); err != nil {
			t.Errorf("cleanup: failed to rename friendships back to original: %v", err)
		}
	})

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"friendships-query error must fail-CLOSED with 500, not silently allow the DM")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to open conversation", body["error"])
}

// TestOpenConversation_ServerMembersQueryError_FailsClosed verifies the
// shared-server fallback path (inside isDMAllowedByRelationship) fails-CLOSED
// on DB error rather than silently returning "doesn't share a server."
// Setup arranges: non-friend sender, target with dm_privacy_level=2 and FoF
// disabled, so execution reaches the server_members query at the bottom
// of the function. Renaming the server_members table makes that query error.
func TestOpenConversation_ServerMembersQueryError_FailsClosed(t *testing.T) {
	ts := setupTS(t)
	sender := ts.CreateTestUser(t, "srverr1")
	target := ts.CreateTestUser(t, "srverr2")
	// No friendship between sender and target.
	// Target allows friends + server members (level 2), no FoF — forces
	// the shared-server fallback path inside isDMAllowedByRelationship.
	setDMPrivacy(t, ts, target.ID, 2, false)

	_, err := ts.DB.Exec("ALTER TABLE server_members RENAME TO server_members_dberrtest")
	require.NoError(t, err, "failed to rename server_members table for fault injection")
	t.Cleanup(func() {
		// See TestOpenConversation_FriendshipsQueryError_FailsClosed cleanup
		// comment: surface rename-back failures rather than silently swallow.
		if _, err := ts.DB.Exec("ALTER TABLE server_members_dberrtest RENAME TO server_members"); err != nil {
			t.Errorf("cleanup: failed to rename server_members back to original: %v", err)
		}
	})

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(sender.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"server_members-query error must fail-CLOSED with 500")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to open conversation", body["error"])
}

func TestOpenConversation_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": uuid.New().String(),
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// CreateGroup Tests
// ============================================================================

func TestCreateGroup_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "grp1")
	user2 := ts.CreateTestUser(t, "grp2")
	user3 := ts.CreateTestUser(t, "grp3")

	groupName := "Test Group"
	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID, user3.ID},
		"name":     groupName,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
	assert.Equal(t, true, conv["is_group"])
	assert.Equal(t, groupName, conv["name"])

	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 3)
}

func TestCreateGroup_NoName(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "grpnoname1")
	user2 := ts.CreateTestUser(t, "grpnoname2")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.True(t, conv["is_group"].(bool))
}

func TestCreateGroup_MissingUserIDs(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpmissing")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateGroup_EmptyUserIDs(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpempty")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateGroup_TooManyParticipants(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "grpbig")

	// Create 10 other users — exceeds the limit of 9
	userIDs := make([]string, 10)
	for i := 0; i < 10; i++ {
		u := ts.CreateTestUser(t, fmt.Sprintf("grpbig%d", i))
		userIDs[i] = u.ID
	}

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": userIDs,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "10 participants")
}

func TestCreateGroup_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpinvaliduid")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{notAUUID},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateGroup_IncludesSelf(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpself")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user.ID},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "yourself")
}

func TestCreateGroup_MaxParticipants(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "grpmax")

	// Create exactly 9 other users — total 10 including creator, at the limit
	userIDs := make([]string, 9)
	for i := 0; i < 9; i++ {
		u := ts.CreateTestUser(t, fmt.Sprintf("grpmax%d", i))
		userIDs[i] = u.ID
	}

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": userIDs,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateGroup_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{uuid.New().String()},
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// UpdateConversation Tests
// ============================================================================

func TestUpdateConversation_RenameGroup(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconv1")
	user2 := ts.CreateTestUser(t, "upconv2")
	user3 := ts.CreateTestUser(t, "upconv3")

	// Create a group DM via API
	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID, user3.ID},
		"name":     "Old Name",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	newName := "New Group Name"
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": newName,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateConversation_CannotRenameOneOnOne(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconv1on1a")
	user2 := ts.CreateTestUser(t, "upconv1on1b")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": "New Name",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "1:1")
}

func TestUpdateConversation_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconvnp1")
	user2 := ts.CreateTestUser(t, "upconvnp2")
	outsider := ts.CreateTestUser(t, "upconvnp3")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
		"name":     "Secret Group",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": "Hacked Name",
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateConversation_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "upconvinv")

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+notAUUID, map[string]interface{}{
		"name": "New Name",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateConversation_InvalidBody(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconvbadbody1")
	user2 := ts.CreateTestUser(t, "upconvbadbody2")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	// Send request with no JSON body
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateConversation_ClearName(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconvclear1")
	user2 := ts.CreateTestUser(t, "upconvclear2")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
		"name":     "Has Name",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	// Setting name to null clears it
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": nil,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ============================================================================
// GetMessages Tests
// ============================================================================

func TestGetMessages_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsg1")
	user2 := ts.CreateTestUser(t, "getmsg2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	insertDMMessage(t, ts, convID, user1.ID, "hello")
	insertDMMessage(t, ts, convID, user2.ID, "hi there")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Len(t, messages, 2)
}

func TestGetMessages_Empty(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsge1")
	user2 := ts.CreateTestUser(t, "getmsge2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Len(t, messages, 0)
}

func TestGetMessages_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgnp1")
	user2 := ts.CreateTestUser(t, "getmsgnp2")
	outsider := ts.CreateTestUser(t, "getmsgnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessages_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsginv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+"not-a-uuid/messages", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMessages_CursorPagination(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgcur1")
	user2 := ts.CreateTestUser(t, "getmsgcur2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Insert some messages
	insertDMMessage(t, ts, convID, user1.ID, "msg1")
	insertDMMessage(t, ts, convID, user1.ID, "msg2")
	cursorMsgID := insertDMMessage(t, ts, convID, user1.ID, "msg3")

	// Fetch messages before the cursor
	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages?before="+cursorMsgID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	// msg3 is the cursor, so we should get msg1 and msg2 (created before msg3)
	assert.Equal(t, 2, len(messages))
}

func TestGetMessages_InvalidCursor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgbadc1")
	user2 := ts.CreateTestUser(t, "getmsgbadc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages?before=not-a-uuid", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMessages_MessageFields(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgfields1")
	user2 := ts.CreateTestUser(t, "getmsgfields2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	insertDMMessage(t, ts, convID, user1.ID, "test content")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	require.Len(t, messages, 1)

	msg := messages[0].(map[string]interface{})
	assert.NotEmpty(t, msg["id"])
	assert.Equal(t, convID, msg["conversation_id"])
	assert.Equal(t, user1.ID, msg["user_id"])
	assert.Equal(t, "test content", msg["content"])
	assert.Equal(t, "text", msg["type"])
	assert.NotEmpty(t, msg["created_at"])
	assert.NotEmpty(t, msg["username"])
}

func TestGetMessages_IncludesCallEventPayload(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "cep-caller")
	other := ts.CreateTestUser(t, "cep-other")
	ts.CreateFriendship(t, caller.ID, other.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, caller.ID, other.ID)

	payload := `{"status":"completed","participant_user_ids":["` + caller.ID + `","` + other.ID + `"],"duration_seconds":42,"started_at":"2026-06-15T00:00:00Z"}`
	_, err := ts.DB.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, type, call_event_payload, created_at)
		VALUES (gen_random_uuid(), $1, $2, '', 'call_event', $3, NOW())`, convID, caller.ID, payload)
	require.NoError(t, err)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &body)
	require.Len(t, body.Messages, 1)
	assert.JSONEq(t, `"call_event"`, string(body.Messages[0]["type"]))
	assert.Contains(t, string(body.Messages[0]["call_event_payload"]), `"status":"completed"`)
}

func TestGetMessages_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgunauth1")
	user2 := ts.CreateTestUser(t, "getmsgunauth2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// MarkRead Tests
// ============================================================================

func TestMarkRead_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markread1")
	user2 := ts.CreateTestUser(t, "markread2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Marked as read", body["message"])
}

func TestMarkRead_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markread3")
	user2 := ts.CreateTestUser(t, "markread4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Mark read twice — should succeed both times (UPSERT)
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMarkRead_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markreadnp1")
	user2 := ts.CreateTestUser(t, "markreadnp2")
	outsider := ts.CreateTestUser(t, "markreadnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestMarkRead_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "markreadinv")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/read", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMarkRead_ResetsUnreadCount(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markreadreset1")
	user2 := ts.CreateTestUser(t, "markreadreset2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// user2 sends messages
	insertDMMessage(t, ts, convID, user2.ID, "unread1")
	insertDMMessage(t, ts, convID, user2.ID, "unread2")

	// Verify unread count > 0
	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var listBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &listBody)
	convs := listBody["conversations"].([]interface{})
	require.Len(t, convs, 1)
	assert.Equal(t, float64(2), convs[0].(map[string]interface{})["unread_count"])

	// Mark as read
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify unread count is now 0
	w = ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &listBody)
	convs = listBody["conversations"].([]interface{})
	require.Len(t, convs, 1)
	assert.Equal(t, float64(0), convs[0].(map[string]interface{})["unread_count"])
}

// ============================================================================
// GetKeys Tests
// ============================================================================

func TestGetKeys_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeys1")
	user2 := ts.CreateTestUser(t, "getkeys2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	key := body["key"].(map[string]interface{})
	assert.Equal(t, convID, key["conversation_id"])
	assert.Equal(t, user1.ID, key["user_id"])
	assert.Equal(t, float64(1), key["key_version"])
}

func TestGetKeys_NoKeysYet(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeysnone1")
	user2 := ts.CreateTestUser(t, "getkeysnone2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	// No keys seeded

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["pending"])
}

func TestGetKeys_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeysnp1")
	user2 := ts.CreateTestUser(t, "getkeysnp2")
	outsider := ts.CreateTestUser(t, "getkeysnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetKeys_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getkeysinv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+"not-a-uuid/keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetKeys_ReturnsLatestVersion(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeyslat1")
	user2 := ts.CreateTestUser(t, "getkeyslat2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user1.ID, 2)
	ts.SeedDMKey(t, convID, user1.ID, 3)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	key := body["key"].(map[string]interface{})
	assert.Equal(t, float64(3), key["key_version"])
}

// ============================================================================
// DistributeKeys Tests
// ============================================================================

func TestDistributeKeys_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeys1")
	user2 := ts.CreateTestUser(t, "distkeys2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user1.ID: "wrapped-key-for-user1",
			user2.ID: "wrapped-key-for-user2",
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["distributed"])
}

func TestDistributeKeys_WithExplicitVersion(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeyver1")
	user2 := ts.CreateTestUser(t, "distkeyver2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	keyVersion := 5
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-v5",
		},
		"key_version": keyVersion,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["distributed"])
}

func TestDistributeKeys_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysnp1")
	user2 := ts.CreateTestUser(t, "distkeysnp2")
	outsider := ts.CreateTestUser(t, "distkeysnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user1.ID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDistributeKeys_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "distkeysinv")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user.ID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDistributeKeys_InvalidRequestBody(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysbad1")
	user2 := ts.CreateTestUser(t, "distkeysbad2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDistributeKeys_DuplicateKeyVersionSkipped(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysdup1")
	user2 := ts.CreateTestUser(t, "distkeysdup2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Distribute once
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-v1",
		},
		"key_version": 1,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Distribute again with same version — ON CONFLICT DO NOTHING
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-v1-again",
		},
		"key_version": 1,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"], "duplicate should not be distributed")
}

func TestDistributeKeys_InvalidMemberUUID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysbaduuid1")
	user2 := ts.CreateTestUser(t, "distkeysbaduuid2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Invalid member UUID should be silently skipped
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			notAUUID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"])
}

// TestDistributeKeys_NewConversationStartsAtVersion1 verifies that the legacy
// DM key distribution endpoint defaults to version 1 for a brand-new
// conversation with no existing keys.
func TestDistributeKeys_NewConversationStartsAtVersion1(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "lgdmnew1")
	user2 := ts.CreateTestUser(t, "lgdmnew2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-newconv",
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var version int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user2.ID,
	).Scan(&version))
	assert.Equal(t, 1, version,
		"first key in a new conversation should be version 1, not MAX+1")
}

// TestDistributeKeys_PeerFulfillmentPreservesVersion is the legacy-endpoint
// regression test for the same bug fixed on the unified endpoint
// (PR #1080 / #1023). When the caller does NOT pass an explicit key_version,
// the server must preserve the EXISTING key_version on peer fulfillment, not
// stamp MAX+1.
func TestDistributeKeys_PeerFulfillmentPreservesVersion(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "lgdmpfa")
	user2 := ts.CreateTestUser(t, "lgdmpfb")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Seed user1 at version 1 (simulates established participant).
	ts.SeedDMKey(t, convID, user1.ID, 1)

	// user1 peer-fulfills the wrap for user2 with no explicit version.
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-peer",
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var version int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user2.ID,
	).Scan(&version))
	assert.Equal(t, 1, version,
		"peer-fulfilled wrap on legacy endpoint must preserve existing key_version (1), not stamp MAX+1 (2)")
}

// TestDistributeKeys_KeyVersionQueryError_FailsClosed verifies that when the
// keyVersion lookup query inside DistributeKeys errors (simulated here by
// renaming dm_channel_keys so the SELECT returns "relation does not exist"),
// the handler responds with HTTP 500 — i.e., fail-CLOSED — instead of silently
// falling through to keyVersion=1 and stamping the wrong key epoch.
//
// This is the behavioral regression guard for the line-1349 errcheck fix.
// Without the fix, a DB error here silently defaults keyVersion to 1, which
// for an established conversation corrupts the E2EE key-epoch tracking
// because peer-fulfillment is supposed to wrap the EXISTING cached CSK at
// MAX, not stamp a wrong version-1 row (see #1023 / PR #1080 / spec D1).
//
// t.Cleanup runs in LIFO order, so the rename-back runs BEFORE the harness's
// TruncateAllTables cleanup — TruncateAllTables sees the table back at its
// original name and runs normally.
func TestDistributeKeys_KeyVersionQueryError_FailsClosed(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeyverr1")
	user2 := ts.CreateTestUser(t, "distkeyverr2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Rename dm_channel_keys so the COALESCE(MAX(key_version), 1) lookup
	// inside DistributeKeys errors with "relation does not exist" — a
	// non-nil error from Scan.
	_, err := ts.DB.Exec("ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_dberrtest")
	require.NoError(t, err, "failed to rename dm_channel_keys for fault injection")
	t.Cleanup(func() {
		// Surface rename-back failures explicitly — silently swallowing them
		// would leave the table renamed for the rest of the package run, and
		// every subsequent test would fail with confusing "relation does not
		// exist" errors that don't point at this cleanup. t.Errorf (not
		// t.Fatalf) marks the test failed but allows other cleanups to run.
		if _, err := ts.DB.Exec("ALTER TABLE dm_channel_keys_dberrtest RENAME TO dm_channel_keys"); err != nil {
			t.Errorf("cleanup: failed to rename dm_channel_keys back to original: %v", err)
		}
	})

	// Request without explicit key_version forces the SELECT MAX path.
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"key_version query error must fail-CLOSED with 500, not silently stamp keyVersion=1")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to distribute keys", body["error"])
}

// TestDistributeKeys_PendingDeleteError_DoesNotFailDistribution verifies the
// log-and-continue contract for the DELETE cleanup inside distributeKeyToMember
// (handlers.go:1391). The cleanup is best-effort — the channel key was already
// inserted into dm_channel_keys before the DELETE runs, so a cleanup failure
// must NOT cause the handler to report distribution as failed.
//
// Setup: seed a pending-key-request row for the recipient. After the request,
// that row should be DELETEd by distributeKeyToMember. If we rename
// dm_pending_key_requests mid-test, the DELETE errors silently (pre-fix) or
// logs a Warn (post-fix). Either way, the observable post-condition is:
//   - HTTP 200 + distributed == 1 (key insert succeeded)
//   - Seeded pending row still exists after rename-back
//   - dm_channel_keys contains the wrapped key (insert happened before
//     DELETE failure path)
//
// This is a COVERAGE test, not a TDD test — it passes both pre- and post-fix.
// Its purpose is to exercise the new error-handling branch added by the fix
// (so it counts toward the 80% new-code Quality Gate) and to lock in the
// log-and-continue contract as a regression guard against future scope-creep
// converting this to fail-CLOSED.
//
// Implicitly covers Site 3 (RowsAffected) on the HAPPY path: the INSERT
// returns RowsAffected=1, the new (rowsAffected, err := ...) line executes
// with err=nil, and the function falls through to the DELETE. Site 3's
// ERROR branch is by-design untested per spec D3 (RowsAffected only errors
// on driver-state corruption, not portably fault-injectable).
func TestDistributeKeys_PendingDeleteError_DoesNotFailDistribution(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeypend1")
	user2 := ts.CreateTestUser(t, "distkeypend2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Seed a pending key request for user2 so the DELETE has a row to target.
	_, err := ts.DB.Exec(
		`INSERT INTO dm_pending_key_requests (conversation_id, user_id) VALUES ($1, $2)`,
		convID, user2.ID,
	)
	require.NoError(t, err, "failed to seed dm_pending_key_requests row")

	// Rename dm_pending_key_requests so the DELETE inside distributeKeyToMember
	// errors with "relation does not exist".
	_, err = ts.DB.Exec("ALTER TABLE dm_pending_key_requests RENAME TO dm_pending_key_requests_dberrtest")
	require.NoError(t, err, "failed to rename dm_pending_key_requests for fault injection")
	t.Cleanup(func() {
		// See TestDistributeKeys_KeyVersionQueryError_FailsClosed cleanup
		// comment: surface rename-back failures rather than silently swallow.
		if _, err := ts.DB.Exec("ALTER TABLE dm_pending_key_requests_dberrtest RENAME TO dm_pending_key_requests"); err != nil {
			t.Errorf("cleanup: failed to rename dm_pending_key_requests back to original: %v", err)
		}
	})

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: wrappedKey,
		},
		"key_version": 1,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code,
		"DELETE-cleanup error must NOT fail the handler — key was already distributed")

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["distributed"],
		"distributed count should reflect successful key insert despite cleanup failure")

	// Verify the key was actually inserted into dm_channel_keys — confirms the
	// "insert happened before DELETE failure" sequencing rather than relying on
	// distributed==1 as a proxy. If the INSERT had silently failed and the
	// DELETE failure was the only path-error, distributed would still be 0,
	// but a future regression could flip those semantics.
	var keyRowCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user2.ID,
	).Scan(&keyRowCount))
	assert.Equal(t, 1, keyRowCount,
		"dm_channel_keys should contain the wrapped key — insert succeeded before DELETE failure path")

	// Verify the seeded pending row STILL exists — proves the DELETE failed
	// silently (pre-fix) or was logged-and-skipped (post-fix). Either way, the
	// log-and-continue contract holds. NOTE: t.Cleanup runs LATER (after this
	// test function returns), so at this assertion point the table is still
	// renamed; we query the renamed name (dm_pending_key_requests_dberrtest).
	var rowCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests_dberrtest WHERE conversation_id = $1 AND user_id = $2`,
		convID, user2.ID,
	).Scan(&rowCount))
	assert.Equal(t, 1, rowCount,
		"seeded pending row should still exist (DELETE failed; cleanup is log-and-continue)")
}

// ============================================================================
// AuthorizeVoiceJoin Tests
// ============================================================================

func TestAuthorizeVoiceJoin_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "voice1")
	user2 := ts.CreateTestUser(t, "voice2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
	assert.NotNil(t, body["ice_servers"])

	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, convID, conv["id"])
}

func TestAuthorizeVoiceJoin_EncryptedConversation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "voiceenc1")
	user2 := ts.CreateTestUser(t, "voiceenc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, convID, conv["id"])
}

func TestAuthorizeVoiceJoin_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "voicenp1")
	user2 := ts.CreateTestUser(t, "voicenp2")
	outsider := ts.CreateTestUser(t, "voicenp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceJoin_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "voiceinv")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/voice/join", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceJoin_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts
	convID := uuid.New().String()

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// GetVoiceParticipants Tests
// ============================================================================

func TestGetVoiceParticipants_EmptyList(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getvp1")
	user2 := ts.CreateTestUser(t, "getvp2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathVoiceParts, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 0)
}

func TestGetVoiceParticipants_WithParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getvpwith1")
	user2 := ts.CreateTestUser(t, "getvpwith2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// Insert a voice participant directly
	_, err := ts.DB.Exec(
		`INSERT INTO dm_voice_participants (conversation_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing) VALUES ($1, $2, false, false, false, false)`,
		convID, user1.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathVoiceParts, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 1)

	p := participants[0].(map[string]interface{})
	assert.Equal(t, user1.ID, p["user_id"])
	assert.NotEmpty(t, p["username"])
}

func TestGetVoiceParticipants_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getvpnp1")
	user2 := ts.CreateTestUser(t, "getvpnp2")
	outsider := ts.CreateTestUser(t, "getvpnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathVoiceParts, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetVoiceParticipants_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getvpinv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+"not-a-uuid/voice/participants", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ============================================================================
// GetOrCreatePersonalThread Tests
// ============================================================================

func TestGetOrCreatePersonalThread_Create(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "personal1")

	w := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
	assert.Equal(t, true, conv["is_personal"])
	assert.Equal(t, false, conv["is_group"])

	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 1, "personal thread should have only the creator")
}

func TestGetOrCreatePersonalThread_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "personal2")

	// First call creates
	w1 := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w1.Code)

	var body1 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	conv1ID := body1["conversation"].(map[string]interface{})["id"].(string)

	// Second call returns existing
	w2 := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w2.Code)

	var body2 map[string]interface{}
	testhelpers.ParseJSON(t, w2, &body2)
	conv2ID := body2["conversation"].(map[string]interface{})["id"].(string)

	assert.Equal(t, conv1ID, conv2ID, "same personal thread should be returned")
}

func TestGetOrCreatePersonalThread_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// UpdateMessage Tests
// ============================================================================

func TestUpdateMessage_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsg1")
	user2 := ts.CreateTestUser(t, "updatemsg2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "original text")

	editedCiphertext := testhelpers.ValidCiphertext()
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": editedCiphertext,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, editedCiphertext, msg["content"])
	assert.NotNil(t, msg["edited_at"], "edited_at should be set")
}

func TestUpdateMessage_NotAuthor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgna1")
	user2 := ts.CreateTestUser(t, "updatemsgna2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "user1's message")

	// user2 tries to edit user1's message
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "hacked",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "own messages")
}

func TestUpdateMessage_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgnp1")
	user2 := ts.CreateTestUser(t, "updatemsgnp2")
	outsider := ts.CreateTestUser(t, "updatemsgnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "message")

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "hacked",
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateMessage_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updatemsginv1")
	msgID := uuid.New().String()

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+"not-a-uuid/messages/"+msgID, map[string]interface{}{
		"content": "edited",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessage_InvalidMessageID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsginv2a")
	user2 := ts.CreateTestUser(t, "updatemsginv2b")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+"/messages/not-a-uuid", map[string]interface{}{
		"content": "edited",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessage_MessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgnotf1")
	user2 := ts.CreateTestUser(t, "updatemsgnotf2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	fakeMsgID := uuid.New().String()

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+fakeMsgID, map[string]interface{}{
		"content": "edited",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateMessage_MissingContent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgnocon1")
	user2 := ts.CreateTestUser(t, "updatemsgnocon2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "original")

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Under E2EE-everywhere (#201) every DM conversation is encrypted by
// construction; the "_EncryptedConversation_" name preserves the test's
// historical intent (validate ciphertext shape) rather than describing
// a runtime branch.
func TestUpdateMessage_EncryptedConversation_ValidCiphertext(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgenc1")
	user2 := ts.CreateTestUser(t, "updatemsgenc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	ciphertext := testhelpers.ValidCiphertext()
	msgID := insertDMMessage(t, ts, convID, user1.ID, ciphertext)

	newCiphertext := testhelpers.ValidCiphertext()
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": newCiphertext,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// See sibling test above for the "_EncryptedConversation_" naming rationale (#201).
func TestUpdateMessage_EncryptedConversation_InvalidCiphertext(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgencinv1")
	user2 := ts.CreateTestUser(t, "updatemsgencinv2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	ciphertext := testhelpers.ValidCiphertext()
	msgID := insertDMMessage(t, ts, convID, user1.ID, ciphertext)

	// Plaintext in encrypted conversation should be rejected
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "plaintext in encrypted conv",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "ciphertext")
}

// ============================================================================
// DeleteMessage Tests
// ============================================================================

func TestDeleteMessage_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsg1")
	user2 := ts.CreateTestUser(t, "delmsg2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "to be deleted")

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["success"])

	// Verify message is actually deleted
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_messages WHERE id = $1`, msgID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "message should be deleted from database")
}

func TestDeleteMessage_NotAuthor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgna1")
	user2 := ts.CreateTestUser(t, "delmsgna2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "user1's msg")

	// user2 tries to delete user1's message
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "own messages")
}

func TestDeleteMessage_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgnp1")
	user2 := ts.CreateTestUser(t, "delmsgnp2")
	outsider := ts.CreateTestUser(t, "delmsgnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "message")

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteMessage_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delmsginv1")
	msgID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+"not-a-uuid/messages/"+msgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteMessage_InvalidMessageID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsginv2a")
	user2 := ts.CreateTestUser(t, "delmsginv2b")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/messages/not-a-uuid", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteMessage_MessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgnotf1")
	user2 := ts.CreateTestUser(t, "delmsgnotf2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	fakeMsgID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+fakeMsgID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteMessage_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgunauth1")
	user2 := ts.CreateTestUser(t, "delmsgunauth2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "msg")

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// DM Attachment Enrichment Tests (#178)
// ============================================================================

func insertDMMediaFile(t *testing.T, ts *testhelpers.TestServer, uploaderID, convID, fileType, mimeType string, fileSize int64) string {
	t.Helper()
	var fileID string
	err := ts.DB.QueryRow(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id, created_at)
		 VALUES (gen_random_uuid(), $1, $2, 2, $3, $4, 'attachments/' || gen_random_uuid(), 1, $5, NOW())
		 RETURNING id`,
		uploaderID, fileType, mimeType, fileSize, convID,
	).Scan(&fileID)
	require.NoError(t, err)
	return fileID
}

func insertDMMessageAttachment(t *testing.T, ts *testhelpers.TestServer, messageID, fileID string, position int) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, $3)`,
		messageID, fileID, position,
	)
	require.NoError(t, err)
}

func TestGetMessagesWithAttachments(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmatt1")
	user2 := ts.CreateTestUser(t, "dmatt2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	msgID := insertDMMessage(t, ts, convID, user1.ID, "see attached")
	fileID := insertDMMediaFile(t, ts, user1.ID, convID, "photo", mimeImagePNG, 12345)
	insertDMMessageAttachment(t, ts, msgID, fileID, 0)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string `json:"id"`
			Attachments []struct {
				ID       string `json:"id"`
				FileType string `json:"file_type"`
				MimeType string `json:"mime_type"`
				FileSize int64  `json:"file_size"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	var found bool
	for _, msg := range body.Messages {
		if msg.ID == msgID {
			found = true
			require.Len(t, msg.Attachments, 1)
			assert.Equal(t, fileID, msg.Attachments[0].ID)
			assert.Equal(t, "photo", msg.Attachments[0].FileType)
			assert.Equal(t, mimeImagePNG, msg.Attachments[0].MimeType)
			assert.Equal(t, int64(12345), msg.Attachments[0].FileSize)
			break
		}
	}
	assert.True(t, found, "DM message with attachment not found")
}

func TestGetMessagesWithMultipleAttachmentsOrderedByPosition(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmmulti1")
	user2 := ts.CreateTestUser(t, "dmmulti2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	msgID := insertDMMessage(t, ts, convID, user1.ID, "multi")
	f1 := insertDMMediaFile(t, ts, user1.ID, convID, "photo", mimeImagePNG, 100)
	f2 := insertDMMediaFile(t, ts, user1.ID, convID, "file", "application/pdf", 200)

	// Insert in reverse order to verify position sorting
	insertDMMessageAttachment(t, ts, msgID, f2, 1)
	insertDMMessageAttachment(t, ts, msgID, f1, 0)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string `json:"id"`
			Attachments []struct {
				ID string `json:"id"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	for _, msg := range body.Messages {
		if msg.ID == msgID {
			require.Len(t, msg.Attachments, 2)
			assert.Equal(t, f1, msg.Attachments[0].ID)
			assert.Equal(t, f2, msg.Attachments[1].ID)
			return
		}
	}
	t.Fatal("DM message not found")
}

func TestGetMessagesDMNoAttachments(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmnoatt1")
	user2 := ts.CreateTestUser(t, "dmnoatt2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	insertDMMessage(t, ts, convID, user1.ID, "plain msg")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			Attachments []struct{ ID string } `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Messages)
	assert.Empty(t, body.Messages[0].Attachments)
}

// ============================================================================
// Helper: createTestGroup creates a group DM via API and returns its ID.
// ============================================================================

const (
	pathMembers      = "/members"
	pathMembersSlash = "/members/"
)

func createTestGroup(t *testing.T, ts *testhelpers.TestServer, creator testhelpers.TestUser, members ...testhelpers.TestUser) string {
	t.Helper()
	userIDs := make([]string, len(members))
	for i, m := range members {
		userIDs[i] = m.ID
	}
	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": userIDs,
		"name":     "Test Group",
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	return conv["id"].(string)
}

// ============================================================================
// Piece 2 Tests: Role Awareness
// ============================================================================

func TestCreateGroupCreatorIsAdmin(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "cadmin1")
	member1 := ts.CreateTestUser(t, "cadmin2")
	member2 := ts.CreateTestUser(t, "cadmin3")

	convID := createTestGroup(t, ts, creator, member1, member2)

	// Fetch the conversation and check roles
	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	participants := conv["participants"].([]interface{})
	require.Len(t, participants, 3)

	for _, p := range participants {
		participant := p.(map[string]interface{})
		if participant["user_id"] == creator.ID {
			assert.Equal(t, "admin", participant["role"], "creator should be admin")
		} else {
			assert.Equal(t, "member", participant["role"], "non-creator should be member")
		}
	}
}

func TestListConversationsIncludesRole(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "listrole1")
	member := ts.CreateTestUser(t, "listrole2")

	createTestGroup(t, ts, creator, member)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.GreaterOrEqual(t, len(conversations), 1)

	// Find the group conversation
	for _, c := range conversations {
		conv := c.(map[string]interface{})
		if conv["is_group"] == true {
			participants := conv["participants"].([]interface{})
			for _, p := range participants {
				participant := p.(map[string]interface{})
				assert.Contains(t, []string{"admin", "member"}, participant["role"],
					"every participant should have a role field")
			}
			return
		}
	}
	t.Fatal("group conversation not found in list")
}

func TestGetConversationIncludesRoleAndCreatedBy(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "getrole1")
	member := ts.CreateTestUser(t, "getrole2")

	convID := createTestGroup(t, ts, creator, member)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})

	assert.Equal(t, creator.ID, conv["created_by"], "created_by should be the creator")

	participants := conv["participants"].([]interface{})
	for _, p := range participants {
		participant := p.(map[string]interface{})
		assert.NotEmpty(t, participant["role"], "role should be present")
	}
}

func TestUpdateConversationAdminOnly(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "updadm1")
	member := ts.CreateTestUser(t, "updadm2")

	convID := createTestGroup(t, ts, admin, member)

	// Admin can rename
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": "Admin Renamed",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateConversationMemberForbidden(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "updmfb1")
	member := ts.CreateTestUser(t, "updmfb2")

	convID := createTestGroup(t, ts, admin, member)

	// Member cannot rename
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": "Member Rename Attempt",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ============================================================================
// Piece 3 Tests: Add/Remove Member
// ============================================================================

func TestAddMemberSuccess(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "addmem1")
	member := ts.CreateTestUser(t, "addmem2")
	newUser := ts.CreateTestUser(t, "addmem3")

	convID := createTestGroup(t, ts, admin, member)
	ts.CreateFriendship(t, admin.ID, newUser.ID, statusAccepted)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers, map[string]interface{}{
		"user_id": newUser.ID,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 3, "should now have 3 participants")
}

func TestAddMemberNotAdmin(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "addna1")
	member := ts.CreateTestUser(t, "addna2")
	newUser := ts.CreateTestUser(t, "addna3")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers, map[string]interface{}{
		"user_id": newUser.ID,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAddMemberMaxParticipants(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "addmax0")

	// Create 9 members (total 10 with admin)
	members := make([]testhelpers.TestUser, 9)
	for i := 0; i < 9; i++ {
		members[i] = ts.CreateTestUser(t, fmt.Sprintf("addmax%d", i+1))
	}

	convID := createTestGroup(t, ts, admin, members...)

	// Try to add an 11th
	extra := ts.CreateTestUser(t, "addmaxextra")
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers, map[string]interface{}{
		"user_id": extra.ID,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "10")
}

func TestAddMemberAlreadyParticipant(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "addalr1")
	member := ts.CreateTestUser(t, "addalr2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers, map[string]interface{}{
		"user_id": member.ID,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestAddMemberInvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "addinv1")

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/not-a-uuid/members", map[string]interface{}{
		"user_id": uuid.New().String(),
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAddMemberNotGroup(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "addng1")
	user2 := ts.CreateTestUser(t, "addng2")
	newUser := ts.CreateTestUser(t, "addng3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers, map[string]interface{}{
		"user_id": newUser.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAddMemberTriggersKeyRevocation(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "addrev1")
	member := ts.CreateTestUser(t, "addrev2")
	newUser := ts.CreateTestUser(t, "addrev3")

	convID := createTestGroup(t, ts, admin, member)
	ts.CreateFriendship(t, admin.ID, newUser.ID, statusAccepted)

	// Seed a key so revocation can be recorded
	ts.SeedDMKey(t, convID, admin.ID, 1)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers, map[string]interface{}{
		"user_id": newUser.ID,
	}, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify a revocation record was created
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1 AND reason = 'member_added'`, convID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "should have one key revocation for member_added")
}

func TestRemoveMemberAdminRemovesOther(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rmadm1")
	member := ts.CreateTestUser(t, "rmadm2")
	target := ts.CreateTestUser(t, "rmadm3")

	convID := createTestGroup(t, ts, admin, member, target)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMembersSlash+target.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify target is removed
	var exists bool
	err := ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, target.ID).Scan(&exists)
	require.NoError(t, err)
	assert.False(t, exists, "removed user should no longer be a participant")
}

func TestRemoveMemberSelfLeave(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rmself1")
	member := ts.CreateTestUser(t, "rmself2")
	leaver := ts.CreateTestUser(t, "rmself3")

	convID := createTestGroup(t, ts, admin, member, leaver)

	// leaver removes themselves
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMembersSlash+leaver.ID, nil, testhelpers.AuthHeaders(leaver.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify leaver is removed
	var exists bool
	err := ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, leaver.ID).Scan(&exists)
	require.NoError(t, err)
	assert.False(t, exists, "self-leave user should be removed")
}

func TestRemoveMemberCannotRemoveCreator(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "rmcr1")
	member := ts.CreateTestUser(t, "rmcr2")

	convID := createTestGroup(t, ts, creator, member)

	// Promote member to admin so they can try removing creator
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID)
	require.NoError(t, err)

	// Member (now admin) tries to remove creator
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMembersSlash+creator.ID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoveMemberCreatorLeavesTransfersAdmin(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "rmxfer1")
	member := ts.CreateTestUser(t, "rmxfer2")
	member2 := ts.CreateTestUser(t, "rmxfer3")

	convID := createTestGroup(t, ts, creator, member, member2)

	// Creator leaves
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMembersSlash+creator.ID, nil, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify creator is gone
	var exists bool
	err := ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, creator.ID).Scan(&exists)
	require.NoError(t, err)
	assert.False(t, exists, "creator should be removed")

	// Verify new created_by is set
	var newCreatedBy string
	err = ts.DB.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, convID).Scan(&newCreatedBy)
	require.NoError(t, err)
	assert.NotEqual(t, creator.ID, newCreatedBy, "created_by should have transferred")

	// Verify the new creator has admin role
	var newRole string
	err = ts.DB.QueryRow(`SELECT role FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, newCreatedBy).Scan(&newRole)
	require.NoError(t, err)
	assert.Equal(t, "admin", newRole, "new creator should be admin")
}

func TestRemoveMemberNotAdminCannotRemoveOther(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rmnona1")
	member := ts.CreateTestUser(t, "rmnona2")
	target := ts.CreateTestUser(t, "rmnona3")

	convID := createTestGroup(t, ts, admin, member, target)

	// member (non-admin) tries to remove target
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMembersSlash+target.ID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ============================================================================
// Piece 4 Tests: Role Management and Group Deletion
// ============================================================================

func TestUpdateMemberRolePromoteToAdmin(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rolpro1")
	member := ts.CreateTestUser(t, "rolpro2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+member.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "admin", body["role"])

	// Verify in DB
	var role string
	err := ts.DB.QueryRow(`SELECT role FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID).Scan(&role)
	require.NoError(t, err)
	assert.Equal(t, "admin", role)
}

func TestUpdateMemberRoleDemoteToMember(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "roldem1")
	promoted := ts.CreateTestUser(t, "roldem2")

	convID := createTestGroup(t, ts, creator, promoted)

	// First promote
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+promoted.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Then demote
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+promoted.ID, map[string]interface{}{
		"role": "member",
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "member", body["role"])
}

func TestUpdateMemberRoleCannotDemoteCreator(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "rolndc1")
	member := ts.CreateTestUser(t, "rolndc2")

	convID := createTestGroup(t, ts, creator, member)

	// Promote member to admin
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+member.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Member (now admin) tries to demote creator
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+creator.ID, map[string]interface{}{
		"role": "member",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateMemberRoleCannotChangeSelf(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rolself1")
	member := ts.CreateTestUser(t, "rolself2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+admin.ID, map[string]interface{}{
		"role": "member",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMemberRoleNotAdmin(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rolna1")
	member := ts.CreateTestUser(t, "rolna2")
	target := ts.CreateTestUser(t, "rolna3")

	convID := createTestGroup(t, ts, admin, member, target)

	// Non-admin tries to change role
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+target.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateMemberRoleInvalidRole(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rolinv1")
	member := ts.CreateTestUser(t, "rolinv2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+member.ID, map[string]interface{}{
		"role": "superadmin",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteGroupSuccess(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "delgrp1")
	member := ts.CreateTestUser(t, "delgrp2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify conversation is deleted
	var exists bool
	err := ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)`, convID).Scan(&exists)
	require.NoError(t, err)
	assert.False(t, exists, "conversation should be deleted")

	// Verify participants are deleted
	var pCount int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_participants WHERE conversation_id = $1`, convID).Scan(&pCount)
	require.NoError(t, err)
	assert.Equal(t, 0, pCount, "participants should be deleted")
}

func TestDeleteGroupNotAdmin(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "delnoa1")
	member := ts.CreateTestUser(t, "delnoa2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Verify conversation still exists
	var exists bool
	err := ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)`, convID).Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "conversation should still exist")
}

func TestDeleteGroupNotGroup(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delng1")
	user2 := ts.CreateTestUser(t, "delng2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMemberRoleInvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rolinvc1")
	targetUser := ts.CreateTestUser(t, "rolinvc2")

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+notAUUID+pathMembersSlash+targetUser.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Invalid conversation ID")
}

func TestUpdateMemberRoleTargetNotParticipant(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "roltnp1")
	member := ts.CreateTestUser(t, "roltnp2")
	outsider := ts.CreateTestUser(t, "roltnp3")

	convID := createTestGroup(t, ts, admin, member)

	// Try to change role of someone who is not in the group
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+outsider.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Target user is not a participant", body["error"])
}

func TestDeleteGroupInvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delinvc1")

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Invalid conversation ID")
}

const errNotParticipant = "Not a participant"

// --- UpdateMemberRole additional error branch coverage ---

func TestUpdateMemberRoleInvalidTargetUserID(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "roltid1")
	member := ts.CreateTestUser(t, "roltid2")

	convID := createTestGroup(t, ts, admin, member)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+notAUUID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Invalid user ID", body["error"])
}

func TestUpdateMemberRoleMissingBody(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rolmb1")
	member := ts.CreateTestUser(t, "rolmb2")

	convID := createTestGroup(t, ts, admin, member)

	// Send nil body — ShouldBindJSON should fail
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+member.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Invalid request body", body["error"])
}

func TestUpdateMemberRoleCallerNotParticipant(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "rolcnp1")
	member := ts.CreateTestUser(t, "rolcnp2")
	outsider := ts.CreateTestUser(t, "rolcnp3")

	convID := createTestGroup(t, ts, admin, member)

	// Outsider (not in the group) tries to change role
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+member.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, errNotParticipant, body["error"])
}

func TestUpdateMemberRoleNotGroupConversation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rolng1")
	user2 := ts.CreateTestUser(t, "rolng2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMembersSlash+user2.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Cannot change roles in non-group conversations", body["error"])
}

// --- DeleteGroup additional error branch coverage ---

func TestDeleteGroupCallerNotParticipant(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "delcnp1")
	member := ts.CreateTestUser(t, "delcnp2")
	outsider := ts.CreateTestUser(t, "delcnp3")

	convID := createTestGroup(t, ts, admin, member)

	// Outsider (not in the group) tries to delete the group
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, errNotParticipant, body["error"])

	// Verify conversation still exists
	var exists bool
	err := ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)`, convID).Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "conversation should still exist")
}

func TestDeleteGroupNonExistentConversation(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delnex1")

	fakeConvID := uuid.New().String()
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+fakeConvID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, errNotParticipant, body["error"])
}

func TestUpdateMemberRoleNonExistentConversation(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rolnex1")
	target := ts.CreateTestUser(t, "rolnex2")

	fakeConvID := uuid.New().String()
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+fakeConvID+pathMembersSlash+target.ID, map[string]interface{}{
		"role": "admin",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, errNotParticipant, body["error"])
}

func TestDeleteGroupVerifiesAllDataDeleted(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "delvad1")
	member := ts.CreateTestUser(t, "delvad2")

	convID := createTestGroup(t, ts, admin, member)

	// Insert a message so we can verify message deletion too
	insertDMMessage(t, ts, convID, admin.ID, "hello group")

	// Insert a read state
	_, err := ts.DB.Exec(
		`INSERT INTO dm_read_states (user_id, conversation_id, last_read_at) VALUES ($1, $2, NOW())`,
		admin.ID, convID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Group deleted", body["message"])

	// Verify messages are deleted
	var msgCount int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_messages WHERE conversation_id = $1`, convID).Scan(&msgCount)
	require.NoError(t, err)
	assert.Equal(t, 0, msgCount, "messages should be deleted")

	// Verify read states are deleted
	var rsCount int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_read_states WHERE conversation_id = $1`, convID).Scan(&rsCount)
	require.NoError(t, err)
	assert.Equal(t, 0, rsCount, "read states should be deleted")

	// Verify participants are deleted
	var pCount int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_participants WHERE conversation_id = $1`, convID).Scan(&pCount)
	require.NoError(t, err)
	assert.Equal(t, 0, pCount, "participants should be deleted")

	// Verify conversation is deleted
	var exists bool
	err = ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)`, convID).Scan(&exists)
	require.NoError(t, err)
	assert.False(t, exists, "conversation should be deleted")
}

// ============================================================================
// DMUserMute Tests (soft-mute: any participant can mute another)
// ============================================================================

func TestDMUserMute(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "usrmute1")
		userB := ts.CreateTestUser(t, "usrmute2")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		// Both users are in voice
		_, err := ts.DB.Exec(
			`INSERT INTO dm_voice_participants (conversation_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing) VALUES ($1, $2, false, false, false, false)`,
			convID, userA.ID,
		)
		require.NoError(t, err)
		_, err = ts.DB.Exec(
			`INSERT INTO dm_voice_participants (conversation_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing) VALUES ($1, $2, false, false, false, false)`,
			convID, userB.ID,
		)
		require.NoError(t, err)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+userB.ID+pathUserMute, nil, testhelpers.AuthHeaders(userA.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, true, body["success"])
	})

	t.Run("NotParticipant", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "usrmutenp1")
		userB := ts.CreateTestUser(t, "usrmutenp2")
		outsider := ts.CreateTestUser(t, "usrmutenp3")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+userA.ID+pathUserMute, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("TargetNotInVoice", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "usrmuteniv1")
		userB := ts.CreateTestUser(t, "usrmuteniv2")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		// Only userA is in voice, userB is NOT
		_, err := ts.DB.Exec(
			`INSERT INTO dm_voice_participants (conversation_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing) VALUES ($1, $2, false, false, false, false)`,
			convID, userA.ID,
		)
		require.NoError(t, err)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+userB.ID+pathUserMute, nil, testhelpers.AuthHeaders(userA.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("CannotMuteSelf", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "usrmuteself1")
		userB := ts.CreateTestUser(t, "usrmuteself2")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+userA.ID+pathUserMute, nil, testhelpers.AuthHeaders(userA.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// ============================================================================
// DMHardMute Tests (server-mute: group admin only)
// ============================================================================

func TestDMHardMute(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardmute1")
		member := ts.CreateTestUser(t, "hardmute2")

		convID := createTestGroup(t, ts, admin, member)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+member.ID+pathMute, nil, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify server_muted flag set in DB
		var serverMuted bool
		err := ts.DB.QueryRow(`SELECT server_muted FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID).Scan(&serverMuted)
		require.NoError(t, err)
		assert.True(t, serverMuted, "server_muted should be true after hard mute")
	})

	t.Run("NotGroupDM", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "hardmutengrp1")
		userB := ts.CreateTestUser(t, "hardmutengrp2")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+userB.ID+pathMute, nil, testhelpers.AuthHeaders(userA.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "Hard enforcement is only available in group DMs", body["error"])
	})

	t.Run("NotAdmin", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardmutena1")
		member := ts.CreateTestUser(t, "hardmutena2")
		other := ts.CreateTestUser(t, "hardmutena3")

		convID := createTestGroup(t, ts, admin, member, other)

		// member (non-admin) tries to mute other
		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+other.ID+pathMute, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("NotParticipant", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardmuteout1")
		member := ts.CreateTestUser(t, "hardmuteout2")
		outsider := ts.CreateTestUser(t, "hardmuteout3")

		convID := createTestGroup(t, ts, admin, member)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+member.ID+pathMute, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ============================================================================
// DMHardUnmute Tests (remove server-mute: group admin only)
// ============================================================================

func TestDMHardUnmute(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardunmute1")
		member := ts.CreateTestUser(t, "hardunmute2")

		convID := createTestGroup(t, ts, admin, member)

		// Pre-set server_muted
		_, err := ts.DB.Exec(`UPDATE dm_participants SET server_muted = true WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID)
		require.NoError(t, err)

		w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathVoiceSlash+member.ID+pathMute, nil, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify flag cleared
		var serverMuted bool
		err = ts.DB.QueryRow(`SELECT server_muted FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID).Scan(&serverMuted)
		require.NoError(t, err)
		assert.False(t, serverMuted, "server_muted should be false after unmute")
	})

	t.Run("CannotUnmuteWhileDeafened", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardunmutedf1")
		member := ts.CreateTestUser(t, "hardunmutedf2")

		convID := createTestGroup(t, ts, admin, member)

		// Pre-set both flags (deafen implies mute)
		_, err := ts.DB.Exec(`UPDATE dm_participants SET server_muted = true, server_deafened = true WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID)
		require.NoError(t, err)

		w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathVoiceSlash+member.ID+pathMute, nil, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusConflict, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Contains(t, body["error"], "Cannot unmute while server-deafened")
	})
}

// ============================================================================
// DMHardDeafen Tests (server-deafen: group admin only, implies mute)
// ============================================================================

func TestDMHardDeafen(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "harddeaf1")
		member := ts.CreateTestUser(t, "harddeaf2")

		convID := createTestGroup(t, ts, admin, member)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+member.ID+pathDeafen, nil, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Deafen implies mute — both flags should be true
		var serverMuted, serverDeafened bool
		err := ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID).Scan(&serverMuted, &serverDeafened)
		require.NoError(t, err)
		assert.True(t, serverMuted, "server_muted should be true after deafen")
		assert.True(t, serverDeafened, "server_deafened should be true after deafen")
	})

	t.Run("NotGroupDM", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "harddeafngrp1")
		userB := ts.CreateTestUser(t, "harddeafngrp2")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+userB.ID+pathDeafen, nil, testhelpers.AuthHeaders(userA.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotAdmin", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "harddeafna1")
		member := ts.CreateTestUser(t, "harddeafna2")

		convID := createTestGroup(t, ts, admin, member)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+admin.ID+pathDeafen, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ============================================================================
// DMHardUndeafen Tests (remove server-deafen: clears both flags)
// ============================================================================

func TestDMHardUndeafen(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardundeaf1")
		member := ts.CreateTestUser(t, "hardundeaf2")

		convID := createTestGroup(t, ts, admin, member)

		// Pre-set both flags
		_, err := ts.DB.Exec(`UPDATE dm_participants SET server_muted = true, server_deafened = true WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID)
		require.NoError(t, err)

		w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathVoiceSlash+member.ID+pathDeafen, nil, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Both flags should be cleared
		var serverMuted, serverDeafened bool
		err = ts.DB.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID).Scan(&serverMuted, &serverDeafened)
		require.NoError(t, err)
		assert.False(t, serverMuted, "server_muted should be false after undeafen")
		assert.False(t, serverDeafened, "server_deafened should be false after undeafen")
	})

	t.Run("NotGroupDM", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "hardundeafngrp1")
		userB := ts.CreateTestUser(t, "hardundeafngrp2")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathVoiceSlash+userB.ID+pathDeafen, nil, testhelpers.AuthHeaders(userA.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotAdmin", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "hardundeafna1")
		member := ts.CreateTestUser(t, "hardundeafna2")

		convID := createTestGroup(t, ts, admin, member)

		w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathVoiceSlash+admin.ID+pathDeafen, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ============================================================================
// AuthorizeVoiceJoin Enforcement Tests (extended response fields)
// ============================================================================

func TestAuthorizeVoiceJoinEnforcement(t *testing.T) {
	t.Run("IncludesEnforcementFlags", func(t *testing.T) {
		ts := setupTS(t)
		admin := ts.CreateTestUser(t, "vjoinenf1")
		member := ts.CreateTestUser(t, "vjoinenf2")

		convID := createTestGroup(t, ts, admin, member)

		// Pre-set server_muted on the joining member
		_, err := ts.DB.Exec(`UPDATE dm_participants SET server_muted = true WHERE conversation_id = $1 AND user_id = $2`, convID, member.ID)
		require.NoError(t, err)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(member.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, true, body["allowed"])
		assert.Equal(t, true, body["server_muted"])
		assert.Equal(t, false, body["server_deafened"])

		conv := body["conversation"].(map[string]interface{})
		assert.Equal(t, true, conv["is_group"])
		assert.Equal(t, "member", conv["caller_role"])
	})

	t.Run("OneOnOneDefaults", func(t *testing.T) {
		ts := setupTS(t)
		userA := ts.CreateTestUser(t, "vjoinenf1on1a")
		userB := ts.CreateTestUser(t, "vjoinenf1on1b")
		ts.CreateFriendship(t, userA.ID, userB.ID, statusAccepted)
		convID := ts.CreateDMConversation(t, userA.ID, userB.ID)

		w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(userA.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, false, body["server_muted"])
		assert.Equal(t, false, body["server_deafened"])

		conv := body["conversation"].(map[string]interface{})
		assert.Equal(t, false, conv["is_group"])
	})
}

// --- Rate Limit Tests ---

const fmtRateLimitDMKey = "ratelimit:dm_rotate:%s"
const fmtUserRLKeyDMRotate = "ratelimit:user:%s:POST:/api/v1/dm/conversations/:id/rotate-key"

func TestDMRotateKeyRateLimitBlocks11th(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrl-user1")
	user2 := ts.CreateTestUser(t, "dmrl-user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	headers := testhelpers.AuthHeaders(user1.AccessToken)
	endpoint := pathDMConversationsPrefix + convID + pathRotateKey

	// Clear any pre-existing rate limit counter for this conversation
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitDMKey, convID))

	// First 10 calls should succeed.
	for i := 1; i <= 10; i++ {
		// Clear per-user middleware key every 4 requests (middleware allows 5/min)
		if i%4 == 0 {
			userRLKey := fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID)
			ts.Redis.Del(context.Background(), userRLKey)
		}
		w := ts.DoRequest("POST", endpoint, nil, headers)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should succeed", i)
	}

	// Clear per-user middleware key before 11th request
	userRLKey := fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID)
	ts.Redis.Del(context.Background(), userRLKey)

	// 11th should be rate limited by per-conversation limit
	w := ts.DoRequest("POST", endpoint, nil, headers)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Rate limit exceeded", body["error"])
	assert.Contains(t, body["message"], "Try again in")
	assert.NotNil(t, body["retry_after"])
}

func TestDMRotateKeyRateLimitIndependentConversations(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrl-indep1")
	user2 := ts.CreateTestUser(t, "dmrl-indep2")
	user3 := ts.CreateTestUser(t, "dmrl-indep3")

	convA := ts.CreateDMConversation(t, user1.ID, user2.ID)
	convB := ts.CreateDMConversation(t, user1.ID, user3.ID)

	headers := testhelpers.AuthHeaders(user1.AccessToken)

	// Clear any pre-existing rate limit counters
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitDMKey, convA))
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitDMKey, convB))

	// Exhaust conv A's limit
	for i := 1; i <= 10; i++ {
		if i%4 == 0 {
			userRLKey := fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID)
			ts.Redis.Del(context.Background(), userRLKey)
		}
		w := ts.DoRequest("POST", pathDMConversationsPrefix+convA+pathRotateKey, nil, headers)
		assert.Equal(t, http.StatusOK, w.Code, "conv A request %d should succeed", i)
	}

	// Clear per-user middleware key
	userRLKey := fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID)
	ts.Redis.Del(context.Background(), userRLKey)

	// Conv A should now be rate limited
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convA+pathRotateKey, nil, headers)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Rate limit exceeded", body["error"])
	assert.Contains(t, body["message"], "Try again in")
	assert.NotNil(t, body["retry_after"])

	// Clear per-user middleware key again
	ts.Redis.Del(context.Background(), userRLKey)

	// Conv B should still work
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convB+pathRotateKey, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code, "conv B should still work")
}

// ─── RingDMCall tests (#1209, plan Task B3) ──────────────────────────────

const pathVoiceRing = "/voice/ring"

func TestRingDMCall_Success(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_caller")
	callee := ts.CreateTestUser(t, "ring_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotEmpty(t, body["ring_id"], "ring_id present")
	assert.NotEmpty(t, body["ring_started_at"], "ring_started_at present")

	calleeIDs, ok := body["ringing_user_ids"].([]interface{})
	require.True(t, ok, "ringing_user_ids is an array")
	require.Len(t, calleeIDs, 1, "DM 1:1 has exactly one callee")
	assert.Equal(t, callee.ID, calleeIDs[0])

	// pendingDMCalls populated
	convUUID := uuid.MustParse(convID)
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "pendingDMCalls populated after Ring")
}

func TestRingDMCall_NonParticipant_Returns403(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	owner := ts.CreateTestUser(t, "ring_owner")
	other := ts.CreateTestUser(t, "ring_other")
	outsider := ts.CreateTestUser(t, "ring_outsider")
	convID := ts.CreateDMConversation(t, owner.ID, other.ID) // outsider is NOT in this conv

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "non-participant gets 403; body: %s", w.Body.String())

	convUUID := uuid.MustParse(convID)
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "no ring created on failed auth")
}

func TestRingDMCall_AlreadyRinging_Returns409(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_caller_2")
	callee := ts.CreateTestUser(t, "ring_callee_2")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	headers := testhelpers.AuthHeaders(caller.AccessToken)

	// First ring succeeds
	w1 := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, headers)
	require.Equal(t, http.StatusOK, w1.Code)

	// Reset per-user rate-limit so the second call isnt blocked by middleware
	ts.Redis.Del(context.Background(), fmt.Sprintf("ratelimit:user:%s", caller.ID))

	// Second ring on same conv returns 409 with existing_ring metadata
	w2 := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, headers)
	require.Equal(t, http.StatusConflict, w2.Code, "body: %s", w2.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &body))
	assert.Equal(t, "already ringing", body["error"])

	existing, ok := body["existing_ring"].(map[string]interface{})
	require.True(t, ok, "existing_ring object present")
	assert.Equal(t, caller.ID, existing["caller_user_id"])
	assert.NotEmpty(t, existing["ring_id"])
	assert.NotEmpty(t, existing["ring_started_at"])
}

func TestRingDMCall_InvalidConvID_Returns400(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "ring_caller_3")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid"+pathVoiceRing, nil, testhelpers.AuthHeaders(caller.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRingDMCall_Group_ExcludesOfflineCallees(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "rg-caller")
	online := ts.CreateTestUser(t, "rg-online")
	offline := ts.CreateTestUser(t, "rg-offline")
	convID := ts.CreateGroupDMConversation(t, caller.ID, online.ID, offline.ID)
	ts.Hub.MarkUserOnlineForTest(uuid.MustParse(caller.ID))
	ts.Hub.MarkUserOnlineForTest(uuid.MustParse(online.ID)) // offline is NOT marked

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		RingingUserIDs []string `json:"ringing_user_ids"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.RingingUserIDs, online.ID)
	assert.NotContains(t, body.RingingUserIDs, offline.ID)
}

func TestRingDMCall_Group_AllOffline_Returns400(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "rg-caller-alloff")
	b := ts.CreateTestUser(t, "rg-b-alloff")
	cc := ts.CreateTestUser(t, "rg-c-alloff")
	convID := ts.CreateGroupDMConversation(t, caller.ID, b.ID, cc.ID)
	ts.Hub.MarkUserOnlineForTest(uuid.MustParse(caller.ID)) // only caller online; B and C offline

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body["error"], "no online members")

	convUUID := uuid.MustParse(convID)
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "no ring created when all callees offline")
}

// ─── DeclineDMCall tests (#1209, plan Task B4) ───────────────────────────

const pathVoiceDecline = "/voice/decline"

// ringForTest is a small test helper that POSTs /voice/ring as the given
// user and asserts 200. Returns the convID for chaining into subsequent
// decline/cancel requests.
func ringForTest(t *testing.T, ts *testhelpers.TestServer, caller testhelpers.TestUser, convID string) {
	t.Helper()
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceRing, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "ring setup failed; body: %s", w.Body.String())
}

func TestDeclineDMCall_DM1on1_EndsCallAndClearsRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "decl_caller")
	callee := ts.CreateTestUser(t, "decl_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(callee.AccessToken))
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	// DM 1:1: sole-callee decline = all-declined → ring should be cleared
	convUUID := uuid.MustParse(convID)
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared after sole-callee decline")
}

func TestDeclineDMCall_Group_RingPersistsUntilLastDecline(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "gd-caller")
	b := ts.CreateTestUser(t, "gd-b")
	cc := ts.CreateTestUser(t, "gd-c")
	convID := ts.CreateGroupDMConversation(t, caller.ID, b.ID, cc.ID)
	for _, u := range []string{caller.ID, b.ID, cc.ID} {
		ts.Hub.MarkUserOnlineForTest(uuid.MustParse(u))
	}

	ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)

	// B declines → ring still exists (C still ringing).
	w1 := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(b.AccessToken))
	require.Equal(t, http.StatusNoContent, w1.Code, "body: %s", w1.Body.String())
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring persists after first group decline")

	// C declines → all callees declined → ring cleared.
	w2 := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(cc.AccessToken))
	require.Equal(t, http.StatusNoContent, w2.Code, "body: %s", w2.Body.String())
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared after all callees decline")
}

func TestJoinDMCall_Group_AcceptClearsRingForRemaining(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "gj-caller")
	b := ts.CreateTestUser(t, "gj-b")
	cc := ts.CreateTestUser(t, "gj-c")
	convID := ts.CreateGroupDMConversation(t, caller.ID, b.ID, cc.ID)
	for _, u := range []string{caller.ID, b.ID, cc.ID} {
		ts.Hub.MarkUserOnlineForTest(uuid.MustParse(u))
	}

	ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)
	require.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring exists before accept")

	// B (a callee, not the caller) accepts via /voice/join → the accept-path
	// branch clears the ring for ALL remaining callees (C), N-generically.
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(b.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "callee accept succeeds; body: %s", w.Body.String())

	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared for remaining callees after one accept")
}

func TestDeclineDMCall_NoActiveRing_Returns404(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	owner := ts.CreateTestUser(t, "decl_owner_404")
	other := ts.CreateTestUser(t, "decl_other_404")
	convID := ts.CreateDMConversation(t, owner.ID, other.ID)

	// No ring started — decline should 404
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(other.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

func TestDeclineDMCall_CallerCannotDeclineOwnRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "decl_caller_self")
	callee := ts.CreateTestUser(t, "decl_callee_self")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	// Caller tries to decline their own ring → 403 (not in ringing set)
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(caller.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())

	// Ring still active
	convUUID := uuid.MustParse(convID)
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring should still be active after invalid decline")
}

func TestDeclineDMCall_NonParticipant_Returns403(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "decl_caller_np")
	callee := ts.CreateTestUser(t, "decl_callee_np")
	outsider := ts.CreateTestUser(t, "decl_outsider")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	// Outsider (not in conv) tries to decline → 403
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())

	convUUID := uuid.MustParse(convID)
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring untouched by outsider decline attempt")
}

// ─── CancelDMCall + insertCallEvent tests (#1209, plan Task B5) ──────────

const pathVoiceCancel = "/voice/cancel"

// fetchLatestCallEvent returns the most-recent call_event row for a
// conversation. Parses the JSONB payload into a generic map so tests
// can assert on individual fields regardless of Postgres' JSONB
// pretty-printing whitespace.
func fetchLatestCallEvent(t *testing.T, ts *testhelpers.TestServer, convID string) (dbType string, payload map[string]interface{}, found bool) {
	t.Helper()
	var payloadJSON string
	err := ts.DB.QueryRow(`
		SELECT type, COALESCE(call_event_payload::text, '')
		FROM dm_messages
		WHERE conversation_id = $1 AND type = 'call_event'
		ORDER BY created_at DESC LIMIT 1
	`, convID).Scan(&dbType, &payloadJSON)
	if err != nil {
		return "", nil, false
	}
	payload = make(map[string]interface{})
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("failed to unmarshal call_event_payload JSON: %v\nraw: %s", err, payloadJSON)
	}
	return dbType, payload, true
}

func TestCancelDMCall_Success(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "canc_caller")
	callee := ts.CreateTestUser(t, "canc_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	// Reset rate-limit so the cancel call isnt blocked
	ts.Redis.Del(context.Background(), fmt.Sprintf("ratelimit:user:%s", caller.ID))

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceCancel, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	// Ring cleared
	convUUID := uuid.MustParse(convID)
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared after caller-cancel")

	// Call event row written with status=canceled (verifies insertCallEvent
	// helper integration end-to-end)
	dbType, payload, found := fetchLatestCallEvent(t, ts, convID)
	require.True(t, found, "call_event row inserted")
	assert.Equal(t, "call_event", dbType)
	assert.Equal(t, "canceled", payload["status"])
	assert.Equal(t, caller.ID, payload["caller_user_id"])
}

func TestCancelDMCall_NotCaller_Returns403(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "canc_caller_2")
	callee := ts.CreateTestUser(t, "canc_callee_2")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	// Callee tries to cancel (should /decline instead) → 403
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceCancel, nil, testhelpers.AuthHeaders(callee.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())

	// Ring still active
	convUUID := uuid.MustParse(convID)
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring untouched by non-caller cancel")
}

func TestCancelDMCall_NoRing_Returns404(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "canc_caller_3")
	callee := ts.CreateTestUser(t, "canc_callee_3")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	// No ring → 404
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceCancel, nil, testhelpers.AuthHeaders(caller.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// ─── AuthorizeDMVoiceForMediaPlane tests (#1209, plan Task B6 / G7) ──────

const pathVoiceAuthorize = "/voice/authorize"

func TestAuthorizeDMVoiceForMediaPlane_Member_Returns200(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	user := ts.CreateTestUser(t, "g7_member")
	other := ts.CreateTestUser(t, "g7_other")
	convID := ts.CreateDMConversation(t, user.ID, other.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceAuthorize, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, true, body["authorized"])
	assert.Equal(t, false, body["is_group"], "DM 1:1 = not group")
}

func TestAuthorizeDMVoiceForMediaPlane_NonMember_Returns403(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	owner := ts.CreateTestUser(t, "g7_owner")
	other := ts.CreateTestUser(t, "g7_other_2")
	outsider := ts.CreateTestUser(t, "g7_outsider")
	convID := ts.CreateDMConversation(t, owner.ID, other.ID) // outsider NOT in conv

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceAuthorize, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, false, body["authorized"])
}

func TestAuthorizeDMVoiceForMediaPlane_InvalidConvID_Returns400(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	user := ts.CreateTestUser(t, "g7_user_400")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid"+pathVoiceAuthorize, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─── HandleUserDisconnect tests (#1209, plan Task B7 Part 2) ─────────────

func TestHandleUserDisconnect_CancelsCallerInitiatedRings(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "disc_caller")
	callee := ts.CreateTestUser(t, "disc_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	// Simulate the caller's WS disconnect via the canceler that the hub
	// would invoke. The dmHandler is constructed during SetupTestServer
	// router init; we test the HandleUserDisconnect path directly with
	// a freshly-constructed Handler (the test ts.Hub + ts.DB are the
	// authoritative path that production wires).
	h := dm.NewHandler(ts.DB, logger.New("test"), ts.Hub, nil, nil, ts.Redis)

	callerUUID := uuid.MustParse(caller.ID)
	h.HandleUserDisconnect(callerUUID)

	// Ring cleared
	convUUID := uuid.MustParse(convID)
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared after caller disconnect")

	// Canceled call_event inserted
	dbType, payload, found := fetchLatestCallEvent(t, ts, convID)
	require.True(t, found, "call_event row inserted on caller disconnect")
	assert.Equal(t, "call_event", dbType)
	assert.Equal(t, "canceled", payload["status"])
}

func TestHandleUserDisconnect_IgnoresNonCallerDisconnects(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "disc_caller_2")
	callee := ts.CreateTestUser(t, "disc_callee_2")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	h := dm.NewHandler(ts.DB, logger.New("test"), ts.Hub, nil, nil, ts.Redis)

	// Simulate the CALLEE disconnecting — should NOT cancel the ring
	calleeUUID := uuid.MustParse(callee.ID)
	h.HandleUserDisconnect(calleeUUID)

	convUUID := uuid.MustParse(convID)
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring untouched by non-caller disconnect")
}

// ─── InsertCompletedCallEventForDMRoom tests (#1209, plan Task B7 Part 1) ─

func TestInsertCompletedCallEventForDMRoom_HappyPath(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	caller := ts.CreateTestUser(t, "comp_caller")
	callee := ts.CreateTestUser(t, "comp_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	// Simulate a live call: two dm_voice_participants rows with caller
	// joining first (used as the "caller" in the call_event payload).
	_, err := ts.DB.Exec(`
		INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
		VALUES ($1, $2, NOW() - INTERVAL '30 seconds'), ($1, $3, NOW() - INTERVAL '25 seconds')
	`, convID, caller.ID, callee.ID)
	require.NoError(t, err)

	convUUID := uuid.MustParse(convID)
	err = dm.InsertCompletedCallEventForDMRoom(context.Background(), ts.DB, convUUID)
	require.NoError(t, err)

	dbType, payload, found := fetchLatestCallEvent(t, ts, convID)
	require.True(t, found, "call_event row inserted on room empty")
	assert.Equal(t, "call_event", dbType)
	assert.Equal(t, "completed", payload["status"])
	assert.Equal(t, caller.ID, payload["caller_user_id"], "earliest-joined user is the 'caller'")

	// Duration should be ~30 seconds (allow a few seconds of test slack)
	durF, ok := payload["duration_seconds"].(float64)
	require.True(t, ok, "duration_seconds is a number; got %T", payload["duration_seconds"])
	assert.InDelta(t, 30.0, durF, 10.0, "duration ~ 30s (test slack ±10s)")

	participants, ok := payload["participant_user_ids"].([]interface{})
	require.True(t, ok)
	assert.Len(t, participants, 2)
}

func TestInsertCompletedCallEventForDMRoom_NoParticipants_NoOp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	caller := ts.CreateTestUser(t, "comp_caller_none")
	callee := ts.CreateTestUser(t, "comp_callee_none")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	// No dm_voice_participants rows — graceful no-op
	convUUID := uuid.MustParse(convID)
	err := dm.InsertCompletedCallEventForDMRoom(context.Background(), ts.DB, convUUID)
	assert.NoError(t, err, "no participants is a graceful no-op, not an error")

	_, _, found := fetchLatestCallEvent(t, ts, convID)
	assert.False(t, found, "no call_event row inserted when no participants")
}

func TestDeclineDMCall_InsertsDeclinedCallEvent(t *testing.T) {
	// Verifies the all-declined branch of DeclineDMCall now wires through
	// to insertCallEvent (replaces the TODO from plan task B4 with the
	// B5 helper).
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "decl_ce_caller")
	callee := ts.CreateTestUser(t, "decl_ce_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceDecline, nil, testhelpers.AuthHeaders(callee.AccessToken))
	require.Equal(t, http.StatusNoContent, w.Code)

	// DM 1:1: sole-callee decline triggers all-declined path → call_event inserted
	dbType, payload, found := fetchLatestCallEvent(t, ts, convID)
	require.True(t, found, "call_event row inserted on all-declined")
	assert.Equal(t, "call_event", dbType)
	assert.Equal(t, "declined", payload["status"])

	participants, ok := payload["participant_user_ids"].([]interface{})
	require.True(t, ok, "participant_user_ids is an array")
	require.NotEmpty(t, participants)
	// Should contain the decliner (callee)
	foundDecliner := false
	for _, p := range participants {
		if p == callee.ID {
			foundDecliner = true
			break
		}
	}
	assert.True(t, foundDecliner, "participant list includes the decliner; got: %v", participants)
}

// TestAuthorizeVoiceJoin_CalleeAccept_ClearsRing exercises the accept-path
// branch added in #1231 review (Gitar G1): when a callee POSTs /voice/join
// and a PendingCall exists for the conversation, the handler atomically
// clears it and (in production) broadcasts dm_voice_call_canceled with
// canceled_by='someone_accepted'. We assert the ring-clear side effect
// since the broadcast goes through the in-memory hub.
func TestAuthorizeVoiceJoin_CalleeAccept_ClearsRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "auth_accept_caller")
	callee := ts.CreateTestUser(t, "auth_accept_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)
	require.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring exists before accept")

	// Callee POSTs /voice/join — the accept-path branch should clear the ring.
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/voice/join", nil, testhelpers.AuthHeaders(callee.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "callee auth succeeds; body: %s", w.Body.String())

	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared after callee accept")
}

// TestAuthorizeVoiceJoin_CallerSelf_DoesNotClearRing verifies the
// caller-self defense: if the original caller (ring.CallerUserID) hits
// /voice/join, the accept-path branch must NOT fire (caller is allowed
// to authorize their own ring without claiming "I accepted").
func TestAuthorizeVoiceJoin_CallerSelf_DoesNotClearRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "auth_selfauth_caller")
	callee := ts.CreateTestUser(t, "auth_selfauth_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)
	require.True(t, dm.PendingDMCallExistsForTest(convUUID))

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/voice/join", nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "ring NOT cleared when caller self-auths")
}

// TestOnRingTimeout_BroadcastsAndInsertsMissedEvent invokes onRingTimeout
// directly via the test export (no real timer wait). Covers the
// broadcast + insertCallEvent path that was previously 0% covered.
func TestOnRingTimeout_BroadcastsAndInsertsMissedEvent(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "timeout_caller")
	callee := ts.CreateTestUser(t, "timeout_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	convUUID := uuid.MustParse(convID)
	callerUUID := uuid.MustParse(caller.ID)
	calleeUUID := uuid.MustParse(callee.ID)
	ring := dm.NewPendingCallForTest(convUUID, callerUUID, []uuid.UUID{calleeUUID})
	dm.StoreRingForTest(convUUID, ring)

	h := dm.NewHandler(ts.DB, logger.New("test"), ts.Hub, nil, nil, ts.Redis)
	dm.HandlerOnRingTimeoutForTest(h, convUUID, ring)

	assert.False(t, dm.PendingDMCallExistsForTest(convUUID), "ring cleared after timeout")

	dbType, payload, found := fetchLatestCallEvent(t, ts, convID)
	require.True(t, found, "missed call_event row inserted on timeout")
	assert.Equal(t, "call_event", dbType)
	assert.Equal(t, "missed", payload["status"])
}

// TestOnRingTimeout_OrphanedTimer_NoOp verifies the defensive branch
// when a different ring is now in pendingDMCalls (the timer fired for
// an old ring after a new ring was inserted). The handler must NOT
// broadcast and must NOT delete the newer ring.
func TestOnRingTimeout_OrphanedTimer_NoOp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)

	caller := ts.CreateTestUser(t, "orphan_caller")
	callee := ts.CreateTestUser(t, "orphan_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	convUUID := uuid.MustParse(convID)
	callerUUID := uuid.MustParse(caller.ID)
	calleeUUID := uuid.MustParse(callee.ID)

	// First ring (stale): won't be in the map at timeout time.
	staleRing := dm.NewPendingCallForTest(convUUID, callerUUID, []uuid.UUID{calleeUUID})
	// Active ring: currently in the map.
	activeRing := dm.NewPendingCallForTest(convUUID, callerUUID, []uuid.UUID{calleeUUID})
	dm.StoreRingForTest(convUUID, activeRing)

	h := dm.NewHandler(ts.DB, logger.New("test"), ts.Hub, nil, nil, ts.Redis)
	dm.HandlerOnRingTimeoutForTest(h, convUUID, staleRing)

	// Active ring still present (orphaned timer should not have deleted it).
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "active ring preserved when orphaned timer fires")
}
