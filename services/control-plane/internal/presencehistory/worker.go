package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	retentionInterval      = 24 * time.Hour
	retentionRetryInterval = 15 * time.Minute
	retentionOwnerBatch    = 100
	retentionRowBatch      = 500
	retentionExpiryAlert   = 24 * time.Hour
)

// SweepStats contains aggregate, account-independent cleanup measurements.
type SweepStats struct {
	Duration          time.Duration
	OwnerBatchCount   int
	BatchCount        int
	RowsDeleted       int64
	OldestExpiredAge  time.Duration
	ExpiredRowsRemain bool
}

type retentionWorkerHooks struct {
	afterOwnerBegin func(uuid.UUID)
	afterOwnerLocks func(uuid.UUID)
	afterRowsLocked func(uuid.UUID, []uuid.UUID)
}

// RetentionWorker removes expired Activity History rows in bounded,
// lock-order-safe transactions.
type RetentionWorker struct {
	db  *sql.DB
	log *logger.Logger

	sweep func(context.Context) (SweepStats, error)
	wait  func(context.Context, time.Duration) bool
	hooks retentionWorkerHooks
}

// NewRetentionWorker constructs the process-wide Activity History cleanup
// worker. Run owns cadence; Sweep remains available for startup and tests.
func NewRetentionWorker(db *sql.DB, log *logger.Logger) *RetentionWorker {
	worker := &RetentionWorker{
		db:   db,
		log:  log,
		wait: waitForRetention,
	}
	worker.sweep = worker.Sweep
	return worker
}

// Sweep deletes every currently claimable expired row while bounding owner
// discovery and each deletion transaction.
func (w *RetentionWorker) Sweep(ctx context.Context) (stats SweepStats, returnErr error) {
	started := time.Now()
	defer func() {
		stats.Duration = time.Since(started)
	}()
	if err := ctx.Err(); err != nil {
		return stats, err
	}

	oldestAge, err := w.readOldestExpiredAge(ctx)
	if err != nil {
		return stats, err
	}
	stats.OldestExpiredAge = oldestAge

	for {
		deleted, err := w.sweepExpiredPass(ctx, &stats)
		if err != nil {
			return stats, err
		}
		if deleted == 0 {
			remaining, err := w.hasExpiredRows(ctx)
			if err != nil {
				return stats, err
			}
			stats.ExpiredRowsRemain = remaining
			return stats, nil
		}
	}
}

func (w *RetentionWorker) hasExpiredRows(ctx context.Context) (bool, error) {
	var remaining bool
	if err := w.db.QueryRowContext(ctx, `
		WITH cutoff AS MATERIALIZED (
			SELECT clock_timestamp() AS observed
		)
		SELECT EXISTS (
			SELECT 1
			FROM presence_history
			CROSS JOIN cutoff
			WHERE expires_at <= cutoff.observed
		)
	`).Scan(&remaining); err != nil {
		return false, fmt.Errorf("check remaining expired activity rows: %w", err)
	}
	return remaining, nil
}

func (w *RetentionWorker) sweepExpiredPass(ctx context.Context, stats *SweepStats) (int64, error) {
	var (
		deletedThisPass int64
		after           *uuid.UUID
	)
	for {
		owners, err := w.discoverExpiredOwners(ctx, after)
		if err != nil {
			return 0, err
		}
		if len(owners) > 0 {
			stats.OwnerBatchCount++
		}
		for _, ownerID := range owners {
			if err := w.sweepExpiredOwner(ctx, ownerID, stats, &deletedThisPass); err != nil {
				return 0, err
			}
		}
		if len(owners) < retentionOwnerBatch {
			return deletedThisPass, nil
		}
		last := owners[len(owners)-1]
		after = &last
	}
}

func (w *RetentionWorker) sweepExpiredOwner(
	ctx context.Context,
	ownerID uuid.UUID,
	stats *SweepStats,
	deletedThisPass *int64,
) error {
	for {
		deleted, err := w.deleteExpiredOwnerBatch(ctx, ownerID)
		if err != nil {
			return err
		}
		if deleted > 0 {
			stats.BatchCount++
			stats.RowsDeleted += int64(deleted)
			*deletedThisPass += int64(deleted)
		}
		if deleted < retentionRowBatch {
			return nil
		}
	}
}

func (w *RetentionWorker) readOldestExpiredAge(ctx context.Context) (time.Duration, error) {
	var (
		observed time.Time
		oldest   sql.NullTime
	)
	if err := w.db.QueryRowContext(ctx, `
		WITH cutoff AS MATERIALIZED (
			SELECT clock_timestamp() AS observed
		)
		SELECT cutoff.observed, MIN(rows.expires_at)
		FROM cutoff
		LEFT JOIN presence_history AS rows
		  ON rows.expires_at <= cutoff.observed
		GROUP BY cutoff.observed
	`).Scan(&observed, &oldest); err != nil {
		return 0, fmt.Errorf("read activity cleanup age: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	age := observed.Sub(oldest.Time)
	if age < 0 {
		return 0, nil
	}
	return age, nil
}

func (w *RetentionWorker) discoverExpiredOwners(
	ctx context.Context,
	after *uuid.UUID,
) ([]uuid.UUID, error) {
	var cursor any
	if after != nil {
		cursor = *after
	}
	rows, err := w.db.QueryContext(ctx, `
		WITH cutoff AS MATERIALIZED (
			SELECT clock_timestamp() AS observed
		)
		SELECT DISTINCT entries.sender_id
		FROM presence_history AS entries
		CROSS JOIN cutoff
		WHERE entries.expires_at <= cutoff.observed
		  AND ($1::UUID IS NULL OR entries.sender_id > $1::UUID)
		ORDER BY entries.sender_id
		LIMIT $2
	`, cursor, retentionOwnerBatch)
	if err != nil {
		return nil, fmt.Errorf("discover activity cleanup owners: %w", err)
	}

	owners := make([]uuid.UUID, 0, retentionOwnerBatch)
	for rows.Next() {
		var ownerID uuid.UUID
		if err := rows.Scan(&ownerID); err != nil {
			return nil, closeRowsWithError(rows, "scan activity cleanup owner", err)
		}
		owners = append(owners, ownerID)
	}
	if err := rows.Err(); err != nil {
		return nil, closeRowsWithError(rows, "read activity cleanup owners", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close activity cleanup owners: %w", err)
	}
	return owners, nil
}

func (w *RetentionWorker) deleteExpiredOwnerBatch(
	ctx context.Context,
	ownerID uuid.UUID,
) (deleted int, returnErr error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin activity cleanup owner: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	committed := false
	defer mergeRetentionRollback(tx, &committed, &returnErr)

	if w.hooks.afterOwnerBegin != nil {
		w.hooks.afterOwnerBegin(ownerID)
	}

	ownerExists, err := lockRetentionOwner(ctx, tx, ownerID)
	if err != nil {
		return 0, err
	}
	if !ownerExists {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit missing activity cleanup owner: %w", err)
		}
		committed = true
		return 0, nil
	}

	if err := lockRetentionSettings(ctx, tx, ownerID); err != nil {
		return 0, err
	}
	if w.hooks.afterOwnerLocks != nil {
		w.hooks.afterOwnerLocks(ownerID)
	}

	var cutoff time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
		return 0, fmt.Errorf("read activity cleanup clock: %w", err)
	}

	ids, err := lockExpiredHistoryRows(ctx, tx, ownerID, cutoff)
	if err != nil {
		return 0, err
	}

	if w.hooks.afterRowsLocked != nil && len(ids) > 0 {
		w.hooks.afterRowsLocked(ownerID, append([]uuid.UUID(nil), ids...))
	}
	if err := deleteExpiredHistoryRows(ctx, tx, ownerID, ids); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit activity cleanup owner: %w", err)
	}
	committed = true
	return len(ids), nil
}

func mergeRetentionRollback(tx *sql.Tx, committed *bool, returnErr *error) {
	if *committed {
		return
	}
	rollbackErr := tx.Rollback()
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		*returnErr = errors.Join(*returnErr, fmt.Errorf("rollback activity cleanup owner: %w", rollbackErr))
	}
}

func lockRetentionOwner(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID) (bool, error) {
	var lockedID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM users
		WHERE id = $1
		FOR NO KEY UPDATE
	`, ownerID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock activity cleanup owner: %w", err)
	}
	return true, nil
}

func lockRetentionSettings(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID) error {
	var lockedID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id
		FROM user_presence_settings
		WHERE user_id = $1
		FOR UPDATE
	`, ownerID).Scan(&lockedID); err != nil {
		return fmt.Errorf("lock activity cleanup settings: %w", err)
	}
	return nil
}

func lockExpiredHistoryRows(
	ctx context.Context,
	tx *sql.Tx,
	ownerID uuid.UUID,
	cutoff time.Time,
) ([]uuid.UUID, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT expires_at, id
		FROM presence_history
		WHERE sender_id = $1
		  AND expires_at <= $2
		ORDER BY expires_at, id
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	`, ownerID, cutoff, retentionRowBatch)
	if err != nil {
		return nil, fmt.Errorf("lock expired activity rows: %w", err)
	}
	ids := make([]uuid.UUID, 0, retentionRowBatch)
	for rows.Next() {
		var (
			expiresAt time.Time
			id        uuid.UUID
		)
		if err := rows.Scan(&expiresAt, &id); err != nil {
			return nil, closeRowsWithError(rows, "scan expired activity row", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, closeRowsWithError(rows, "read expired activity rows", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close expired activity rows: %w", err)
	}
	return ids, nil
}

func deleteExpiredHistoryRows(
	ctx context.Context,
	tx *sql.Tx,
	ownerID uuid.UUID,
	ids []uuid.UUID,
) error {
	if len(ids) == 0 {
		return nil
	}
	idStrings := make([]string, len(ids))
	for index, id := range ids {
		idStrings[index] = id.String()
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM presence_history
		WHERE sender_id = $1
		  AND id = ANY($2::UUID[])
	`, ownerID, pq.Array(idStrings))
	if err != nil {
		return fmt.Errorf("delete expired activity rows: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted activity rows: %w", err)
	}
	if affected != int64(len(ids)) {
		return fmt.Errorf("delete expired activity rows: exact row count mismatch")
	}
	return nil
}

func closeRowsWithError(rows *sql.Rows, operation string, cause error) error {
	operationErr := fmt.Errorf("%s: %w", operation, cause)
	if closeErr := rows.Close(); closeErr != nil {
		return errors.Join(operationErr, fmt.Errorf("close activity cleanup rows: %w", closeErr))
	}
	return operationErr
}

// Run executes one startup pass, retries failures after the capped retry
// interval, and returns to the daily cadence after a successful recovery.
func (w *RetentionWorker) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sweep := w.sweep
	if sweep == nil {
		sweep = w.Sweep
	}
	wait := w.wait
	if wait == nil {
		wait = waitForRetention
	}

	for w.runPass(ctx, sweep, wait) {
	}
}

func (w *RetentionWorker) runPass(
	ctx context.Context,
	sweep func(context.Context) (SweepStats, error),
	wait func(context.Context, time.Duration) bool,
) bool {
	stats, err := sweep(ctx)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return false
		}
		w.logFailure(stats, classifyCleanupError(err))
		return wait(ctx, retentionRetryInterval)
	}
	w.logSuccess(stats)
	return wait(ctx, retentionDelay(stats))
}

func retentionDelay(stats SweepStats) time.Duration {
	if stats.ExpiredRowsRemain {
		return retentionRetryInterval
	}
	return retentionInterval
}

func (w *RetentionWorker) logSuccess(stats SweepStats) {
	if stats.OldestExpiredAge > retentionExpiryAlert {
		w.log.Warn(
			"activity cleanup exceeded deletion SLO",
			"operation", "activity_cleanup",
			"duration", stats.Duration,
			"owner_batch_count", stats.OwnerBatchCount,
			"batch_count", stats.BatchCount,
			"rows_deleted_count", stats.RowsDeleted,
			"oldest_expired_age", stats.OldestExpiredAge,
			"expired_rows_remain", stats.ExpiredRowsRemain,
		)
		return
	}
	w.log.Info(
		"activity cleanup completed",
		"operation", "activity_cleanup",
		"duration", stats.Duration,
		"owner_batch_count", stats.OwnerBatchCount,
		"batch_count", stats.BatchCount,
		"rows_deleted_count", stats.RowsDeleted,
		"oldest_expired_age", stats.OldestExpiredAge,
		"expired_rows_remain", stats.ExpiredRowsRemain,
	)
}

func (w *RetentionWorker) logFailure(stats SweepStats, errorClass string) {
	w.log.Error(
		"activity cleanup failed",
		"operation", "activity_cleanup",
		"error_class", errorClass,
		"duration", stats.Duration,
		"owner_batch_count", stats.OwnerBatchCount,
		"batch_count", stats.BatchCount,
		"rows_deleted_count", stats.RowsDeleted,
		"oldest_expired_age", stats.OldestExpiredAge,
		"expired_rows_remain", stats.ExpiredRowsRemain,
	)
}

func classifyCleanupError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "database"
	}
}

func waitForRetention(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
