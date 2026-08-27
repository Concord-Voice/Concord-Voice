package media

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// DefaultSessionSweepInterval is the cadence for the abandoned-session sweep.
	// Hourly is deliberate: the work queue is one ListMultipartUploads call, the
	// bytes it reclaims are not urgent, and a session cannot be swept before it
	// is provably dead (see sessionSweepMinAge), so a shorter interval buys
	// nothing but object-store requests.
	DefaultSessionSweepInterval = 1 * time.Hour

	// sessionSweepGrace is the margin past the hard TTL before an upload becomes
	// sweepable.
	sessionSweepGrace = 30 * time.Minute

	// attachmentKeyPrefix scopes the sweep. ListMultipartUploads returns every
	// incomplete upload in the bucket, and aborting one that belongs to another
	// code path would destroy a stranger's in-flight write. Only the session
	// path creates multipart uploads today, and this keeps that from becoming a
	// silent dependency: a future multipart producer under a different prefix is
	// simply not swept, and the bucket lifecycle rule still backstops it.
	attachmentKeyPrefix = "attachments/"
)

// sessionSweepMinAge is how old an incomplete upload must be before the sweeper
// will abort it.
//
// DERIVED from uploadSessionHardTTL rather than written as a literal, and with a
// grace margin on top. Past the hard TTL every route answers 410, so nothing
// legally alive can be older -- but a request that entered the handler just
// before the TTL lapsed is still running, and aborting its multipart upload
// underneath it would turn a completing upload into a corrupt one. The margin
// buys that request time to finish or fail on its own.
func sessionSweepMinAge() time.Duration {
	return uploadSessionHardTTL + sessionSweepGrace
}

// sweepStore is the object-store surface the sweeper needs. Narrow on purpose:
// it makes the test double trivial and makes it obvious that the sweeper reads
// nothing from Redis.
type sweepStore interface {
	ListIncompleteUploads(ctx context.Context, olderThan time.Time) ([]storage.IncompleteUpload, error)
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
}

// SessionSweeper reclaims object-store bytes staged by attachment upload
// sessions that were never committed or cancelled.
//
// IT READS ITS WORK QUEUE FROM THE OBJECT STORE, NOT FROM REDIS. That is the
// whole point and it is not an implementation detail: a Redis-derived sweeper
// fails exactly when Redis does, which is precisely when sessions get orphaned
// -- the session record is the thing that went missing, so a sweep driven by it
// has nothing to iterate. Deriving the queue from ListMultipartUploads means a
// total Redis loss cannot strand bytes, which is what lets the client's
// cancel/unmount DELETE be genuinely best-effort. Those paths are an
// optimisation; this is the correctness guarantee behind them.
type SessionSweeper struct {
	store  sweepStore
	log    *logger.Logger
	minAge time.Duration
}

// NewSessionSweeper builds a sweeper over the given object store.
func NewSessionSweeper(store sweepStore, log *logger.Logger) *SessionSweeper {
	return &SessionSweeper{store: store, log: log, minAge: sessionSweepMinAge()}
}

// SweepResult reports what a sweep attempted, not only what it achieved.
// Counting successes alone let a TOTALLY BROKEN sweep report as a clean one:
// lose s3:AbortMultipartUpload (an IAM edit, a read-only token rotation) and
// every abort fails, swept == 0, err == nil, and the worker's `n > 0` guard
// prints nothing at all. The bucket then fills without bound while the one
// component main.go calls "LOAD-BEARING FOR CORRECTNESS" reports success --
// and every best-effort cleanup path in this feature defers to it, so they all
// become silently wrong at once.
type SweepResult struct {
	Swept     int
	Attempted int
	Failed    int
}

// SweepAbandoned aborts every incomplete attachment upload older than the
// minimum age and reports what it attempted.
//
// One failed abort never stops the sweep: the next upload in the list is
// unrelated to it, and giving up on the batch would let a single permanently
// broken key block every later reclaim forever. A batch in which EVERY abort
// failed is a different thing and returns an error.
func (s *SessionSweeper) SweepAbandoned(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	cutoff := time.Now().Add(-s.minAge)
	uploads, err := s.store.ListIncompleteUploads(ctx, cutoff)
	if err != nil {
		return res, err
	}

	for _, u := range uploads {
		if !strings.HasPrefix(u.Key, attachmentKeyPrefix) {
			continue
		}
		res.Attempted++
		if err := s.store.AbortMultipartUpload(ctx, u.Key, u.UploadID); err != nil {
			// storage_key is not a secret; the SESSION ID is the bearer
			// capability and it is deliberately absent here -- the sweeper never
			// reads one, and could not log one if it wanted to.
			s.log.Warn("attachment session sweep: abort failed",
				"error", err, "storage_key", u.Key)
			res.Failed++
			continue
		}
		res.Swept++
	}

	// A batch in which EVERY abort failed is not a successful sweep and must not
	// report as one. Continuing past a single bad key is right; reporting the
	// aggregate as clean is what hid a dead reclaim path.
	if res.Attempted > 0 && res.Swept == 0 && res.Failed == res.Attempted {
		return res, fmt.Errorf(
			"every abort failed (%d attempted); object-store reclaim is not working", res.Failed)
	}
	return res, nil
}

// StartSessionSweepWorker runs the sweep once at startup -- catching sessions
// abandoned while the process was down, which is the case a ticker alone never
// covers -- and then on a fixed interval. It stops when ctx is cancelled.
// Mirrors subscriptions.StartExpirySweepWorker, the established background-job
// pattern; failures are logged, never fatal.
func StartSessionSweepWorker(
	ctx context.Context,
	store sweepStore,
	log *logger.Logger,
	interval time.Duration,
) {
	sweeper := NewSessionSweeper(store, log)
	run := func(phase string) {
		res, err := sweeper.SweepAbandoned(ctx)
		if err != nil {
			log.Error("attachment session sweep FAILED", "phase", phase, "error", err,
				"attempted", res.Attempted, "failed", res.Failed)
			return
		}
		// Logged whenever there was WORK, not only when some of it succeeded:
		// "we looked and found nothing" and "we found 400 and reclaimed none"
		// must not be the same silence.
		if res.Attempted > 0 {
			log.Info("attachment session sweep",
				"phase", phase, "attempted", res.Attempted,
				"reclaimed", res.Swept, "failed", res.Failed)
		}
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		run("startup")
		for {
			select {
			case <-ctx.Done():
				log.Info("attachment session sweep worker stopped")
				return
			case <-ticker.C:
				run("tick")
			}
		}
	}()
}
