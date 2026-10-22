package media

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Orphan reclamation is restricted to per-upload UUID keys whose public/read
// path requires a live media_files row: attachments and feedback screenshots.
// Fixed-key avatars, banners and icons may outlive their uploader's row while
// remaining referenced by another user or server; sweeping them would erase
// live assets. A 24-hour age floor protects the PutObject-before-row window.
// The attachment listing currently covers only the active attachment prefix;
// a future prefix rename also needs its older prefixes listed (#1608).

const (
	// DefaultOrphanSweepInterval is the cadence for the orphan sweep. Daily,
	// not hourly: unlike the session sweeper this one enumerates COMPLETED
	// objects, which is the whole bucket rather than a short queue of in-flight
	// uploads, and the bytes it reclaims have already been leaked for however
	// long the account deletion that stranded them has been in the past. There
	// is nothing to be gained by finding them sooner.
	DefaultOrphanSweepInterval = 24 * time.Hour

	// orphanSweepMinAge is how old an object must be before absence of a row is
	// read as evidence rather than as a race.
	//
	// BOTH attachment write paths put the object down BEFORE inserting its row
	// -- handleTier1Upload/insertTier1Record for tier 1, and the single-shot and
	// chunked attachment paths for tier 2. So between the PUT (or the multipart
	// complete) and the INSERT there is a window in which a perfectly healthy
	// upload looks exactly like an orphan, and deleting it there would destroy
	// an attachment out from under the request that was creating it.
	//
	// The window is bounded by one request's remaining lifetime, so it is
	// seconds. 24 hours is not a calculation, it is a refusal to make one: the
	// cost of being generous is that a leaked object waits an extra day, and
	// the cost of being tight is silent data loss on the happy path.
	orphanSweepMinAge = 24 * time.Hour

	// orphanClaimBatch bounds how many keys are checked against media_files per
	// round trip.
	orphanClaimBatch = 500
)

// orphanStore is the object-store surface the orphan reaper needs. Narrow on
// purpose, mirroring sweepStore: it makes the test double trivial and makes it
// evident that the reaper reads its work queue from the BUCKET.
type orphanStore interface {
	ListObjects(ctx context.Context, prefix string, olderThan time.Time) ([]storage.StoredObject, error)
	DeleteObject(ctx context.Context, key string) error
}

// claimedKeysQuery reports which of the candidate keys some media_files row
// claims ON THIS BACKEND.
//
// NO deleted_at PREDICATE, and that is the point. A soft-deleted row still
// claims its object: it is either awaiting the straggler sweep (which will
// delete the object and mark it) or already reaped. Either way the row exists,
// so the object is not an orphan and this reaper must not touch it -- the
// straggler sweep owns it, and two reclaimers racing the same object is how a
// blob_reaped_at marker comes to describe a delete nobody performed.
//
// PAIR-KEYED on (storage_key, storage_backend), like every other statement on
// the delete rail. The reaper enumerates ONE bucket, so "some row claims this
// key" is the wrong question -- a row naming a different backend describes a
// different object and must not spare this one. `IS NOT DISTINCT FROM` because
// NULL is the permanent value of every pre-cutover object and `= NULL` never
// matches.
const claimedKeysQuery = `SELECT storage_key FROM media_files
	         WHERE storage_key = ANY($1::text[])
	           AND storage_backend IS NOT DISTINCT FROM $2::text`

// OrphanReaper deletes unique-key objects that no media_files row claims.
//
// IT RECOVERS RESIDUE THE ROW-DRIVEN RAILS CANNOT SEE, which is the only reason
// it exists. Every other reclamation path in this codebase starts from a row:
// the purge engine's queue, the straggler sweep, CleanupObject. An account
// erasure HARD-deletes rows through media_files.uploader_id's ON DELETE CASCADE
// (migration 000042), so the moment a user is erased, every attachment they
// uploaded becomes invisible to all of them at once -- including rows that were
// soft-deleted and still waiting for the straggler sweep. The erasure capture
// in users.deleteAccountTx closes that going forward; this closes the history it
// cannot reach, and remains the backstop for any future path that drops a row
// without reclaiming its bytes.
type OrphanReaper struct {
	db      *sql.DB
	store   orphanStore
	log     *logger.Logger
	minAge  time.Duration
	backend string
}

// NewOrphanReaper builds a reaper over one backend's object store. backend is
// the registered backend identifier whose rows this store holds; it is both the
// pair key for the claim check and the log label.
func NewOrphanReaper(db *sql.DB, store orphanStore, backend string, log *logger.Logger) *OrphanReaper {
	return &OrphanReaper{db: db, store: store, log: log, minAge: orphanSweepMinAge, backend: backend}
}

// OrphanSweepResult reports what a sweep attempted, not only what it achieved.
//
// Same reasoning as SweepResult: counting successes alone lets a totally broken
// reaper report as a clean one. Lose s3:DeleteObject and every delete fails,
// Reaped stays 0, err stays nil, and a caller guarding on `Reaped > 0` prints
// nothing while the bucket fills.
type OrphanSweepResult struct {
	Listed   int
	Orphaned int
	Reaped   int
	Failed   int
}

// SweepOrphans deletes every unclaimed unique-key object older than the
// minimum age, and reports what it attempted.
//
// One failed delete never stops the sweep -- the next key is unrelated to it --
// but a batch in which EVERY delete failed is not a successful sweep and says so.
func (r *OrphanReaper) SweepOrphans(ctx context.Context) (OrphanSweepResult, error) {
	var res OrphanSweepResult
	candidates, err := r.listCandidates(ctx)
	if err != nil {
		return res, err
	}
	res.Listed = len(candidates)

	for start := 0; start < len(candidates); start += orphanClaimBatch {
		end := min(start+orphanClaimBatch, len(candidates))
		batch := candidates[start:end]

		claimed, err := r.claimedKeys(ctx, batch)
		if err != nil {
			// Fail closed, and note the scope honestly: this abandons the whole
			// SWEEP, not just this batch. Batches are independent windows over a
			// stable listing order, so a deterministic failure (a
			// statement_timeout on the ANY probe once media_files is large
			// enough) reproduces at the same offset daily and every key sorting
			// after it is never reclaimed. That is loud rather than silent --
			// the worker logs this at Error every run -- but the message would
			// not otherwise say that the tail of the bucket has been unreachable
			// since the first occurrence, so it says it here.
			return res, fmt.Errorf(
				"orphan sweep: claim check failed after %d of %d candidates; "+
					"every key ordered after this batch is unreclaimed until it succeeds: %w",
				start, len(candidates), err)
		}

		for _, key := range batch {
			// Shutdown is not a broken reaper. Without this, cancelling
			// cleanupCtx mid-sweep fails every remaining delete and trips the
			// all-failed alarm below with "object-store reclaim is not working"
			// -- an alertable line describing an orderly shutdown.
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			if _, ok := claimed[key]; ok {
				continue
			}
			res.Orphaned++
			if err := r.store.DeleteObject(ctx, key); err != nil {
				r.log.Warn("orphan sweep: delete failed; leaving for the next sweep",
					"error", err, "storage_key", key, "backend", r.backendLabel())
				res.Failed++
				continue
			}
			res.Reaped++
		}
	}

	// `res.Failed == res.Orphaned` is implied by `res.Reaped == 0` (every orphan
	// increments exactly one of the two), so this is the all-or-nothing case
	// only. The partial case is caught by the WARN in the worker, not here --
	// returning an error on a partial failure would abort a sweep that is
	// mostly working.
	if res.Orphaned > 0 && res.Reaped == 0 {
		return res, fmt.Errorf(
			"every orphan delete failed (%d attempted); object-store reclaim is not working", res.Failed)
	}
	return res, nil
}

func (r *OrphanReaper) listCandidates(ctx context.Context) ([]string, error) {
	var candidates []string
	cutoff := time.Now().Add(-r.minAge)
	for _, prefix := range []string{attachmentKeyPrefix, "feedback-screenshots/"} {
		objects, err := r.store.ListObjects(ctx, prefix, cutoff)
		if err != nil {
			return nil, err
		}
		for _, obj := range objects {
			if !isRecognizedAttachmentKey(obj.Key) && !strings.HasPrefix(obj.Key, "feedback-screenshots/") {
				continue
			}
			candidates = append(candidates, obj.Key)
		}
	}
	return candidates, nil
}

// claimedKeys returns the subset of keys that some media_files row claims on
// this reaper's backend.
func (r *OrphanReaper) claimedKeys(ctx context.Context, keys []string) (map[string]struct{}, error) {
	claimed := make(map[string]struct{}, len(keys))
	rows, err := r.db.QueryContext(ctx, claimedKeysQuery, pq.Array(keys), r.backendArg())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		claimed[key] = struct{}{}
	}
	return claimed, rows.Err()
}

// backendArg renders this reaper's backend for the pair key. The LEGACY backend
// is passed as NULL.
//
// This is NARROWER than isLegacyBackend, which accepts NULL *or* the literal
// "legacy" -- an earlier revision claimed to match it and did not. The narrowing
// is safe only because no writer produces the literal: write_routing.go returns
// "" for LegacyBackendID and its contract says to leave the column NULL. But the
// asymmetry points toward data loss (a row carrying the literal would fail the
// claim check and its live object would be deleted), so if a writer ever starts
// emitting it, this must widen to an accepted-spellings array rather than stay
// silent.
func (r *OrphanReaper) backendArg() any {
	if r.backend == "" || r.backend == string(storage.LegacyBackendID) {
		return nil
	}
	return r.backend
}

func (r *OrphanReaper) backendLabel() string {
	if r.backend == "" {
		return "(unlabelled)"
	}
	return r.backend
}

// StartOrphanSweepWorkers starts one orphan reaper per REGISTERED backend and
// returns how many it actually started, so a caller can tell "reaped nothing"
// from "started nothing".
//
// Per-backend for the same reason StartSessionSweepWorkers is: a reaper holding
// one client enumerates one bucket, so wiring only the legacy client would leave
// every vendor-resident orphan invisible -- and invisible in the silent
// direction, because Listed simply falls to zero on a bucket nobody enumerated.
func StartOrphanSweepWorkers(
	ctx context.Context,
	db *sql.DB,
	registry SweepBackendSource,
	log *logger.Logger,
	interval time.Duration,
) int {
	targets := resolveSweepTargets(registry, "attachment orphan sweep", log)
	for _, t := range targets {
		startOrphanSweepWorker(ctx, NewOrphanReaper(db, t.store, t.backend, log), log, interval)
	}
	return len(targets)
}

// startOrphanSweepWorker runs the sweep once at startup -- catching orphans
// stranded while the process was down -- and then on a fixed interval.
func startOrphanSweepWorker(
	ctx context.Context,
	reaper *OrphanReaper,
	log *logger.Logger,
	interval time.Duration,
) {
	run := func(phase string) {
		res, err := reaper.SweepOrphans(ctx)
		if err != nil {
			log.Error("attachment orphan sweep FAILED", "phase", phase, "backend", reaper.backendLabel(),
				"error", err, "listed", res.Listed, "orphaned", res.Orphaned, "failed", res.Failed)
			return
		}
		// UNCONDITIONAL, and at WARN when anything failed. The previous
		// `res.Orphaned > 0` guard was the same success-shaped mistake
		// OrphanSweepResult's own doc comment warns about, applied one field
		// over: a backend whose bucket or prefix is misconfigured lists zero
		// objects, so Listed = Orphaned = 0, err = nil, and the worker logged
		// NOTHING -- every day, forever, while that bucket's entire erasure
		// residue accumulated and the only evidence was an absence.
		//
		// Failed > 0 is WARN independently of whether anything succeeded. A
		// partial denial (an object-lock rule or a bucket policy refusing a
		// subset) yields Reaped=12 Failed=388 with err nil, which the
		// all-or-nothing alarm below cannot see; those 388 keys are re-listed
		// and re-fail identically tomorrow, so at Info a 97%-broken reaper is
		// invisible to any alerting rule keyed on WARN.
		fields := []any{
			"phase", phase, "backend", reaper.backendLabel(),
			"listed", res.Listed, "orphaned", res.Orphaned,
			"reclaimed", res.Reaped, "failed", res.Failed,
		}
		if res.Failed > 0 {
			log.Warn("attachment orphan sweep completed with failures", fields...)
			return
		}
		log.Info("attachment orphan sweep", fields...)
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		// Checked BEFORE the startup sweep, not only in the select below. A
		// context that is already dead means the process is shutting down, and
		// a sweep started into it would issue object-store calls that cannot
		// complete -- the ticker branch would catch it one full interval later.
		if ctx.Err() != nil {
			return
		}
		run("startup")
		for {
			select {
			case <-ctx.Done():
				log.Info("attachment orphan sweep worker stopped")
				return
			case <-ticker.C:
				run("tick")
			}
		}
	}()
}
