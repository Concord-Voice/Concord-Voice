// Package ownership provides handlers for server ownership transfer.
package ownership

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	transferPendingDuration = 24 * time.Hour
	reversalWindowDuration  = 24 * time.Hour
	ownershipAuditTimeout   = 5 * time.Second

	errMsgInvalidServerID        = "Invalid server ID"
	errMsgServerNotFound         = "Server not found"
	errMsgFailedQueryOwner       = "Failed to query server owner"
	errMsgFailedVerifyOwnership  = "Failed to verify ownership"
	errMsgFailedVerifyPassword   = "Failed to verify password"
	errMsgFailedReverseTransfer  = "Failed to reverse transfer"
	errMsgFailedInitiateTransfer = "Failed to initiate transfer"
	errMsgFailedCancelTransfer   = "Failed to cancel transfer"
	errMsgTransferAlreadyPending = "A transfer is already pending for this server"

	keyServerID   = "server_id"
	keyUserID     = "user_id"
	keyToUserID   = "to_user_id"
	keyFromUserID = "from_user_id"
	keyExpiresAt  = "expires_at"
	keyServer     = "server"
	keyTransferID = "transfer_id"
	keyMessage    = "message"
)

// Handler handles ownership transfer requests.
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	redis       *redis.Client
	cache       *rbac.PermissionCache
	audit       *rbac.AuditWriter
	emailSvc    *email.Service
	mfaVerifier mfa.Verifier
	// voiceEnforcer pushes recomputed permissions to voice-connected members
	// after an ownership change (CV-CAN-007 review P1) — the largest single
	// permission delta in the system (the resolver short-circuits the owner to
	// OwnerPermissions). Wired via SetVoiceEnforcer; nil means no push.
	voiceEnforcer rbac.VoiceEnforcer
	// presenceRecheck captures the pre-mutation Server Voice audience and
	// refreshes it after an ownership change commits. It is wired at router
	// construction; a nil value preserves the isolated-handler test default.
	presenceRecheck ownershipPresenceRecheck
}

type ownershipPresenceRecheck interface {
	rbac.PresenceRecheck
	PrepareCaptureStrict(
		ctx context.Context, serverID string, channelIDs []string, onlyUserID *string,
	) (rbac.PresenceRecheckPlan, error)
}

// SetVoiceEnforcer wires the mid-session voice permission push. Called once at
// router construction, before the handler serves traffic.
func (h *Handler) SetVoiceEnforcer(e rbac.VoiceEnforcer) {
	h.voiceEnforcer = e
}

// SetPresenceRecheck wires Rich Presence reconciliation for ownership changes.
func (h *Handler) SetPresenceRecheck(p ownershipPresenceRecheck) {
	h.presenceRecheck = p
}

// HasPresenceRecheck reports whether Rich Presence reconciliation was wired.
func (h *Handler) HasPresenceRecheck() bool {
	return h.presenceRecheck != nil
}

// recheckVoiceBothParties re-pushes permissions for the two sides of an
// ownership change who may be sitting in a voice channel right now.
func (h *Handler) recheckVoiceBothParties(serverID, fromUserID, toUserID string) {
	if h.voiceEnforcer == nil {
		return
	}
	h.voiceEnforcer.RecheckUser(serverID, fromUserID)
	h.voiceEnforcer.RecheckUser(serverID, toUserID)
}

func (h *Handler) presenceExecute(plan rbac.PresenceRecheckPlan) {
	if h.presenceRecheck == nil || plan == nil || !plan.HasWork() {
		return
	}
	h.presenceRecheck.Execute(plan)
}

func (h *Handler) presenceAbandon(plan rbac.PresenceRecheckPlan, cause string) {
	if h.presenceRecheck == nil || plan == nil || !plan.HasWork() {
		return
	}
	h.presenceRecheck.Abandon(plan, cause)
}

// HandlerDeps groups the dependencies required to construct a Handler.
type HandlerDeps struct {
	DB          *sql.DB
	Log         *logger.Logger
	Hub         *websocket.Hub
	Redis       *redis.Client
	Cache       *rbac.PermissionCache
	Audit       *rbac.AuditWriter
	EmailSvc    *email.Service
	MFAVerifier mfa.Verifier
}

// NewHandler creates a new ownership transfer handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		db:          deps.DB,
		log:         deps.Log,
		hub:         deps.Hub,
		redis:       deps.Redis,
		cache:       deps.Cache,
		audit:       deps.Audit,
		emailSvc:    deps.EmailSvc,
		mfaVerifier: deps.MFAVerifier,
	}
}

// initiateTransferRequest is the request body for InitiateTransfer.
type initiateTransferRequest struct {
	TargetUserID string `json:"target_user_id" binding:"required,uuid"`
	Password     string `json:"password" binding:"required"` //nolint:gosec // G117: request binding field, not a credential
	MFACode      string `json:"mfa_code"`
}

// InitiateTransfer starts an ownership transfer to a target user.
// POST /servers/:id/transfer-ownership
func (h *Handler) InitiateTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var req initiateTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.TargetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot transfer ownership to yourself"})
		return
	}

	if err := h.requireServerOwner(ctx, c, serverID, userID, "transfer ownership"); err != nil {
		return
	}

	if err := h.verifyPassword(c, userID, req.Password); err != nil {
		return
	}

	if err := h.verifyMFA(c, userID, req.MFACode); err != nil {
		return
	}

	if err := h.requireMembership(ctx, c, serverID, req.TargetUserID); err != nil {
		return
	}

	if err := h.requireNoPendingTransfer(ctx, c, serverID); err != nil {
		return
	}

	reversalToken, err := generateReversalToken()
	if err != nil {
		h.log.Error("Failed to generate reversal token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedInitiateTransfer})
		return
	}

	rec := &transferRecord{
		id:            uuid.New().String(),
		serverID:      serverID,
		fromUserID:    userID,
		toUserID:      req.TargetUserID,
		reversalToken: reversalToken,
		requestedAt:   time.Now(),
		expiresAt:     time.Now().Add(transferPendingDuration),
	}

	if err := h.insertTransferRecord(ctx, c, rec); err != nil {
		return
	}

	h.sendTransferEmail(ctx, userID, serverID, req.TargetUserID, reversalToken)

	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transfer_initiated",
			Data: map[string]interface{}{
				keyServerID:   serverID,
				keyFromUserID: userID,
				keyToUserID:   req.TargetUserID,
				keyExpiresAt:  rec.expiresAt.Format(time.RFC3339),
			},
		})
	}

	if err := h.audit.Log(ctx, serverID, &userID, "ownership_transfer_initiated", keyServer, &serverID, map[string]interface{}{
		keyToUserID:  req.TargetUserID,
		keyExpiresAt: rec.expiresAt.Format(time.RFC3339),
	}); err != nil {
		h.log.Error("Failed to write audit log for ownership_transfer_initiated", "error", err, "server_id", serverID)
	}

	c.JSON(http.StatusCreated, gin.H{
		keyTransferID: rec.id,
		keyServerID:   serverID,
		keyFromUserID: userID,
		keyToUserID:   req.TargetUserID,
		"status":      "pending",
		keyExpiresAt:  rec.expiresAt.Format(time.RFC3339),
	})
}

// GetTransferStatus returns the current transfer status for a server.
// GET /servers/:id/transfer-ownership
func (h *Handler) GetTransferStatus(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify caller is a member
	var isMember bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to verify server membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify membership"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Fetch pending transfer
	var transferID, fromUserID, toUserID, status string
	var requestedAt, expiresAt time.Time
	err = h.db.QueryRowContext(ctx, `
		SELECT id, from_user_id, to_user_id, status, requested_at, expires_at
		FROM ownership_transfers
		WHERE server_id = $1 AND status = 'pending'
		ORDER BY requested_at DESC LIMIT 1
	`, serverID).Scan(&transferID, &fromUserID, &toUserID, &status, &requestedAt, &expiresAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, gin.H{"transfer": nil})
		return
	}
	if err != nil {
		h.log.Error("Failed to query transfer status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transfer status"})
		return
	}

	resp := gin.H{
		keyTransferID:  transferID,
		"status":       status,
		"requested_at": requestedAt.Format(time.RFC3339),
		keyExpiresAt:   expiresAt.Format(time.RFC3339),
	}

	// Only owner and target see the to_user_id
	if userID == fromUserID || userID == toUserID {
		resp[keyFromUserID] = fromUserID
		resp[keyToUserID] = toUserID
	}

	c.JSON(http.StatusOK, gin.H{"transfer": resp})
}

// CancelTransfer cancels a pending ownership transfer.
// DELETE /servers/:id/transfer-ownership
func (h *Handler) CancelTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin transfer cancellation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCancelTransfer})
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back transfer cancellation", "error", rollbackErr)
		}
	}()

	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
			return
		}
		h.internalError(c, errMsgFailedQueryOwner, errMsgFailedVerifyOwnership, err)
		return
	}
	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can cancel the transfer"})
		return
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE ownership_transfers
		SET status = 'cancelled', cancelled_at = NOW()
		WHERE server_id = $1 AND status = 'pending'
	`, serverID)
	if err != nil {
		h.log.Error(errMsgFailedCancelTransfer, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCancelTransfer})
		return
	}

	rows, err := res.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read transfer cancellation result", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCancelTransfer})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "No pending transfer to cancel"})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transfer cancellation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCancelTransfer})
		return
	}

	// Broadcast
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transfer_cancelled",
			Data: map[string]interface{}{
				keyServerID: serverID,
			},
		})
	}

	// Audit log
	if err := h.audit.Log(ctx, serverID, &userID, "ownership_transfer_cancelled", keyServer, &serverID, nil); err != nil {
		h.log.Error("Failed to write audit log for ownership_transfer_cancelled", "error", err, "server_id", serverID)
	}

	c.JSON(http.StatusOK, gin.H{keyMessage: "Transfer cancelled"})
}

// ConfirmTransfer completes a pending transfer early (owner confirms).
// POST /servers/:id/transfer-ownership/confirm
func (h *Handler) ConfirmTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	if err := h.requireServerOwner(ctx, c, serverID, userID, "confirm the transfer"); err != nil {
		return
	}

	// Fetch pending transfer
	var transferID, fromUserID, toUserID string
	err := h.db.QueryRowContext(ctx, `
		SELECT id, from_user_id, to_user_id
		FROM ownership_transfers
		WHERE server_id = $1 AND status = 'pending'
	`, serverID).Scan(&transferID, &fromUserID, &toUserID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "No pending transfer to confirm"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query pending transfer", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm transfer"})
		return
	}

	// Execute the transfer
	if err := h.executeTransfer(ctx, serverID, transferID, fromUserID, toUserID); err != nil {
		switch {
		case errors.Is(err, errTransferAlreadyCompleted):
			c.JSON(http.StatusConflict, gin.H{"error": "Transfer has already been completed or cancelled"})
		case errors.Is(err, errTransferOwnershipChanged):
			c.JSON(http.StatusConflict, gin.H{"error": "Server ownership changed; transfer was cancelled"})
		case errors.Is(err, errToUserNotMember):
			c.JSON(http.StatusConflict, gin.H{"error": "Target user is no longer a member of this server"})
		case errors.Is(err, errFromUserNotMember):
			c.JSON(http.StatusConflict, gin.H{"error": "Current owner membership record is missing"})
		default:
			h.log.Error("Failed to execute transfer", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete transfer"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		keyMessage:     "Ownership transferred",
		"new_owner_id": toUserID,
	})
}

// reverseTransferRequest is the request body for ReverseTransfer.
type reverseTransferRequest struct {
	Password string `json:"password" binding:"required"` //nolint:gosec // G117: request binding field, not a credential
	MFACode  string `json:"mfa_code"`
}

// reversalRecord holds data from a completed transfer lookup for reversal.
type reversalRecord struct {
	transferID  string
	serverID    string
	fromUserID  string
	toUserID    string
	completedAt time.Time
}

// ReverseTransfer reverses a completed transfer using the email reversal token.
// POST /ownership/reverse/:token
func (h *Handler) ReverseTransfer(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	token := c.Param("token")

	if len(token) != 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reversal token"})
		return
	}

	rec, err := h.lookupCompletedTransfer(ctx, c, token)
	if err != nil {
		return
	}

	if time.Since(rec.completedAt) > reversalWindowDuration {
		c.JSON(http.StatusGone, gin.H{"error": "Reversal window has expired"})
		return
	}

	if userID != rec.fromUserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the original owner can reverse the transfer"})
		return
	}

	var req reverseTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if err := h.verifyPassword(c, userID, req.Password); err != nil {
		return
	}

	if err := h.verifyMFA(c, userID, req.MFACode); err != nil {
		return
	}

	plan, err := h.executeReversal(ctx, rec)
	if err != nil {
		switch {
		case errors.Is(err, errReversalOwnershipChanged):
			c.JSON(http.StatusConflict, gin.H{"error": "Ownership has changed since this transfer — reversal is no longer possible"})
		case errors.Is(err, errReversalOriginalOwnerNotMember):
			c.JSON(http.StatusConflict, gin.H{"error": "Original owner is no longer a member of this server"})
		default:
			h.log.Error("Failed to reverse ownership transfer", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReverseTransfer})
		}
		return
	}

	h.reconcileOwnershipPostCommit(rec.serverID, rec.fromUserID, rec.toUserID, plan)

	if serverUUID, err := uuid.Parse(rec.serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transfer_reversed",
			Data: map[string]interface{}{
				keyServerID: rec.serverID,
				"owner_id":  rec.fromUserID,
			},
		})
	}

	if err := h.audit.Log(ctx, rec.serverID, &userID, "ownership_transfer_reversed", keyServer, &rec.serverID, map[string]interface{}{
		keyTransferID:   rec.transferID,
		"reversed_from": rec.toUserID,
	}); err != nil {
		h.log.Error("Failed to write audit log for ownership_transfer_reversed", "error", err, "server_id", rec.serverID)
	}

	c.JSON(http.StatusOK, gin.H{
		keyMessage: "Ownership transfer reversed",
		"owner_id": rec.fromUserID,
	})
}

func (h *Handler) requireServerOwner(ctx context.Context, c *gin.Context, serverID, userID, action string) error {
	var ownerID string
	err := h.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return err
	}
	if err != nil {
		h.internalError(c, errMsgFailedQueryOwner, errMsgFailedVerifyOwnership, err)
		return err
	}
	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can " + action})
		return fmt.Errorf("not owner")
	}
	return nil
}

func (h *Handler) requireMembership(ctx context.Context, c *gin.Context, serverID, targetUserID string) error {
	var exists bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, targetUserID,
	).Scan(&exists)
	if err != nil {
		h.internalError(c, "Failed to check target membership", "Failed to verify target user", err)
		return err
	}
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Target user is not a member of this server"})
		return fmt.Errorf("not a member")
	}
	return nil
}

func (h *Handler) requireNoPendingTransfer(ctx context.Context, c *gin.Context, serverID string) error {
	var exists bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM ownership_transfers WHERE server_id = $1 AND status = 'pending')`,
		serverID,
	).Scan(&exists)
	if err != nil {
		h.internalError(c, "Failed to check pending transfers", "Failed to check transfer status", err)
		return err
	}
	if exists {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgTransferAlreadyPending})
		return fmt.Errorf("pending transfer exists")
	}
	return nil
}

func generateReversalToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(tokenBytes), nil
}

type transferRecord struct {
	id            string
	serverID      string
	fromUserID    string
	toUserID      string
	reversalToken string
	requestedAt   time.Time
	expiresAt     time.Time
}

func (h *Handler) insertTransferRecord(ctx context.Context, c *gin.Context, rec *transferRecord) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.internalError(c, "Failed to begin transfer creation", errMsgFailedInitiateTransfer, err)
		return fmt.Errorf("begin transfer creation transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back transfer creation", "error", rollbackErr)
		}
	}()

	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, rec.serverID).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return h.transferOwnershipChanged(c)
		}
		h.internalError(c, "Failed to lock transfer server", errMsgFailedInitiateTransfer, err)
		return fmt.Errorf("lock transfer server: %w", err)
	}
	if ownerID != rec.fromUserID {
		return h.transferOwnershipChanged(c)
	}

	var pending bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM ownership_transfers WHERE server_id = $1 AND status = 'pending')
	`, rec.serverID).Scan(&pending); err != nil {
		h.internalError(c, "Failed to check pending transfers", "Failed to check transfer status", err)
		return fmt.Errorf("check pending transfer: %w", err)
	}
	if pending {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgTransferAlreadyPending})
		return errTransferAlreadyPending
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7)
	`, rec.id, rec.serverID, rec.fromUserID, rec.toUserID, rec.reversalToken, rec.requestedAt, rec.expiresAt); err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": errMsgTransferAlreadyPending})
			return errTransferAlreadyPending
		}
		h.internalError(c, "Failed to create transfer record", errMsgFailedInitiateTransfer, err)
		return fmt.Errorf("insert transfer record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		h.internalError(c, "Failed to commit transfer creation", errMsgFailedInitiateTransfer, err)
		return fmt.Errorf("commit transfer creation transaction: %w", err)
	}
	return nil
}

func (h *Handler) transferOwnershipChanged(c *gin.Context) error {
	c.JSON(http.StatusConflict, gin.H{"error": "Server ownership changed; retry the transfer"})
	return errTransferOwnershipChanged
}

func (h *Handler) lookupCompletedTransfer(ctx context.Context, c *gin.Context, token string) (*reversalRecord, error) {
	var rec reversalRecord
	err := h.db.QueryRowContext(ctx, `
		SELECT id, server_id, from_user_id, to_user_id, completed_at
		FROM ownership_transfers
		WHERE reversal_token = $1 AND status = 'completed'
	`, token).Scan(&rec.transferID, &rec.serverID, &rec.fromUserID, &rec.toUserID, &rec.completedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid or expired reversal token"})
		return nil, err
	}
	if err != nil {
		h.internalError(c, "Failed to query reversal token", "Failed to process reversal", err)
		return nil, err
	}
	return &rec, nil
}

// internalError logs an error and sends a 500 JSON response. Used by transactional
// helpers to reduce boilerplate in error branches.
func (h *Handler) internalError(c *gin.Context, msg, userMsg string, err error) {
	h.log.Error(msg, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": userMsg})
}

func (h *Handler) executeReversal(ctx context.Context, rec *reversalRecord) (rbac.PresenceRecheckPlan, error) {
	var reversalStillPossible bool
	if err := h.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM ownership_transfers
			JOIN servers ON servers.id = ownership_transfers.server_id
			WHERE ownership_transfers.id = $1
				AND ownership_transfers.status = 'completed'
				AND ownership_transfers.server_id = $2
				AND servers.owner_id = $3
		)
	`, rec.transferID, rec.serverID, rec.toUserID).Scan(&reversalStillPossible); err != nil {
		return nil, fmt.Errorf("preflight reversal ownership: %w", err)
	}
	if !reversalStillPossible {
		return nil, errReversalOwnershipChanged
	}

	plan, changed, err := h.withOwnershipCapture(ctx, rec.serverID, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		var transferID string
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM ownership_transfers WHERE id = $1 AND status = 'completed' FOR UPDATE
		`, rec.transferID).Scan(&transferID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, errReversalOwnershipChanged
			}
			return false, fmt.Errorf("lock completed transfer: %w", err)
		}

		var currentOwnerID string
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1`, rec.serverID).Scan(&currentOwnerID); err != nil {
			return false, fmt.Errorf("query current owner for reversal: %w", err)
		}
		if currentOwnerID != rec.toUserID {
			return false, errReversalOwnershipChanged
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE ownership_transfers
			SET status = 'cancelled', cancelled_at = NOW()
			WHERE server_id = $1 AND status = 'pending'
		`, rec.serverID); err != nil {
			return false, fmt.Errorf("cancel pending transfers for reversal: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE servers SET owner_id = $1 WHERE id = $2`, rec.fromUserID, rec.serverID); err != nil {
			return false, fmt.Errorf("update server owner for reversal: %w", err)
		}

		resFrom, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, rec.serverID, rec.fromUserID)
		if err != nil {
			return false, fmt.Errorf("update from_user role for reversal: %w", err)
		}
		n, err := resFrom.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("read from_user role result for reversal: %w", err)
		}
		if n == 0 {
			return false, errReversalOriginalOwnerNotMember
		}

		resTo, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, rec.serverID, rec.toUserID)
		if err != nil {
			return false, fmt.Errorf("update to_user role for reversal: %w", err)
		}
		if _, err := resTo.RowsAffected(); err != nil {
			return false, fmt.Errorf("read to_user role result for reversal: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ownership_transfers SET status = 'reversed', reversed_at = NOW() WHERE id = $1`, transferID); err != nil {
			return false, fmt.Errorf("mark transfer reversed: %w", err)
		}
		return true, nil
	})
	if err != nil {
		if errors.Is(err, rbac.ErrPresenceCaptureLimited) {
			if recheckErr := h.classifyReversalCaptureLimit(ctx, rec); recheckErr != nil {
				if !errors.Is(recheckErr, errReversalOwnershipChanged) && !errors.Is(recheckErr, errReversalOriginalOwnerNotMember) {
					h.log.Error("ownership reversal capture-limit classification failed",
						"failure_class", "ownership_reversal_capture_limit_classification",
						"error", recheckErr,
					)
					return nil, errors.Join(errReversalOwnershipChanged, recheckErr)
				}
				return nil, recheckErr
			}
		}
		return nil, err
	}
	if !changed {
		return nil, errReversalOwnershipChanged
	}
	return plan, nil
}

// classifyReversalCaptureLimit distinguishes an ownership change that raced
// capture from a current capture-limit failure without mutating any state.
func (h *Handler) classifyReversalCaptureLimit(ctx context.Context, rec *reversalRecord) (retErr error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reversal capture-limit classification: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			return
		}
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("rollback reversal capture-limit classification: %w", rollbackErr)
			if retErr == nil {
				retErr = rollbackErr
			} else {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()

	var currentOwnerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, rec.serverID).Scan(&currentOwnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errReversalOwnershipChanged
		}
		return fmt.Errorf("lock reversal server for capture-limit classification: %w", err)
	}
	if currentOwnerID != rec.toUserID {
		return errReversalOwnershipChanged
	}

	var transferID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM ownership_transfers
		WHERE id = $1
			AND server_id = $2
			AND from_user_id = $3
			AND to_user_id = $4
			AND status = 'completed'
		FOR UPDATE
	`, rec.transferID, rec.serverID, rec.fromUserID, rec.toUserID).Scan(&transferID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errReversalOwnershipChanged
		}
		return fmt.Errorf("lock reversal transfer for capture-limit classification: %w", err)
	}

	var originalOwnerID string
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id
		FROM server_members
		WHERE server_id = $1 AND user_id = $2
		FOR UPDATE
	`, rec.serverID, rec.fromUserID).Scan(&originalOwnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errReversalOriginalOwnerNotMember
		}
		return fmt.Errorf("lock original owner membership for reversal capture-limit classification: %w", err)
	}
	return nil
}

// Sentinel errors for executeTransfer so callers can map to appropriate HTTP status codes.
var (
	errTransferAlreadyCompleted       = errors.New("transfer already completed or cancelled")
	errTransferAlreadyPending         = errors.New("transfer already pending")
	errTransferOwnershipChanged       = errors.New("transfer ownership changed")
	errFromUserNotMember              = fmt.Errorf("from_user is no longer a member")
	errToUserNotMember                = fmt.Errorf("to_user is no longer a member")
	errReversalOwnershipChanged       = errTransferOwnershipChanged
	errReversalOriginalOwnerNotMember = fmt.Errorf("original owner is no longer a member")
)

type ownershipWriteOutcome uint8

const (
	ownershipWriteUnchanged ownershipWriteOutcome = iota
	ownershipWriteChanged
	ownershipWriteStaleCancelled
)

// ownershipPrepareFailureError marks errors produced before the ownership
// transaction starts, so only those errors receive a stale-transfer recheck.
type ownershipPrepareFailureError struct{ cause error }

func (e *ownershipPrepareFailureError) Error() string { return e.cause.Error() }
func (e *ownershipPrepareFailureError) Unwrap() error { return e.cause }

func isOwnershipPrepareFailure(err error) bool {
	var prepareErr *ownershipPrepareFailureError
	return errors.As(err, &prepareErr)
}

type ownershipPrepareClassification uint8

const (
	ownershipPrepareCurrent ownershipPrepareClassification = iota
	ownershipPrepareNoOp
	ownershipPrepareStaleCancelled
)

// withOwnershipCapture runs an ownership write atomically with its pre-write
// Server Voice visibility capture. The transaction locks advisory capture,
// then the server relation, ownership_transfers, and server_members in that
// order. A server deleted before its relation lock is a no-op.
func (h *Handler) withOwnershipCapture(
	ctx context.Context,
	serverID string,
	write func(context.Context, *sql.Tx) (bool, error),
) (plan rbac.PresenceRecheckPlan, changed bool, retErr error) {
	plan, outcome, retErr := h.withOwnershipCaptureOutcome(ctx, serverID, func(ctx context.Context, tx *sql.Tx) (ownershipWriteOutcome, error) {
		changed, err := write(ctx, tx)
		if !changed {
			return ownershipWriteUnchanged, err
		}
		return ownershipWriteChanged, err
	})
	return plan, outcome == ownershipWriteChanged, retErr
}

func (h *Handler) withOwnershipCaptureOutcome(
	ctx context.Context,
	serverID string,
	write func(context.Context, *sql.Tx) (ownershipWriteOutcome, error),
) (plan rbac.PresenceRecheckPlan, outcome ownershipWriteOutcome, retErr error) {
	if h.presenceRecheck != nil {
		prepared, err := h.presenceRecheck.PrepareCaptureStrict(ctx, serverID, nil, nil)
		if err != nil {
			return nil, ownershipWriteUnchanged, &ownershipPrepareFailureError{
				cause: fmt.Errorf("prepare ownership presence capture: %w", err),
			}
		}
		plan = prepared
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ownershipWriteUnchanged, fmt.Errorf("begin ownership transaction: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			return
		}
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("rollback ownership transaction: %w", rollbackErr)
			if retErr == nil {
				retErr = rollbackErr
			} else {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()

	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return nil, ownershipWriteUnchanged, fmt.Errorf("lock ownership visibility capture: %w", err)
	}
	if h.presenceRecheck != nil && plan != nil {
		if err := h.presenceRecheck.CaptureVisibility(ctx, tx, plan); err != nil {
			return nil, ownershipWriteUnchanged, fmt.Errorf("capture ownership visibility: %w", err)
		}
	}
	var lockedServerID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&lockedServerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ownershipWriteUnchanged, nil
		}
		return nil, ownershipWriteUnchanged, fmt.Errorf("lock ownership server: %w", err)
	}
	outcome, err = write(ctx, tx)
	if err != nil {
		return nil, ownershipWriteUnchanged, err
	}
	if outcome == ownershipWriteUnchanged {
		return nil, ownershipWriteUnchanged, nil
	}
	if err := tx.Commit(); err != nil {
		h.presenceAbandon(plan, "ambiguous_commit")
		return nil, ownershipWriteUnchanged, fmt.Errorf("commit ownership transaction: %w", err)
	}
	return plan, outcome, nil
}

// classifyOwnershipPrepareFailure rechecks a pending transfer without presence
// capture after strict preparation failed. It only makes a stale transfer
// terminal; a current transfer keeps the original preparation error.
func (h *Handler) classifyOwnershipPrepareFailure(
	ctx context.Context, serverID, transferID, fromUserID, toUserID string,
) (classification ownershipPrepareClassification, retErr error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return ownershipPrepareCurrent, fmt.Errorf("begin ownership prepare-failure classification: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			return
		}
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("rollback ownership prepare-failure classification: %w", rollbackErr)
			if retErr == nil {
				retErr = rollbackErr
			} else {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()

	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return ownershipPrepareCurrent, fmt.Errorf("lock ownership visibility for prepare-failure classification: %w", err)
	}
	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ownershipPrepareNoOp, nil
		}
		return ownershipPrepareCurrent, fmt.Errorf("lock ownership server for prepare-failure classification: %w", err)
	}
	var lockedTransferID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM ownership_transfers
		WHERE id = $1 AND server_id = $2 AND from_user_id = $3 AND to_user_id = $4 AND status = 'pending'
		FOR UPDATE
	`, transferID, serverID, fromUserID, toUserID).Scan(&lockedTransferID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ownershipPrepareNoOp, nil
		}
		return ownershipPrepareCurrent, fmt.Errorf("lock ownership transfer for prepare-failure classification: %w", err)
	}
	if ownerID == fromUserID {
		return ownershipPrepareCurrent, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE ownership_transfers SET status = 'cancelled', cancelled_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, lockedTransferID)
	if err != nil {
		return ownershipPrepareCurrent, fmt.Errorf("cancel stale ownership transfer after prepare failure: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return ownershipPrepareCurrent, fmt.Errorf("read stale ownership prepare-failure cancellation result: %w", err)
	}
	if rows != 1 {
		return ownershipPrepareNoOp, nil
	}
	if err := tx.Commit(); err != nil {
		return ownershipPrepareCurrent, fmt.Errorf("commit ownership prepare-failure classification: %w", err)
	}
	return ownershipPrepareStaleCancelled, nil
}

func swapOwnershipMemberRoles(ctx context.Context, tx *sql.Tx, serverID, fromUserID, toUserID string) error {
	resFrom, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, serverID, fromUserID)
	if err != nil {
		return fmt.Errorf("update from_user role: %w", err)
	}
	n, err := resFrom.RowsAffected()
	if err != nil {
		return fmt.Errorf("read from_user role result: %w", err)
	}
	if n == 0 {
		return errFromUserNotMember
	}
	resTo, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, serverID, toUserID)
	if err != nil {
		return fmt.Errorf("update to_user role: %w", err)
	}
	n, err = resTo.RowsAffected()
	if err != nil {
		return fmt.Errorf("read to_user role result: %w", err)
	}
	if n == 0 {
		return errToUserNotMember
	}
	return nil
}

func (h *Handler) reconcileOwnershipPostCommit(
	serverID, fromUserID, toUserID string,
	plan rbac.PresenceRecheckPlan,
) {
	h.presenceExecute(plan)
	if h.cache != nil {
		for _, userID := range []string{fromUserID, toUserID} {
			if err := h.cache.Invalidate(context.Background(), serverID, userID); err != nil {
				h.log.Error("ownership permission-cache invalidation failed", "failure_class", "ownership_cache_invalidation")
			}
		}
	}
	h.recheckVoiceBothParties(serverID, fromUserID, toUserID)
}

func (h *Handler) cancelStaleTransfer(ctx context.Context, tx *sql.Tx, transferID, serverID, fromUserID string) (ownershipWriteOutcome, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE ownership_transfers
		SET status = 'cancelled', cancelled_at = NOW(), completed_at = NULL
		WHERE id = $1 AND server_id = $2 AND from_user_id = $3 AND status = 'completed'
	`, transferID, serverID, fromUserID)
	if err != nil {
		return ownershipWriteUnchanged, fmt.Errorf("cancel stale ownership transfer: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return ownershipWriteUnchanged, fmt.Errorf("read stale ownership transfer cancellation result: %w", err)
	}
	if rows != 1 {
		return ownershipWriteUnchanged, errTransferAlreadyCompleted
	}
	return ownershipWriteStaleCancelled, nil
}

type expiredTransfer struct {
	id, serverID, fromUserID, toUserID string
}

// CompleteExpiredTransfers completes pending transfers whose confirmation
// window elapsed. It is the only scheduled ownership writer.
func (h *Handler) CompleteExpiredTransfers(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, server_id, from_user_id, to_user_id
		FROM ownership_transfers
		WHERE status = 'pending' AND expires_at <= NOW()
	`)
	if err != nil {
		h.log.Error("ownership expiry query failed", "failure_class", "ownership_expiry_query")
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			h.log.Error("ownership expiry row close failed", "failure_class", "ownership_expiry_rows_close")
		}
	}()

	for rows.Next() {
		var transfer expiredTransfer
		if err := rows.Scan(&transfer.id, &transfer.serverID, &transfer.fromUserID, &transfer.toUserID); err != nil {
			h.log.Error("ownership expiry row scan failed", "failure_class", "ownership_expiry_scan")
			continue
		}
		if _, err := h.completeExpiredTransfer(ctx, transfer); err != nil {
			h.log.Error("ownership expiry completion failed", "failure_class", "ownership_expiry_completion")
		}
	}
	if err := rows.Err(); err != nil {
		h.log.Error("ownership expiry iteration failed", "failure_class", "ownership_expiry_iteration")
	}
}

func (h *Handler) completeExpiredTransfer(ctx context.Context, transfer expiredTransfer) (bool, error) {
	plan, outcome, err := h.withOwnershipCaptureOutcome(ctx, transfer.serverID, func(ctx context.Context, tx *sql.Tx) (ownershipWriteOutcome, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE ownership_transfers SET status = 'completed', completed_at = NOW()
			WHERE id = $1 AND server_id = $2 AND from_user_id = $3 AND status = 'pending'
		`, transfer.id, transfer.serverID, transfer.fromUserID)
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("mark expired transfer completed: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("read expired transfer completion result: %w", err)
		}
		if rows == 0 {
			return ownershipWriteUnchanged, nil
		}
		res, err = tx.ExecContext(ctx, `UPDATE servers SET owner_id = $1 WHERE id = $2 AND owner_id = $3`, transfer.toUserID, transfer.serverID, transfer.fromUserID)
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("update server owner for expired transfer: %w", err)
		}
		rows, err = res.RowsAffected()
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("read expired transfer owner update result: %w", err)
		}
		if rows == 0 {
			return h.cancelStaleTransfer(ctx, tx, transfer.id, transfer.serverID, transfer.fromUserID)
		}
		if err := swapOwnershipMemberRoles(ctx, tx, transfer.serverID, transfer.fromUserID, transfer.toUserID); err != nil {
			return ownershipWriteUnchanged, err
		}
		return ownershipWriteChanged, nil
	})
	if err != nil {
		if isOwnershipPrepareFailure(err) {
			classification, classifyErr := h.classifyOwnershipPrepareFailure(
				ctx, transfer.serverID, transfer.id, transfer.fromUserID, transfer.toUserID)
			if classifyErr != nil {
				return false, errors.Join(err, classifyErr)
			}
			if classification != ownershipPrepareCurrent {
				return false, nil
			}
		}
		return false, err
	}
	if outcome != ownershipWriteChanged {
		return false, nil
	}

	h.reconcileOwnershipPostCommit(transfer.serverID, transfer.fromUserID, transfer.toUserID, plan)
	if serverUUID, err := uuid.Parse(transfer.serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transferred",
			Data: map[string]interface{}{
				keyServerID:    transfer.serverID,
				"old_owner_id": transfer.fromUserID,
				"new_owner_id": transfer.toUserID,
			},
		})
	}
	return true, nil
}

// executeTransfer atomically transfers ownership from one user to another.
// Lock order: servers → ownership_transfers → server_members.
func (h *Handler) executeTransfer(ctx context.Context, serverID, transferID, fromUserID, toUserID string) error {
	plan, outcome, err := h.withOwnershipCaptureOutcome(ctx, serverID, func(ctx context.Context, tx *sql.Tx) (ownershipWriteOutcome, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE ownership_transfers SET status = 'completed', completed_at = NOW()
			WHERE id = $1 AND server_id = $2 AND from_user_id = $3 AND status = 'pending'
		`, transferID, serverID, fromUserID)
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("mark transfer completed: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("read transfer completion result: %w", err)
		}
		if rows == 0 {
			return ownershipWriteUnchanged, nil
		}
		res, err = tx.ExecContext(ctx, `UPDATE servers SET owner_id = $1 WHERE id = $2 AND owner_id = $3`, toUserID, serverID, fromUserID)
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("update server owner: %w", err)
		}
		rows, err = res.RowsAffected()
		if err != nil {
			return ownershipWriteUnchanged, fmt.Errorf("read transfer owner update result: %w", err)
		}
		if rows == 0 {
			return h.cancelStaleTransfer(ctx, tx, transferID, serverID, fromUserID)
		}
		if err := swapOwnershipMemberRoles(ctx, tx, serverID, fromUserID, toUserID); err != nil {
			return ownershipWriteUnchanged, err
		}
		return ownershipWriteChanged, nil
	})
	if err != nil {
		if isOwnershipPrepareFailure(err) {
			classification, classifyErr := h.classifyOwnershipPrepareFailure(ctx, serverID, transferID, fromUserID, toUserID)
			if classifyErr != nil {
				return errors.Join(err, classifyErr)
			}
			switch classification {
			case ownershipPrepareStaleCancelled:
				return errTransferOwnershipChanged
			case ownershipPrepareNoOp:
				return errTransferAlreadyCompleted
			}
		}
		return err
	}
	if outcome == ownershipWriteStaleCancelled {
		return errTransferOwnershipChanged
	}
	if outcome != ownershipWriteChanged {
		return errTransferAlreadyCompleted
	}

	h.reconcileOwnershipPostCommit(serverID, fromUserID, toUserID, plan)

	// Broadcast
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "ownership_transferred",
			Data: map[string]interface{}{
				keyServerID:    serverID,
				"old_owner_id": fromUserID,
				"new_owner_id": toUserID,
			},
		})
	}

	// The transfer has committed; cancellation from the requester must not suppress
	// its durable audit record.
	auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), ownershipAuditTimeout)
	defer cancelAudit()
	if err := h.audit.Log(auditCtx, serverID, nil, "ownership_transferred", keyServer, &serverID, map[string]interface{}{
		keyFromUserID: fromUserID,
		keyToUserID:   toUserID,
	}); err != nil {
		h.log.Error("Failed to write audit log for ownership_transferred", "error", err, "server_id", serverID)
	}

	return nil
}

// verifyPassword checks the user's password against the stored hash.
// On failure, sends an error response to the gin context and returns an error.
func (h *Handler) verifyPassword(c *gin.Context, userID, password string) error {
	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(), `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&passwordHash)
	if err != nil {
		h.internalError(c, "Failed to fetch password hash", errMsgFailedVerifyPassword, err)
		return err
	}

	match, err := auth.VerifyPassword(password, passwordHash)
	if err != nil {
		h.internalError(c, "Failed to verify password", errMsgFailedVerifyPassword, err)
		return err
	}
	if !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid password"})
		return fmt.Errorf("password mismatch")
	}

	return nil
}

// verifyMFA checks MFA if enabled for the user.
// On failure, sends an error response to the gin context and returns an error.
func (h *Handler) verifyMFA(c *gin.Context, userID, mfaCode string) error {
	ctx := c.Request.Context()
	if !h.mfaVerifier.IsEnabled(ctx, userID) {
		return nil
	}

	if mfaCode == "" {
		methods, _ := h.mfaVerifier.GetEnabledMethods(ctx, userID)
		c.JSON(http.StatusForbidden, gin.H{
			"error":        "MFA verification required",
			"mfa_required": true,
			"methods":      methods,
		})
		return fmt.Errorf("MFA required")
	}

	valid, err := h.mfaVerifier.VerifyCode(ctx, userID, mfaCode)
	if err != nil {
		h.log.Error("Failed to verify MFA code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify MFA code"})
		return err
	}
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
		return fmt.Errorf("invalid MFA code")
	}

	return nil
}

// sendTransferEmail sends an ownership transfer notification to the current owner.
// Failures are logged but do not block the transfer.
func (h *Handler) sendTransferEmail(ctx context.Context, ownerID, serverID, targetUserID, reversalToken string) {
	// Fetch owner's email
	var ownerEmail string
	err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, ownerID).Scan(&ownerEmail)
	if err != nil {
		h.log.Error("Failed to fetch owner email for transfer notification", "error", err)
		return
	}

	// Fetch server name
	var serverName string
	err = h.db.QueryRowContext(ctx, `SELECT name FROM servers WHERE id = $1`, serverID).Scan(&serverName)
	if err != nil {
		h.log.Error("Failed to fetch server name for transfer notification", "error", err)
		return
	}

	// Fetch target username
	var targetUsername string
	err = h.db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = $1`, targetUserID).Scan(&targetUsername)
	if err != nil {
		h.log.Error("Failed to fetch target username for transfer notification", "error", err)
		return
	}

	if err := h.emailSvc.SendOwnershipTransferNotification(ownerEmail, serverName, targetUsername, reversalToken); err != nil {
		h.log.Error("Failed to send ownership transfer email", "error", err, "to", ownerEmail)
	}
}
