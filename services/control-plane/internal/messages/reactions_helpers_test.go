package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newReactionHelperHandler(t *testing.T) *Handler {
	t.Helper()

	db, _ := testdb.SetupTestDB(t)

	return NewHandler(db, logger.New("test"), nil, nil, nil, nil)
}

func TestReactionHelpersHandleDatabaseErrors(t *testing.T) {
	h := newReactionHelperHandler(t)
	thumbsUp := "\U0001f44d"

	t.Run("summary query error", func(t *testing.T) {
		summary := h.buildReactionSummaryWithQuery("SELECT user_id FROM missing_reaction_table WHERE id = $1 AND emoji = $2", uuid.NewString(), thumbsUp, uuid.NewString())
		assert.Nil(t, summary)
	})

	t.Run("response query error", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		h.writeReactionsResponse(c, "SELECT emoji FROM missing_reaction_table WHERE id = $1", uuid.NewString(), uuid.NewString())

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("single summary scan error", func(t *testing.T) {
		rows, err := h.db.Query("SELECT $1::uuid AS user_id", uuid.NewString())
		require.NoError(t, err)

		summary, err := scanSingleReactionSummary(rows, thumbsUp, uuid.NewString())

		assert.Nil(t, summary)
		require.Error(t, err)
	})

	t.Run("response scan error", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		h.writeReactionsResponse(c, "SELECT $1::text AS emoji", uuid.NewString(), uuid.NewString())

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("batch scan error", func(t *testing.T) {
		loaded, err := loadReactionsForMessagesWithQuery(h.db, "SELECT $1::uuid AS message_id", []string{uuid.NewString()}, uuid.NewString())

		assert.Nil(t, loaded)
		require.Error(t, err)
	})

	t.Run("toggle delete error", func(t *testing.T) {
		zeroRowSQL := "SELECT $1::text, $2::text, $3::text, $4::text WHERE false"

		action, err := h.toggleReactionRow(zeroRowSQL, "SELECT * FROM missing_reaction_table WHERE id = $1", uuid.NewString(), uuid.NewString(), thumbsUp)

		assert.Empty(t, action)
		require.Error(t, err)
	})
}

func TestToggleDMReactionRowRequiresCurrentParticipant(t *testing.T) {
	h := newReactionHelperHandler(t)
	thumbsUp := "\U0001f44d"
	userID := uuid.NewString()
	otherUserID := uuid.NewString()
	convID := uuid.NewString()
	msgID := uuid.NewString()

	t.Cleanup(func() {
		_, _ = h.db.Exec("DELETE "+"FROM users WHERE id IN ($1, $2)", userID, otherUserID)
	})
	_, err := h.db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true), ($4, $5, $6, 'hash', true, true)`,
		userID, userID+"@test.concord.chat", "dmraceuser",
		otherUserID, otherUserID+"@test.concord.chat", "dmraceother",
	)
	require.NoError(t, err)
	_, err = h.db.Exec(
		`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`,
		convID, otherUserID,
	)
	require.NoError(t, err)
	_, err = h.db.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
		convID, userID, otherUserID,
	)
	require.NoError(t, err)
	_, err = h.db.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content) VALUES ($1, $2, $3, 'hello')`,
		msgID, convID, otherUserID,
	)
	require.NoError(t, err)
	_, err = h.db.Exec(
		"DELETE "+"FROM dm_participants WHERE conversation_id = $1 AND user_id = $2",
		convID, userID,
	)
	require.NoError(t, err)

	action, _, err := h.toggleDMReactionInConversation(context.Background(), msgID, userID, thumbsUp, convID)

	assert.Empty(t, action)
	require.Error(t, err)

	var count int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM dm_message_reactions WHERE message_id = $1 AND user_id = $2`, msgID, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestToggleDMReactionLocksActorBeforeConversation(t *testing.T) {
	h := newReactionHelperHandler(t)
	messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)

	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(4)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })

	barrier, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("failed to roll back reaction lock barrier: %v", rollbackErr)
		}
	})
	var barrierTxID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&barrierTxID))
	var lockedUserID string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM users WHERE id = $1 FOR UPDATE`, actorID).Scan(&lockedUserID))

	reactionCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan struct {
		action  string
		summary *models.ReactionSummary
		err     error
	}, 1)
	go func() {
		action, summary, toggleErr := h.toggleDMReactionInConversation(reactionCtx, messageID, actorID, "👍", conversationID)
		result <- struct {
			action  string
			summary *models.ReactionSummary
			err     error
		}{action: action, summary: summary, err: toggleErr}
	}()

	testdb.WaitForRowLockWaiter(t, probe, barrierTxID)
	var lockedConversationID string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, conversationID).Scan(&lockedConversationID))
	assert.Equal(t, actorID, lockedUserID)
	assert.Equal(t, conversationID, lockedConversationID)

	require.NoError(t, barrier.Commit())
	select {
	case outcome := <-result:
		require.NoError(t, outcome.err)
		assert.Equal(t, "added", outcome.action)
		require.NotNil(t, outcome.summary)
		assert.Equal(t, 1, outcome.summary.Count)
	case <-reactionCtx.Done():
		require.FailNow(t, "reaction did not finish after releasing the actor lock: %v", reactionCtx.Err())
	}

	var reactionCount int
	require.NoError(t, h.db.QueryRow(`SELECT count(*) FROM dm_message_reactions WHERE message_id = $1 AND user_id = $2`, messageID, actorID).Scan(&reactionCount))
	assert.Equal(t, 1, reactionCount)
}

func TestToggleDMReactionRequiresVisibleMessageForCurrentParticipant(t *testing.T) {
	h := newReactionHelperHandler(t)
	messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, ok := h.lookupMessageContext(c, messageID, actorID)
	require.True(t, ok, "actor must resolve the message before visibility changes")
	_, err := h.db.Exec(`
		INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', NOW() + INTERVAL '1 minute', false)`, actorID, conversationID)
	require.NoError(t, err)

	action, summary, err := h.toggleDMReactionInConversation(context.Background(), messageID, actorID, "👍", conversationID)
	assert.Empty(t, action)
	assert.Nil(t, summary)
	require.Error(t, err)
	assertNoDMReaction(t, h, messageID, actorID)
}

func seedDMReactionForAggregateTest(t *testing.T, h *Handler) (messageID, actorID, peerID, conversationID string) {
	t.Helper()

	actorID = uuid.NewString()
	peerID = uuid.NewString()
	conversationID = uuid.NewString()
	messageID = uuid.NewString()
	actorName := "reaction_actor_" + actorID[:8]
	peerName := "reaction_peer_" + peerID[:8]

	t.Cleanup(func() {
		_, err := h.db.Exec("DELETE FROM users WHERE id IN ($1, $2)", actorID, peerID)
		assert.NoError(t, err)
	})

	_, err := h.db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'hash', true, true), ($4, $5, $6, 'hash', true, true)`,
		actorID, actorID+"@test.concord.chat", actorName,
		peerID, peerID+"@test.concord.chat", peerName,
	)
	require.NoError(t, err)
	_, err = h.db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, peerID)
	require.NoError(t, err)
	_, err = h.db.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
		conversationID, actorID, peerID,
	)
	require.NoError(t, err)
	_, err = h.db.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content) VALUES ($1, $2, $3, 'reaction target')`,
		messageID, conversationID, peerID,
	)
	require.NoError(t, err)

	return messageID, actorID, peerID, conversationID
}

func assertNoDMReaction(t *testing.T, h *Handler, messageID, userID string) {
	t.Helper()

	var count int
	err := h.db.QueryRow(
		`SELECT COUNT(*) FROM dm_message_reactions WHERE message_id = $1 AND user_id = $2`, messageID, userID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestToggleDMReactionAggregateQueryFailureRollsBackMutation(t *testing.T) {
	h := newReactionHelperHandler(t)
	messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)

	original := dmMessageReactionBroadcastSummarySQL
	dmMessageReactionBroadcastSummarySQL = `
		SELECT user_id FROM missing_dm_reaction_summary
		WHERE message_id = $1 AND emoji = $2
	`
	t.Cleanup(func() { dmMessageReactionBroadcastSummarySQL = original })

	action, summary, err := h.toggleDMReactionInConversation(context.Background(), messageID, actorID, "👍", conversationID)
	require.Error(t, err)
	assert.Empty(t, action)
	assert.Nil(t, summary)
	assertNoDMReaction(t, h, messageID, actorID)
}

func TestToggleDMReactionAggregateScanFailureRollsBackMutation(t *testing.T) {
	h := newReactionHelperHandler(t)
	messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)

	original := dmMessageReactionBroadcastSummarySQL
	dmMessageReactionBroadcastSummarySQL = `
		SELECT mr.user_id
		FROM dm_message_reactions mr
		WHERE mr.message_id = $1 AND mr.emoji = $2
	`
	t.Cleanup(func() { dmMessageReactionBroadcastSummarySQL = original })

	action, summary, err := h.toggleDMReactionInConversation(context.Background(), messageID, actorID, "👍", conversationID)
	require.Error(t, err)
	assert.Empty(t, action)
	assert.Nil(t, summary)
	assertNoDMReaction(t, h, messageID, actorID)
}

func TestToggleDMReactionPostCommitSummaryFailureReturnsCommittedAction(t *testing.T) {
	base := newReactionHelperHandler(t)
	h := NewHandler(base.db, logger.New("test"), websocket.NewHub(nil, nil), nil, nil, nil)
	messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)

	original := dmMessageReactionSummarySQL
	dmMessageReactionSummarySQL = `
		SELECT user_id FROM missing_dm_reaction_summary
		WHERE message_id = $1 AND emoji = $2 AND user_id = $3
	`
	t.Cleanup(func() { dmMessageReactionSummarySQL = original })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/", nil)
	h.toggleDMReaction(c, messageID, actorID, "👍", conversationID)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.JSONEq(t, `"added"`, string(response["action"]))
	_, hasReaction := response["reaction"]
	assert.False(t, hasReaction, "post-commit summary failure must omit the optional reaction")

	var count int
	require.NoError(t, h.db.QueryRow(
		`SELECT COUNT(*) FROM dm_message_reactions WHERE message_id = $1 AND user_id = $2`, messageID, actorID,
	).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestToggleDMReactionCapturesAggregateBeforeClear(t *testing.T) {
	h := newReactionHelperHandler(t)
	messageID, actorID, peerID, conversationID := seedDMReactionForAggregateTest(t, h)

	const (
		barrierLockClassID  = 2820
		barrierLockObjectID = 1
	)
	_, err := h.db.Exec(`
		CREATE OR REPLACE FUNCTION test_dm_reaction_aggregate_barrier() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(2820, 1);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_dm_reaction_aggregate_barrier
		BEFORE INSERT ON dm_message_reactions
		FOR EACH ROW EXECUTE FUNCTION test_dm_reaction_aggregate_barrier();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := h.db.Exec(`
			DROP TRIGGER IF EXISTS test_dm_reaction_aggregate_barrier ON dm_message_reactions;
			DROP FUNCTION IF EXISTS test_dm_reaction_aggregate_barrier();
		`)
		assert.NoError(t, dropErr)
	})

	barrierConn, err := h.db.Conn(context.Background())
	require.NoError(t, err)
	barrierHeld := false
	t.Cleanup(func() {
		if barrierHeld {
			var released bool
			releaseErr := barrierConn.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, barrierLockClassID, barrierLockObjectID).Scan(&released)
			assert.NoError(t, releaseErr)
			assert.True(t, released)
		}
		assert.NoError(t, barrierConn.Close())
	})
	require.NoError(t, barrierConn.QueryRowContext(context.Background(), `SELECT pg_advisory_lock($1, $2)`, barrierLockClassID, barrierLockObjectID).Scan(new(interface{})))
	barrierHeld = true

	toggleResult := make(chan struct {
		action       string
		summaryCount int
		err          error
	}, 1)
	go func() {
		action, summary, toggleErr := h.toggleDMReactionInConversation(context.Background(), messageID, actorID, "👍", conversationID)
		result := struct {
			action       string
			summaryCount int
			err          error
		}{action: action, err: toggleErr}
		if summary != nil {
			result.summaryCount = summary.Count
		}
		toggleResult <- result
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := h.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE locktype = 'advisory'
					AND NOT granted
					AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND classid = $1
					AND objid = $2
			)`, barrierLockClassID, barrierLockObjectID).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond, "reaction insert did not reach the barrier")

	clearDone := make(chan error, 1)
	clearStarted := make(chan int, 1)
	go func() {
		tx, clearErr := h.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if clearErr != nil {
			clearDone <- clearErr
			return
		}
		defer func() {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback Clear transaction: %v", rollbackErr)
			}
		}()
		var clearPID int
		if clearErr = tx.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&clearPID); clearErr != nil {
			clearDone <- clearErr
			return
		}
		clearStarted <- clearPID

		var lockedConversationID string
		if clearErr = tx.QueryRowContext(context.Background(),
			`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID,
		).Scan(&lockedConversationID); clearErr != nil {
			clearDone <- clearErr
			return
		}
		var lockedParticipantID string
		if clearErr = tx.QueryRowContext(context.Background(),
			`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, conversationID, actorID,
		).Scan(&lockedParticipantID); clearErr != nil {
			clearDone <- clearErr
			return
		}
		if _, clearErr = tx.ExecContext(context.Background(), `
			INSERT INTO dm_message_hidden_ranges
				(user_id, conversation_id, hidden_from, hidden_to, includes_own)
			VALUES ($1, $2, '-infinity', NOW(), TRUE)`, actorID, conversationID); clearErr != nil {
			clearDone <- clearErr
			return
		}
		clearDone <- tx.Commit()
	}()

	select {
	case clearPID := <-clearStarted:
		require.Eventually(t, func() bool {
			var waiting bool
			err := h.db.QueryRow(`
				SELECT EXISTS (
					SELECT 1
					FROM pg_stat_activity
					WHERE pid = $1
						AND datname = current_database()
						AND wait_event_type = 'Lock'
						AND query LIKE '%SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE%'
				)`, clearPID).Scan(&waiting)
			return err == nil && waiting
		}, 2*time.Second, 10*time.Millisecond, "Clear must wait for the reaction's conversation lock")
	case clearErr := <-clearDone:
		t.Fatalf("Clear completed before waiting for the reaction aggregate: %v", clearErr)
	}

	var released bool
	require.NoError(t, barrierConn.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, barrierLockClassID, barrierLockObjectID).Scan(&released))
	require.True(t, released)
	barrierHeld = false

	select {
	case result := <-toggleResult:
		require.NoError(t, result.err)
		assert.Equal(t, "added", result.action)
		assert.Equal(t, 1, result.summaryCount, "broadcast aggregate must be captured before Clear commits")
	case <-time.After(2 * time.Second):
		t.Fatal("reaction toggle did not finish after the barrier released")
	}

	select {
	case clearErr := <-clearDone:
		require.NoError(t, clearErr)
	case <-time.After(2 * time.Second):
		t.Fatal("Clear did not finish after the reaction committed")
	}

	peerSummary := h.buildDMReactionSummary(messageID, "👍", peerID)
	require.NotNil(t, peerSummary)
	assert.Equal(t, 1, peerSummary.Count)
	assert.Nil(t, h.buildDMReactionSummary(messageID, "👍", actorID))
}
