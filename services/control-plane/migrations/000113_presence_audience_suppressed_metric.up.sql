-- Migration: presence_audience_suppressed_metric (up)
-- Purpose: Admit one further key into the closed operations metric catalog so
-- that a suppressed base-presence broadcast is countable rather than log-only.
--
-- Companion to PR #2975 (#1654), which moves presence audience computation off
-- the hub Run goroutine. That change fails CLOSED when the audience cannot be
-- computed -- correct for #47, since an audience you could not compute must
-- never be fanned out -- but the suppression was visible only in a log line. The
-- code's own comment describes a suppressed terminal frame as a permanent
-- privacy loss, and the semaphore constant is deliberately not env-tunable on
-- the grounds that an operator cannot see the budget. Both are arguments for a
-- counter.
--
-- Scalar and dimension-free, per [internal]rules/opsmetrics.md: no user, server,
-- channel or error dimension rides along, so the count discloses volume only.
-- In particular it does NOT distinguish WHY a broadcast was suppressed -- a
-- per-reason breakdown would be a privacy-decision discriminator of exactly the
-- kind observability.md principle 7 forbids.
--
-- Both retention-bounded tables are revalidated synchronously, matching the
-- 000091 precedent: raw samples hold 24 hours and rollups eight days, so the
-- constraint rewrite scans a bounded set.

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
        'media_participant_hours_screenshare',
        'presence_audience_suppressed_total'
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
        'media_participant_hours_screenshare',
        'presence_audience_suppressed_total'
    ));
