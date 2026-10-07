package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gates on the role routes (#3454): CreateRole, UpdateRole
// when the request writes permissions, and DeleteRole. On a server that
// enforces MFA on dangerous actions each asks the actor for an inline factor
// under the gate's locks; on one that does not, a request with no mfa_code is
// answered exactly as before. Each runs, in order:
//
//  1. chargeRoleGate, before BeginTx: the budget charge and, for the one
//     grace-eligible purpose (roles.delete), the grace read. These are the only
//     Redis calls the gate makes (A-2, A-9);
//  2. in the route's transaction, after any credepoch.GuardTx it already runs
//     (A-8): lockRoleGateTx, then the route's own permission and hierarchy
//     verdicts (I7), then any prior-state read, then requireRoleGate, then the
//     write;
//  3. after COMMIT, settleRoleGate.
//
// An UpdateRole that does not carry permissions never reaches any of this
// (I-UNGATED): it takes no new lock and charges nothing.

// dangerousGrantPermissions is the set whose conferral the role and override
// grant gates confirm (#3454 X9): a role write or an override allow is gated
// when it newly confers one of these bits. It is narrower than
// DangerousPermissions, the set the mask withholds: conferring ManageRoles, for
// example, hands out no authority the actor could not already exercise.
const dangerousGrantPermissions = PermAdministrator | PermManageDevResources | PermManageCryptoRotation

// grantsDangerous reports whether writing next over prior newly confers a bit
// in dangerousGrantPermissions: (next &^ prior) & D ≠ 0. A row that does not
// exist yet, or could not be read, is prior 0, which can only over-gate.
func grantsDangerous(prior, next int64) bool {
	return (Permission(next)&^Permission(prior))&dangerousGrantPermissions != 0
}

// errRoleGateLock marks a lock conflict on the gate's own servers lock, so
// mapGuardError answers it with the gate's 503 rather than with the role-row
// guard's 500. It is wrapped beside the 55P03 or 40P01 it marks, never in
// place of it.
var errRoleGateLock = errors.New("rbac: role gate lock conflict")

// errRoleCommitLock marks a deadlock or lock timeout PostgreSQL reported at
// CreateRole's COMMIT. Like errRoleGateLock it is wrapped beside the 55P03 or
// 40P01 it marks.
var errRoleCommitLock = errors.New("rbac: role commit lock conflict")

// roleCommitError marks a lock conflict at COMMIT for isRoleGateError. A COMMIT
// PostgreSQL refused rolled the transaction back, so the retryable
// lock_conflict 503 is true of it, as WithGateTx and the channel expiration
// route already answer it; it used to reach mapGuardError's 500 (Codex review
// of #3454). Any other commit error is returned unchanged. The role-row
// guard's own lock timeout is not a commit error and keeps its 500.
func roleCommitError(err error) error {
	if err != nil && mfaenforce.IsLockConflict(err) {
		return fmt.Errorf("%w: %w", errRoleCommitLock, err)
	}
	return err
}

// roleGateConfirm is what a role gate confirms with: the route's purpose, the
// request's mfa_code, the grace read before BeginTx (zero for an always-fresh
// purpose), and the session's cred_epoch claim for LockGateTx.
type roleGateConfirm struct {
	purpose    stepup.Purpose
	code       string
	grace      stepup.GraceRead
	tokenEpoch string
}

// chargeRoleGate is a role gate's pre-transaction half. It charges the
// dangerous-action budget when code is non-empty, whatever the server's
// setting (C6), and reads the grace only for a grace-eligible purpose: an
// always-fresh one (roles.create, roles.update) would ignore it, so reading it
// would cost a Redis round trip for nothing. A grace that cannot be read is no
// grace, so the actor is prompted. It writes the charge's refusal and returns
// false.
func (h *Handler) chargeRoleGate(
	c *gin.Context, serverID, actorID string, purpose stepup.Purpose, code string,
) (roleGateConfirm, bool) {
	ctx := c.Request.Context()
	if e := stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), actorID, code); e != nil {
		mfaenforce.WriteError(c, h.log, e, nil)
		return roleGateConfirm{}, false
	}
	confirm := roleGateConfirm{purpose: purpose, code: code, tokenEpoch: middleware.TokenCredentialEpoch(c)}
	if !purpose.GraceEligible() {
		return confirm, true
	}
	server, serverErr := uuid.Parse(serverID)
	actor, actorErr := uuid.Parse(actorID)
	if serverErr == nil && actorErr == nil {
		confirm.grace = stepup.NewGraceStore(h.redis).Read(ctx, stepup.GraceActorFromContext(c, actor),
			stepup.DangerousActionGraceScope(server, int64(PermManageRoles)))
	}
	return confirm, true
}

// lockRoleGateTx takes the gate's locks on a role transaction, after the
// credepoch.GuardTx that transaction already runs (A-8), so the gate's own
// epoch and missing-row refusals are unreachable backstops. userLock must be no
// stronger than the strongest lock the transaction already holds on the
// actor's users row: CreateRole holds it FOR SHARE (GuardTx), the authority
// wrappers FOR NO KEY UPDATE (LockAuthorityPrincipalsTx), and upgrading SHARE
// to NO KEY UPDATE deadlocks two concurrent holders.
//
// A missing servers row is ErrNotMember, the 403 #3508 gives every role route
// for a server that vanished mid-request. A lock conflict on the gate's own
// statements is marked with errRoleGateLock; a *stepup.Error passes through.
func lockRoleGateTx(
	ctx context.Context, tx *sql.Tx, serverID, actorID string,
	userLock stepup.Lock, serverLock mfaenforce.ServerLock, in roleGateConfirm,
) (mfaenforce.Gate, error) {
	g, err := mfaenforce.LockGateTx(ctx, tx, serverID, actorID, userLock, serverLock, in.tokenEpoch)
	var stepErr *stepup.Error
	switch {
	case err == nil:
		return g, nil
	case errors.Is(err, mfaenforce.ErrServerNotFound):
		return g, ErrNotMember
	case errors.As(err, &stepErr):
		return g, err
	case mfaenforce.IsLockConflict(err):
		return g, fmt.Errorf("%w: %w", errRoleGateLock, err)
	default:
		return g, fmt.Errorf("lock role gate: %w", err)
	}
}

// requireRoleGate is the gate's confirmation, on the role transaction. It runs
// after that transaction's own permission and hierarchy verdicts (I7) and
// before any write, so a refusal writes nothing. The owner reaches it too
// (I-ID). fires is the route's predicate: true for DeleteRole, grantsDangerous
// for CreateRole and UpdateRole.
func (h *Handler) requireRoleGate(
	ctx context.Context, tx *sql.Tx, g mfaenforce.Gate, actorID string, in roleGateConfirm, fires bool,
) (mfaenforce.Outcome, error) {
	outcome, e := mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
		Verifier: h.mfaVerifier,
		ActorID:  actorID,
		Purpose:  in.purpose,
		Code:     in.code,
		Grace:    in.grace,
	}, fires)
	if e != nil {
		return outcome, e
	}
	// Not `return outcome, e`: a nil *stepup.Error in an error is non-nil.
	return outcome, nil
}

// settleRoleGate runs once the role write has COMMITTED: on a verified outcome
// it clears the budget and, for roles.delete, grants the grace. Any other
// outcome settles nothing, so a grace-covered delete never extends its grace,
// and an always-fresh purpose carries a zero grace read, which grants nothing.
func (h *Handler) settleRoleGate(ctx context.Context, outcome mfaenforce.Outcome, in roleGateConfirm, actorID string) {
	mfaenforce.Settle(ctx, h.log, outcome, stepup.DangerousActionBudget(h.redis), stepup.NewGraceStore(h.redis),
		in.grace, actorID)
}

// isRoleGateError reports whether err is the gate's to answer through
// mfaenforce.WriteError: a *stepup.Error (a gate, RS4 or RS5 refusal, or a
// verification fault), a lock conflict on the gate's own lock, or one at
// CreateRole's COMMIT. Every other error, including a 55P03 on the role-row
// guard's lock, keeps the route's own answer.
func isRoleGateError(err error) bool {
	var stepErr *stepup.Error
	return errors.As(err, &stepErr) || errors.Is(err, errRoleGateLock) || errors.Is(err, errRoleCommitLock)
}

// roleOverrideAllowsQuery is the BIT_OR of every ALLOW bit a role carries
// through permission overrides on its own server: channel overrides, which the
// resolver ORs into the holder's channel permissions (applyChannelOverrides),
// and category overrides, which the category sync copies onto synced children,
// including a channel created in or moved into the category later with no gate
// (C11). Assigning the role confers all of it, so RS4 must count it. A role
// with permissions = 0 and a dangerous override is otherwise a subset of
// every masked set (Codex security review of #3454).
//
// It takes no row lock. Writing a role-targeted override needs
// ManageChannels, which the mask withholds from an unenrolled actor on an
// enforcing server (RS5), so the actor RS4 constrains cannot race a dangerous
// override in behind this read. An enrolled actor who can write one is not the
// actor RS4 holds.
//
// The same-server predicates match what can confer: the resolver applies a
// role override only on the role's own server. Both target lookups use
// (target_type, target_id) indexes.
const roleOverrideAllowsQuery = `
	SELECT COALESCE(BIT_OR(allow), 0) FROM (
		SELECT o.allow FROM channel_permission_overrides o
		JOIN channels c ON c.id = o.channel_id
		WHERE o.target_type = 'role' AND o.target_id = $1 AND c.server_id = $2
		UNION ALL
		SELECT o.allow FROM category_permission_overrides o
		JOIN channel_groups g ON g.id = o.category_id
		WHERE o.target_type = 'role' AND o.target_id = $1 AND g.server_id = $2
	) AS allows`

// roleOverrideAllows reads roleOverrideAllowsQuery on q.
func roleOverrideAllows(ctx context.Context, q rowQuerier, serverID, roleID string) (int64, error) {
	var allows int64
	if err := q.QueryRowContext(ctx, roleOverrideAllowsQuery, roleID, serverID).Scan(&allows); err != nil {
		return 0, fmt.Errorf("rbac: read role override allows: %w", err)
	}
	return allows, nil
}

// unenrolledOwnerConferral is RS4 (#3454 §6) for the authoritative guard: on an
// enforcing server, an owner with no inline factor who assigns a role is held
// to the masked owner set, MFAMask{Enforcing: true}.Apply(OwnerPermissions),
// instead of bypassing the subset check. What the assignment confers is the
// role's own bitfield plus every ALLOW bit its overrides carry
// (roleOverrideAllowsQuery). A conferral with a bit outside the masked set is
// refused with the enrollment body: the refusal an inline factor would lift.
// An enrolled owner keeps the bypass and pays no override read. Both reads run
// on q, the guard's transaction, and a failed read is a 500, never a grant.
func unenrolledOwnerConferral(ctx context.Context, q rowQuerier, req roleGuardRequest, base int64) error {
	methods, err := stepup.InlineMFAMethods(ctx, q, req.ActorID)
	if err != nil {
		return stepup.VerificationFailed(fmt.Errorf("rbac: read owner MFA enrollment: %w", err))
	}
	if len(methods) > 0 {
		return nil
	}
	allows, err := roleOverrideAllows(ctx, q, req.ServerID, req.RoleID)
	if err != nil {
		return stepup.VerificationFailed(err)
	}
	return maskedOwnerConferral(base | allows)
}

// maskedOwnerConferral refuses conferred when it carries a bit the masked owner
// set lacks. It is RS4's verdict, shared by the guard and its pooled pre-check.
func maskedOwnerConferral(conferred int64) error {
	if Permission(conferred)&^(MFAMask{Enforcing: true}).Apply(OwnerPermissions) != 0 {
		return stepup.EnrollmentRequired()
	}
	return nil
}
