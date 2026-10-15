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
	started := time.Now()
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

	isGroup, ok := h.authorizeDMExpirationTx(mutationCtx, c, tx, conversationUUID, userID)
	if !ok {
		return
	}

	service := expiration.NewService(h.db)
	transition, err := service.StartConversationTransition(mutationCtx, tx, conversationID, request)
	if err != nil {
		h.writeExpirationStartError(c, err)
		return
	}
	policy := transition.Current

	staged, respawned, ok := h.stageDMExpirationEvent(mutationCtx, c, tx, conversationUUID, userID, request, transition)
	if !ok {
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("DM expiration commit outcome is ambiguous", "error", err)
		c.JSON(http.StatusServiceUnavailable, policy)
		return
	}
	// After a clean commit, in this order (#3459 §8): announce the policy
	// change, delete the pins that were already due when the conversation
	// stopped keeping them, then resume any backfill. That is the order the
	// calls are ISSUED in, so a slow deletion cannot hold the announcement
	// back. It is not the order a client receives the two frames in: the hub
	// delivers the policy broadcast and dm_purged on separate lanes, and either
	// can arrive first. The issue order is pinned by
	// TestDMExpiration_PolicyIsAnnouncedWhileTheDueDeletionIsStillHeld. The
	// deletion runs on the REQUEST context, never mutationCtx: that 3-second
	// budget bounds the write transaction, and the expirer bounds its own run.
	if staged.Present {
		h.hub.PublishDMRespawn(conversationUUID, respawned)
		h.broadcastDMExpirationEvent(conversationUUID, staged.MessageID, userID, staged.Payload, policy)
	}
	// The deletion and the backfill share one deadline measured from the
	// handler's start, so the response still goes out inside the server's
	// write timeout however long the steps before them took.
	answerCtx, cancel := context.WithDeadline(c.Request.Context(), h.responseDeadline(started))
	defer cancel()
	if len(transition.DuePinnedIDs) > 0 {
		h.pinnedExpirer.ExpireConversationPins(answerCtx, conversationID, isGroup, transition)
	}
	if !policy.BackfillPending {
		c.JSON(http.StatusOK, policy)
		return
	}
	h.respondDMExpirationBackfill(answerCtx, c, service, conversationID, policy)
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
// It also returns is_group, read under the conversation lock, which names the
// purge context type a pinned-expiry deletion is audited under.
func (h *Handler) authorizeDMExpirationTx(
	ctx context.Context,
	c *gin.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
	userID string,
) (isGroupConversation bool, ok bool) {
	var isGroup bool
	if err := tx.QueryRowContext(ctx,
		`SELECT is_group FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, conversationID,
	).Scan(&isGroup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return false, false
		}
		h.log.Error("Failed to lock DM expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return false, false
	}
	if err := dmvisibility.LockParticipantsForWrite(ctx, tx, conversationID); err != nil {
		h.log.Error("Failed to lock DM expiration participant visibility", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return false, false
	}
	var role string
	if err := tx.QueryRowContext(ctx,
		`SELECT role FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, conversationID, userID,
	).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return false, false
		}
		h.log.Error("Failed to lock DM expiration participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return false, false
	}
	if isGroup && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotAdmin})
		return false, false
	}
	return isGroup, true
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
) (expiration.StagedEvent, []uuid.UUID, bool) {
	payload, ok := expiration.EventFor(request, transition, userID)
	if !ok {
		return expiration.StagedEvent{}, nil, true
	}
	messageID, respawned, err := insertDMExpirationEvent(ctx, tx, conversationID, userID, payload)
	if err != nil {
		h.log.Error("Failed to insert DM expiration_event row", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return expiration.StagedEvent{}, nil, false
	}
	return expiration.StagedEvent{MessageID: messageID, Payload: payload, Present: true}, respawned, true
}

// respondDMExpirationBackfill runs the post-commit resume batch and writes the
// response. It deliberately uses the REQUEST context, not the mutation context:
// that 3-second budget bounds the write transaction, and the batch runs after
// the commit.
func (h *Handler) respondDMExpirationBackfill(
	ctx context.Context,
	c *gin.Context,
	service *expiration.Service,
	conversationID string,
	policy expiration.Policy,
) {
	resumed, resumeErr := service.ResumeConversation(ctx, conversationID, policy.Revision)
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

// SetPinnedExpirer wires the terminal that deletes a conversation's
// already-due pinned messages when its policy stops keeping pins (#3459 §8).
// The router's requirePinnedExpiryWired refuses to boot without it.
func (h *Handler) SetPinnedExpirer(e *expiration.ContextExpirer) { h.pinnedExpirer = e }

// HasPinnedExpiryRunner reports whether SetPinnedExpirer was called with a
// non-nil expirer.
func (h *Handler) HasPinnedExpiryRunner() bool { return h != nil && h.pinnedExpirer != nil }

// responseDeadline is when the work after a policy commit must stop so the
// response can still be written: start plus expiration.ResponseBudget, or the
// budget a test set on the handler.
func (h *Handler) responseDeadline(start time.Time) time.Time {
	budget := h.responseBudget
	if budget <= 0 {
		budget = expiration.ResponseBudget
	}
	return start.Add(budget)
}
