package dm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmvisibility"
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
	conversationUUID, err := uuid.Parse(conversationID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	conversationID = conversationUUID.String()
	request, ok := h.bindExpirationRequest(c)
	if !ok {
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

	if !h.authorizeDMExpirationTx(mutationCtx, c, tx, conversationUUID, userID) {
		return
	}

	service := expiration.NewService(h.db)
	transition, err := service.StartConversationTransition(mutationCtx, tx, conversationID, request)
	if err != nil {
		h.writeExpirationStartError(c, err)
		return
	}
	policy := transition.Current

	staged, ok := h.stageDMExpirationEvent(mutationCtx, c, tx, conversationUUID, userID, request, transition)
	if !ok {
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("DM expiration commit outcome is ambiguous", "error", err)
		c.JSON(http.StatusServiceUnavailable, policy)
		return
	}
	if staged.Present {
		h.broadcastDMExpirationEvent(conversationUUID, staged.MessageID, userID, staged.Payload, policy)
	}
	if !policy.BackfillPending {
		c.JSON(http.StatusOK, policy)
		return
	}
	h.respondDMExpirationBackfill(c, service, conversationID, policy)
}

// bindExpirationRequest applies the byte cap and the strict full-document read
// this repo requires of every JSON handler: MaxBytesReader alone is not enough,
// because the decoder may stop after the first valid value without reading the
// oversized remainder, so the cached body is re-checked with json.Valid. Error
// ordering is part of the wire contract — 413 before 400.
func (h *Handler) bindExpirationRequest(c *gin.Context) (expiration.Request, bool) {
	var request expiration.Request
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxExpirationRequestBytes)
	if err := c.ShouldBindBodyWithJSON(&request); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": errMsgExpirationBodyTooLarge})
			return expiration.Request{}, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return expiration.Request{}, false
	}
	body, ok := c.Get(gin.BodyBytesKey)
	bodyBytes, bodyIsBytes := body.([]byte)
	if !ok || !bodyIsBytes || !json.Valid(bodyBytes) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return expiration.Request{}, false
	}
	if err := request.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return expiration.Request{}, false
	}
	return request, true
}

// authorizeDMExpirationTx locks the conversation and every participant in
// canonical order before it locks the actor's row and applies the group-admin
// rule. This preserves membership stability and the users -> conversation ->
// participants -> message lock order.
//
// A non-participant and a missing conversation deliberately return the same
// 404: distinguishing them would tell a stranger that a conversation exists.
func (h *Handler) authorizeDMExpirationTx(
	ctx context.Context,
	c *gin.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	userID string,
) bool {
	var isGroup bool
	if err := tx.QueryRowContext(ctx,
		`SELECT is_group FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID,
	).Scan(&isGroup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return false
		}
		h.log.Error("Failed to lock DM expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return false
	}
	if err := dmvisibility.LockParticipantsForWrite(ctx, tx, conversationID); err != nil {
		h.log.Error("Failed to lock DM expiration participant visibility", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return false
	}
	var role string
	if err := tx.QueryRowContext(ctx,
		`SELECT role FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, conversationID, userID,
	).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return false
		}
		h.log.Error("Failed to lock DM expiration participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return false
	}
	if isGroup && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotAdmin})
		return false
	}
	return true
}

// stageDMExpirationEvent writes the durable system row on the SAME transaction
// as the policy update — see insertDMExpirationEvent's doc comment for the
// transactional-correctness argument. The second return is "keep going", not
// "wrote a row".
func (h *Handler) stageDMExpirationEvent(
	ctx context.Context,
	c *gin.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	userID string,
	request expiration.Request,
	transition expiration.Transition,
) (expiration.StagedEvent, bool) {
	payload, ok := expiration.EventFor(request, transition, userID)
	if !ok {
		return expiration.StagedEvent{}, true
	}
	messageID, err := insertDMExpirationEvent(ctx, tx, conversationID, userID, payload)
	if err != nil {
		h.log.Error("Failed to insert DM expiration_event row", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return expiration.StagedEvent{}, false
	}
	return expiration.StagedEvent{MessageID: messageID, Payload: payload, Present: true}, true
}

// respondDMExpirationBackfill runs the post-commit resume batch and writes the
// response. It deliberately uses the REQUEST context, not the mutation context:
// that 3-second budget bounds the write transaction, and the batch runs after
// the commit.
func (h *Handler) respondDMExpirationBackfill(
	c *gin.Context,
	service *expiration.Service,
	conversationID string,
	policy expiration.Policy,
) {
	resumed, resumeErr := service.ResumeConversation(c.Request.Context(), conversationID, policy.Revision)
	resumed, status := expiration.NormalizeResume(policy, resumed, resumeErr)
	if status == http.StatusServiceUnavailable && !errors.Is(resumeErr, expiration.ErrBackfillPending) {
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
