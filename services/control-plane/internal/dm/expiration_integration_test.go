package dm_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dmExpirationPath = "/api/v1/dm/conversations/"

func TestDMExpirationAuthorizationAndPolicyMetadata(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "exp_alice")
	bob := ts.CreateTestUser(t, "exp_bob")
	outsider := ts.CreateTestUser(t, "exp_outsider")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	t.Run("participant sets new-only policy and receives metadata", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"},
			testhelpers.AuthHeaders(alice.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			WindowSeconds *int   `json:"window_seconds"`
			Revision      int64  `json:"revision"`
			Pending       bool   `json:"backfill_pending"`
			UpdatedAt     string `json:"updated_at"`
		}
		testhelpers.ParseJSON(t, w, &body)
		require.NotNil(t, body.WindowSeconds)
		assert.Equal(t, 3600, *body.WindowSeconds)
		assert.Equal(t, int64(1), body.Revision)
		assert.False(t, body.Pending)
		assert.NotEmpty(t, body.UpdatedAt)

		var window, revision int
		require.NoError(t, ts.DB.QueryRow(`SELECT expiration_window_seconds, expiration_revision FROM dm_conversations WHERE id = $1`, convID).Scan(&window, &revision))
		assert.Equal(t, 3600, window)
		assert.Equal(t, 1, revision)
	})

	t.Run("outsider is not found and cannot mutate policy", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"},
			testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		var window, revision int
		require.NoError(t, ts.DB.QueryRow(`SELECT expiration_window_seconds, expiration_revision FROM dm_conversations WHERE id = $1`, convID).Scan(&window, &revision))
		assert.Equal(t, 3600, window)
		assert.Equal(t, 1, revision)
	})

	t.Run("resume without a pending marker returns conflict", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "resume", "revision": 1}, testhelpers.AuthHeaders(alice.AccessToken))
		assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	})

	t.Run("invalid window, missing auth, and missing conversation fail closed", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 7200, "retroactive": "new_only"}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		w = ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 7200, "retroactive": "new_only"}, testhelpers.AuthHeaders(alice.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		w = ts.DoRequest(http.MethodPatch, dmExpirationPath+uuid.NewString()+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, testhelpers.AuthHeaders(alice.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDMExpirationRequestRejectsOversizedAndMultipleJSONDocuments(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "exp_body_alice")
	bob := ts.CreateTestUser(t, "exp_body_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	valid := `{"mode":"set","window_seconds":3600,"retroactive":"new_only"}`
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "oversized", body: `{"mode":"set","window_seconds":3600,"retroactive":"new_only","padding":"` + strings.Repeat("x", 1100) + `"}`, want: http.StatusRequestEntityTooLarge},
		{name: "multiple documents", body: valid + "{}", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration", strings.NewReader(tc.body))
			req.Header = testhelpers.AuthHeaders(alice.AccessToken)
			w := httptest.NewRecorder()
			ts.Router.ServeHTTP(w, req)
			assert.Equal(t, tc.want, w.Code, w.Body.String())
		})
	}
}

func TestDMExpirationStartTimesOutOnBlockedConversationWithoutMutation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "exp_timeout_alice")
	bob := ts.CreateTestUser(t, "exp_timeout_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(4)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	barrier, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("failed to roll back timeout barrier: %v", rollbackErr)
		}
	})
	var txID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&txID))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID).Scan(&locked))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, testhelpers.AuthHeaders(alice.AccessToken))
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	select {
	case response := <-done:
		assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("expiration setter exceeded its bounded transaction timeout")
	}
	var revision int64
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_revision FROM dm_conversations WHERE id = $1`, convID).Scan(&revision))
	assert.Zero(t, revision)
	if err := barrier.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("failed to release timeout barrier: %v", err)
	}
}

func TestDMExpiration_GroupAdminOnly(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	admin := ts.CreateTestUser(t, "exp_group_admin")
	member := ts.CreateTestUser(t, "exp_group_member")
	convID := ts.CreateGroupDMConversation(t, admin.ID, member.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	w = ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestDMExpiration_StaleCredentialCannotMutatePolicy(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "exp_stale_alice")
	bob := ts.CreateTestUser(t, "exp_stale_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	staleToken := ts.SimulateStaleEpochWindow(t, alice.ID)

	w := ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var window sql.NullInt64
	var revision int64
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_window_seconds, expiration_revision FROM dm_conversations WHERE id = $1`, convID).Scan(&window, &revision))
	assert.False(t, window.Valid)
	assert.Zero(t, revision)
}

func TestDMExpiration_GroupAdminRechecksRoleAfterConversationLock(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	admin := ts.CreateTestUser(t, "exp_lock_admin")
	member := ts.CreateTestUser(t, "exp_lock_member")
	convID := ts.CreateGroupDMConversation(t, admin.ID, member.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)

	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(4)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	barrier, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("failed to roll back row-lock barrier: %v", rollbackErr)
		}
	})
	var txID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&txID))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID).Scan(&locked))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.DoRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration",
			map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"}, testhelpers.AuthHeaders(admin.AccessToken))
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	_, err = barrier.Exec(`UPDATE dm_participants SET role = 'member' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())
	response := <-done
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	var revision int64
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_revision FROM dm_conversations WHERE id = $1`, convID).Scan(&revision))
	assert.Zero(t, revision)
}

func TestDMHistoryRoundTripIncludesExpiry(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "exp_history_alice")
	bob := ts.CreateTestUser(t, "exp_history_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	_, err := ts.DB.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, expires_at) VALUES ($1, $2, 'ciphertext', 'user', $3)`, convID, alice.ID, expiresAt)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodGet, dmExpirationPath+convID+"/messages", nil, testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Messages []struct {
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Messages, 1)
	require.NotNil(t, body.Messages[0].ExpiresAt)
	assert.WithinDuration(t, expiresAt, *body.Messages[0].ExpiresAt, time.Microsecond)
}

func TestCompletedCallEventUsesEndedAtAndPreservesExpiryOnConflict(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "exp_call_caller")
	peer := ts.CreateTestUser(t, "exp_call_peer")
	convID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	convUUID := uuid.MustParse(convID)
	endedAt := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	startedAt := endedAt.Add(-2 * time.Minute)
	summary := dm.CompletedCallSummary{CallID: uuid.New(), CallerUserID: uuid.MustParse(caller.ID), ParticipantUserIDs: []uuid.UUID{uuid.MustParse(caller.ID), uuid.MustParse(peer.ID)}, StartedAt: startedAt, EndedAt: endedAt}

	for _, tc := range []struct {
		name   string
		window any
		want   bool
	}{
		{name: "null policy", window: nil, want: false},
		{name: "one hour policy", window: 3600, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ts.DB.Exec(`UPDATE dm_conversations SET expiration_window_seconds = $1 WHERE id = $2`, tc.window, convID)
			require.NoError(t, err)
			summary.CallID = uuid.New()
			require.NoError(t, dm.InsertCompletedCallEvent(context.Background(), ts.DB, convUUID, summary))
			var created time.Time
			var expires sql.NullTime
			require.NoError(t, ts.DB.QueryRow(`SELECT created_at, expires_at FROM dm_messages WHERE id = $1`, summary.CallID).Scan(&created, &expires))
			assert.WithinDuration(t, endedAt, created, time.Microsecond)
			if tc.want {
				require.True(t, expires.Valid)
				assert.WithinDuration(t, endedAt.Add(time.Hour), expires.Time, time.Microsecond)
			} else {
				assert.False(t, expires.Valid)
			}
		})
	}

	_, err := ts.DB.Exec(`UPDATE dm_conversations SET expiration_window_seconds = 3600 WHERE id = $1`, convID)
	require.NoError(t, err)
	conflictID := uuid.New()
	first := summary
	first.CallID = conflictID
	first.EndedAt = endedAt
	require.NoError(t, dm.InsertCompletedCallEvent(context.Background(), ts.DB, convUUID, first))
	second := first
	second.EndedAt = endedAt.Add(24 * time.Hour)
	require.NoError(t, dm.InsertCompletedCallEvent(context.Background(), ts.DB, convUUID, second))
	var expires time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM dm_messages WHERE id = $1`, conflictID).Scan(&expires))
	assert.WithinDuration(t, endedAt.Add(time.Hour), expires, time.Microsecond)
}
