package messages

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		h.unpinDMMessage(c, messageID, actorID, ctx.conversationID)
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
		h.unpinDMMessage(c, messageID, actorID, ctx.conversationID)
		assert.Equal(t, http.StatusNotFound, w.Code)
		var pinnedAt sql.NullTime
		require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT pinned_at FROM dm_messages WHERE id = $1`, messageID).Scan(&pinnedAt))
		assert.False(t, pinnedAt.Valid)
	})
}
