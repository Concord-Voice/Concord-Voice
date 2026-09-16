-- Migration: presence_liveness_ops_metrics (up)
-- Purpose: Admit two further keys into the closed operations metric catalog so
-- that the two failure modes #3328 separated become countable rather than only
-- described in a design document.
--
--   presence_ttl_lapsed_total
--     Counts heartbeats that arrived to find presence:<uuid> already gone. That
--     is the renderer-throttling case #3328 exists to contain, and this is the
--     witness for the grace band that change widened from a deterministic 120s
--     to 210-240s. Without it, "did the widening actually buy headroom?" is
--     unanswerable except by waiting for user reports. It also measures the
--     latch fix shipping alongside it: before that fix a lapse was terminal for
--     the connection, so the same event had a far worse consequence than it does
--     now, and only a rate makes that difference visible.
--
--   websocket_abnormal_closes_total
--     Counts sockets that died without a clean close handshake -- the 1006 shape
--     the Cloudflare edge produced before #3328 gave the server its own
--     unsolicited keepalive. Paired with the counter above it separates two
--     failure modes that are indistinguishable from a support ticket: a
--     transport the edge is reaping, versus a renderer too throttled to hold its
--     own presence. Diagnosing either one by staring at the other is how the
--     originating incident was misread as a message-queue performance bug.
--
-- Scalar and dimension-free, per [internal]rules/opsmetrics.md. No user, no
-- channel, no close code. The omissions are load-bearing rather than incidental:
-- a close CODE would partition users by why their connection died, and splitting
-- the lapse counter by OUTCOME -- restored versus fail-closed -- would be a
-- privacy-decision discriminator under observability.md principle 7, because
-- restorable versus fenced is exactly the branch the #2444/#2461 offline fence
-- exists to keep indistinguishable.
--
-- Both retention-bounded tables are revalidated synchronously, matching the
-- 000091, 000113 and 000135 precedent: raw samples hold 24 hours and rollups
-- eight days, so the constraint rewrite scans a bounded set.

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
        'presence_audience_suppressed_total',
        'media_camera_layering_gate_flips_total',
        'media_camera_pressure_demands_total',
        'presence_ttl_lapsed_total',
        'websocket_abnormal_closes_total'
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
        'presence_audience_suppressed_total',
        'media_camera_layering_gate_flips_total',
        'media_camera_pressure_demands_total',
        'presence_ttl_lapsed_total',
        'websocket_abnormal_closes_total'
    ));
