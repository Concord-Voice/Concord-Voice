package opsmetrics

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storeIntegrationDatabaseURL = "OPS_METRICS_TEST_DATABASE_URL"

const floatingPointRollupSample = "0.008333333333333333"

func TestPostgresStoreIntegrationConcurrentWrites(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	store := newOpsMetricsIntegrationStore(t, db)

	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }

	const writeCount = 12
	errorsByWrite := make(chan error, writeCount)
	var workers sync.WaitGroup
	workers.Add(writeCount)
	for index := 0; index < writeCount; index++ {
		go func(value float64) {
			defer workers.Done()
			errorsByWrite <- store.WriteSamples(context.Background(), []Sample{{
				Key: MetricHostCPUPercent, Value: value, Source: SourceHost,
			}})
		}(float64(index + 1))
	}
	workers.Wait()
	close(errorsByWrite)
	for err := range errorsByWrite {
		require.NoError(t, err)
	}

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ops_metric_samples`).Scan(&count))
	assert.Equal(t, writeCount, count)
}

func TestPostgresStoreIntegrationRollupIdempotencyAndPrune(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	store := newOpsMetricsIntegrationStore(t, db)
	ctx := context.Background()

	writeOpsMetricsIntegrationSample(t, store, time.Date(2026, 7, 12, 10, 5, 0, 0, time.UTC), 2)
	writeOpsMetricsIntegrationSample(t, store, time.Date(2026, 7, 12, 10, 45, 0, 0, time.UTC), 8)
	writeOpsMetricsIntegrationSample(t, store, time.Date(2026, 7, 12, 11, 5, 0, 0, time.UTC), 4)

	require.NoError(t, store.rollup(ctx, time.Date(2026, 7, 12, 11, 37, 0, 0, time.UTC)))
	assert.Equal(t, 1, opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_rollups`))

	var minValue, maxValue, avgValue, lastValue float64
	var sampleCount int
	require.NoError(t, db.QueryRow(`
		SELECT min_value, max_value, avg_value, last_value, sample_count
		FROM ops_metric_rollups
		WHERE metric_key = $1 AND bucket_start = $2
	`, MetricHostCPUPercent, time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)).Scan(
		&minValue, &maxValue, &avgValue, &lastValue, &sampleCount,
	))
	assert.Equal(t, 2.0, minValue)
	assert.Equal(t, 8.0, maxValue)
	assert.Equal(t, 5.0, avgValue)
	assert.Equal(t, 8.0, lastValue)
	assert.Equal(t, 2, sampleCount)

	require.NoError(t, store.prune(
		ctx,
		time.Date(2026, 7, 12, 10, 30, 0, 0, time.UTC),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	))
	assert.Equal(t, 2, opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_samples`))

	require.NoError(t, store.rollup(ctx, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)))
	require.NoError(t, store.rollup(ctx, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)))
	assert.Equal(t, 2, opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_rollups`))
	require.NoError(t, db.QueryRow(`
		SELECT min_value, max_value, avg_value, last_value, sample_count
		FROM ops_metric_rollups
		WHERE metric_key = $1 AND bucket_start = $2
	`, MetricHostCPUPercent, time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)).Scan(
		&minValue, &maxValue, &avgValue, &lastValue, &sampleCount,
	))
	assert.Equal(t, 2.0, minValue)
	assert.Equal(t, 8.0, maxValue)
	assert.Equal(t, 5.0, avgValue)
	assert.Equal(t, 8.0, lastValue)
	assert.Equal(t, 2, sampleCount, "a partially pruned raw bucket must not overwrite its finalized rollup")

	require.NoError(t, store.prune(
		ctx,
		time.Date(2026, 7, 12, 11, 37, 0, 0, time.UTC),
		time.Date(2026, 7, 12, 11, 0, 0, 0, time.UTC),
	))
	assert.Equal(t, 0, opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_samples`))
	assert.Equal(t, 1, opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_rollups`))
}

func TestPostgresStoreIntegrationMaintainOrdersFloatingPointRollup(t *testing.T) {
	// Regression for #2283.
	db := setupOpsMetricsIntegrationDB(t)
	store := newOpsMetricsIntegrationStore(t, db)
	ctx := context.Background()
	bucketStart := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	insertIdenticalOpsMetricsIntegrationSamples(t, db, store.nodeID, MetricHostCPUPercent, bucketStart)
	require.NoError(t, store.Maintain(ctx, bucketStart.Add(2*time.Hour)))

	var minimum, maximum, average float64
	var sampleCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT min_value, max_value, avg_value, sample_count
		FROM ops_metric_rollups
		WHERE node_id = $1 AND metric_key = $2 AND bucket_start = $3
	`, store.nodeID, MetricHostCPUPercent, bucketStart).Scan(
		&minimum, &maximum, &average, &sampleCount,
	))
	assert.LessOrEqual(t, minimum, average)
	assert.LessOrEqual(t, average, maximum)
	assert.Equal(t, 240, sampleCount)
}

func TestPostgresStoreIntegrationRollupBucketsAreUTCIndependentOfSessionTimezone(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	db.SetMaxOpenConns(1)
	require.NoError(t, db.Ping())
	require.NoError(t, func() error {
		_, err := db.Exec(`SET TIME ZONE 'Asia/Kathmandu'`)
		return err
	}())

	store := newOpsMetricsIntegrationStore(t, db)
	writeOpsMetricsIntegrationSample(t, store, time.Date(2026, 7, 12, 10, 5, 0, 0, time.UTC), 2)
	require.NoError(t, store.rollup(context.Background(), time.Date(2026, 7, 12, 11, 0, 0, 0, time.UTC)))

	var bucket time.Time
	require.NoError(t, db.QueryRow(`SELECT bucket_start FROM ops_metric_rollups`).Scan(&bucket))
	assert.Equal(t, time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC), bucket.UTC())
}

func TestPostgresStoreIntegrationRollbackAndFailedRollupRetention(t *testing.T) {
	db := setupOpsMetricsIntegrationDB(t)
	store := newOpsMetricsIntegrationStore(t, db)
	ctx := context.Background()
	timestamp := time.Date(2026, 7, 12, 10, 5, 0, 0, time.UTC)
	store.now = func() time.Time { return timestamp }

	_, err := db.ExecContext(ctx, `
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		VALUES ($1, $2, $3, 50)
	`, store.nodeID, MetricHostMemoryPercent, timestamp)
	require.NoError(t, err)

	err = store.WriteSamples(ctx, []Sample{
		{Key: MetricHostCPUPercent, Value: 25, Source: SourceHost},
		{Key: MetricHostMemoryPercent, Value: 50, Source: SourceHost},
	})
	require.ErrorContains(t, err, "write metric sample")
	assert.Equal(t, 0, opsMetricsIntegrationRowCount(t, db,
		`SELECT COUNT(*) FROM ops_metric_samples WHERE metric_key = 'host_cpu_percent'`))
	assert.Equal(t, 1, opsMetricsIntegrationRowCount(t, db,
		`SELECT COUNT(*) FROM ops_metric_samples WHERE metric_key = 'host_memory_percent'`))

	_, err = db.ExecContext(ctx, `
		CREATE FUNCTION ops_metrics_test_fail_rollup() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'forced rollup failure';
		END;
		$$;
		CREATE TRIGGER ops_metrics_test_fail_rollup
		BEFORE INSERT ON ops_metric_rollups
		FOR EACH ROW EXECUTE FUNCTION ops_metrics_test_fail_rollup();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS ops_metrics_test_fail_rollup() CASCADE`)
		assert.NoError(t, cleanupErr)
	})

	rawCountBefore := opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_samples`)
	err = store.Maintain(ctx, time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC))
	require.ErrorContains(t, err, "roll up operations metrics")
	assert.Equal(t, rawCountBefore, opsMetricsIntegrationRowCount(t, db, `SELECT COUNT(*) FROM ops_metric_samples`),
		"failed rollup must leave raw samples available for retry")
}

func setupOpsMetricsIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()

	databaseURL := os.Getenv(storeIntegrationDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping isolated PostgreSQL store integration test", storeIntegrationDatabaseURL)
	}
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	require.Contains(t, strings.TrimPrefix(parsed.Path, "/"), "opsmetrics", "database name must identify an isolated opsmetrics database")

	db, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	db.SetMaxOpenConns(16)

	down := readOpsMetricsIntegrationMigration(t, "down")
	up := readOpsMetricsIntegrationMigration(t, "up")
	_, err = db.Exec(down)
	require.NoError(t, err)
	_, err = db.Exec(up)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, cleanupErr := db.Exec(down)
		assert.NoError(t, cleanupErr)
		assert.NoError(t, db.Close())
	})
	return db
}

func newOpsMetricsIntegrationStore(t *testing.T, db *sql.DB) *PostgresStore {
	t.Helper()
	store, err := NewPostgresStore(db, "cvn_aaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	return store
}

func writeOpsMetricsIntegrationSample(t *testing.T, store *PostgresStore, timestamp time.Time, value float64) {
	t.Helper()
	store.now = func() time.Time { return timestamp }
	require.NoError(t, store.WriteSamples(context.Background(), []Sample{{
		Key: MetricHostCPUPercent, Value: value, Source: SourceHost,
	}}))
}

func insertIdenticalOpsMetricsIntegrationSamples(
	t *testing.T,
	db *sql.DB,
	nodeID string,
	key MetricKey,
	bucketStart time.Time,
) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		SELECT $1, $2, $3::TIMESTAMPTZ + sample_number * INTERVAL '1 second',
		       $4::DOUBLE PRECISION
		FROM generate_series(1, 240) AS sample_number
	`, nodeID, key, bucketStart, floatingPointRollupSample)
	require.NoError(t, err)
}

func opsMetricsIntegrationRowCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(query).Scan(&count))
	return count
}

func readOpsMetricsIntegrationMigration(t *testing.T, direction string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "migrations", "000086_ops_metrics."+direction+".sql")
	// #nosec G304 -- path is based on runtime.Caller and a fixed migration filename.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
