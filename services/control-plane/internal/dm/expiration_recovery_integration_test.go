//go:build integration

package dm_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMExpirationREST_RecoveryAfterBackfillFailure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "expiration_recovery_alice")
	bob := ts.CreateTestUser(t, "expiration_recovery_bob")
	conversationID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	var messageID string
	var createdAt time.Time
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO dm_messages (conversation_id, user_id, content, type) VALUES ($1, $2, 'ciphertext', 'user') RETURNING id, created_at`, conversationID, alice.ID).Scan(&messageID, &createdAt))
	installDMExpirationFailure(t, ts.DB, conversationID)
	auth := testhelpers.AuthHeaders(alice.AccessToken)

	w := ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration", map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "apply"}, auth)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var pending struct {
		Window   *int  `json:"window_seconds"`
		Revision int64 `json:"revision"`
		Pending  bool  `json:"backfill_pending"`
	}
	testhelpers.ParseJSON(t, w, &pending)
	require.True(t, pending.Pending)
	require.NotZero(t, pending.Revision)
	require.NotNil(t, pending.Window)
	assert.Equal(t, 3600, *pending.Window)
	before := dmExpirationState(t, ts.DB, conversationID)
	assert.True(t, before.pending)
	assert.Equal(t, pending.Revision, before.revision)
	assert.Equal(t, 3600, before.window)

	w = ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration", map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var conflict struct {
		Error string `json:"error"`
	}
	testhelpers.ParseJSON(t, w, &conflict)
	assert.Equal(t, expiration.ErrBackfillPending.Error(), conflict.Error)
	assert.Equal(t, before, dmExpirationState(t, ts.DB, conversationID))
	w = ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration", map[string]any{"mode": "resume", "revision": pending.Revision + 1}, auth)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	testhelpers.ParseJSON(t, w, &conflict)
	assert.Equal(t, expiration.ErrRevisionMismatch.Error(), conflict.Error)
	assert.Equal(t, before, dmExpirationState(t, ts.DB, conversationID))

	removeDMExpirationFailure(t, ts.DB)
	w = ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration", map[string]any{"mode": "resume", "revision": pending.Revision}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resumed struct {
		Window   *int  `json:"window_seconds"`
		Revision int64 `json:"revision"`
		Pending  bool  `json:"backfill_pending"`
	}
	testhelpers.ParseJSON(t, w, &resumed)
	require.NotNil(t, resumed.Window)
	assert.Equal(t, 3600, *resumed.Window)
	assert.Equal(t, pending.Revision, resumed.Revision)
	assert.False(t, resumed.Pending)
	var expires time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM dm_messages WHERE id = $1`, messageID).Scan(&expires))
	assert.Equal(t, createdAt.Add(time.Hour).UTC(), expires.UTC())
	after := dmExpirationState(t, ts.DB, conversationID)
	assert.False(t, after.pending)
	assert.Equal(t, pending.Revision, after.revision)
	assert.Equal(t, 3600, after.window)
}

func TestDMExpirationREST_RecoveryRejectsMalformedInput(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "expiration_recovery_invalid")
	bob := ts.CreateTestUser(t, "expiration_recovery_invalid_peer")
	conversationID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	auth := testhelpers.AuthHeaders(alice.AccessToken)
	assert.Equal(t, http.StatusBadRequest, ts.DoRequest(http.MethodPatch, dmExpirationPath+"not-a-uuid/expiration", map[string]any{}, auth).Code)
	assert.Equal(t, http.StatusBadRequest, ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration", "not-json", auth).Code)
}

func TestDMExpirationREST_CommitFailureResponseContract(t *testing.T) {
	for _, tc := range []struct {
		name          string
		request       map[string]any
		pending       bool
		windowSeconds *int
	}{
		{"retroactive apply maps commit failure to pending response", map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "apply"}, true, ptrInt(3600)},
		{"new only returns candidate policy on commit failure", map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, false, ptrInt(3600)},
		{"leave pending returns candidate policy on commit failure", map[string]any{"mode": "clear", "retroactive": "leave_pending"}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			alice := ts.CreateTestUser(t, "expiration_commit_alice")
			bob := ts.CreateTestUser(t, "expiration_commit_bob")
			conversationID := ts.CreateDMConversation(t, alice.ID, bob.ID)
			installDMExpirationCommitFailure(t, ts.DB, conversationID)
			before := dmExpirationState(t, ts.DB, conversationID)
			w := ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration", tc.request, testhelpers.AuthHeaders(alice.AccessToken))
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			var body struct {
				Window   *int  `json:"window_seconds"`
				Revision int64 `json:"revision"`
				Pending  bool  `json:"backfill_pending"`
			}
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, tc.windowSeconds, body.Window)
			assert.Equal(t, int64(1), body.Revision)
			assert.Equal(t, tc.pending, body.Pending)
			assert.Equal(t, before, dmExpirationState(t, ts.DB, conversationID))
			assert.Equal(t, 0, before.window)
			assert.Equal(t, int64(0), before.revision)
		})
	}
}

func ptrInt(value int) *int { return &value }

func installDMExpirationFailure(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	t.Cleanup(func() { removeDMExpirationFailure(t, db) })
	_, err := db.Exec(`
CREATE TABLE expiration_recovery_dm_control (conversation_id UUID PRIMARY KEY, fail BOOLEAN NOT NULL);
CREATE OR REPLACE FUNCTION expiration_recovery_dm_fail() RETURNS trigger AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM expiration_recovery_dm_control WHERE conversation_id = NEW.conversation_id AND fail) THEN
    RAISE EXCEPTION 'expiration recovery test failure';
  END IF;
  RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER expiration_recovery_dm_trigger BEFORE UPDATE OF expires_at ON dm_messages
FOR EACH ROW EXECUTE FUNCTION expiration_recovery_dm_fail()`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO expiration_recovery_dm_control(conversation_id, fail) VALUES ($1, TRUE)`, conversationID)
	require.NoError(t, err)
}

func installDMExpirationCommitFailure(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	t.Cleanup(func() { removeDMExpirationCommitFailure(t, db) })
	_, err := db.Exec(`
CREATE TABLE expiration_recovery_dm_commit_control (scope_id UUID PRIMARY KEY, fail BOOLEAN NOT NULL);
CREATE OR REPLACE FUNCTION expiration_recovery_dm_commit_fail() RETURNS trigger AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM expiration_recovery_dm_commit_control WHERE scope_id = NEW.id AND fail) THEN
    RAISE EXCEPTION 'expiration recovery commit failure';
  END IF;
  RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER expiration_recovery_dm_commit_trigger
AFTER UPDATE ON dm_conversations DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION expiration_recovery_dm_commit_fail()`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO expiration_recovery_dm_commit_control(scope_id, fail) VALUES ($1, TRUE)`, conversationID)
	require.NoError(t, err)
}

func removeDMExpirationCommitFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{`DROP TRIGGER IF EXISTS expiration_recovery_dm_commit_trigger ON dm_conversations`, `DROP FUNCTION IF EXISTS expiration_recovery_dm_commit_fail()`, `DROP TABLE IF EXISTS expiration_recovery_dm_commit_control`} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("cleanup %q: %v", q, err)
		}
	}
}

type dmExpirationStateRow struct {
	window   int
	revision int64
	pending  bool
}

func dmExpirationState(t *testing.T, db *sql.DB, conversationID string) dmExpirationStateRow {
	t.Helper()
	var state dmExpirationStateRow
	var window sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT expiration_window_seconds, expiration_revision, expiration_backfill_mode IS NOT NULL FROM dm_conversations WHERE id = $1`, conversationID).Scan(&window, &state.revision, &state.pending))
	if window.Valid {
		state.window = int(window.Int64)
	}
	return state
}

func removeDMExpirationFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{`DROP TRIGGER IF EXISTS expiration_recovery_dm_trigger ON dm_messages`, `DROP FUNCTION IF EXISTS expiration_recovery_dm_fail()`, `DROP TABLE IF EXISTS expiration_recovery_dm_control`} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("cleanup %q: %v", q, err)
		}
	}
}
