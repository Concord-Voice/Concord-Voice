package graphpresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// dispatchTimeout bounds one post-commit refresh or disconnect, mirroring
// voicepresence's presenceRecheckTimeout. Dispatch runs on a single sequential
// worker, so an unbounded call does not merely delay one plan — it wedges the
// queue, and every plan that then fails to enqueue takes the queue_full
// abandon terminal.
const dispatchTimeout = 10 * time.Second

// ActivityRefresher is the in-memory active-category delivery leg. Declared
// structurally so this package depends on behaviour, not on *presence.ActivityService.
type ActivityRefresher interface {
	RefreshServerVoiceRecheck(
		ctx context.Context, senderID uuid.UUID, scope presence.Scope,
		recheckViewers map[uuid.UUID]bool,
	) error
}

// Disconnector is the fail-closed sink, implemented by *websocket.Hub.
//
// BeginAudienceRevocation sits on this interface rather than on a settable
// field for two reasons. The Hub is ALREADY here — router.go passes it to New
// as the disconnector — so there is nothing new to wire and no second
// construction site to forget. And widening the interface breaks every fake at
// COMPILE time, which forces an explicit answer at each one; a nil-able setter
// is silently forgettable, and a fence nobody wired is a fence that does not
// exist (#2992).
type Disconnector interface {
	DisconnectRichPresenceClients(ctx context.Context, recipients map[uuid.UUID]bool) error
	DisconnectAllRichPresenceClients(ctx context.Context) error

	// BeginAudienceRevocation opens a revocation bracket and returns its closer.
	// The bracket must be held across the whole revoking transaction.
	BeginAudienceRevocation() func()
}

// Reconciler implements presencecapture.GraphPresenceCapture.
type Reconciler struct {
	db             *sql.DB
	activity       ActivityRefresher
	disconnector   Disconnector
	senderPresence presence.SenderPresenceResolver
	log            *logger.Logger
	sink           dispatchSink

	// rail is the durable Custom Status (C2) leg, declared in topology.go. It
	// is a setter-wired field rather than a New parameter so the construction
	// site adds one line and no existing caller or test changes shape;
	// HasTopologyRail is what lets a boot guard turn a forgotten
	// SetTopologyRail into a startup failure instead of a silent
	// in-memory-only replica.
	rail TopologyRail
}

// The presencecapture.GraphPresenceCapture assertion lives in capture.go, next
// to CaptureInTx — the method that completes the implementation.

// New builds the reconciler and starts its dispatch worker.
func New(
	db *sql.DB,
	activity ActivityRefresher,
	disconnector Disconnector,
	senderPresence presence.SenderPresenceResolver,
	log *logger.Logger,
) *Reconciler {
	r := &Reconciler{
		db: db, activity: activity, disconnector: disconnector,
		senderPresence: senderPresence, log: log,
	}
	r.sink = newInMemorySink(r.dispatch, r.abandonPlan, log)
	return r
}

// Close stops the dispatch worker. Queued plans are abandoned fail-closed.
func (r *Reconciler) Close() {
	if r != nil && r.sink != nil {
		r.sink.Close()
	}
}

// checkFocalBound fails CLOSED regardless of posture. Exceeding maxFocalSenders
// means the focal-set derivation is wrong — a bug, not load — so degrading past
// it would hide the defect behind a conservative disconnect.
func (r *Reconciler) checkFocalBound(focal []uuid.UUID) error {
	if len(focal) > maxFocalSenders {
		return fmt.Errorf("%w: %d focal senders exceeds %d",
			presencecapture.ErrCaptureBound, len(focal), maxFocalSenders)
	}
	return nil
}

// degradePlan builds the PRINCIPALS-ONLY fallback. Disconnecting them forces a
// rebuild from committed state, which post-commit no longer authorizes whatever
// they were holding. uuid.Nil is never a user.
//
// It is NOT a superset of the exact plan, and three comments used to call it
// one (PR #2738 review, @code-reviewer). The stale state this family exists to
// clear is held by THIRD PARTIES — users who saw a sender only through the
// mutated edge. Those live in leg.captured, which a degraded plan discards; they
// are never disconnected and never rechecked, and fall back to the pre-#2446
// baseline: the ≤90 s presence TTL.
//
// The residual is widest for FamilyFriendsOfFriendsToggle, where Counterpart is
// uuid.Nil, so the degraded set is the single principal and clears none of the
// population that just lost access. Do not restate this as complete coverage —
// #2447/#2448 implementers will build on whatever this says.
func (r *Reconciler) degradePlan(
	subject presencecapture.Subject, cause degradeCause,
) *Plan {
	viewers := make(map[uuid.UUID]bool, 2)
	if subject.Principal != uuid.Nil {
		viewers[subject.Principal] = true
	}
	if subject.Counterpart != uuid.Nil {
		viewers[subject.Counterpart] = true
	}
	if r.log != nil {
		// Fixed enum only — never the underlying error, never any user ID.
		r.log.Error("graph presence capture degraded",
			"failure_class", cause.String(),
			"family", uint8(subject.Family),
			"viewer_count", len(viewers),
		)
	}
	return &Plan{subject: subject, degraded: true, cause: cause, viewers: viewers}
}

// Complete commits tx, then dispatches. It is the last statement that may touch
// tx: handlers must not call tx.Commit() themselves.
//
// The context is THREADED, not discarded. It arrived as `_ context.Context`
// under a comment calling it unused, which was true while the only terminal was
// a bare sql.Tx.Commit — that takes no context, and the post-commit in-memory
// dispatch must outlive the request rather than be cancelled with it. The
// durable leg changed that: CompleteTopologyBatchWithOutcome reads and delivers
// under a context, refuses a nil one outright
// (presencehistory.validateTopologyBatchCompletion), and detaches it from
// request cancellation itself via context.WithoutCancel before the commit and
// its classification run. Passing the request context through is therefore both
// required and safe.
func (r *Reconciler) Complete(
	ctx context.Context, tx *sql.Tx, plan presencecapture.Plan,
) error {
	if tx == nil {
		return errors.New("graphpresence: Complete requires a transaction")
	}
	p, ok := plan.(*Plan)
	if !ok && plan != nil {
		// A Plan this bridge did not build. Committing while silently dropping
		// the dispatch would leave viewers holding revoked state with no signal
		// at all, so refuse rather than guess (PR #2738 review, CodeRabbit).
		return errors.New("graphpresence: Complete received a foreign presencecapture.Plan")
	}

	if p != nil && p.hasTopology {
		if r.rail == nil {
			// Unreachable today — hasTopology is only set under capture.go's own
			// r.rail != nil guard — and it fails CLOSED anyway, because the one
			// alternative is worse than a 500. Falling through to the bare
			// commit below would land the markers and never call
			// CompleteTopologyBatchWithOutcome, which is invariant TB-1's
			// failure mode exactly: the batch dropped after BeginTopologyBatch
			// ran, leaving the sender's Custom Status suppressed for every
			// reconnecting viewer until the pending-operation grace expires.
			// A rail swapped out between capture and completion is a wiring
			// bug, not a posture.
			return errors.New(
				"graphpresence: Complete cannot resolve a topology batch without a rail")
		}
		// The durable rail owns the COMMIT for this transaction, and with it
		// every terminal that follows — including the C1 enqueue, because a
		// topology batch reconciles Custom Status only and says nothing about
		// Server Voice.
		return r.completeTopology(ctx, tx, p)
	}

	if err := tx.Commit(); err != nil {
		// An unresolved commit fails closed: we cannot prove the write landed,
		// so we disconnect whatever the capture held rather than assume either
		// outcome.
		r.Abandon(p, presencecapture.CauseCommitUnresolved)
		return fmt.Errorf("commit graph mutation: %w", err)
	}
	r.enqueue(p)
	return nil
}

// enqueue hands a committed plan to the in-memory C1 sink when it has work.
// HasWork() is false for a plan that carries only topology markers, which is
// correct: the C2 leg was already delivered synchronously by the rail.
func (r *Reconciler) enqueue(p *Plan) {
	if p.HasWork() {
		r.sink.Enqueue(p)
	}
}

// Abandon is the fail-closed terminal. It never touches tx.
//
// A cause that PROVES the transaction never committed is a no-op (#2738
// security fix): the handler's deferred rollback discards the write, so no
// viewer's authorization changed and nothing stale exists to clear. Without
// this split, a caller who aborts the request mid-flight converts one HTTP
// request into a full websocket teardown of every user in the captured
// audience — a set derived from an attacker-supplied path parameter.
//
// Unknown outcomes (CauseCommitUnresolved) still fan out, and the post-commit
// terminals in dispatch call abandonPlan directly and are unaffected.
func (r *Reconciler) Abandon(plan presencecapture.Plan, cause presencecapture.Cause) {
	if presencecapture.CauseProvesNoCommit(cause) {
		return
	}
	p, ok := plan.(*Plan)
	if !ok && plan != nil {
		if r.log != nil {
			r.log.Error("graph presence abandon received a foreign plan",
				"failure_class", "foreign_plan", "cause", string(cause))
		}
		// Escalate rather than return. Abandon is the fail-CLOSED terminal for a
		// cause that does not prove the write was discarded, so a plan we cannot
		// read is exactly the case where we do not know who to clear — and
		// returning silently chose the open direction in the one place that
		// exists to choose the closed one. Complete already refuses a foreign
		// plan (PR #2738 review, @security-reviewer).
		if r.disconnector != nil {
			ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
			defer cancel()
			if err := r.disconnector.DisconnectAllRichPresenceClients(ctx); err != nil && r.log != nil {
				r.log.Error("graph presence foreign-plan escalation failed",
					"failure_class", "foreign_plan")
			}
		}
		return
	}
	r.abandonPlan(p, string(cause))
}

func (r *Reconciler) abandonPlan(p *Plan, cause string) {
	if p == nil {
		return
	}
	r.disconnect(p.capturedAudience(), cause)
}

// disconnect is the single fail-closed sink, mirroring
// voicepresence.Executor.disconnect.
//
// It bounds the call (an unbounded post-commit disconnect can wedge the single
// sequential dispatch worker, and a wedged worker fills the queue until every
// later plan takes the queue_full terminal) and ESCALATES to a global
// disconnect when the targeted one fails. Logging the failure and returning —
// which this did before the PR #2738 review — made the fail-closed terminal
// fail OPEN: the recipients kept holding presence the committed graph no longer
// authorizes, and DisconnectAllRichPresenceClients sat on the interface
// declared and never called.
func (r *Reconciler) disconnect(recipients map[uuid.UUID]bool, cause string) {
	if len(recipients) == 0 || r.disconnector == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()
	err := r.disconnector.DisconnectRichPresenceClients(ctx, recipients)
	if err == nil {
		return
	}

	// A bare context error is NOT a failed disconnect. Hub.DisconnectRichPresenceClients
	// returns errors.Join(perClientErr, ctx.Err()) and never checks ctx inside its
	// loop, so every recipient is processed even when the deadline elapses
	// mid-loop. Escalating here disconnected every client on the node precisely
	// when the node was already slow — and they all reconnect at once, feeding
	// the load that caused the timeout (PR #2738 review, @code-reviewer).
	//
	// The already-closed half of this is fixed at the source: disconnectPrivacyCriticalClient
	// no longer reports an already-gone socket as an error, because the
	// recipient WAS reached.
	if onlyContextError(err) {
		if r.log != nil {
			r.log.Warn("graph presence disconnect exceeded its deadline",
				"failure_class", cause, "recipient_count", len(recipients))
		}
		return
	}

	if r.log != nil {
		r.log.Error("graph presence disconnect failed",
			"failure_class", cause, "recipient_count", len(recipients))
	}

	// The escalation gets its OWN context. Sharing the targeted call's context
	// made the fail-closed terminal fail OPEN in the exact case it exists for:
	// when the targeted disconnect fails BECAUSE it hit dispatchTimeout — the
	// stall-under-load the timeout is there to bound — that context is already
	// deadline-exceeded, so the global escalation returned instantly with a
	// context error and merely logged, leaving recipients holding presence the
	// committed graph no longer authorizes (PR #2738 review, Gitar).
	escalationCtx, escalationCancel := context.WithTimeout(
		context.Background(), dispatchTimeout)
	defer escalationCancel()
	if r.disconnector.DisconnectAllRichPresenceClients(escalationCtx) != nil && r.log != nil {
		r.log.Error("graph presence global disconnect failed",
			"failure_class", "disconnect_all", "cause", cause)
	}
}

// dispatch delivers a committed plan. A degraded plan has no exact legs, so it
// resolves to the conservative disconnect its viewer set already describes.
//
// Failure handling is PER LEG, matching voicepresence.Executor.run. An earlier
// version aborted the whole loop on any leg error and disconnected
// capturedAudience() — the union of every leg plus the peripheral viewers —
// which was wrong twice over: it skipped the remaining principal's
// reconciliation entirely, and it tore down sessions for senders whose own legs
// had already succeeded. Worse, the common trigger is routine: capture happens
// at write time and dispatch is post-commit, so a principal who simply left the
// voice channel in between yields ErrRecheckSenderNotCurrent on an ordinary
// request (PR #2738 review).
func (r *Reconciler) dispatch(p *Plan) {
	if p == nil {
		return
	}

	if p.degraded || len(p.active) == 0 {
		// A non-degraded plan with no legs is the ORDINARY peripheral case —
		// every FoF toggle, and every removal or block where neither party is
		// in voice. Logging it as the plan's cause emitted failure_class=none
		// for the same operation the exact path logs as peripheral_disconnect
		// (PR #2738 review, @code-reviewer).
		cause := p.cause.String()
		if !p.degraded {
			cause = "peripheral_disconnect"
		}
		r.abandonPlan(p, cause)
		return
	}

	// Disconnect each recipient at most once per dispatch. leg.captured and
	// p.viewers overlap whenever the principals share a server, so the
	// unconditional peripheral call below re-closed a socket the leg loop had
	// just closed. That double-close is what manufactured the error that used
	// to escalate to a whole-node teardown (PR #2738 review, @security-reviewer);
	// the escalation is now guarded on both sides, and not asking twice is the
	// cheaper half of the fix.
	disconnected := make(map[uuid.UUID]bool)
	disconnectOnce := func(recipients map[uuid.UUID]bool, cause string) {
		pending := make(map[uuid.UUID]bool, len(recipients))
		for id, included := range recipients {
			if !included || disconnected[id] {
				continue
			}
			pending[id] = true
			disconnected[id] = true
		}
		r.disconnect(pending, cause)
	}

	for i := range p.active {
		leg := p.active[i]
		err := r.refreshLeg(leg)
		switch {
		case err == nil:
			continue
		case errors.Is(err, presence.ErrRecheckSenderNotCurrent):
			// Documented benign per-sender terminal: the caller disconnects
			// only THAT sender's captured viewers. Not a silent skip — the
			// sender's own leave path computes the post-mutation audience and
			// would never clear the revoked viewer.
			disconnectOnce(leg.captured, "sender_not_current")
		default:
			// Unresolved for this sender only. Fail closed at leg scope and
			// keep reconciling the others.
			disconnectOnce(leg.captured, "refresh_failed")
		}
	}

	disconnectOnce(p.viewers, "peripheral_disconnect")
}

// onlyContextError reports whether EVERY leaf of err is a context deadline or
// cancellation. errors.Join produces a tree, so this walks it rather than using
// errors.Is, which answers "any leaf matches" — the wrong question here. One
// real per-client failure alongside a deadline must still escalate.
func onlyContextError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		leaves := joined.Unwrap()
		if len(leaves) == 0 {
			return false
		}
		for _, leaf := range leaves {
			if !onlyContextError(leaf) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// refreshLeg bounds one leg's post-commit refresh. The sender gate this enters
// is shared with the voice-lifecycle writers and does stall under load, so an
// unbounded call here can wedge the single dispatch worker permanently.
//
// The nil guard mirrors disconnect's r.disconnector == nil check. It is API
// hygiene rather than a crash fix — router.go passes a concrete non-nil
// *presence.ActivityService, and even a typed-nil pointer is rejected by
// validateActivityServiceCall before any dereference — but the asymmetry was
// real, and the sink worker has no recover(), so a panic here would take down
// the control plane rather than one plan (PR #2738 review, @code-reviewer).
func (r *Reconciler) refreshLeg(leg activeLeg) error {
	if r.activity == nil {
		return errors.New("graphpresence: activity refresher is not wired")
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()
	return r.activity.RefreshServerVoiceRecheck(ctx, leg.senderID, leg.scope, leg.captured)
}
