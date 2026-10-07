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
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
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
// step-up token: the password itself reaches only the mint endpoint, #3509).
// admitDeleteTx is the one place that choice is made, for every route in this
// package, and on a purge it is also where the D1 gate (#3454 A-3) confirms:
// one lock, one decision, at most one verification for both.
//
// Grace (#3454 D-2, A-9, A-10). A committed, verified confirmation grants a
// 10-minute delete-scope grace for the actor's session, stamped with the
// strength it was earned with; a later trip in the same session and scope is
// confirmed by that grace, with no prompt and no verifier call, when the
// governing rule accepts its strength (stepup.GraceRead.Covers). The grace is
// read before the transaction opens, only over the threshold, and judged under
// the locks the confirmation takes. A grace-covered confirmation resets the
// counters but neither clears the budget nor grants, so the window never
// slides.
//
// Invariant: over the threshold, a delete commits only if a factor verified
// in the same transaction, a grace the governing rule accepts — judged under
// that transaction's locks — covered it, or no rule applies to the actor any
// more under those locks. Breaking it takes a branch that answers "confirmed"
// without either, or one that proceeds unverified while a rule still applies —
// the in-flight OFF flip below is where the second is easy to write — or a
// grace judged against a rule other than the one the locked re-read chose.
//
// Logging (C7). Only population members reach the lines in this file, so none
// of them carries user_id, a scope, server_id, the enforcement flag, MFA
// methods, the governing rule or which rule put the actor in the population.
// Each carries a fixed failure_class and the error, nothing else, and no line
// or field tells a grace-covered confirmation from a verified one
// (observability principle 7).

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
	// deleteAdmission.rule).
	enforcing bool
	// ownRule is true when the actor is the author of what is being deleted
	// and their require_auth_before_purge is on (a missing row is on).
	ownRule bool
	purpose stepup.Purpose
	input   stepup.Input
	epoch   string
	// grace is the delete-scope grace read before the transaction, over the
	// threshold only (armSoftLockConfirmation). The zero value covers nothing,
	// so every other path is prompted as before.
	grace stepup.GraceRead
}

// softLockResult is one confirmation's outcome. strength is the grace a
// Verified outcome earned and is meaningless for the other two.
type softLockResult struct {
	outcome  mfaenforce.Outcome
	strength stepup.GraceStrength
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
// reports its outcome: Verified when a factor verified, GraceCovered when the
// pre-read grace covered it under that rule, Unconfirmed when no rule applies
// any more. It is the composed admission (admitDeleteTx) with nothing but the
// soft-lock to confirm, which is what the single-message delete has, and like
// it must be the transaction's first statement. The error is classified with
// respondSoftLockError.
func (h *Handler) confirmSoftLockTx(ctx context.Context, tx *sql.Tx, g softLockGate) (softLockResult, error) {
	return h.admitDeleteTx(ctx, tx, &deleteAdmission{gate: g, mode: confirmOverThreshold})
}

// confirmMode is what the soft-lock asks of an admission.
type confirmMode int

const (
	// confirmNone: nothing. The soft-lock counted the action under the
	// threshold, or never counted it.
	confirmNone confirmMode = iota
	// confirmOverThreshold: the action is over the threshold, so the governing
	// rule must confirm it.
	confirmOverThreshold
	// confirmRecheck: the unlocked read put the actor OUTSIDE the population,
	// so the admission confirms only if the server turned enforcement on in
	// flight. Without it the action would carry neither a confirmation nor a
	// count, and the flip would wave it through (review of #3509).
	confirmRecheck
)

// confirmRule is the rule an admission confirms under (A-3.3).
type confirmRule int

const (
	// ruleNone admits the action unverified (Unconfirmed).
	ruleNone confirmRule = iota
	// ruleServer is the server rule: inline MFA only.
	ruleServer
	// ruleOwn is the own rule: MFA when the account has an inline factor,
	// otherwise a password step-up token.
	ruleOwn
)

// deleteAdmission is one delete's or purge's composed admission (#3454 A-3):
// the soft-lock's ask and, on a purge, the D1 gate over the plan's specs
// whose author is not the actor. admitDeleteTx admits it under ONE lock and
// ONE confirmation decision, so a request that both rules apply to verifies
// once: a second ConfirmTx would be refused by acceptTOTPStep as a replay,
// and a token can be spent only once.
//
// Invariant: the admission commits only if no rule applies under its locks,
// or every rule that applies is confirmed by one verified factor or by every
// applicable grace — and on an enforcing server only after the D1 specs'
// authority was re-checked under those same locks (I7). Breaking it takes a
// confirmation before that check, a second confirmation, a grace for one
// scope standing in for another's, or provenance recorded as confirmed for a
// D1 spec the server rule did not confirm.
type deleteAdmission struct {
	// gate is the actor's soft-lock gate: who, where, the purpose, the
	// request's factor and epoch, the unlocked population read, and the
	// delete-scope grace (read over the threshold only).
	gate softLockGate
	mode confirmMode
	// d1 is the purge plan's D1 specs, empty when it has none (A-3.1).
	d1 []purge.DeleteSpec
	// d1Grace is the dangerous-action grace for purgeDangerousBit, read before
	// the transaction (A-9). It is judged only when d1 is non-empty.
	d1Grace stepup.GraceRead
	// authorize is the plan's per-spec authority check (selfPurge.authorize),
	// run over d1 before any confirmation.
	authorize func(context.Context, *sql.Tx, purge.DeleteSpec) error

	// The purge route's answers to a refusal.
	retryAfter          time.Duration
	notFound, forbidden string

	// result, enforcing and err are what the admission found. They count only
	// if its transaction committed; see settleSelfPurge.
	result softLockResult
	// enforcing is the flag as read under the servers-row lock.
	enforcing bool
	// err is the admission's own error, which refuseSelfPurge answers.
	err error
}

// admitDeleteTx is the composed admission, the one helper every soft-locked
// confirmation and every purge's Plan.Admit runs (A-3.1). It must be the
// transaction's first statement: it takes LockGateTx(users FOR SHARE, servers
// FOR SHARE) once (lockSoftLockGateTx), re-reads the enforcement flag under
// it, and decides by A-3.3's table (rule). When a rule applies and the plan
// has D1 specs, it first runs the plan's authority check over each of them
// (I7, A-3.2), so an actor who lost their authority is refused as the batch
// would refuse them and never sees mfa_required; that check's channels and
// server_members locks are children of servers, and the order is users →
// servers → those → the factor write → the audit row. On a server that does
// not enforce and with no soft-lock confirmation to make, nothing is checked
// or confirmed and the admission is what it was before #3454.
//
// The error is a *stepup.Error (a refusal, a 401, or a 500 with Cause),
// mfaenforce.ErrServerNotFound, the authority check's refusal, or a wrapped
// database error.
func (h *Handler) admitDeleteTx(ctx context.Context, tx *sql.Tx, a *deleteAdmission) (softLockResult, error) {
	gate, err := lockSoftLockGateTx(ctx, tx, a.gate)
	if err != nil {
		return softLockResult{}, err
	}
	a.enforcing = gate.Enforcing
	rule := a.rule(gate.Enforcing)
	if rule == ruleNone {
		return softLockResult{}, nil
	}
	for _, ds := range a.d1 {
		if err := a.authorize(ctx, tx, ds); err != nil {
			return softLockResult{}, err
		}
	}
	if rule == ruleServer {
		return h.confirmServerRuleTx(ctx, tx, a.gate, gate.Subject, a.graceCovers(gate.Subject))
	}
	return h.verifyOwnRuleTx(ctx, tx, a.gate, gate.Subject)
}

// rule is A-3.3's decision table, on the flag as re-read under the
// servers-row lock (enforcing), never on the unlocked read, so a flip in
// either direction is observed:
//
//   - enforcing, with a D1 spec or a soft-lock ask: the server rule, once.
//     Turned ON in flight, it governs even for an actor the own rule brought
//     here, and a password no longer passes.
//   - not enforcing and over the threshold: the own rule, exactly as before
//     #3454. Turned OFF in flight it governs if it applies; when no rule
//     applies any more the action proceeds unverified. That arm needs both
//     reads: an actor the unlocked read did not put in the population is
//     never waved through here, but asked for the own rule's factor, so a
//     caller that confirms outside the population is refused. D1 does not
//     fire.
//   - otherwise: unverified.
func (a *deleteAdmission) rule(enforcing bool) confirmRule {
	switch {
	case enforcing && (len(a.d1) > 0 || a.mode != confirmNone):
		return ruleServer
	case enforcing || a.mode != confirmOverThreshold:
		return ruleNone
	case a.gate.enforcing && !a.gate.ownRule:
		return ruleNone // turned OFF in flight, and no rule applies to this actor
	default:
		return ruleOwn
	}
}

// graceCovers reports whether the pre-read graces confirm the server rule for
// every reason it applies: the D1 specs' dangerous-action grace, and the
// delete-scope grace when the soft-lock asked. Each covers only its own
// scope, so a purge both apply to needs both. A recheck read no grace (the
// population check that sent it made no Redis call), so it is always
// prompted. Called only under ruleServer, where at least one reason applies.
func (a *deleteAdmission) graceCovers(subj stepup.Subject) bool {
	p := a.gate.purpose
	d1 := len(a.d1) == 0 || a.d1Grace.Covers(p, stepup.GraceServerRule, subj)
	softLock := a.mode == confirmNone || a.gate.grace.Covers(p, stepup.GraceServerRule, subj)
	return d1 && softLock
}

// d1Provenance is the provenance the plan's D1 specs carry once the
// admission committed (A-3.6): confirmed when it confirmed the server rule, by
// a factor or by grace; unconfirmed when the server did not enforce.
func (a *deleteAdmission) d1Provenance() PurgeProvenance {
	if a.enforcing && a.result.outcome.Confirmed() {
		return PurgeConfirmed
	}
	return PurgeUnconfirmed
}

// softLockCharged reports whether the soft-lock charged this request's
// factor to the budget (armSoftLockConfirmation): over the threshold, with a
// factor present.
func (a *deleteAdmission) softLockCharged() bool {
	return a.mode == confirmOverThreshold && a.gate.hasFactor()
}

// lockSoftLockGateTx takes the soft-lock's locks — the actor's users row,
// fenced against the token's epoch, then the servers row — and re-reads the
// enforcement flag under them.
func lockSoftLockGateTx(ctx context.Context, tx *sql.Tx, g softLockGate) (mfaenforce.Gate, error) {
	return mfaenforce.LockGateTx(ctx, tx, g.serverID, g.userID,
		stepup.LockForShare, mfaenforce.ServerForShare, g.epoch)
}

// confirmServerRuleTx confirms under the server rule: covered, when the
// pre-read graces cover it (A-10; graceCovers judges them against the
// subject read under the lock), otherwise an inline MFA factor, whatever the
// account's own setting. ConfirmTx rather than RequireConfirmationTx: the
// latter's nil also means "not enforcing", and the callers must tell the two
// apart. mfaenforce.Require is not used for the same reason: it answers
// Unconfirmed for a gate that does not enforce, and this helper must prompt
// whenever it is reached.
func (h *Handler) confirmServerRuleTx(ctx context.Context, tx *sql.Tx, g softLockGate, subj stepup.Subject, covered bool) (softLockResult, error) {
	if covered {
		return softLockResult{outcome: mfaenforce.GraceCovered}, nil
	}
	if e := mfaenforce.ConfirmTx(ctx, tx, subj, h.mfaVerifier, g.userID, g.purpose, g.input.MFACode); e != nil {
		return softLockResult{}, e
	}
	return softLockResult{outcome: mfaenforce.Verified, strength: stepup.GraceStrengthMFA}, nil
}

// verifyOwnRuleTx confirms under the own rule (stepup.VerifyOwnRuleTx, the DM
// Clear seam): a grace the own rule accepts (A-9: mfa, or password while the
// account has no inline factor), otherwise the inline MFA code when the
// account has a factor, otherwise a password step-up token minted for this
// route's purpose (#3509).
func (h *Handler) verifyOwnRuleTx(ctx context.Context, tx *sql.Tx, g softLockGate, subj stepup.Subject) (softLockResult, error) {
	if g.grace.Covers(g.purpose, stepup.GraceOwnRule, subj) {
		return softLockResult{outcome: mfaenforce.GraceCovered}, nil
	}
	route := stepup.OwnRuleRoute{Purpose: g.purpose, Copy: softLockStepUpCopy}
	if e := stepup.VerifyOwnRuleTx(ctx, tx, h.mfaVerifier, g.userID, route, g.input, subj); e != nil {
		return softLockResult{}, e
	}
	return softLockResult{outcome: mfaenforce.Verified, strength: stepup.OwnRuleGraceStrength(subj)}, nil
}

// chargeMessageDelete is the single-message delete's soft-lock charge: nothing
// outside the population (no Redis call); one attempt counted inside it; and,
// over the threshold, the budget charged and the grace read into g before the
// delete transaction opens (armSoftLockConfirmation). verdict.Over tells the
// caller to confirm. On a refusal it has written the 503 or 429 and returns
// false.
func (h *Handler) chargeMessageDelete(ctx context.Context, c *gin.Context, g *softLockGate) (stepup.SoftLockVerdict, bool) {
	if !g.inPopulation() {
		return stepup.SoftLockVerdict{}, true
	}
	verdict, ok := h.chargeSoftLock(ctx, c, *g, 1)
	if !ok || !verdict.Over {
		return verdict, ok
	}
	return verdict, h.armSoftLockConfirmation(ctx, c, g)
}

// armSoftLockConfirmation is the over-threshold path's pre-transaction Redis
// work, shared by the channel delete and both self-purges: the budget charge
// (consumeSoftLockBudget), then the delete-scope grace read into g.grace. Both
// run before BeginTx, because no Redis call may sit inside the confirming
// transaction (A-9), and only here, where the soft-lock has already called
// Redis, so a request under the threshold or outside the population costs no
// extra round trip (A-10). The read never fails the request: any Redis error
// reads as no grace, so the actor is prompted. On a budget refusal it has
// written the 429 or 503 and returns false.
func (h *Handler) armSoftLockConfirmation(ctx context.Context, c *gin.Context, g *softLockGate) bool {
	if !h.consumeSoftLockBudget(ctx, c, *g) {
		return false
	}
	// chargeSoftLock has already parsed the same inputs, so this cannot fail;
	// if it did, the zero read prompts.
	if uid, scope, err := g.scope(); err == nil {
		g.grace = stepup.NewGraceStore(h.redis).Read(ctx,
			stepup.GraceActorFromContext(c, uid), stepup.DeleteGraceScope(scope))
	}
	return true
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
	return h.admitStepUpCharge(c, stepup.NewBudget(h.redis, stepup.MFASettingsBudgetPrefix).Consume(ctx, g.userID))
}

// admitStepUpCharge answers a budget charge's outcome e: nil admits; a 429 or
// 503 is written, the 503 logging its Cause, and returns false. A purge's D1
// charge (armD1Purge) is answered here too, on the same budget, so the line
// it logs cannot tell which rule charged the request.
func (h *Handler) admitStepUpCharge(c *gin.Context, e *stepup.Error) bool {
	if e == nil {
		return true
	}
	if e.Cause != nil {
		h.log.Error("Delete soft-lock step-up budget unavailable",
			"failure_class", failureClassSoftLockBudget, "error", e.Cause)
	}
	e.Write(c)
	return false
}

// settleSoftLock runs the single-message delete's post-commit writes: its
// admission had the soft-lock alone to confirm (settleAdmission).
func (h *Handler) settleSoftLock(ctx context.Context, g softLockGate, r softLockResult, succeeded bool) {
	h.settleAdmission(ctx, &deleteAdmission{gate: g, mode: confirmOverThreshold, result: r}, succeeded)
}

// settleAdmission runs an admission's post-commit writes (A-10, A-3.5). Call
// it only once the transaction that produced a.result COMMITTED; succeeded is
// whether the action itself succeeded. A confirmed (verified or grace-covered)
// soft-lock ask resets the counters once the action succeeded. A Verified
// outcome clears the budget once, and grants each grace it earned: the
// delete-scope one, of the strength it earned, when the soft-lock asked, and
// the D1 specs' mfa one when the server rule confirmed them. Unconfirmed
// settles nothing, and a grace-covered confirmation neither clears the budget
// nor grants, so a grace never slides.
func (h *Handler) settleAdmission(ctx context.Context, a *deleteAdmission, succeeded bool) {
	r := a.result
	if succeeded && r.outcome.Confirmed() && a.mode != confirmNone {
		h.resetSoftLock(ctx, a.gate)
	}
	if r.outcome != mfaenforce.Verified {
		return
	}
	h.clearSoftLockBudget(ctx, a.gate)
	var stamped stepup.GraceRead
	if a.mode != confirmNone {
		stamped = h.grantGrace(ctx, a.gate.grace, r.strength)
	}
	if len(a.d1) > 0 && a.enforcing {
		// The soft-lock grant may have just seeded the generations both reads
		// found absent; without adopting them this grant's SET NX would find
		// them and refuse (stepup.GraceStore.GrantRead).
		h.grantGrace(ctx, a.d1Grace.AdoptGenerations(stamped), stepup.GraceStrengthMFA)
	}
}

// grantGrace records a verified confirmation as a grace of strength, stamped
// with what read found before the transaction, and returns the read it
// stamped (zero when it granted nothing). Best-effort, and on
// stepup.SettleContext, like clearSoftLockBudget: a failure only prompts
// sooner. Every grace it grants logs the same line, which names no scope.
func (h *Handler) grantGrace(ctx context.Context, read stepup.GraceRead, strength stepup.GraceStrength) stepup.GraceRead {
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	stamped, err := stepup.NewGraceStore(h.redis).GrantRead(ctx, read, strength)
	if err != nil {
		h.log.Warn("Could not record the delete soft-lock step-up grace",
			"failure_class", stepup.FailureClassGraceGrant, "error", err)
	}
	return stamped
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

// resetSoftLock clears both tiers after a confirmed (verified or
// grace-covered) action succeeded. Best-effort, and on stepup.SettleContext, like
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
