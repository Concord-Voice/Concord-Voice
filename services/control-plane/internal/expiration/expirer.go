package expiration

import (
	"context"
	"errors"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// pinnedExpiryTimeout bounds the synchronous deletion of due pinned messages
// after a context stops keeping pins (#3459 §8, X15). It is a constant, never
// configuration: the PATCH that triggers it must still answer inside the
// control plane's 15-second write deadline, and the sweeper deletes anything
// it leaves within one pass.
const pinnedExpiryTimeout = 10 * time.Second

// pinnedExpiryNotifyTimeout bounds the purge announcement after a deletion. It
// is separate from pinnedExpiryTimeout so a batch that used its whole budget
// still announces the rows it did delete.
const pinnedExpiryNotifyTimeout = time.Second

// ResponseBudget is how long a policy PATCH may take, measured from the start
// of its handler, before the work after its commit must stop and answer. The
// control plane's http.Server has a 15-second WriteTimeout; a handler that is
// still working when it passes has committed the policy and deleted pins, and
// the client never learns it (#3459 Codex review). Three seconds are left for
// writing the response. The handlers put start + ResponseBudget on the context
// after their commit; the deletion of due pins and the backfill both read it.
const ResponseBudget = 12 * time.Second

// The two failure classes of pinned expiry (#3459 §8, C-OBS). The log line
// carries one of these and two counts; never an id, a scope, or a direction.
const (
	failureClassPinnedExpiryRunFailed = "pinned_expiry_run_failed"
	failureClassPinnedExpiryDeadline  = "pinned_expiry_deadline"
)

// ExpiryBatchRunner is the restricted expiry terminal the expirer drives.
// *purge.Engine satisfies it.
type ExpiryBatchRunner interface {
	RunExpiryBatch(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error)
}

// PurgeNotifier announces an expiry deletion to connected clients: a
// server_purged frame to a channel's server, a dm_purged frame to a
// conversation's participants. The sweeper's two notification sinks have the
// same shape, so one implementation can serve both.
type PurgeNotifier interface {
	EmitServerPurged(ctx context.Context, serverID string, cutoff time.Time)
	EmitDMPurged(ctx context.Context, conversationID string, cutoff time.Time)
}

// ContextExpirer deletes the pinned messages that were already due when a
// context stopped keeping pins (Transition.DuePinnedIDs), before the policy
// PATCH answers, so turning "Include pinned messages" on deletes them "right
// away" rather than at the next sweep.
//
// It is best-effort by design. The policy has already committed when it runs,
// so a failure changes nothing about the response: the rows are still due and
// the sweeper deletes them within one pass. The purge engine rechecks every
// candidate under a row lock against the accepted cutoff, so a racing unpin or
// toggle-off can neither rescue a due row (C-NORES) nor widen the deletion.
type ContextExpirer struct {
	runner   ExpiryBatchRunner
	notifier PurgeNotifier
	// shutdown closes when the process shuts down; nil means it never does.
	shutdown <-chan struct{}
	log      *logger.Logger
	timeout  time.Duration
}

// NewContextExpirer validates the expirer's dependencies. lifetime is the
// process's lifecycle context (NewRouter's lifecycleCtx): a run it cancels is
// shutdown, not failure, and is not logged. A nil lifetime means the process
// never shuts the expirer down.
func NewContextExpirer(lifetime context.Context, runner ExpiryBatchRunner, notifier PurgeNotifier, log *logger.Logger) (*ContextExpirer, error) {
	if runner == nil {
		return nil, errors.New("pinned expiry requires the expiry purge terminal")
	}
	if notifier == nil {
		return nil, errors.New("pinned expiry requires a purge notifier")
	}
	if log == nil {
		return nil, errors.New("pinned expiry requires a logger")
	}
	var shutdown <-chan struct{}
	if lifetime != nil {
		shutdown = lifetime.Done()
	}
	return &ContextExpirer{runner: runner, notifier: notifier, shutdown: shutdown, log: log, timeout: pinnedExpiryTimeout}, nil
}

// ExpireChannelPins deletes a channel transition's due pinned messages and, if
// any were deleted, announces server_purged to the channel's server. It
// returns how many rows it deleted. Its failures are already logged under a
// fixed class; the count is for the caller's own bookkeeping, never a log.
func (e *ContextExpirer) ExpireChannelPins(ctx context.Context, channelID, serverID string, transition Transition) int {
	return e.expire(ctx, purge.ExpiryPlan{
		ContextType: purge.ContextChannel, ContextID: channelID, ServerID: &serverID,
	}, transition, func(notifyCtx context.Context) {
		e.notifier.EmitServerPurged(notifyCtx, serverID, *transition.Current.UpdatedAt)
	})
}

// ExpireConversationPins is ExpireChannelPins for a DM or group conversation,
// announcing dm_purged to its participants.
func (e *ContextExpirer) ExpireConversationPins(ctx context.Context, conversationID string, isGroup bool, transition Transition) int {
	contextType := purge.ContextDM
	if isGroup {
		contextType = purge.ContextGroup
	}
	return e.expire(ctx, purge.ExpiryPlan{ContextType: contextType, ContextID: conversationID}, transition, func(notifyCtx context.Context) {
		e.notifier.EmitDMPurged(notifyCtx, conversationID, *transition.Current.UpdatedAt)
	})
}

// expire runs restricted expiry batches over the transition's due ids at the
// transition's accepted cutoff until every due id is gone. The cutoff is the C
// the reconcile collected against, so the engine's `expires_at < C` recheck
// deletes exactly the rows the collect saw as due and nothing a later stamp made
// due. One batch deletes at most the engine's stride (PURGE_MAX_BATCH), which
// can be smaller than the due set, so a single call would leave the rest to the
// periodic sweep while the PATCH reported success (#3459 Codex review). A batch
// that deletes nothing ends the loop: the rest are no longer due or no longer
// there. Every batch shares one deadline, and one announcement covers them all.
func (e *ContextExpirer) expire(ctx context.Context, plan purge.ExpiryPlan, transition Transition, notify func(context.Context)) int {
	if e == nil || len(transition.DuePinnedIDs) == 0 || transition.Current.UpdatedAt == nil {
		return 0
	}
	plan.CandidateIDs = transition.DuePinnedIDs
	plan.ExpiresBefore = *transition.Current.UpdatedAt

	budget := e.runBudget(ctx)
	if budget <= 0 {
		// Nothing is left of the caller's response deadline: answering beats
		// deleting, and the sweeper deletes these rows within one pass.
		e.reportSkipped(len(plan.CandidateIDs))
		return 0
	}
	deleted := 0
	e.detached(ctx, budget, func(runCtx context.Context) {
		var err error
		for deleted < len(plan.CandidateIDs) {
			var result purge.Result
			result, err = e.runner.RunExpiryBatch(runCtx, plan)
			deleted += result.DeletedCount
			if err != nil || result.DeletedCount == 0 {
				break
			}
		}
		if err != nil {
			e.reportFailure(runCtx, err, len(plan.CandidateIDs), deleted)
		}
	})
	if deleted > 0 {
		e.detached(ctx, pinnedExpiryNotifyTimeout, notify)
	}
	return deleted
}

// runBudget is the deletion's time limit: its own timeout, cut to what is left
// of the caller's deadline once the announcement's second is set aside. The
// deadline is read here because detached drops it along with cancellation.
func (e *ContextExpirer) runBudget(ctx context.Context) time.Duration {
	budget := e.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline) - pinnedExpiryNotifyTimeout; left < budget {
			budget = left
		}
	}
	return budget
}

// reportSkipped writes the deadline line for a run that had no time to start.
func (e *ContextExpirer) reportSkipped(due int) {
	if e.shuttingDown() {
		return
	}
	e.log.Warn("Pinned expiry did not complete", "failure_class", failureClassPinnedExpiryDeadline, "due", due, "deleted", 0)
}

// detached runs fn with a context that keeps parent's values but not its
// cancellation, so a client that hangs up after the policy committed cannot
// cancel the deletion, and that ends at timeout or when the process shuts
// down, whichever comes first.
func (e *ContextExpirer) detached(parent context.Context, timeout time.Duration, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-e.shutdown:
			cancel()
		case <-done:
		}
	}()
	fn(ctx)
}

// shuttingDown reports whether the process has begun shutting down.
func (e *ContextExpirer) shuttingDown() bool {
	select {
	case <-e.shutdown:
		return true
	default:
		return false
	}
}

// reportFailure writes the one failure line. A run cut short by process
// shutdown is not a failure and writes nothing. The line carries no error
// text: a driver error can echo a parameter value, which here is a message id.
func (e *ContextExpirer) reportFailure(runCtx context.Context, err error, due, deleted int) {
	if e.shuttingDown() {
		return
	}
	class := failureClassPinnedExpiryRunFailed
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		class = failureClassPinnedExpiryDeadline
	}
	e.log.Warn("Pinned expiry did not complete", "failure_class", class, "due", due, "deleted", deleted)
}
