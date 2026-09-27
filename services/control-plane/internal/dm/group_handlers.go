package dm

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	errMsgFailedUpdateRole  = "Failed to update role"
	errMsgFailedDeleteGroup = "Failed to delete group"
)

// maxGroupVoiceCandidates bounds the participants one group deletion may name
// as active-call subjects. It mirrors activepresence's own maxActiveSubjects
// (unexported there) and exists for the same reason: a group DM caps at 10
// members (AddMember's `count >= 10` check in handlers.go), but that check
// counts with a bare SELECT COUNT(*) and inserts with no row lock, so its
// TOCTOU can produce an 11-member group. A bound of exactly 10 would make such
// a group permanently undeletable. Exceeding this one is a derivation bug and
// fails CLOSED — including on a replica with no rail, so an oversized set can
// never be deleted with its obligation silently dropped.
const maxGroupVoiceCandidates = 16

// errCandidateSetDrifted reports that the active-call participant set changed
// between the pre-transaction candidate read and the conversation lock. It is a
// CONFLICT, not a server fault: the set can only grow through a concurrent DM
// call join, so it is self-clearing and client-retryable.
var (
	errCandidateSetDrifted = errors.New("dm: voice participant set changed under the conversation lock")
	// errMemberRemovalStateDrifted reports that the preflight authorization or
	// target membership changed before the destructive transaction acquired its
	// locks. Retrying performs a fresh authorization decision.
	errMemberRemovalStateDrifted = errors.New("dm: member removal state changed under transaction lock")
	errDMTopologyBlocked         = errors.New("dm: participant topology blocked")
	errDMTopologyPrivacy         = errors.New("dm: participant topology privacy denied")
)

// groupDeleteStatements is the child-first deletion order. dm_voice_participants
// leads, which is why plan capture must precede it: those rows are the evidence
// a later resolver would need.
var groupDeleteStatements = []struct{ query, label string }{
	{"DELETE FROM dm_voice_participants WHERE conversation_id = $1", "voice participants"},
	{"DELETE FROM dm_read_states WHERE conversation_id = $1", "read states"},
	{"DELETE FROM dm_pending_key_requests WHERE conversation_id = $1", "pending key requests"},
	{"DELETE FROM dm_key_revocations WHERE conversation_id = $1", "key revocations"},
	{"DELETE FROM dm_channel_keys WHERE conversation_id = $1", "channel keys"},
	{"DELETE FROM dm_messages WHERE conversation_id = $1", "messages"},
	{"DELETE FROM dm_participants WHERE conversation_id = $1", "participants"},
	{"DELETE FROM dm_conversations WHERE id = $1", "conversation"},
}

// rowsQuerier is the read half shared by the pre-transaction candidate read and
// its under-the-lock re-read, so the two cannot drift into asking different
// questions of the same table.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// VoiceParticipantSetAdvisoryKey derives the lock that serializes a private
// call's ingress mutations with group-member removal. It is shared with voice
// ingress so neither transaction can hold a participant row while waiting for
// the other to acquire the conversation-wide mutation lock.
func VoiceParticipantSetAdvisoryKey(conversationID uuid.UUID) (int64, error) {
	if conversationID == uuid.Nil {
		return 0, errors.New("invalid private voice participant-set lock")
	}
	digest := sha256.Sum256([]byte(
		"private_voice_participant_set\x00" + conversationID.String(),
	))
	// Preserve the complete digest bit pattern in PostgreSQL's signed key type.
	return int64(binary.BigEndian.Uint64(digest[:8])), nil //nolint:gosec
}

// LockDMVoiceParticipantSetTx takes the shared private-call participant-set
// transaction lock before a mutation reads or writes dm_participants.
func LockDMVoiceParticipantSetTx(
	ctx context.Context,
	tx *sql.Tx,
	conversationID uuid.UUID,
) error {
	if tx == nil {
		return errors.New("private voice participant-set transaction unavailable")
	}
	lockKey, err := VoiceParticipantSetAdvisoryKey(conversationID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("lock private voice participant set: %w", err)
	}
	return nil
}

// PrivateVoiceScopeAdvisoryKey derives a stable PostgreSQL advisory-lock key
// for a private-call audience. SHA-256 has no confidentiality or key-material
// role here.
func PrivateVoiceScopeAdvisoryKey(userID uuid.UUID) (int64, error) {
	if userID == uuid.Nil {
		return 0, errors.New("invalid private voice scope lock")
	}
	digest := sha256.Sum256([]byte("private_voice_scope\x00" + userID.String()))
	return int64(binary.BigEndian.Uint64(digest[:8])), nil //nolint:gosec
}

// LockPrivateVoiceScopesTx serializes mutations of each affected private-call
// audience in deterministic order.
func LockPrivateVoiceScopesTx(ctx context.Context, tx *sql.Tx, userIDs []uuid.UUID) error {
	if tx == nil || len(userIDs) == 0 {
		return errors.New("invalid private voice scope locks")
	}
	ordered := append([]uuid.UUID(nil), userIDs...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	previous := uuid.Nil
	for _, userID := range ordered {
		if userID == uuid.Nil {
			return errors.New("invalid private voice scope lock")
		}
		if userID == previous {
			continue
		}
		previous = userID
		lockKey, err := PrivateVoiceScopeAdvisoryKey(userID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
			return fmt.Errorf("lock private voice scope: %w", err)
		}
	}
	return nil
}

// UpdateRoleRequest represents the request body for updating a DM participant's role.
type UpdateRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

type dmRoleChange struct {
	conversationID string
	actorID        string
	targetID       string
	role           string
	actor          uuid.UUID
	target         uuid.UUID
}

// UpdateMemberRole changes a group DM participant's role (admin/member).
func (h *Handler) UpdateMemberRole(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")
	targetUserID := c.Param("userId")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	var req UpdateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be 'admin' or 'member'"})
		return
	}

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot change your own role"})
		return
	}

	change, valid := h.preflightDMRoleChange(c, convID, userID, targetUserID, req.Role)
	if !valid {
		return
	}
	updated := false
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = h.updateDMRoleTx(c.Request.Context(), change, middleware.TokenCredentialEpoch(c))
		if errors.Is(err, dmblock.ErrMembershipChanged) && attempt == 0 {
			continue
		}
		updated = err == nil
		break
	}
	if !updated {
		h.respondDMRoleChangeError(c, err)
		return
	}

	// Broadcast role change to all participants
	h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
		Type: "dm_role_changed",
		Data: map[string]interface{}{
			"conversation_id": convID,
			"user_id":         targetUserID,
			"role":            req.Role,
			"changed_by":      userID,
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Role updated",
		"user_id": targetUserID,
		"role":    req.Role,
	})
}

func (h *Handler) preflightDMRoleChange(c *gin.Context, convID, userID, targetUserID, role string) (dmRoleChange, bool) {
	// Verify caller is participant, admin, and conversation is a group.
	var isGroup bool
	var createdBy string
	var callerRole string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dc.created_by, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, userID).Scan(&isGroup, &createdBy, &callerRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return dmRoleChange{}, false
	}
	if err != nil {
		h.log.Error("Failed to verify caller role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return dmRoleChange{}, false
	}
	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot change roles in non-group conversations"})
		return dmRoleChange{}, false
	}
	if callerRole != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admins can change roles"})
		return dmRoleChange{}, false
	}

	// Verify target is a participant
	var exists bool
	err = h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convID, targetUserID).Scan(&exists)
	if err != nil {
		h.log.Error("Failed to check target participation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return dmRoleChange{}, false
	}
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Target user is not a participant"})
		return dmRoleChange{}, false
	}

	// Cannot demote the group creator
	if targetUserID == createdBy && role == "member" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot demote the group creator"})
		return dmRoleChange{}, false
	}

	actor, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_unavailable"})
		return dmRoleChange{}, false
	}
	target, err := uuid.Parse(targetUserID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_unavailable"})
		return dmRoleChange{}, false
	}
	return dmRoleChange{
		conversationID: convID,
		actorID:        userID,
		targetID:       targetUserID,
		role:           role,
		actor:          actor,
		target:         target,
	}, true
}

func (h *Handler) updateDMRoleTx(ctx context.Context, change dmRoleChange, credentialEpoch string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()
	if _, err := dmblock.PrepareConversationTx(ctx, tx, change.conversationID, []uuid.UUID{change.actor, change.target}, dmblock.LockShare, dmblock.LockNoKeyUpdate); err != nil {
		return err
	}
	if err := credepoch.GuardTx(ctx, tx, change.actorID, credentialEpoch); err != nil {
		return err
	}
	var isGroup bool
	var creatorID, actorRole string
	if err := tx.QueryRowContext(ctx, `
		SELECT dc.is_group, dc.created_by, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1 FOR SHARE OF dc, dp`, change.conversationID, change.actorID,
	).Scan(&isGroup, &creatorID, &actorRole); err != nil {
		return err
	}
	if !isGroup || actorRole != "admin" || (change.targetID == creatorID && change.role == "member") {
		return dmblock.ErrUnavailable
	}
	result, err := tx.ExecContext(ctx, `UPDATE dm_participants SET role = $1 WHERE conversation_id = $2 AND user_id = $3`, change.role, change.conversationID, change.targetID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return dmblock.ErrUnavailable
	}
	return tx.Commit()
}

func (h *Handler) respondDMRoleChangeError(c *gin.Context, err error) {
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		h.respondGuardTxError(c, err, errMsgFailedUpdateRole)
		return
	}
	if errors.Is(err, dmblock.ErrUnavailable) || errors.Is(err, dmblock.ErrMembershipChanged) {
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_unavailable"})
		return
	}
	h.log.Error("Failed to update member role", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
}

// DeleteGroup permanently deletes a group DM conversation and all associated data.
func (h *Handler) DeleteGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	// Verify caller is participant, admin, and conversation is a group
	var isGroup bool
	var callerRole string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, userID).Scan(&isGroup, &callerRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}
	if err != nil {
		h.log.Error("Failed to verify caller for group deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteGroup})
		return
	}
	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot delete non-group conversations"})
		return
	}
	if callerRole != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admins can delete groups"})
		return
	}

	// Fetch participant list before deletion for broadcasting
	participantIDs, err := h.fetchParticipantIDs(convID)
	if err != nil {
		h.log.Error("Failed to fetch participants for deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteGroup})
		return
	}

	// Delete all group data in a transaction.
	if err := h.deleteGroupData(c.Request.Context(), convID, userID, middleware.TokenCredentialEpoch(c)); err != nil {
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
			h.respondGuardTxError(c, err, errMsgFailedDeleteGroup)
		} else {
			h.respondDeleteGroupError(c, err)
		}
		return
	}

	h.broadcastGroupDeleted(convID, userID, participantIDs)

	c.JSON(http.StatusOK, gin.H{"message": "Group deleted"})
}

// respondDeleteGroupError maps a deletion failure onto its status. The three
// arms are NOT interchangeable: only the default one means "nothing happened,
// retry the same request".
func (h *Handler) respondDeleteGroupError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, dmblock.ErrUnavailable), errors.Is(err, dmblock.ErrMembershipChanged):
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_unavailable"})
	case errors.Is(err, errCandidateSetDrifted), errors.Is(err, errMemberRemovalStateDrifted):
		// A conflict, not a server fault: the active-call participant set or
		// the caller's delete authority changed under the transaction lock.
		// Client-retryable, so no in-handler retry loop — that would hold the
		// sender gates across an indeterminate wait.
		c.JSON(http.StatusConflict, gin.H{"error": errMsgFailedDeleteGroup})
	case errors.Is(err, activepresence.ErrDeliveryIncomplete):
		// The conversation IS deleted and only presence delivery failed. A 500
		// "Failed to delete group" would invite a destructive retry against a
		// conversation that no longer exists; the plan row survives and the
		// reconciler's next pass owns it.
		h.log.Error("group deleted but presence delivery did not settle",
			"failure_class", "delivery")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgFailedDeleteGroup})
	default:
		h.log.Error("Failed to delete group data", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteGroup})
	}
}

// broadcastGroupDeleted is a UI-refresh hint, corrected by the next
// conversation-list fetch. It carries no presence semantics and deliberately
// fails open — never route a presence obligation through it.
func (h *Handler) broadcastGroupDeleted(convID, deletedBy string, participantIDs []string) {
	if h.hub == nil {
		return
	}
	for _, uid := range participantIDs {
		targetUUID, parseErr := uuid.Parse(uid)
		if parseErr != nil {
			continue
		}
		h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
			Type: "dm_group_deleted",
			Data: map[string]interface{}{
				"conversation_id": convID,
				"deleted_by":      deletedBy,
			},
		})
	}
}

// fetchParticipantIDs returns user IDs for all participants in a conversation.
func (h *Handler) fetchParticipantIDs(convID string) ([]string, error) {
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, convID)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		ids = append(ids, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate participants: %w", err)
	}
	return ids, nil
}

func fetchParticipantIDsTx(ctx context.Context, tx *sql.Tx, convID string) (ids []string, returnErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM dm_participants WHERE conversation_id = $1 ORDER BY user_id`, convID)
	if err != nil {
		return nil, fmt.Errorf("query locked participants: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			ids = nil
			returnErr = errors.Join(returnErr, fmt.Errorf("close locked participants: %w", closeErr))
		}
	}()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan locked participant: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked participants: %w", err)
	}
	return ids, nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// deleteGroupData deletes a group DM and captures the durable active-category
// reconciliation obligation for every participant in an active call (#2448).
//
// LOCK ORDER IS LOAD-BEARING and all three clauses must hold TOGETHER; each
// stated alone loses one of the others:
//
//  1. Sender gates are acquired BEFORE BeginTx (inside Rail.WithGatedTx).
//     Taking a gate while holding row locks builds a cycle half of which is a
//     buffered Go channel, and no deadlock detector can break that.
//  2. users is locked BEFORE dm_conversations. The canonical order is
//     [internal]rules/migrations.md — domain parents come AFTER users (#2447) —
//     credepoch.GuardTx follows it, and updateDMMessageCiphertext
//     (handlers.go, the E2EE message edit) takes users FOR SHARE first and says
//     so inline. Locking dm_conversations first produces a real
//     two-transaction cycle that Postgres breaks with 40P01, surfacing as a
//     failed group deletion OR a failed E2EE message edit with no bug visible
//     in either handler, under ordinary concurrency and with no adversary.
//  3. The candidate set is re-read under the conversation lock and the whole
//     mutation fails closed if it drifted. The extra users row is NOT locked
//     mid-transaction — that reintroduces the unordered acquisition clause 2
//     exists to prevent.
//
// Plan capture sits after the conversation lock and before the first DELETE,
// because DELETE FROM dm_voice_participants destroys the evidence.
func (h *Handler) deleteGroupData(ctx context.Context, convID, actorUserID, credentialEpoch string) error {
	for attempt := 0; attempt < 2; attempt++ {
		// Resolved on the plain connection: the gates are acquired from this set,
		// and acquiring them inside the transaction is what clause 1 forbids.
		candidates, err := readVoiceCandidates(ctx, h.db, convID)
		if err != nil {
			return fmt.Errorf("read voice candidates: %w", err)
		}
		if h.afterCandidateReadHook != nil {
			h.afterCandidateReadHook()
		}
		if len(candidates) > maxGroupVoiceCandidates {
			return fmt.Errorf("%w: %d", activepresence.ErrTooManySubjects, len(candidates))
		}
		var deleteErr error
		if h.activePlans == nil || len(candidates) == 0 {
			deleteErr = h.deleteGroupRows(ctx, convID, actorUserID, credentialEpoch, candidates)
		} else {
			deleteErr = h.deleteGroupWithPlans(ctx, convID, actorUserID, credentialEpoch, candidates)
		}
		if errors.Is(deleteErr, dmblock.ErrMembershipChanged) && attempt == 0 {
			continue
		}
		return deleteErr
	}
	return dmblock.ErrMembershipChanged
}

// deleteGroupWithPlans runs the gated, plan-capturing path. The rail owns the
// transaction; this function owns the commit inside it.
func (h *Handler) deleteGroupWithPlans(
	ctx context.Context,
	convID string,
	actorUserID string,
	credentialEpoch string,
	candidates []uuid.UUID,
) error {
	return h.activePlans.WithGatedTx(ctx, candidates, func(tx *sql.Tx) error {
		result, err := h.deleteGroupRowsTx(ctx, tx, convID, actorUserID, credentialEpoch, candidates)
		if err != nil {
			return err
		}
		// work owns the commit: WithGatedTx rolls back anything it returns
		// uncommitted, so the deletes and their plans would both be discarded.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit group deletion: %w", err)
		}
		h.purgeEngine.EnqueueBlobDeletes(result.refs)
		// Post-commit, STILL INSIDE the gate. Re-entering WithGatedTx here
		// would hash to the same buffer-1 stripe and block forever.
		return h.activePlans.CompleteAlreadyGated(ctx, nil, result.keys)
	})
}

// deleteGroupRows is the no-obligation path: no active call, or a replica whose
// rail is unwired. It opens and commits its own transaction without voice sender
// gates or plans, but still locks the DM topology's users-first prefix.
//
// It STILL revalidates. Red-team finding, #2448 pre-PR pass: routing on the
// pre-transaction candidate count sent the EMPTY set -- the ordinary state of a
// group DM -- down a path with no re-read at all. A participant who started a
// call between the plain-connection read and the conversation lock had their
// dm_voice_participants row, the C3 evidence, destroyed in a transaction that
// recorded no obligation and delivered no clear frame. That is precisely the
// fail-open this issue exists to close, reachable through the common case.
//
// Refusing with 409 is correct rather than harsh: the caller retries, the
// second attempt reads the non-empty candidate set, and the guarded path
// captures the plan. Capturing here instead is not available -- writing a plan
// row needs that subject's sender gate, and acquiring a gate inside an open
// transaction is the cycle the gate ordering exists to prevent.
func (h *Handler) deleteGroupRows(
	ctx context.Context,
	convID, actorUserID, credentialEpoch string,
	candidates []uuid.UUID,
) error {
	if h.purgeEngine == nil {
		return errors.New("group deletion: purge engine is unavailable")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	extra, err := topologyActor(actorUserID)
	if err != nil {
		return err
	}
	participants, err := dmblock.PrepareResolutionConversationTx(ctx, tx, convID, extra, dmblock.LockNoKeyUpdate, dmblock.LockUpdate)
	if err != nil {
		return err
	}
	if err := credepoch.GuardTx(ctx, tx, actorUserID, credentialEpoch); err != nil {
		return err
	}
	if err := revalidateDeleteGroupAuthority(ctx, tx, convID, actorUserID); err != nil {
		return err
	}
	if err := revalidateVoiceCandidates(ctx, tx, convID, candidates); err != nil {
		return err
	}
	if err := lockConversationMessages(ctx, tx, convID); err != nil {
		return err
	}
	fileIDs, refs, err := h.purgeEngine.CaptureConversationBlobsTx(ctx, tx, convID)
	if err != nil {
		return err
	}
	if err := dmblock.EnqueueVoiceEjectionsTx(ctx, tx, convID, participants); err != nil {
		return err
	}
	if err := execGroupChildDeletes(ctx, tx, convID); err != nil {
		return err
	}
	if err := ensureNoAttachmentBridges(ctx, tx, fileIDs); err != nil {
		return err
	}
	if err := deleteGroupConversation(ctx, tx, convID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	h.purgeEngine.EnqueueBlobDeletes(refs)
	return nil
}

// deleteGroupRowsTx performs the ordered locking, the re-validation, the plan
// capture, and the deletes. It does NOT commit.
func (h *Handler) deleteGroupRowsTx(
	ctx context.Context,
	tx *sql.Tx,
	convID, actorUserID, credentialEpoch string,
	candidates []uuid.UUID,
) (groupDeleteResult, error) {
	if h.purgeEngine == nil {
		return groupDeleteResult{}, errors.New("group deletion: purge engine is unavailable")
	}
	// 1. Complete topology users FIRST — candidates alone omit inactive
	// participants and the creator referenced by the deletion.
	extra, err := topologyActor(actorUserID)
	if err != nil {
		return groupDeleteResult{}, err
	}
	participants, err := dmblock.PrepareResolutionConversationTx(ctx, tx, convID, extra, dmblock.LockNoKeyUpdate, dmblock.LockUpdate)
	if err != nil {
		return groupDeleteResult{}, err
	}
	if err := credepoch.GuardTx(ctx, tx, actorUserID, credentialEpoch); err != nil {
		return groupDeleteResult{}, err
	}
	if h.afterUsersLockHook != nil {
		h.afterUsersLockHook(tx)
	}
	// 2. Then the domain parent.
	if err := revalidateDeleteGroupAuthority(ctx, tx, convID, actorUserID); err != nil {
		return groupDeleteResult{}, err
	}
	// 3. Re-validate under the lock, fail closed.
	if err := revalidateVoiceCandidates(ctx, tx, convID, candidates); err != nil {
		return groupDeleteResult{}, err
	}
	// 4. Lock messages, then capture media BEFORE the evidence-destroying DELETE.
	if err := lockConversationMessages(ctx, tx, convID); err != nil {
		return groupDeleteResult{}, err
	}
	fileIDs, refs, err := h.purgeEngine.CaptureConversationBlobsTx(ctx, tx, convID)
	if err != nil {
		return groupDeleteResult{}, err
	}
	keys, err := capturePrivateCallPlans(ctx, tx, h.activePlans, candidates)
	if err != nil {
		return groupDeleteResult{}, err
	}
	// 5. The existing deletes, unchanged order.
	if err := dmblock.EnqueueVoiceEjectionsTx(ctx, tx, convID, participants); err != nil {
		return groupDeleteResult{}, err
	}
	if err := execGroupChildDeletes(ctx, tx, convID); err != nil {
		return groupDeleteResult{}, err
	}
	if err := ensureNoAttachmentBridges(ctx, tx, fileIDs); err != nil {
		return groupDeleteResult{}, err
	}
	if err := deleteGroupConversation(ctx, tx, convID); err != nil {
		return groupDeleteResult{}, err
	}
	return groupDeleteResult{keys: keys, refs: refs}, nil
}

type groupDeleteResult struct {
	keys []activepresence.PlanKey
	refs []media.BlobRef
}

// lockConversation serializes destructive retirement with participant inserts.
// INSERT into dm_participants takes a KEY SHARE lock on its FK parent, which
// conflicts with FOR UPDATE but not FOR NO KEY UPDATE.
func lockConversation(ctx context.Context, tx *sql.Tx, convID string) error {
	var lockedConversationID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`, convID,
	).Scan(&lockedConversationID); err != nil {
		return fmt.Errorf("lock conversation: %w", err)
	}
	return nil
}

func lockConversationMessages(ctx context.Context, tx *sql.Tx, convID string) (returnErr error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM dm_messages WHERE conversation_id = $1 ORDER BY created_at, id FOR UPDATE`, convID)
	if err != nil {
		return fmt.Errorf("lock conversation messages: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close conversation message locks: %w", closeErr)
		}
	}()
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return fmt.Errorf("scan conversation message lock: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate conversation message locks: %w", err)
	}
	return nil
}

func ensureNoAttachmentBridges(ctx context.Context, tx *sql.Tx, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	var fileID string
	err := tx.QueryRowContext(ctx, `
		SELECT file_id FROM (
			SELECT file_id FROM message_attachments WHERE file_id = ANY($1::uuid[])
			UNION ALL
			SELECT file_id FROM dm_message_attachments WHERE file_id = ANY($1::uuid[])
		) AS remaining LIMIT 1`, pq.Array(fileIDs)).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check remaining attachment bridges: %w", err)
	}
	return fmt.Errorf("group deletion: attachment %s remains referenced", fileID)
}

func topologyActor(actorID string) ([]uuid.UUID, error) {
	if actorID == "" {
		return nil, nil
	}
	actor, err := uuid.Parse(actorID)
	if err != nil {
		return nil, fmt.Errorf("parse topology actor: %w", err)
	}
	return []uuid.UUID{actor}, nil
}

func revalidateDeleteGroupAuthority(ctx context.Context, tx *sql.Tx, convID, actorID string) error {
	if actorID == "" {
		return nil
	}
	var isGroup bool
	var role string
	if err := tx.QueryRowContext(ctx, `
		SELECT dc.is_group, dp.role
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
		FOR SHARE OF dc, dp
	`, convID, actorID).Scan(&isGroup, &role); errors.Is(err, sql.ErrNoRows) {
		return errMemberRemovalStateDrifted
	} else if err != nil {
		return fmt.Errorf("revalidate group deletion authority: %w", err)
	}
	if !isGroup || role != "admin" {
		return errMemberRemovalStateDrifted
	}
	return nil
}

// lockVoiceCandidates takes the users rows in database-side sorted order, so
// two concurrent group deletions with overlapping call participants agree on
// acquisition order and cannot self-deadlock. FOR NO KEY UPDATE, not FOR
// UPDATE: nothing here writes a users key, and the weaker mode still conflicts
// with the FOR SHARE that credepoch.GuardTx takes on the edit path.
func lockVoiceCandidates(ctx context.Context, tx *sql.Tx, candidates []uuid.UUID) error {
	ids := make([]string, 0, len(candidates))
	for _, id := range candidates {
		ids = append(ids, id.String())
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE`,
		pq.Array(ids),
	); err != nil {
		return fmt.Errorf("lock participants: %w", err)
	}
	return nil
}

// revalidateVoiceCandidates fails the whole mutation closed when a participant
// joined the call after the gates were taken. Handling the newcomer instead
// would mean taking their gate from inside the transaction, which is the
// inversion clause 1 forbids.
func revalidateVoiceCandidates(
	ctx context.Context,
	tx *sql.Tx,
	convID string,
	candidates []uuid.UUID,
) error {
	current, err := readVoiceCandidates(ctx, tx, convID)
	if err != nil {
		return fmt.Errorf("re-read voice candidates: %w", err)
	}
	gated := make(map[uuid.UUID]struct{}, len(candidates))
	for _, id := range candidates {
		gated[id] = struct{}{}
	}
	for _, id := range current {
		if _, ok := gated[id]; !ok {
			return errCandidateSetDrifted
		}
	}
	return nil
}

// capturePrivateCallPlans writes one conservative obligation per candidate and
// returns the keys the post-commit completion will resolve.
func capturePrivateCallPlans(
	ctx context.Context,
	tx *sql.Tx,
	rail ActivePlanRail,
	candidates []uuid.UUID,
) ([]activepresence.PlanKey, error) {
	now := time.Now()
	plans := make([]activepresence.Plan, 0, len(candidates))
	keys := make([]activepresence.PlanKey, 0, len(candidates))
	for _, id := range candidates {
		plans = append(plans, activepresence.Plan{
			SubjectID:   id,
			Category:    activepresence.CategoryPrivateCall,
			OperationID: uuid.New(),
			// Conservative BY CONSTRUCTION: the conversation, participant and
			// voice-participant rows all die in this transaction, so no
			// resolver can ever prove an exact generation for this leg.
			Resolution: activepresence.ResolutionConservative,
			EventAt:    now,
		})
		keys = append(keys, activepresence.PlanKey{
			SubjectID: id, Category: activepresence.CategoryPrivateCall,
		})
	}
	if err := rail.CapturePlansTx(ctx, tx, plans); err != nil {
		return nil, fmt.Errorf("capture active-category plans: %w", err)
	}
	return keys, nil
}

func (h *Handler) captureGroupPlans(
	ctx context.Context,
	tx *sql.Tx,
	candidates []uuid.UUID,
) ([]activepresence.PlanKey, error) {
	return capturePrivateCallPlans(ctx, tx, h.activePlans, candidates)
}

// readVoiceCandidates lists the conversation's active-call participants. The
// same statement serves the pre-transaction read and the under-lock re-read.
func readVoiceCandidates(ctx context.Context, q rowsQuerier, convID string) ([]uuid.UUID, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT user_id FROM dm_voice_participants WHERE conversation_id = $1`, convID)
	if err != nil {
		return nil, fmt.Errorf("query voice participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan voice participant: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate voice participants: %w", err)
	}
	return candidates, nil
}

func execGroupChildDeletes(ctx context.Context, tx *sql.Tx, convID string) error {
	for _, d := range groupDeleteStatements[:len(groupDeleteStatements)-1] {
		if _, err := tx.ExecContext(ctx, d.query, convID); err != nil {
			return fmt.Errorf("delete %s: %w", d.label, err)
		}
	}
	return nil
}

func deleteGroupConversation(ctx context.Context, tx *sql.Tx, convID string) error {
	if _, err := tx.ExecContext(ctx, groupDeleteStatements[len(groupDeleteStatements)-1].query, convID); err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	return nil
}
