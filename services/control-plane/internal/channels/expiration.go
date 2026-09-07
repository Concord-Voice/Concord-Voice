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

	var serverID string
	if err := h.db.QueryRowContext(c.Request.Context(), `SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
			return
		}
		h.log.Error("Failed to look up channel expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
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

	// This is deliberately the transaction's first statement. It serializes the
	// live authority writers that can alter the actor's channel permissions.
	if err := rbac.LockServerVisibilityCapture(c.Request.Context(), tx, serverID); err != nil {
		h.log.Error("Failed to lock channel expiration authority", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	if guardErr := credepoch.GuardTx(c.Request.Context(), tx, userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		h.respondCreateChannelGuardError(c, guardErr)
		return
	}

	var lockedServerID, channelType string
	if err := tx.QueryRowContext(c.Request.Context(),
		`SELECT server_id, type FROM channels WHERE id = $1 FOR NO KEY UPDATE`, channelID,
	).Scan(&lockedServerID, &channelType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
			return
		}
		h.log.Error("Failed to lock channel expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	if lockedServerID != serverID {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	permissions, err := h.resolver.ResolveChannelPermissionsTx(c.Request.Context(), tx, serverID, userID, channelID)
	if errors.Is(err, rbac.ErrNotMember) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		h.log.Error("Failed to resolve channel expiration permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	if !permissions.Has(rbac.PermManageChannels) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}
	if request.Mode == "set" && channelType != "text" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Expiration is only available for text channels"})
		return
	}

	service := expiration.NewService(h.db)
	policy, err := service.StartChannel(c.Request.Context(), tx, channelID, request)
	if err != nil {
		h.writeExpirationStartError(c, err)
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Channel expiration commit outcome is ambiguous", "error", err)
		c.JSON(http.StatusServiceUnavailable, policy)
		return
	}
	if !policy.BackfillPending {
		c.JSON(http.StatusOK, policy)
		return
	}

	resumed, resumeErr := service.ResumeChannel(c.Request.Context(), channelID, policy.Revision)
	resumed, status := expiration.NormalizeResume(policy, resumed, resumeErr)
	if status != http.StatusServiceUnavailable {
		c.JSON(status, resumed)
		return
	}
	if !errors.Is(resumeErr, expiration.ErrBackfillPending) {
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
