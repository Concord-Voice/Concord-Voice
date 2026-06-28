package messages

import (
	"database/sql"
	"log"
	"net/http"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
)

const (
	errMsgInvalidEmoji   = "Invalid emoji"
	errMsgReactionFailed = "Failed to toggle reaction"
	errMsgFetchReactions = "Failed to fetch reactions"
)

// ToggleReactionRequest is the request body for toggling a reaction.
type ToggleReactionRequest struct {
	Emoji string `json:"emoji" binding:"required"`
}

// isValidEmoji checks that the string is a plausible emoji sequence:
// non-empty, at most 32 bytes, no ASCII letters/digits, no control/whitespace,
// and contains at least one non-ASCII rune (standard emoji are non-ASCII).
func isValidEmoji(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	hasNonASCII := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
		if r > 127 {
			hasNonASCII = true
		}
	}
	return hasNonASCII
}

// messageContext holds the resolved location of a message (server channel or DM)
// along with the authorization outcome for the requesting user.
type messageContext struct {
	channelID      string // server-channel messages
	serverID       string // server-channel messages
	conversationID string // DM messages
	isDM           bool
}

// lookupMessageContext resolves a message id to its channel/server (for server
// messages) or DM conversation (for DM messages) and verifies the user is a
// member of the containing server or a participant in the conversation.
//
// On failure it writes the appropriate JSON error to the Gin context and
// returns ok=false. Returns ok=true with a populated messageContext on success.
func (h *Handler) lookupMessageContext(c *gin.Context, messageID, userID string) (messageContext, bool) {
	var ctx messageContext

	// Try server-channel messages first.
	err := h.db.QueryRow(`
		SELECT m.channel_id, c.server_id
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		WHERE m.id = $1
	`, messageID).Scan(&ctx.channelID, &ctx.serverID)
	if err == nil {
		// Check server membership.
		var exists bool
		memErr := h.db.QueryRow(`
			SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)
		`, ctx.serverID, userID).Scan(&exists)
		if memErr != nil {
			h.log.Error("Failed to check server membership", "error", memErr, "server_id", ctx.serverID, "user_id", userID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
			return messageContext{}, false
		}
		if !exists {
			c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
			return messageContext{}, false
		}
		return ctx, true
	}
	if err != sql.ErrNoRows {
		h.log.Error("Failed to look up message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return messageContext{}, false
	}

	// Fall back to DM messages.
	dmErr := h.db.QueryRow(`
		SELECT conversation_id FROM dm_messages WHERE id = $1
	`, messageID).Scan(&ctx.conversationID)
	if dmErr == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return messageContext{}, false
	}
	if dmErr != nil {
		h.log.Error("Failed to look up DM message", "error", dmErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return messageContext{}, false
	}
	ctx.isDM = true

	// Check DM participation.
	var isParticipant bool
	partErr := h.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)
	`, ctx.conversationID, userID).Scan(&isParticipant)
	if partErr != nil {
		h.log.Error("Failed to check DM participation", "error", partErr, "conversation_id", ctx.conversationID, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return messageContext{}, false
	}
	if !isParticipant {
		// Return 404 rather than 403 to avoid leaking that the conversation/message exists.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return messageContext{}, false
	}

	return ctx, true
}

func (h *Handler) rejectActiveMemberTimeout(c *gin.Context, serverID, userID string) bool {
	var timedOutUntil sql.NullTime
	if err := h.db.QueryRow("SELECT timed_out_until FROM server_members WHERE server_id = $1 AND user_id = $2", serverID, userID).Scan(&timedOutUntil); err != nil {
		h.log.Error("Failed to check member timeout", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return true
	}
	if !timedOutUntil.Valid || !timedOutUntil.Time.After(time.Now().UTC()) {
		return false
	}

	c.JSON(http.StatusForbidden, gin.H{
		"error":           "Member is timed out",
		"code":            "member_timed_out",
		"timed_out_until": timedOutUntil.Time,
	})
	return true
}

// ToggleReaction adds or removes a reaction on a message.
func (h *Handler) ToggleReaction(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	var req ToggleReactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if !isValidEmoji(req.Emoji) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidEmoji})
		return
	}

	mctx, ok := h.lookupMessageContext(c, messageID, userID)
	if !ok {
		return
	}
	if mctx.isDM {
		// Reactions on DM messages are not yet supported by this endpoint.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	}
	channelID, serverID := mctx.channelID, mctx.serverID

	// Check PermSendMessages (reacting is a form of communication)
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermSendMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	if h.rejectActiveMemberTimeout(c, serverID, userID) {
		return
	}

	// Toggle: try INSERT, if unique constraint fires → DELETE
	reactionID := uuid.New().String()
	result, err := h.db.Exec(`
		INSERT INTO message_reactions (id, message_id, user_id, emoji)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (message_id, user_id, emoji) DO NOTHING
	`, reactionID, messageID, userID, req.Emoji)
	if err != nil {
		h.log.Error("Failed to insert reaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to get affected rows for reaction insert", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
		return
	}
	action := "added"
	if rowsAffected == 0 {
		// Reaction already existed — remove it
		_, err = h.db.Exec(`
			DELETE FROM message_reactions
			WHERE message_id = $1 AND user_id = $2 AND emoji = $3
		`, messageID, userID, req.Emoji)
		if err != nil {
			h.log.Error("Failed to delete reaction", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgReactionFailed})
			return
		}
		action = "removed"
	}

	// Build updated summary and broadcast + respond
	summary := h.buildReactionSummary(messageID, req.Emoji, userID)
	h.broadcastReaction(channelID, messageID, req.Emoji, userID, action, summary)

	response := gin.H{"action": action}
	if summary != nil {
		response["reaction"] = summary
	}
	c.JSON(http.StatusOK, response)
}

// buildReactionSummary queries the current state of a specific emoji reaction on a message.
// Returns nil if no reactions exist for this emoji.
func (h *Handler) buildReactionSummary(messageID, emoji, currentUserID string) *models.ReactionSummary {
	rows, err := h.db.Query(`
		SELECT mr.user_id, u.username, u.display_name
		FROM message_reactions mr
		INNER JOIN users u ON mr.user_id = u.id
		WHERE mr.message_id = $1 AND mr.emoji = $2
		ORDER BY mr.created_at ASC
	`, messageID, emoji)
	if err != nil {
		h.log.Error("Failed to query reaction summary", "error", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	summary := &models.ReactionSummary{
		Emoji: emoji,
		Users: []models.ReactionUser{},
	}

	for rows.Next() {
		var user models.ReactionUser
		if err := rows.Scan(&user.UserID, &user.Username, &user.DisplayName); err != nil {
			continue
		}
		summary.Users = append(summary.Users, user)
		summary.Count++
		if user.UserID == currentUserID {
			summary.Me = true
		}
	}

	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating reaction summary rows", "error", err)
		return nil
	}

	if summary.Count == 0 {
		return nil
	}
	return summary
}

// broadcastReaction sends a reaction event to all channel subscribers.
func (h *Handler) broadcastReaction(channelID, messageID, emoji, userID, action string, summary *models.ReactionSummary) {
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	eventType := "message_reaction_added"
	if action == "removed" {
		eventType = "message_reaction_removed"
	}

	eventData := map[string]interface{}{
		"message_id": messageID,
		"channel_id": channelID,
		"emoji":      emoji,
		"user_id":    userID,
	}
	if summary != nil {
		eventData["reaction_summary"] = summary
	}

	h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
		Type: eventType,
		Data: eventData,
	})
}

// GetReactions returns all reactions for a message, grouped by emoji.
func (h *Handler) GetReactions(c *gin.Context) {
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
		// Reactions on DM messages are not yet supported by this endpoint.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	}
	channelID, serverID := mctx.channelID, mctx.serverID

	// Check PermReadMessageHistory
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermReadMessageHistory)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchReactions})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	rows, err := h.db.Query(`
		SELECT mr.emoji, mr.user_id, u.username, u.display_name
		FROM message_reactions mr
		INNER JOIN users u ON mr.user_id = u.id
		WHERE mr.message_id = $1
		ORDER BY mr.emoji, mr.created_at ASC
	`, messageID)
	if err != nil {
		h.log.Error("Failed to query reactions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchReactions})
		return
	}
	defer func() { _ = rows.Close() }()

	// Group by emoji
	summaryMap := make(map[string]*models.ReactionSummary)
	var order []string

	for rows.Next() {
		var emoji, rUserID, username string
		var displayName *string
		if err := rows.Scan(&emoji, &rUserID, &username, &displayName); err != nil {
			continue
		}

		s, exists := summaryMap[emoji]
		if !exists {
			s = &models.ReactionSummary{
				Emoji: emoji,
				Users: []models.ReactionUser{},
			}
			summaryMap[emoji] = s
			order = append(order, emoji)
		}
		s.Users = append(s.Users, models.ReactionUser{
			UserID:      rUserID,
			Username:    username,
			DisplayName: displayName,
		})
		s.Count++
		if rUserID == userID {
			s.Me = true
		}
	}

	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating reaction rows", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchReactions})
		return
	}

	reactions := make([]models.ReactionSummary, 0, len(order))
	for _, emoji := range order {
		reactions = append(reactions, *summaryMap[emoji])
	}

	c.JSON(http.StatusOK, gin.H{"reactions": reactions})
}

// loadReactionsForMessages batch-loads reaction summaries for a set of messages.
// Returns a map from message ID to its reaction summaries. Avoids N+1 queries.
func loadReactionsForMessages(db *sql.DB, messageIDs []string, currentUserID string) (map[string][]models.ReactionSummary, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT mr.message_id, mr.emoji, mr.user_id, u.username, u.display_name
		FROM message_reactions mr
		INNER JOIN users u ON mr.user_id = u.id
		WHERE mr.message_id = ANY($1::uuid[])
		ORDER BY mr.message_id, mr.emoji, mr.created_at ASC
	`, pq.Array(messageIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Accumulate: messageID → emoji → *ReactionSummary
	type key struct{ msgID, emoji string }
	summaries := make(map[key]*models.ReactionSummary)
	orderByMsg := make(map[string][]string) // messageID → ordered emoji list

	for rows.Next() {
		var msgID, emoji, rUserID, username string
		var displayName *string
		if err := rows.Scan(&msgID, &emoji, &rUserID, &username, &displayName); err != nil {
			continue
		}

		k := key{msgID, emoji}
		s, exists := summaries[k]
		if !exists {
			s = &models.ReactionSummary{
				Emoji: emoji,
				Users: []models.ReactionUser{},
			}
			summaries[k] = s
			orderByMsg[msgID] = append(orderByMsg[msgID], emoji)
		}
		s.Users = append(s.Users, models.ReactionUser{
			UserID:      rUserID,
			Username:    username,
			DisplayName: displayName,
		})
		s.Count++
		if rUserID == currentUserID {
			s.Me = true
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Convert to result map
	result := make(map[string][]models.ReactionSummary, len(orderByMsg))
	for msgID, emojis := range orderByMsg {
		reactionList := make([]models.ReactionSummary, 0, len(emojis))
		for _, emoji := range emojis {
			reactionList = append(reactionList, *summaries[key{msgID, emoji}])
		}
		result[msgID] = reactionList
	}

	return result, nil
}

// collectReplyToIDs returns the distinct reply_to_id values from a slice of messages.
func collectReplyToIDs(messages []models.MessageWithUser) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, m := range messages {
		if m.ReplyToID != nil && !seen[*m.ReplyToID] {
			ids = append(ids, *m.ReplyToID)
			seen[*m.ReplyToID] = true
		}
	}
	return ids
}

// loadRepliedToForMessages batch-loads reply-to summaries for messages that have reply_to_id set.
// Returns a map from the reply message's ID to its replied-to summary. Avoids N+1 queries.
// Replies whose reply_to_id is nil are ignored. If a replied-to message cannot be found
// (e.g. it no longer exists or a load error occurred), that reply will not have an entry
// in the returned map.
func loadRepliedToForMessages(db *sql.DB, messages []models.MessageWithUser) (map[string]*models.RepliedToSummary, error) {
	replyIDs := collectReplyToIDs(messages)
	if len(replyIDs) == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT m.id, m.user_id, u.username, u.display_name, m.content, COALESCE(m.key_version, 1)
		FROM messages m
		INNER JOIN users u ON m.user_id = u.id
		WHERE m.id = ANY($1::uuid[])
	`, pq.Array(replyIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	summaries := make(map[string]*models.RepliedToSummary)
	for rows.Next() {
		var s models.RepliedToSummary
		if err := rows.Scan(&s.ID, &s.UserID, &s.Username, &s.DisplayName, &s.Content, &s.KeyVersion); err != nil {
			log.Printf("Failed to scan replied-to summary row: %v", err)
			continue
		}
		summaries[s.ID] = &s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Map from the reply message's ID → its replied-to summary
	result := make(map[string]*models.RepliedToSummary)
	for _, m := range messages {
		if m.ReplyToID != nil {
			if s, ok := summaries[*m.ReplyToID]; ok {
				result[m.ID] = s
			}
		}
	}

	return result, nil
}
