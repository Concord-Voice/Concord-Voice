// Package presencecapture declares the pre-mutation Rich Presence capture
// contract for graph-destroying writes (#2446 friendship/FoF; #2447 membership
// and account lifecycle; #2448 durability).
//
// It is a LEAF: it imports nothing from internal/, so the consumer packages
// gain zero presence imports and no import cycle is expressible in either
// direction. That is the same guarantee internal/rbac gets from declaring
// PresenceRecheck at the consumer, achieved once instead of per-consumer.
//
// This contract is SEPARATE from rbac.PresenceRecheck and must stay separate.
// Per [internal]rules/backend.md, durability for the RBAC/SBAC family belongs to
// the shared enforcement outbox (#2635); nothing here may be retrofitted onto
// it. Do not unify the two interfaces.
package presencecapture

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// Family names the graph mutation shape. It is a CLOSED, dense enum: the bridge
// switches on it exhaustively, so adding a mutation family is a compile-visible
// change rather than a silently uncaptured write.
type Family uint8

// The graph mutation families #2446 hooks. Values are positional: keep them
// dense and append only, never reorder.
const (
	FamilyFriendshipAccept Family = iota
	FamilyFriendshipRemove
	FamilyBlock
	FamilyFriendsOfFriendsToggle
)

// CanRevokeVisibility reports whether this mutation is capable of REMOVING a
// viewer's authorization. It gates the peripheral leg — the viewer-scoped
// disconnect of the principals — which exists solely to clear state a viewer
// may still be holding for senders reachable only through the mutated edge.
//
// This is NOT the "direction branch" that [internal]rules/backend.md forbids for
// the #2445 RBAC family, and the distinction is worth stating because it will
// be challenged. There, assigning a role can NARROW visibility via role-targeted
// deny bits and unassigning can WIDEN it, so no static property of the mutation
// tells you which way it goes — hence the rule that every hooked RBAC write
// captures and refreshes unconditionally. Here the answer is a static property
// of the family: an accepted friendship edge is purely ADDITIVE to every
// audience it touches, so no viewer can lose authorization and no stale state
// can exist to clear. The widened audience is delivered by the focal refresh.
//
// FamilyFriendsOfFriendsToggle returns true for BOTH directions on purpose. The
// family alone cannot tell on→off from off→on, and being wrong in the revoking
// direction leaks; being wrong in the widening direction merely costs a
// reconnect. Keep it conservative unless the flag transition is threaded in.
//
// A new family must state its answer here, which is the point of switching
// exhaustively rather than defaulting.
func (f Family) CanRevokeVisibility() bool {
	switch f {
	case FamilyFriendshipAccept:
		return false
	case FamilyFriendshipRemove, FamilyBlock, FamilyFriendsOfFriendsToggle:
		return true
	default:
		// Unknown family: fail closed. An uncleared viewer is a disclosure; an
		// unnecessary disconnect is a reconnect.
		return true
	}
}

// FailPosture is declared BY THE CALL SITE. It is not a property of the family.
type FailPosture uint8

const (
	// FailClosedBlockWrite is the #2445 posture: a capture failure rolls the
	// transaction back and the handler returns 500. Nothing changed, nothing
	// disclosed, retryable. This is the zero value on purpose.
	FailClosedBlockWrite FailPosture = iota

	// FailConservativeDegrade proceeds with the write and substitutes a
	// viewer-scoped disconnect of the principals for exact reconciliation.
	// Reserved for writes where refusing is itself the security regression:
	// #2446 names blocking the priority regression, so a large friend graph
	// must not be able to deny a safety affordance. It NEVER silently no-ops
	// (see Plan.Degraded), and it applies to stage-1 capture failures ONLY.
	FailConservativeDegrade
)

// Subject is everything the call site knows before the capture reads.
//
// It deliberately carries NO audience, NO recipient set, and NO channel list:
// the bridge derives all of that from tx. A caller therefore CANNOT hand in a
// pre-transaction-computed audience, which is exactly the #2445 phase placement
// this family must not copy.
type Subject struct {
	Family      Family
	FailPosture FailPosture

	// Principal is the user whose graph edges are being created or destroyed.
	Principal uuid.UUID
	// Counterpart is the other friendship or block endpoint. uuid.Nil for
	// families that have none, such as a friends-of-friends toggle.
	Counterpart uuid.UUID
}

// Plan is the opaque capture handed back to the consumer. Consumer packages
// never inspect it and cannot name its concrete type.
type Plan interface {
	// HasWork reports whether anything was captured. An empty plan is the
	// benign terminal: no dispatch, no disconnect, the handler's normal 2xx.
	HasWork() bool
	// Degraded reports that the plan carries only the principals instead of an
	// exact delta. TWO paths set it, not one: a stage-1 capture read that failed
	// under FailConservativeDegrade, and a capture whose fan-out exceeded the
	// implementation's bound — the second is a bound, not a failure, and is
	// posture-independent.
	//
	// The bound path degrades only for a family whose CanRevokeVisibility is
	// true. An ADDITIVE family over the bound returns a clean, non-degraded,
	// empty plan instead, because it has nothing stale to clear and the
	// conservative principal set would disconnect both users for a mutation
	// that revokes nothing. So Degraded() == false does not by itself mean the
	// bound was respected — it can also mean the bound was exceeded by a
	// mutation that needed no reconciliation (PR #2770 review, CodeRabbit).
	//
	// It is NOT a viewer superset. Third parties who saw a sender only through
	// the mutated edge are not covered and fall back to the presence TTL; an
	// earlier version of this contract called it a superset, which overstated
	// the guarantee to every consumer of this interface (PR #2738 review).
	//
	// It exists so a call site can record a counter — NOT so it can branch its
	// business logic.
	Degraded() bool
}

// ErrCaptureBound marks a capture refused because its fan-out exceeded the
// bridge's bound. Declared at the consumer boundary so handlers can classify
// with errors.Is without importing the implementation.
var ErrCaptureBound = errors.New("presence capture fan-out bound exceeded")

// Cause is the abandon vocabulary. It is a named type, not a bare string,
// because the bridge BRANCHES on it and it is logged as a fixed `failure_class`
// enum: an untyped parameter invited a caller to pass an interpolated DB error
// straight into that field, which is both a log-hygiene break and a silent
// change of meaning at a security boundary (PR #2738 review, @security-reviewer).
//
// This mirrors the mfa.CredEpoch / mfa.JWTSecret precedent in this codebase —
// the type does not make a determined `Cause(fmt.Sprintf(...))` impossible, it
// makes the accidental version a compile error and states the intent where a
// caller reads it.
type Cause string

// The closed vocabulary. A cause that proves the transaction never committed
// must not fan a disconnect out over the captured audience.
const (
	// CauseWriteFailed is the mutation write erroring inside the transaction.
	// The handler returns immediately and the deferred rollback discards it.
	CauseWriteFailed Cause = "write_failed"
	// CauseRowsAffected is the driver failing to report the write's row count.
	// The handler returns immediately and the deferred rollback discards it.
	CauseRowsAffected Cause = "rows_affected"
	// CauseCommitUnresolved is a COMMIT that neither succeeded nor provably
	// failed. The outcome is unknown, so this one must fail closed.
	CauseCommitUnresolved Cause = "commit_unresolved"
)

// CauseProvesNoCommit reports whether the cause is positive proof that the
// transaction did not commit.
//
// Every hooked site abandons and then returns, leaving its deferred
// RollbackUnlessDone to discard the transaction, so for these causes NO write
// landed: no viewer's authorization changed and there is nothing stale to
// clear. Disconnecting anyway hands an unauthenticated-relationship caller a
// fan-out over a stranger's whole captured audience (#2738), which is the same
// defect class as the RemoveFriend rowsAffected == 0 branch.
//
// CauseCommitUnresolved is deliberately NOT in this set: an unresolved commit
// is unknown state and unknown state fails closed.
func CauseProvesNoCommit(cause Cause) bool {
	switch cause {
	case CauseWriteFailed, CauseRowsAffected:
		return true
	default:
		return false
	}
}

// GraphPresenceCapture reconciles Rich Presence against a graph-destroying
// mutation.
//
// Both methods take *sql.Tx. That is the enforcement mechanism, not a
// convention: there is no pre-transaction method, so the #2445 split is not
// expressible here. For this family the graph read IS the state the write
// destroys, so it must run under the same transaction as the write.
type GraphPresenceCapture interface {
	// CaptureInTx is IN-TRANSACTION and runs BEFORE the mutation write.
	//
	// Required call-site sequence, with NOTHING between the capture and the
	// write:
	//
	//	tx, err := db.BeginTx(ctx, nil)
	//	defer rollbackUnlessDone(tx)
	//	<authorization / precondition reads>
	//	plan, err := capture.CaptureInTx(ctx, tx, subject)
	//	<THE MUTATION WRITE>
	//	err = capture.Complete(ctx, tx, plan)   // commits; do NOT tx.Commit()
	//
	// A nil error with HasWork() == false is the ordinary empty terminal.
	CaptureInTx(ctx context.Context, tx *sql.Tx, subject Subject) (Plan, error)

	// Complete is IN-TRANSACTION-TERMINAL: it COMMITS tx and then dispatches.
	// It is the last statement that may touch tx. A non-nil error means the
	// mutation did not commit, or is unproven, and the handler must return 5xx.
	//
	// A handler that commits for itself gets sql.ErrTxDone from this call.
	Complete(ctx context.Context, tx *sql.Tx, plan Plan) error

	// Abandon is the fail-closed terminal for a path that will not reach
	// Complete. It disconnects the captured audience. Safe with a nil or empty
	// plan. It does NOT touch tx.
	Abandon(plan Plan, cause Cause)
}
