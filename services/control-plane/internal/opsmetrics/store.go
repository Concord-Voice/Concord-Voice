package opsmetrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// MetricStore persists scalar samples and manages their hourly retention tiers.
type MetricStore interface {
	WriteSamples(ctx context.Context, samples []Sample) error
	Maintain(ctx context.Context, now time.Time) error
}

const (
	rawSampleRetention = 24 * time.Hour
	rollupRetention    = 8 * 24 * time.Hour
)

// PostgresStore is the PostgreSQL implementation of MetricStore for one configured node.
type PostgresStore struct {
	db     *sql.DB
	nodeID string
	now    func() time.Time

	timestampMu   sync.Mutex
	lastTimestamp time.Time
}

const writeSampleSQL = `
	INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
	VALUES ($1, $2, $3, $4)
`

const rollupSQL = `
	INSERT INTO ops_metric_rollups (
		node_id,
		metric_key,
		bucket_start,
		min_value,
		max_value,
		avg_value,
		last_value,
		sample_count
	)
	SELECT
		node_id,
		metric_key,
		date_trunc('hour', ts, 'UTC') AS bucket_start,
		MIN(value) AS min_value,
		MAX(value) AS max_value,
		GREATEST(MIN(value), LEAST(MAX(value), AVG(value))) AS avg_value,
		(ARRAY_AGG(value ORDER BY ts DESC))[1] AS last_value,
		COUNT(*)::INTEGER AS sample_count
	FROM ops_metric_samples
	WHERE ts < $1
	GROUP BY node_id, metric_key, date_trunc('hour', ts, 'UTC')
	ON CONFLICT (node_id, metric_key, bucket_start) DO NOTHING
`

const pruneRawSQL = `DELETE FROM ops_metric_samples WHERE ts < $1`

const pruneRollupSQL = `DELETE FROM ops_metric_rollups WHERE bucket_start < $1`

// NewPostgresStore creates a store bound to one opaque configured node ID.
func NewPostgresStore(db *sql.DB, nodeID string) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("operations metric database is required")
	}
	if err := ValidateNodeID(nodeID); err != nil {
		return nil, fmt.Errorf("operations metric node id: %w", err)
	}
	return &PostgresStore{db: db, nodeID: nodeID, now: time.Now}, nil
}

// WriteSamples validates and writes one timestamped sample batch atomically.
func (store *PostgresStore) WriteSamples(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	if err := validateSampleBatch(samples); err != nil {
		return err
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operations metric sample transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statement, err := tx.PrepareContext(ctx, writeSampleSQL)
	if err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("prepare operations metric sample insert: %w", err))
	}

	timestamp := store.sampleTimestamp()
	for _, sample := range samples {
		if _, execErr := statement.ExecContext(ctx, store.nodeID, sample.Key, timestamp, sample.Value); execErr != nil {
			operationErr := fmt.Errorf("write metric sample %q: %w", sample.Key, execErr)
			if closeErr := statement.Close(); closeErr != nil {
				operationErr = errors.Join(operationErr, fmt.Errorf("close operations metric sample statement: %w", closeErr))
			}
			return rollbackStoreTransaction(tx, operationErr)
		}
	}

	if err := statement.Close(); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("close operations metric sample statement: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("commit operations metric sample transaction: %w", err))
	}
	return nil
}

// Maintain rolls up completed hours before pruning either retention tier.
func (store *PostgresStore) Maintain(ctx context.Context, now time.Time) error {
	if err := store.rollup(ctx, now); err != nil {
		return err
	}
	return store.prune(ctx, now.Add(-rawSampleRetention), now.Add(-rollupRetention))
}

func (store *PostgresStore) rollup(ctx context.Context, completedBefore time.Time) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operations metric rollup transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, rollupSQL, completedHour(completedBefore)); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("roll up operations metrics: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("commit operations metric rollup transaction: %w", err))
	}
	return nil
}

func (store *PostgresStore) prune(ctx context.Context, rawBefore, rollupBefore time.Time) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operations metric prune transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, pruneRawSQL, rawBefore.UTC()); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("prune raw operations metrics: %w", err))
	}
	if _, err := tx.ExecContext(ctx, pruneRollupSQL, rollupBefore.UTC()); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("prune operations metric rollups: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return rollbackStoreTransaction(tx, fmt.Errorf("commit operations metric prune transaction: %w", err))
	}
	return nil
}

func validateSampleBatch(samples []Sample) error {
	seen := make(map[MetricKey]struct{}, len(samples))
	for _, sample := range samples {
		if err := ValidateSample(sample); err != nil {
			return fmt.Errorf("validate operations metric sample: %w", err)
		}
		if _, exists := seen[sample.Key]; exists {
			return fmt.Errorf("duplicate operations metric sample %q", sample.Key)
		}
		seen[sample.Key] = struct{}{}
	}
	return nil
}

func completedHour(value time.Time) time.Time {
	return value.UTC().Truncate(time.Hour)
}

func (store *PostgresStore) sampleTimestamp() time.Time {
	store.timestampMu.Lock()
	defer store.timestampMu.Unlock()

	timestamp := store.now().UTC().Truncate(time.Microsecond)
	if !timestamp.After(store.lastTimestamp) {
		timestamp = store.lastTimestamp.Add(time.Microsecond)
	}
	store.lastTimestamp = timestamp
	return timestamp
}

func rollbackStoreTransaction(tx *sql.Tx, operationErr error) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return errors.Join(operationErr, fmt.Errorf("rollback operations metric transaction: %w", err))
	}
	return operationErr
}
