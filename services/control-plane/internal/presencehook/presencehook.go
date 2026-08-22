// Package presencehook holds the handler-side plumbing every #2446
// graph-destroying write shares: the deferred rollback, the endpoint-ID parse,
// and the three capture terminals.
//
// It is deliberately NOT part of internal/presencecapture, but NOT for the
// reason an earlier draft of this comment gave. That draft said these helpers
// need pkg/logger and so "would break" the leaf's zero-internal-dependency
// guarantee by risking an import cycle. pkg/logger has zero internal
// dependencies of its own, so no cycle is possible either way (PR #2738 review).
//
// The actual reason is narrower and is a preference, not a constraint: keeping
// internal/presencecapture free of behaviour makes the contract auditable in
// one read — it is types and one interface, with nothing that can act. The
// plumbing lives here so it is still written once rather than once per consumer.
// Merging the two packages would cost that auditability and nothing else.
//
// Everything here is a FREE FUNCTION taking the capture as a parameter, so a
// consumer keeps only the field, its setter, and the boot-guard accessor.
package presencehook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// RollbackUnlessDone is the deferred rollback for hooked transactions.
// sql.ErrTxDone means the terminal already committed and is not an error — it
// is in fact the normal successful path, because Complete owns the commit.
//
// A genuine failure emits BOTH a fixed classification and the driver error,
// which is the shape this feature's other terminals already use:
// internal/friends/handlers.go and internal/users/handlers.go each have exactly
// ONE classified failure sink — respondPresenceTerminal — and both log the
// classification alongside the error it came from. (They name the field
// "error_class" where this one says "failure_class"; that divergence is
// recorded, not resolved here.) The constant is the stable grep target; the
// error is the cause. Neither substitutes for the other.
//
// Keeping the error is load-bearing HERE specifically, because this defer has
// no return value — the log is the only place a failed discard is ever
// recorded. Exactly ONE production call site reaches it: WithGatedTx's own
// unwired fallback below. A hooked handler lands on that fallback whenever it
// passes a nil capture, which is NOT only the unhooked replica.
//
// The set that nils it deliberately (the count was stale at "TWO of the five"
// and the five was itself stale — there are NINE call sites):
//   - internal/users, a privacy PATCH that never supplies dm_friends_of_friends
//   - internal/friends' ClaimFriendCode, a claim that does not auto-accept
//   - internal/friends' BlockUser, when its probe finds no accepted edge
//   - internal/members' execBanTx, when its probe finds the target is not a
//     member (a pre-emptive ban reconciles nothing)
//
// The last two arrived with #2854 stage C and change this fallback's character:
// it is no longer a degraded-replica path but a HOT one carrying real writes —
// stranger blocks and pre-emptive bans — on every replica. Its divergence from
// graphpresence's runInTx (this defer logs the driver error rather than joining
// it into a return value, because it has no return value) is therefore now
// load-bearing for ordinary production traffic, not just for an unhooked boot. A non-nil capture
// routes the call into graphpresence's runInTx instead, whose defer joins the
// cause into what it returns and so does not depend on its log. Dropping the
// error here leaves a rollback failure permanently undiagnosable.
func RollbackUnlessDone(tx *sql.Tx, log *logger.Logger) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		log.Error("presence-hooked transaction rollback failed",
			"failure_class", "gated_rollback", "error", err)
	}
}

// Spec is a handler's capture request: the two enum choices the call site
// declares plus the endpoint IDs in the string form the handlers already carry.
//
// The posture is a field rather than a default so every hooked site states its
// failure behaviour explicitly at the call site.
type Spec struct {
	Family  presencecapture.Family
	Posture presencecapture.FailPosture

	// PrincipalID is the user whose graph edges the write creates or destroys.
	PrincipalID string
	// CounterpartID is the other friendship or block endpoint. Empty for a
	// family that has none, such as the friends-of-friends toggle.
	CounterpartID string
}

// Subject parses the spec's endpoint IDs. A malformed ID fails CLOSED: capturing
// against uuid.Nil would drop that endpoint from the bridge's focal set silently
// and reconcile only half the mutation — which is also why this never uses
// uuid.MustParse, whose panic would take the handler down.
//
// An earlier version of this comment claimed uuid's parse errors "describe
// only length and shape, never the rejected value". That is FALSE: google/uuid
// v1.6.0 formats the 45-byte URN branch as `invalid urn prefix: %q` on s[:9],
// echoing the first nine bytes of input (PR #2738 review, CodeRabbit). The
// wrap is still safe — those bytes are the caller's own path parameter, and
// %q escapes control characters so the value cannot forge a log line
// ([internal]rules/observability.md) — but do not repeat the stronger claim.
func (s Spec) Subject() (presencecapture.Subject, error) {
	principal, err := uuid.Parse(s.PrincipalID)
	if err != nil {
		return presencecapture.Subject{}, fmt.Errorf("parse capture principal: %w", err)
	}

	counterpart := uuid.Nil
	if s.CounterpartID != "" {
		counterpart, err = uuid.Parse(s.CounterpartID)
		if err != nil {
			return presencecapture.Subject{}, fmt.Errorf("parse capture counterpart: %w", err)
		}
	}

	return presencecapture.Subject{
		Family:      s.Family,
		FailPosture: s.Posture,
		Principal:   principal,
		Counterpart: counterpart,
	}, nil
}

// Capture resolves the pre-mutation audience inside the caller's transaction. It
// is a no-op when the capture is unwired, so a replica without the hook behaves
// exactly as it did before #2446 and degrades to the pre-existing <=90s presence
// TTL.
//
// It parses the endpoint IDs itself so an unwired handler cannot fail on an ID
// it never had to parse before.
func Capture(
	ctx context.Context,
	capture presencecapture.GraphPresenceCapture,
	tx *sql.Tx,
	spec Spec,
) (presencecapture.Plan, error) {
	if capture == nil {
		return nil, nil
	}
	subject, err := spec.Subject()
	if err != nil {
		return nil, err
	}
	return capture.CaptureInTx(ctx, tx, subject)
}

// Complete commits tx. When the capture is unwired it commits directly, so the
// caller NEVER calls tx.Commit() itself on either path — which is what lets the
// durable rail (whose terminal owns the commit) be swapped in at the
// construction site alone.
func Complete(
	ctx context.Context,
	capture presencecapture.GraphPresenceCapture,
	tx *sql.Tx,
	plan presencecapture.Plan,
) error {
	if capture == nil {
		if err := tx.Commit(); err != nil {
			// Wrapped, not bare, so the caller can still errors.Is against
			// sql.ErrTxDone while getting the operation context backend.md
			// requires on every returned error.
			return fmt.Errorf("commit unhooked graph mutation: %w", err)
		}
		return nil
	}
	return capture.Complete(ctx, tx, plan)
}

// Abandon is the fail-closed terminal for a path that will not reach Complete.
// It never touches tx.
func Abandon(
	capture presencecapture.GraphPresenceCapture,
	plan presencecapture.Plan,
	cause presencecapture.Cause,
) {
	if capture != nil {
		capture.Abandon(plan, cause)
	}
}

// WithGatedTx opens the hooked transaction. It is the ONLY sanctioned way for a
// handler to begin a transaction that will reach Capture.
//
// Wired: it delegates to the capture, which acquires the process-local sender
// gates BEFORE opening the transaction. The durable topology rail's
// BeginTopologyBatch requires that order — acquiring the gates after BeginTx
// creates a gate-vs-row-lock cycle against internal/users/presence_settings.go,
// whose UpdatePresenceSettings takes the same gates first (through
// presencehistory.WithReadySenderModeBeforeReconcile) and only then opens its
// transaction and takes the same users row.
//
// Unwired: a plain db.BeginTx plus the deferred RollbackUnlessDone — the
// pre-#2446-PR-2 shape — so a replica without the hook behaves as it did
// before and degrades to the pre-existing <=90s presence TTL.
//
// It never commits on either path. work's Complete owns the commit.
//
// The endpoint IDs are parsed BEFORE anything is opened, so a malformed ID
// costs no transaction and no gate — and fails closed, because capturing
// against uuid.Nil would drop that endpoint from the focal set silently. The
// unwired path deliberately parses NOTHING, matching Capture: an unwired
// handler must not start failing on an ID it never had to parse before.
//
// TWO deliberate differences from the wired bridge, and the second follows from
// the first:
//
//   - graphpresence's runInTx JOINS a failed discard into the returned error
//     (internal/graphpresence/topology.go); this fallback does not. The join
//     would change the outcome for exactly one case — work returning nil
//     without having committed — which the Complete-owns-the-commit contract
//     excludes, so preserving the pre-existing shape costs nothing.
//   - RollbackUnlessDone therefore logs the driver error alongside the fixed
//     failure_class, while runInTx's defer logs the class alone. That defer
//     loses nothing by omitting it, because it just joined the same error into
//     what it returns; this one has no return value, so its log is the only
//     record of the cause.
func WithGatedTx(
	ctx context.Context,
	capture presencecapture.GraphPresenceCapture,
	db *sql.DB,
	log *logger.Logger,
	spec Spec,
	work func(tx *sql.Tx) error,
) error {
	if capture == nil {
		if db == nil {
			return errors.New("presencehook: unhooked gated transaction requires a database")
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin unhooked graph mutation: %w", err)
		}
		defer RollbackUnlessDone(tx, log)
		return work(tx)
	}
	subject, err := spec.Subject()
	if err != nil {
		return err
	}
	return capture.WithGatedTx(ctx, subject, work)
}

// HeaderRetryAfter is the response header RetryAfterHeader's value belongs in.
//
// Exported and homed here because two handler packages were each declaring a
// private copy, which is the drift this naming exists to prevent (CodeRabbit,
// PR #2840). The value and the header name now live in the same package.
const HeaderRetryAfter = "Retry-After"

// Failure is the HTTP shape a hooked handler returns for a terminal error.
//
// It carries no message: each handler keeps its own existing copy so no route
// shape changes. Code is for the structured log's fixed failure_class field and
// must stay a closed vocabulary — never an interpolated database error. The
// basis is [internal]rules/backend.md's #2446 bullet, which requires the cause
// vocabulary be a named type BECAUSE an untyped parameter invited a caller to
// pass an interpolated DB error into that field. observability.md carries no
// such rule; do not cite it here.
type Failure struct {
	Status     int
	Code       string
	RetryAfter time.Duration
}

// codePending is the one Failure.Code whose terminal resolves on its own. It is
// named rather than repeated so Classify and RetryAfterHeader cannot drift: the
// header must be emitted for exactly the terminal that self-resolves, and a
// second copy of the string is all it would take for one of them to stop
// meaning the other.
const codePending = "presence_operation_pending"

// codeDelivery is the post-commit arm: the mutation COMMITTED and only its
// presence delivery failed. Named for the same reason as codePending -- Classify
// and Body must not drift about which arm they mean.
const codeDelivery = "delivery"

// ErrProbeStale is the terminal for a pre-transaction probe that the
// authoritative in-transaction read contradicted, on a path that would
// otherwise perform an audience-destroying write with no capture.
//
// It is FAIL-CLOSED (#2854 stage C). Proceeding would DELETE a membership or
// flip a friendship to blocked while the counterpart keeps holding the
// principal's Custom Status -- which carries no TTL and is not republished on a
// heartbeat, so "indefinitely" is literal. That is CWE-284, the exact class the
// #2446 family exists to close.
//
// It carries NO presencecapture.Cause and must never reach Abandon: no plan was
// created on the ungated path, so asserting one existed would put a
// DisconnectRichPresenceClients call on a path that proved nothing happened --
// the defect PR #2738 removed from RemoveFriend.
//
// It must never be handled by re-entering WithGatedTx to "promote" the request.
// Acquiring a gate while the failed transaction still holds row locks
// reconstructs the channel-vs-row cycle no deadlock detector can break, because
// half of it is a Go channel. The retry belongs to the CLIENT, after the
// transaction has fully unwound.
var ErrProbeStale = errors.New("presencehook: probe disagreed with the authoritative read")

// codeProbeStale is the failure_class for ErrProbeStale.
//
// Deliberately NOT codePending: that value means another operation holds this
// sender's durable marker, which is false here, and collapsing the two would
// make a benign race indistinguishable from marker contention in logs and
// alerting.
const codeProbeStale = "probe_stale"

// probeStaleRetryAfter is a const, never configuration -- consistent with every
// other bound in this family, where making a latency bound configurable would
// let a deployment raise it instead of fixing what made the path slow.
const probeStaleRetryAfter = time.Second

// bodyPostCommitDelivery is what a post-commit terminal tells the client.
//
// It exists because the status code alone was not enough. ErrPostCommitDelivery's
// contract requires "a 503 whose body still describes the mutation as having
// happened", and reusing the site's failure message broke exactly that: a caller
// whose friend WAS removed read {"error":"Failed to remove friend"} and had every
// reason to retry an action that already succeeded -- the duplicate-action lie
// the sentinel exists to prevent, moved from the status line into the body
// (Gitar review, PR #2823).
//
// Deliberately generic across all five hooked sites. It says the two things a
// client can act on: the change is saved, and the visible catch-up is delayed.
const bodyPostCommitDelivery = "Your change was saved. Updating everyone who can see it is taking longer than usual."

// Classify maps a hooked-transaction error onto its HTTP shape.
//
//	ErrCapturePending      -> 503 presence_operation_pending + Retry-After, nothing written
//	ErrPostCommitDelivery  -> 503 delivery, the mutation IS durable
//	anything else          -> 500 internal, nothing proven, fail closed
//
// presencecapture.ErrCaptureBound is deliberately NOT mapped and lands on the
// 500 arm — but that arm's "nothing proven" does not describe it. Its only
// producer is checkFocalBound (internal/graphpresence/reconciler.go), which
// returns it exactly when len(focal) > maxFocalSenders, and both call sites
// fail CLOSED regardless of posture: graphpresence's WithGatedTx returns before
// any gate or transaction is taken, and CaptureInTx returns before its savepoint
// and before the handler's write, leaving the deferred rollback to discard the
// transaction. An oversized focal set is a defect in the focal-set derivation,
// not a retryable condition, so 500 is the intended shape — and a response body
// must not hedge that the mutation may have landed, because it provably did not.
//
// The ErrPostCommitDelivery arm is reachable in a running process as of the
// durable Custom Status leg. graphpresence.classifyTopologyCompletion
// (internal/graphpresence/topology.go) wraps it when the rail reports Committed
// with a delivery error, and internal/api/router.go now calls SetTopologyRail at
// the one site that builds the reconciler — so Reconciler.rail is non-nil on
// every boot that gets that far, capture.go's marker step sets Plan.hasTopology
// for any family whose policy carries the topology, and completeTopology runs.
// A deployed control-plane takes this path.
//
// The ordering matters: ErrCapturePending is checked first because it is ONE OF
// TWO terminals that both prove no write happened AND are self-resolving, so
// conflating it with a 500 would tell the client to give up on a request that
// would succeed 30 seconds later.
//
// ErrProbeStale is the second such terminal (#2854 stage C). A pre-transaction
// probe selected the ungated path, the authoritative in-transaction read
// disagreed, and the write was abandoned rather than performed without a
// capture. It is checked after the two capture terminals because it is the
// narrower condition -- it can only arise on a path that already chose to run
// ungated -- and its retry succeeds for a different reason than a pending
// marker's: the retry's probe reads the now-committed state and takes the gated
// path, rather than waiting for a marker to clear.
//
// Every Code is a literal here rather than anything derived from err, which is
// what keeps the handler's failure_class field a closed enum no matter what the
// database or the delivery path put in the error's text.
func Classify(err error) Failure {
	switch {
	case err == nil:
		return Failure{Status: http.StatusOK}
	case errors.Is(err, presencecapture.ErrCapturePending):
		return Failure{
			Status:     http.StatusServiceUnavailable,
			Code:       codePending,
			RetryAfter: RetryAfter(err),
		}
	case errors.Is(err, presencecapture.ErrPostCommitDelivery):
		return Failure{Status: http.StatusServiceUnavailable, Code: codeDelivery}
	case errors.Is(err, ErrProbeStale):
		return Failure{
			Status:     http.StatusServiceUnavailable,
			Code:       codeProbeStale,
			RetryAfter: probeStaleRetryAfter,
		}
	default:
		return Failure{Status: http.StatusInternalServerError, Code: "internal"}
	}
}

// RetryAfter surfaces the pending marker's remaining grace. It returns 0 for any
// other error, including a bare ErrCapturePending with no delay attached.
//
// The delay reaches a *PendingError through graphpresence.translateRailError,
// which has TWO branches and only one of them copies a producer's value: a
// presencehistory.ServiceError coded presence_operation_pending yields that
// struct's RetryAfter field, while ErrPendingOperationEligible substitutes
// graphpresence's own reconcileRetryAfter constant, because a marker whose
// grace already elapsed carries no producer field left to copy. That
// translation has a production caller — capture.go's marker step routes
// BeginTopologyBatch's error through it — and, like the post-commit arm above,
// it sits behind capture.go's r.rail != nil guard, which internal/api/router.go
// satisfies at boot. A deployed control-plane reaches it.
func RetryAfter(err error) time.Duration {
	var pending *presencecapture.PendingError
	if errors.As(err, &pending) && pending.After > 0 {
		return pending.After
	}
	return 0
}

// Body returns the response body for this class, given the call site's own
// failure message.
//
// Only the post-commit arm overrides it, and that override is the whole point:
// every other class describes a mutation that did NOT land, for which the site's
// "Failed to X" message is accurate. Returning siteFailure for the delivery arm
// would contradict this package's own documented contract.
func (f Failure) Body(siteFailure string) string {
	if f.Code == codeDelivery {
		return bodyPostCommitDelivery
	}
	return siteFailure
}

// RetryAfterHeader is the ONLY sanctioned way to derive a Retry-After header
// from a Failure. It yields a value for the pending terminal and ("", false)
// for every other one, so a handler writes:
//
//	if retry, ok := failure.RetryAfterHeader(); ok {
//		c.Header("Retry-After", retry)
//	}
//
// The gate IS the point, and it is why retryAfterSeconds is unexported. That
// conversion floors at 1, so applying it to whatever a Failure happens to carry
// stamps Retry-After: 1 onto the 500 arm as well — telling every client to
// re-drive a failure that does not resolve on its own, once per second, which
// is the inverse of the behaviour the 503-vs-500 split exists to produce
// (internal/presencecapture/capture.go, ErrCapturePending vs
// ErrPostCommitDelivery). Exactly TWO terminals both prove no write happened
// and clear themselves -- the pending marker and ErrProbeStale (#2854 stage C)
// -- so only those two may promise a retry. The gate is an allowlist for that
// reason: a new Code defaults to no header, which is the safe direction.
func (f Failure) RetryAfterHeader() (string, bool) {
	if f.Code != codePending && f.Code != codeProbeStale {
		return "", false
	}
	return strconv.FormatInt(retryAfterSeconds(f.RetryAfter), 10), true
}

// retryAfterSeconds converts a delay to the whole seconds an HTTP Retry-After
// header takes, rounding UP and flooring at 1.
//
// The floor is load-bearing: Retry-After has one-second granularity, so a
// sub-second delay truncates to 0, and 0 invites an immediate retry straight
// back into the marker that is still held. It is also why this is unexported:
// the floor makes a delay-less Failure look retryable, so the only caller may
// be RetryAfterHeader, which has already established that a retry is warranted.
func retryAfterSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return seconds
}

// RowQuerier is the single-row read both *sql.DB and *sql.Tx satisfy.
//
// It is declared HERE, at the consumer, matching the shape
// graphpresence.TopologyRail already uses, rather than importing a database
// abstraction from anywhere.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AcceptedEdgeExists reports whether an accepted friendship joins the two
// principals. It is the ONLY presence-visibility edge a #2446 friendship or
// block write can destroy.
//
// It lives here, and graphpresence delegates to it, because three callers need
// the same predicate against different handles: RemoveFriend's pooled probe,
// BlockUser's pooled probe, and BlockUser's in-transaction authoritative read
// (#2854 stage C).
//
// A shared CONSTANT was considered and rejected. It would deduplicate the SQL
// text while leaving each site to bind its own arguments and scan its own
// target — removing textual drift and leaving semantic drift, since this
// predicate is symmetric in (requester_id, addressee_id) and a swapped-argument
// copy is textually identical. Sharing the function removes both, because the
// binding is shared too.
//
// DIRECTION NOTE: graphpresence -> presencehook reads backwards for an
// implementation package importing handler plumbing. It is acyclic and
// deliberate — presencehook imports only stdlib, uuid, presencecapture and
// pkg/logger, and holds no handler types. Do NOT "fix" it by copying the body
// back into graphpresence; that reintroduces the duplication this removes.
//
// The caller chooses the handle and therefore the semantics: pass *sql.DB for a
// non-authoritative pooled probe, *sql.Tx for an authoritative in-transaction
// read. The query takes NO row lock in either case, which is load-bearing for
// the pooled form: no SET LOCAL lock_timeout applies on the pool, so a locking
// clause here could block indefinitely.
//
// An error is returned rather than folded into a false verdict, because every
// probe call site fails OPEN on error and can only do so if it can tell an
// error from a negative answer.
func AcceptedEdgeExists(
	ctx context.Context, q RowQuerier, principal, counterpart uuid.UUID,
) (bool, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2)
			    OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		)
	`, principal, counterpart).Scan(&exists); err != nil {
		return false, fmt.Errorf("read accepted friendship edge: %w", err)
	}
	return exists, nil
}
