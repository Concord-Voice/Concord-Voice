// Package messages provides handlers for managing chat messages.
package messages

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/klipy"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgInvalidRequestBody     = "Invalid request body"
	errMsgInvalidMessageID       = "Invalid message ID"
	errMsgInsufficientPerms      = "Insufficient permissions"
	errMsgMessageNotFound        = "Message not found"
	errMsgFailedCheckPerms       = "Failed to check permissions"
	errMsgFailedFetchMessages    = "Failed to fetch messages"
	errMsgFailedSendMessage      = "Failed to send message"
	errMsgFailedUpdateMessage    = "Failed to update message"
	errMsgFailedDeleteMessage    = "Failed to delete message"
	errMsgFailedSuppressEmbeds   = "Failed to suppress embeds"
	errMsgInsufficientPermsLower = "insufficient permissions"
	errMsgNotMember              = "Not a member of this channel's server"
	errMsgFailedCheckMembership  = "Failed to check membership"
	errMsgInvalidCiphertext      = "Invalid ciphertext format for E2EE channel"
)

// minCiphertextSize is the minimum base64-decoded size for a valid AES-GCM ciphertext:
// 12 bytes IV + 16 bytes auth tag = 28 bytes minimum (empty plaintext).
const minCiphertextSize = 28

// isValidCiphertext checks that content is valid base64 and meets the minimum
// size for an AES-256-GCM ciphertext (12-byte IV + 16-byte auth tag).
func isValidCiphertext(content string) bool {
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return false
	}
	return len(decoded) >= minCiphertextSize
}

// Handler handles message-related requests
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
}

// NewHandler creates a new message handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		hub:      hub,
		resolver: resolver,
	}
}

// SendMessageRequest represents a request to send a message.
// Max content length is 65536 bytes (64 KiB) of ciphertext — sized for a future
// 10,240-char paid-tier message under worst-case CJK UTF-8 (3 bytes/char) plus
// AES-GCM + base64 envelope, with ~60% headroom for envelope evolution.
type SendMessageRequest struct {
	ChannelID   string  `json:"channel_id" binding:"required,uuid"`
	Content     string  `json:"content" binding:"required,min=1,max=65536"`
	KeyVersion  int     `json:"key_version"`
	ReplyToID   *string `json:"reply_to_id,omitempty"`  // Optional: UUID of message being replied to (must be same channel)
	MentionMeta string  `json:"mention_meta,omitempty"` // Accepted but unused — mention routing is WebSocket-only. Field exists so REST clients don't get a 400 for including it.
	GifSlug     *string `json:"gif_slug,omitempty"`     // Optional: KLIPY GIF slug to embed in the message
}

// UpdateMessageRequest represents a request to update a message.
// Max content length is 65536 bytes (64 KiB) — matches SendMessageRequest.
type UpdateMessageRequest struct {
	Content string `json:"content" binding:"required,min=1,max=65536"`
}

// isFKViolation returns true if the error is a PostgreSQL foreign key violation (23503).
func isFKViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23503"
}

// checkChannelAccess validates channel ID, checks membership, and verifies PermReadMessageHistory.
// Returns (serverID, ok). On failure, writes the JSON error to c.
func (h *Handler) checkChannelAccess(c *gin.Context, channelID, userID string) (string, bool) {
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return "", false
	}

	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`, channelID, userID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return "", false
	}
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return "", false
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermReadMessageHistory)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return "", false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return "", false
	}

	return serverID, true
}

// parsePagination extracts and validates limit + before cursor from query params.
func parsePagination(c *gin.Context) (limit int, before string, ok bool) {
	limit = 50
	if limitParam := c.Query("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}
	before = c.Query("before")
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid 'before' parameter"})
			return 0, "", false
		}
	}
	return limit, before, true
}

// parseBulkPagination is like parsePagination but with a higher default and max limit (200)
// for the search backfill bulk endpoint.
func parseBulkPagination(c *gin.Context) (limit int, before string, ok bool) {
	limit = 200
	if limitParam := c.Query("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 && l <= 200 {
			limit = l
		}
	}
	before = c.Query("before")
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid 'before' parameter"})
			return 0, "", false
		}
	}
	return limit, before, true
}

// GetMessagesBulk returns message history with a larger page size (200) for search backfill.
func (h *Handler) GetMessagesBulk(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, ok := h.checkChannelAccess(c, channelID, userID); !ok {
		return
	}

	limit, before, ok := parseBulkPagination(c)
	if !ok {
		return
	}

	messages, err := h.queryMessages(channelID, before, limit)
	if err != nil {
		h.log.Error("Failed to query messages (bulk)", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}

	h.enrichMessages(messages, userID)

	c.JSON(http.StatusOK, gin.H{
		"messages": messages,
		"count":    len(messages),
	})
}

// enrichMessages batch-loads reactions and replied-to summaries onto messages (non-fatal on failure).
func (h *Handler) enrichMessages(messages []models.MessageWithUser, userID string) {
	if len(messages) == 0 {
		return
	}
	h.attachReactions(messages, userID)
	h.attachRepliedTo(messages)
	h.attachAttachments(messages)
}

// attachAttachments batch-loads and attaches file attachment summaries to messages.
func (h *Handler) attachAttachments(messages []models.MessageWithUser) {
	messageIDs := make([]string, len(messages))
	for i, m := range messages {
		messageIDs[i] = m.ID
	}
	attachmentMap, err := loadAttachmentsForMessages(h.db, messageIDs)
	if err != nil {
		h.log.Error("Failed to load attachments", "error", err)
		return
	}
	for i := range messages {
		if attachments, ok := attachmentMap[messages[i].ID]; ok {
			messages[i].Attachments = attachments
		}
	}
}

// attachReactions batch-loads and attaches reaction summaries to messages.
func (h *Handler) attachReactions(messages []models.MessageWithUser, userID string) {
	messageIDs := make([]string, len(messages))
	for i, m := range messages {
		messageIDs[i] = m.ID
	}
	reactionMap, err := loadReactionsForMessages(h.db, messageIDs, userID)
	if err != nil {
		h.log.Error("Failed to load reactions", "error", err)
		return
	}
	for i := range messages {
		if reactions, ok := reactionMap[messages[i].ID]; ok {
			messages[i].Reactions = reactions
		}
	}
}

// attachRepliedTo batch-loads and attaches replied-to summaries to messages.
func (h *Handler) attachRepliedTo(messages []models.MessageWithUser) {
	replyMap, err := loadRepliedToForMessages(h.db, messages)
	if err != nil {
		h.log.Error("Failed to load replied-to summaries", "error", err)
		return
	}
	if replyMap == nil {
		return
	}
	for i := range messages {
		if summary, ok := replyMap[messages[i].ID]; ok {
			messages[i].RepliedTo = summary
		}
	}
}

// GetMessages returns message history for a channel
func (h *Handler) GetMessages(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, ok := h.checkChannelAccess(c, channelID, userID); !ok {
		return
	}

	limit, before, ok := parsePagination(c)
	if !ok {
		return
	}

	messages, err := h.queryMessages(channelID, before, limit)
	if err != nil {
		h.log.Error("Failed to query messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}

	h.enrichMessages(messages, userID)

	c.JSON(http.StatusOK, gin.H{
		"messages": messages,
		"count":    len(messages),
	})
}

// queryMessages fetches messages for a channel with optional cursor-based pagination.
func (h *Handler) queryMessages(channelID, before string, limit int) ([]models.MessageWithUser, error) {
	var query string
	var args []interface{}

	if before != "" {
		query = `
			SELECT m.id, m.channel_id, m.user_id, m.content, COALESCE(m.key_version, 1),
			       m.embeds_suppressed, m.reply_to_id, m.pinned_at, m.pinned_by, m.edited_at, m.created_at, m.updated_at,
			       u.username, u.display_name, u.avatar_url
			FROM messages m
			INNER JOIN users u ON m.user_id = u.id
			WHERE m.channel_id = $1
			  AND m.created_at < (SELECT created_at FROM messages WHERE id = $2)
			ORDER BY m.created_at DESC
			LIMIT $3
		`
		args = []interface{}{channelID, before, limit}
	} else {
		query = `
			SELECT m.id, m.channel_id, m.user_id, m.content, COALESCE(m.key_version, 1),
			       m.embeds_suppressed, m.reply_to_id, m.pinned_at, m.pinned_by, m.edited_at, m.created_at, m.updated_at,
			       u.username, u.display_name, u.avatar_url
			FROM messages m
			INNER JOIN users u ON m.user_id = u.id
			WHERE m.channel_id = $1
			ORDER BY m.created_at DESC
			LIMIT $2
		`
		args = []interface{}{channelID, limit}
	}

	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	messages := []models.MessageWithUser{}
	for rows.Next() {
		var msg models.MessageWithUser
		scanErr := rows.Scan(
			&msg.ID,
			&msg.ChannelID,
			&msg.UserID,
			&msg.Content,
			&msg.KeyVersion,
			&msg.EmbedsSuppressed,
			&msg.ReplyToID,
			&msg.PinnedAt,
			&msg.PinnedBy,
			&msg.EditedAt,
			&msg.CreatedAt,
			&msg.UpdatedAt,
			&msg.Username,
			&msg.DisplayName,
			&msg.AvatarURL,
		)
		if scanErr != nil {
			h.log.Error("Failed to scan message row", "error", scanErr)
			continue
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// validateReplyToID checks that a reply_to_id is a valid UUID referencing a message
// in the given channel. Returns the validated pointer and whether to continue.
// On validation failure, writes the appropriate JSON error to c.
func (h *Handler) validateReplyToID(c *gin.Context, replyToID *string, channelID string) (*string, bool) {
	if replyToID == nil || *replyToID == "" {
		return nil, true
	}
	if _, err := uuid.Parse(*replyToID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reply_to_id"})
		return nil, false
	}
	var replyChannelUUID uuid.UUID
	err := h.db.QueryRow(`SELECT channel_id FROM messages WHERE id = $1`, *replyToID).Scan(&replyChannelUUID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Reply target message not found"})
		return nil, false
	}
	if err != nil {
		h.log.Error("Failed to validate reply_to_id", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return nil, false
	}
	parsedChannelID, _ := uuid.Parse(channelID)
	if replyChannelUUID != parsedChannelID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Reply target must be in the same channel"})
		return nil, false
	}
	return replyToID, true
}

// checkSendAccess validates membership, PermSendMessages, and fetches the embed policy.
// Returns (serverID, allowEmbeds, ok). On failure, writes the JSON error to c.
func (h *Handler) checkSendAccess(c *gin.Context, channelID, userID string) (string, bool, bool) {
	var serverID string
	var serverAllowEmbeds bool
	var timedOutUntil sql.NullTime
	err := h.db.QueryRow(`
		SELECT c.server_id, s.allow_embedded_content, sm.timed_out_until
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		INNER JOIN servers s ON c.server_id = s.id
		WHERE c.id = $1 AND sm.user_id = $2
	`, channelID, userID).Scan(&serverID, &serverAllowEmbeds, &timedOutUntil)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return "", false, false
	}
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return "", false, false
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermSendMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return "", false, false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return "", false, false
	}
	if timedOutUntil.Valid && timedOutUntil.Time.After(time.Now().UTC()) {
		c.JSON(http.StatusForbidden, gin.H{
			"error":           "Member is timed out",
			"code":            "member_timed_out",
			"timed_out_until": timedOutUntil.Time,
		})
		return "", false, false
	}

	return serverID, serverAllowEmbeds, true
}

// enforceE2EE validates ciphertext shape and epoch revocation for the channel.
// All channels are encrypted under E2EE-everywhere (#201).
// Returns (keyVersion, ok). On failure, writes the JSON error to c.
func (h *Handler) enforceE2EE(c *gin.Context, channelID string, content string, reqKeyVersion int) (int, bool) {
	if !isValidCiphertext(content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCiphertext})
		return 0, false
	}

	keyVersion := reqKeyVersion
	if keyVersion <= 0 {
		keyVersion = 1
	}

	var epochRevoked bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = $2)`,
		channelID, keyVersion,
	).Scan(&epochRevoked); err != nil {
		h.log.Error("Failed to check epoch revocation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify key epoch"})
		return 0, false
	}
	if epochRevoked {
		currentEpoch := 1
		if err := h.db.QueryRow(
			`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
			channelID,
		).Scan(&currentEpoch); err != nil {
			h.log.Error("Failed to fetch current epoch", "error", err, "channel_id", channelID)
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":         "Key epoch has been revoked — re-encrypt with current epoch",
			"code":          "epoch_revoked",
			"current_epoch": currentEpoch,
			"channel_id":    channelID,
		})
		return 0, false
	}

	return keyVersion, true
}

// SendMessage sends a new message to a channel
func (h *Handler) SendMessage(c *gin.Context) {
	userID := c.GetString("user_id")

	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	_, serverAllowEmbeds, accessOK := h.checkSendAccess(c, req.ChannelID, userID)
	if !accessOK {
		return
	}

	keyVersion, e2eeOK := h.enforceE2EE(c, req.ChannelID, req.Content, req.KeyVersion)
	if !e2eeOK {
		return
	}

	// Validate reply_to_id if provided (must reference a message in the same channel)
	replyToID, replyOK := h.validateReplyToID(c, req.ReplyToID, req.ChannelID)
	if !replyOK {
		return
	}

	// Normalize empty/whitespace gif_slug to nil so an explicit `"gif_slug": ""`
	// or `"gif_slug": "   "` round-trips to a NULL column ("no GIF attached")
	// instead of being persisted as an empty string. The migration semantics
	// reserve NULL for "no GIF" — we never want a non-NULL empty value.
	gifSlug := klipy.NormalizeSlug(req.GifSlug)
	if !klipy.ValidateSlug(gifSlug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": klipy.SlugValidationError(gifSlug)})
		return
	}

	// Create message — stamp embeds_suppressed based on server policy.
	// The server is the ONLY entity trusted to set this flag to false (allow).
	// If server policy is OFF (default), embeds_suppressed = true.
	embedsSuppressed := !serverAllowEmbeds

	messageID := uuid.New().String()
	insertQuery := `
		INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, reply_to_id, gif_slug, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.ChannelID = req.ChannelID
	message.UserID = userID
	message.Content = req.Content
	message.KeyVersion = keyVersion
	message.EmbedsSuppressed = embedsSuppressed
	message.ReplyToID = replyToID
	message.GifSlug = gifSlug

	err := h.db.QueryRow(insertQuery, messageID, req.ChannelID, userID, req.Content, keyVersion, embedsSuppressed, replyToID, gifSlug).Scan(
		&message.CreatedAt,
		&message.UpdatedAt,
	)
	if err != nil {
		if isFKViolation(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Reply target message not found"})
			return
		}
		h.log.Error("Failed to create message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}

	h.log.Info("Message sent", "message_id", messageID, "channel_id", req.ChannelID, "user_id", userID)

	c.JSON(http.StatusCreated, gin.H{"message": message})
}

// UpdateMessage updates a message's content
func (h *Handler) UpdateMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	var req UpdateMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check if message exists and user is the author
	var authorID string
	authorQuery := `SELECT user_id FROM messages WHERE id = $1`

	err := h.db.QueryRow(authorQuery, messageID).Scan(&authorID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own messages"})
		return
	}

	// E2EE enforcement — all messages are encrypted under #201; require ciphertext shape unconditionally.
	if !isValidCiphertext(req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCiphertext})
		return
	}

	// Update message
	updateQuery := `
		UPDATE messages
		SET content = $1, edited_at = NOW(), updated_at = NOW()
		WHERE id = $2
		RETURNING channel_id, key_version, embeds_suppressed, edited_at, created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.UserID = userID
	message.Content = req.Content

	err = h.db.QueryRow(updateQuery, req.Content, messageID).Scan(
		&message.ChannelID,
		&message.KeyVersion,
		&message.EmbedsSuppressed,
		&message.EditedAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)

	if err != nil {
		h.log.Error("Failed to update message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return
	}

	h.log.Info("Message updated", "message_id", messageID, "user_id", userID)

	// Broadcast update to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(message.ChannelID)
	if err == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":                messageID,
				"channel_id":        message.ChannelID,
				"content":           message.Content,
				"key_version":       message.KeyVersion,
				"embeds_suppressed": message.EmbedsSuppressed,
				"edited_at":         message.EditedAt,
				"updated_at":        message.UpdatedAt,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": message})
}

// DeleteMessage deletes a message
func (h *Handler) DeleteMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	// Check if message exists and get author + server info + channel ID
	var authorID, channelID, serverID string
	checkQuery := `
		SELECT m.user_id, m.channel_id, c.server_id
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		WHERE m.id = $1
	`

	err := h.db.QueryRow(checkQuery, messageID).Scan(&authorID, &channelID, &serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		return
	}

	// Author can edit/delete own messages (if they have PermManageOwnMessages).
	// Others need PermManageAllMessages to delete other people's messages or suppress embeds.
	canDelete := false
	if authorID == userID {
		has, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageOwnMessages)
		if permErr != nil {
			h.log.Error("Failed to check PermManageOwnMessages", "error", permErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
			return
		}
		canDelete = has
	}
	if !canDelete {
		has, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageAllMessages)
		if permErr != nil {
			h.log.Error("Failed to check PermManageAllMessages", "error", permErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
			return
		}
		canDelete = has
	}

	if !canDelete {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermsLower})
		return
	}

	// Delete message
	deleteQuery := `DELETE FROM messages WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, messageID)
	if err != nil {
		h.log.Error("Failed to delete message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		return
	}

	h.log.Info("Message deleted", "message_id", messageID, "deleted_by", userID, "author", authorID)

	// Broadcast deletion to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(channelID)
	if err == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_delete",
			Data: map[string]interface{}{
				"id":         messageID,
				"channel_id": channelID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Message deleted successfully"})
}

// SuppressEmbeds suppresses embedded content on a message (one-way ratchet).
// Requires PermManageAllMessages. Can only set embeds_suppressed = true, never false.
// Once suppressed, only the server policy can allow embeds on NEW messages.
func (h *Handler) SuppressEmbeds(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	// Get message info for permission check
	var channelID, serverID string
	var alreadySuppressed bool
	checkQuery := `
		SELECT m.channel_id, c.server_id, m.embeds_suppressed
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		WHERE m.id = $1
	`

	err := h.db.QueryRow(checkQuery, messageID).Scan(&channelID, &serverID, &alreadySuppressed)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message for embed suppression", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}

	// Already suppressed — no-op, return success
	if alreadySuppressed {
		c.JSON(http.StatusOK, gin.H{"message": "Embeds already suppressed"})
		return
	}

	// Check PermManageAllMessages
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageAllMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// One-way ratchet: suppress only (false → true). Never un-suppress.
	_, err = h.db.Exec(
		`UPDATE messages SET embeds_suppressed = TRUE, updated_at = NOW() WHERE id = $1 AND embeds_suppressed = FALSE`,
		messageID,
	)
	if err != nil {
		h.log.Error("Failed to suppress embeds", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}

	h.log.Info("Embeds suppressed", "message_id", messageID, "suppressed_by", userID)

	// Broadcast update to channel subscribers so clients hide the embeds
	channelUUID, parseErr := uuid.Parse(channelID)
	if parseErr == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":                messageID,
				"channel_id":        channelID,
				"embeds_suppressed": true,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Embeds suppressed successfully"})
}
