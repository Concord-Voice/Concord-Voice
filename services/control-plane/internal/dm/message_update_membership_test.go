package dm

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateDMMessageCiphertextRechecksMembershipAndAuthor exercises the
// transaction-side authorization used after the conversation lock. The
// endpoint preflight cannot cover a participant removed after that check.
func TestUpdateDMMessageCiphertextRechecksMembershipAndAuthor(t *testing.T) {
	t.Run("removed participant cannot edit and ciphertext is unchanged", func(t *testing.T) {
		db := hiddenTestDB(t)
		convID, actor, _ := seedHiddenConv(t, db)
		messageID := insertUpdateTestMessage(t, db, convID, actor, "original")
		_, err := db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, actor)
		require.NoError(t, err)

		result, recorder := invokeDMMessageCiphertextUpdate(t, db, convID, messageID, actor, "replacement")

		assert.False(t, result)
		assert.Equal(t, http.StatusNotFound, recorder.Code)
		assert.Equal(t, "original", storedUpdateTestMessage(t, db, messageID))
	})

	t.Run("active author can edit", func(t *testing.T) {
		db := hiddenTestDB(t)
		convID, actor, _ := seedHiddenConv(t, db)
		messageID := insertUpdateTestMessage(t, db, convID, actor, "original")

		result, recorder := invokeDMMessageCiphertextUpdate(t, db, convID, messageID, actor, "replacement")

		assert.True(t, result)
		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "replacement", storedUpdateTestMessage(t, db, messageID))
	})

	t.Run("active non-author cannot edit and ciphertext is unchanged", func(t *testing.T) {
		db := hiddenTestDB(t)
		convID, actor, peer := seedHiddenConv(t, db)
		messageID := insertUpdateTestMessage(t, db, convID, actor, "original")

		result, recorder := invokeDMMessageCiphertextUpdate(t, db, convID, messageID, peer, "replacement")

		assert.False(t, result)
		assert.Equal(t, http.StatusNotFound, recorder.Code)
		assert.Equal(t, "original", storedUpdateTestMessage(t, db, messageID))
	})
}

func insertUpdateTestMessage(t *testing.T, db *sql.DB, convID, userID, content string) string {
	t.Helper()
	var messageID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type)
		VALUES ($1, $2, $3, 'text') RETURNING id`, convID, userID, content).Scan(&messageID))
	return messageID
}

func storedUpdateTestMessage(t *testing.T, db *sql.DB, messageID string) string {
	t.Helper()
	var content string
	require.NoError(t, db.QueryRow(`SELECT content FROM dm_messages WHERE id = $1`, messageID).Scan(&content))
	return content
}

func invokeDMMessageCiphertextUpdate(t *testing.T, db *sql.DB, convID, messageID, userID, content string) (bool, *httptest.ResponseRecorder) {
	t.Helper()
	h := NewHandler(HandlerDeps{DB: db, Log: logger.NewWithWriter(io.Discard)})
	c, recorder := testCtx()
	c.Set("user_id", userID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": ""})

	_, updated := h.updateDMMessageCiphertext(c, convID, messageID, updateDMMessageRequest{Content: content, KeyVersion: 1})
	return updated, recorder
}
