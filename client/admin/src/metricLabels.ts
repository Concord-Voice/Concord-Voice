import type { MetricKey } from "./contracts";

// The one label map for the closed operations-metrics catalog.
//
// It lives here rather than beside either consumer because BOTH the chart and
// the workspace tables need a name for every key, and two hand-maintained maps
// over one closed key set is the drift hazard `[internal]rules/opsmetrics.md`
// warns about -- the workspace copy was `Partial`, so a key added to the
// catalog and to `contracts.ts` but forgotten here rendered "Unknown metric"
// with nothing failing. `Record<MetricKey, string>` is TOTAL, so the same
// omission is now a type error at the next `tsc --noEmit`.

export const METRIC_LABELS: Record<MetricKey, string> = {
  host_cpu_percent: "Host CPU",
  host_memory_percent: "Host memory",
  host_disk_percent: "Host disk",
  host_load_1m: "One-minute host load",
  service_control_plane_running: "Control plane running",
  service_control_plane_healthy: "Control plane health",
  service_control_plane_cpu_percent: "Control plane CPU",
  service_control_plane_memory_bytes: "Control plane memory",
  service_media_plane_running: "Media plane running",
  service_media_plane_healthy: "Media plane health",
  service_media_plane_cpu_percent: "Media plane CPU",
  service_media_plane_memory_bytes: "Media plane memory",
  service_postgres_running: "PostgreSQL running",
  service_postgres_healthy: "PostgreSQL health",
  service_postgres_cpu_percent: "PostgreSQL CPU",
  service_postgres_memory_bytes: "PostgreSQL memory",
  service_redis_running: "Redis running",
  service_redis_healthy: "Redis health",
  service_redis_cpu_percent: "Redis CPU",
  service_redis_memory_bytes: "Redis memory",
  service_nats_running: "NATS running",
  service_nats_healthy: "NATS health",
  service_nats_cpu_percent: "NATS CPU",
  service_nats_memory_bytes: "NATS memory",
  service_minio_running: "MinIO running",
  service_minio_healthy: "MinIO health",
  service_minio_cpu_percent: "MinIO CPU",
  service_minio_memory_bytes: "MinIO memory",
  service_coturn_running: "Coturn running",
  service_coturn_healthy: "Coturn health",
  service_coturn_cpu_percent: "Coturn CPU",
  service_coturn_memory_bytes: "Coturn memory",
  http_requests_total: "HTTP requests",
  http_client_errors_total: "HTTP client errors",
  http_server_errors_total: "HTTP server errors",
  websocket_connections_current: "WebSocket connections",
  channel_messages_total: "Channel messages",
  dm_messages_total: "Direct messages",
  ops_snapshot_rejections_total: "Operations snapshot rejections",
  presence_audience_suppressed_total: "Presence broadcast suppressions",
  presence_ttl_lapsed_total: "Presence TTL lapses",
  websocket_abnormal_closes_total: "Abnormal socket closes",
  server_voice_terminal_outbox_captured_total:
    "Server Voice terminal outbox captures",
  server_voice_terminal_outbox_delivered_total:
    "Server Voice terminal Hub admissions",
  server_voice_terminal_outbox_successor_suppressed_total:
    "Server Voice terminal successor suppressions",
  server_voice_terminal_outbox_channel_suppressed_total:
    "Server Voice terminal channel suppressions",
  server_voice_terminal_outbox_lock_retained_total:
    "Server Voice terminal lock retentions",
  server_voice_terminal_outbox_queue_rescheduled_total:
    "Server Voice terminal queue reschedules",
  media_camera_layering_gate_flips_total: "Camera layering gate flips",
  media_camera_pressure_demands_total: "Camera pressure demands",
  registered_users_current: "Registered users",
  pending_registrations_current: "Pending registrations",
  users_online_current: "Users online",
  active_sessions_current: "Active sessions",
  active_users_24h: "Active users over 24 hours",
  active_users_7d: "Active users over 7 days",
  active_users_15d: "Active users over 15 days",
  active_users_30d: "Active users over 30 days",
  media_uploads_total: "Media uploads",
  media_rooms_current: "Media rooms",
  media_participants_audio_current: "Audio participants",
  media_participants_webcam_current: "Webcam participants",
  media_participants_screenshare_current: "Screen-share participants",
  media_camera_publishers_current: "Camera publishers",
  media_screen_publishers_current: "Screen publishers",
  media_peak_video_publishers_per_room: "Peak video publishers per room",
  media_egress_current_bps: "Current media egress",
  media_egress_peak_bps: "Peak media egress",
  media_egress_cumulative_bytes: "Cumulative media egress",
  media_participant_hours_audio: "Audio participant hours",
  media_participant_hours_webcam: "Webcam participant hours",
  media_participant_hours_screenshare: "Screen-share participant hours",
};

// The workspace tables address an operator scanning a column of rows, so a
// handful of keys read better noun-first there than they do as a chart title.
// Only the keys whose wording DELIBERATELY differs appear below; everything
// else is inherited, which is the whole point of deriving rather than copying.
//
// Note what is NOT here: the 28 `service_*` keys. `metricLabel` derives those
// from `SERVICE_LABELS` plus the metric suffix and never reaches this map for
// them -- so it renders "Control plane healthy" where the chart says "Control
// plane health". Moving the map lookup ahead of that derivation would silently
// rewrite seven rendered strings.
export const OPERATOR_METRIC_LABELS: Record<MetricKey, string> = {
  ...METRIC_LABELS,
  host_load_1m: "One-minute load",
  ops_snapshot_rejections_total: "Rejected operations snapshots",
  presence_audience_suppressed_total: "Suppressed presence broadcasts",
  presence_ttl_lapsed_total: "Lapsed presence TTLs",
  websocket_abnormal_closes_total: "Sockets closed abnormally",
  media_participants_screenshare_current: "Screenshare participants",
  media_participant_hours_screenshare: "Screenshare participant hours",
};
