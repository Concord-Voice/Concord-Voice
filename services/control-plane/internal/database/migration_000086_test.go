package database_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const opsMetricsTestDatabaseURL = "OPS_METRICS_TEST_DATABASE_URL"

var migration000086MetricLiteral = regexp.MustCompile(`'(?:host|service|http|websocket|channel|dm|ops|media)_[a-z0-9_]+'`)

var migration000086ExpectedMetricKeys = []string{
	"channel_messages_total",
	"dm_messages_total",
	"host_cpu_percent",
	"host_disk_percent",
	"host_load_1m",
	"host_memory_percent",
	"http_client_errors_total",
	"http_requests_total",
	"http_server_errors_total",
	"media_camera_publishers_current",
	"media_egress_cumulative_bytes",
	"media_egress_current_bps",
	"media_egress_peak_bps",
	"media_participant_hours_audio",
	"media_participant_hours_screenshare",
	"media_participant_hours_webcam",
	"media_participants_audio_current",
	"media_participants_screenshare_current",
	"media_participants_webcam_current",
	"media_peak_video_publishers_per_room",
	"media_rooms_current",
	"media_screen_publishers_current",
	"ops_snapshot_rejections_total",
	"service_control_plane_cpu_percent",
	"service_control_plane_healthy",
	"service_control_plane_memory_bytes",
	"service_control_plane_running",
	"service_coturn_cpu_percent",
	"service_coturn_healthy",
	"service_coturn_memory_bytes",
	"service_coturn_running",
	"service_media_plane_cpu_percent",
	"service_media_plane_healthy",
	"service_media_plane_memory_bytes",
	"service_media_plane_running",
	"service_minio_cpu_percent",
	"service_minio_healthy",
	"service_minio_memory_bytes",
	"service_minio_running",
	"service_nats_cpu_percent",
	"service_nats_healthy",
	"service_nats_memory_bytes",
	"service_nats_running",
	"service_postgres_cpu_percent",
	"service_postgres_healthy",
	"service_postgres_memory_bytes",
	"service_postgres_running",
	"service_redis_cpu_percent",
	"service_redis_healthy",
	"service_redis_memory_bytes",
	"service_redis_running",
	"websocket_connections_current",
}

func TestMigration000086_FilesAndSchemaLock(t *testing.T) {
	up := migration000086SQL(t, "up")
	down := migration000086SQL(t, "down")
	readme := migrationReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	assert.Contains(t, up, "CREATE TABLE ops_metric_samples")
	assert.Contains(t, up, "CREATE TABLE ops_metric_rollups")
	assert.Contains(t, up, "USING BRIN (ts)")
	assert.Contains(t, up, "ops_metric_samples_metric_key_check")
	assert.Contains(t, up, "ops_metric_rollups_metric_key_check")
	assert.Contains(t, up, "ops_metric_samples_node_id_check")
	assert.Contains(t, up, "ops_metric_rollups_node_id_check")
	assert.Contains(t, up, "ops_metric_samples_value_finite_check")
	assert.Contains(t, up, "ops_metric_rollups_values_finite_check")
	assert.Contains(t, up, "date_trunc('hour', bucket_start, 'UTC')")
	assert.Equal(t, migration000086ExpectedMetricKeys, migration000086MetricKeys(up))

	rollupsDrop := strings.Index(down, "DROP TABLE IF EXISTS ops_metric_rollups")
	samplesDrop := strings.Index(down, "DROP TABLE IF EXISTS ops_metric_samples")
	require.NotEqual(t, -1, rollupsDrop)
	require.NotEqual(t, -1, samplesDrop)
	assert.Less(t, rollupsDrop, samplesDrop, "down migration must drop rollups before samples")
	assert.Contains(t, readme, "| 000086 | ops_metrics | Aggregate-only operations samples and hourly rollups (#1689) |")
}

func TestMigration000086_IntegrationSchemaConstraintsAndSymmetry(t *testing.T) {
	db := migration000086OpenIsolatedDB(t)
	ctx := context.Background()
	down := migration000086SQL(t, "down")
	up := migration000086SQL(t, "up")

	_, err := db.ExecContext(ctx, down)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.ExecContext(context.Background(), down)
		assert.NoError(t, cleanupErr)
		assert.NoError(t, db.Close())
	})

	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"metric_key:text:NO",
		"node_id:text:NO",
		"ts:timestamp with time zone:NO",
		"value:double precision:NO",
	}, migration000086Columns(t, db, "ops_metric_samples"))
	assert.Equal(t, []string{
		"avg_value:double precision:NO",
		"bucket_start:timestamp with time zone:NO",
		"last_value:double precision:NO",
		"max_value:double precision:NO",
		"metric_key:text:NO",
		"min_value:double precision:NO",
		"node_id:text:NO",
		"sample_count:integer:NO",
	}, migration000086Columns(t, db, "ops_metric_rollups"))
	assert.Equal(t, "node_id,metric_key,ts", migration000086PrimaryKey(t, db, "ops_metric_samples"))
	assert.Equal(t, "node_id,metric_key,bucket_start", migration000086PrimaryKey(t, db, "ops_metric_rollups"))
	assert.Equal(t, migration000086ExpectedMetricKeys, migration000086ConstraintKeys(t, db, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, migration000086ExpectedMetricKeys, migration000086ConstraintKeys(t, db, "ops_metric_rollups_metric_key_check"))

	var indexMethod string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT access_method.amname
		FROM pg_class AS index_relation
		JOIN pg_am AS access_method ON access_method.oid = index_relation.relam
		WHERE index_relation.relname = 'idx_ops_metric_samples_ts_brin'
	`).Scan(&indexMethod))
	assert.Equal(t, "brin", indexMethod)

	const validNodeID = "cvn_aaaaaaaaaaaaaaaa"
	_, err = db.ExecContext(ctx, `
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		VALUES ($1, $2, '2026-07-12T20:00:00Z', 12.5)
	`, validNodeID, opsmetrics.MetricHostCPUPercent)
	require.NoError(t, err)

	for _, testCase := range []struct {
		name       string
		nodeID     string
		metricKey  string
		value      string
		constraint string
	}{
		{name: "uuid node", nodeID: "550e8400-e29b-41d4-a716-446655440000", metricKey: string(opsmetrics.MetricHostCPUPercent), value: "1", constraint: "ops_metric_samples_node_id_check"},
		{name: "ip node", nodeID: "192.0.2.10", metricKey: string(opsmetrics.MetricHostCPUPercent), value: "1", constraint: "ops_metric_samples_node_id_check"},
		{name: "hostname node", nodeID: "api.concordvoice.chat", metricKey: string(opsmetrics.MetricHostCPUPercent), value: "1", constraint: "ops_metric_samples_node_id_check"},
		{name: "unknown metric", nodeID: validNodeID, metricKey: "custom_metric", value: "1", constraint: "ops_metric_samples_metric_key_check"},
		{name: "nan value", nodeID: validNodeID, metricKey: string(opsmetrics.MetricHostCPUPercent), value: "NaN", constraint: "ops_metric_samples_value_finite_check"},
		{name: "infinite value", nodeID: validNodeID, metricKey: string(opsmetrics.MetricHostCPUPercent), value: "Infinity", constraint: "ops_metric_samples_value_finite_check"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, insertErr := db.ExecContext(ctx, `
				INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
				VALUES ($1, $2, '2026-07-12T20:01:00Z', $3::DOUBLE PRECISION)
			`, testCase.nodeID, testCase.metricKey, testCase.value)
			migration000086RequireConstraint(t, insertErr, testCase.constraint)
		})
	}

	for _, testCase := range []struct {
		name        string
		nodeID      string
		metricKey   string
		bucketStart string
		minValue    string
		maxValue    string
		avgValue    string
		lastValue   string
		sampleCount int
		constraint  string
	}{
		{name: "invalid node", nodeID: "api.concordvoice.chat", metricKey: string(opsmetrics.MetricHostCPUPercent), bucketStart: "2026-07-12T20:00:00Z", minValue: "1", maxValue: "1", avgValue: "1", lastValue: "1", sampleCount: 1, constraint: "ops_metric_rollups_node_id_check"},
		{name: "unknown metric", nodeID: validNodeID, metricKey: "custom_metric", bucketStart: "2026-07-12T20:00:00Z", minValue: "1", maxValue: "1", avgValue: "1", lastValue: "1", sampleCount: 1, constraint: "ops_metric_rollups_metric_key_check"},
		{name: "unaligned bucket", nodeID: validNodeID, metricKey: string(opsmetrics.MetricHostCPUPercent), bucketStart: "2026-07-12T20:01:00Z", minValue: "1", maxValue: "1", avgValue: "1", lastValue: "1", sampleCount: 1, constraint: "ops_metric_rollups_bucket_start_check"},
		{name: "non-finite value", nodeID: validNodeID, metricKey: string(opsmetrics.MetricHostCPUPercent), bucketStart: "2026-07-12T20:00:00Z", minValue: "NaN", maxValue: "NaN", avgValue: "NaN", lastValue: "NaN", sampleCount: 1, constraint: "ops_metric_rollups_values_finite_check"},
		{name: "invalid value order", nodeID: validNodeID, metricKey: string(opsmetrics.MetricHostCPUPercent), bucketStart: "2026-07-12T20:00:00Z", minValue: "3", maxValue: "4", avgValue: "2", lastValue: "3", sampleCount: 1, constraint: "ops_metric_rollups_value_order_check"},
		{name: "empty sample count", nodeID: validNodeID, metricKey: string(opsmetrics.MetricHostCPUPercent), bucketStart: "2026-07-12T20:00:00Z", minValue: "1", maxValue: "1", avgValue: "1", lastValue: "1", sampleCount: 0, constraint: "ops_metric_rollups_sample_count_check"},
	} {
		t.Run("rollup "+testCase.name, func(t *testing.T) {
			_, insertErr := db.ExecContext(ctx, `
				INSERT INTO ops_metric_rollups (
					node_id, metric_key, bucket_start, min_value, max_value,
					avg_value, last_value, sample_count
				) VALUES ($1, $2, $3::TIMESTAMPTZ, $4::DOUBLE PRECISION,
					$5::DOUBLE PRECISION, $6::DOUBLE PRECISION,
					$7::DOUBLE PRECISION, $8)
			`, testCase.nodeID, testCase.metricKey, testCase.bucketStart,
				testCase.minValue, testCase.maxValue, testCase.avgValue,
				testCase.lastValue, testCase.sampleCount)
			migration000086RequireConstraint(t, insertErr, testCase.constraint)
		})
	}

	_, err = db.ExecContext(ctx, down)
	require.NoError(t, err)
	assert.False(t, migration000086TableExists(t, db, "ops_metric_samples"))
	assert.False(t, migration000086TableExists(t, db, "ops_metric_rollups"))
	_, err = db.ExecContext(ctx, up)
	require.NoError(t, err, "migration must re-apply after rollback")
}

func migration000086OpenIsolatedDB(t *testing.T) *sql.DB {
	t.Helper()

	databaseURL := os.Getenv(opsMetricsTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is unset; skipping isolated PostgreSQL migration test", opsMetricsTestDatabaseURL)
	}
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	require.Contains(t, strings.TrimPrefix(parsed.Path, "/"), "opsmetrics", "database name must identify an isolated opsmetrics database")

	db, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	return db
}

func migration000086Columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()

	rows, err := db.Query(`
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		ORDER BY column_name
	`, table)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()

	var columns []string
	for rows.Next() {
		var name, dataType, nullable string
		require.NoError(t, rows.Scan(&name, &dataType, &nullable))
		columns = append(columns, name+":"+dataType+":"+nullable)
	}
	require.NoError(t, rows.Err())
	return columns
}

func migration000086PrimaryKey(t *testing.T, db *sql.DB, table string) string {
	t.Helper()

	var columns string
	require.NoError(t, db.QueryRow(`
		SELECT string_agg(attribute.attname, ',' ORDER BY key_column.ordinality)
		FROM pg_constraint AS constraint_definition
		JOIN pg_class AS relation ON relation.oid = constraint_definition.conrelid
		CROSS JOIN LATERAL unnest(constraint_definition.conkey)
			WITH ORDINALITY AS key_column(attnum, ordinality)
		JOIN pg_attribute AS attribute
			ON attribute.attrelid = relation.oid AND attribute.attnum = key_column.attnum
		WHERE relation.relname = $1 AND constraint_definition.contype = 'p'
	`, table).Scan(&columns))
	return columns
}

func migration000086ConstraintKeys(t *testing.T, db *sql.DB, constraint string) []string {
	t.Helper()

	var definition string
	require.NoError(t, db.QueryRow(`
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conname = $1
	`, constraint).Scan(&definition))
	return migration000086MetricKeys(definition)
}

func migration000086MetricKeys(contents string) []string {
	matches := migration000086MetricLiteral.FindAllString(contents, -1)
	keys := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		keys[strings.Trim(match, "'")] = struct{}{}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func migration000086RequireConstraint(t *testing.T, err error, name string) {
	t.Helper()
	require.Error(t, err)

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected PostgreSQL constraint error, got %T", err)
	assert.Equal(t, name, pqErr.Constraint)
}

func migration000086TableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var relation sql.NullString
	require.NoError(t, db.QueryRow(`SELECT to_regclass('public.' || $1)`, table).Scan(&relation))
	return relation.Valid
}

func migration000086SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, filepath.Join("..", "..", "migrations", "000086_ops_metrics."+direction+".sql"))
}

func migrationReadFile(t *testing.T, relativePath string) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(filename), relativePath)
	// #nosec G304 -- path is based on runtime.Caller and fixed test-owned filenames.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
