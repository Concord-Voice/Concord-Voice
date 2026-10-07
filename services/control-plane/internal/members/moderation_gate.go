package members

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gate on the moderation routes (#3454): a ban, and a
// kick that purges the target's messages. Each runs, in order:
//
//  1. chargeModerationGate, before BeginTx: the budget charge and the grace
//     read, the only Redis calls the gate makes (A-2, A-9);
//  2. in the existing ban or removal transaction, LockGateTx in place of the
//     plain servers FOR UPDATE, after lockModerationUsersTx and GuardTx (A-8),
//     then the transaction's own authorizeModerationTx (the owner skips it),
//     then requireModerationGate;
//  3. after COMMIT, settleModerationGate.
//
// A kick that purges nothing never reaches any of this (I-UNGATED).

// moderationConfirm is what a moderation gate confirms with: the request's
// mfa_code and the grace read before BeginTx. The zero value carries neither,
// so a gate that fires on it prompts.
type moderationConfirm struct {
	code  string
	grace stepup.GraceRead
}

// chargeModerationGate is a moderation gate's pre-transaction half: it charges
// the dangerous-action budget when the request carries a code, whatever the
// server's setting (C6), then reads the grace for bit, the dangerous permission
// the route requires. It writes the charge's refusal and returns false. A grace
// that cannot be read is no grace, so the actor is prompted.
func (h *Handler) chargeModerationGate(
	c *gin.Context, serverID, actorID, code string, bit rbac.Permission,
) (moderationConfirm, bool) {
	ctx := c.Request.Context()
	if e := stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), actorID, code); e != nil {
		mfaenforce.WriteError(c, h.log, e, nil)
		return moderationConfirm{}, false
	}
	confirm := moderationConfirm{code: code}
	server, serverErr := uuid.Parse(serverID)
	actor, actorErr := uuid.Parse(actorID)
	if serverErr == nil && actorErr == nil {
		confirm.grace = stepup.NewGraceStore(h.redis).Read(ctx, stepup.GraceActorFromContext(c, actor),
			stepup.DangerousActionGraceScope(server, int64(bit)))
	}
	return confirm, true
}

// lockModerationGateTx is the moderation transactions' servers lock taken
// through the gate: LockGateTx(NO KEY UPDATE, FOR UPDATE), after
// lockModerationUsersTx and GuardTx (A-8). Re-taking the actor's users row at
// the strength lockModerationUsersTx already holds adds no edge, and GuardTx
// has already fenced the session, so the gate's own epoch and missing-row
// refusals are unreachable backstops. A missing servers row becomes vanished,
// the route's own answer for a server deleted mid-request (#3508).
func lockModerationGateTx(
	ctx context.Context, tx *sql.Tx, serverID, actorID, credentialEpoch string, vanished error,
) (mfaenforce.Gate, error) {
	g, err := mfaenforce.LockGateTx(ctx, tx, serverID, actorID,
		stepup.LockForNoKeyUpdate, mfaenforce.ServerForUpdate, credentialEpoch)
	if errors.Is(err, mfaenforce.ErrServerNotFound) {
		return g, vanished
	}
	if err != nil {
		return g, fmt.Errorf("lock moderation gate: %w", err)
	}
	return g, nil
}

// requireModerationGate is the gate's confirmation, on the moderation
// transaction. It runs after that transaction's own permission check (I7) and
// before any write or authority-channel lock, so a refusal leaves nothing
// behind: not even the fail-closed channel recovery a later error triggers.
// The owner reaches it too (I-ID). A ban and a purging kick are unconditional
// D1 actions, so the gate always fires.
func (h *Handler) requireModerationGate(
	ctx context.Context, tx *sql.Tx, g mfaenforce.Gate, actorID string, purpose stepup.Purpose, in moderationConfirm,
) (mfaenforce.Outcome, error) {
	outcome, e := mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
		Verifier: h.mfaVerifier,
		ActorID:  actorID,
		Purpose:  purpose,
		Code:     in.code,
		Grace:    in.grace,
	}, true)
	if e != nil {
		return outcome, e
	}
	// Not `return outcome, e`: a nil *stepup.Error in an error is non-nil.
	return outcome, nil
}

// settleModerationGate runs once the ban or removal has COMMITTED: on a
// verified outcome it clears the budget and grants the grace. Any other
// outcome settles nothing, so a grace-covered action never extends its grace.
func (h *Handler) settleModerationGate(ctx context.Context, outcome mfaenforce.Outcome, in moderationConfirm, actorID string) {
	mfaenforce.Settle(ctx, h.log, outcome, stepup.DangerousActionBudget(h.redis), stepup.NewGraceStore(h.redis),
		in.grace, actorID)
}

// writeModerationDenial writes a moderation preflight's permission refusal.
// When that refusal is the generic 403 and the MFA mask caused it, the actor
// gets RS5's mfa_enrollment_required instead (#3454 D-3); every other answer
// is written unchanged. It reads on the pool, as the denial did: the preflight
// holds no transaction.
func (h *Handler) writeModerationDenial(
	c *gin.Context, serverID, actorID string, bit rbac.Permission, status int, msg string,
) {
	if status == http.StatusForbidden {
		if e := rbac.EnrollmentDenial(c.Request.Context(), h.db, serverID, "", actorID, bit); e != nil {
			mfaenforce.WriteError(c, h.log, e, nil)
			return
		}
	}
	c.JSON(status, gin.H{"error": msg})
}
