// Package presencecapture declares the pre-mutation Rich Presence capture
// contract for graph-destroying writes (#2446 friendship/FoF; #2447 membership
// and account lifecycle; #2448 durability).
//
// Family policy is declared in a REGISTRY, not a switch. PolicyFor returns
// ErrFamilyUnregistered for a family with no entry, CaptureInTx propagates it
// fail-closed, and requireGraphPresenceFamilyRegistry fatal-exits at boot on
// any gap. That combination -- not exhaustive switching, which a `default` arm
// silently defeats -- is what makes adding a mutation family a visible change
// rather than a silently uncaptured write.
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
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Family names the graph mutation shape. It is a CLOSED, dense enum, and the
// REGISTRY below — not a switch — is what makes adding a mutation family a
// visible change rather than a silently uncaptured write.
//
// The switch this replaced ended in `default: return true`, so an appended
// family compiled and inherited an answer nobody wrote down. For an ADDITIVE
// family that default seeded plan.viewers, which is a full device teardown of
// the joining user. #2447 appends four families to this enum; a dense
// append-only enum makes simultaneous appends collide AND compile, so the
// registry plus the boot guard is the mechanism that catches it.
type Family uint8

// The graph mutation families #2446 hooks. Values are positional: keep them
// dense and append only, never reorder. Append ABOVE familyCount.
const (
	FamilyFriendshipAccept Family = iota
	FamilyFriendshipRemove
	FamilyBlock
	FamilyFriendsOfFriendsToggle

	// familyCount is NOT a family. It is the exclusive upper bound of the dense
	// enum, maintained by iota so an appended family raises it with no second
	// edit — which is what lets UnregisteredFamilies see a family whose author
	// forgot the registry entry.
	familyCount
)

// FamilyPolicy is one family's declared answer on both independent axes.
//
// The axes are genuinely independent and must not be collapsed.
// CanRevokeVisibility is false for FamilyFriendshipAccept, but accept WIDENS a
// Custom Status audience and there is no heartbeat to deliver the widening, so
// CarriesCustomTextTopology is true for it. Reusing one field for both would
// drop the widening on the floor.
type FamilyPolicy struct {
	// CanRevokeVisibility reports whether this mutation is capable of REMOVING
	// a viewer's authorization. It gates the peripheral leg — the viewer-scoped
	// disconnect of the principals — which exists solely to clear state a viewer
	// may still be holding for senders reachable only through the mutated edge.
	//
	// This is NOT the "direction branch" that [internal]rules/backend.md forbids
	// for the #2445 RBAC family, and the distinction is worth stating because it
	// will be challenged. There, assigning a role can NARROW visibility via
	// role-targeted deny bits and unassigning can WIDEN it, so no static
	// property of the mutation tells you which way it goes — hence the rule that
	// every hooked RBAC write captures and refreshes unconditionally. Here the
	// answer is a static property of the family: an accepted friendship edge is
	// purely ADDITIVE to every audience it touches, so no viewer can lose
	// authorization and no stale state can exist to clear. The widened audience
	// is delivered by the focal refresh.
	//
	// FamilyFriendsOfFriendsToggle is true for BOTH directions on purpose. The
	// family alone cannot tell on→off from off→on, and being wrong in the
	// revoking direction leaks; being wrong in the widening direction merely
	// costs a reconnect. Keep it conservative unless the flag transition is
	// threaded in.
	CanRevokeVisibility bool

	// CarriesCustomTextTopology reports whether this mutation changes the
	// sender's Custom Status audience and therefore needs the durable #2419
	// topology rail. Custom Status is not republished on a heartbeat and carries
	// no TTL, so its staleness horizon is unbounded (durability amendment
	// §6.7.1, class C2) and the leg fails CLOSED under every posture.
	CarriesCustomTextTopology bool
}

// ErrFamilyUnregistered marks a Family with no registry entry. It fails the
// capture CLOSED rather than guessing a policy.
var ErrFamilyUnregistered = errors.New("presencecapture: mutation family is not registered")

// familyRegistry is the single declaration point for every family's policy. It
// is a package variable rather than a const map only because Go has no const
// maps; nothing outside this package may write it, and the registry_test
// swap-and-restore is the only mutation in the tree.
var familyRegistry = map[Family]FamilyPolicy{
	FamilyFriendshipAccept:       {CanRevokeVisibility: false, CarriesCustomTextTopology: true},
	FamilyFriendshipRemove:       {CanRevokeVisibility: true, CarriesCustomTextTopology: true},
	FamilyBlock:                  {CanRevokeVisibility: true, CarriesCustomTextTopology: true},
	FamilyFriendsOfFriendsToggle: {CanRevokeVisibility: true, CarriesCustomTextTopology: true},
}

// PolicyFor returns the registered policy for f, or ErrFamilyUnregistered.
func PolicyFor(f Family) (FamilyPolicy, error) {
	policy, registered := familyRegistry[f]
	if !registered {
		return FamilyPolicy{}, fmt.Errorf("%w: %d", ErrFamilyUnregistered, uint8(f))
	}
	return policy, nil
}

// UnregisteredFamilies returns every declared family with no registry entry, in
// ascending order. It is the boot guard's input and takes no arguments so the
// guard cannot be handed a stale snapshot of the enum.
func UnregisteredFamilies() []Family {
	var missing []Family
	for value := Family(0); value < familyCount; value++ {
		if _, registered := familyRegistry[value]; !registered {
			missing = append(missing, value)
		}
	}
	return missing
}

// CanRevokeVisibility reads the family's declared FamilyPolicy answer on that
// axis. The rationale for the answers — including why this is NOT the
// "direction branch" [internal]rules/backend.md forbids for the #2445 RBAC
// family, and why FamilyFriendsOfFriendsToggle is true for both directions —
// lives on FamilyPolicy.CanRevokeVisibility, which is where a new family must
// now state its answer. Keeping a second copy here would let the two drift.
//
// NOTHING IN services/control-plane CALLS THIS OUTSIDE TESTS. Every production
// consumer resolves PolicyFor once and reads the FamilyPolicy field: the
// accepted-edge gate and the peripheral seed inside graphpresence's
// CaptureInTx, plus planForBoundExceeded which it calls, all read
// policy.CanRevokeVisibility from the single value step 1 resolved. The remaining callers are this package's own
// capture_test.go and registry_test.go, and graphpresence's
// review_regression_test.go reads PolicyFor for its preconditions rather than
// this method. An earlier revision of this comment asserted the error branch
// below was reachable via graphpresence — it never was after that call chain
// resolved PolicyFor at step 1, and that claim was wrong when written.
//
// It is kept as an exported convenience on an exported enum: a future consumer
// of this leaf contract can reach it, and the answer it gives must be the safe
// one.
func (f Family) CanRevokeVisibility() bool {
	policy, err := PolicyFor(f)
	if err != nil {
		// Unknown family: fail closed. An uncleared viewer is a disclosure; an
		// unnecessary disconnect is a reconnect.
		//
		// A family DECLARED in the enum but missing from the registry is caught
		// twice over: by the boot guard (requireGraphPresenceFamilyRegistry in
		// internal/api/router.go, which refuses to start on a non-empty
		// UnregisteredFamilies) and, at request time, by CaptureInTx's own
		// PolicyFor at step 1.
		//
		// Only the BOOT GUARD is blind to a value cast from OUTSIDE the enum:
		// UnregisteredFamilies scans just [0, familyCount), so Family(99) never
		// appears in its gap list. PolicyFor is a plain registry lookup with no
		// range check, so it rejects that cast exactly as it rejects any
		// unregistered family. What would therefore reach THIS branch is a
		// future caller of the leaf contract that reads the bool directly
		// instead of resolving PolicyFor — not either of those two controls.
		// Nothing in this service does that today.
		return true
	}
	return policy.CanRevokeVisibility
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

// ErrCapturePending marks a capture refused because another operation already
// holds this sender's durable marker. THE WRITE DID NOT HAPPEN: the caller's
// transaction is rolled back and the request is retryable.
//
// It is a distinct sentinel rather than a 500 because the correct client
// behaviour differs: this one resolves on its own within
// presencehistory.pendingOperationGrace (30s) and carries a Retry-After, while
// a 500 means the request failed for a reason retrying will not fix.
var ErrCapturePending = errors.New("presencecapture: another presence operation is pending for this sender")

// ErrPostCommitDelivery marks a terminal whose transaction COMMITTED and whose
// presence delivery then failed.
//
// The mutation is durable and visible. Reporting it as a 500 would be a
// correctness lie that invites a duplicate-action retry against state that
// already changed, so a caller must surface it as a 503 whose body still
// describes the mutation as having happened.
var ErrPostCommitDelivery = errors.New("presencecapture: the mutation committed and presence delivery failed")

// PendingError carries the retry delay alongside ErrCapturePending.
//
// The delay crosses the package boundary as a plain time.Duration rather than
// as the producing package's error type, so consumers classify on this leaf and
// take NO internal/presencehistory import — the property the leaf exists for.
type PendingError struct {
	// After is the delay a caller should surface in Retry-After. Producers pass
	// a positive value; a zero or negative After means the producer could not
	// derive one and the consumer should substitute its own floor.
	After time.Duration
}

func (e *PendingError) Error() string { return ErrCapturePending.Error() }

// Unwrap makes errors.Is(err, ErrCapturePending) true for a *PendingError, so a
// caller that does not need the delay classifies with the sentinel alone.
func (e *PendingError) Unwrap() error { return ErrCapturePending }

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
// Every CAPTURE-READING method takes *sql.Tx. That is the enforcement
// mechanism, not a convention: there is no pre-transaction capture method, so
// the #2445 split is not expressible here. For this family the graph read IS
// the state the write destroys, so it must run under the same transaction as
// the write. WithGatedTx is not a counterexample — it reads nothing and opens
// the very transaction the capture then runs in.
type GraphPresenceCapture interface {
	// WithGatedTx acquires the subject's process-local sender gates, THEN opens
	// the transaction, then runs work under both, then releases the gates only
	// after work has returned — which is after work's own terminal (Complete or
	// Abandon) has run.
	//
	// It does NOT commit. work's Complete owns the commit, exactly as before.
	//
	// It exists because the durable topology rail's BeginTopologyBatch requires
	// the sender gates to be held from BEFORE BeginTx. Acquiring them after
	// BeginTx inverts the order and creates a gate-vs-row-lock cycle against
	// internal/users/presence_settings.go, which takes the same gates and then
	// the same users row. Handlers therefore surrender BeginTx to the hook,
	// which is also what keeps internal/friends free of any gate dependency.
	//
	// A transaction opened any other way must not reach CaptureInTx, and the
	// Subject handed HERE must be the same value handed to CaptureInTx inside
	// work. The gate set is derived from this Subject, while the durable
	// topology rail's marker set is derived from the one CaptureInTx receives:
	// gating on Subject{Principal: p} and then capturing with
	// Subject{Principal: p, Counterpart: c} writes a durable marker for c with
	// c's sender stripe ungated. Nothing detects that — it is not a compile
	// error, no test observes it, and nothing correlates the two calls today —
	// so resolve every Subject field BEFORE WithGatedTx rather than inside work
	// (#2446 PR-2 spec section 7 hoists ClaimFriendCode's counterpart lookup out
	// of the transaction for exactly this reason). Enforcement is deferred, not
	// impossible: while handlers still reach CaptureInTx with plain db.BeginTx
	// transactions, a correlation check would have to treat absence as allowed
	// and buy nothing yet.
	WithGatedTx(ctx context.Context, subject Subject, work func(tx *sql.Tx) error) error

	// CaptureInTx is IN-TRANSACTION and runs BEFORE the mutation write.
	//
	// Required call-site sequence, with NOTHING between the capture and the
	// write. The transaction comes from WithGatedTx, never from db.BeginTx:
	//
	//	err := capture.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
	//	    <authorization / precondition reads>
	//	    plan, err := capture.CaptureInTx(ctx, tx, subject)
	//	    <THE MUTATION WRITE>
	//	    return capture.Complete(ctx, tx, plan)   // commits; do NOT tx.Commit()
	//	})
	//
	// FIVE production call sites follow this sequence, all through the
	// presencehook wrapper rather than this interface directly:
	// internal/friends/handlers.go (accept, RemoveFriend, BlockUser and the
	// friend-code claim) and internal/users/handlers.go (the friends-of-friends
	// privacy toggle). None of them opens its own db.BeginTx any more —
	// WithGatedTx owns that, which is what lets the sender gates be acquired
	// BEFORE the transaction rather than inside it.
	//
	// A nil error with HasWork() == false is the ordinary empty terminal, and
	// with the durable leg wired it no longer implies "nothing was written":
	// a plan may carry topology markers while HasWork() is false. Handlers must
	// still not branch on HasWork() — it is for counters.
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
