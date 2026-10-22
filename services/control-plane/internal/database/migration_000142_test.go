package database_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration000142_AdmitsAndRollsBackTerminalOutboxMetrics(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000142_server_voice_terminal_outbox_ops_metrics.up.sql")
	down := migrationReadFile(t, "../../migrations/000142_server_voice_terminal_outbox_ops_metrics.down.sql")
	keys := []string{
		"server_voice_terminal_outbox_captured_total",
		"server_voice_terminal_outbox_delivered_total",
		"server_voice_terminal_outbox_successor_suppressed_total",
		"server_voice_terminal_outbox_channel_suppressed_total",
		"server_voice_terminal_outbox_lock_retained_total",
		"server_voice_terminal_outbox_queue_rescheduled_total",
	}
	catalogKeys := migration000142FrozenKeys()
	require.Len(t, catalogKeys, 72)
	upSamples := migration000136ConstraintKeys(up, "ops_metric_samples_metric_key_check")
	upRollups := migration000136ConstraintKeys(up, "ops_metric_rollups_metric_key_check")
	require.Len(t, upSamples, len(catalogKeys), "migration 142 must parse a non-empty key set")
	require.Equal(t, catalogKeys, upSamples)
	require.Equal(t, catalogKeys, upRollups)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	_, err = tx.Exec(down)
	require.NoError(t, err)
	downDefinition := migration000142ConstraintDefinition(t, tx)
	for _, key := range keys {
		require.NotContains(t, downDefinition, key, "down migration must remove %q", key)
	}
	downRollupDefinition := migration000142ConstraintDefinitionNamed(t, tx, "ops_metric_rollups_metric_key_check")
	for _, key := range keys {
		require.NotContains(t, downRollupDefinition, key, "down migration must remove %q from rollups", key)
	}

	_, err = tx.Exec(up)
	require.NoError(t, err)
	upDefinition := migration000142ConstraintDefinition(t, tx)
	for _, key := range keys {
		require.Contains(t, upDefinition, key, "up migration must admit %q", key)
	}
	upRollupDefinition := migration000142ConstraintDefinitionNamed(t, tx, "ops_metric_rollups_metric_key_check")
	for _, key := range catalogKeys {
		require.Contains(t, upRollupDefinition, key, "up migration must admit %q in rollups", key)
	}
}

func migration000142ConstraintDefinition(t *testing.T, tx *sql.Tx) string {
	return migration000142ConstraintDefinitionNamed(t, tx, "ops_metric_samples_metric_key_check")
}

func migration000142ConstraintDefinitionNamed(t *testing.T, tx *sql.Tx, name string) string {
	t.Helper()
	var definition string
	row := tx.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name)
	require.NoError(t, row.Scan(&definition))
	return strings.ToLower(definition)
}

// migration000142FrozenKeys is the catalog AS OF 000142 -- the exact 72 keys this
// migration's own CHECK admits, sorted.
//
// Deliberately a literal rather than a read of opsmetrics.Catalog(). The live
// catalog is now larger and will keep growing; this SQL file is finished and
// will not. Deriving it would assert that a file written earlier already
// contains every key added since, which is false by construction (000183 admits
// two more). Nothing may be added to this list. A new catalog key belongs in
// the newest migration's test, which owns the pin.
func migration000142FrozenKeys() []string {
	return []string{
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
		"media_camera_layering_gate_flips_total",
		"media_camera_pressure_demands_total",
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
		"presence_audience_suppressed_total",
		"presence_ttl_lapsed_total",
		"registered_users_current",
		"server_voice_terminal_outbox_captured_total",
		"server_voice_terminal_outbox_channel_suppressed_total",
		"server_voice_terminal_outbox_delivered_total",
		"server_voice_terminal_outbox_lock_retained_total",
		"server_voice_terminal_outbox_queue_rescheduled_total",
		"server_voice_terminal_outbox_successor_suppressed_total",
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
		"websocket_abnormal_closes_total",
		"websocket_connections_current",
	}
}
