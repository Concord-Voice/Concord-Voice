//go:build integration

package channels_test

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelExpirationREST_RecoveryAfterBackfillFailure(t *testing.T) {
	ts, owner, _, channelID := setupWithChannel(t)
	messageID := ts.CreateTestMessage(t, channelID, owner, testhelpers.ValidCiphertext())
	var createdAt time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT created_at FROM messages WHERE id = $1`, messageID).Scan(&createdAt))
	installChannelExpirationFailure(t, ts.DB, channelID)
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	set := map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "apply"}

	w := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", set, auth)
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
	before := channelExpirationState(t, ts.DB, channelID)
	assert.True(t, before.pending)
	assert.Equal(t, pending.Revision, before.revision)
	assert.Equal(t, 3600, before.window)

	w = ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var conflict struct {
		Error string `json:"error"`
	}
	testhelpers.ParseJSON(t, w, &conflict)
	assert.Equal(t, expiration.ErrBackfillPending.Error(), conflict.Error)
	assert.Equal(t, before, channelExpirationState(t, ts.DB, channelID))
	w = ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]any{"mode": "resume", "revision": pending.Revision + 1}, auth)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	testhelpers.ParseJSON(t, w, &conflict)
	assert.Equal(t, expiration.ErrRevisionMismatch.Error(), conflict.Error)
	assert.Equal(t, before, channelExpirationState(t, ts.DB, channelID))

	removeChannelExpirationFailure(t, ts.DB)
	w = ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]any{"mode": "resume", "revision": pending.Revision}, auth)
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
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM messages WHERE id = $1`, messageID).Scan(&expires))
	assert.Equal(t, createdAt.Add(time.Hour).UTC(), expires.UTC())
	after := channelExpirationState(t, ts.DB, channelID)
	assert.False(t, after.pending)
	assert.Equal(t, pending.Revision, after.revision)
	assert.Equal(t, 3600, after.window)
}

func TestChannelExpirationREST_RecoveryRejectsMalformedInput(t *testing.T) {
	ts, owner, _, channelID := setupWithChannel(t)
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, ts.DoRequest(http.MethodPatch, channelExpirationPath+"not-a-uuid/expiration", map[string]any{}, auth).Code)
	assert.Equal(t, http.StatusBadRequest, ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", "not-json", auth).Code)
}

func TestChannelExpirationREST_StrictBodyCapAndSingleDocument(t *testing.T) {
	ts, owner, _, channelID := setupWithChannel(t)
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	valid := `{"mode":"set","window_seconds":3600,"retroactive":"new_only"}`
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"oversized document", `{"mode":"set","window_seconds":3600,"retroactive":"new_only","padding":"` + strings.Repeat("x", 1100) + `"}`, http.StatusRequestEntityTooLarge},
		{"multiple documents", valid + `{}`, http.StatusBadRequest},
		{"trailing junk", valid + "junk", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doRawChunkedJSONRequest(ts, http.MethodPatch, channelExpirationPath+channelID+"/expiration", tc.body, auth)
			assert.Equal(t, tc.code, w.Code, w.Body.String())
		})
	}
}

func TestChannelExpirationREST_CommitFailureResponseContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request map[string]any
		status  int
		pending bool
	}{
		{"retroactive apply maps commit failure to pending response", map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "apply"}, http.StatusServiceUnavailable, true},
		{"new only returns candidate policy on commit failure", map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, http.StatusServiceUnavailable, false},
		{"leave pending returns candidate policy on commit failure", map[string]any{"mode": "clear", "retroactive": "leave_pending"}, http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, owner, _, channelID := setupWithChannel(t)
			installChannelExpirationCommitFailure(t, ts.DB, channelID)
			before := channelExpirationState(t, ts.DB, channelID)
			w := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", tc.request, testhelpers.AuthHeaders(owner.AccessToken))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.pending {
				var body struct {
					Window   *int  `json:"window_seconds"`
					Revision int64 `json:"revision"`
					Pending  bool  `json:"backfill_pending"`
				}
				testhelpers.ParseJSON(t, w, &body)
				require.NotNil(t, body.Window)
				assert.Equal(t, 3600, *body.Window)
				assert.Equal(t, int64(1), body.Revision)
				assert.True(t, body.Pending)
			} else {
				var body struct {
					Window  *int `json:"window_seconds"`
					Pending bool `json:"backfill_pending"`
				}
				testhelpers.ParseJSON(t, w, &body)
				assert.False(t, body.Pending)
				if tc.request["mode"] == "set" {
					require.NotNil(t, body.Window)
					assert.Equal(t, 3600, *body.Window)
				} else {
					assert.Nil(t, body.Window)
				}
			}
			assert.Equal(t, before, channelExpirationState(t, ts.DB, channelID))
			assert.Equal(t, 0, before.window)
			assert.Equal(t, int64(0), before.revision)
		})
	}
}

func installChannelExpirationFailure(t *testing.T, db *sql.DB, channelID string) {
	t.Helper()
	t.Cleanup(func() { removeChannelExpirationFailure(t, db) })
	_, err := db.Exec(`
CREATE TABLE expiration_recovery_channel_control (channel_id UUID PRIMARY KEY, fail BOOLEAN NOT NULL);
CREATE OR REPLACE FUNCTION expiration_recovery_channel_fail() RETURNS trigger AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM expiration_recovery_channel_control WHERE channel_id = NEW.channel_id AND fail) THEN
    RAISE EXCEPTION 'expiration recovery test failure';
  END IF;
  RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER expiration_recovery_channel_trigger BEFORE UPDATE OF expires_at ON messages
FOR EACH ROW EXECUTE FUNCTION expiration_recovery_channel_fail()`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO expiration_recovery_channel_control(channel_id, fail) VALUES ($1, TRUE)`, channelID)
	require.NoError(t, err)
}

func installChannelExpirationCommitFailure(t *testing.T, db *sql.DB, channelID string) {
	t.Helper()
	t.Cleanup(func() { removeChannelExpirationCommitFailure(t, db) })
	_, err := db.Exec(`
CREATE TABLE expiration_recovery_channel_commit_control (scope_id UUID PRIMARY KEY, fail BOOLEAN NOT NULL);
CREATE OR REPLACE FUNCTION expiration_recovery_channel_commit_fail() RETURNS trigger AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM expiration_recovery_channel_commit_control WHERE scope_id = NEW.id AND fail) THEN
    RAISE EXCEPTION 'expiration recovery commit failure';
  END IF;
  RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER expiration_recovery_channel_commit_trigger
AFTER UPDATE ON channels DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION expiration_recovery_channel_commit_fail()`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO expiration_recovery_channel_commit_control(scope_id, fail) VALUES ($1, TRUE)`, channelID)
	require.NoError(t, err)
}

func removeChannelExpirationCommitFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{`DROP TRIGGER IF EXISTS expiration_recovery_channel_commit_trigger ON channels`, `DROP FUNCTION IF EXISTS expiration_recovery_channel_commit_fail()`, `DROP TABLE IF EXISTS expiration_recovery_channel_commit_control`} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("cleanup %q: %v", q, err)
		}
	}
}

type channelExpirationStateRow struct {
	window   int
	revision int64
	pending  bool
}

func channelExpirationState(t *testing.T, db *sql.DB, channelID string) channelExpirationStateRow {
	t.Helper()
	var state channelExpirationStateRow
	var window sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT expiration_window_seconds, expiration_revision, expiration_backfill_mode IS NOT NULL FROM channels WHERE id = $1`, channelID).Scan(&window, &state.revision, &state.pending))
	if window.Valid {
		state.window = int(window.Int64)
	}
	return state
}

func removeChannelExpirationFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{`DROP TRIGGER IF EXISTS expiration_recovery_channel_trigger ON messages`, `DROP FUNCTION IF EXISTS expiration_recovery_channel_fail()`, `DROP TABLE IF EXISTS expiration_recovery_channel_control`} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("cleanup %q: %v", q, err)
		}
	}
}
