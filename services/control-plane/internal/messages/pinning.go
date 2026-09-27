package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var errDMMessageMutationNotParticipant = errors.New("dm mutation user is not a current participant")

func respondDMMessageMutationGuardError(c *gin.Context, err error) bool {
	if errors.Is(err, dmblock.ErrUnavailable) || errors.Is(err, dmblock.ErrMembershipChanged) {
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_unavailable"})
		return true
	}
	return false
}

// withDMMessageMutation serializes every DM mutation with membership and key
// rotation. A FOR NO KEY UPDATE parent lock also serializes concurrent toggles
// that otherwise both observe the same reaction row before deleting it.
func (h *Handler) withDMMessageMutation(ctx context.Context, conversationID, userID, credentialEpoch string, mutation func(*sql.Tx) error) error {
	actor, err := uuid.Parse(userID)
	if err != nil {
		return errDMMessageMutationNotParticipant
	}
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := h.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin dm message mutation: %w", err)
		}
		err = func() error {
			// Preparation owns the users-before-parent prefix and must precede the
			// sender's credential guard.
			if _, guardErr := dmblock.PrepareConversationTx(ctx, tx, conversationID, []uuid.UUID{actor}, dmblock.LockShare, dmblock.LockNoKeyUpdate); guardErr != nil {
				return guardErr
			}
			if guardErr := credepoch.GuardTx(ctx, tx, userID, credentialEpoch); guardErr != nil {
				return guardErr
			}
			var participantID string
			if err := tx.QueryRowContext(ctx,
				`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR SHARE`, conversationID, userID,
			).Scan(&participantID); errors.Is(err, sql.ErrNoRows) {
				return errDMMessageMutationNotParticipant
			} else if err != nil {
				return fmt.Errorf("lock dm message participant: %w", err)
			}
			if err := mutation(tx); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit dm message mutation: %w", err)
			}
			return nil
		}()
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			if h.log != nil {
				h.log.Error("Failed to rollback dm message mutation", "error", rbErr)
			}
			if err == nil {
				return fmt.Errorf("rollback dm message mutation: %w", rbErr)
			}
		}
		if errors.Is(err, dmblock.ErrMembershipChanged) && attempt == 0 {
			continue
		}
		return err
	}
	return dmblock.ErrMembershipChanged
}

const (
	errMsgPinFailed       = "Failed to pin message"
	errMsgUnpinFailed     = "Failed to unpin message"
	errMsgFetchPinsFailed = "Failed to fetch pinned messages"
	errMsgPinLimitReached = "Maximum of 50 pinned messages per channel"
	maxPinsPerChannel     = 50
)

// lockChannelMessageMutationTx is the common write fence for channel pins and
// reactions. It validates the resource again after the transaction starts;
// request-time lookups are only response shaping. The message is locked FOR
// UPDATE here because every caller mutates it directly or writes a child that
// must serialize with its deletion; taking FOR SHARE and upgrading later lets
// concurrent writers deadlock.
func (h *Handler) lockChannelMessageMutationTx(c *gin.Context, tx *sql.Tx, messageID, userID string, required rbac.Permission, genericMsg string) (string, string, string, bool) {
	var channelID, serverID, channelType string
	if err := tx.QueryRowContext(c.Request.Context(), `
		SELECT m.channel_id, ch.server_id FROM messages m
		INNER JOIN channels ch ON ch.id = m.channel_id WHERE m.id = $1`, messageID,
	).Scan(&channelID, &serverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		} else {
			h.log.Error("Failed to lock channel message mutation", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": genericMsg})
		}
		return "", "", "", false
	}
	if guardErr := credepoch.GuardTx(c.Request.Context(), tx, userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		h.respondGuardTxError(c, guardErr, genericMsg)
		return "", "", "", false
	}
	authorization := messageChannelAuthorization{
		serverID: serverID, channelID: channelID, userID: userID,
		required: required, genericMsg: genericMsg,
	}
	channelType, locked := h.lockMessageChannelTx(c.Request.Context(), c, tx, authorization)
	if !locked {
		return "", "", "", false
	}
	if !h.lockCurrentMessageMembership(c.Request.Context(), c, tx, serverID, userID, genericMsg) ||
		!h.authorizeMessageChannelTx(c.Request.Context(), c, tx, authorization, channelType) {
		return "", "", "", false
	}
	var lockedMessageID string
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT id FROM messages WHERE id = $1 AND channel_id = $2 FOR UPDATE`, messageID, channelID).Scan(&lockedMessageID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		} else {
			h.log.Error("Failed to lock channel message", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": genericMsg})
		}
		return "", "", "", false
	}
	return channelID, serverID, channelType, true
}

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

	// Pin with a committed transaction-local authority check.
	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("Failed to rollback message pin transaction", "error", rbErr)
		}
	}()
	lockedChannelID, _, _, allowed := h.lockChannelMessageMutationTx(c, tx, messageID, userID, rbac.PermPinMessages, errMsgPinFailed)
	if !allowed {
		return
	}
	channelID = lockedChannelID
	// NOTE: Under high concurrency, two simultaneous requests could both see
	// COUNT(*) < 50 and succeed. For production at scale, consider wrapping in
	// a transaction with an advisory lock keyed by channel_id.
	var pinnedAt time.Time
	var pinnedBy string
	err = tx.QueryRowContext(c.Request.Context(), `
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
		checkErr := tx.QueryRowContext(c.Request.Context(), `SELECT pinned_at, pinned_by FROM messages WHERE id = $1`, messageID).Scan(&existingPinnedAt, &existingPinnedBy)
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
	if err = tx.Commit(); err != nil {
		h.log.Error(errMsgPinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}

	// Broadcast to channel (server channel: recheck per-recipient view access).
	broadcastPin(h.hub, channelID, messageID, pinnedAt, pinnedBy, true)

	c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": pinnedAt, "pinned_by": pinnedBy})
}

// pinDMMessage handles the DM branch of PinMessage. The caller has already
// verified conversation participation via lookupMessageContext.
func (h *Handler) pinDMMessage(c *gin.Context, messageID, userID, conversationID string) {
	var pinnedAt time.Time
	var pinnedBy string
	var existingPinnedAt *time.Time
	var existingPinnedBy *string
	fallback := false
	err := h.withDMMessageMutation(c.Request.Context(), conversationID, userID, middleware.TokenCredentialEpoch(c), func(tx *sql.Tx) error {
		// The request-time lookup only shapes the response. Recheck visibility
		// inside the locked mutation so Clear cannot make hidden history mutable.
		//nolint:gosec // G202: HiddenRangeFilter is a compile-time SQL fragment; values are parameterized.
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		err := tx.QueryRowContext(c.Request.Context(), `
		UPDATE dm_messages dm
		SET pinned_at = NOW(), pinned_by = $1, updated_at = NOW()
		WHERE dm.id = $2 AND dm.conversation_id = $3 AND dm.pinned_at IS NULL
		  AND EXISTS (
			  SELECT 1 FROM dm_participants dp
			  WHERE dp.conversation_id = dm.conversation_id AND dp.user_id = $1
		  )
		`+purge.HiddenRangeFilter("dm", 1)+`
		  AND (SELECT COUNT(*) FROM dm_messages pin_count
		       WHERE pin_count.conversation_id = $3 AND pin_count.pinned_at IS NOT NULL
		       `+purge.HiddenRangeFilter("pin_count", 1)+`) < $4
		RETURNING pinned_at, pinned_by
		`, userID, messageID, conversationID, maxPinsPerChannel).Scan(&pinnedAt, &pinnedBy)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		fallback = true
		// Keep the disambiguation in this transaction. A pool query after the
		// fence releases would re-open the Clear race for the already-pinned
		// response path.
		//nolint:gosec // G202: HiddenRangeFilter is a compile-time SQL fragment; values are parameterized.
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		if err := tx.QueryRowContext(c.Request.Context(), `
			SELECT dm.pinned_at, dm.pinned_by
			FROM dm_messages dm
			WHERE dm.id = $1 AND dm.conversation_id = $3
			  AND EXISTS (
				  SELECT 1 FROM dm_participants dp
				  WHERE dp.conversation_id = dm.conversation_id AND dp.user_id = $2
			  )
			`+purge.HiddenRangeFilter("dm", 2), messageID, userID, conversationID).Scan(&existingPinnedAt, &existingPinnedBy); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errDMMessageMutationNotParticipant
			}
			return fmt.Errorf("check visible DM pin fallback: %w", err)
		}
		return nil
	})
	if errors.Is(err, errDMMessageMutationNotParticipant) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	}
	if respondDMMessageMutationGuardError(c, err) {
		return
	}
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if err != nil {
		h.log.Error(errMsgPinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}

	if fallback {
		if existingPinnedAt != nil {
			c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": existingPinnedAt, "pinned_by": existingPinnedBy, "already_pinned": true})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": errMsgPinLimitReached})
		return
	}

	if convUUID, convErr := uuid.Parse(conversationID); convErr == nil {
		if messageUUID, messageErr := uuid.Parse(messageID); messageErr == nil {
			h.hub.BroadcastToDMMessageRecipients(convUUID, messageUUID, websocket.OutgoingMessage{
				Type: "message_pinned",
				Data: map[string]interface{}{
					"message_id": messageID,
					"channel_id": conversationID,
					"pinned_at":  pinnedAt,
					"pinned_by":  pinnedBy,
				},
			})
		}
	}

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

	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("Failed to rollback message unpin transaction", "error", rbErr)
		}
	}()
	lockedChannelID, _, _, allowed := h.lockChannelMessageMutationTx(c, tx, messageID, userID, rbac.PermPinMessages, errMsgUnpinFailed)
	if !allowed {
		return
	}
	channelID = lockedChannelID
	result, err := tx.ExecContext(c.Request.Context(), `
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
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
			return
		}
		// Already unpinned — idempotent success
		c.JSON(http.StatusOK, gin.H{"message_id": messageID, "already_unpinned": true})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error(errMsgUnpinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}

	// Broadcast to channel
	channelUUID, parseErr := uuid.Parse(channelID)
	if parseErr == nil {
		h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
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
	userID := c.GetString("user_id")
	var rowsAffected int64
	err := h.withDMMessageMutation(c.Request.Context(), conversationID, userID, middleware.TokenCredentialEpoch(c), func(tx *sql.Tx) error {
		//nolint:gosec // G202: HiddenRangeFilter is a compile-time SQL fragment; values are parameterized.
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		result, err := tx.ExecContext(c.Request.Context(), `
		UPDATE dm_messages dm SET pinned_at = NULL, pinned_by = NULL, updated_at = NOW()
		WHERE dm.id = $1 AND dm.conversation_id = $3 AND dm.pinned_at IS NOT NULL
		  AND EXISTS (
			  SELECT 1 FROM dm_participants dp
			  WHERE dp.conversation_id = dm.conversation_id AND dp.user_id = $2
		  )
		`+purge.HiddenRangeFilter("dm", 2), messageID, userID, conversationID)
		if err != nil {
			return fmt.Errorf("unpin visible DM message: %w", err)
		}
		var rowsErr error
		rowsAffected, rowsErr = result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("count visible DM unpin rows: %w", rowsErr)
		}
		if rowsAffected != 0 {
			return nil
		}

		// A zero-row update is idempotent only when the target remains visible
		// to this actor under the same transaction fence.
		//nolint:gosec // G202: HiddenRangeFilter is a compile-time SQL fragment; values are parameterized.
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		var visiblePinnedAt *time.Time
		if err := tx.QueryRowContext(c.Request.Context(), `
			SELECT dm.pinned_at
			FROM dm_messages dm
			WHERE dm.id = $1 AND dm.conversation_id = $3
			  AND EXISTS (
				  SELECT 1 FROM dm_participants dp
				  WHERE dp.conversation_id = dm.conversation_id AND dp.user_id = $2
			  )
			`+purge.HiddenRangeFilter("dm", 2), messageID, userID, conversationID).Scan(&visiblePinnedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errDMMessageMutationNotParticipant
			}
			return fmt.Errorf("check visible DM unpin fallback: %w", err)
		}
		return nil
	})
	if errors.Is(err, errDMMessageMutationNotParticipant) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	}
	if respondDMMessageMutationGuardError(c, err) {
		return
	}
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if err != nil {
		h.log.Error(errMsgUnpinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusOK, gin.H{"message_id": messageID, "already_unpinned": true})
		return
	}

	if convUUID, parseErr := uuid.Parse(conversationID); parseErr == nil {
		if messageUUID, messageErr := uuid.Parse(messageID); messageErr == nil {
			h.hub.BroadcastToDMMessageRecipients(convUUID, messageUUID, websocket.OutgoingMessage{
				Type: "message_unpinned",
				Data: map[string]interface{}{
					"message_id":      messageID,
					"conversation_id": conversationID,
				},
			})
		}
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
		       m.embeds_suppressed, m.reply_to_id, m.pinned_at, m.pinned_by, m.edited_at, m.expires_at, m.created_at, m.updated_at,
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
			&msg.ExpiresAt, &msg.CreatedAt, &msg.UpdatedAt, &msg.Username, &msg.DisplayName, &msg.AvatarURL,
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
	ExpiresAt      *time.Time `json:"expires_at"`
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

	// purge.HiddenRangeFilter excludes pins the requester purged-from-view
	// (#1352 receiver-hide) — a hidden message must not resurface via the pin
	// list. Concatenated fragment is a compile-time constant; values are
	// parameterized.
	//nolint:gosec // G202: concatenated fragment is a compile-time constant; all values parameterized
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
	rows, err := h.db.Query(`
		SELECT dm.id, dm.conversation_id, dm.user_id, dm.content, dm.type,
		       dm.pinned_at, dm.pinned_by, dm.edited_at, dm.expires_at, dm.created_at, dm.updated_at,
		       u.username, u.display_name, u.avatar_url
		FROM dm_messages dm
		INNER JOIN users u ON dm.user_id = u.id
		WHERE dm.conversation_id = $1 AND dm.pinned_at IS NOT NULL
		`+purge.HiddenRangeFilter("dm", 2)+`
		ORDER BY dm.pinned_at DESC
	`, conversationID, userID)
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
			&m.PinnedAt, &m.PinnedBy, &m.EditedAt, &m.ExpiresAt, &m.CreatedAt, &m.UpdatedAt,
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
// authorized=true applies the per-recipient view-permission recheck used for
// server channels (CV-CAN-021..026). DM conversations must pass false: they
// have no server view permission and deliveryAuthForChannel queries the
// channels table, so the authorized path would drop the event entirely.
func broadcastPin(hub *websocket.Hub, channelID, messageID string, pinnedAt time.Time, pinnedBy string, authorized bool) {
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}
	msg := websocket.OutgoingMessage{
		Type: "message_pinned",
		Data: map[string]interface{}{
			"message_id": messageID,
			"channel_id": channelID,
			"pinned_at":  pinnedAt,
			"pinned_by":  pinnedBy,
		},
	}
	if authorized {
		hub.BroadcastToChannelAuthorized(channelUUID, msg)
	} else {
		hub.BroadcastToChannel(channelUUID, msg)
	}
}
