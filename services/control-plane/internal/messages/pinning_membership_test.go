package messages

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pinBranchContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, w
}

func TestDMPinBranchesRecheckParticipantAfterContextLookup(t *testing.T) {
	h := newReactionHelperHandler(t)

	t.Run("pin mutation", func(t *testing.T) {
		messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
		c, _ := pinBranchContext()
		ctx, ok := h.lookupMessageContext(c, messageID, actorID)
		require.True(t, ok)
		_, err := h.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, actorID)
		require.NoError(t, err)
		c, w := pinBranchContext()
		h.pinDMMessage(c, messageID, actorID, ctx.conversationID)
		assert.Equal(t, http.StatusNotFound, w.Code)
		var pinnedAt sql.NullTime
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt))
		assert.False(t, pinnedAt.Valid)
	})

	t.Run("pin already-pinned fallback", func(t *testing.T) {
		messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
		c, _ := pinBranchContext()
		ctx, ok := h.lookupMessageContext(c, messageID, actorID)
		require.True(t, ok)
		_, err := h.db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $1 WHERE id = $2`, actorID, messageID)
		require.NoError(t, err)
		var beforeAt time.Time
		var beforeBy string
		require.NoError(t, h.db.QueryRow(`SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, messageID).Scan(&beforeAt, &beforeBy))
		_, err = h.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, actorID)
		require.NoError(t, err)
		c, w := pinBranchContext()
		h.pinDMMessage(c, messageID, actorID, ctx.conversationID)
		assert.Equal(t, http.StatusNotFound, w.Code)
		var pinnedAt time.Time
		var pinnedBy string
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt, &pinnedBy))
		assert.Equal(t, beforeAt, pinnedAt)
		assert.Equal(t, beforeBy, pinnedBy)
	})

	t.Run("unpin mutation", func(t *testing.T) {
		messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
		c, _ := pinBranchContext()
		ctx, ok := h.lookupMessageContext(c, messageID, actorID)
		require.True(t, ok)
		_, err := h.db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $1 WHERE id = $2`, actorID, messageID)
		require.NoError(t, err)
		var beforeAt time.Time
		var beforeBy string
		require.NoError(t, h.db.QueryRow(`SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, messageID).Scan(&beforeAt, &beforeBy))
		_, err = h.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, actorID)
		require.NoError(t, err)
		c, w := pinBranchContext()
		h.unpinDMMessage(c, messageID, ctx.conversationID)
		assert.Equal(t, http.StatusNotFound, w.Code)
		var pinnedAt time.Time
		var pinnedBy string
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt, &pinnedBy))
		assert.Equal(t, beforeAt, pinnedAt)
		assert.Equal(t, beforeBy, pinnedBy)
	})

	t.Run("unpin already-unpinned fallback", func(t *testing.T) {
		messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
		c, _ := pinBranchContext()
		ctx, ok := h.lookupMessageContext(c, messageID, actorID)
		require.True(t, ok)
		_, err := h.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, actorID)
		require.NoError(t, err)
		c, w := pinBranchContext()
		h.unpinDMMessage(c, messageID, ctx.conversationID)
		assert.Equal(t, http.StatusNotFound, w.Code)
		var pinnedAt sql.NullTime
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt))
		assert.False(t, pinnedAt.Valid)
	})
}

// TestDMPinClearAfterLookupCannotMutateHiddenMessage pins the transaction-time
// visibility fence: a Clear committed after request lookup must invalidate a
// pin of the message it hides. A pinned message is never hidden (#3458 §18), so
// the same Clear leaves an unpin of a pinned message in force.
func TestDMPinClearAfterLookupCannotMutateHiddenMessage(t *testing.T) {
	h := newReactionHelperHandler(t)
	h.hub = websocket.NewHub(nil, nil)

	t.Run("pin", func(t *testing.T) {
		messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
		c, _ := pinBranchContext()
		_, ok := h.lookupMessageContext(c, messageID, actorID)
		require.True(t, ok)
		_, err := h.db.Exec(`
			INSERT INTO dm_message_hidden_ranges
				(user_id, conversation_id, hidden_from, hidden_to, includes_own)
			VALUES ($1, $2, '-infinity', NOW() + INTERVAL '1 minute', TRUE)`, actorID, conversationID)
		require.NoError(t, err)

		c, w := pinBranchContext()
		c.Set("user_id", actorID)
		h.pinDMMessage(c, messageID, actorID, conversationID)

		assert.Equal(t, http.StatusNotFound, w.Code, "Clear committed after lookup must invalidate the pin mutation")
		var pinnedAt sql.NullTime
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt))
		assert.False(t, pinnedAt.Valid, "hidden history was mutated by the stale pin request")
	})

	t.Run("unpin of a pinned message stays in force", func(t *testing.T) {
		messageID, actorID, _, conversationID := seedDMReactionForAggregateTest(t, h)
		_, err := h.db.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $1 WHERE id = $2`, actorID, messageID)
		require.NoError(t, err)
		c, _ := pinBranchContext()
		_, ok := h.lookupMessageContext(c, messageID, actorID)
		require.True(t, ok)
		_, err = h.db.Exec(`
			INSERT INTO dm_message_hidden_ranges
				(user_id, conversation_id, hidden_from, hidden_to, includes_own)
			VALUES ($1, $2, '-infinity', NOW() + INTERVAL '1 minute', TRUE)`, actorID, conversationID)
		require.NoError(t, err)

		c, w := pinBranchContext()
		c.Set("user_id", actorID)
		h.unpinDMMessage(c, messageID, conversationID)

		assert.Equal(t, http.StatusOK, w.Code, "a Clear cannot hide a pinned message, so it cannot invalidate its unpin")
		var after sql.NullTime
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at FROM dm_messages WHERE id = $1`, messageID).Scan(&after))
		assert.False(t, after.Valid, "the unpin of a visible pinned message must apply")
	})
}
