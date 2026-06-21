package messages

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
)

const (
	errMsgPinFailed       = "Failed to pin message"
	errMsgUnpinFailed     = "Failed to unpin message"
	errMsgFetchPinsFailed = "Failed to fetch pinned messages"
	errMsgPinLimitReached = "Maximum of 50 pinned messages per channel"
	maxPinsPerChannel     = 50
)

// PinMessage pins a message in its channel or DM conversation.
// For server channels, requires PermPinMessages. For DM conversations,
// any participant may pin.
func (h *Handler) PinMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	mctx, ok := h.lookupMessageContext(c, messageID, userID)
	if !ok {
		return
	}

	if mctx.isDM {
		h.pinDMMessage(c, messageID, userID, mctx.conversationID)
		return
	}

	channelID, serverID := mctx.channelID, mctx.serverID
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermPinMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Pin with 50-message limit check via UPDATE subquery.
	// NOTE: Under high concurrency, two simultaneous requests could both see
	// COUNT(*) < 50 and succeed. For production at scale, consider wrapping in
	// a transaction with an advisory lock keyed by channel_id.
	var pinnedAt time.Time
	var pinnedBy string
	err := h.db.QueryRow(`
		UPDATE messages
		SET pinned_at = NOW(), pinned_by = $1, updated_at = NOW()
		WHERE id = $2 AND pinned_at IS NULL
		  AND (SELECT COUNT(*) FROM messages WHERE channel_id = $3 AND pinned_at IS NOT NULL) < $4
		RETURNING pinned_at, pinned_by
	`, userID, messageID, channelID, maxPinsPerChannel).Scan(&pinnedAt, &pinnedBy)

	if err == sql.ErrNoRows {
		// Disambiguate: already pinned vs limit reached
		var existingPinnedAt *time.Time
		var existingPinnedBy *string
		checkErr := h.db.QueryRow(`SELECT pinned_at, pinned_by FROM messages WHERE id = $1`, messageID).Scan(&existingPinnedAt, &existingPinnedBy)
		if checkErr != nil {
			h.log.Error(errMsgPinFailed, "error", checkErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
			return
		}
		if existingPinnedAt != nil {
			c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": existingPinnedAt, "pinned_by": existingPinnedBy, "already_pinned": true})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": errMsgPinLimitReached})
		return
	}
	if err != nil {
		h.log.Error(errMsgPinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}

	// Broadcast to channel
	broadcastPin(h.hub, channelID, messageID, pinnedAt, pinnedBy)

	c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": pinnedAt, "pinned_by": pinnedBy})
}

// pinDMMessage handles the DM branch of PinMessage. The caller has already
// verified conversation participation via lookupMessageContext.
func (h *Handler) pinDMMessage(c *gin.Context, messageID, userID, conversationID string) {
	var pinnedAt time.Time
	var pinnedBy string
	err := h.db.QueryRow(`
		UPDATE dm_messages
		SET pinned_at = NOW(), pinned_by = $1, updated_at = NOW()
		WHERE id = $2 AND pinned_at IS NULL
		  AND (SELECT COUNT(*) FROM dm_messages WHERE conversation_id = $3 AND pinned_at IS NOT NULL) < $4
		RETURNING pinned_at, pinned_by
	`, userID, messageID, conversationID, maxPinsPerChannel).Scan(&pinnedAt, &pinnedBy)

	if err == sql.ErrNoRows {
		var existingPinnedAt *time.Time
		var existingPinnedBy *string
		checkErr := h.db.QueryRow(`SELECT pinned_at, pinned_by FROM dm_messages WHERE id = $1`, messageID).Scan(&existingPinnedAt, &existingPinnedBy)
		if checkErr != nil {
			h.log.Error(errMsgPinFailed, "error", checkErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
			return
		}
		if existingPinnedAt != nil {
			c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": existingPinnedAt, "pinned_by": existingPinnedBy, "already_pinned": true})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": errMsgPinLimitReached})
		return
	}
	if err != nil {
		h.log.Error(errMsgPinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}

	// Best-effort DM broadcast: reuse the channel broadcast keyed by the
	// conversation id (the WS hub treats DM conversations as "channels" on
	// the wire for purposes of pin events).
	broadcastPin(h.hub, conversationID, messageID, pinnedAt, pinnedBy)

	c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": pinnedAt, "pinned_by": pinnedBy, "conversation_id": conversationID})
}

// UnpinMessage unpins a message. Requires PermPinMessages.
func (h *Handler) UnpinMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	mctx, ok := h.lookupMessageContext(c, messageID, userID)
	if !ok {
		return
	}

	if mctx.isDM {
		h.unpinDMMessage(c, messageID, mctx.conversationID)
		return
	}

	channelID, serverID := mctx.channelID, mctx.serverID
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermPinMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	result, err := h.db.Exec(`
		UPDATE messages SET pinned_at = NULL, pinned_by = NULL, updated_at = NOW()
		WHERE id = $1 AND pinned_at IS NOT NULL
	`, messageID)
	if err != nil {
		h.log.Error(errMsgUnpinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}

	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		h.log.Error("Failed to get affected rows for unpin", "error", raErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	if rowsAffected == 0 {
		// Already unpinned — idempotent success
		c.JSON(http.StatusOK, gin.H{"message_id": messageID, "already_unpinned": true})
		return
	}

	// Broadcast to channel
	channelUUID, parseErr := uuid.Parse(channelID)
	if parseErr == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_unpinned",
			Data: map[string]interface{}{
				"message_id": messageID,
				"channel_id": channelID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message_id": messageID})
}

// unpinDMMessage handles the DM branch of UnpinMessage. The caller has already
// verified conversation participation via lookupMessageContext.
func (h *Handler) unpinDMMessage(c *gin.Context, messageID, conversationID string) {
	result, err := h.db.Exec(`
		UPDATE dm_messages SET pinned_at = NULL, pinned_by = NULL, updated_at = NOW()
		WHERE id = $1 AND pinned_at IS NOT NULL
	`, messageID)
	if err != nil {
		h.log.Error(errMsgUnpinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		h.log.Error("Failed to get affected rows for DM unpin", "error", raErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusOK, gin.H{"message_id": messageID, "already_unpinned": true})
		return
	}

	if convUUID, parseErr := uuid.Parse(conversationID); parseErr == nil {
		h.hub.BroadcastToChannel(convUUID, websocket.OutgoingMessage{
			Type: "message_unpinned",
			Data: map[string]interface{}{
				"message_id":      messageID,
				"conversation_id": conversationID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message_id": messageID, "conversation_id": conversationID})
}

// GetChannelPins returns all pinned messages for a server channel or DM
// conversation. The URL parameter is either a channel id or a DM conversation
// id — the handler resolves whichever exists.
func (h *Handler) GetChannelPins(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Does a server channel with this id exist?
	var serverChannelExists bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1)`,
		channelID,
	).Scan(&serverChannelExists); err != nil {
		h.log.Error("Failed to check channel existence", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}

	if !serverChannelExists {
		h.getDMConversationPins(c, channelID, userID)
		return
	}

	if _, ok := h.checkChannelAccess(c, channelID, userID); !ok {
		return
	}

	rows, err := h.db.Query(`
		SELECT m.id, m.channel_id, m.user_id, m.content, COALESCE(m.key_version, 1),
		       m.embeds_suppressed, m.reply_to_id, m.pinned_at, m.pinned_by, m.edited_at, m.created_at, m.updated_at,
		       u.username, u.display_name, u.avatar_url
		FROM messages m
		INNER JOIN users u ON m.user_id = u.id
		WHERE m.channel_id = $1 AND m.pinned_at IS NOT NULL
		ORDER BY m.pinned_at DESC
	`, channelID)
	if err != nil {
		h.log.Error(errMsgFetchPinsFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}
	defer func() { _ = rows.Close() }()

	messages := []models.MessageWithUser{}
	for rows.Next() {
		var msg models.MessageWithUser
		scanErr := rows.Scan(
			&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content, &msg.KeyVersion,
			&msg.EmbedsSuppressed, &msg.ReplyToID, &msg.PinnedAt, &msg.PinnedBy, &msg.EditedAt,
			&msg.CreatedAt, &msg.UpdatedAt, &msg.Username, &msg.DisplayName, &msg.AvatarURL,
		)
		if scanErr != nil {
			h.log.Error("Failed to scan pinned message row", "error", scanErr)
			continue
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating pinned messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}

	h.enrichMessages(messages, userID)

	c.JSON(http.StatusOK, gin.H{"pinned_messages": messages, "count": len(messages)})
}

// dmPinnedMessage is the response shape for a single pinned DM message.
type dmPinnedMessage struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	UserID         string     `json:"user_id"`
	Content        string     `json:"content"`
	Type           string     `json:"type"`
	PinnedAt       *time.Time `json:"pinned_at"`
	PinnedBy       *string    `json:"pinned_by"`
	EditedAt       *time.Time `json:"edited_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Username       string     `json:"username"`
	DisplayName    *string    `json:"display_name,omitempty"`
	AvatarURL      *string    `json:"avatar_url,omitempty"`
}

// getDMConversationPins returns all pinned messages for a DM conversation.
// Authorization: the requester must be a participant of the conversation.
func (h *Handler) getDMConversationPins(c *gin.Context, conversationID, userID string) {
	// Confirm the conversation exists and the requester is a participant.
	var isParticipant bool
	if err := h.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)
	`, conversationID, userID).Scan(&isParticipant); err != nil {
		h.log.Error("Failed to check DM participation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}
	if !isParticipant {
		// Return 404 regardless of whether the conversation exists, so
		// non-participants cannot probe conversation IDs.
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
		return
	}

	rows, err := h.db.Query(`
		SELECT dm.id, dm.conversation_id, dm.user_id, dm.content, dm.type,
		       dm.pinned_at, dm.pinned_by, dm.edited_at, dm.created_at, dm.updated_at,
		       u.username, u.display_name, u.avatar_url
		FROM dm_messages dm
		INNER JOIN users u ON dm.user_id = u.id
		WHERE dm.conversation_id = $1 AND dm.pinned_at IS NOT NULL
		ORDER BY dm.pinned_at DESC
	`, conversationID)
	if err != nil {
		h.log.Error(errMsgFetchPinsFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}
	defer func() { _ = rows.Close() }()

	pinned := []dmPinnedMessage{}
	for rows.Next() {
		var m dmPinnedMessage
		if scanErr := rows.Scan(
			&m.ID, &m.ConversationID, &m.UserID, &m.Content, &m.Type,
			&m.PinnedAt, &m.PinnedBy, &m.EditedAt, &m.CreatedAt, &m.UpdatedAt,
			&m.Username, &m.DisplayName, &m.AvatarURL,
		); scanErr != nil {
			h.log.Error("Failed to scan pinned DM message row", "error", scanErr)
			continue
		}
		pinned = append(pinned, m)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating pinned DM messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}

	c.JSON(http.StatusOK, gin.H{"pinned_messages": pinned, "count": len(pinned), "conversation_id": conversationID})
}

// broadcastPin sends a message_pinned event to all channel subscribers.
func broadcastPin(hub *websocket.Hub, channelID, messageID string, pinnedAt time.Time, pinnedBy string) {
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}
	hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
		Type: "message_pinned",
		Data: map[string]interface{}{
			"message_id": messageID,
			"channel_id": channelID,
			"pinned_at":  pinnedAt,
			"pinned_by":  pinnedBy,
		},
	})
}
