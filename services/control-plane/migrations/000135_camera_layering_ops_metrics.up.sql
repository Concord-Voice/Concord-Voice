-- Migration: camera_layering_ops_metrics (up)
-- Purpose: Admit two further keys into the closed operations metric catalog so
-- that two consequences #3094 accepted IN WRITING become observable rather than
-- only recorded.
--
--   media_camera_layering_gate_flips_total
--     #3094 let a single overloaded viewer's `pressureStepDown: true` reach
--     computeCameraLayeringGate, where it is one disjunct of the ROOM-WIDE gate
--     condition. A gate transition broadcasts `camera-layering-gate`, which every
--     member answers with fastReproduceCamera(). The design accepted that, and
--     said so precisely "so the first production gate flap attributable to one
--     viewer's decoder is diagnosed, not investigated from scratch". A note
--     cannot do that; a flap rate can.
--
--   media_camera_pressure_demands_total
--     #3094's accepted consequence 5 was "production blindness": no aggregate
--     witness that yellow started emitting or that red still escalates. Before
--     that change NO client ever sent the flag, so a non-zero value here is
--     itself proof the activated path is live.
--
-- Scalar and dimension-free, per [internal]rules/opsmetrics.md. No room, user or
-- channel rides along; deliberately no direction on the flip counter and no
-- depth on the demand counter. An on/off split says nothing a flap rate does
-- not, and depth is not observable at this seam (the wire flag is boolean), so
-- inferring it from the requested spatialLayer would reintroduce a render-state
-- dimension this catalog does not carry. Neither counter discriminates a branch
-- of a deliberately-uniform refusal, so observability.md principle 7 holds.
--
-- Both retention-bounded tables are revalidated synchronously, matching the
-- 000091 and 000113 precedent: raw samples hold 24 hours and rollups eight days,
-- so the constraint rewrite scans a bounded set.

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
        'media_camera_pressure_demands_total'
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
        'media_camera_pressure_demands_total'
    ));
