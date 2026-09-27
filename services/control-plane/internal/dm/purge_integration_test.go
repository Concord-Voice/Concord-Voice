package dm_test

// Integration tests for the DM/group bulk-purge endpoint (#1352): participant
// authorization, step-up auth (fail-closed default, M7), delete-own +
// persistent receiver-hide (M3 serve filters), and group-admin delete-all.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
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
	var ctxType, status string
	var completedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT context_type, status, deleted_count, hidden_count, completed_at FROM message_purges WHERE context_id = $1`,
		convID).Scan(&ctxType, &status, &deleted, &hidden, &completedAt))
	assert.Equal(t, "dm", ctxType)
	assert.Equal(t, "completed", status)
	assert.Equal(t, 2, deleted)
	assert.Equal(t, 1, hidden)
	assert.True(t, completedAt.Valid)

	// The hidden message must not resurface via the conversation-list preview (M3).
	w = ts.DoRequest(http.MethodGet, "/api/v1/dm/conversations", nil,
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, strings.Contains(w.Body.String(), "bob-1"),
		"hidden content leaked into the actor's conversation-list preview")
}

// purgeUnderTableLock runs actor's All Time DM purge while lockQuery holds a
// table lock, and returns the response the synchronous purge deadline produced
// (mirrors assertPurgeChannelPreflightTimeout in
// internal/messages/purge_integration_test.go). The lock is released before
// returning, so callers may read the locked table.
func purgeUnderTableLock(t *testing.T, ts *testhelpers.TestServer, lockQuery string, actor testhelpers.TestUser, convID string) *httptest.ResponseRecorder {
	t.Helper()
	lockTx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.Exec(lockQuery)
	require.NoError(t, err)

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
			map[string]any{"range": "all", "current_password": actor.Password},
			testhelpers.AuthHeaders(actor.AccessToken))
	}()

	select {
	case w := <-responses:
		require.NoError(t, lockTx.Rollback())
		return w
	case <-time.After(11 * time.Second):
		require.NoError(t, lockTx.Rollback())
		select {
		case <-responses:
		case <-time.After(time.Second):
			t.Fatal("DM purge request did not complete after releasing the table lock")
		}
		t.Fatal("DM purge did not honor the synchronous purge timeout")
		return nil
	}
}

// dialDMObserver opens a WebSocket for userID subscribed to convID.
func dialDMObserver(t *testing.T, ts *testhelpers.TestServer, userID, convID string) *gorillaWS.Conn {
	t.Helper()
	ticket := "dm-purge-observer-" + uuid.NewString()
	require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":purge-observer", time.Minute).Err())
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+wsServer.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	readUntilDMEvent(t, conn, "connected")
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_dm",
		"data": map[string]interface{}{"conversation_id": convID},
	}))
	readUntilDMEvent(t, conn, "dm_subscribed")
	return conn
}

// readUntilDMEvent reads frames until one of eventType arrives and returns its data.
func readUntilDMEvent(t *testing.T, conn *gorillaWS.Conn, eventType string) map[string]interface{} {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		var event struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		require.NoError(t, conn.ReadJSON(&event), "waiting for %s", eventType)
		if event.Type == eventType {
			return event.Data
		}
	}
}

// TestPurgeConversation_RoleLookupHonorsSynchronousTimeout proves the deadline
// starts before the preflight reads, as it does for channel and server purges:
// a blocked participant lookup answers 500 inside the budget instead of holding
// the request past the control plane's 15-second write deadline.
func TestPurgeConversation_RoleLookupHonorsSynchronousTimeout(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "purge_role_to_alice")
	bob := ts.CreateTestUser(t, "purge_role_to_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "alice-1")

	w := purgeUnderTableLock(t, ts, `LOCK TABLE dm_participants IN ACCESS EXCLUSIVE MODE`, alice, convID)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Equal(t, 1, countDMMessages(t, ts, convID))
}

// TestPurgeConversation_EngineRunHonorsSynchronousTimeout proves the engine run
// is bounded: a blocked audit INSERT answers 500 with no audit row and no delete.
func TestPurgeConversation_EngineRunHonorsSynchronousTimeout(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "purge_to_alice")
	bob := ts.CreateTestUser(t, "purge_to_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "alice-1")

	w := purgeUnderTableLock(t, ts, `LOCK TABLE message_purges IN ACCESS EXCLUSIVE MODE`, alice, convID)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	var audits int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM message_purges WHERE context_id = $1`, convID).Scan(&audits))
	assert.Equal(t, 0, audits)
	assert.Equal(t, 1, countDMMessages(t, ts, convID))
}

// TestPurgeConversation_ReceiverHideTimeoutKeepsAuditOpenAndBroadcasts covers a
// deadline that fires after the actor's messages are deleted but before the
// hide lands. The deletes are irreversible, so the peer still gets dm_purged;
// the audit row stays in_progress with the committed deleted_count instead of
// claiming a completed purge.
func TestPurgeConversation_ReceiverHideTimeoutKeepsAuditOpenAndBroadcasts(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "purge_hide_to_alice")
	bob := ts.CreateTestUser(t, "purge_hide_to_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "alice-1")
	insertDMMsg(t, ts, convID, bob.ID, "bob-1")
	bobConn := dialDMObserver(t, ts, bob.ID, convID)

	w := purgeUnderTableLock(t, ts, `LOCK TABLE dm_message_hidden_ranges IN ACCESS EXCLUSIVE MODE`, alice, convID)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	var aliceMsgs, bobMsgs int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1 AND user_id = $2`, convID, alice.ID).Scan(&aliceMsgs))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1 AND user_id = $2`, convID, bob.ID).Scan(&bobMsgs))
	assert.Equal(t, 0, aliceMsgs, "the engine run committed before the hide timed out")
	assert.Equal(t, 1, bobMsgs)

	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE conversation_id = $1 AND user_id = $2`, convID, alice.ID).Scan(&ranges))
	assert.Equal(t, 0, ranges)

	var status string
	var deleted, hidden int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT status, deleted_count, hidden_count FROM message_purges WHERE context_id = $1`, convID).Scan(&status, &deleted, &hidden))
	assert.Equal(t, "in_progress", status, "a purge whose hide failed must not read as completed")
	assert.Equal(t, 1, deleted)
	assert.Equal(t, 0, hidden)

	event := readUntilDMEvent(t, bobConn, "dm_purged")
	assert.Equal(t, convID, event["conversation_id"])
	assert.EqualValues(t, 1, event["deleted_count"])
}

// A failure completing the receiver-hide audit must not turn a committed
// deletion into a successful response or leave a half-applied hide. The
// trigger forces the deferred hidden_count update to fail.
// The trigger keeps this regression on the atomic hide/audit boundary.
func TestPurgeConversationHideAuditFailureIsAtomic(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "purge_audit_trigger_alice")
	bob := ts.CreateTestUser(t, "purge_audit_trigger_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "delete me")
	insertDMMsg(t, ts, convID, bob.ID, "keep me")
	_, err := ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION test_reject_dm_hide_audit() RETURNS trigger AS $$
		BEGIN
			IF NEW.hidden_count > 0 THEN RAISE EXCEPTION 'forced receiver-hide audit failure'; END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE TRIGGER test_reject_dm_hide_audit
		BEFORE UPDATE OF hidden_count ON message_purges
		FOR EACH ROW EXECUTE FUNCTION test_reject_dm_hide_audit()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec(`DROP TRIGGER IF EXISTS test_reject_dm_hide_audit ON message_purges`); err != nil {
			t.Errorf("drop purge audit test trigger: %v", err)
		}
		if _, err := ts.DB.Exec(`DROP FUNCTION IF EXISTS test_reject_dm_hide_audit()`); err != nil {
			t.Errorf("drop purge audit test function: %v", err)
		}
	})

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": alice.Password},
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.Equal(t, 1, countDMMessages(t, ts, convID), "peer message must remain")
	var hiddenRanges int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_message_hidden_ranges WHERE conversation_id = $1 AND user_id = $2`, convID, alice.ID,
	).Scan(&hiddenRanges))
	require.Zero(t, hiddenRanges, "failed hide must not commit a hidden range")
	var status string
	var deleted, hidden int
	var completedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`
		SELECT status, deleted_count, hidden_count, completed_at
		FROM message_purges WHERE context_id = $1`, convID).
		Scan(&status, &deleted, &hidden, &completedAt))
	require.Equal(t, "in_progress", status)
	require.Equal(t, 1, deleted)
	require.Zero(t, hidden)
	require.False(t, completedAt.Valid)
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

// TestPurgeConversation_P1EmailOnlyAccountPasswordAlone locks policy P1 on the
// DM purge: stepup.LoadSubject derives MFA from the factor tables, so an
// email/SMS-only account is not asked for an inline code it cannot supply.
func TestPurgeConversation_P1EmailOnlyAccountPasswordAlone(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "p1_alice")
	bob := ts.CreateTestUser(t, "p1_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "mine")
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{email}' WHERE id = $1`, alice.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": testhelpers.TestAuthPlaintext},
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 0, countDMMessages(t, ts, convID))
}

// The stale credential passes the request-time role lookup but must be rejected
// by the real Plan.Guard without deleting the message. Engine.Run leaves the
// recovery audit in progress so the failure is observable and retryable.
func TestPurgeConversationRejectsStaleCredentialWithoutDeleting(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "purge_stale_epoch")
	bob := ts.CreateTestUser(t, "purge_stale_peer")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "stale purge must survive")
	_, err := ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, alice.ID)
	require.NoError(t, err)
	staleToken := ts.SimulateStaleEpochWindow(t, alice.ID)

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID), map[string]any{"range": "all"},
		testhelpers.AuthHeaders(staleToken))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Equal(t, 1, countDMMessages(t, ts, convID))
	var status string
	var deleted int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT status, deleted_count FROM message_purges WHERE context_id = $1`, convID,
	).Scan(&status, &deleted))
	require.NotEqual(t, "completed", status)
	require.Zero(t, deleted)
}

// Group-admin scope is resolved before the engine transaction. Hold the users
// fence so the real Plan.Guard waits, demote the admin during that window, and
// verify the group message survives with an incomplete recovery audit.
func TestPurgeConversationRejectsAdminDemotionAtGuard(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	admin := ts.CreateTestUser(t, "purge_demote_admin")
	member := ts.CreateTestUser(t, "purge_demote_member")
	convID := ts.CreateGroupDMConversation(t, admin.ID, member.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)
	insertDMMsg(t, ts, convID, member.ID, "demotion must preserve message")
	_, err = ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, admin.ID)
	require.NoError(t, err)

	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var xid int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&xid))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, admin.ID).Scan(&locked))

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID), map[string]any{"range": "all"},
			testhelpers.AuthHeaders(admin.AccessToken))
		result <- w.Code
	}()
	dbtest.WaitForRowLockWaiter(t, ts.DB, xid)
	_, err = ts.DB.Exec(`UPDATE dm_participants SET role = 'member' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	select {
	case status := <-result:
		require.Equal(t, http.StatusForbidden, status)
	case <-time.After(time.Second):
		t.Fatal("group purge did not resume after releasing the users fence")
	}
	require.Equal(t, 1, countDMMessages(t, ts, convID))
	var auditStatus string
	var deleted int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT status, deleted_count FROM message_purges WHERE context_id = $1`, convID,
	).Scan(&auditStatus, &deleted))
	require.NotEqual(t, "completed", auditStatus)
	require.Zero(t, deleted)
}
