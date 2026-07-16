package purge

import (
	"context"
	"database/sql"
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

	// stragglerGraceSeconds excludes rows soft-deleted so recently that the
	// background worker is still expected to be draining them (do not race it).
	stragglerGraceSeconds = 300 // 5 minutes

	// stragglerSweepLimit bounds rows scanned per sweep tick. It is a stride, NOT a
	// cap: reaped rows are marked (blob_reaped_at) and therefore leave the candidate
	// set, so consecutive ticks advance through the backlog until it is drained.
	stragglerSweepLimit = 1000
)

// Reaper deletes attachment blobs from object storage after their message rows are
// purged. Deletes happen off the request path via a bounded queue drained by
// StartWorker; SweepStragglers is a periodic safety net for blobs orphaned by a
// crash between the engine's metadata soft-delete commit and the worker draining.
type Reaper struct {
	db    *sql.DB
	log   *logger.Logger
	store media.ObjectDeleter // may be nil (dev / no object store) — reapBlob tolerates it
	jobs  chan string
}

// NewReaper constructs a reaper with a bounded blob-delete queue.
func NewReaper(db *sql.DB, log *logger.Logger, store media.ObjectDeleter) *Reaper {
	return &Reaper{
		db:    db,
		log:   log,
		store: store,
		jobs:  make(chan string, blobQueueSize),
	}
}

// EnqueueBlobDeletes hands storage keys to the background worker. Non-blocking: if
// the bounded buffer is full, the key is dropped (and logged) rather than stalling
// the caller — SweepStragglers re-reaps any key the worker never drains.
//
// CONTRACT: callers may only enqueue keys that are unique per upload (today:
// `attachments/<fileID>` from the engine's reap CTE). The worker deletes the object
// unconditionally, WITHOUT the live-key guard reapSweptBlob applies — enqueueing a
// DETERMINISTIC tier-1 key (`avatars/<userID>`, media.tier1StorageKey) would delete
// an object a live row may still point at. Route any such key through the sweep.
func (r *Reaper) EnqueueBlobDeletes(keys []string) {
	for _, key := range keys {
		if key == "" {
			continue
		}
		select {
		case r.jobs <- key:
		default:
			r.log.Warn("purge reaper: blob-delete queue full; dropping key for sweeper recovery", "key", key)
		}
	}
}

// StartWorker drains the queue, reaping one blob per key (best-effort, off the
// request path). Blocks until ctx is cancelled; run it in a goroutine.
func (r *Reaper) StartWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-r.jobs:
			r.reapBlob(ctx, key)
		}
	}
}

// reapBlob deletes one object and, only on success, marks its media_files row
// reaped so the sweep stops considering it.
//
// This does NOT use media.CleanupObject, which is the wrong tool for the purge
// path in two ways. (1) Its metadata UPDATE is guarded on `deleted_at IS NULL`,
// but the engine's delete CTE already set deleted_at — so for purge rows it is a
// permanent no-op, leaving nothing to mark the blob reaped. (2) It marks the row
// even when DeleteObject failed. Here a failed delete returns WITHOUT marking, so
// the row stays a sweep candidate and is retried rather than being recorded as a
// reap that never happened (which would leak the object forever).
//
// A nil store is dev/no-object-store: there is no blob to leak, so mark it reaped
// rather than leaving the sweep to spin on it every tick.
func (r *Reaper) reapBlob(ctx context.Context, key string) {
	if r.store != nil {
		if err := r.store.DeleteObject(ctx, key); err != nil {
			r.log.Warn("purge reaper: blob delete failed; leaving unmarked for sweep retry",
				"error", err, "key", key)
			return
		}
	}
	r.markReaped(ctx, key)
}

// reapSweptBlob is reapBlob plus the live-key guard, for keys the SWEEP selected.
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
func (r *Reaper) reapSweptBlob(ctx context.Context, key string) {
	var liveExists bool
	if err := r.db.QueryRowContext(ctx, liveRowForKeyQuery, key).Scan(&liveExists); err != nil {
		// Fail closed: neither delete nor mark, so the next tick retries. Never delete
		// an object whose live-reference status could not be established.
		r.log.Warn("purge reaper: live-key check failed; skipping reap for retry",
			"error", err, "key", key)
		return
	}
	if liveExists {
		// The object belongs to a live row now. Mark the soft-deleted row so the sweep
		// advances past it, but leave the object alone — it is in use.
		r.markReaped(ctx, key)
		return
	}
	r.reapBlob(ctx, key)
}

// markReaped records that a key needs no further reaping. Best-effort: if the mark
// does not land, the sweep re-issues an idempotent delete and retries it — a
// redundant delete, never a leak.
func (r *Reaper) markReaped(ctx context.Context, key string) {
	if _, err := r.db.ExecContext(ctx, markBlobReapedQuery, key); err != nil {
		r.log.Warn("purge reaper: failed to mark blob reaped", "error", err, "key", key)
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
// Accepted limitation (head-of-line): a failed delete stays unmarked and keeps its
// old deleted_at, so if a full stride's worth of the OLDEST rows fail persistently
// while storage is otherwise healthy, each tick re-selects that same set and newer
// rows wait behind it. Not fixed here: the realistic failures are storage-wide
// (nothing to starve — every key is stuck) or transient (clears on the next tick),
// a persistently-failing subset of 1000+ keys is exotic, and each failure logs. New
// purges are also unaffected — they reap through the queue; only this safety net
// backs up. Upgrade path if it ever bites: a reap_attempts counter ordered
// (attempts, deleted_at) so repeat failures sink instead of blocking the head.
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

// sweepOnce runs a single straggler sweep: collect candidate keys, then reap each.
// Best-effort — errors are logged, not returned. Re-reaping an already-deleted blob
// is an idempotent no-op (DeleteObject is idempotent; the mark is guarded on
// blob_reaped_at IS NULL).
func (r *Reaper) sweepOnce(ctx context.Context) {
	keys, err := r.collectStragglers(ctx)
	if err != nil {
		r.log.Warn("purge reaper: straggler sweep failed", "error", err)
		return
	}
	for _, key := range keys {
		r.reapSweptBlob(ctx, key)
	}
}

// collectStragglers reads the next stride of unreaped straggler storage keys.
func (r *Reaper) collectStragglers(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, stragglerSweepQuery,
		stragglerGraceSeconds, stragglerSweepLimit)
	if err != nil {
		return nil, fmt.Errorf("purge reaper: straggler query: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("purge reaper: straggler scan: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("purge reaper: straggler iteration: %w", err)
	}
	return keys, nil
}

// stragglerSweepQuery selects storage keys of soft-deleted media_files rows whose
// blob is not yet reaped. $1 grace skips rows the worker is still expected to be
// draining (do not race it); $2 is the per-tick stride.
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
const stragglerSweepQuery = `SELECT storage_key FROM media_files
	         WHERE deleted_at IS NOT NULL
	           AND blob_reaped_at IS NULL
	           AND media_tier = 2
	           AND deleted_at < NOW() - make_interval(secs => $1)
	         ORDER BY deleted_at
	         LIMIT $2`

// markBlobReapedQuery records a confirmed object delete. Guarded on
// blob_reaped_at IS NULL so a concurrent worker/sweep re-mark is a no-op.
//
// `deleted_at IS NOT NULL` is load-bearing, not redundant: tier-1 keys are
// deterministic, so a LIVE row can share this storage_key with the soft-deleted row
// being reaped (see reapBlob). Marking that live row would pre-stamp it reaped, and
// when it is later soft-deleted the sweep would skip it — leaking its object. Only
// already-soft-deleted rows may ever carry the marker.
const markBlobReapedQuery = `UPDATE media_files SET blob_reaped_at = NOW()
	         WHERE storage_key = $1 AND deleted_at IS NOT NULL AND blob_reaped_at IS NULL`

// liveRowForKeyQuery reports whether any LIVE (not soft-deleted) media_files row
// still points at this storage_key — i.e. whether the object is still in use.
const liveRowForKeyQuery = `SELECT EXISTS (
	         SELECT 1 FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL)`
