package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	dmRotateLimit  = 10
	dmRotateWindow = 24 * time.Hour

	errMsgInvalidConversationID    = "Invalid conversation ID"
	errMsgRotationNextEpoch        = "Key rotation must claim the next epoch"
	errMsgRotationNotHolder        = "You must hold the current key to rotate it"
	errMsgRotationIncomplete       = "Key rotation must wrap the key for every participant"
	errMsgRotationRecipientChanged = "A participant's encryption key changed; fetch their keys again"
	errMsgRotationGroupAdminOnly   = "Only the group's creator or an admin can rotate its key"
)

var (
	// errDMEpochClaimStale: the batch names an epoch the conversation is not
	// at. Above current+1 would leave a gap no message references; below the
	// current epoch would write a NEW wrap under a superseded number. Either
	// way the refusal names the current epoch so the client can resync — a
	// claim at current+1 that finds current+1 already written is not stale
	// but a rewrap at that epoch, fenced by admitDMCurrentEpochWriteTx.
	errDMEpochClaimStale = errors.New("dm epoch claim is not the next epoch")
	// errDMEpochClaimNotHolder mirrors the channel rotator's admission: only a
	// holder of the current epoch may establish its successor, so a participant
	// who never received the key cannot re-key the conversation onto their own.
	errDMEpochClaimNotHolder = errors.New("dm epoch claim requires the current key")
	// errDMEpochClaimIncomplete: a successor batch that omits a participant
	// strands them at a revoked epoch the moment the claim commits — the prod
	// lockout of 2026-09-17 in miniature. Refuse the batch instead.
	errDMEpochClaimIncomplete = errors.New("dm epoch claim must wrap for every participant")
	// errDMEpochClaimStaleRecipient: a successor batch wrapped for a participant
	// against an identity key that has since rotated (#2420). Stored it would
	// be unusable; skipped it would strand them at the revoked epoch with no
	// way to claim out. The batch is refused so the caller refetches.
	errDMEpochClaimStaleRecipient = errors.New("dm epoch claim wrapped for a recipient whose key changed")
)

// dmEpochClaimStaleError carries the epoch the conversation is actually at so
// the client can resynchronise; it Is errDMEpochClaimStale.
type dmEpochClaimStaleError struct{ current int }

func (e *dmEpochClaimStaleError) Error() string        { return errDMEpochClaimStale.Error() }
func (e *dmEpochClaimStaleError) Is(target error) bool { return target == errDMEpochClaimStale }

// dmEpochClaimIncompleteError counts the participants the batch left out; it
// Is errDMEpochClaimIncomplete. Ids are not carried — participants already know
// each other, but the count is all a client needs to refetch and retry.
type dmEpochClaimIncompleteError struct{ missing int }

func (e *dmEpochClaimIncompleteError) Error() string { return errDMEpochClaimIncomplete.Error() }
func (e *dmEpochClaimIncompleteError) Is(target error) bool {
	return target == errDMEpochClaimIncomplete
}

// dmEpochClaimStaleRecipientError counts the recipients whose identity key no
// longer matched; it Is errDMEpochClaimStaleRecipient.
type dmEpochClaimStaleRecipientError struct{ stale int }

func (e *dmEpochClaimStaleRecipientError) Error() string {
	return errDMEpochClaimStaleRecipient.Error()
}
func (e *dmEpochClaimStaleRecipientError) Is(target error) bool {
	return target == errDMEpochClaimStaleRecipient
}

// RotateDMKey is the manual seal-and-rotate for a DM conversation, and it is
// ATOMIC: the successor wraps for every current participant and the revocation
// of the current epoch commit in one transaction, or nothing does.
//
// Its predecessor in the dm package recorded the revocation first and answered
// "Key rotated" with no key material in the request at all, leaving the
// successor to the clients' rotation coordinator — which resolves members
// through a server a DM conversation does not have and returned silently. The
// only epoch anyone held was revoked, nobody could read or send, and no
// self-heal existed because the rewrap queue only serves a MISSING row, not a
// revoked one (prod, 2026-09-17). A body-less request is therefore refused: an
// old client that still speaks that contract fails its rotation rather than
// locking the thread.
//
// POST /dm/conversations/:id/rotate-key
func (h *Handler) RotateDMKey(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	parsedConvID, parseErr := uuid.Parse(convID)
	if parseErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	// Canonicalize before the per-conversation limiter key is built — see
	// DistributeUnifiedKeys for why (#1218 red-team).
	convID = parsedConvID.String()

	isParticipant, err := h.dmParticipantExists(convID, userID)
	if err != nil {
		h.log.Error("dm key rotation: participant check failed",
			"conversation_id", sanitizeID(convID), "user_id", sanitizeID(userID), "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRotateKey})
		return
	}
	if !isParticipant {
		// Same not-found a non-participant gets from every unified key route:
		// no existence oracle.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgContextNotFoundOrDenied})
		return
	}
	// The renderer offers the action to a group's creator only; the server
	// used to admit any participant, so a direct POST from any of the ten
	// members could re-key the group for everyone (CWE-602). Group authority
	// is the creator or an admin role — the same set that can change
	// membership. A 1:1 conversation has no hierarchy and either party may.
	allowed, err := h.dmRotationAuthority(convID, userID)
	if err != nil {
		h.log.Error("dm key rotation: authority check failed",
			"conversation_id", sanitizeID(convID), "user_id", sanitizeID(userID), "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRotateKey})
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgRotationGroupAdminOnly})
		return
	}

	var req DistributeChannelKeysRequest
	if !bindStrictJSONBody(c, &req, maxDMWrappedKeysRequestBytes) {
		return
	}
	if len(req.WrappedKeys) == 0 || req.KeyVersion == nil || *req.KeyVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	if len(req.WrappedKeys) > maxDMWrappedKeys || len(req.WrappedKeyVersions) > maxDMWrappedKeys {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTooManyWrappedKeys})
		return
	}

	// The per-conversation budget (10 per 24 h) is READ here and SPENT only
	// by a committed rotation, below. Every participant shares it, and the
	// route now refuses bodyless, incomplete, stale and non-holder requests —
	// if refusals spent it, one member (or one client still speaking the old
	// bodyless contract) could exhaust it in seconds and block the owner's
	// incident-response rotation for a day. The budget is a ceiling on
	// re-keys, not on attempts; attempts are bounded by the route's own
	// per-user limiter. It sits after the body checks for the same reason.
	rateLimitKey := fmt.Sprintf("ratelimit:dm_rotate:%s", convID)
	if blocked, retryAfter := h.dmRotateBudgetExhausted(c.Request.Context(), rateLimitKey); blocked {
		middleware.RespondRateLimited(c, retryAfter, dmRotateLimit)
		return
	}

	outcome, rotErr := h.rotateDMKeyTx(c.Request.Context(), userID, middleware.TokenCredentialEpoch(c), convID, req)
	if rotErr != nil {
		if status, body, known := keyDistributionErrorResponse(rotErr); known {
			c.JSON(status, body)
			return
		}
		h.log.Error("dm key rotation failed", "error", rotErr, "conversation_id", sanitizeID(convID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRotateKey})
		return
	}

	h.consumeDMRotateBudget(rateLimitKey)
	h.log.Info("dm key rotated",
		"conversation_id", sanitizeID(convID), "user_id", sanitizeID(userID),
		"previous_version", outcome.previousVersion, "new_key_version", outcome.keyVersion)
	if outcome.previousVersion > 0 {
		// Every participant including the actor: BroadcastToUser reaches all
		// of a user's devices, and the actor's OTHER devices still hold the
		// revoked epoch in cache. The actor's own device re-checks and finds
		// the epoch already established (one GET).
		h.broadcastDMKeyRevocation(convID, outcome.previousVersion, outcome.keyVersion, "manual_rotation")
	}
	h.notifyDMKeyDistribution(convID, outcome.delivered)
	c.JSON(http.StatusOK, gin.H{"message": "Key rotated", "new_key_version": outcome.keyVersion})
}

// dmRotationAuthority reports whether userID may rotate convID's key: any party
// of a 1:1 conversation, and a group's creator or an admin-role participant.
func (h *Handler) dmRotationAuthority(convID, userID string) (bool, error) {
	var isGroup, isCreator bool
	var role sql.NullString
	err := h.db.QueryRow(`
		SELECT dc.is_group, dc.created_by = $2, dp.role
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&isGroup, &isCreator, &role)
	if err != nil {
		return false, fmt.Errorf("read dm rotation authority: %w", err)
	}
	return !isGroup || isCreator || (role.Valid && role.String == "admin"), nil
}

// dmRotateBudgetExhausted reads the per-conversation rotation counter without
// spending it. Fail-open on any Redis fault, as IsRateLimited is: a stalled
// counter says nothing about the budget and must not wedge a re-key.
func (h *Handler) dmRotateBudgetExhausted(ctx context.Context, key string) (bool, time.Duration) {
	if h.redis == nil {
		return false, 0
	}
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	count, err := h.redis.Get(ctx, key).Int()
	if err != nil || count < dmRotateLimit {
		return false, 0
	}
	ttl, err := h.redis.TTL(ctx, key).Result()
	if err != nil {
		return false, 0
	}
	if ttl < 0 {
		// A counter with no expiry would block forever; re-arm the window
		// rather than trust it.
		h.redis.Expire(ctx, key, dmRotateWindow) //nolint:errcheck // best-effort TTL repair
		ttl = dmRotateWindow
	}
	return true, ttl
}

// consumeDMRotateBudget spends one unit of the per-conversation budget for a
// rotation that COMMITTED. IsRateLimited increments and repairs the window's
// expiry; its verdict is irrelevant here, the re-key already happened.
func (h *Handler) consumeDMRotateBudget(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	middleware.IsRateLimited(ctx, h.redis, key, dmRotateLimit, dmRotateWindow)
}

// rotateDMKeyTx runs the shared DM distribution transaction body as a successor
// claim; distributeDMKeysTx records the revocation of the epoch it supersedes
// in the same commit. The claim fences (next epoch only, current-key holder
// only, every participant wrapped, no stale recipient) live there so the
// unified route enforces them too; what this route adds is that a rotation
// must ADVANCE — a batch at the current epoch is a rewrap, not a rotation,
// and is refused here.
func (h *Handler) rotateDMKeyTx(ctx context.Context, actorID, tokenEpoch, convID string, req DistributeChannelKeysRequest) (dmDistributionOutcome, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return dmDistributionOutcome{}, fmt.Errorf("begin dm rotation tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("Failed to rollback dm rotation tx", "error", rbErr)
		}
	}()

	outcome, err := distributeDMKeysTx(ctx, tx, dmDistributionBatch{
		actorID:            actorID,
		tokenEpoch:         tokenEpoch,
		conversationID:     convID,
		wrappedKeys:        req.WrappedKeys,
		wrappedKeyVersions: req.WrappedKeyVersions,
		explicitVersion:    req.KeyVersion,
		reason:             "manual_rotation",
	})
	if err != nil {
		return dmDistributionOutcome{}, err
	}
	if outcome.keyVersion != outcome.previousVersion+1 {
		return dmDistributionOutcome{}, &dmEpochClaimStaleError{current: outcome.previousVersion}
	}
	if err := tx.Commit(); err != nil {
		return dmDistributionOutcome{}, fmt.Errorf("commit dm rotation tx: %w", err)
	}
	return outcome, nil
}

// dmParticipantExists reports whether userID is a participant of an existing
// conversation — the same single query the unified distribute route runs.
func (h *Handler) dmParticipantExists(convID, userID string) (bool, error) {
	var exists bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_conversations dc
			INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
			WHERE dc.id = $1
		)
	`, convID, userID).Scan(&exists)
	return exists, err
}

// broadcastDMKeyRevocation tells every participant, the actor included, that
// an epoch was revoked and which one succeeds it — the same key_revocation
// event the server-channel rotator emits, so the client's existing handler
// invalidates its cached key and runs the rotation coordinator. Post-commit
// only; a failed lookup drops the broadcast, and the next key fetch answers
// REVOKED_EPOCH, which the client treats the same way.
func (h *Handler) broadcastDMKeyRevocation(convID string, revokedEpoch, successorEpoch int, reason string) {
	h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
		Type: "key_revocation",
		Data: map[string]interface{}{
			"channel_id":    convID,
			"revoked_epoch": revokedEpoch,
			"new_epoch":     successorEpoch,
			"reason":        reason,
		},
	})
}

// notifyDMKeyNeeded pages every other participant to run its pending rewrap
// queue after requesterID enrolled a request on convID. The payload is the
// server-channel key_needed shape minus server_id, which a DM does not have;
// the client's handler reads only channel_ids. Non-holders fetch an empty
// queue — appendDMPendingRequests serves a row to current-epoch holders only.
func (h *Handler) notifyDMKeyNeeded(convID, requesterID string) {
	h.broadcastToDMParticipants(convID, requesterID, websocket.OutgoingMessage{
		Type: "key_needed",
		Data: map[string]interface{}{
			"user_id":     requesterID,
			"channel_ids": []string{convID},
		},
	})
}

// broadcastToDMParticipants sends msg to every participant of convID except
// excludeUserID (pass "" to include everyone). A participant lookup failure
// is logged and the send skipped: every caller is a best-effort push whose
// absence the client tolerates (a reconnect or the next key fetch re-cues it).
func (h *Handler) broadcastToDMParticipants(convID, excludeUserID string, msg websocket.OutgoingMessage) {
	if h.hub == nil {
		return
	}
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, convID)
	if err != nil {
		h.log.Warn("dm participant broadcast: participant lookup failed",
			"conversation_id", sanitizeID(convID), "event", msg.Type, "error", err)
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			h.log.Warn("dm participant broadcast: participant scan failed",
				"conversation_id", sanitizeID(convID), "event", msg.Type, "error", err)
			continue
		}
		if uid == excludeUserID {
			continue
		}
		if parsed, parseErr := uuid.Parse(uid); parseErr == nil {
			h.hub.BroadcastToUser(parsed, msg)
		}
	}
	if err := rows.Err(); err != nil {
		// Best-effort fan-out, but a truncated one must not be invisible: a
		// participant not reached learns of the epoch on their next fetch.
		h.log.Warn("dm participant broadcast: participant iteration failed",
			"conversation_id", sanitizeID(convID), "event", msg.Type, "error", err)
	}
}
