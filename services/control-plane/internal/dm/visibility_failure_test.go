package dm_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dmhandler "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMVisibility_ClearRejectsStaleCredentialEpoch(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_stale_epoch")
	peer := ts.CreateTestUser(t, "visibility_stale_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	stale := ts.SimulateStaleEpochWindow(t, actor.ID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(stale))

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "stale credentials must not create a visibility range")
}

func TestDMVisibility_ClearPrivacySettingsFailureFailsClosed(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_settings_error")
	peer := ts.CreateTestUser(t, "visibility_settings_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)

	_, err := ts.DB.Exec("ALTER TABLE privacy_settings RENAME TO privacy_settings_visibility_error")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, restoreErr := ts.DB.Exec("ALTER TABLE privacy_settings_visibility_error RENAME TO privacy_settings")
		if restoreErr != nil {
			t.Errorf("restore privacy_settings: %v", restoreErr)
		}
	})

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "settings read failure must not mutate visibility")
}

func TestDMVisibility_ClearOversizedBodyIsRejected(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_oversized")
	peer := ts.CreateTestUser(t, "visibility_oversized_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	rawRequest := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", pathDMConversationsPrefix+convID+"/clear", strings.NewReader(body))
		req.Header = testhelpers.AuthHeaders(actor.AccessToken)
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		return w
	}

	w := rawRequest(`{"current_password":"` + strings.Repeat("x", 1100) + `"}`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "an oversized body must not create a visibility range")

	w = rawRequest(`{}` + strings.Repeat(" ", 1100))
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "an oversized suffix after a valid document must be rejected")
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "an oversized suffix must not create a visibility range")
}

func TestDMVisibility_ClearMalformedJSONIsRejected(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_malformed_json")
	peer := ts.CreateTestUser(t, "visibility_malformed_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, false)
		ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = false`, actor.ID)
	require.NoError(t, err)
	rawRequest := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", pathDMConversationsPrefix+convID+"/clear", strings.NewReader(body))
		req.Header = testhelpers.AuthHeaders(actor.AccessToken)
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		return w
	}

	w := rawRequest(`["not an object"]`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = rawRequest(`{} {}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "a trailing JSON document must be rejected")
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "rejected JSON must not mutate history visibility")

	w = rawRequest(`null`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "a JSON null document must be rejected")
	w = rawRequest(" \n\tnull\n ")
	assert.Equal(t, http.StatusBadRequest, w.Code, "whitespace-wrapped JSON null must be rejected")
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "rejected null JSON must not mutate history visibility")

	w = rawRequest("{}\n \t")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String(), "trailing whitespace is valid JSON framing")
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Equal(t, 1, ranges, "the valid whitespace-terminated request creates exactly one range")
}

func TestDMVisibility_HideUnknownAndNonMemberAreUniformNotFound(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_hide_actor")
	peer := ts.CreateTestUser(t, "visibility_hide_peer")
	outsider := ts.CreateTestUser(t, "visibility_hide_outsider")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	auth := testhelpers.AuthHeaders(outsider.AccessToken)

	unknown := uuid.NewString()
	w := ts.DoRequest("POST", pathDMConversationsPrefix+unknown+"/hide", nil, auth)
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/hide", nil, auth)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDMVisibility_HideMutationFailureRollsBackAndEmitsNoEvent(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_hide_trigger")
	peer := ts.CreateTestUser(t, "visibility_hide_trigger_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)

	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_reject_dm_visibility_hide() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced visibility hide failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_dm_visibility_hide
		BEFORE UPDATE OF hidden_at ON dm_participants
		FOR EACH ROW EXECUTE FUNCTION test_reject_dm_visibility_hide()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_reject_dm_visibility_hide ON dm_participants;
			DROP FUNCTION IF EXISTS test_reject_dm_visibility_hide()`)
		if cleanupErr != nil {
			t.Errorf("cleanup hide trigger: %v", cleanupErr)
		}
	})

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/hide", nil, testhelpers.AuthHeaders(actor.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var hiddenAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_at FROM dm_participants WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&hiddenAt))
	assert.False(t, hiddenAt.Valid, "failed hide must leave the participant visible")
}

func TestDMVisibility_ClearRangeFailureRollsBack(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_clear_trigger")
	peer := ts.CreateTestUser(t, "visibility_clear_trigger_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, false) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = false`, actor.ID)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_reject_dm_visibility_range() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced visibility range failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_dm_visibility_range
		BEFORE INSERT ON dm_message_hidden_ranges
		FOR EACH ROW EXECUTE FUNCTION test_reject_dm_visibility_range()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_reject_dm_visibility_range ON dm_message_hidden_ranges;
			DROP FUNCTION IF EXISTS test_reject_dm_visibility_range()`)
		if cleanupErr != nil {
			t.Errorf("cleanup range trigger: %v", cleanupErr)
		}
	})

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&ranges))
	assert.Zero(t, ranges, "failed clear must not create a hidden range")
}

func TestDMVisibility_ClearBackupCodeSurvivesRangeRollback(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_backup_rollback")
	peer := ts.CreateTestUser(t, "visibility_backup_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	// Reuse the existing encrypted TOTP fixture, then add one deterministic
	// backup code to the same MFA record.
	enableVisibilityTOTP(t, ts, actor)
	const backupCode = "RECOV123"
	hash := sha256.Sum256([]byte(backupCode))
	_, err := ts.DB.Exec(`
		UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2 WHERE user_id = $3`,
		pq.Array([]string{fmt.Sprintf("%x", hash)}), pq.Array([]bool{false}), actor.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE FUNCTION test_reject_dm_visibility_backup_range() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced visibility range failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_reject_dm_visibility_backup_range
		BEFORE INSERT ON dm_message_hidden_ranges
		FOR EACH ROW EXECUTE FUNCTION test_reject_dm_visibility_backup_range()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_reject_dm_visibility_backup_range ON dm_message_hidden_ranges;
			DROP FUNCTION IF EXISTS test_reject_dm_visibility_backup_range()`)
		if cleanupErr != nil {
			t.Errorf("cleanup backup trigger: %v", cleanupErr)
		}
	})

	clearHistory := func() *httptest.ResponseRecorder {
		return ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{"mfa_code": backupCode}, testhelpers.AuthHeaders(actor.AccessToken))
	}
	w := clearHistory()
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var used []bool
	require.NoError(t, ts.DB.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, actor.ID).Scan(pq.Array(&used)))
	require.Len(t, used, 1)
	assert.False(t, used[0], "rolled-back Clear must not consume the backup code")

	_, err = ts.DB.Exec(`DROP TRIGGER test_reject_dm_visibility_backup_range ON dm_message_hidden_ranges; DROP FUNCTION test_reject_dm_visibility_backup_range()`)
	require.NoError(t, err)
	w = clearHistory()
	assert.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, ts.DB.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, actor.ID).Scan(pq.Array(&used)))
	require.Len(t, used, 1)
	assert.True(t, used[0], "successful Clear must consume the backup code")

	w = clearHistory()
	assert.Equal(t, http.StatusForbidden, w.Code, "a backup code is single use")
}

func TestDMVisibility_ClearPasswordlessNoMFAFailsClosedThenOptOutSucceeds(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_no_factor")
	peer := ts.CreateTestUser(t, "visibility_no_factor_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`UPDATE users SET password_hash = '', mfa_enabled = false, mfa_methods = '{}' WHERE id = $1`, actor.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM privacy_settings WHERE user_id = $1`, actor.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code, "the no-factor step-up contract is an actionable 400")

	_, err = ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, false)`, actor.ID)
	require.NoError(t, err)
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDMVisibility_ClearAmbiguousCommitRecoversExactReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "committed transaction"},
		{name: "request canceled after commit", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			actor := ts.CreateTestUser(t, "clear_ambiguous_actor_"+uuid.NewString()[:8])
			peer := ts.CreateTestUser(t, "clear_ambiguous_peer_"+uuid.NewString()[:8])
			conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
			_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge)
				VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actor.ID)
			require.NoError(t, err)

			h := dmhandler.NewHandler(dmhandler.HandlerDeps{DB: ts.DB, Log: logger.New("clear-ambiguous"), Hub: ts.Hub})
			requestCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			dmhandler.SetDMClearCommitForTest(h, func(tx *sql.Tx) error {
				require.NoError(t, tx.Commit())
				if tc.cancel {
					cancel()
				}
				return errors.New("injected ambiguous commit acknowledgement")
			})
			t.Cleanup(func() { dmhandler.SetDMClearCommitForTest(h, nil) })

			actorConn := connectDMVisibilityWebSocket(t, ts, actor.ID, conversationID, "clear-ambiguous-"+uuid.NewString())
			response := invokeDMVisibilityClearWithContext(requestCtx, h, strings.ToUpper(actor.ID), strings.ToUpper(conversationID))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			cutoff := dmVisibilityClearCutoff(t, ts, actor.ID, conversationID)
			var body struct {
				ClearedAt time.Time `json:"cleared_at"`
			}
			testhelpers.ParseJSON(t, response, &body)
			assert.True(t, body.ClearedAt.Equal(cutoff), "response must preserve the exact stored cutoff")
			var ranges int
			require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges
				WHERE user_id = $1 AND conversation_id = $2 AND includes_own`, actor.ID, conversationID).Scan(&ranges))
			assert.Equal(t, 1, ranges, "ambiguous commit recovery must identify exactly one stored range")
			frame := readVisibilityFrame(t, actorConn, "dm_conversation_cleared")
			assert.WithinDuration(t, cutoff, dmVisibilityFrameTime(t, frame, "cleared_at"), time.Microsecond)
			require.NoError(t, actorConn.SetReadDeadline(time.Now().Add(250*time.Millisecond)))
			var duplicate visibilityWSFrame
			if err := actorConn.ReadJSON(&duplicate); err == nil {
				assert.NotEqual(t, "dm_conversation_cleared", duplicate.Type, "ambiguous recovery must publish one actor event")
			}
		})
	}
}

func TestDMVisibility_ClearRollbackDoesNotConfirmOlderReceipt(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "clear_rollback_actor_"+uuid.NewString()[:8])
	peer := ts.CreateTestUser(t, "clear_rollback_peer_"+uuid.NewString()[:8])
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actor.ID)
	require.NoError(t, err)
	oldCutoff := time.Now().UTC().Add(-time.Minute)
	_, err = ts.DB.Exec(`INSERT INTO dm_message_hidden_ranges
		(user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', $3, TRUE)`, actor.ID, conversationID, oldCutoff)
	require.NoError(t, err)

	h := dmhandler.NewHandler(dmhandler.HandlerDeps{DB: ts.DB, Log: logger.New("clear-rollback"), Hub: ts.Hub})
	dmhandler.SetDMClearCommitForTest(h, func(tx *sql.Tx) error {
		require.NoError(t, tx.Rollback())
		return errors.New("injected rollback commit acknowledgement")
	})
	t.Cleanup(func() { dmhandler.SetDMClearCommitForTest(h, nil) })
	actorConn := connectDMVisibilityWebSocket(t, ts, actor.ID, conversationID, "clear-rollback")
	response := invokeDMVisibilityClear(h, actor.ID, conversationID)
	assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())

	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2 AND includes_own`, actor.ID, conversationID).Scan(&ranges))
	assert.Equal(t, 1, ranges, "rollback must not confirm an older range as this request's receipt")
	assert.WithinDuration(t, oldCutoff, dmVisibilityClearCutoff(t, ts, actor.ID, conversationID), time.Microsecond)
	require.NoError(t, actorConn.SetReadDeadline(time.Now().Add(250*time.Millisecond)))
	var frame visibilityWSFrame
	if err := actorConn.ReadJSON(&frame); err == nil {
		assert.NotEqual(t, "dm_conversation_cleared", frame.Type, "failed Clear must not publish an event")
	}
}

type visibilityWSFrame struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

func readVisibilityFrame(t *testing.T, conn *gorillaWS.Conn, want string) visibilityWSFrame {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		var frame visibilityWSFrame
		require.NoError(t, conn.ReadJSON(&frame))
		if frame.Type == want {
			return frame
		}
	}
}

func expectVisibilityEvent(t *testing.T, conn *gorillaWS.Conn, want string, convID string) {
	t.Helper()
	frame := readVisibilityFrame(t, conn, want)
	assert.Equal(t, convID, frame.Data["conversation_id"])
}

func assertPeerVisibilityEventAbsentBeforeBarrier(t *testing.T, conn *gorillaWS.Conn, convID string) {
	t.Helper()
	// Re-subscribing supplies a server acknowledgement barrier. Any queued
	// event is observed before that acknowledgement without relying on sleeps.
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_dm",
		"data": map[string]interface{}{"conversation_id": convID},
	}))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		var frame visibilityWSFrame
		require.NoError(t, conn.ReadJSON(&frame))
		if frame.Type == "dm_conversation_hidden" || frame.Type == "dm_conversation_cleared" {
			t.Fatalf("peer received actor-only visibility event %q", frame.Type)
		}
		if frame.Type == "dm_subscribed" {
			return
		}
	}
}

func TestDMVisibility_EventsAreActorOnlyAndStatePreserved(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "visibility_events_actor")
	peer := ts.CreateTestUser(t, "visibility_events_peer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	insertDMMessage(t, ts, convID, peer.ID, "unread visibility event")
	readAt := time.Now().UTC().Add(-time.Minute)
	_, err := ts.DB.Exec(`INSERT INTO dm_read_states (user_id, conversation_id, last_read_at) VALUES ($1, $2, $3)`, actor.ID, convID, readAt)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, false) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = false`, actor.ID)
	require.NoError(t, err)

	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	connect := func(userID, suffix string) *gorillaWS.Conn {
		ticket := "dm-visibility-" + suffix + "-" + uuid.NewString()
		require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":"+suffix, time.Minute).Err())
		conn, _, dialErr := gorillaWS.DefaultDialer.Dial("ws"+wsServer.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
		require.NoError(t, dialErr)
		t.Cleanup(func() { _ = conn.Close() })
		readVisibilityFrame(t, conn, "connected")
		require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_dm", "data": map[string]interface{}{"conversation_id": convID}}))
		readVisibilityFrame(t, conn, "dm_subscribed")
		return conn
	}
	actorOne := connect(actor.ID, "actor-one")
	actorTwo := connect(actor.ID, "actor-two")
	peerConn := connect(peer.ID, "peer")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/hide", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	expectVisibilityEvent(t, actorOne, "dm_conversation_hidden", convID)
	expectVisibilityEvent(t, actorTwo, "dm_conversation_hidden", convID)
	assertPeerVisibilityEventAbsentBeforeBarrier(t, peerConn, convID)

	var hiddenAt sql.NullTime
	var persistedReadAt time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_at FROM dm_participants WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&hiddenAt))
	require.NoError(t, ts.DB.QueryRow(`SELECT last_read_at FROM dm_read_states WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&persistedReadAt))
	assert.WithinDuration(t, readAt, persistedReadAt, time.Second)
	assert.True(t, hiddenAt.Valid)

	w = ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/hide", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	expectVisibilityEvent(t, actorOne, "dm_conversation_hidden", convID)
	expectVisibilityEvent(t, actorTwo, "dm_conversation_hidden", convID)
	assertPeerVisibilityEventAbsentBeforeBarrier(t, peerConn, convID)
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_at FROM dm_participants WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&hiddenAt))
	assert.False(t, hiddenAt.Valid, "unhide must clear only the actor hide timestamp")

	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear", map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	expectVisibilityEvent(t, actorOne, "dm_conversation_cleared", convID)
	expectVisibilityEvent(t, actorTwo, "dm_conversation_cleared", convID)
	assertPeerVisibilityEventAbsentBeforeBarrier(t, peerConn, convID)
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_at FROM dm_participants WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&hiddenAt))
	assert.False(t, hiddenAt.Valid, "clear must not hide the conversation")
	require.NoError(t, ts.DB.QueryRow(`SELECT last_read_at FROM dm_read_states WHERE user_id = $1 AND conversation_id = $2`, actor.ID, convID).Scan(&persistedReadAt))
	assert.WithinDuration(t, readAt, persistedReadAt, time.Second)

	w = ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages", nil, testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Empty(t, testhelpers.JSONField[[]interface{}](t, body, "messages"))
}

func TestDMVisibility_PublicationGateOrdersUnhideThenHide(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "order_actor_"+uuid.NewString()[:8])
	peer := ts.CreateTestUser(t, "order_peer_"+uuid.NewString()[:8])
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET hidden_at = clock_timestamp() WHERE conversation_id = $1 AND user_id = $2`, conversationID, actor.ID)
	require.NoError(t, err)

	actorConn := connectDMVisibilityWebSocket(t, ts, actor.ID, conversationID, "publication-order")
	h := dmhandler.NewHandler(dmhandler.HandlerDeps{DB: ts.DB, Log: logger.New("visibility-order"), Hub: ts.Hub})
	firstCommitted, secondCommitted, release := installDMVisibilityPublicationPause(t, h)
	var unhideFinished, hideFinished <-chan struct{}
	defer func() {
		release()
		waitDMVisibilityFinished(t, unhideFinished, "Unhide")
		waitDMVisibilityFinished(t, hideFinished, "Hide")
	}()

	// uuid.Parse canonicalizes both route values before lock selection. The first
	// request deliberately uses equivalent non-canonical spellings to prove the
	// later canonical request uses the same publication lock.
	unhideResult, unhideDone := startDMVisibilityRequest(func() *httptest.ResponseRecorder {
		return invokeDMVisibilityToggle(h, http.MethodDelete, strings.ToUpper(actor.ID), strings.ToUpper(conversationID))
	})
	unhideFinished = unhideDone
	requireSignal(t, firstCommitted, "Unhide did not commit before publication pause")
	assertDMVisibilityHiddenAt(t, ts, actor.ID, conversationID, false)

	hideResult, hideDone := startDMVisibilityRequest(func() *httptest.ResponseRecorder {
		return invokeDMVisibilityToggle(h, http.MethodPost, actor.ID, conversationID)
	})
	hideFinished = hideDone
	assertDMVisibilityDoesNotReachSecondCommit(t, secondCommitted, "Hide committed or published while Unhide still held the publication gate")
	assertDMVisibilityHiddenAt(t, ts, actor.ID, conversationID, false)

	release()
	require.Equal(t, http.StatusOK, waitDMVisibilityResponse(t, unhideResult, unhideFinished, "Unhide").Code)
	require.Equal(t, http.StatusOK, waitDMVisibilityResponse(t, hideResult, hideFinished, "Hide").Code)

	first := readVisibilityFrame(t, actorConn, "dm_conversation_hidden")
	second := readVisibilityFrame(t, actorConn, "dm_conversation_hidden")
	assert.Equal(t, conversationID, first.Data["conversation_id"])
	assert.Nil(t, first.Data["hidden_at"], "Unhide must publish the null state first")
	assert.Equal(t, conversationID, second.Data["conversation_id"])
	assert.NotNil(t, second.Data["hidden_at"], "Hide must publish only after Unhide")
	assertDMVisibilityHiddenAt(t, ts, actor.ID, conversationID, true)
}

func TestDMVisibility_PublicationGateOrdersConcurrentClears(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "order_clear_actor_"+uuid.NewString()[:8])
	peer := ts.CreateTestUser(t, "order_clear_peer_"+uuid.NewString()[:8])
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actor.ID)
	require.NoError(t, err)

	actorConn := connectDMVisibilityWebSocket(t, ts, actor.ID, conversationID, "clear-publication-order")
	h := dmhandler.NewHandler(dmhandler.HandlerDeps{DB: ts.DB, Log: logger.New("clear-publication-order"), Hub: ts.Hub})
	var commitCalls atomic.Int32
	dmhandler.SetDMClearCommitForTest(h, func(tx *sql.Tx) error {
		if commitCalls.Add(1) == 1 {
			require.NoError(t, tx.Commit())
			return errors.New("injected first Clear commit acknowledgement")
		}
		return tx.Commit()
	})
	t.Cleanup(func() { dmhandler.SetDMClearCommitForTest(h, nil) })
	firstCommitted, secondCommitted, release := installDMVisibilityPublicationPause(t, h)
	var firstFinished, secondFinished <-chan struct{}
	defer func() {
		release()
		waitDMVisibilityFinished(t, firstFinished, "first Clear")
		waitDMVisibilityFinished(t, secondFinished, "second Clear")
	}()

	firstResult, firstDone := startDMVisibilityRequest(func() *httptest.ResponseRecorder {
		return invokeDMVisibilityClear(h, strings.ToUpper(actor.ID), strings.ToUpper(conversationID))
	})
	firstFinished = firstDone
	requireSignal(t, firstCommitted, "first Clear did not commit before publication pause")
	firstCutoff := dmVisibilityClearCutoff(t, ts, actor.ID, conversationID)

	secondResult, secondDone := startDMVisibilityRequest(func() *httptest.ResponseRecorder {
		return invokeDMVisibilityClear(h, actor.ID, conversationID)
	})
	secondFinished = secondDone
	assertDMVisibilityDoesNotReachSecondCommit(t, secondCommitted, "second Clear committed or published while first Clear held the publication gate")
	assert.WithinDuration(t, firstCutoff, dmVisibilityClearCutoff(t, ts, actor.ID, conversationID), time.Microsecond)

	release()
	require.Equal(t, http.StatusOK, waitDMVisibilityResponse(t, firstResult, firstFinished, "first Clear").Code)
	require.Equal(t, http.StatusOK, waitDMVisibilityResponse(t, secondResult, secondFinished, "second Clear").Code)

	first := readVisibilityFrame(t, actorConn, "dm_conversation_cleared")
	second := readVisibilityFrame(t, actorConn, "dm_conversation_cleared")
	firstEventCutoff := dmVisibilityFrameTime(t, first, "cleared_at")
	secondEventCutoff := dmVisibilityFrameTime(t, second, "cleared_at")
	assert.True(t, firstEventCutoff.Before(secondEventCutoff), "Clear events must retain increasing cutoff order")
	assert.WithinDuration(t, secondEventCutoff, dmVisibilityClearCutoff(t, ts, actor.ID, conversationID), time.Microsecond)
}

func installDMVisibilityPublicationPause(t *testing.T, h *dmhandler.Handler) (<-chan struct{}, <-chan struct{}, func()) {
	t.Helper()
	firstCommitted := make(chan struct{})
	secondCommitted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	dmhandler.SetDMVisibilityCommitHookForTest(h, func() {
		switch calls.Add(1) {
		case 1:
			close(firstCommitted)
			<-release
		case 2:
			close(secondCommitted)
		}
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		dmhandler.SetDMVisibilityCommitHookForTest(h, nil)
	})
	return firstCommitted, secondCommitted, func() {
		releaseOnce.Do(func() { close(release) })
	}
}

func startDMVisibilityRequest(request func() *httptest.ResponseRecorder) (<-chan *httptest.ResponseRecorder, <-chan struct{}) {
	result := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- request()
	}()
	return result, finished
}

func waitDMVisibilityResponse(t *testing.T, result <-chan *httptest.ResponseRecorder, finished <-chan struct{}, name string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-result:
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s request did not finish", name)
		}
		return response
	case <-time.After(5 * time.Second):
		t.Fatalf("%s request did not return", name)
		return nil
	}
}

func waitDMVisibilityFinished(t *testing.T, finished <-chan struct{}, name string) {
	t.Helper()
	if finished == nil {
		return
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Errorf("%s request did not finish during cleanup", name)
	}
}

func requireSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func assertDMVisibilityDoesNotReachSecondCommit(t *testing.T, secondCommitted <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-secondCommitted:
		t.Fatal(message)
	case <-time.After(time.Second):
	}
}

func invokeDMVisibilityToggle(h *dmhandler.Handler, method, actorID, conversationID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/api/v1/dm/conversations/"+conversationID+"/hide", nil)
	c.Params = gin.Params{{Key: "id", Value: conversationID}}
	c.Set("user_id", actorID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": ""})
	if method == http.MethodDelete {
		h.UnhideConversation(c)
	} else {
		h.HideConversation(c)
	}
	return w
}

func invokeDMVisibilityClear(h *dmhandler.Handler, actorID, conversationID string) *httptest.ResponseRecorder {
	return invokeDMVisibilityClearWithContext(context.Background(), h, actorID, conversationID)
}

func invokeDMVisibilityClearWithContext(requestCtx context.Context, h *dmhandler.Handler, actorID, conversationID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/api/v1/dm/conversations/"+conversationID+"/clear", strings.NewReader(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: conversationID}}
	c.Set("user_id", actorID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": ""})
	h.ClearConversation(c)
	return w
}

func connectDMVisibilityWebSocket(t *testing.T, ts *testhelpers.TestServer, userID, conversationID, suffix string) *gorillaWS.Conn {
	t.Helper()
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	ticket := "dm-visibility-" + suffix + "-" + uuid.NewString()
	require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":"+suffix, time.Minute).Err())
	conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+wsServer.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	readVisibilityFrame(t, conn, "connected")
	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_dm", "data": map[string]interface{}{"conversation_id": conversationID}}))
	readVisibilityFrame(t, conn, "dm_subscribed")
	return conn
}

func assertDMVisibilityHiddenAt(t *testing.T, ts *testhelpers.TestServer, actorID, conversationID string, wantHidden bool) {
	t.Helper()
	var hiddenAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_at FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, actorID).Scan(&hiddenAt))
	assert.Equal(t, wantHidden, hiddenAt.Valid)
}

func dmVisibilityClearCutoff(t *testing.T, ts *testhelpers.TestServer, actorID, conversationID string) time.Time {
	t.Helper()
	var cutoff time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT hidden_to FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2 AND includes_own
		ORDER BY hidden_to DESC LIMIT 1`, actorID, conversationID).Scan(&cutoff))
	return cutoff
}

func dmVisibilityFrameTime(t *testing.T, frame visibilityWSFrame, key string) time.Time {
	t.Helper()
	value, ok := frame.Data[key].(string)
	require.True(t, ok, "%s must be an RFC3339 timestamp", key)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}
