package opsmetrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	maxSeriesReadWindow = 7 * 24 * time.Hour
	maxSeriesBuckets    = 169
)

// Point is one newest persisted scalar for a catalogued metric key.
type Point struct {
	NodeID    string
	Key       MetricKey
	Value     float64
	SampledAt time.Time
}

// Bucket is one hourly aggregate used to build a public series response.
type Bucket struct {
	NodeID      string
	Key         MetricKey
	BucketStart time.Time
	Minimum     float64
	Maximum     float64
	Average     float64
	Last        float64
	SampleCount int
}

// Reader exposes the bounded read seam consumed by the admin metrics API.
type Reader interface {
	Latest(ctx context.Context, nodeID string, notBefore time.Time) ([]Point, error)
	Series(ctx context.Context, nodeID string, key MetricKey, start, end time.Time) ([]Bucket, error)
}

// PostgresReader reads aggregate metrics through its injected restricted pool.
type PostgresReader struct {
	db *sql.DB
}

const latestMetricsSQL = `
	SELECT DISTINCT ON (metric_key)
		node_id,
		metric_key,
		value,
		ts
	FROM ops_metric_samples
	WHERE node_id = $1
	  AND ts >= $2
	ORDER BY metric_key, ts DESC
	LIMIT $3
`

const metricSeriesSQL = `
	WITH raw_buckets AS (
		SELECT
			node_id,
			metric_key,
			date_trunc('hour', ts, 'UTC') AS bucket_start,
			MIN(value) AS min_value,
			MAX(value) AS max_value,
			AVG(value) AS avg_value,
			(ARRAY_AGG(value ORDER BY ts DESC))[1] AS last_value,
			COUNT(*)::INTEGER AS sample_count,
			2 AS source_priority
		FROM ops_metric_samples
		WHERE node_id = $1
		  AND metric_key = $2
		  AND ts >= $3
		  AND ts < $4
		GROUP BY node_id, metric_key, date_trunc('hour', ts, 'UTC')
	), candidates AS (
		SELECT
			node_id,
			metric_key,
			bucket_start,
			min_value,
			max_value,
			avg_value,
			last_value,
			sample_count,
			1 AS source_priority
		FROM ops_metric_rollups
		WHERE node_id = $1
		  AND metric_key = $2
		  AND bucket_start >= date_trunc('hour', $3::TIMESTAMPTZ, 'UTC')
		  AND bucket_start < $4

		UNION ALL

		SELECT * FROM raw_buckets
	), ranked AS (
		SELECT
			*,
			ROW_NUMBER() OVER (
				PARTITION BY node_id, metric_key, bucket_start
				ORDER BY source_priority
			) AS candidate_rank
		FROM candidates
	)
	SELECT
		node_id,
		metric_key,
		bucket_start,
		min_value,
		max_value,
		avg_value,
		last_value,
		sample_count
	FROM ranked
	WHERE candidate_rank = 1
	ORDER BY bucket_start
	LIMIT $5
`

// NewPostgresReader creates a bounded reader over a restricted database pool.
func NewPostgresReader(db *sql.DB) (*PostgresReader, error) {
	if db == nil {
		return nil, errors.New("operations metrics reader database is required")
	}
	return &PostgresReader{db: db}, nil
}

// Latest returns the newest fresh sample for each catalogued key on one node.
func (reader *PostgresReader) Latest(ctx context.Context, nodeID string, notBefore time.Time) ([]Point, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateNodeID(nodeID); err != nil {
		return nil, fmt.Errorf("operations metrics latest node: %w", err)
	}
	if notBefore.IsZero() {
		return nil, errors.New("operations metrics latest lower bound is required")
	}

	rows, err := reader.db.QueryContext(ctx, latestMetricsSQL, nodeID, notBefore.UTC(), CatalogSize())
	if err != nil {
		return nil, fmt.Errorf("query latest operations metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	points := make([]Point, 0, CatalogSize())
	for rows.Next() {
		var point Point
		if err := rows.Scan(&point.NodeID, &point.Key, &point.Value, &point.SampledAt); err != nil {
			return nil, fmt.Errorf("scan latest operations metric: %w", err)
		}
		if err := validateReadPoint(point, nodeID, notBefore); err != nil {
			return nil, err
		}
		point.SampledAt = point.SampledAt.UTC()
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest operations metrics: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close latest operations metrics rows: %w", err)
	}
	return points, nil
}

// Series returns at most 169 hourly buckets for one node and catalog key.
func (reader *PostgresReader) Series(ctx context.Context, nodeID string, key MetricKey, start, end time.Time) ([]Bucket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSeriesRead(nodeID, key, start, end); err != nil {
		return nil, err
	}

	start = start.UTC()
	end = end.UTC()
	rows, err := reader.db.QueryContext(ctx, metricSeriesSQL, nodeID, key, start, end, maxSeriesBuckets)
	if err != nil {
		return nil, fmt.Errorf("query operations metric series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	buckets := make([]Bucket, 0, maxSeriesBuckets)
	for rows.Next() {
		var bucket Bucket
		if err := rows.Scan(
			&bucket.NodeID,
			&bucket.Key,
			&bucket.BucketStart,
			&bucket.Minimum,
			&bucket.Maximum,
			&bucket.Average,
			&bucket.Last,
			&bucket.SampleCount,
		); err != nil {
			return nil, fmt.Errorf("scan operations metric series bucket: %w", err)
		}
		if err := validateReadBucket(bucket, nodeID, key, start, end); err != nil {
			return nil, err
		}
		bucket.BucketStart = bucket.BucketStart.UTC()
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operations metric series: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close operations metric series rows: %w", err)
	}
	return buckets, nil
}

func validateReadPoint(point Point, requestedNode string, notBefore time.Time) error {
	if point.NodeID != requestedNode {
		return errors.New("operations metrics latest returned an unexpected node")
	}
	definition, exists := Definition(point.Key)
	if !exists {
		return errors.New("operations metrics latest returned an unknown key")
	}
	if err := ValidateSample(Sample{Key: point.Key, Value: point.Value, Source: definition.Source}); err != nil {
		return fmt.Errorf("validate latest operations metric: %w", err)
	}
	if point.SampledAt.Before(notBefore) {
		return errors.New("operations metrics latest returned a stale point")
	}
	return nil
}

func validateSeriesRead(nodeID string, key MetricKey, start, end time.Time) error {
	if err := ValidateNodeID(nodeID); err != nil {
		return fmt.Errorf("operations metrics series node: %w", err)
	}
	if _, exists := Definition(key); !exists {
		return errors.New("operations metrics series key is not catalogued")
	}
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return errors.New("operations metrics series range is invalid")
	}
	if end.Sub(start) > maxSeriesReadWindow {
		return errors.New("operations metrics series range exceeds seven days")
	}
	return nil
}

func validateReadBucket(bucket Bucket, requestedNode string, requestedKey MetricKey, start, end time.Time) error {
	if bucket.NodeID != requestedNode || bucket.Key != requestedKey {
		return errors.New("operations metrics series returned an unexpected dimension")
	}
	definition, exists := Definition(bucket.Key)
	if !exists {
		return errors.New("operations metrics series returned an unknown key")
	}
	for _, value := range []float64{bucket.Minimum, bucket.Maximum, bucket.Average, bucket.Last} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("operations metrics series returned a non-finite value")
		}
		if err := ValidateSample(Sample{Key: bucket.Key, Value: value, Source: definition.Source}); err != nil {
			return fmt.Errorf("validate operations metric series value: %w", err)
		}
	}
	if bucket.Minimum > bucket.Average || bucket.Average > bucket.Maximum ||
		bucket.Minimum > bucket.Last || bucket.Last > bucket.Maximum || bucket.SampleCount < 1 {
		return errors.New("operations metrics series returned an invalid aggregate")
	}
	bucketStart := bucket.BucketStart.UTC()
	if !bucketStart.Equal(bucketStart.Truncate(time.Hour)) ||
		bucketStart.Before(start.Truncate(time.Hour)) || !bucketStart.Before(end) {
		return errors.New("operations metrics series returned an invalid bucket")
	}
	return nil
}
