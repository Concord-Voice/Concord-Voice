package dm_test

// Integration tests for the DM/group bulk-purge endpoint (#1352): participant
// authorization, step-up auth (fail-closed default, M7), delete-own +
// persistent receiver-hide (M3 serve filters), and group-admin delete-all.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func purgeConvPath(convID string) string {
	return "/api/v1/dm/conversations/" + convID + "/messages"
}

func insertDMMsg(t *testing.T, ts *testhelpers.TestServer, convID, userID, content string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, type)
		 VALUES (gen_random_uuid(), $1, $2, $3, 'text')`, convID, userID, content)
	require.NoError(t, err)
}

func countDMMessages(t *testing.T, ts *testhelpers.TestServer, convID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, convID).Scan(&n))
	return n
}

// fetchVisibleMessages returns the message contents the given user sees via
// GET /dm/conversations/:id/messages (the hidden-range serve filter applies).
func fetchVisibleMessages(t *testing.T, ts *testhelpers.TestServer, convID, token string) []string {
	t.Helper()
	w := ts.DoRequest(http.MethodGet, purgeConvPath(convID), nil, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	out := make([]string, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		out = append(out, m.Content)
	}
	return out
}

// TestPurgeConversation_DeleteOwnHideOther is the core 1:1 semantic (spec §5):
// the actor's own messages are deleted for both parties; the other party's
// messages are persistently hidden from the actor only. The default (absent)
// privacy_settings row fail-closes to step-up-required (M7), so the request
// carries the actor's password.
func TestPurgeConversation_DeleteOwnHideOther(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "purge_alice")
	bob := ts.CreateTestUser(t, "purge_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	insertDMMsg(t, ts, convID, alice.ID, "alice-1")
	insertDMMsg(t, ts, convID, alice.ID, "alice-2")
	insertDMMsg(t, ts, convID, bob.ID, "bob-1")

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": alice.Password},
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		DeletedCount int `json:"deleted_count"`
		HiddenCount  int `json:"hidden_count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.DeletedCount, "alice's own messages deleted for both")
	assert.Equal(t, 1, resp.HiddenCount, "bob's message hidden for alice")

	// Bob's message row survives (hide is view-local, not a delete).
	assert.Equal(t, 1, countDMMessages(t, ts, convID))

	// Alice no longer sees bob's message; bob still does.
	assert.Empty(t, fetchVisibleMessages(t, ts, convID, alice.AccessToken),
		"actor's view must exclude the hidden message after refetch")
	bobSees := fetchVisibleMessages(t, ts, convID, bob.AccessToken)
	require.Len(t, bobSees, 1)
	assert.Equal(t, "bob-1", bobSees[0], "peer's view is unaffected by the actor's hide")

	// Audit row records both counts, no content.
	var deleted, hidden int
	var ctxType string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT context_type, deleted_count, hidden_count FROM message_purges WHERE context_id = $1`,
		convID).Scan(&ctxType, &deleted, &hidden))
	assert.Equal(t, "dm", ctxType)
	assert.Equal(t, 2, deleted)
	assert.Equal(t, 1, hidden)

	// The hidden message must not resurface via the conversation-list preview (M3).
	w = ts.DoRequest(http.MethodGet, "/api/v1/dm/conversations", nil,
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, strings.Contains(w.Body.String(), "bob-1"),
		"hidden content leaked into the actor's conversation-list preview")
}

// TestPurgeConversation_StepUpWrongPassword403 locks the step-up gate: a wrong
// password mutates nothing and writes no audit row.
func TestPurgeConversation_StepUpWrongPassword403(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "su_alice")
	bob := ts.CreateTestUser(t, "su_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "keep-me")

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": "wrong-password-123"},
		testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 1, countDMMessages(t, ts, convID))

	var audits int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM message_purges WHERE context_id = $1`, convID).Scan(&audits))
	assert.Equal(t, 0, audits, "failed step-up must write no audit row")
}

// TestPurgeConversation_MissingPasswordFailClosed locks M7: with NO
// privacy_settings row (the common lazily-created case), step-up is REQUIRED —
// a purge without current_password is rejected.
func TestPurgeConversation_MissingPasswordFailClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "fc_alice")
	bob := ts.CreateTestUser(t, "fc_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, bob.ID, "still-here")

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 1, countDMMessages(t, ts, convID))
}

// TestPurgeConversation_RequireAuthOffSkipsStepUp: the user's explicit opt-out
// (require_auth_before_purge=false) allows a passwordless purge.
func TestPurgeConversation_RequireAuthOffSkipsStepUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "off_alice")
	bob := ts.CreateTestUser(t, "off_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "mine")

	_, err := ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE)
		ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, alice.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 0, countDMMessages(t, ts, convID))
}

// TestPurgeConversation_GroupAdminDeletesAll: a group admin deletes everyone's
// messages for both; a non-admin member only deletes their own (others hidden).
func TestPurgeConversation_GroupAdminDeletesAll(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	admin := ts.CreateTestUser(t, "grp_admin")
	m1 := ts.CreateTestUser(t, "grp_m1")
	m2 := ts.CreateTestUser(t, "grp_m2")
	convID := ts.CreateGroupDMConversation(t, admin.ID, m1.ID, m2.ID)
	_, err := ts.DB.Exec(
		`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`,
		convID, admin.ID)
	require.NoError(t, err)

	insertDMMsg(t, ts, convID, admin.ID, "admin-msg")
	insertDMMsg(t, ts, convID, m1.ID, "m1-msg")
	insertDMMsg(t, ts, convID, m2.ID, "m2-msg")

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": admin.Password},
		testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		DeletedCount int `json:"deleted_count"`
		HiddenCount  int `json:"hidden_count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.DeletedCount, "group admin deletes ALL messages for both")
	assert.Equal(t, 0, resp.HiddenCount)
	assert.Equal(t, 0, countDMMessages(t, ts, convID))

	var ctxType string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT context_type FROM message_purges WHERE context_id = $1`, convID).Scan(&ctxType))
	assert.Equal(t, "group", ctxType)
}

func TestPurgeConversation_RejectsInvalidInput(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "ivd_alice")
	bob := ts.CreateTestUser(t, "ivd_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	hdrs := testhelpers.AuthHeaders(alice.AccessToken)

	t.Run("non-uuid conversation id", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, "/api/v1/dm/conversations/not-a-uuid/messages",
			map[string]any{"range": "all", "current_password": alice.Password}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("missing range", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
			map[string]any{"current_password": alice.Password}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("unknown range value", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
			map[string]any{"range": "forever", "current_password": alice.Password}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// TestPurgeConversation_TimeRangeScopesHide locks that a ranged purge hides only
// the peer messages inside the window — older peer messages stay visible.
func TestPurgeConversation_TimeRangeScopesHide(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "tr_alice")
	bob := ts.CreateTestUser(t, "tr_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	insertDMMsg(t, ts, convID, bob.ID, "bob-recent")
	insertDMMsg(t, ts, convID, bob.ID, "bob-ancient")
	_, err := ts.DB.Exec(
		`UPDATE dm_messages SET created_at = NOW() - INTERVAL '30 days'
		 WHERE conversation_id = $1 AND content = 'bob-ancient'`, convID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "1d", "current_password": alice.Password},
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	visible := fetchVisibleMessages(t, ts, convID, alice.AccessToken)
	require.Len(t, visible, 1, "only the out-of-range peer message remains visible")
	assert.Equal(t, "bob-ancient", visible[0])
}

func TestPurgeConversation_NonParticipant403(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "np_alice")
	bob := ts.CreateTestUser(t, "np_bob")
	eve := ts.CreateTestUser(t, "np_eve")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "private")

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": eve.Password},
		testhelpers.AuthHeaders(eve.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 1, countDMMessages(t, ts, convID))
}
