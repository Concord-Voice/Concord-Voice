-- Migration: account_activity_metrics (up)
-- Purpose: Add a retention-minimized activity marker and expand the closed
-- operations metric catalog for aggregate account and upload telemetry.

ALTER TABLE users
    ADD COLUMN ops_last_active_at TIMESTAMPTZ;

-- The users table remains below the repository's large-table threshold during
-- Beta, and this new nullable column makes the partial index initially empty.
-- Collection also clears markers outside the 30-day window, keeping the index
-- bounded. Keep this transactional, matching the documented migration 000079
-- precedent.
CREATE INDEX idx_users_ops_last_active_at
    ON users (ops_last_active_at)
    WHERE ops_last_active_at IS NOT NULL;

-- These recently introduced tables are retention-bounded to 24 hours of raw
-- samples and eight days of rollups. Validate the expanded closed catalog
-- synchronously while those tables remain bounded.

ALTER TABLE ops_metric_samples
    DROP CONSTRAINT ops_metric_samples_metric_key_check;

ALTER TABLE ops_metric_samples
    ADD CONSTRAINT ops_metric_samples_metric_key_check CHECK (metric_key IN (
        'host_cpu_percent',
        'host_disk_percent',
        'host_load_1m',
        'host_memory_percent',
        'service_control_plane_running',
        'service_control_plane_healthy',
        'service_control_plane_cpu_percent',
        'service_control_plane_memory_bytes',
        'service_media_plane_running',
        'service_media_plane_healthy',
        'service_media_plane_cpu_percent',
        'service_media_plane_memory_bytes',
        'service_postgres_running',
        'service_postgres_healthy',
        'service_postgres_cpu_percent',
        'service_postgres_memory_bytes',
        'service_redis_running',
        'service_redis_healthy',
        'service_redis_cpu_percent',
        'service_redis_memory_bytes',
        'service_nats_running',
        'service_nats_healthy',
        'service_nats_cpu_percent',
        'service_nats_memory_bytes',
        'service_minio_running',
        'service_minio_healthy',
        'service_minio_cpu_percent',
        'service_minio_memory_bytes',
        'service_coturn_running',
        'service_coturn_healthy',
        'service_coturn_cpu_percent',
        'service_coturn_memory_bytes',
        'http_requests_total',
        'http_client_errors_total',
        'http_server_errors_total',
        'websocket_connections_current',
        'channel_messages_total',
        'dm_messages_total',
        'ops_snapshot_rejections_total',
        'registered_users_current',
        'pending_registrations_current',
        'users_online_current',
        'active_sessions_current',
        'active_users_24h',
        'active_users_7d',
        'active_users_15d',
        'active_users_30d',
        'media_uploads_total',
        'media_rooms_current',
        'media_participants_audio_current',
        'media_participants_webcam_current',
        'media_participants_screenshare_current',
        'media_camera_publishers_current',
        'media_screen_publishers_current',
        'media_peak_video_publishers_per_room',
        'media_egress_current_bps',
        'media_egress_peak_bps',
        'media_egress_cumulative_bytes',
        'media_participant_hours_audio',
        'media_participant_hours_webcam',
        'media_participant_hours_screenshare'
    ));

ALTER TABLE ops_metric_rollups
    DROP CONSTRAINT ops_metric_rollups_metric_key_check;

ALTER TABLE ops_metric_rollups
    ADD CONSTRAINT ops_metric_rollups_metric_key_check CHECK (metric_key IN (
        'host_cpu_percent',
        'host_disk_percent',
        'host_load_1m',
        'host_memory_percent',
        'service_control_plane_running',
        'service_control_plane_healthy',
        'service_control_plane_cpu_percent',
        'service_control_plane_memory_bytes',
        'service_media_plane_running',
        'service_media_plane_healthy',
        'service_media_plane_cpu_percent',
        'service_media_plane_memory_bytes',
        'service_postgres_running',
        'service_postgres_healthy',
        'service_postgres_cpu_percent',
        'service_postgres_memory_bytes',
        'service_redis_running',
        'service_redis_healthy',
        'service_redis_cpu_percent',
        'service_redis_memory_bytes',
        'service_nats_running',
        'service_nats_healthy',
        'service_nats_cpu_percent',
        'service_nats_memory_bytes',
        'service_minio_running',
        'service_minio_healthy',
        'service_minio_cpu_percent',
        'service_minio_memory_bytes',
        'service_coturn_running',
        'service_coturn_healthy',
        'service_coturn_cpu_percent',
        'service_coturn_memory_bytes',
        'http_requests_total',
        'http_client_errors_total',
        'http_server_errors_total',
        'websocket_connections_current',
        'channel_messages_total',
        'dm_messages_total',
        'ops_snapshot_rejections_total',
        'registered_users_current',
        'pending_registrations_current',
        'users_online_current',
        'active_sessions_current',
        'active_users_24h',
        'active_users_7d',
        'active_users_15d',
        'active_users_30d',
        'media_uploads_total',
        'media_rooms_current',
        'media_participants_audio_current',
        'media_participants_webcam_current',
        'media_participants_screenshare_current',
        'media_camera_publishers_current',
        'media_screen_publishers_current',
        'media_peak_video_publishers_per_room',
        'media_egress_current_bps',
        'media_egress_peak_bps',
        'media_egress_cumulative_bytes',
        'media_participant_hours_audio',
        'media_participant_hours_webcam',
        'media_participant_hours_screenshare'
    ));
