package dm

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

const (
	errMsgPurgeFailed        = "Purge failed"
	errMsgVerificationFailed = "Verification failed"
)

// dmPurgeRequest is the body for DELETE /dm/conversations/:id/messages (#1352).
// current_password/mfa_code are the step-up factors, required when the acting
// user's privacy_settings.require_auth_before_purge is true (the default).
type dmPurgeRequest struct {
	Range           string `json:"range" binding:"required"`
	CurrentPassword string `json:"current_password"`
	MFACode         string `json:"mfa_code"`
}

// PurgeConversation handles DELETE /dm/conversations/:id/messages — bulk-delete a
// 1:1 or group DM's messages, scoped by time range (#1352).
//
// Authorization (spec §5):
//   - 1:1: either participant. Own messages are deleted-for-both; the other
//     party's messages are persistently HIDDEN from the actor's view only.
//   - Group non-admin: own deleted-for-both; others hidden.
//   - Group admin (dm_participants.role='admin'): ALL messages deleted-for-both.
//   - Non-participant → 403, no mutation, no audit row.
//
// Step-up auth (spec §6): when the actor's require_auth_before_purge is true
// (fail-closed default), current_password (+ MFA if enabled) is verified BEFORE
// any mutation. DM/group only — server purges are RBAC-gated, never step-up.
func (h *Handler) PurgeConversation(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")
	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	var req dmPurgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	rangeFrom, err := purge.ParseRange(req.Range)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid range"})
		return
	}

	isGroup, isAdmin, isParticipant := h.resolveDMRole(c.Request.Context(), convID, userID)
	if !isParticipant {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Step-up BEFORE any mutation. Failure paths inside write their own response.
	if h.requireAuthBeforePurge(c.Request.Context(), userID) {
		if !h.verifyPurgeStepUp(c, userID, req.CurrentPassword, req.MFACode) {
			return
		}
	}

	ctxType := purge.ContextDM
	if isGroup {
		ctxType = purge.ContextGroup
	}
	var author *string
	hide := false
	if isGroup && isAdmin {
		author = nil // group admin: delete everyone's messages for both
	} else {
		self := userID
		author = &self // delete own for both...
		hide = true    // ...and hide the rest from the actor's view
	}

	plan := purge.Plan{
		ContextType: ctxType,
		ContextID:   convID,
		ActorID:     userID,
		Reason:      "manual",
		RangeFrom:   rangeFrom,
		Deletes: []purge.DeleteSpec{{
			MessagesTable:    "dm_messages",
			ScopeColumn:      "conversation_id",
			ScopeID:          convID,
			AttachmentsTable: "dm_message_attachments",
			Author:           author,
		}},
	}
	res, err := h.purgeEngine.Run(c.Request.Context(), plan)
	if err != nil {
		h.log.Error("DM purge failed", "error", err, "conversation_id", convID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return
	}

	if hide {
		hidden, err := h.applyReceiverHide(c.Request.Context(), userID, convID, rangeFrom, res.PurgeID)
		if err != nil {
			h.log.Error("DM purge hide failed", "error", err, "conversation_id", convID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
			return
		}
		res.HiddenCount = hidden
	}

	h.log.Info("DM conversation purged", "conversation_id", convID, "actor", userID,
		"deleted", res.DeletedCount, "hidden", res.HiddenCount)
	h.emitDMPurged(convID, userID, res.DeletedCount, req.Range)
	c.JSON(http.StatusOK, gin.H{"deleted_count": res.DeletedCount, "hidden_count": res.HiddenCount})
}

// applyReceiverHide records the actor's hidden window for messages they cannot
// delete-for-both and returns how many of the peers' messages it covers. The hide
// runs in its own transaction; a failure leaves the audit row at in_progress, which
// is the recovery handle for the accepted delete/hide TOCTOU (spec §7).
func (h *Handler) applyReceiverHide(ctx context.Context, userID, convID string, rangeFrom *time.Time, purgeID string) (int, error) {
	// Hidden window: [range cutoff (or epoch for All Time), now].
	from := time.Time{}
	if rangeFrom != nil {
		from = rangeFrom.UTC()
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin hide tx: %w", err)
	}
	hidden, err := InsertHiddenRange(ctx, tx, userID, convID, from, time.Now().UTC())
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("insert hidden range: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit hide tx: %w", err)
	}

	if err := h.purgeEngine.FinalizeHidden(ctx, purgeID, hidden); err != nil {
		// Audit enrichment only — the hide itself is committed. Log, don't fail.
		h.log.Error("DM purge: finalize hidden_count failed", "error", err, "purge_id", purgeID)
	}
	return hidden, nil
}

// resolveDMRole resolves (isGroup, isAdmin, isParticipant) for one query round-trip.
// Errors fail closed (not a participant).
func (h *Handler) resolveDMRole(ctx context.Context, convID, userID string) (isGroup, isAdmin, isParticipant bool) {
	var role string
	err := h.db.QueryRowContext(ctx, `
		SELECT dc.is_group, dp.role
		FROM dm_conversations dc
		INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, userID).Scan(&isGroup, &role)
	if err != nil {
		return false, false, false
	}
	return isGroup, role == "admin", true
}

// requireAuthBeforePurge reads the actor's privacy_settings.require_auth_before_purge.
//
// SECURITY (review finding M7): FAIL-CLOSED. privacy_settings rows are created lazily
// (only on a settings PATCH), so most users have NO row — and a transient query error
// must not silently skip step-up on a destructive operation. Missing row OR any error
// → step-up required. Do NOT mirror the friendsOfFriendsEnabled false-on-ErrNoRows shape.
func (h *Handler) requireAuthBeforePurge(ctx context.Context, userID string) bool {
	var v bool
	err := h.db.QueryRowContext(ctx,
		`SELECT require_auth_before_purge FROM privacy_settings WHERE user_id = $1`,
		userID).Scan(&v)
	if err != nil {
		return true // fail-closed: no row OR query error → require step-up
	}
	return v
}

// verifyPurgeStepUp verifies the actor's identity before a DM/group purge, mirroring
// the E2EE key-reset step-up (#1293): current password + MFA when enabled. Writes the
// error response and returns false on any failure — the caller must return without
// mutating. SSO/passwordless accounts (empty password_hash) step up with their enabled
// MFA method instead of a password; with neither factor available the request is
// rejected with an actionable message, never a raw 500 (review finding S1).
func (h *Handler) verifyPurgeStepUp(c *gin.Context, userID, currentPassword, mfaCode string) bool {
	ctx := c.Request.Context()

	var passwordHash string
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(password_hash, '') FROM users WHERE id = $1`, userID).Scan(&passwordHash); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVerificationFailed})
		return false
	}

	mfaEnabled := h.mfaVerifier != nil && h.mfaVerifier.IsEnabled(ctx, userID)

	if !h.verifyPurgePasswordFactor(c, passwordHash, currentPassword, mfaEnabled) {
		return false
	}
	if mfaEnabled && !h.verifyPurgeMFAFactor(c, userID, mfaCode) {
		return false
	}
	return true
}

// verifyPurgePasswordFactor checks the password half of the purge step-up. An empty
// passwordHash means an SSO/passwordless account: MFA becomes the only available
// factor, so this passes iff MFA is enabled — otherwise the actor is told how to
// proceed rather than receiving a raw 500 (review finding S1). Writes the error
// response and returns false on failure.
func (h *Handler) verifyPurgePasswordFactor(c *gin.Context, passwordHash, currentPassword string, mfaEnabled bool) bool {
	if passwordHash == "" {
		if !mfaEnabled {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Bulk deletion requires verification, but this account has no password and no MFA method. Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
			})
			return false
		}
		return true // passwordless: MFA carries the step-up
	}

	if currentPassword == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Current password required to purge messages", "password_required": true})
		return false
	}
	match, err := auth.VerifyPassword(currentPassword, passwordHash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVerificationFailed})
		return false
	}
	if !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid password"})
		return false
	}
	return true
}

// verifyPurgeMFAFactor checks the MFA half of the purge step-up (only called when the
// actor has MFA enabled). Writes the error response and returns false on failure.
func (h *Handler) verifyPurgeMFAFactor(c *gin.Context, userID, mfaCode string) bool {
	ctx := c.Request.Context()
	if mfaCode == "" {
		methods, _ := h.mfaVerifier.GetEnabledMethods(ctx, userID)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "MFA verification required", "mfa_required": true, "methods": methods,
		})
		return false
	}
	valid, err := h.mfaVerifier.VerifyCode(ctx, userID, mfaCode)
	if err != nil {
		h.log.Error("Purge step-up: MFA verify failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVerificationFailed})
		return false
	}
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
		return false
	}
	return true
}

// emitDMPurged broadcasts the bulk-purge event to the conversation's other
// participants and to the actor's own other sessions (multi-device prune).
// Payload carries counts and context only — never message content. Hides do NOT
// broadcast to peers (they are view-local to the actor).
func (h *Handler) emitDMPurged(convID, actorID string, count int, rng string) {
	if h.hub == nil {
		return
	}
	msg := websocket.OutgoingMessage{
		Type: "dm_purged",
		Data: map[string]interface{}{
			"conversation_id": convID,
			"purged_by":       actorID,
			"deleted_count":   count,
			"range":           rng,
		},
	}
	h.broadcastToDMParticipants(convID, actorID, msg)
	// BroadcastToUser takes uuid.UUID — parse the actor id (review finding M5).
	if actorUUID, err := uuid.Parse(actorID); err == nil {
		h.hub.BroadcastToUser(actorUUID, msg)
	}
}
