package dm_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClearWinsOverMessageMutationAfterInitialVisibilityPrecheck(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{name: "delete", method: http.MethodDelete},
		{name: "edit", method: http.MethodPatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupTS(t)
			actor := ts.CreateTestUser(t, "clear_"+tt.name+"_race_actor")
			peer := ts.CreateTestUser(t, "clear_"+tt.name+"_race_peer")
			ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
			conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
			originalContent := testhelpers.ValidCiphertext()
			messageID := insertDMMessage(t, ts, conversationID, actor.ID, originalContent)
			_, err := ts.DB.Exec(`UPDATE dm_messages SET created_at = clock_timestamp() - INTERVAL '1 minute' WHERE id = $1`, messageID)
			require.NoError(t, err)
			fileID := insertDMMediaFile(t, ts, actor.ID, conversationID, "photo", mimeImagePNG, 1)
			insertDMMessageAttachment(t, ts, messageID, fileID, 0)
			_, err = ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge)
				VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actor.ID)
			require.NoError(t, err)

			clearTx, err := ts.DB.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			t.Cleanup(func() {
				if rollbackErr := clearTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					t.Errorf("rollback clear lock transaction: %v", rollbackErr)
				}
			})
			var blockerPID int
			require.NoError(t, clearTx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
			var credentialEpoch sql.NullString
			require.NoError(t, clearTx.QueryRow(`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, actor.ID).Scan(&credentialEpoch))
			var requireAuth bool
			require.NoError(t, clearTx.QueryRow(`SELECT require_auth_before_purge FROM privacy_settings WHERE user_id = $1 FOR SHARE`, actor.ID).Scan(&requireAuth))
			var lockedConversation, lockedParticipant string
			require.NoError(t, clearTx.QueryRow(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID).Scan(&lockedConversation))
			require.NoError(t, clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE user_id = $1 AND conversation_id = $2 FOR UPDATE`, actor.ID, conversationID).Scan(&lockedParticipant))

			status := make(chan int, 1)
			go func() {
				var body interface{}
				if tt.method == http.MethodPatch {
					body = map[string]interface{}{"content": testhelpers.ValidCiphertext(), "key_version": 1}
				}
				status <- ts.DoRequest(tt.method, pathDMConversationsPrefix+conversationID+pathMsgSlash+messageID, body, testhelpers.AuthHeaders(actor.AccessToken)).Code
			}()
			requireBlockedBy(t, ts.DB, blockerPID, "%FROM users WHERE id = $1 FOR SHARE%", 1, tt.name+" should reach its transactional recheck after the initial visibility precheck")

			var cutoff time.Time
			require.NoError(t, clearTx.QueryRow(`SELECT clock_timestamp()`).Scan(&cutoff))
			require.NoError(t, dm.InsertClearRange(context.Background(), clearTx, actor.ID, conversationID, cutoff))
			require.NoError(t, clearTx.Commit())

			select {
			case responseStatus := <-status:
				assert.Equal(t, http.StatusNotFound, responseStatus)
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not resume after Clear committed", tt.name)
			}
			if tt.method == http.MethodDelete {
				assertPreservedDMDeleteSource(t, ts, messageID, fileID)
				return
			}
			var content string
			require.NoError(t, ts.DB.QueryRow(`SELECT content FROM dm_messages WHERE id = $1`, messageID).Scan(&content))
			assert.Equal(t, originalContent, content, "a hidden message must not be edited after Clear wins")
		})
	}
}

func TestConcurrentClearRequestsCoalesceWithoutUserLockUpgrade(t *testing.T) {
	ts := setupTS(t)
	observer := newVisibilityConcurrencyObserver(t)
	actor := ts.CreateTestUser(t, "concurrent_clear_actor")
	peer := ts.CreateTestUser(t, "concurrent_clear_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	conversationID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actor.ID)
	require.NoError(t, err)

	userTx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := userTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback user lock transaction: %v", rollbackErr)
		}
	})
	var blockerPID int
	require.NoError(t, userTx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
	var userID string
	require.NoError(t, userTx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, actor.ID).Scan(&userID))

	statuses := make(chan int, 2)
	clearPath := pathDMConversationsPrefix + conversationID + "/clear"
	for range 2 {
		go func() {
			statuses <- ts.DoRequest(http.MethodPost, clearPath, map[string]string{}, testhelpers.AuthHeaders(actor.AccessToken)).Code
		}()
	}
	// The publication gate is acquired before a Clear opens its transaction, so
	// only its owner can wait on the users row. The second request must remain
	// outside the database until the first publishes, avoiding a lock upgrade.
	requireBlockedBy(t, observer, blockerPID, "", 1, "only the first Clear should wait on the users lock")
	require.NoError(t, userTx.Commit())

	for range 2 {
		select {
		case status := <-statuses:
			assert.Equal(t, http.StatusOK, status)
		case <-time.After(5 * time.Second):
			t.Fatal("Clear did not complete after the users lock released")
		}
	}
	var ranges int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_hidden_ranges
		WHERE user_id = $1 AND conversation_id = $2 AND includes_own`, actor.ID, conversationID).Scan(&ranges))
	assert.Equal(t, 1, ranges, "concurrent Clear ranges should coalesce")
}

func newVisibilityConcurrencyObserver(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func requireBlockedBy(t *testing.T, db *sql.DB, blockerPID int, queryPattern string, want int, message string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := db.QueryRow(`WITH RECURSIVE waiters(pid) AS (
				SELECT waiter.pid
				FROM pg_stat_activity waiter
				WHERE waiter.datname = current_database()
				  AND waiter.wait_event_type = 'Lock'
				  AND $1 = ANY(pg_blocking_pids(waiter.pid))
				UNION
				SELECT waiter.pid
				FROM pg_stat_activity waiter
				JOIN waiters parent ON parent.pid = ANY(pg_blocking_pids(waiter.pid))
				WHERE waiter.datname = current_database()
				  AND waiter.wait_event_type = 'Lock'
			)
			SELECT count(*) FROM pg_stat_activity waiter
			WHERE waiter.pid IN (SELECT pid FROM waiters)
			  AND ($2 = '' OR waiter.query LIKE $2)`, blockerPID, queryPattern).Scan(&waiting)
		return err == nil && waiting == want
	}, 5*time.Second, 10*time.Millisecond, message)
}

func assertPreservedDMDeleteSource(t *testing.T, ts *testhelpers.TestServer, messageID, fileID string) {
	t.Helper()
	var messages, attachments int
	var deletedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, messageID).Scan(&messages))
	require.NoError(t, ts.DB.QueryRow(`SELECT deleted_at FROM media_files WHERE id = $1`, fileID).Scan(&deletedAt))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_message_attachments WHERE message_id = $1 AND file_id = $2`, messageID, fileID).Scan(&attachments))
	assert.Equal(t, 1, messages)
	assert.False(t, deletedAt.Valid, "Clear winning must leave the source media unretired")
	assert.Equal(t, 1, attachments)
}
