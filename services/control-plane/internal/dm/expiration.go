package dm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	maxExpirationRequestBytes    int64 = 1024
	errMsgExpirationBodyTooLarge       = "Request body too large"
	errMsgConversationNotFound         = "Conversation not found"
)

// UpdateExpiration changes a DM conversation's shared message-expiration policy.
func (h *Handler) UpdateExpiration(c *gin.Context) {
	userID, conversationID := c.GetString("user_id"), c.Param("id")
	if _, err := uuid.Parse(conversationID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	var request expiration.Request
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxExpirationRequestBytes)
	if err := c.ShouldBindBodyWithJSON(&request); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": errMsgExpirationBodyTooLarge})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	body, ok := c.Get(gin.BodyBytesKey)
	bodyBytes, bodyIsBytes := body.([]byte)
	if !ok || !bodyIsBytes || !json.Valid(bodyBytes) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	if err := request.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	mutationCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	tx, err := h.db.BeginTx(mutationCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin DM expiration transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back DM expiration transaction", "error", rollbackErr)
		}
	}()
	if guardErr := credepoch.GuardTx(mutationCtx, tx, userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		h.respondGuardTxError(c, guardErr, errMsgFailedUpdateConversation)
		return
	}

	var isGroup bool
	if err := tx.QueryRowContext(mutationCtx,
		`SELECT is_group FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID,
	).Scan(&isGroup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return
		}
		h.log.Error("Failed to lock DM expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return
	}
	var role string
	if err := tx.QueryRowContext(mutationCtx,
		`SELECT role FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR SHARE`, conversationID, userID,
	).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return
		}
		h.log.Error("Failed to lock DM expiration participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return
	}
	if isGroup && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotAdmin})
		return
	}

	service := expiration.NewService(h.db)
	policy, err := service.StartConversation(mutationCtx, tx, conversationID, request)
	if err != nil {
		h.writeExpirationStartError(c, err)
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("DM expiration commit outcome is ambiguous", "error", err)
		c.JSON(http.StatusServiceUnavailable, policy)
		return
	}
	if !policy.BackfillPending {
		c.JSON(http.StatusOK, policy)
		return
	}

	resumed, resumeErr := service.ResumeConversation(c.Request.Context(), conversationID, policy.Revision)
	resumed, status := expiration.NormalizeResume(policy, resumed, resumeErr)
	if status != http.StatusServiceUnavailable {
		c.JSON(status, resumed)
		return
	}
	if !errors.Is(resumeErr, expiration.ErrBackfillPending) {
		h.log.Error("Failed to resume DM expiration backfill", "error", resumeErr)
	}
	c.JSON(status, resumed)
}

func (h *Handler) writeExpirationStartError(c *gin.Context, err error) {
	status := expiration.StartErrorStatus(err)
	if status == http.StatusInternalServerError {
		h.log.Error("Failed to start DM expiration policy", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return
	}
	message := err.Error()
	if status == http.StatusNotFound {
		message = errMsgConversationNotFound
	}
	c.JSON(status, gin.H{"error": message})
}
