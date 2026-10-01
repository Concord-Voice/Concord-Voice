package messages

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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The delete-rate soft-lock (#3455, design spec §2.4–§2.9 as amended by D-1).
//
// Population. An action is counted when the server enforces MFA on dangerous
// actions (the server rule) or when the actor is deleting their OWN messages
// and their require_auth_before_purge setting is on, a missing row reading as
// on (the own rule). A moderator deleting someone else's message on a
// non-enforcing server is outside both, is never counted and never refused.
// Both inputs are read before the permission check and consulted only after
// it (invariant I7), so nothing a member is refused with depends on them.
//
// Governing rule. Over the threshold, the strictest applicable rule confirms:
// the server rule (inline MFA only) when the server enforces, otherwise the
// own rule (MFA when the account has an inline factor, otherwise a password
// step-up token: the password itself reaches only the mint endpoint, #3509). confirmSoftLockTx is the one place that choice is made, for
// every route in this package; #3454 retrofits its grace check there.
//
// Invariant: over the threshold, a delete commits only if a factor verified
// in the same transaction, or no rule applies to the actor any more under the
// locks that transaction took. Breaking it takes a branch that answers
// "confirmed" without a verified factor, or one that proceeds unverified while
// a rule still applies — the in-flight OFF flip below is where the second is
// easy to write.
//
// Logging (C7). Only population members reach the lines in this file, so none
// of them carries user_id, a scope, server_id, the enforcement flag, MFA
// methods, the governing rule or which rule put the actor in the population.
// Each carries a fixed failure_class and the error, nothing else.

// The fixed failure classes of the soft-lock's own log lines.
const (
	failureClassSoftLockUnavailable = "delete_softlock_unavailable"
	failureClassSoftLockBudget      = "delete_softlock_budget_unavailable"
	failureClassSoftLockConfirm     = "delete_softlock_confirm_failed"
	failureClassSoftLockReset       = "delete_softlock_reset_failed"
	failureClassSoftLockClear       = "delete_softlock_budget_clear_failed"
	failureClassDeleteLockConflict  = "delete_lock_conflict"
)

// softLockStepUpCopy is the own rule's password-path copy on every soft-locked
// route, shared with the password mint (stepup.OwnRuleCopy). It names no
// setting and no server state (H3). The three purposes share one copy.
var softLockStepUpCopy = stepup.OwnRuleCopy(stepup.PurposeMessageDelete)

// softLockGate is one request's soft-lock state: who is acting, where, which
// rules apply to them, and the factor the request carried.
type softLockGate struct {
	userID   string
	serverID string
	// enforcing is servers.enforce_mfa_dangerous_actions as read WITHOUT a lock
	// (the role_guard precedent). It decides population membership only; the
	// confirmation chooses its rule from the servers-row lock's re-read (see
	// confirmSoftLockTx).
	enforcing bool
	// ownRule is true when the actor is the author of what is being deleted
	// and their require_auth_before_purge is on (a missing row is on).
	ownRule bool
	purpose stepup.Purpose
	input   stepup.Input
	epoch   string
}

// inPopulation reports whether the soft-lock counts this action at all.
func (g softLockGate) inPopulation() bool { return g.enforcing || g.ownRule }

// scope parses the counter's key inputs. userID comes from the auth context
// and serverID from a database row, so a parse failure is a fault, never a
// member's doing.
func (g softLockGate) scope() (uuid.UUID, stepup.SoftLockScope, error) {
	uid, userErr := uuid.Parse(g.userID)
	sid, serverErr := uuid.Parse(g.serverID)
	if err := errors.Join(userErr, serverErr); err != nil {
		return uuid.Nil, stepup.SoftLockScope{}, fmt.Errorf("delete soft-lock scope: %w", err)
	}
	return uid, stepup.ServerDeleteScope(sid), nil
}

// hasFactor reports whether the request carried a step-up factor, which is
// what the shared attempt budget charges for (X4).
func (g softLockGate) hasFactor() bool {
	return g.input.MFACode != "" || g.input.StepUpToken != ""
}

// confirmSoftLockTx runs the governing rule's confirmation inside tx and
// reports whether a factor verified. It must be the transaction's first
// statement: it locks the actor's users row first (stepup.LockSubjectTx
// placement contract) and then the servers row, whichever rule applies, so the
// order is users → servers → the factor write → the caller's own locks. On
// the channel delete those are #3142's authority locks (channels, then
// server_members, then the message), all children of servers; their
// credential-epoch guard re-takes the users row this already holds, which
// adds no lock-order edge.
//
// The rule is chosen from the flag as re-read under the servers-row lock,
// never from the unlocked read, so a flip in either direction is observed.
// Turned ON in flight, the server rule governs even for an actor the own rule
// brought here, and a password no longer passes. Turned OFF in flight, the own
// rule governs if it applies, reusing the subject the gate already locked; only
// when no rule applies any more does the action proceed unverified, and then
// confirmed is false, so the caller neither resets the counters nor clears the
// budget. That arm needs both reads: an actor the unlocked read did not put in
// the population is never waved through here, but asked for the own rule's
// factor, so a caller that confirms outside the population is refused.
//
// The error is a *stepup.Error (a refusal, a 401, or a 500 with Cause),
// mfaenforce.ErrServerNotFound, or a wrapped database error; classify it with
// respondSoftLockError.
func (h *Handler) confirmSoftLockTx(ctx context.Context, tx *sql.Tx, g softLockGate) (bool, error) {
	gate, err := lockSoftLockGateTx(ctx, tx, g)
	if err != nil {
		return false, err
	}
	if gate.Enforcing {
		return h.confirmServerRuleTx(ctx, tx, g, gate.Subject)
	}
	if g.enforcing && !g.ownRule {
		return false, nil // turned OFF in flight, and no rule applies to this actor
	}
	return h.verifyOwnRuleTx(ctx, tx, g, gate.Subject)
}

// recheckSoftLockTx admits an action the unlocked read put OUTSIDE the
// population, and like confirmSoftLockTx must be the transaction's first
// statement. That read is unlocked, so the flag is re-read under the same
// users → servers locks: still not enforcing, nothing applies and the action
// proceeds unverified (confirmed false); turned ON in flight, the server rule
// governs exactly as in confirmSoftLockTx, so a request without a verified
// inline factor is refused. The own rule is not re-read: an owner can turn
// enforcement on for someone else, but nobody else can turn on this actor's
// own setting. Without this re-read the action would carry neither a
// confirmation nor a count, and the flip would wave it through (review of
// #3509).
func (h *Handler) recheckSoftLockTx(ctx context.Context, tx *sql.Tx, g softLockGate) (bool, error) {
	gate, err := lockSoftLockGateTx(ctx, tx, g)
	if err != nil || !gate.Enforcing {
		return false, err
	}
	return h.confirmServerRuleTx(ctx, tx, g, gate.Subject)
}

// lockSoftLockGateTx takes the soft-lock's locks — the actor's users row,
// fenced against the token's epoch, then the servers row — and re-reads the
// enforcement flag under them.
func lockSoftLockGateTx(ctx context.Context, tx *sql.Tx, g softLockGate) (mfaenforce.Gate, error) {
	return mfaenforce.LockGateTx(ctx, tx, g.serverID, g.userID,
		stepup.LockForShare, mfaenforce.ServerForShare, g.epoch)
}

// confirmServerRuleTx confirms under the server rule: an inline MFA factor,
// whatever the account's own setting. ConfirmTx rather than
// RequireConfirmationTx: the latter's nil also means "not enforcing", and the
// callers must tell the two apart.
func (h *Handler) confirmServerRuleTx(ctx context.Context, tx *sql.Tx, g softLockGate, subj stepup.Subject) (bool, error) {
	if e := mfaenforce.ConfirmTx(ctx, tx, subj, h.mfaVerifier, g.userID, g.purpose, g.input.MFACode); e != nil {
		return false, e
	}
	return true, nil
}

// verifyOwnRuleTx confirms under the own rule (stepup.VerifyOwnRuleTx, the DM
// Clear seam): the inline MFA code when the account has a factor, otherwise a
// password step-up token minted for this route's purpose (#3509).
func (h *Handler) verifyOwnRuleTx(ctx context.Context, tx *sql.Tx, g softLockGate, subj stepup.Subject) (bool, error) {
	route := stepup.OwnRuleRoute{Purpose: g.purpose, Copy: softLockStepUpCopy}
	if e := stepup.VerifyOwnRuleTx(ctx, tx, h.mfaVerifier, g.userID, route, g.input, subj); e != nil {
		return false, e
	}
	return true, nil
}

// chargeMessageDelete is the single-message delete's soft-lock charge: nothing
// outside the population (no Redis call); one attempt counted inside it; and,
// over the threshold, the budget charged before the delete transaction opens.
// verdict.Over tells the caller to confirm. On a refusal it has written the
// 503 or 429 and returns false.
func (h *Handler) chargeMessageDelete(ctx context.Context, c *gin.Context, g softLockGate) (stepup.SoftLockVerdict, bool) {
	if !g.inPopulation() {
		return stepup.SoftLockVerdict{}, true
	}
	verdict, ok := h.chargeSoftLock(ctx, c, g, 1)
	if !ok || !verdict.Over {
		return verdict, ok
	}
	return verdict, h.consumeSoftLockBudget(ctx, c, g)
}

// chargeSoftLock records n authorized delete attempts on the server scope and
// returns the verdict. When the soft-lock cannot be evaluated it has written
// the 503 and returns false: a counter that no-ops is the fail-open D9 forbids.
func (h *Handler) chargeSoftLock(ctx context.Context, c *gin.Context, g softLockGate, n int64) (stepup.SoftLockVerdict, bool) {
	uid, scope, err := g.scope()
	if err != nil {
		h.writeSoftLockUnavailable(c, err)
		return stepup.SoftLockVerdict{}, false
	}
	verdict, e := stepup.NewDeleteSoftLock(h.redis).HitN(ctx, uid, scope, n)
	if e != nil {
		h.writeSoftLockUnavailable(c, e.Cause)
		return stepup.SoftLockVerdict{}, false
	}
	return verdict, true
}

// writeSoftLockUnavailable answers a soft-lock that cannot be evaluated: 503,
// Retry-After one window, and a body that names no setting (H3).
func (h *Handler) writeSoftLockUnavailable(c *gin.Context, cause error) {
	h.log.Error("Delete soft-lock unavailable", "failure_class", failureClassSoftLockUnavailable, "error", cause)
	c.Header("Retry-After", strconv.Itoa(int(stepup.DeleteSoftLockWindow/time.Second)))
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": stepup.ErrMsgDeleteGuardUnavailable})
}

// consumeSoftLockBudget charges the shared step-up budget before the
// confirmation transaction opens, and only when the request carries a factor:
// a factor-less request learns nothing but the refusal every member gets
// (X4, H4). On a refusal it has written the 429 or 503 and returns false.
func (h *Handler) consumeSoftLockBudget(ctx context.Context, c *gin.Context, g softLockGate) bool {
	if !g.hasFactor() {
		return true
	}
	if e := stepup.NewBudget(h.redis, stepup.MFASettingsBudgetPrefix).Consume(ctx, g.userID); e != nil {
		if e.Cause != nil {
			h.log.Error("Delete soft-lock step-up budget unavailable",
				"failure_class", failureClassSoftLockBudget, "error", e.Cause)
		}
		e.Write(c)
		return false
	}
	return true
}

// clearSoftLockBudget clears the budget after a verified confirmation has
// committed. Best-effort: a failure leaves the counter high, which limits
// more, never less. It runs on stepup.SettleContext, so a hang-up cannot skip
// it and a stalled Redis cannot hold it.
func (h *Handler) clearSoftLockBudget(ctx context.Context, g softLockGate) {
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	if err := stepup.NewBudget(h.redis, stepup.MFASettingsBudgetPrefix).Clear(ctx, g.userID); err != nil {
		h.log.Warn("Could not clear the delete soft-lock step-up budget",
			"failure_class", failureClassSoftLockClear, "error", err)
	}
}

// resetSoftLock clears both tiers after a verified confirmation's action
// succeeded. Best-effort, and on stepup.SettleContext, like
// clearSoftLockBudget; a failure leaves the member prompted again sooner,
// never later.
func (h *Handler) resetSoftLock(ctx context.Context, g softLockGate) {
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	uid, scope, err := g.scope()
	if err == nil {
		err = stepup.NewDeleteSoftLock(h.redis).Reset(ctx, uid, scope)
	}
	if err != nil {
		h.log.Warn("Could not reset the delete soft-lock",
			"failure_class", failureClassSoftLockReset, "error", err)
	}
}

// respondSoftLockError answers an error from a transaction that may have run
// confirmSoftLockTx, in design spec §2.6's order:
//
//  1. a lock conflict, first, because a 55P03 can arrive inside a 500
//     *stepup.Error's Cause;
//  2. a *stepup.Error: a 403 is a soft-lock refusal and carries
//     delete_rate_limited and Retry-After, a 5xx logs its Cause;
//  3. a missing server or row, answered with notFound;
//  4. anything else, a logged 500 with failed.
//
// The ordinary path's credential-epoch refusal never arrives here: on the
// delete, #3142's authority guard writes its own 401 through
// respondGuardTxError, and the confirmation's is a *stepup.Error.
func (h *Handler) respondSoftLockError(c *gin.Context, err error, retryAfter time.Duration, notFound, failed string) {
	var stepErr *stepup.Error
	isStepErr := errors.As(err, &stepErr)
	switch {
	case mfaenforce.IsLockConflict(err):
		cause := err
		if isStepErr && stepErr.Cause != nil {
			cause = stepErr.Cause // a users-row timeout arrives inside a *stepup.Error
		}
		h.log.Warn(failed, "failure_class", failureClassDeleteLockConflict, "error", cause)
		mfaenforce.WriteBusy(c)
	case isStepErr:
		h.writeSoftLockStepUpError(c, stepErr, retryAfter)
	case errors.Is(err, mfaenforce.ErrServerNotFound), errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": notFound})
	default:
		h.log.Error(failed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failed})
	}
}

// writeSoftLockStepUpError writes a confirmation's *stepup.Error. Every 403
// the confirmation can produce is a soft-lock refusal — mfa_required, Invalid
// MFA code, mfa_enrollment_required, password_required, Invalid password — so
// each gains delete_rate_limited and a Retry-After naming when the window
// that tripped rolls over. The body is copied rather than mutated.
func (h *Handler) writeSoftLockStepUpError(c *gin.Context, e *stepup.Error, retryAfter time.Duration) {
	if e.Cause != nil {
		h.log.Error("Delete soft-lock confirmation failed",
			"failure_class", failureClassSoftLockConfirm, "error", e.Cause)
	}
	if e.Status != http.StatusForbidden {
		e.Write(c)
		return
	}
	body := make(gin.H, len(e.Body)+1)
	maps.Copy(body, e.Body)
	body["delete_rate_limited"] = true
	c.Header("Retry-After", strconv.Itoa(max(1, int(retryAfter/time.Second))))
	c.JSON(e.Status, body)
}

// runBeforeSoftLockConfirmHook runs the test-only seam that sits between the
// unlocked population read and the transaction that re-reads it. Nil in
// production: nothing outside this package's tests can set it.
func (h *Handler) runBeforeSoftLockConfirmHook() {
	if h.beforeSoftLockConfirmHook != nil {
		h.beforeSoftLockConfirmHook()
	}
}
