package media

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// DefaultTier1ErasureReclaimInterval is the fixed retry cadence.
	DefaultTier1ErasureReclaimInterval = time.Minute

	tier1ErasureReclaimBatchMax      = 100
	tier1ErasureReclaimQuantum       = 10
	tier1ErasureClaimTimeout         = 10 * time.Second
	tier1ErasureDeleteTimeout        = 10 * time.Second
	tier1ErasureRetryDelay           = time.Minute
	tier1ErasureMaintenanceDelay     = 24 * time.Hour
	logMsgTier1ErasureReclaimFailure = "tier1 erasure reclaim pass failed"
)

// reconcile_after schedules retry; it is not an exclusive external-I/O lease.
// A non-cooperative ObjectDeleter may overlap a later retry. This is intentional
// at-least-once delivery: obligations name exact, permanently dead profile keys.
const claimTier1ErasureObligationsQuery = `WITH due AS (
	SELECT storage_key
	FROM tier1_erasure_delete_obligations
	WHERE reconcile_after <= $3
	ORDER BY reconcile_after, storage_key
	LIMIT $1
	FOR UPDATE SKIP LOCKED
)
UPDATE tier1_erasure_delete_obligations AS obligations
SET attempts = obligations.attempts + 1,
	reconcile_after = clock_timestamp() + make_interval(secs => $2)
FROM due
WHERE obligations.storage_key = due.storage_key
RETURNING obligations.storage_key`

const expireTier1ProfileUploadIntentsQuery = `WITH due AS (
	SELECT storage_key
	FROM tier1_profile_upload_intents
	WHERE expires_at <= $2
	ORDER BY expires_at, storage_key
	LIMIT $1
	FOR UPDATE SKIP LOCKED
), terminalized AS (
	INSERT INTO tier1_erasure_delete_obligations (storage_key)
	SELECT storage_key FROM due
	ON CONFLICT (storage_key) DO NOTHING
)
DELETE FROM tier1_profile_upload_intents AS intents
USING due
WHERE intents.storage_key = due.storage_key
RETURNING intents.storage_key`

// acknowledgeTier1ErasureQuery removes only a proven published immutable
// generation. A missing metadata row is intentional evidence of an ambiguous
// PutObject and retains its permanent tombstone for late-write reclamation.
const acknowledgeTier1ErasureQuery = `WITH removed AS (
	DELETE FROM tier1_erasure_delete_obligations AS obligations
	WHERE obligations.storage_key = $1
	  AND (obligations.storage_key LIKE 'avatars/%/%' OR obligations.storage_key LIKE 'banners/%/%')
	  AND EXISTS (
		SELECT 1 FROM media_files
		WHERE storage_key = obligations.storage_key
		  AND media_tier = 1
		  AND profile_slot IS NOT NULL
		  AND deleted_at IS NOT NULL
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM media_files
		WHERE storage_key = obligations.storage_key AND deleted_at IS NULL
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM tier1_profile_upload_intents
		WHERE storage_key = obligations.storage_key
	  )
	RETURNING storage_key
), updated AS (
	UPDATE tier1_erasure_delete_obligations
	SET last_delete_at = clock_timestamp(),
	    reconcile_after = clock_timestamp() + make_interval(secs => CASE
		WHEN last_delete_at IS NULL THEN $2::double precision
		ELSE $3::double precision
	END)
	WHERE storage_key = $1
	  AND NOT EXISTS (SELECT 1 FROM removed)
	RETURNING storage_key
)
SELECT storage_key FROM removed
UNION ALL
SELECT storage_key FROM updated`

// Tier1ErasureReclaimStats contains aggregate results from one reclaim pass.
type Tier1ErasureReclaimStats struct {
	Claimed  int
	Deleted  int
	Retained int
}

// Tier1ErasureReclaimer owns durable tier-1 erasure retries.
type Tier1ErasureReclaimer struct {
	db     *sql.DB
	legacy ObjectDeleter
	log    *logger.Logger
	wake   chan struct{}
}

// NewTier1ErasureReclaimer constructs the durable tier-1 erasure worker.
func NewTier1ErasureReclaimer(
	db *sql.DB,
	legacy ObjectDeleter,
	log *logger.Logger,
) *Tier1ErasureReclaimer {
	return &Tier1ErasureReclaimer{
		db: db, legacy: legacy, log: log, wake: make(chan struct{}, 1),
	}
}

// Start begins startup and fixed-interval processing and returns its waiter.
func (r *Tier1ErasureReclaimer) Start(ctx context.Context) func() {
	if r == nil {
		return func() {
			// No worker was started, so there is nothing to wait for.
		}
	}
	return r.start(ctx, r.reclaimFairDue)
}

// start owns lifecycle orchestration and remains package-test-callable.
func (r *Tier1ErasureReclaimer) start(ctx context.Context, run func(context.Context)) func() {
	if r == nil || run == nil {
		return func() {
			// No worker was started, so there is nothing to wait for.
		}
	}
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		run(ctx)
		ticker := time.NewTicker(DefaultTier1ErasureReclaimInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
				run(ctx)
			case <-ticker.C:
				run(ctx)
			}
		}
	}()
	return done.Wait
}

// Wake requests a coalesced reclaim pass.
func (r *Tier1ErasureReclaimer) Wake() {
	if r == nil || r.wake == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// reclaimDue claims and processes all permanent obligations due at the start of
// the pass. Its draining behaviour is package-test-callable and intentionally
// remains separate from Start's fair two-rail scheduling.
func (r *Tier1ErasureReclaimer) reclaimDue(
	ctx context.Context,
	limit int,
) (stats Tier1ErasureReclaimStats, returnErr error) {
	if r == nil || r.db == nil {
		return stats, errors.New("tier1 erasure reclaimer database is unavailable")
	}
	if limit <= 0 || limit > tier1ErasureReclaimBatchMax {
		limit = tier1ErasureReclaimBatchMax
	}

	cutoffCtx, cancelCutoff := context.WithTimeout(ctx, tier1ErasureClaimTimeout)
	var cutoff time.Time
	err := r.db.QueryRowContext(cutoffCtx, `SELECT clock_timestamp()`).Scan(&cutoff)
	cancelCutoff()
	if err != nil {
		return stats, fmt.Errorf("capture tier1 erasure reclaim cutoff: %w", err)
	}
	// Keep rows that become due during external deletion for the next pass.
	for {
		batchStats, more, err := r.reclaimDueAtCutoff(ctx, limit, cutoff)
		if err != nil {
			return stats, err
		}
		stats.add(batchStats)
		if !more {
			break
		}
	}

	r.logStats(stats)
	return stats, nil
}

func (r *Tier1ErasureReclaimer) reclaimDueAtCutoff(
	ctx context.Context,
	limit int,
	cutoff time.Time,
) (stats Tier1ErasureReclaimStats, more bool, returnErr error) {
	keys, err := r.claimTier1ErasureBatch(ctx, limit, cutoff)
	if err != nil {
		return stats, false, err
	}
	stats.Claimed = len(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return stats, false, err
		}
		if !r.deleteAndReschedule(ctx, key) {
			stats.Retained++
			continue
		}
		stats.Deleted++
	}
	return stats, len(keys) == limit, nil
}

func (s *Tier1ErasureReclaimStats) add(other Tier1ErasureReclaimStats) {
	s.Claimed += other.Claimed
	s.Deleted += other.Deleted
	s.Retained += other.Retained
}

// reclaimFairDue first terminalizes expired upload intents, then reclaims
// permanent tombstones. Intents never invoke storage directly: their exact,
// immutable keys become the existing permanent deletion rail before deletion.
func (r *Tier1ErasureReclaimer) reclaimFairDue(ctx context.Context) {
	if r == nil || r.db == nil {
		r.logReclaimFailure(nil, "database")
		return
	}
	cutoffCtx, cancelCutoff := context.WithTimeout(ctx, tier1ErasureClaimTimeout)
	var cutoff time.Time
	err := r.db.QueryRowContext(cutoffCtx, `SELECT clock_timestamp()`).Scan(&cutoff)
	cancelCutoff()
	if err != nil {
		r.logReclaimFailure(err, "cutoff")
		return
	}

	for {
		more, err := r.expireProfileUploadIntentsAtCutoff(ctx, tier1ErasureReclaimQuantum, cutoff)
		if err != nil {
			if r.log != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				r.log.Error("tier1 profile upload intent expiry failed", "failure_class", "database")
			}
			break
		}
		if !more {
			break
		}
	}

	var permanentStats Tier1ErasureReclaimStats
	for {
		stats, more, err := r.reclaimDueAtCutoff(ctx, tier1ErasureReclaimQuantum, cutoff)
		if err != nil {
			r.logReclaimFailure(err, "claim")
			break
		}
		permanentStats.add(stats)
		if !more {
			break
		}
	}
	r.logStats(permanentStats)
}

func (r *Tier1ErasureReclaimer) logReclaimFailure(err error, failureClass string) {
	if r == nil || r.log == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	r.log.Error(logMsgTier1ErasureReclaimFailure, "failure_class", failureClass)
}

func (r *Tier1ErasureReclaimer) claimTier1ErasureBatch(
	ctx context.Context,
	limit int,
	cutoff time.Time,
) (keys []string, returnErr error) {
	claimCtx, cancelClaim := context.WithTimeout(ctx, tier1ErasureClaimTimeout)
	defer cancelClaim()
	tx, err := r.db.BeginTx(claimCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tier1 erasure claim: %w", err)
	}
	defer func() {
		if returnErr == nil {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rollback tier1 erasure claim: %w", rollbackErr))
		}
	}()

	rows, err := tx.QueryContext(
		claimCtx,
		claimTier1ErasureObligationsQuery,
		limit,
		int(tier1ErasureRetryDelay.Seconds()),
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("claim tier1 erasure obligations: %w", err)
	}
	keys = make([]string, 0, limit)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return nil, errors.Join(
					fmt.Errorf("scan claimed tier1 erasure obligation: %w", err),
					fmt.Errorf("close claimed tier1 erasure obligations: %w", closeErr),
				)
			}
			return nil, fmt.Errorf("scan claimed tier1 erasure obligation: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("read claimed tier1 erasure obligations: %w", err),
				fmt.Errorf("close claimed tier1 erasure obligations: %w", closeErr),
			)
		}
		return nil, fmt.Errorf("read claimed tier1 erasure obligations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close claimed tier1 erasure obligations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tier1 erasure claim: %w", err)
	}
	return keys, nil
}

func (r *Tier1ErasureReclaimer) expireProfileUploadIntentsAtCutoff(
	ctx context.Context,
	limit int,
	cutoff time.Time,
) (more bool, returnErr error) {
	claimCtx, cancel := context.WithTimeout(ctx, tier1ErasureClaimTimeout)
	defer cancel()
	tx, err := r.db.BeginTx(claimCtx, nil)
	if err != nil {
		return false, fmt.Errorf("begin profile upload intent expiry: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rollback profile upload intent expiry: %w", rollbackErr))
		}
	}()

	rows, err := tx.QueryContext(claimCtx, expireTier1ProfileUploadIntentsQuery, limit, cutoff)
	if err != nil {
		return false, fmt.Errorf("terminalize expired profile upload intents: %w", err)
	}
	count := 0
	for rows.Next() {
		var storageKey string
		if err := rows.Scan(&storageKey); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return false, errors.Join(
					fmt.Errorf("scan expired profile upload intent: %w", err),
					fmt.Errorf("close expired profile upload intents: %w", closeErr),
				)
			}
			return false, fmt.Errorf("scan expired profile upload intent: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return false, errors.Join(
				fmt.Errorf("read expired profile upload intents: %w", err),
				fmt.Errorf("close expired profile upload intents: %w", closeErr),
			)
		}
		return false, fmt.Errorf("read expired profile upload intents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close expired profile upload intents: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit profile upload intent expiry: %w", err)
	}
	committed = true
	return count == limit, nil
}

func (r *Tier1ErasureReclaimer) logStats(stats Tier1ErasureReclaimStats) {
	if r.log == nil || (stats.Claimed == 0 && stats.Retained == 0) {
		return
	}
	fields := []any{"claimed", stats.Claimed, "deleted", stats.Deleted, "retained", stats.Retained}
	if stats.Retained > 0 {
		r.log.Warn("tier1 erasure reclaim pass retrying tombstones", append(fields, "failure_class", "storage_or_reschedule")...)
		return
	}
	r.log.Info("tier1 erasure reclaim pass", fields...)
}

// deleteAndReschedule records a successful delete without removing the permanent tombstone.
func (r *Tier1ErasureReclaimer) deleteAndReschedule(ctx context.Context, key string) bool {
	if r.legacy == nil {
		return false
	}
	deleteCtx, cancelDelete := context.WithTimeout(context.WithoutCancel(ctx), tier1ErasureDeleteTimeout)
	deleteErr := r.legacy.DeleteObject(deleteCtx, key)
	cancelDelete()
	if deleteErr != nil {
		return false
	}

	ackCtx, cancelAck := context.WithTimeout(context.WithoutCancel(ctx), tier1ErasureDeleteTimeout)
	var acknowledgedKey string
	ackErr := r.db.QueryRowContext(
		ackCtx,
		acknowledgeTier1ErasureQuery,
		key,
		int(tier1ErasureRetryDelay.Seconds()),
		int(tier1ErasureMaintenanceDelay.Seconds()),
	).Scan(&acknowledgedKey)
	cancelAck()
	if ackErr != nil {
		return false
	}
	return acknowledgedKey == key
}
