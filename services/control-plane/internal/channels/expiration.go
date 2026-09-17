package channels

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxExpirationRequestBytes = 1024

// UpdateExpiration changes a channel's shared message-expiration policy.
func (h *Handler) UpdateExpiration(c *gin.Context) {
	userID, channelID := c.GetString("user_id"), c.Param("id")
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var request expiration.Request
	if !bindStrictJSONBody(c, &request, maxExpirationRequestBytes) {
		return
	}
	if err := request.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	serverID, ok := h.resolveChannelExpirationServer(c, channelID)
	if !ok {
		return
	}

	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin channel expiration transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back channel expiration transaction", "error", rollbackErr)
		}
	}()

	if !h.authorizeChannelExpirationTx(c, tx, channelID, serverID, userID, request.Mode) {
		return
	}

	service := expiration.NewService(h.db)
	transition, err := service.StartChannelTransition(c.Request.Context(), tx, channelID, request)
	if err != nil {
		h.writeExpirationStartError(c, err)
		return
	}
	policy := transition.Current

	staged, ok := h.stageChannelExpirationEvent(c, tx, channelID, userID, request, transition)
	if !ok {
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Channel expiration commit outcome is ambiguous", "error", err)
		c.JSON(http.StatusServiceUnavailable, policy)
		return
	}
	if staged.Present {
		h.broadcastChannelExpirationEvent(channelID, staged.MessageID, userID, staged.Payload, policy)
	}
	if !policy.BackfillPending {
		c.JSON(http.StatusOK, policy)
		return
	}
	h.respondChannelExpirationBackfill(c, service, channelID, policy)
}

// resolveChannelExpirationServer reads the channel's server WITHOUT a lock, so
// the advisory lock below can be keyed by it. The value is deliberately
// re-read under lock in authorizeChannelExpirationTx and compared: this one is
// a routing hint, never the authorization operand.
func (h *Handler) resolveChannelExpirationServer(c *gin.Context, channelID string) (string, bool) {
	var serverID string
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT server_id FROM channels WHERE id = $1`, channelID,
	).Scan(&serverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
			return "", false
		}
		h.log.Error("Failed to look up channel expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return "", false
	}
	return serverID, true
}

// authorizeChannelExpirationTx takes the transaction's locks and resolves the
// actor's authority for this mutation. It writes its own response and returns
// false when the caller must stop.
//
// The lock ordering here is load-bearing and is the reason this is one helper
// rather than several: LockServerVisibilityCapture must remain the
// transaction's FIRST statement, because it serializes the live authority
// writers that can alter the actor's channel permissions underneath this read.
func (h *Handler) authorizeChannelExpirationTx(
	c *gin.Context,
	tx *sql.Tx,
	channelID, serverID, userID, mode string,
) bool {
	ctx := c.Request.Context()
	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		h.log.Error("Failed to lock channel expiration authority", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return false
	}
	if guardErr := credepoch.GuardTx(ctx, tx, userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		h.respondCreateChannelGuardError(c, guardErr)
		return false
	}

	var lockedServerID, channelType string
	if err := tx.QueryRowContext(ctx,
		`SELECT server_id, type FROM channels WHERE id = $1 FOR NO KEY UPDATE`, channelID,
	).Scan(&lockedServerID, &channelType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
			return false
		}
		h.log.Error("Failed to lock channel expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return false
	}
	// The unlocked pre-read and the locked read must agree: a channel moved
	// between servers in that window would otherwise be authorized against the
	// server it used to be on.
	if lockedServerID != serverID {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return false
	}

	permissions, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, serverID, userID, channelID)
	if errors.Is(err, rbac.ErrNotMember) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return false
	}
	if err != nil {
		h.log.Error("Failed to resolve channel expiration permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return false
	}
	if !permissions.Has(rbac.PermManageChannels) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return false
	}
	if mode == "set" && channelType != "text" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Expiration is only available for text channels"})
		return false
	}
	return true
}

// stageChannelExpirationEvent writes the durable system row on the SAME
// transaction as the policy update — see insertChannelExpirationEvent's doc
// comment for the transactional-correctness argument. The second return is
// "keep going", not "wrote a row": a transition that produced no event is an
// ordinary success with Present false.
func (h *Handler) stageChannelExpirationEvent(
	c *gin.Context,
	tx *sql.Tx,
	channelID, userID string,
	request expiration.Request,
	transition expiration.Transition,
) (expiration.StagedEvent, bool) {
	payload, ok := expiration.EventFor(request, transition, userID)
	if !ok {
		return expiration.StagedEvent{}, true
	}
	messageID, err := insertChannelExpirationEvent(c.Request.Context(), tx, channelID, userID, payload)
	if err != nil {
		h.log.Error("Failed to insert channel expiration_event row", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return expiration.StagedEvent{}, false
	}
	return expiration.StagedEvent{MessageID: messageID, Payload: payload, Present: true}, true
}

// respondChannelExpirationBackfill runs the post-commit resume batch and writes
// the response. ErrBackfillPending is the expected outcome of a bounded batch
// that has more to do, so it is not logged as a failure.
func (h *Handler) respondChannelExpirationBackfill(
	c *gin.Context,
	service *expiration.Service,
	channelID string,
	policy expiration.Policy,
) {
	resumed, resumeErr := service.ResumeChannel(c.Request.Context(), channelID, policy.Revision)
	resumed, status := expiration.NormalizeResume(policy, resumed, resumeErr)
	if status == http.StatusServiceUnavailable && !errors.Is(resumeErr, expiration.ErrBackfillPending) {
		h.log.Error("Failed to resume channel expiration backfill", "error", resumeErr)
	}
	c.JSON(status, resumed)
}

func (h *Handler) writeExpirationStartError(c *gin.Context, err error) {
	status := expiration.StartErrorStatus(err)
	if status == http.StatusInternalServerError {
		h.log.Error("Failed to start channel expiration policy", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	message := err.Error()
	if status == http.StatusNotFound {
		message = errMsgChannelNotFound
	}
	c.JSON(status, gin.H{"error": message})
}
