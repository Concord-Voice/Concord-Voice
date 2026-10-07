package channels

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxExpirationRequestBytes = 1024

// UpdateExpiration changes a channel's shared message-expiration policy. A
// change that shortens retention is a dangerous action (#3454): on a server
// that enforces MFA on dangerous actions it asks for an inline factor under
// the gate's locks (see dangerous_gates.go).
func (h *Handler) UpdateExpiration(c *gin.Context) {
	userID, channelID := c.GetString("user_id"), c.Param("id")
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var request channelExpirationRequest
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
	// Only a request that sets a window can shorten retention.
	confirm, ok := h.chargeChannelGate(c, serverID, userID, request.MFACode, request.WindowSeconds != nil)
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

	outcome, ok := h.authorizeChannelExpirationTx(c, tx, channelID, serverID, userID, request.Request, confirm)
	if !ok {
		return
	}

	service := expiration.NewService(h.db)
	transition, err := service.StartChannelTransition(c.Request.Context(), tx, channelID, request.Request)
	if err != nil {
		h.writeExpirationStartError(c, err)
		return
	}
	policy := transition.Current

	staged, ok := h.stageChannelExpirationEvent(c, tx, channelID, userID, request.Request, transition)
	if !ok {
		return
	}

	if err := tx.Commit(); err != nil {
		// A deadlock or lock timeout PostgreSQL reports AT commit (deferred
		// work waiting on a lock) is a refused COMMIT, so the transaction rolled
		// back. It gets the same retryable lock_conflict 503 as a lock conflict
		// on any earlier statement in this gated transaction, as WithGateTx
		// gives the other gated routes (Codex review of #3454). It settles
		// nothing.
		if mfaenforce.IsLockConflict(err) {
			mfaenforce.WriteError(c, h.log, err, nil)
			return
		}
		// Any other commit failure is answered as ambiguous, with the
		// candidate policy and no error key, and settles nothing.
		h.log.Error("Channel expiration commit outcome is ambiguous", "error", err)
		c.JSON(http.StatusServiceUnavailable, policy)
		return
	}
	h.settleChannelGate(c.Request.Context(), outcome, confirm, userID)
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
// the advisory lock and the gate below can be keyed by it (I-CHANNEL-SCOPE).
// The channel is re-read under lock in lockChannelExpirationScopeTx, scoped to
// this server: this read is a routing hint, never the authorization operand.
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

// authorizeChannelExpirationTx takes the transaction's locks, resolves the
// actor's authority for this mutation, and runs the dangerous-action gate. It
// writes its own response and returns false when the caller must stop.
//
// Order: the gate's locks (lockChannelExpirationGateTx), the channel lock,
// the channel-scope ManageChannels check (I7), the channel-type check, then
// Require when the request shortens retention against the window read under
// the channel lock. Require precedes every write, so a refusal writes nothing.
func (h *Handler) authorizeChannelExpirationTx(
	c *gin.Context,
	tx *sql.Tx,
	channelID, serverID, userID string,
	request expiration.Request,
	confirm channelGateConfirm,
) (mfaenforce.Outcome, bool) {
	ctx := c.Request.Context()
	g, ok := h.lockChannelExpirationGateTx(c, tx, serverID, userID)
	if !ok {
		return mfaenforce.Unconfirmed, false
	}
	locked, ok := h.lockChannelExpirationScopeTx(c, tx, channelID, serverID)
	if !ok {
		return mfaenforce.Unconfirmed, false
	}

	permissions, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, serverID, userID, channelID)
	if errors.Is(err, rbac.ErrNotMember) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return mfaenforce.Unconfirmed, false
	}
	if err != nil {
		h.log.Error("Failed to resolve channel expiration permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return mfaenforce.Unconfirmed, false
	}
	if !permissions.Has(rbac.PermManageChannels) {
		// RS5 at channel scope, on tx: the check above resolved this channel.
		h.respondChannelExpirationError(c, manageChannelsDenial(ctx, tx, serverID, channelID, userID))
		return mfaenforce.Unconfirmed, false
	}
	if request.Mode == "set" && locked.channelType != "text" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Expiration is only available for text channels"})
		return mfaenforce.Unconfirmed, false
	}
	outcome, err := h.requireChannelGate(ctx, tx, g, userID, stepup.PurposeChannelExpirationShorten, confirm,
		shortensRetention(locked.window, request.WindowSeconds, request.Retroactive))
	if err != nil {
		h.respondChannelExpirationError(c, err)
		return mfaenforce.Unconfirmed, false
	}
	return outcome, true
}

// lockChannelExpirationGateTx takes the expiration transaction's server-level
// locks, in order: the visibility-capture advisory lock, the credential-epoch
// fence on the actor's users row (FOR SHARE), then LockGateTx, which re-takes
// that users row at the same strength (never an upgrade, A-8) and locks the
// servers row FOR SHARE. It writes its own response and returns false when the
// caller must stop.
//
// LockServerVisibilityCapture must remain the transaction's FIRST statement,
// because it serializes the live authority writers that can alter the actor's
// channel permissions underneath the later read. GuardTx keeps its own
// classification, so the gate's epoch and missing-row refusals are
// unreachable backstops.
func (h *Handler) lockChannelExpirationGateTx(c *gin.Context, tx *sql.Tx, serverID, userID string) (mfaenforce.Gate, bool) {
	ctx := c.Request.Context()
	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		h.log.Error("Failed to lock channel expiration authority", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return mfaenforce.Gate{}, false
	}
	tokenEpoch := middleware.TokenCredentialEpoch(c)
	if guardErr := credepoch.GuardTx(ctx, tx, userID, tokenEpoch); guardErr != nil {
		h.respondChannelGuardError(c, guardErr, errMsgFailedUpdateChannel)
		return mfaenforce.Gate{}, false
	}
	g, err := mfaenforce.LockGateTx(ctx, tx, serverID, userID, stepup.LockForShare, mfaenforce.ServerForShare, tokenEpoch)
	if err != nil {
		h.respondChannelExpirationError(c, err)
		return mfaenforce.Gate{}, false
	}
	return g, true
}

// lockedExpirationChannel is the channel row the expiration transaction
// locked.
type lockedExpirationChannel struct {
	channelType string
	// window is the policy before this mutation; NULL means never expire.
	window sql.NullInt64
}

// lockChannelExpirationScopeTx locks the channel FOR NO KEY UPDATE, scoped to
// the server the gate locked (I-CHANNEL-SCOPE), and reads the prior window
// under that lock. Zero rows is the route's existing 404: the channel is gone,
// or moved to another server since the unlocked read, which would otherwise
// be authorized against the server it used to be on.
func (h *Handler) lockChannelExpirationScopeTx(
	c *gin.Context, tx *sql.Tx, channelID, serverID string,
) (lockedExpirationChannel, bool) {
	var locked lockedExpirationChannel
	err := tx.QueryRowContext(c.Request.Context(),
		`SELECT type, expiration_window_seconds FROM channels WHERE id = $1 AND server_id = $2 FOR NO KEY UPDATE`,
		channelID, serverID,
	).Scan(&locked.channelType, &locked.window)
	if errors.Is(err, sql.ErrNoRows) {
		writeChannelNotFound(c)
		return locked, false
	}
	if err != nil {
		h.log.Error("Failed to lock channel expiration scope", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return locked, false
	}
	return locked, true
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
