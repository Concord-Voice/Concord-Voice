-- Migration: presence_audience_suppressed_metric (down)
-- Purpose: Narrow the closed metric catalog back to its 61-key form.
--
-- Rows carrying the retired key must be removed BEFORE the constraint is
-- re-added, or the ALTER fails with Postgres's opaque check-violation and the
-- rollback is undiagnosable at 3am. Both tables are retention-bounded (24h raw,
-- 8d rollups) so this deletes at most a few days of a single scalar series --
-- aggregate counts, never identity data.

-- ACCESS EXCLUSIVE before the deletes, matching the 000091 precedent. Without
-- it a collector still running the pre-rollback binary can INSERT a row
-- carrying the retired key between the DELETE and the constraint re-add, and
-- the ALTER then fails on data this migration believed it had removed.
LOCK TABLE ops_metric_samples, ops_metric_rollups IN ACCESS EXCLUSIVE MODE;

DELETE FROM ops_metric_samples WHERE metric_key = 'presence_audience_suppressed_total';
DELETE FROM ops_metric_rollups WHERE metric_key = 'presence_audience_suppressed_total';

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
