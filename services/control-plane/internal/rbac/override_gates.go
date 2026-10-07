package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gates on the two override upserts (#3454 T8),
// UpsertChannelOverride and UpsertCategoryOverride. They reuse the role gate's
// helpers (role_gates.go) in the same order:
//
//  1. chargeRoleGate, before BeginTx. Both purposes are always fresh, so it
//     reads no grace;
//  2. inside the authority transaction's write, after the GuardTx the write
//     already runs (A-8): lockRoleGateTx at NO KEY UPDATE and servers FOR
//     UPDATE, both already held by withAuthorityCapture, so the gate adds no
//     lock edge; then the route's ManageChannels and escalation verdicts (I7);
//     then the row locks the write already took; then the prior allow, and
//     requireRoleGate when the new allow newly confers a dangerous bit;
//  3. after COMMIT, settleRoleGate.
//
// The gate fires on (new_allow &^ prior_allow) & D ≠ 0 (grantsDangerous).
// Denies are never gated: adding or removing one confers nothing. On an
// enforcing server the category upsert also fires when its own sync copy would
// newly confer a D bit on a synced child, for ANY target (categoryCopyFires):
// the copy rewrites every child from every category row, so a request whose
// own row confers nothing can still restore another target's dangerous allow a
// child had diverged from. The copy that runs when a channel is created in, or
// moved into, a synced category stays ungated (C11).

// The prior allow is read FOR NO KEY UPDATE, the lock the upsert's own
// ON CONFLICT DO UPDATE takes anyway (it writes no key column), so it adds no
// lock strength. Under READ COMMITTED it reads the latest committed version; a
// row that does not exist yet, or was deleted concurrently, is prior 0, which
// can only over-gate (#3454 §4).
const (
	channelOverridePriorAllowQuery = `
		SELECT allow FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = $2 AND target_id = $3
		FOR NO KEY UPDATE`
	categoryOverridePriorAllowQuery = `
		SELECT allow FROM category_permission_overrides
		WHERE category_id = $1 AND target_type = $2 AND target_id = $3
		FOR NO KEY UPDATE`
)

// categoryCopyFiresQuery is the category upsert's copy arm in one statement:
// for each post-write category row (the category's current rows with the
// request's own row substituted or added) and each locked synced child,
// (cat_allow &^ child_allow) & D ≠ 0, a missing child row being 0. It takes no
// row lock, so it adds no upgrade ahead of the copy's own DELETE. The children
// are already held FOR UPDATE by withStableSyncedCategoryAuthority, the lock
// the channel override upsert and delete (lockChannelOverrideAuthority) and
// every category override write take before touching these rows, so under
// READ COMMITTED it reads what the copy will overwrite. A category row the
// copy would skip (a role outside the server) is still counted, which can
// only over-gate.
const categoryCopyFiresQuery = `
	SELECT EXISTS (
		SELECT 1
		FROM (
			SELECT target_type, target_id, allow FROM category_permission_overrides
			WHERE category_id = $1 AND NOT (target_type = $2 AND target_id = $3)
			UNION ALL
			SELECT $2::varchar, $3::uuid, $4::bigint
		) AS post
		CROSS JOIN unnest($5::uuid[]) AS child(channel_id)
		LEFT JOIN channel_permission_overrides AS cur
			ON cur.channel_id = child.channel_id
			AND cur.target_type = post.target_type AND cur.target_id = post.target_id
		WHERE ((post.allow & ~COALESCE(cur.allow, 0)) & $6::bigint) <> 0
	)`

// authorizeOverrideUpsert is both upserts' pooled pre-check: ManageChannels at
// server scope, then the escalation check on the requested allow. A
// ManageChannels denial the MFA mask caused is answered with RS5's
// mfa_enrollment_required (#3454 §14.1), read on the pool as the denial was
// decided; any other denial keeps the generic 403. It writes the response and
// returns false on any refusal.
func (h *Handler) authorizeOverrideUpsert(c *gin.Context, serverID, userID string, req UpsertOverrideRequest) bool {
	ctx := c.Request.Context()
	hasPerm, err := h.resolver.HasPermission(ctx, serverID, userID, "", PermManageChannels)
	if err != nil {
		h.log.Error(errMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return false
	}
	if !hasPerm {
		if e := EnrollmentDenial(ctx, h.db, serverID, "", userID, PermManageChannels); e != nil {
			mfaenforce.WriteError(c, h.log, e, nil)
			return false
		}
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
		return false
	}
	actorPerms, err := h.resolver.GetEffectivePermissions(ctx, serverID, userID, "")
	if err != nil {
		h.log.Error(errMsgFailedGetActorPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPermissions})
		return false
	}
	if !actorPerms.Has(PermAdministrator) && Permission(req.Allow)&^actorPerms != 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgCannotGrantPerms})
		return false
	}
	return true
}

// authorizeOverrideUpsertTx is the head of both upserts' write: GuardTx and
// its classification, unchanged (A-8); then the gate's locks; then the
// route's ManageChannels and escalation verdicts on tx (I7).
//
// A ManageChannels denial the mask caused is answered with RS5 on tx, at
// server scope like the check that made it. The escalation denial needs no
// RS5: ManageChannels is a masked bit, so an actor the mask binds has already
// been refused above.
func (h *Handler) authorizeOverrideUpsertTx(
	ctx context.Context, tx *sql.Tx, serverID, userID, tokenEpoch string, allow int64, confirm roleGateConfirm,
) (mfaenforce.Gate, error) {
	if err := credepoch.GuardTx(ctx, tx, userID, tokenEpoch); err != nil {
		return mfaenforce.Gate{}, err
	}
	g, err := lockRoleGateTx(ctx, tx, serverID, userID, stepup.LockForNoKeyUpdate, mfaenforce.ServerForUpdate, confirm)
	if err != nil {
		return g, err
	}
	actorPerms, err := h.resolveManageChannelsTx(ctx, tx, serverID, userID)
	if errors.Is(err, errManageChannelsDenied) {
		if e := EnrollmentDenial(ctx, tx, serverID, "", userID, PermManageChannels); e != nil {
			return g, e
		}
	}
	if err != nil {
		return g, err
	}
	if !actorPerms.Has(PermAdministrator) && Permission(allow)&^actorPerms != 0 {
		return g, errEscalationDenied
	}
	return g, nil
}

// overrideGateInput is what requireOverrideGate decides over: the prior-allow
// read (priorQuery, keyed by parentID, the channel or the category), the
// request, the actor and their confirmation, and the category's locked synced
// child set. The channel upsert, which copies nothing, leaves syncedChildIDs
// nil.
type overrideGateInput struct {
	priorQuery, parentID string
	req                  UpsertOverrideRequest
	actorID              string
	confirm              roleGateConfirm
	syncedChildIDs       []string
}

// requireOverrideGate runs the gate when the requested allow newly confers a
// dangerous grant over the target row's prior allow, read with in.priorQuery
// (keyed by in.parentID, the channel or the category), or, for the category
// upsert, when its sync copy would (categoryCopyFires). in.syncedChildIDs is
// the category's locked synced child set; the channel upsert, which copies
// nothing, passes nil. Both arms feed the one Require. An allow that carries
// no dangerous bit cannot fire the first arm whatever the prior holds, so it
// skips the read and leaves the statement trace as it was before #3454. It
// runs after the route's verdicts and before any write, so a refusal writes
// nothing.
func (h *Handler) requireOverrideGate(
	ctx context.Context, tx *sql.Tx, g mfaenforce.Gate, in overrideGateInput,
) (mfaenforce.Outcome, error) {
	fires, err := overrideAllowFires(ctx, tx, in.priorQuery, in.parentID, in.req)
	if err != nil {
		return mfaenforce.Unconfirmed, err
	}
	// Require ignores fires off an enforcing server, so the copy arm is read
	// only where it can decide something.
	if !fires && g.Enforcing && len(in.syncedChildIDs) > 0 {
		if fires, err = categoryCopyFires(ctx, tx, in.parentID, in.req, in.syncedChildIDs); err != nil {
			return mfaenforce.Unconfirmed, err
		}
	}
	return h.requireRoleGate(ctx, tx, g, in.actorID, in.confirm, fires)
}

// overrideAllowFires is the request's own arm: grantsDangerous over the
// target row's prior allow, a missing row being prior 0.
func overrideAllowFires(ctx context.Context, tx *sql.Tx, priorQuery, parentID string, req UpsertOverrideRequest) (bool, error) {
	if Permission(req.Allow)&dangerousGrantPermissions == 0 {
		return false, nil
	}
	var prior int64
	err := tx.QueryRowContext(ctx, priorQuery, parentID, req.TargetType, req.TargetID).Scan(&prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read prior override allow: %w", err)
	}
	return grantsDangerous(prior, req.Allow), nil
}

// categoryCopyFires is the category upsert's copy arm: whether rewriting the
// locked synced children from the post-write category rows newly confers a
// dangerous grant on any target of any child (categoryCopyFiresQuery).
func categoryCopyFires(
	ctx context.Context, tx *sql.Tx, categoryID string, req UpsertOverrideRequest, syncedChildIDs []string,
) (bool, error) {
	var fires bool
	if err := tx.QueryRowContext(ctx, categoryCopyFiresQuery, categoryID, req.TargetType, req.TargetID, req.Allow,
		pq.Array(syncedChildIDs), int64(dangerousGrantPermissions)).Scan(&fires); err != nil {
		return false, fmt.Errorf("read category copy conferral: %w", err)
	}
	return fires, nil
}
