package purge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// blobQueueSize bounds the in-memory blob-delete backlog. Overflow is dropped
	// (logged) rather than blocking the request path — a dropped key is recovered
	// by SweepStragglers, never leaked.
	blobQueueSize = 4096

	// stragglerSweepInterval is how often the crash-orphan sweep runs.
	stragglerSweepInterval = 15 * time.Minute

	// blobDeleteTimeout prevents one stalled backend request from pinning the
	// serial queue worker or straggler sweep indefinitely.
	blobDeleteTimeout = 15 * time.Second

	// stragglerGraceSeconds excludes rows soft-deleted so recently that the
	// background worker is still expected to be draining them (do not race it).
	stragglerGraceSeconds = 300 // 5 minutes

	// stragglerSweepLimit bounds rows scanned per sweep tick. It is a stride, NOT a
	// cap: reaped rows are marked (blob_reaped_at) and therefore leave the candidate
	// set, so consecutive ticks advance through the backlog until it is drained.
	// One tenth is reserved for retries so persistent failures cannot fill a tick.
	stragglerSweepLimit = 1000
)

// Reaper deletes attachment blobs from object storage after their message rows are
// purged. Deletes happen off the request path via a bounded queue drained by
// StartWorker; SweepStragglers is a periodic safety net for blobs orphaned by a
// crash between the engine's metadata soft-delete commit and the worker draining.
//
// PLACEMENT IS PER OBJECT (ADR-0038 / #2759 unit B2). Every blob the reaper
// touches carries the backend that holds it, resolved from the
// media_files.storage_backend column of the SAME ROW the reaper already read —
// never from a single process-wide client. See reapBlob for what a single-store
// reaper silently destroys.
type Reaper struct {
	db  *sql.DB
	log *logger.Logger
	// backends resolves a row's storage_backend to the store holding its object.
	//
	// A NIL resolver is an explicit statement by the embedder that this process
	// has NO object storage at all — it is what preserves the old nil-store
	// behaviour for dev and for tests. It is NOT reachable from cmd/server,
	// which always constructs a registry, so a production deployment whose
	// object storage is misconfigured fails closed and stays a retry candidate
	// rather than quietly marking blobs reaped.
	backends media.DeleterResolver
	jobs     chan media.BlobRef
}

// NewReaper constructs a reaper with a bounded blob-delete queue.
func NewReaper(db *sql.DB, log *logger.Logger, backends media.DeleterResolver) *Reaper {
	return &Reaper{
		db:       db,
		log:      log,
		backends: backends,
		jobs:     make(chan media.BlobRef, blobQueueSize),
	}
}

// EnqueueBlobDeletes hands blob references to the background worker. Non-blocking:
// if the bounded buffer is full, the ref is dropped (and logged) rather than
// stalling the caller — SweepStragglers re-reaps any blob the worker never drains.
//
// A ref carries the BACKEND as well as the key. Enqueueing a bare key would leave
// the worker to guess at placement, and the wrong guess is not a failed delete but
// a SUCCESSFUL one against the wrong bucket (see reapBlob).
//
// CONTRACT: callers may only enqueue refs that are unique per upload (today:
// `attachments/<fileID>` from the engine's reap CTE). The worker deletes the object
// unconditionally, WITHOUT the live-key guard reapSweptBlob applies — enqueueing a
// DETERMINISTIC tier-1 key (`avatars/<userID>`, media.tier1StorageKey) would delete
// an object a live row may still point at. Route any such key through the sweep.
func (r *Reaper) EnqueueBlobDeletes(refs []media.BlobRef) {
	for _, ref := range refs {
		if ref.Key == "" {
			continue
		}
		select {
		case r.jobs <- ref:
		default:
			r.log.Warn("purge reaper: blob-delete queue full; dropping key for sweeper recovery",
				"key", ref.Key, "storage_backend", ref.BackendLabel())
		}
	}
}

// StartWorker drains the queue, reaping one blob per ref (best-effort, off the
// request path). Blocks until ctx is cancelled; run it in a goroutine.
func (r *Reaper) StartWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ref := <-r.jobs:
			r.reapBlob(ctx, ref)
		}
	}
}

// reapBlob deletes one object from THE BACKEND ITS ROW NAMES and, only on
// success, marks that row reaped so the sweep stops considering it.
//
// THE FAILURE THIS SHAPE EXISTS TO PREVENT. An S3 DELETE against a key that is
// absent from the target bucket returns SUCCESS. So resolving the wrong store —
// or resolving no store and deleting from the one process-wide client anyway —
// does not fail: it returns nil, markReaped stamps blob_reaped_at, and the row
// leaves the straggler sweep's candidate set (`blob_reaped_at IS NULL`)
// PERMANENTLY while the object survives at the vendor. Postgres then records an
// erasure that never happened on a path that carries account deletions (000059),
// message purges (000090) and GDPR Article 17 requests, and the only retry
// signal has been destroyed. An unresolvable backend therefore leaves the row
// UNMARKED — the same fail-closed shape reapSweptBlob's live-key check already
// uses, for the same reason: never record what you could not establish.
//
// This does NOT use media.CleanupObject, which is the wrong tool for the purge
// path in two ways. (1) Its metadata UPDATE is guarded on `deleted_at IS NULL`,
// but the engine's delete CTE already set deleted_at — so for purge rows it is a
// permanent no-op, leaving nothing to mark the blob reaped. (2) It marks the row
// even when DeleteObject failed. Here a failed delete returns WITHOUT marking, so
// the row stays a sweep candidate and is retried rather than being recorded as a
// reap that never happened (which would leak the object forever).
func (r *Reaper) reapBlob(ctx context.Context, ref media.BlobRef) {
	if r.backends == nil {
		// No object storage in this process at all (dev / test). There is no
		// blob to leak for a row that never named a backend, so mark it reaped
		// rather than leaving the sweep to spin on it every tick. A row that
		// DOES name one is a different claim entirely — refuse it, because
		// "there is nothing to delete" is no longer something we can assert.
		if ref.Backend != nil {
			r.log.Error("purge reaper: row names a storage backend but no resolver is wired; leaving unmarked for sweep retry",
				"key", ref.Key, "storage_backend", ref.BackendLabel())
			r.recordReapFailure(ctx, ref)
			return
		}
		r.markReaped(ctx, ref)
		return
	}

	store, err := r.backends.ResolveDeleter(ref.Backend)
	if err != nil {
		r.log.Error("purge reaper: could not resolve the storage backend for a blob; leaving unmarked for sweep retry",
			"error", err, "key", ref.Key, "storage_backend", ref.BackendLabel())
		r.recordReapFailure(ctx, ref)
		return
	}
	deleteCtx, cancel := context.WithTimeout(ctx, blobDeleteTimeout)
	deleteErr := store.DeleteObject(deleteCtx, ref.Key)
	cancel()
	if deleteErr != nil {
		r.log.Warn("purge reaper: blob delete failed; leaving unmarked for sweep retry",
			"error", deleteErr, "key", ref.Key, "storage_backend", ref.BackendLabel())
		r.recordReapFailure(ctx, ref)
		return
	}
	r.markReaped(ctx, ref)
}

// reapSweptBlob is reapBlob plus the live-key guard, for blobs the SWEEP selected.
//
// DEFENSE IN DEPTH. The sweep is bounded to media_tier = 2 (see stragglerSweepQuery),
// where keys are per-upload unique, so no live row can share a swept key today and
// this guard should never fire. It stays because it is cheap and it is the backstop
// if that bound is ever widened or a deterministic key otherwise reaches this path —
// the failure it prevents (deleting a user's current avatar) is silent and severe.
//
// The guard lives here rather than in reapBlob because the two paths face different
// inputs. The worker only ever reaps keys the engine just produced from an
// attachments join — `attachments/<fileID>`, minted per upload from a fresh uuid, so
// no live row can share them. The sweep selects ARBITRARY historical soft-deleted
// rows, including tier-1 media whose keys are DETERMINISTIC per subject
// (`avatars/<userID>`, `server-icons/<serverID>`, `dm-icons/<conversationID>` —
// media.tier1StorageKey). Because media_files' unique index on storage_key is PARTIAL
// (WHERE deleted_at IS NULL), replacing an avatar must soft-delete the old row before
// inserting the new one — leaving an old soft-deleted row legitimately sharing
// `avatars/<userID>` with the new LIVE row. Reaping that key would delete the user's
// CURRENT avatar, so the sweep must never delete an object a live row still uses.
//
// The guard is PAIR-KEYED (see liveRowForKeyQuery) because "an object a live row
// still uses" is a statement about (bucket, key), not about the key alone. A live
// row on a DIFFERENT backend does not use THIS object, and blocking on it would
// skip a delete that must happen — a leak in the direction the whole unit exists
// to close. Pair-keying makes the guard exact in both directions rather than
// merely conservative in one.
func (r *Reaper) reapSweptBlob(ctx context.Context, ref media.BlobRef) {
	var liveExists bool
	if err := r.db.QueryRowContext(ctx, liveRowForKeyQuery, ref.Key, ref.Backend).Scan(&liveExists); err != nil {
		// Fail closed: neither delete nor mark, so the next tick retries. Never delete
		// an object whose live-reference status could not be established.
		r.log.Warn("purge reaper: live-key check failed; skipping reap for retry",
			"error", err, "key", ref.Key, "storage_backend", ref.BackendLabel())
		r.recordReapFailure(ctx, ref)
		return
	}
	if liveExists {
		// The object belongs to a live row now. Mark the soft-deleted row so the sweep
		// advances past it, but leave the object alone — it is in use.
		r.markReaped(ctx, ref)
		return
	}
	r.reapBlob(ctx, ref)
}

// markReaped records that a blob needs no further reaping. Best-effort: if the mark
// does not land, the sweep re-issues an idempotent delete and retries it — a
// redundant delete, never a leak.
func (r *Reaper) markReaped(ctx context.Context, ref media.BlobRef) {
	if _, err := r.db.ExecContext(ctx, markBlobReapedQuery, ref.Key, ref.Backend); err != nil {
		r.log.Warn("purge reaper: failed to mark blob reaped",
			"error", err, "key", ref.Key, "storage_backend", ref.BackendLabel())
	}
}

// SweepStragglers periodically reaps blobs the worker never deleted: media_files
// rows soft-deleted (deleted_at IS NOT NULL) whose object is still live
// (blob_reaped_at IS NULL), whether orphaned by a mid-purge restart or by queue
// overflow on a large purge. Blocks until ctx is cancelled; run it in a goroutine.
//
// This is the ONLY guarantee that an EnqueueBlobDeletes drop is recoverable, so it
// must have no upper time bound — see stragglerSweepQuery.
//
// Failed deletes stay unmarked but increment reap_attempts. Each tick reserves 10%
// (100 rows) for retries and fills the rest with fresh rows, so persistent failures
// cannot pin the stride. No finite worker guarantees retry progress: active backend
// groups must stay below 100, and persistent retry arrivals must stay below service
// capacity. The counter is keyed by the row's (storage_key, storage_backend) pair,
// preserving the placement claim that the delete path must never substitute from
// another row.
//
// An unresolvable BACKEND now joins that same class: those rows also stay unmarked
// and are retried every tick, which is the intended trade — a loud, bounded retry
// loop in exchange for never recording an erasure that did not happen.
func (r *Reaper) SweepStragglers(ctx context.Context) {
	ticker := time.NewTicker(stragglerSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweepOnce(ctx)
		}
	}
}

// sweepOnce runs a single straggler sweep: collect candidate blobs, then reap each.
// Best-effort — errors are logged, not returned. Re-reaping an already-deleted blob
// is an idempotent no-op (DeleteObject is idempotent; the mark is guarded on
// blob_reaped_at IS NULL).
func (r *Reaper) sweepOnce(ctx context.Context) {
	refs, err := r.collectStragglers(ctx)
	if err != nil {
		r.log.Warn("purge reaper: straggler sweep failed", "error", err)
		return
	}
	for _, ref := range refs {
		r.reapSweptBlob(ctx, ref)
	}
}

// collectStragglers reads the next stride of unreaped straggler blobs, each with
// the backend its row names. The backend comes from the SAME ROW as the key, in
// the same scan — the sweep never re-queries for placement it already selected.
func (r *Reaper) collectStragglers(ctx context.Context) (refs []media.BlobRef, returnErr error) {
	rows, err := r.db.QueryContext(ctx, stragglerSweepQuery,
		stragglerGraceSeconds, stragglerSweepLimit)
	if err != nil {
		return nil, fmt.Errorf("purge reaper: straggler query: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			refs = nil
			returnErr = errors.Join(returnErr, fmt.Errorf("purge reaper: close straggler rows: %w", closeErr))
		}
	}()

	for rows.Next() {
		var key string
		var backend sql.NullString
		if err := rows.Scan(&key, &backend); err != nil {
			return nil, fmt.Errorf("purge reaper: straggler scan: %w", err)
		}
		refs = append(refs, media.NewBlobRef(key, backend))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge reaper: straggler iteration: %w", err)
	}
	return refs, nil
}

// stragglerSweepQuery selects the storage key AND BACKEND of soft-deleted
// media_files rows whose blob is not yet reaped. $1 grace skips rows the worker is
// still expected to be draining (do not race it); $2 is the per-tick stride.
//
// storage_backend rides along with storage_key deliberately: it is the placement of
// the object being reaped, it is already on the row being selected, and reading it
// here is what lets every downstream statement key on the PAIR without a second
// query. NULL is the permanent value for every pre-cutover object.
//
// There is deliberately NO upper time bound. An earlier revision bounded the
// look-back to 24h "so the scan stays cheap", which was wrong twice over, because
// reaping did not (and could not) change a row's selectability:
//
//   - Rows aged past the bound were skipped forever, so any key dropped by a full
//     queue leaked its object permanently. One large purge — the feature's headline
//     "All Time" case — could orphan tens of thousands of blobs.
//   - Within the bound, ORDER BY + LIMIT re-selected the SAME oldest rows every
//     tick and never advanced (a purge stamps thousands of rows with near-identical
//     deleted_at), so the sweep reaped ~stride rows total and starved on the rest.
//
// `blob_reaped_at IS NULL` is what makes an unbounded sweep correct: a reaped row
// leaves the candidate set, so ticks advance and the backlog drains monotonically.
// Dropping the bound WITHOUT that predicate would be strictly worse than the bound —
// permanent starvation on the oldest rows instead of a 24h cliff. The cost stays
// bounded by the matching partial index (000090), which only covers unreaped rows.
//
// Declared const (not a function returning a literal) so static analysis can see the
// query is not dynamically constructed — no identifier or value is ever interpolated.
// The three materialized lanes reserve 10% of a stride for backend-fair retry rows,
// use the remainder for fresh rows, then backfill an underfull fresh lane with more
// retries. Fresh rows execute first so a stalled retry backend cannot block them.
// `media_tier = 2` bounds the sweep to MESSAGE ATTACHMENTS — the only rows a purge
// can orphan (the engine's reap CTE joins message_attachments / dm_message_attachments,
// and the valid_media_context CHECK makes tier 2 exactly the channel/conversation-scoped
// attachment set). It is a safety bound, not an optimization:
//
//   - Tier 1 is avatars/banners/icons, whose keys are DETERMINISTIC per subject
//     (`avatars/<userID>`). Sweeping them would put this reaper in the path of assets
//     it never owned — they have their own cleanup at their own call sites, unchanged
//     since before #1352.
//   - It makes the deterministic-key hazards STRUCTURALLY unreachable rather than
//     merely guarded: no live/soft-deleted key sharing, and no check-then-act TOCTOU
//     (a re-upload cannot land on an attachment key — `attachments/<fileID>` is minted
//     per upload from a fresh uuid, so there is nothing to race).
//
// Do NOT widen this to all tiers to "also clean up old avatars": tier-1 uploads write
// the object BEFORE inserting the row (media/handlers.go PutObject → insertTier1Record),
// so a sweep racing a re-upload would delete the live asset's object out from under it.
const stragglerSweepQuery = `WITH retry_ranked AS MATERIALIZED (
	SELECT id, storage_key, storage_backend, reap_attempts, deleted_at,
	       row_number() OVER (
	         PARTITION BY COALESCE(storage_backend, 'legacy')
	         ORDER BY reap_attempts, deleted_at
	       ) AS backend_rank
	  FROM media_files
	 WHERE deleted_at IS NOT NULL
	   AND blob_reaped_at IS NULL
	   AND media_tier = 2
	   AND deleted_at < NOW() - make_interval(secs => $1)
	   AND reap_attempts > 0
	), retry_reservation AS MATERIALIZED (
	SELECT id, storage_key, storage_backend, reap_attempts, deleted_at, backend_rank
	  FROM retry_ranked
	 ORDER BY backend_rank, reap_attempts, deleted_at
	 LIMIT GREATEST(1, $2 / 10)
), fresh AS MATERIALIZED (
	SELECT id, storage_key, storage_backend, reap_attempts, deleted_at
	  FROM media_files
	 WHERE deleted_at IS NOT NULL
	   AND blob_reaped_at IS NULL
	   AND media_tier = 2
	   AND deleted_at < NOW() - make_interval(secs => $1)
	   AND reap_attempts = 0
	 ORDER BY deleted_at
	 LIMIT GREATEST($2 - (SELECT count(*) FROM retry_reservation), 0)
), retry_overflow AS MATERIALIZED (
	SELECT id, storage_key, storage_backend, reap_attempts, deleted_at, backend_rank
	  FROM retry_ranked
	 WHERE NOT EXISTS (SELECT 1 FROM retry_reservation WHERE retry_reservation.id = retry_ranked.id)
	 ORDER BY backend_rank, reap_attempts, deleted_at
	 LIMIT GREATEST(
		$2 - (SELECT count(*) FROM retry_reservation) - (SELECT count(*) FROM fresh),
		0
	 )
)
SELECT storage_key, storage_backend
  FROM (
	SELECT 0 AS lane, storage_key, storage_backend, reap_attempts, deleted_at, NULL::bigint AS backend_rank FROM fresh
	UNION ALL
	SELECT 1, storage_key, storage_backend, reap_attempts, deleted_at, backend_rank FROM retry_reservation
	UNION ALL
	SELECT 2, storage_key, storage_backend, reap_attempts, deleted_at, backend_rank FROM retry_overflow
  ) AS selected
 ORDER BY lane, backend_rank NULLS LAST, reap_attempts, deleted_at
 LIMIT $2`

// markBlobReapedQuery records a confirmed object delete. Guarded on
// blob_reaped_at IS NULL so a concurrent worker/sweep re-mark is a no-op.
//
// `deleted_at IS NOT NULL` is load-bearing, not redundant: tier-1 keys are
// deterministic, so a LIVE row can share this storage_key with the soft-deleted row
// being reaped (see reapBlob). Marking that live row would pre-stamp it reaped, and
// when it is later soft-deleted the sweep would skip it — leaking its object. Only
// already-soft-deleted rows may ever carry the marker.
//
// PAIR-KEYED on (storage_key, storage_backend). The marker asserts "the object this
// row names is gone from the backend this row names", and one delete can only ever
// support that claim for rows on the SAME backend. Keying on storage_key alone would
// let a confirmed MinIO delete stamp a vendor-resident row that nobody touched — the
// exact silent-erasure defect this unit closes, arriving by a second route.
//
// The pair-keying is a NARROWING, so it cannot weaken the `deleted_at IS NOT NULL`
// property above: it can only ever match fewer rows, never a live one that the old
// predicate excluded. Rows sharing the key on another backend are left unmarked and
// stay sweep candidates, which is correct — their objects have not been deleted.
//
// `IS NOT DISTINCT FROM` (not `=`) because NULL is the permanent value of every
// pre-cutover object and `storage_backend = NULL` is never true; the explicit
// ::text cast removes any dependence on Postgres inferring a bare parameter's type.
const markBlobReapedQuery = `UPDATE media_files SET blob_reaped_at = NOW()
	         WHERE storage_key = $1
	           AND storage_backend IS NOT DISTINCT FROM $2::text
	           AND deleted_at IS NOT NULL
	           AND blob_reaped_at IS NULL`

// incrementReapAttemptsQuery records a failed delete without retiring the row from
// the sweep. Pair-keying keeps retry state attached to the object named by this row.
const incrementReapAttemptsQuery = `UPDATE media_files SET reap_attempts = reap_attempts + 1
	         WHERE storage_key = $1
	           AND storage_backend IS NOT DISTINCT FROM $2::text
	           AND deleted_at IS NOT NULL
	           AND blob_reaped_at IS NULL`

// recordReapFailure advances a failed row's fairness counter. Concurrent worker
// and sweep failures each count because this is an atomic ranking signal, not an
// idempotency token. Best-effort: failure accounting cannot turn a failed delete
// into a successful reap, so any database error is logged and the row remains
// eligible for another attempt.
func (r *Reaper) recordReapFailure(ctx context.Context, ref media.BlobRef) {
	if _, err := r.db.ExecContext(ctx, incrementReapAttemptsQuery, ref.Key, ref.Backend); err != nil {
		r.log.Warn("purge reaper: failed to record blob reap failure",
			"error", err, "key", ref.Key, "storage_backend", ref.BackendLabel())
	}
}

// liveRowForKeyQuery reports whether any LIVE (not soft-deleted) media_files row
// still points at this OBJECT — i.e. whether it is still in use.
//
// PAIR-KEYED for the same reason as markBlobReapedQuery, but note the direction is
// the opposite one: this predicate BLOCKS a delete, so narrowing it lets more
// deletes through. That is correct rather than permissive, because an object's
// identity is (bucket, key): a live row on a different backend points at a
// different object entirely, and refusing to delete ours on account of it would
// leak an object the sweep had correctly selected. The property "never delete an
// object a live row still uses" is preserved exactly — it is stated more precisely.
const liveRowForKeyQuery = `SELECT EXISTS (
	         SELECT 1 FROM media_files
	          WHERE storage_key = $1
	            AND storage_backend IS NOT DISTINCT FROM $2::text
	            AND deleted_at IS NULL)`
