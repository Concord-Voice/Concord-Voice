package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// Guard sentinels (#2721). Call sites map these through mapGuardError to the
// EXACT statuses and bodies the pre-#2721 guards produced — the ~40 existing
// integration tests are the oracle and must pass unmodified.
//
// These travel out through withAuthorityCapture's write closure, which rolls
// back and returns WITHOUT calling presenceAbandon. That is correct and
// load-bearing: Abandon DISCONNECTS the captured audience, and must never fire
// for a denial where nothing was written. A guard denial is an authorization
// outcome, not a transaction failure, and must not be logged as the class-2 500.
var (
	errRoleGone         = errors.New("rbac: target role no longer exists")
	errHierarchyDenied  = errors.New("rbac: actor role hierarchy forbids this mutation")
	errEscalationDenied = errors.New("rbac: conferred permissions exceed actor permissions")
	errGuardLockTimeout = errors.New("rbac: guard timed out waiting for the target role row lock")
)

// roleFlagDenied rejects a mutation because the target role's is_managed /
// is_default flag forbids it. It carries the message because the body differs
// per handler ("Cannot modify managed roles" vs "Cannot delete managed roles" vs
// "Cannot unassign default roles") and those strings are part of the wire
// contract.
//
// It exists because the cheap pre-transaction check is NOT authoritative. #2721
// discards the pre-check's POSITION and re-derives it under FOR SHARE; leaving
// the pre-check's FLAG verdict authoritative would keep exactly the straddle
// this issue closed, one new is_managed writer away from being reachable. Today
// no endpoint writes those columns after server creation, so this is
// defence-in-depth rather than a live path — but the guard already READS both
// flags, so enforcing them costs one comparison.
type roleFlagDenied struct{ msg string }

func (e roleFlagDenied) Error() string { return e.msg }

// rejectRoleFlags applies a handler's flag policy to a guard result. An empty
// message means that handler does not reject that flag — AssignRole rejects
// neither, matching its pre-#2721 behaviour.
func rejectRoleFlags(res roleGuardResult, managedMsg, defaultMsg string) error {
	if res.IsManaged && managedMsg != "" {
		return roleFlagDenied{managedMsg}
	}
	if res.IsDefault && defaultMsg != "" {
		return roleFlagDenied{defaultMsg}
	}
	return nil
}

// conferredMode selects which permission bitfield the escalation subset check
// evaluates.
//
// It is an explicit enum rather than a nil-or-sentinel pointer because the three
// cases ask genuinely different questions, and a reader should not have to
// recognise pointer identity to tell them apart.
type conferredMode uint8

const (
	// confersNothing — the mutation grants no bits AT SERVER SCOPE, so no subset
	// check runs. DeleteRole and UnassignRole.
	//
	// For UnassignRole this is true at SERVER scope only. At CHANNEL scope the
	// resolver SUBTRACTS role-attached DENY overrides (applyChannelOverrides:
	// finalPerms &^= roleDeny), so removing a deny-bearing role is a monotonic
	// WIDENING. That is #2724 — a separate defect with its own issue and its own
	// PoC, deliberately not addressed here. Do not "fix" it by adding a subset
	// check to the unassign path; the bits it would compare are the wrong ones.
	confersNothing conferredMode = iota

	// confersRequested — the caller supplies the bitfield being written.
	// UpdateRole, checking req.Permissions.
	confersRequested

	// confersTargetRole — the mutation confers whatever the target role
	// currently carries, so the check MUST use the value the guard re-read under
	// FOR SHARE and never a pre-transaction snapshot. AssignRole.
	confersTargetRole
)

// roleGuardResult carries every value the guard read, so a call site never
// re-queries one and thereby re-opens the straddle this guard exists to close.
type roleGuardResult struct {
	IsManaged   bool
	IsDefault   bool
	Position    int
	Permissions int64
	IsOwner     bool
	// EnforceMFADangerousActions mirrors servers.enforce_mfa_dangerous_actions,
	// read UNLOCKED in the same statement and for the same reason owner_id is
	// (#3453): it costs nothing beyond the existing round trip and must never
	// gain a locking clause.
	EnforceMFADangerousActions bool
	// ActorBasePermissions is COALESCE(BIT_OR(...), 0) over the actor's roles,
	// read in the SAME statement as everything else. For a NON-OWNER it is
	// exactly what resolveServerPermissions returns (that function's non-owner
	// branch is the identical BIT_OR), which is what lets the cheap pre-check
	// reach an escalation verdict with ZERO extra round-trips. It is NOT valid
	// for an owner — resolveServerPermissions returns OwnerPermissions there —
	// so every consumer must short-circuit on IsOwner first.
	//
	// The AUTHORITATIVE path ignores this field and resolves through
	// ResolveServerPermissionsTx, which additionally distinguishes ErrNotMember.
	ActorBasePermissions int64
}

// roleGuardQuery reads every operand of BOTH guards in ONE statement, so they
// share ONE snapshot.
//
// Why one statement rather than a lock per operand: the actor ceiling is an
// aggregate, and PostgreSQL rejects a locking clause at a query level carrying
// aggregation ("FOR SHARE is not allowed with aggregate functions" — verified
// against PG 16). There is therefore NO row-lock form of the ceiling read.
// Snapshot coherence is the only available mechanism, and it is sufficient: the
// pre-#2721 defect was that rolePosition and actorMaxPosition came from two
// DIFFERENT autocommit snapshots, so raising the ceiling between the two reads
// made `5 >= 6` false and opened the guard.
//
// FOR SHARE OF r locks ONLY the target roles row. It is table-qualified on
// purpose: LockRows sits above the index scan while the aggregating SubPlans sit
// below it, so the lock's one-row scope is a plan-shape property — qualifying it
// makes that explicit instead of incidental.
//
// servers.owner_id is read UNLOCKED, deliberately and load-bearingly: that is
// what keeps a roles <-> servers lock edge from existing in EITHER direction.
// Do NOT "harden" it to FOR SHARE.
//
// EPQ, and why the residual is safe. Under READ COMMITTED, if the target row was
// updated-and-committed between this statement's snapshot and the FOR SHARE
// acquisition, LockRows runs EvalPlanQual: it re-reads the LOCKED relation's
// tuple at the newest version while the subqueries stay pinned to the original
// snapshot. So the target position may be newer than the ceiling, but THE
// CEILING CAN NEVER BE NEWER THAN THE TARGET — which is exactly the direction
// the escalation exploited. That asymmetry IS the security property; do not
// "fix" it by folding the ceiling into the locked relation.
//
// Rejected variant, recorded so a future edit is visibly a rule violation:
// dropping the aggregate and locking the actor's N role rows with a non-aggregate
// SELECT ... FOR SHARE. That locks N rows in unspecified order, making
// "T1 holds A wants B / T2 holds B wants A" constructible against any other
// multi-row roles writer. A guard takes AT MOST ONE row lock, always the target
// roles row, always after the advisory lock, never on servers or member_roles.
//
// UPDATED after #2861 (#2851) landed. An earlier revision of this comment argued
// safety from "applyRolePositions takes no advisory lock and therefore can never
// wait on us — a one-directional wait cannot close a cycle." That premise is now
// FALSE: applyRolePositions takes LockServerVisibilityCapture as its own first
// statement. The conclusion survives and is in fact stronger — both families now
// acquire the same per-server advisory key first, so they are totally ordered and
// cannot interleave at all. Do not reinstate the one-directional-wait argument;
// it describes a world that no longer exists, and the rejected variant above is
// rejected on its own merits regardless of who else holds the advisory lock.
//
// BOTH actor subqueries below server-qualify their roles join (#2869). Until
// migration 000144 they did not, while reorderGuardQuery and resolveNewRolePosition
// already did — so this guard, the one that refuses a cross-server role assignment
// via `WHERE r.id = $1 AND r.server_id = $2`, was computing the ACTOR's own ceiling
// and permission bits through exactly the hole it exists to close. A single foreign
// member_roles row raised actor_max_position to that role's position and ORed its
// bits (including bit 62) into actor_base_permissions, opening the hierarchy and
// subset checks together. Proven twice by @red-team, in #2850 and again in #3350.
// 000144's composite FK now makes such a row unrepresentable; keep these predicates
// anyway — they are what holds for a row arriving by a route the FK does not cover,
// and they cost nothing, since mr.server_id is already pinned by the WHERE clause.
//
// enforce_mfa_dangerous_actions rides this same statement, in the same
// UNLOCKED shape as owner_id and for the same reason: both are servers-table
// facts a guard reads to make its decision, not rows a guard mutation ever
// writes, so locking either would only manufacture a roles<->servers edge that
// does not need to exist (#3453).
const roleGuardSelect = `
	SELECT r.is_managed, r.is_default, r.position, r.permissions,
	       (SELECT COALESCE(MAX(r2.position), 0)
	          FROM member_roles mr
	          INNER JOIN roles r2 ON mr.role_id = r2.id AND r2.server_id = mr.server_id
	         WHERE mr.server_id = $2 AND mr.user_id = $3) AS actor_max_position,
	       (SELECT s.owner_id FROM servers s WHERE s.id = r.server_id) AS owner_id,
	       (SELECT s.enforce_mfa_dangerous_actions FROM servers s WHERE s.id = r.server_id) AS enforce_mfa_dangerous_actions,
	       (SELECT COALESCE(BIT_OR(r3.permissions), 0)
	          FROM member_roles mr3
	          INNER JOIN roles r3 ON mr3.role_id = r3.id AND r3.server_id = mr3.server_id
	         WHERE mr3.server_id = $2 AND mr3.user_id = $3) AS actor_base_permissions
	  FROM roles r
	 WHERE r.id = $1 AND r.server_id = $2`

// roleGuardQuery is the AUTHORITATIVE form, run inside the write transaction.
const roleGuardQuery = roleGuardSelect + `
	   FOR SHARE OF r`

// roleGuardPreCheckQuery is the same statement WITHOUT the locking clause, for
// the cheap pre-transaction denial. It is derived from the same constant on
// purpose: two hand-maintained copies would drift, and a pre-check that
// disagreed with the guard would produce 403s the authoritative guard would have
// allowed.
//
// It MUST NOT carry a locking clause. It runs on the pooled connection in
// autocommit, where no `SET LOCAL lock_timeout` applies, so a `FOR SHARE` here
// could block indefinitely on a concurrent `roles` writer — converting a cheap
// denial into an unbounded wait, which is the opposite of the point.
const roleGuardPreCheckQuery = roleGuardSelect

// authorizeRoleMutationTx is the authoritative hierarchy + escalation guard for
// the role-mutation family (#2721).
//
// It MUST run inside the write transaction, after LockServerVisibilityCapture
// and after capturePresenceVisibility. Hoisting it above capture breaks the
// #2445 invariant that the capture is the exact pre-write authorized audience,
// and the ordering regression tests pin that.
//
// requested is consulted only when mode is confersRequested.
func (h *Handler) authorizeRoleMutationTx(
	ctx context.Context,
	q rowQuerier,
	serverID, actorID, roleID string,
	mode conferredMode,
	requested int64,
) (roleGuardResult, error) {
	return h.evaluateRoleGuard(ctx, q, roleGuardRequest{
		Query: roleGuardQuery, ServerID: serverID, ActorID: actorID, RoleID: roleID,
		Mode: mode, Requested: requested,
	})
}

// preCheckRoleMutation is a CHEAP, NON-AUTHORITATIVE denial run on the pooled
// connection BEFORE the write transaction opens. Its only job is to stop a
// request that is certainly going to be denied from paying for a transaction.
//
// Why it exists (#2721 red-team). Moving the guard inside the write closure put
// three things ahead of every denial that were previously behind it:
//
//   - `preparePresenceCapture`, which is O(#senders) and, above
//     presenceCaptureMaxChannels, fails closed with ErrPresenceCaptureLimited —
//     turning what should be a 403 into a 500 and thereby DISCLOSING aggregate
//     voice occupancy. `authority_tx.go` names that exact signal as the class it
//     exists to contain, so a denial must not be able to observe it.
//   - `pg_advisory_xact_lock(server)`, which an actor authorized for NOTHING on
//     this role could otherwise force the server to take.
//   - the guard's own row-lock wait, which is bounded by lock_timeout but is
//     held WHILE holding that advisory lock — so one concurrent `roles` writer
//     became a ~3s server-wide stall of every role mutation, the owner's included.
//
// It is NOT authoritative and must never become so: it reads on the pool, in its
// own snapshot, exactly the way the pre-#2721 code did. The in-transaction guard
// is still the decision. That asymmetry is what makes the failure mode safe —
// **a pre-check error returns nil and falls through to the transaction**, so it
// can only ever save work, never grant it. Do not "harden" this into a denial on
// error: a transient pool blip would then 403 a legitimate mutation.
//
// It does NOT call the resolver, which would cost three more pooled queries on
// every happy-path mutation. It still denies on both halves, hierarchy and
// escalation, at no extra round trip: the guard statement returns
// ActorBasePermissions, the actor's raw BIT_OR, in the same row as the hierarchy
// operands. That value equals what the resolver returns for a NON-owner only, so
// every consumer short-circuits on IsOwner first.
//
// The pre-check verdict must equal the in-transaction verdict for the MFA mask
// too, not only for hierarchy and escalation (#3453). The authoritative guard's
// escalation check is already masked, because it resolves actorPerms through
// ResolveServerPermissionsTx, which applies MFAMask.Apply at its exit. A
// pre-check that compared unmasked ActorBasePermissions against an enforcing
// server's dangerous conferral would pass exactly the escalation the masked
// in-tx guard denies, reopening F3 for that one case: the mismatched pre-check
// waves the request into withAuthorityCapture, which fails the SAME
// unauthorized request 403-below-bound / 500-above-bound depending on how many
// channels the target holds, disclosing aggregate voice occupancy. Masking
// ActorBasePermissions here before the subset comparison closes it with no
// additional round trip on a non-enforcing server, because MaskFor issues no
// query when its enforcing argument is false.
func (h *Handler) preCheckRoleMutation(
	ctx context.Context, serverID, actorID, roleID string,
	mode conferredMode, requested int64,
) error {
	res, err := h.evaluateRoleGuard(ctx, h.db, roleGuardRequest{
		Query: roleGuardPreCheckQuery, ServerID: serverID, ActorID: actorID, RoleID: roleID,
		Mode: confersNothing,
	})
	switch {
	case errors.Is(err, errRoleGone), errors.Is(err, errHierarchyDenied):
		return err
	case err != nil:
		// A DB fault, ErrNotMember, or an unexpected sentinel — fail OPEN to the
		// authoritative guard. See the fail-open note above.
		//
		//nolint:nilerr // Deliberate and load-bearing: this pre-check is NOT
		// authoritative, and swallowing the error is what keeps it incapable of
		// granting anything. Propagating it would turn a transient pool blip into
		// a spurious 403 on a legitimate mutation, while the in-transaction guard
		// re-decides regardless. Do not "fix" this by returning err.
		return nil
	}

	// Escalation half. Gitar flagged that denying only on hierarchy left F3 — the
	// 403-below-bound / 500-above-bound voice-occupancy oracle — open for the
	// escalation path, and it was right. Closing it costs ZERO extra round-trips:
	// ActorBasePermissions came back in the statement already executed above.
	//
	// Owners short-circuit. For them ActorBasePermissions is the wrong value
	// (resolveServerPermissions returns OwnerPermissions), and owners bypass the
	// escalation check in UpdateRole/AssignRole anyway — so evaluating it here
	// would manufacture a false denial. CreateRole has no pre-check at all, which
	// is what preserves its deliberate absence of an owner bypass.
	if res.IsOwner || mode == confersNothing {
		return nil
	}

	// MFA mask (#3453). MaskFor issues no query when EnforceMFADangerousActions
	// is false, so a non-enforcing server's pre-check pays no extra round trip.
	// A read error here follows the SAME fail-open rule as the guard read above:
	// it can only ever save work, never grant it, because the authoritative
	// in-tx guard re-decides through an already-masked ResolveServerPermissionsTx.
	mask, err := MaskFor(ctx, h.db, actorID, res.EnforceMFADangerousActions)
	if err != nil {
		//nolint:nilerr // deliberate and load-bearing, matching the guard-read
		// fail-open above: this pre-check is NOT authoritative, so a P1 read
		// fault must fall through to the transaction rather than deny a
		// legitimate mutation on a transient error.
		return nil
	}
	actorPerms := mask.Apply(Permission(res.ActorBasePermissions))

	conferred := requested
	if mode == confersTargetRole {
		conferred = res.Permissions
	}
	if Permission(conferred)&^actorPerms != 0 {
		return errEscalationDenied
	}
	return nil
}

// roleGuardRequest is one guard evaluation's inputs.
//
// Its immediate reason is go:S107 — evaluateRoleGuard took 8 parameters against
// a limit of 7. Naming the three same-typed UUID strings is a genuine secondary
// benefit, because transposing actorID and roleID compiles cleanly and yields a
// guard that authorizes the wrong pair.
//
// SCOPE OF THAT PROTECTION, stated honestly (an earlier revision of this comment
// overclaimed it as "named at every call site"): it covers only the two internal
// calls from authorizeRoleMutationTx and preCheckRoleMutation. Both of those
// still take positional strings themselves, so the nine handler call sites — the
// ones an author actually edits — remain positional and remain capable of the
// transposition. Lifting the struct to those two seams would close that; until
// someone does, do not read this type as a guarantee it does not provide.
type roleGuardRequest struct {
	// Query is roleGuardQuery (authoritative, locking) or
	// roleGuardPreCheckQuery (cheap, pooled, non-locking).
	Query     string
	ServerID  string
	ActorID   string
	RoleID    string
	Mode      conferredMode
	Requested int64
}

func (h *Handler) evaluateRoleGuard(
	ctx context.Context,
	q rowQuerier,
	req roleGuardRequest,
) (roleGuardResult, error) {
	query, serverID, actorID, roleID := req.Query, req.ServerID, req.ActorID, req.RoleID
	mode, requested := req.Mode, req.Requested
	var res roleGuardResult
	var actorMaxPosition int
	var ownerID sql.NullString
	var enforceMFA sql.NullBool

	err := q.QueryRowContext(ctx, query, roleID, serverID, actorID).Scan(
		&res.IsManaged, &res.IsDefault, &res.Position, &res.Permissions,
		&actorMaxPosition, &ownerID, &enforceMFA, &res.ActorBasePermissions,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return res, errRoleGone
	case isGuardLockTimeout(err):
		return res, errGuardLockTimeout
	case err != nil:
		return res, fmt.Errorf("role guard read: %w", err)
	}

	// A servers row that vanished between request and guard leaves owner_id NULL.
	// Fail CLOSED: treat it as "not the owner" so the hierarchy check still runs.
	res.IsOwner = ownerID.Valid && ownerID.String == actorID

	// Same vanished-row case for the enforcement flag (#3453). Fail CLOSED in the
	// opposite-looking but consistent direction: treat an unreadable flag as
	// enforcing, mirroring MaskFor's own fail-closed default (Enforcing: true).
	// An invisible servers row must never silently widen what a non-owner may
	// confer.
	res.EnforceMFADangerousActions = !enforceMFA.Valid || enforceMFA.Bool

	if !res.IsOwner && res.Position >= actorMaxPosition {
		return res, errHierarchyDenied
	}

	// Owner bypasses the subset check for UpdateRole and AssignRole, matching
	// pre-#2721 behaviour. CreateRole does NOT route through this guard and has
	// no owner bypass at all, so because OwnerPermissions excludes bit 62 an owner
	// cannot CREATE a role carrying PermAdministrator. That does not keep bit 62
	// out of a server: this bypass lets an owner write it into an existing role
	// through UpdateRole (#2869). Owners may delegate PermAdministrator by
	// decision, so this bypass is intended; CreateRole's refusal is the defect,
	// tracked as #3407. Do not cite CreateRole as containment.
	if mode == confersNothing || res.IsOwner {
		return res, nil
	}

	// Reached only for non-owners.
	//
	// HONEST ACCOUNTING (corrected in review — an earlier revision of this
	// comment claimed servers.owner_id is "read exactly once", and that is
	// FALSE). resolveServerPermissions issues THREE statements of its own —
	// membership EXISTS, a SECOND `SELECT owner_id FROM servers`, and the BIT_OR
	// — and the enclosing transaction is READ COMMITTED, so each takes a fresh
	// snapshot. Deriving IsOwner from the guard statement narrows the straddle;
	// it does not eliminate it.
	//
	// Why that residual is acceptable: every divergence direction lands either
	// fail-closed or on committed truth at one of the two instants. An actor who
	// GAINED ownership between the two reads gets OwnerPermissions, which they
	// legitimately hold; one who LOST it gets the narrower set. The advisory lock
	// serialises the whole RBAC writer family, so only a non-advisory writer
	// (internal/ownership, members.RemoveMember/execBanTx, invite-join's
	// default-role insert, applyRolePositions) can commit into the window.
	//
	// Do NOT restate this as a single-snapshot guarantee. If you need one, the
	// transaction has to open at REPEATABLE READ the way
	// ResolveEffectivePermissionsUncached does — that is a real change with real
	// serialization-failure handling, not a comment edit.
	actorPerms, permErr := h.resolver.ResolveServerPermissionsTx(ctx, q, serverID, actorID)
	if permErr != nil {
		// ErrNotMember flows through to a 403 at the call site.
		return res, permErr
	}

	conferredBits := requested
	if mode == confersTargetRole {
		conferredBits = res.Permissions
	}
	if Permission(conferredBits)&^actorPerms != 0 {
		return res, errEscalationDenied
	}
	return res, nil
}

// guardLockTimeout is the SET LOCAL statement every guarded write closure runs
// as its FIRST statement inside the closure — i.e. AFTER the advisory lock and
// AFTER the capture, so the "advisory lock is the transaction's first statement"
// invariant is untouched.
//
// The guard is the first place in this family where a request can block on a ROW
// lock, and it does so WHILE HOLDING the per-server advisory lock. The pool sets
// no lock_timeout, statement_timeout, or idle_in_transaction_session_timeout, so
// a stuck applyRolePositions transaction would otherwise pin every role mutation
// on the server. This bounds only the newly introduced wait; the write that
// follows cannot wait, because it locks a row the transaction already holds.
const guardLockTimeout = `SET LOCAL lock_timeout = '3s'`

// applyGuardLockTimeout runs guardLockTimeout on the write transaction.
func applyGuardLockTimeout(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, guardLockTimeout); err != nil {
		return fmt.Errorf("set guard lock timeout: %w", err)
	}
	return nil
}

// isGuardLockTimeout reports whether err is PostgreSQL 55P03 lock_not_available,
// raised when SET LOCAL lock_timeout expires waiting for the target row lock.
func isGuardLockTimeout(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "55P03"
}

// mapGuardError translates a guarded write's error to the EXACT status and body
// the pre-#2721 guards produced. Returns true when a response was written.
//
// hierarchyMsg is the caller-specific 403 string ("Cannot delete a role at or
// above your own position", etc.); failureMsg is the caller-specific 500 body.
func (h *Handler) mapGuardError(c *gin.Context, err error, hierarchyMsg, failureMsg string) bool {
	var flagErr roleFlagDenied
	switch {
	case err == nil:
		return false
	case errors.Is(err, errRoleGone), err == sql.ErrNoRows: //nolint:errorlint // see below
		// BARE sql.ErrNoRows only, deliberately — NOT errors.Is.
		// resolveServerPermissions wraps a vanished `servers` row as
		// fmt.Errorf("failed to fetch server owner: %w", sql.ErrNoRows), so an
		// errors.Is match here rendered a genuine 500-class DB fault as
		// 404 "Role not found" — nonsensical on a CREATE, which names no role.
		// The intended producers are the guard's own errRoleGone and the write
		// path's execRequiringRow / UPDATE...RETURNING, both of which return the
		// sentinel unwrapped. A wrapped one falls to the 500 default, correctly.
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgRoleNotFound})
	case errors.Is(err, errHierarchyDenied):
		c.JSON(http.StatusForbidden, gin.H{"error": hierarchyMsg})
	case errors.As(err, &flagErr):
		c.JSON(http.StatusForbidden, gin.H{"error": flagErr.msg})
	case errors.Is(err, errEscalationDenied):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgCannotGrantPerms})
	case errors.Is(err, ErrNotMember):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
	case errors.Is(err, errGuardLockTimeout), isGuardLockTimeout(err):
		// isGuardLockTimeout is checked HERE as well as inside the guard because
		// SET LOCAL is TRANSACTION-scoped: the post-guard write can also raise
		// 55P03 on a lock the transaction does not already hold (AssignRole's FK
		// on server_members, UnassignRole's member_roles row, CreateRole's FK on
		// servers). Without this arm those fell to the generic default and lost
		// the failure_class the operator diagnoses by.
		// Distinct log class, identical on the wire — mirrors the
		// ErrPresenceCaptureLimited treatment. Do NOT promote to a new status.
		h.log.Error("Role mutation refused: guard lock timeout",
			"failure_class", "guard_lock_timeout", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failureMsg})
	default:
		h.log.Error("Role mutation failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failureMsg})
	}
	return true
}
