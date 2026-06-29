package messages

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newReactionHelperHandler(t *testing.T) *Handler {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL required for reaction helper coverage")
	}

	db, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })

	return NewHandler(db, logger.New("test"), nil, nil)
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

	action, err := h.toggleDMReactionRow(msgID, userID, thumbsUp)

	assert.Empty(t, action)
	require.Error(t, err)

	var count int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM dm_message_reactions WHERE message_id = $1 AND user_id = $2`, msgID, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}
