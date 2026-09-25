package dm

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	maxClearRequestBytes   = int64(1024)
	errMsgVisibilityFailed = "Failed to update conversation visibility"
)

// ponytail: 64 fixed locks bound memory; collisions serialize unrelated
// visibility requests. This assumes one control-plane process; multiple
// replicas need durable ordering.
var dmVisibilityPublicationLocks [64]sync.Mutex

func lockDMVisibilityPublication(actorID, conversationID uuid.UUID) func() {
	stripe := uint(actorID[0]) ^ uint(conversationID[0])
	lock := &dmVisibilityPublicationLocks[stripe%uint(len(dmVisibilityPublicationLocks))]
	lock.Lock()
	return lock.Unlock
}

type clearConversationRequest struct {
	CurrentPassword string `json:"current_password"`
	MFACode         string `json:"mfa_code"`
}

// HideConversation hides a conversation from the caller's list without
// changing history or read state.
func (h *Handler) HideConversation(c *gin.Context) {
	h.setConversationHidden(c, true)
}

// UnhideConversation restores a hidden conversation to the caller's list.
func (h *Handler) UnhideConversation(c *gin.Context) {
	h.setConversationHidden(c, false)
}

func (h *Handler) setConversationHidden(c *gin.Context, hidden bool) {
	actorID, conversationID, ok := visibilityRequestIDs(c)
	if !ok {
		return
	}
	userID, convID := actorID.String(), conversationID.String()
	unlockPublication := lockDMVisibilityPublication(actorID, conversationID)
	defer unlockPublication()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin DM visibility transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}
	defer rollbackVisibilityTx(h, tx)
	if guardErr := credepoch.GuardTx(ctx, tx, userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		h.respondGuardTxError(c, guardErr, errMsgVisibilityFailed)
		return
	}
	if !h.lockVisibilityConversation(ctx, c, tx, userID, convID) {
		return
	}

	var hiddenAt sql.NullTime
	query := `UPDATE dm_participants SET hidden_at = NULL
		WHERE conversation_id = $1 AND user_id = $2 RETURNING hidden_at`
	if hidden {
		query = `UPDATE dm_participants SET hidden_at = COALESCE(hidden_at, clock_timestamp())
			WHERE conversation_id = $1 AND user_id = $2 RETURNING hidden_at`
	}
	if err := tx.QueryRowContext(ctx, query, convID, userID).Scan(&hiddenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return
		}
		h.log.Error("Failed to update DM visibility", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit DM visibility transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}

	var value *time.Time
	if hiddenAt.Valid {
		value = &hiddenAt.Time
	}
	if h.afterDMVisibilityCommitHook != nil {
		h.afterDMVisibilityCommitHook()
	}
	h.emitDMVisibility(userID, "dm_conversation_hidden", convID, "hidden_at", value)
	c.JSON(http.StatusOK, gin.H{"conversation_id": convID, "hidden_at": value})
}

// ClearConversation records an actor-only history range. Step-up is required
// when the actor has not explicitly disabled purge protection; MFA replaces
// password when enabled and is verified on this transaction.
func (h *Handler) ClearConversation(c *gin.Context) {
	actorID, conversationID, ok := visibilityRequestIDs(c)
	if !ok {
		return
	}
	userID, convID := actorID.String(), conversationID.String()
	req, ok := bindClearRequest(c)
	if !ok {
		return
	}
	unlockPublication := lockDMVisibilityPublication(actorID, conversationID)
	defer unlockPublication()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin DM clear transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}
	defer rollbackVisibilityTx(h, tx)
	subject, subjectErr := h.loadClearStepUpSubject(ctx, tx, userID, middleware.TokenCredentialEpoch(c))
	if subjectErr != nil {
		h.logClearStepUpFailure(subjectErr)
		subjectErr.Write(c)
		return
	}
	requireAuth, settingsErr := requireClearAuth(ctx, tx, userID)
	if settingsErr != nil {
		h.log.Error("Failed to read clear privacy setting", "error", settingsErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}
	if !h.lockVisibilityConversation(ctx, c, tx, userID, convID) {
		return
	}
	if !h.lockVisibilityParticipant(ctx, c, tx, userID, convID) {
		return
	}
	if !h.enforceClearStepUp(ctx, c, tx, userID, subject, req, requireAuth) {
		return
	}
	var cutoff time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
		h.log.Error("Failed to obtain DM clear cutoff", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}
	if err := InsertClearRange(ctx, tx, userID, convID, cutoff); err != nil {
		h.log.Error("Failed to store DM clear range", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return
	}
	if err := h.commitClear(tx); err != nil {
		if !h.recoverAmbiguousClearCommit(c.Request.Context(), userID, convID, cutoff) {
			h.log.Error("Failed to commit DM clear transaction", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
			return
		}
	}
	if h.afterDMVisibilityCommitHook != nil {
		h.afterDMVisibilityCommitHook()
	}
	h.emitDMVisibility(userID, "dm_conversation_cleared", convID, "cleared_at", &cutoff)
	c.JSON(http.StatusOK, gin.H{"conversation_id": convID, "cleared_at": cutoff})
}

// bindClearRequest applies the bounded strict-JSON body rule (backend.md § Gin
// Conventions): an oversized body is a 413 first, and anything but exactly one
// JSON object is a 400. On refusal it has written the response.
func bindClearRequest(c *gin.Context) (clearConversationRequest, bool) {
	var req clearConversationRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxClearRequestBytes)
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Request body too large"})
			return req, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return req, false
	}
	body, ok := c.Get(gin.BodyBytesKey)
	bodyBytes, bodyIsBytes := body.([]byte)
	if !ok || !bodyIsBytes || !json.Valid(bodyBytes) || !bytes.HasPrefix(bytes.TrimSpace(bodyBytes), []byte("{")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return req, false
	}
	return req, true
}

func (h *Handler) commitClear(tx *sql.Tx) error {
	if h.clearCommitForTest != nil {
		return h.clearCommitForTest(tx)
	}
	return tx.Commit()
}

// recoverAmbiguousClearCommit recognizes only the exact range this request
// wrote. The publication gate remains held, so a later Clear by this actor
// cannot turn another range into a false receipt.
func (h *Handler) recoverAmbiguousClearCommit(requestCtx context.Context, userID, convID string, cutoff time.Time) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), 3*time.Second)
	defer cancel()

	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin DM clear receipt transaction", "error", err)
		return false
	}
	defer rollbackVisibilityTx(h, tx)

	var participantID string
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM dm_participants WHERE user_id = $1 AND conversation_id = $2 FOR SHARE`, userID, convID,
	).Scan(&participantID); err != nil {
		h.log.Error("Failed to lock DM clear receipt participant", "error", err)
		return false
	}

	var found bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM dm_message_hidden_ranges
			WHERE user_id = $1 AND conversation_id = $2
			  AND includes_own = TRUE
			  AND hidden_from = '-infinity'::timestamptz
			  AND hidden_to = $3
		)`, userID, convID, cutoff,
	).Scan(&found); err != nil {
		h.log.Error("Failed to read DM clear receipt", "error", err)
		return false
	}
	if !found {
		return false
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to close DM clear receipt transaction", "error", err)
		return false
	}
	return true
}

func visibilityRequestIDs(c *gin.Context) (actorID, conversationID uuid.UUID, ok bool) {
	actorID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCallerID})
		return uuid.Nil, uuid.Nil, false
	}
	conversationID, err = uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return uuid.Nil, uuid.Nil, false
	}
	return actorID, conversationID, true
}

func rollbackVisibilityTx(h *Handler, tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		h.log.Error("Failed to roll back DM visibility transaction", "error", err)
	}
}

func (h *Handler) lockVisibilityConversation(ctx context.Context, c *gin.Context, tx *sql.Tx, userID, convID string) bool {
	var id string
	if err := tx.QueryRowContext(ctx, `
		SELECT c.id
		FROM dm_conversations AS c
		WHERE c.id = $1
		  AND EXISTS (
			SELECT 1
			FROM dm_participants AS p
			WHERE p.user_id = $2 AND p.conversation_id = c.id
		  )
		FOR NO KEY UPDATE OF c
	`, convID, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return false
		}
		h.log.Error("Failed to lock DM visibility conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return false
	}
	return true
}

func (h *Handler) lockVisibilityParticipant(ctx context.Context, c *gin.Context, tx *sql.Tx, userID, convID string) bool {
	var id string
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM dm_participants WHERE user_id = $1 AND conversation_id = $2 FOR UPDATE`, userID, convID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgConversationNotFound})
			return false
		}
		h.log.Error("Failed to lock DM visibility participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgVisibilityFailed})
		return false
	}
	return true
}

func requireClearAuth(ctx context.Context, tx *sql.Tx, userID string) (bool, error) {
	var required bool
	err := tx.QueryRowContext(ctx,
		`SELECT require_auth_before_purge FROM privacy_settings WHERE user_id = $1 FOR SHARE`, userID).Scan(&required)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return required, err
}

// loadClearStepUpSubject takes the users lock once, as the transaction's first
// statement, through the shared stepup.LockSubjectTx: FOR NO KEY UPDATE (Clear
// needs an exclusive subject lock for transactional MFA redemption, so
// GuardTx's FOR SHARE would be an unsafe same-row upgrade under concurrent
// Clears), the credential-epoch fence, then the P1 factor set read from the
// factor tables after the lock. A stale epoch is a 401, a deleted account a
// 401, any read failure a 500 with Cause for the caller to log.
func (h *Handler) loadClearStepUpSubject(ctx context.Context, tx *sql.Tx, userID, tokenEpoch string) (stepup.Subject, *stepup.Error) {
	return stepup.LockSubjectTx(ctx, tx, userID, stepup.LockForNoKeyUpdate, tokenEpoch)
}

var clearStepUpCopy = stepup.Copy{
	NoFactors:          "Clear history requires verification, but this account has no password and no MFA method. Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
	CredentialRequired: "Current password required to clear history",
}

func (h *Handler) verifyClearStepUp(ctx context.Context, tx *sql.Tx, userID string, subject stepup.Subject, req clearConversationRequest) *stepup.Error {
	if subject.MFAEnabled {
		if h.mfaVerifier == nil {
			return &stepup.Error{Status: http.StatusInternalServerError, Body: gin.H{"error": stepup.ErrMsgVerificationFailed}}
		}
		return stepup.VerifyMFAFactorTx(ctx, tx, h.mfaVerifier, userID, req.MFACode, subject.MFAMethods)
	}
	return stepup.VerifyPasswordFactor(subject, req.CurrentPassword, clearStepUpCopy)
}

// enforceClearStepUp verifies the step-up only when the actor's
// require_auth_before_purge setting asks for it. On refusal it has logged and
// written the response and returns false.
func (h *Handler) enforceClearStepUp(ctx context.Context, c *gin.Context, tx *sql.Tx, userID string, subject stepup.Subject, req clearConversationRequest, requireAuth bool) bool {
	if !requireAuth {
		return true
	}
	if stepUpErr := h.verifyClearStepUp(ctx, tx, userID, subject, req); stepUpErr != nil {
		h.logClearStepUpFailure(stepUpErr)
		stepUpErr.Write(c)
		return false
	}
	return true
}

func (h *Handler) logClearStepUpFailure(e *stepup.Error) {
	if e != nil && e.Cause != nil {
		h.log.Error("DM clear step-up failed", "status", e.Status, "error", e.Cause)
	}
}

func (h *Handler) emitDMVisibility(userID, eventType, convID, timestampKey string, value *time.Time) {
	if h.hub == nil {
		return
	}
	actor, err := uuid.Parse(userID)
	if err != nil {
		return
	}
	h.hub.BroadcastToUser(actor, websocket.OutgoingMessage{Type: eventType, Data: map[string]interface{}{
		"conversation_id": convID, timestampKey: value,
	}})
}
