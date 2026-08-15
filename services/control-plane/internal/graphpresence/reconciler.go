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
type Disconnector interface {
	DisconnectRichPresenceClients(ctx context.Context, recipients map[uuid.UUID]bool) error
	DisconnectAllRichPresenceClients(ctx context.Context) error
}

// Reconciler implements presencecapture.GraphPresenceCapture.
type Reconciler struct {
	db             *sql.DB
	activity       ActivityRefresher
	disconnector   Disconnector
	senderPresence presence.SenderPresenceResolver
	log            *logger.Logger
	sink           dispatchSink
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

// degradePlan builds the conservative superset: the principals themselves.
// Disconnecting them forces a rebuild from committed state, which post-commit
// no longer authorizes whatever they were holding. uuid.Nil is never a user.
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
// The context parameter is part of the presencecapture contract but is unused
// here: sql.Tx.Commit takes none, and dispatch is post-commit work that must
// outlive the request context rather than be cancelled with it.
func (r *Reconciler) Complete(
	_ context.Context, tx *sql.Tx, plan presencecapture.Plan,
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

	if err := tx.Commit(); err != nil {
		// An unresolved commit fails closed: we cannot prove the write landed,
		// so we disconnect whatever the capture held rather than assume either
		// outcome.
		r.Abandon(p, presencecapture.CauseCommitUnresolved)
		return fmt.Errorf("commit graph mutation: %w", err)
	}
	if p.HasWork() {
		r.sink.Enqueue(p)
	}
	return nil
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
		r.abandonPlan(p, p.cause.String())
		return
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
			r.disconnect(leg.captured, "sender_not_current")
		default:
			// Unresolved for this sender only. Fail closed at leg scope and
			// keep reconciling the others.
			r.disconnect(leg.captured, "refresh_failed")
		}
	}

	r.disconnect(p.viewers, "peripheral_disconnect")
}

// refreshLeg bounds one leg's post-commit refresh. The sender gate this enters
// is shared with the voice-lifecycle writers and does stall under load, so an
// unbounded call here can wedge the single dispatch worker permanently.
func (r *Reconciler) refreshLeg(leg activeLeg) error {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()
	return r.activity.RefreshServerVoiceRecheck(ctx, leg.senderID, leg.scope, leg.captured)
}
