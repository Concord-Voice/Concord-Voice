package opsmetrics

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresReaderLatestReturnsFreshNewestPointPerKey(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	reader, err := NewPostgresReader(db)
	require.NoError(t, err)
	ctx := context.Background()

	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	otherNodeID := "cvn_bbbbbbbbbbbbbbbb"
	insertReaderSample(t, db, nodeID, MetricHostCPUPercent, time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC), 5)
	insertReaderSample(t, db, nodeID, MetricHostCPUPercent, time.Date(2026, 7, 13, 11, 55, 0, 0, time.UTC), 25)
	insertReaderSample(t, db, nodeID, MetricHostMemoryPercent, time.Date(2026, 7, 13, 11, 50, 0, 0, time.UTC), 50)
	insertReaderSample(t, db, otherNodeID, MetricHostCPUPercent, time.Date(2026, 7, 13, 11, 59, 0, 0, time.UTC), 99)

	points, err := reader.Latest(ctx, nodeID, time.Date(2026, 7, 13, 11, 45, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, []Point{
		{
			NodeID: nodeID, Key: MetricHostCPUPercent, Value: 25,
			SampledAt: time.Date(2026, 7, 13, 11, 55, 0, 0, time.UTC),
		},
		{
			NodeID: nodeID, Key: MetricHostMemoryPercent, Value: 50,
			SampledAt: time.Date(2026, 7, 13, 11, 50, 0, 0, time.UTC),
		},
	}, points)

	empty, err := reader.Latest(ctx, nodeID, time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Empty(t, empty)
	assert.NotNil(t, empty)
}

func TestPostgresReaderSeriesPrefersRollupsAndFillsRawBuckets(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	reader, err := NewPostgresReader(db)
	require.NoError(t, err)
	ctx := context.Background()

	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	otherNodeID := "cvn_bbbbbbbbbbbbbbbb"
	key := MetricHostCPUPercent
	insertReaderRollup(t, db, nodeID, key, time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), 10, 30, 20, 25, 3)
	insertReaderSample(t, db, nodeID, key, time.Date(2026, 7, 13, 10, 15, 0, 0, time.UTC), 90)
	insertReaderSample(t, db, nodeID, key, time.Date(2026, 7, 13, 10, 45, 0, 0, time.UTC), 100)
	insertReaderSample(t, db, nodeID, key, time.Date(2026, 7, 13, 11, 10, 0, 0, time.UTC), 40)
	insertReaderSample(t, db, nodeID, key, time.Date(2026, 7, 13, 11, 50, 0, 0, time.UTC), 60)
	insertReaderSample(t, db, nodeID, MetricHostMemoryPercent, time.Date(2026, 7, 13, 11, 30, 0, 0, time.UTC), 80)
	insertReaderSample(t, db, otherNodeID, key, time.Date(2026, 7, 13, 11, 30, 0, 0, time.UTC), 95)

	buckets, err := reader.Series(
		ctx,
		nodeID,
		key,
		time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	assert.Equal(t, []Bucket{
		{
			NodeID: nodeID, Key: key,
			BucketStart: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
			Minimum:     10, Maximum: 30, Average: 20, Last: 25, SampleCount: 3,
		},
		{
			NodeID: nodeID, Key: key,
			BucketStart: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC),
			Minimum:     40, Maximum: 60, Average: 50, Last: 60, SampleCount: 2,
		},
	}, buckets)
}

func TestPostgresReaderSeriesOrdersFloatingPointRawAverage(t *testing.T) {
	// Regression for #2283.
	db := setupOpsMetricsIntegrationDB(t)
	reader, err := NewPostgresReader(db)
	require.NoError(t, err)
	ctx := context.Background()
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	bucketStart := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	insertIdenticalOpsMetricsIntegrationSamples(t, db, nodeID, MetricHostCPUPercent, bucketStart)
	buckets, err := reader.Series(ctx, nodeID, MetricHostCPUPercent, bucketStart, bucketStart.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.LessOrEqual(t, buckets[0].Minimum, buckets[0].Average)
	assert.LessOrEqual(t, buckets[0].Average, buckets[0].Maximum)
	assert.Equal(t, 240, buckets[0].SampleCount)
}

func TestPostgresReaderRejectsInvalidBoundsBeforeQuery(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	reader, err := NewPostgresReader(db)
	require.NoError(t, err)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	for _, testCase := range []struct {
		name   string
		nodeID string
		key    MetricKey
		start  time.Time
		end    time.Time
	}{
		{name: "invalid node", nodeID: "node-a", key: MetricHostCPUPercent, start: now.Add(-time.Hour), end: now},
		{name: "unknown key", nodeID: "cvn_aaaaaaaaaaaaaaaa", key: "custom_metric", start: now.Add(-time.Hour), end: now},
		{name: "empty range", nodeID: "cvn_aaaaaaaaaaaaaaaa", key: MetricHostCPUPercent, start: now, end: now},
		{name: "reversed range", nodeID: "cvn_aaaaaaaaaaaaaaaa", key: MetricHostCPUPercent, start: now, end: now.Add(-time.Hour)},
		{name: "oversized range", nodeID: "cvn_aaaaaaaaaaaaaaaa", key: MetricHostCPUPercent, start: now.Add(-8 * 24 * time.Hour), end: now},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			buckets, queryErr := reader.Series(context.Background(), testCase.nodeID, testCase.key, testCase.start, testCase.end)
			assert.Error(t, queryErr)
			assert.Nil(t, buckets)
		})
	}

	points, err := reader.Latest(context.Background(), "node-a", now.Add(-time.Minute))
	assert.Error(t, err)
	assert.Nil(t, points)
}

func TestPostgresReaderHonorsCanceledContext(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	reader, err := NewPostgresReader(db)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	points, err := reader.Latest(ctx, "cvn_aaaaaaaaaaaaaaaa", time.Now().Add(-time.Minute))
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, points)
}

func insertReaderSample(t *testing.T, db *sql.DB, nodeID string, key MetricKey, timestamp time.Time, value float64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		VALUES ($1, $2, $3, $4)
	`, nodeID, key, timestamp, value)
	require.NoError(t, err)
}

func insertReaderRollup(
	t *testing.T,
	db *sql.DB,
	nodeID string,
	key MetricKey,
	bucketStart time.Time,
	minimum, maximum, average, last float64,
	sampleCount int,
) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO ops_metric_rollups (
			node_id, metric_key, bucket_start, min_value, max_value,
			avg_value, last_value, sample_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, nodeID, key, bucketStart, minimum, maximum, average, last, sampleCount)
	require.NoError(t, err)
}
