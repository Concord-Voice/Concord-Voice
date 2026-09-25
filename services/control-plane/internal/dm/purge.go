package dm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

const errMsgPurgeFailed = "Purge failed"

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
	// One deadline covers every read and write below, as in the channel and
	// server purges (#2344).
	purgeCtx, cancel := context.WithTimeout(c.Request.Context(), purge.SynchronousRunTimeout)
	defer cancel()

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

	isGroup, isAdmin, err := h.resolveDMRole(purgeCtx, convID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}
	if err != nil {
		h.log.Error("DM purge role lookup failed", "error", err, "conversation_id", convID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return
	}

	// Step-up BEFORE any mutation. Failure paths inside write their own response.
	if h.requireAuthBeforePurge(purgeCtx, userID) {
		if !h.verifyPurgeStepUp(purgeCtx, c, userID, req.CurrentPassword, req.MFACode) {
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
		// With a hide to follow, the audit row stays in_progress until
		// applyReceiverHide completes it in the hide's own transaction.
		DeferCompletion: hide,
	}

	res, err := h.purgeEngine.Run(purgeCtx, plan)
	if err != nil {
		h.log.Error("DM purge failed", "error", err, "conversation_id", convID, "deleted", res.DeletedCount)
		h.failPartialPurge(c, convID, userID, req.Range, res.DeletedCount)
		return
	}

	if hide {
		hidden, err := h.applyReceiverHide(purgeCtx, userID, convID, rangeFrom, res.PurgeID, res.DeletedCount)
		if err != nil {
			h.log.Error("DM purge hide failed", "error", err, "conversation_id", convID, "deleted", res.DeletedCount)
			h.failPartialPurge(c, convID, userID, req.Range, res.DeletedCount)
			return
		}
		res.HiddenCount = hidden
	}

	h.log.Info("DM conversation purged", "conversation_id", convID, "actor", userID,
		"deleted", res.DeletedCount, "hidden", res.HiddenCount)
	h.emitDMPurged(convID, userID, res.DeletedCount, req.Range)
	c.JSON(http.StatusOK, gin.H{"deleted_count": res.DeletedCount, "hidden_count": res.HiddenCount})
}

// failPartialPurge answers a purge that stopped partway. Deletes that already
// committed cannot be undone, so peers and the actor's other sessions still get
// dm_purged for them — the same rule the channel and server purges follow.
func (h *Handler) failPartialPurge(c *gin.Context, convID, actorID, rng string, deleted int) {
	if deleted > 0 {
		h.emitDMPurged(convID, actorID, deleted, rng)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
}

// applyReceiverHide records the actor's hidden window for messages they cannot
// delete-for-both and returns how many of the peers' messages it covers. The hide
// and the audit completion commit in one transaction, so a failure leaves the
// audit row in_progress with the committed deleted_count — the recovery handle
// for the accepted delete/hide TOCTOU (spec §7).
func (h *Handler) applyReceiverHide(ctx context.Context, userID, convID string, rangeFrom *time.Time, purgeID string, deleted int) (int, error) {
	// Hidden window: [range cutoff (or epoch for All Time), now].
	from := time.Time{}
	if rangeFrom != nil {
		from = rangeFrom.UTC()
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin hide tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back DM purge hide transaction", "error", rollbackErr)
		}
	}()

	// Take the FK parents before the participant row. Account erasure locks users
	// before its conversations, and group deletion locks the conversation before
	// removing participants.
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR KEY SHARE`, userID).Scan(&lockedUserID); err != nil {
		return 0, fmt.Errorf("lock hide user: %w", err)
	}
	var lockedConversationID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM dm_conversations WHERE id = $1 FOR KEY SHARE`, convID).Scan(&lockedConversationID); err != nil {
		return 0, fmt.Errorf("lock hide conversation: %w", err)
	}

	hidden, err := InsertHiddenRange(ctx, tx, userID, convID, from, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("insert hidden range: %w", err)
	}
	if err := h.purgeEngine.FinalizeHiddenTx(ctx, tx, purgeID, deleted, hidden); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit hide tx: %w", err)
	}
	return hidden, nil
}

// resolveDMRole resolves the actor's group flag and admin role in one query.
// sql.ErrNoRows means the actor is not a participant; any other error is a
// failed lookup. Both deny the purge.
func (h *Handler) resolveDMRole(ctx context.Context, convID, userID string) (isGroup, isAdmin bool, err error) {
	var role string
	err = h.db.QueryRowContext(ctx, `
		SELECT dc.is_group, dp.role
		FROM dm_conversations dc
		INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, userID).Scan(&isGroup, &role)
	if err != nil {
		return false, false, err
	}
	return isGroup, role == "admin", nil
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
		if !errors.Is(err, sql.ErrNoRows) {
			h.log.Error("Purge step-up setting lookup failed; requiring step-up", "error", err)
		}
		return true // fail-closed: no row OR query error → require step-up
	}
	return v
}

// purgeStepUpCopy is the DM/group wording for the two call-site-specific
// strings. Byte-identical to the pre-extraction messages (#1352 review
// finding S1) — the client discriminates on them.
var purgeStepUpCopy = stepup.Copy{
	NoFactors:          "Bulk deletion requires verification, but this account has no password and no MFA method. Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
	CredentialRequired: "Current password required to purge messages",
}

// verifyPurgeStepUp verifies the actor's identity before a DM/group purge:
// current password + MFA when enabled. Writes the error response and returns
// false on any failure — the caller must return without mutating.
//
// Policy lives in internal/stepup (#2765); this is the DM binding of it. There
// is no enclosing transaction here, so the non-tx MFA form is correct.
func (h *Handler) verifyPurgeStepUp(ctx context.Context, c *gin.Context, userID, currentPassword, mfaCode string) bool {
	// LoadSubject derives MFA from the factor tables (policy P1) and fails
	// closed on a read error; it no longer consults IsEnabled.
	subj, sErr := stepup.LoadSubject(ctx, h.db, userID)
	if sErr != nil {
		h.logPurgeStepUpFailure(sErr)
		sErr.Write(c)
		return false
	}
	if pErr := stepup.VerifyPasswordFactor(subj, currentPassword, purgeStepUpCopy); pErr != nil {
		h.logPurgeStepUpFailure(pErr)
		pErr.Write(c)
		return false
	}
	if subj.MFAEnabled {
		if mErr := stepup.VerifyMFAFactor(ctx, h.mfaVerifier, userID, mfaCode, subj.MFAMethods); mErr != nil {
			h.logPurgeStepUpFailure(mErr)
			mErr.Write(c)
			return false
		}
	}
	return true
}

// logPurgeStepUpFailure records the root cause of a step-up 500. A 4xx carries
// no Cause and is not logged: a rejected credential is a normal outcome, and
// logging one would turn ordinary user error into error-level noise while
// hinting at which accounts are being probed.
//
// Cause never contains a credential or a password hash — internal/stepup
// discards the password-verification error in favour of a fixed sentinel for
// exactly that reason ([internal]rules/observability.md Core principle #1).
func (h *Handler) logPurgeStepUpFailure(e *stepup.Error) {
	if e.Cause == nil {
		return
	}
	h.log.Error("Purge step-up failed", "status", e.Status, "error", e.Cause)
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
