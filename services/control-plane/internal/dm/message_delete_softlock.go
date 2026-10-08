package dm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The fixed failure classes of the soft-lock's own log lines. They are spelled
// exactly as the channel route spells them (internal/messages), so a line says
// nothing about which route, and so which scope, produced it (C7).
const (
	failureClassSoftLockUnavailable = "delete_softlock_unavailable"
	failureClassSoftLockBudget      = "delete_softlock_budget_unavailable"
	failureClassSoftLockConfirm     = "delete_softlock_confirm_failed"
	failureClassSoftLockReset       = "delete_softlock_reset_failed"
	failureClassSoftLockClear       = "delete_softlock_budget_clear_failed"
	failureClassDeleteLockConflict  = "delete_lock_conflict"
)

// dmMessageDeleteStepUpCopy is the own-rule seam's password-path copy for a DM
// message delete past the soft-lock, shared with the password mint
// (stepup.OwnRuleCopy). Neither string names the require_auth_before_purge
// setting (H3).
var dmMessageDeleteStepUpCopy = stepup.OwnRuleCopy(stepup.PurposeDMMessageDelete)

// dmMessageDeleteTarget is the one DM message a delete request acts on, with
// the actor and the session epoch its transaction fences on.
type dmMessageDeleteTarget struct {
	convID, messageID, userID string
	actorUUID                 uuid.UUID
	tokenEpoch                string
}

// dmDeletedMessage is what the committed delete read under its locks, for the
// visibility-scoped broadcast. pinnedAt is part of it because a pinned message
// is never hidden (#3458): without it the broadcast would treat the deleted
// row as unpinned and withhold its delete from viewers who could see it.
type dmDeletedMessage struct {
	authorID  uuid.UUID
	createdAt time.Time
	pinnedAt  sql.NullTime
}

// dmDeleteFence is the delete transaction's session fence: the
// credential-epoch guard on the ordinary path, the step-up confirmation (a
// strict superset of that guard) past the soft-lock. deleteDMMessageTx runs it
// where the DM lock order (#3141) places the credential guard — after the
// users-first prefix and the conversation lock, before the participant and
// message locks.
type dmDeleteFence func(ctx context.Context, tx *sql.Tx) error

// deleteDMMessageUnderSoftLock applies the delete-rate soft-lock (#3455, spec
// §2.4) to an author-checked DM delete and writes every refusal itself.
// ownRule is the author's require_auth_before_purge: a member who turned it
// off is outside the population, never counted and never refused, and costs
// no Redis round trip. Under the threshold any step-up the body carries is
// ignored: it is neither verified nor charged to the budget.
func (h *Handler) deleteDMMessageUnderSoftLock(
	c *gin.Context, t dmMessageDeleteTarget, in stepup.Input, ownRule bool,
) (dmDeletedMessage, bool) {
	if !ownRule {
		return h.deleteDMMessageUnconfirmed(c, t)
	}
	verdict, hitErr := stepup.NewDeleteSoftLock(h.redis).Hit(c.Request.Context(), t.actorUUID, stepup.DMDeleteScope())
	if hitErr != nil {
		// C7: only a population member reaches this line, so it carries the
		// Redis error and nothing that names the member or the scope.
		h.log.Error("Delete soft-lock unavailable", "failure_class", failureClassSoftLockUnavailable, "error", hitErr.Cause)
		c.Header("Retry-After", retryAfterSeconds(stepup.DeleteSoftLockWindow))
		hitErr.Write(c)
		return dmDeletedMessage{}, false
	}
	if !verdict.Over {
		return h.deleteDMMessageUnconfirmed(c, t)
	}
	return h.confirmDMMessageDelete(c, t, in, verdict)
}

// deleteDMMessageUnconfirmed is today's delete: the credential-epoch fence and
// nothing else.
func (h *Handler) deleteDMMessageUnconfirmed(c *gin.Context, t dmMessageDeleteTarget) (dmDeletedMessage, bool) {
	deleted, err := h.deleteDMMessageTx(c.Request.Context(), t, func(ctx context.Context, tx *sql.Tx) error {
		return credepoch.GuardTx(ctx, tx, t.userID, t.tokenEpoch)
	})
	if err != nil {
		// No step-up runs on this path, so no *stepup.Error can reach the
		// soft-lock decoration and retryAfter is never read.
		h.writeDMMessageDeleteError(c, err, 0)
		return dmDeletedMessage{}, false
	}
	return deleted, true
}

// confirmDMMessageDelete is the DM message delete's ONE confirmation helper
// (D-3): everything from the budget charge to the post-commit reset runs from
// here, so #3454's grace retrofit is local to this function. The charge, the
// in-transaction confirmation and the settlement are split into
// chargeDMDeleteBudget, confirmDMDeleteTx and settleDMDeleteConfirmation only
// to keep this function readable; each has this one caller.
//
// A DM delete is author-only, so only the own rule governs it (D-1): a grace
// the own rule accepts (#3454 A-9, A-10), otherwise the account's inline MFA
// when it has any, otherwise a password step-up token minted for this purpose
// (#3509), through the seam DM Clear uses (stepup.VerifyOwnRuleTx).
//
// Budget (X4): charged before BeginTx whenever the body carries either factor,
// because only then is there a credential to guess with; cleared only after a
// verified confirmation commits, when the soft-lock is reset too and a
// delete-scope grace is granted. The grace is read here too, before BeginTx
// (A-9), and only on this over-threshold path, which already called Redis.
// Every post-commit write is best-effort: a failure leaves a counter high, or
// grants no grace, which fails toward more limiting.
func (h *Handler) confirmDMMessageDelete(
	c *gin.Context, t dmMessageDeleteTarget, in stepup.Input, verdict stepup.SoftLockVerdict,
) (dmDeletedMessage, bool) {
	ctx := c.Request.Context()
	budget := stepup.NewBudget(h.redis, stepup.MFASettingsBudgetPrefix)
	if !h.chargeDMDeleteBudget(c, budget, t.userID, in) {
		return dmDeletedMessage{}, false
	}
	grace := stepup.NewGraceStore(h.redis).Read(ctx,
		stepup.GraceActorFromContext(c, t.actorUUID), stepup.DeleteGraceScope(stepup.DMDeleteScope()))

	var confirmation dmDeleteConfirmation
	deleted, err := h.deleteDMMessageTx(ctx, t, func(ctx context.Context, tx *sql.Tx) (err error) {
		// Overwritten on every attempt: DeleteOne retries once on
		// dmblock.ErrMembershipChanged in a fresh transaction. Only
		// dmblock.PrepareConversationTx returns that error, and it runs before
		// this fence, so an attempt that is retried never reached the verifier
		// and spent nothing. Should that ever change, every factor re-verifies
		// on the retry: a TOTP step, a backup code and a step-up token (WebAuthn
		// or password) are all spent on this transaction, so the first
		// attempt's rollback restored them (#3509).
		confirmation, err = h.confirmDMDeleteTx(ctx, tx, t, in, grace)
		return err
	})
	if err != nil {
		h.writeDMMessageDeleteError(c, err, verdict.RetryAfter)
		return dmDeletedMessage{}, false
	}
	h.settleDMDeleteConfirmation(ctx, budget, t, grace, confirmation)
	return deleted, true
}

// dmDeleteConfirmation is confirmDMDeleteTx's outcome. strength is the grace
// a Verified outcome earned and is meaningless for the other two.
type dmDeleteConfirmation struct {
	outcome  mfaenforce.Outcome
	strength stepup.GraceStrength
}

// confirmDMDeleteTx is the delete transaction's fence past the soft-lock. It
// returns Verified or GraceCovered, or an error; never Unconfirmed with a nil
// error, because the own rule always applies to a member who reached it.
//
// LockSubjectTx replaces the ordinary path's credepoch.GuardTx: the same users
// FOR SHARE lock and epoch fence, plus the P1 factor set read after it. It
// runs after the conversation lock, where #3141's order puts the credential
// guard. That is the placement contract's one exception, not a breach of it:
// PrepareConversationTx already holds the actor's users row FOR SHARE (the
// actor is in its sorted users-first prefix), so taking it again adds no
// lock-order edge, and the factor set it reads cannot change while that lock
// is held. The grace is judged against that locked subject, so a password
// grace stops covering the moment the account has an inline factor.
func (h *Handler) confirmDMDeleteTx(
	ctx context.Context, tx *sql.Tx, t dmMessageDeleteTarget, in stepup.Input, grace stepup.GraceRead,
) (dmDeleteConfirmation, error) {
	subject, stepErr := stepup.LockSubjectTx(ctx, tx, t.userID, stepup.LockForShare, t.tokenEpoch)
	if stepErr != nil {
		return dmDeleteConfirmation{}, stepErr
	}
	if grace.Covers(stepup.PurposeDMMessageDelete, stepup.GraceOwnRule, subject) {
		return dmDeleteConfirmation{outcome: mfaenforce.GraceCovered}, nil
	}
	if verifyErr := stepup.VerifyOwnRuleTx(ctx, tx, h.mfaVerifier, t.userID,
		stepup.OwnRuleRoute{Purpose: stepup.PurposeDMMessageDelete, Copy: dmMessageDeleteStepUpCopy},
		in, subject); verifyErr != nil {
		return dmDeleteConfirmation{}, verifyErr
	}
	return dmDeleteConfirmation{outcome: mfaenforce.Verified, strength: stepup.OwnRuleGraceStrength(subject)}, nil
}

// chargeDMDeleteBudget charges the step-up budget when the body carries either
// factor, and answers the refusal itself when the budget is spent or
// unavailable. It reports whether the delete may go on.
func (h *Handler) chargeDMDeleteBudget(c *gin.Context, budget stepup.Budget, userID string, in stepup.Input) bool {
	if in.MFACode == "" && in.StepUpToken == "" {
		return true
	}
	budgetErr := budget.Consume(c.Request.Context(), userID)
	if budgetErr == nil {
		return true
	}
	if budgetErr.Cause != nil {
		h.log.Error("Delete soft-lock step-up budget unavailable",
			"failure_class", failureClassSoftLockBudget, "error", budgetErr.Cause)
	}
	budgetErr.Write(c)
	return false
}

// settleDMDeleteConfirmation runs a committed confirmation's writes (A-10):
// Verified or GraceCovered resets the soft-lock; Verified alone also clears
// the budget and grants a delete-scope grace of the strength it earned, so a
// grace-covered delete never slides its grace. Every write is best-effort and
// runs on stepup.SettleContext, so a client that hangs up after the commit
// cannot cancel them (review of #3509). No line here tells a grace-covered
// delete from a verified one (observability principle 7).
func (h *Handler) settleDMDeleteConfirmation(
	ctx context.Context, budget stepup.Budget, t dmMessageDeleteTarget, grace stepup.GraceRead, r dmDeleteConfirmation,
) {
	if !r.outcome.Confirmed() {
		return
	}
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	if resetErr := stepup.NewDeleteSoftLock(h.redis).Reset(ctx, t.actorUUID, stepup.DMDeleteScope()); resetErr != nil {
		h.log.Warn("Could not reset the delete soft-lock", "failure_class", failureClassSoftLockReset, "error", resetErr)
	}
	if r.outcome != mfaenforce.Verified {
		return
	}
	if clearErr := budget.Clear(ctx, t.userID); clearErr != nil {
		h.log.Warn("Could not clear the delete soft-lock step-up budget",
			"failure_class", failureClassSoftLockClear, "error", clearErr)
	}
	if grantErr := stepup.NewGraceStore(h.redis).Grant(ctx, grace, r.strength); grantErr != nil {
		h.log.Warn("Could not record the delete soft-lock step-up grace",
			"failure_class", stepup.FailureClassGraceGrant, "error", grantErr)
	}
}

// deleteDMMessageTx runs the single-message delete transaction in the DM lock
// order (#3141): dmblock.PrepareConversationTx takes the users-first prefix
// (every participant plus the actor, sorted, FOR SHARE) and then the
// conversation, and refuses a blocked or drifted topology; then the fence;
// then the actor's participant row and the message itself, which recheck the
// handler's unlocked preflight under those locks. A member removed after the
// preflight receives 403, never a misleading 404.
func (h *Handler) deleteDMMessageTx(ctx context.Context, t dmMessageDeleteTarget, fence dmDeleteFence) (dmDeletedMessage, error) {
	var deleted dmDeletedMessage
	err := h.purgeEngine.DeleteOne(ctx, t.messageID, purge.DeleteSpec{
		MessagesTable:    "dm_messages",
		ScopeColumn:      "conversation_id",
		ScopeID:          t.convID,
		AttachmentsTable: "dm_message_attachments",
		Guard: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := dmblock.PrepareConversationTx(ctx, tx, t.convID, []uuid.UUID{t.actorUUID}, dmblock.LockShare, dmblock.LockShare); err != nil {
				return err
			}
			if err := fence(ctx, tx); err != nil {
				return err
			}
			var participant string
			if err := tx.QueryRowContext(ctx,
				`SELECT user_id FROM dm_participants WHERE user_id = $1 AND conversation_id = $2 FOR SHARE`, t.userID, t.convID,
			).Scan(&participant); errors.Is(err, sql.ErrNoRows) {
				return errDMPurgeNotParticipant
			} else if err != nil {
				return fmt.Errorf("recheck DM message participant: %w", err)
			}
			var locked dmDeletedMessage
			//nolint:gosec // G202: the filter is a compile-time constant; all values are parameterized.
			if err := tx.QueryRowContext(ctx,
				`SELECT m.user_id, m.created_at, m.pinned_at FROM dm_messages m WHERE m.id = $1 AND m.conversation_id = $2`+hiddenRangeFilter(3)+` FOR UPDATE`,
				t.messageID, t.convID, t.userID,
			).Scan(&locked.authorID, &locked.createdAt, &locked.pinnedAt); err != nil {
				return fmt.Errorf("recheck DM message author: %w", err)
			}
			if locked.authorID != t.actorUUID {
				return errDMPurgeScopeChanged
			}
			deleted = locked
			return nil
		},
	})
	return deleted, err
}

// writeDMMessageDeleteError answers a failed delete transaction in spec §2.6's
// order, with #3141's statuses for the topology and authority rechecks. A lock
// conflict comes first because a 55P03 can arrive inside a 500
// *stepup.Error's Cause.
func (h *Handler) writeDMMessageDeleteError(c *gin.Context, err error, retryAfter time.Duration) {
	var stepErr *stepup.Error
	isStepErr := errors.As(err, &stepErr)
	switch {
	case mfaenforce.IsLockConflict(err):
		cause := err
		if isStepErr && stepErr.Cause != nil {
			cause = stepErr.Cause // a users-row timeout arrives inside a *stepup.Error
		}
		h.log.Warn("Failed to delete DM message", "failure_class", failureClassDeleteLockConflict, "error", cause)
		mfaenforce.WriteBusy(c)
	case isStepErr:
		h.writeDMMessageDeleteStepUpError(c, stepErr, retryAfter)
	case errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked):
		h.respondGuardTxError(c, err, errMsgFailedDeleteMessage)
	case errors.Is(err, dmblock.ErrUnavailable) || errors.Is(err, dmblock.ErrMembershipChanged):
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_unavailable"})
	case errors.Is(err, errDMPurgeNotParticipant):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
	case errors.Is(err, errDMPurgeScopeChanged):
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only delete your own messages"})
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
	default:
		h.log.Error("Failed to delete DM message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
	}
}

// writeDMMessageDeleteStepUpError writes a confirmation's *stepup.Error. Every
// 403 the own rule can produce — mfa_required, Invalid MFA code,
// password_required, Invalid password — is a soft-lock refusal, so each gains
// delete_rate_limited and a Retry-After naming when the tripped window rolls
// over; the client tells it from the route's other 403s without matching
// copy. Only the soft-lock path reaches this, so a 5xx's Cause is logged with
// a fixed failure class and nothing that names the member, the scope or the
// factors they hold (C7). The body is copied rather than mutated.
func (h *Handler) writeDMMessageDeleteStepUpError(c *gin.Context, e *stepup.Error, retryAfter time.Duration) {
	if e.Cause != nil {
		h.log.Error("Delete soft-lock confirmation failed", "failure_class", failureClassSoftLockConfirm, "error", e.Cause)
	}
	if e.Status != http.StatusForbidden {
		e.Write(c)
		return
	}
	body := make(gin.H, len(e.Body)+1)
	maps.Copy(body, e.Body)
	body["delete_rate_limited"] = true
	c.Header("Retry-After", retryAfterSeconds(retryAfter))
	c.JSON(e.Status, body)
}

// retryAfterSeconds renders d as a Retry-After value in whole seconds.
func retryAfterSeconds(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}
