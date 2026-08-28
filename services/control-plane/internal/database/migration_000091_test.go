package database_test

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var migration000091MetricLiteral = regexp.MustCompile(`'(?:host|service|http|websocket|channel|dm|ops|media|registered|pending|users|active)_[a-z0-9_]+'`)

// migration000091ExpectedMetricKeys freezes the catalog as it stood when 000091
// shipped. This assertion used to compare against the LIVE opsmetrics catalog,
// which was correct only while 000091 was the newest catalog migration — the
// moment 000113 added a key, this test failed for a change it does not describe.
// Following the 000086 precedent, the newest catalog migration owns the
// live-catalog pin and older ones freeze their own list.
var migration000091ExpectedMetricKeys = []string{
	"active_sessions_current",
	"active_users_15d",
	"active_users_24h",
	"active_users_30d",
	"active_users_7d",
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
	"media_uploads_total",
	"ops_snapshot_rejections_total",
	"pending_registrations_current",
	"registered_users_current",
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
	"users_online_current",
	"websocket_connections_current",
}

func TestMigration000091_FilesAndSchemaLock(t *testing.T) {
	up := migration000091SQL(t, "up")
	down := migration000091SQL(t, "down")
	readme := migrationReadFile(t, filepath.Join("..", "..", "migrations", "README.md"))

	assert.Contains(t, up, "ADD COLUMN ops_last_active_at TIMESTAMPTZ")
	assert.Contains(t, up, "CREATE INDEX idx_users_ops_last_active_at")
	assert.Contains(t, up, "WHERE ops_last_active_at IS NOT NULL")
	assert.Contains(t, up, "DROP CONSTRAINT ops_metric_samples_metric_key_check")
	assert.Contains(t, up, "DROP CONSTRAINT ops_metric_rollups_metric_key_check")
	catalogKeys := migration000091ExpectedMetricKeys
	assert.Equal(t, catalogKeys, migration000091ConstraintMetricKeys(up, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, catalogKeys, migration000091ConstraintMetricKeys(up, "ops_metric_rollups_metric_key_check"))

	rollbackLock := strings.Index(down, "LOCK TABLE ops_metric_samples, ops_metric_rollups IN ACCESS EXCLUSIVE MODE")
	deleteRollups := strings.Index(down, "DELETE FROM ops_metric_rollups")
	deleteSamples := strings.Index(down, "DELETE FROM ops_metric_samples")
	restoreConstraint := strings.Index(down, "ADD CONSTRAINT ops_metric_samples_metric_key_check")
	require.NotEqual(t, -1, rollbackLock)
	require.NotEqual(t, -1, deleteRollups)
	require.NotEqual(t, -1, deleteSamples)
	require.NotEqual(t, -1, restoreConstraint)
	assert.Less(t, rollbackLock, deleteRollups)
	assert.Less(t, rollbackLock, deleteSamples)
	assert.Less(t, deleteRollups, restoreConstraint)
	assert.Less(t, deleteSamples, restoreConstraint)
	baselineKeys := migration000086MetricKeys(migration000086SQL(t, "up"))
	assert.Equal(t, baselineKeys, migration000091ConstraintMetricKeys(down, "ops_metric_samples_metric_key_check"))
	assert.Equal(t, baselineKeys, migration000091ConstraintMetricKeys(down, "ops_metric_rollups_metric_key_check"))
	assert.Contains(t, down, "DROP INDEX IF EXISTS idx_users_ops_last_active_at")
	assert.Contains(t, down, "DROP COLUMN IF EXISTS ops_last_active_at")
	assert.Contains(t, readme, "| 000091 | account_activity_metrics |")
}

func TestMigration000091_UpDownReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	const (
		nodeID    = "cvn_aaaaaaaaaaaaaaaa"
		metricKey = "registered_users_current"
	)

	_, err := ts.DB.ExecContext(ctx, `
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		VALUES ($1, $2, TIMESTAMPTZ '2026-07-16 12:00:00+00', 10)
	`, nodeID, metricKey)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO ops_metric_rollups (
			node_id, metric_key, bucket_start, min_value, max_value,
			avg_value, last_value, sample_count
		) VALUES (
			$1, $2, TIMESTAMPTZ '2026-07-16 12:00:00+00', 10, 10, 10, 10, 1
		)
	`, nodeID, metricKey)
	require.NoError(t, err)

	upSQL := migration000091SQL(t, "up")
	_, err = ts.DB.ExecContext(ctx, migration000091SQL(t, "down"))
	require.NoError(t, err)
	reapplied := false
	t.Cleanup(func() {
		if reapplied {
			return
		}
		_, cleanupErr := ts.DB.ExecContext(context.Background(), upSQL)
		assert.NoError(t, cleanupErr)
	})

	var sampleCount, rollupCount int
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM ops_metric_samples WHERE metric_key = $1),
			(SELECT COUNT(*) FROM ops_metric_rollups WHERE metric_key = $1)
	`, metricKey).Scan(&sampleCount, &rollupCount))
	assert.Zero(t, sampleCount)
	assert.Zero(t, rollupCount)
	assert.False(t, migration000091ColumnExists(t, ts, "ops_last_active_at"))

	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		VALUES ($1, $2, TIMESTAMPTZ '2026-07-16 13:00:00+00', 11)
	`, nodeID, metricKey)
	require.Error(t, err, "the restored 52-key constraint must reject migration 91 keys")

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err)
	reapplied = true
	assert.True(t, migration000091ColumnExists(t, ts, "ops_last_active_at"))
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO ops_metric_samples (node_id, metric_key, ts, value)
		VALUES ($1, $2, TIMESTAMPTZ '2026-07-16 14:00:00+00', 12)
	`, nodeID, metricKey)
	require.NoError(t, err, "the reapplied 61-key constraint must accept migration 91 keys")
}

func migration000091ColumnExists(t *testing.T, ts *testhelpers.TestServer, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name = 'users'
			  AND column_name = $1
		)
	`, column).Scan(&exists))
	return exists
}

func migration000091SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, filepath.Join("..", "..", "migrations", "000091_account_activity_metrics."+direction+".sql"))
}

func migration000091MetricKeys(contents string) []string {
	matches := migration000091MetricLiteral.FindAllString(contents, -1)
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

func migration000091ConstraintMetricKeys(contents, constraint string) []string {
	startMarker := "ADD CONSTRAINT " + constraint + " CHECK (metric_key IN ("
	start := strings.Index(contents, startMarker)
	if start == -1 {
		return nil
	}
	body := contents[start+len(startMarker):]
	end := strings.Index(body, "));")
	if end == -1 {
		return nil
	}
	return migration000091MetricKeys(body[:end])
}
