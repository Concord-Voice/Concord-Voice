import type {
  AdminSeriesResponse,
  MetricKey,
  MetricUnit,
  SeriesWindow,
} from "./contracts";

const WIDTH = 800;
const HEIGHT = 240;
const PADDING = 16;

const METRIC_LABELS: Record<MetricKey, string> = {
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

const WINDOW_LABELS: Record<SeriesWindow, string> = {
  "24h": "24 hours",
  "7d": "7 days",
};

const UNIT_SUFFIXES: Record<MetricUnit, string> = {
  percent: "%",
  count: "",
  bytes: " bytes",
  bits_per_second: " bps",
  load: "",
  hours: " hours",
};

function formatValue(value: number, unit: MetricUnit): string {
  return `${String(value)}${UNIT_SUFFIXES[unit]}`;
}

function polylinePoints(response: AdminSeriesResponse): string {
  const values = response.points.map(({ value }) => value);
  const minimum = Math.min(...values);
  const maximum = Math.max(...values);
  const scale = Math.max(Math.abs(minimum), Math.abs(maximum), 1);
  const scaledMinimum = minimum / scale;
  const scaledRange = maximum / scale - scaledMinimum;
  const xRange = WIDTH - 2 * PADDING;
  const yRange = HEIGHT - 2 * PADDING;

  return response.points
    .map(({ value }, index) => {
      const x =
        response.points.length === 1
          ? WIDTH / 2
          : PADDING + (index / (response.points.length - 1)) * xRange;
      const normalized =
        scaledRange === 0 ? 0.5 : (value / scale - scaledMinimum) / scaledRange;
      const y = PADDING + (1 - Math.min(1, Math.max(0, normalized))) * yRange;
      return `${x},${y}`;
    })
    .join(" ");
}

export function SeriesChart({
  response,
}: Readonly<{ response: AdminSeriesResponse }>) {
  const label = METRIC_LABELS[response.metric.metric_key];
  const latestPoint = response.points.at(-1);

  if (latestPoint === undefined) {
    return (
      <section className="chart-wrap">
        <h3>{label}</h3>
        <output>No series data available.</output>
      </section>
    );
  }

  const chartTitle = `${label} over ${WINDOW_LABELS[response.window]}`;
  const latest = latestPoint.value;
  const minimum = Math.min(...response.points.map((point) => point.minimum));
  const maximum = Math.max(...response.points.map((point) => point.maximum));
  const summary = `Latest: ${formatValue(latest, response.metric.unit)}; minimum: ${formatValue(minimum, response.metric.unit)}; maximum: ${formatValue(maximum, response.metric.unit)}.`;

  return (
    <section className="chart-wrap">
      <h3>{label}</h3>
      <p className="chart-summary">{summary}</p>
      <svg
        aria-label={chartTitle}
        className="chart"
        preserveAspectRatio="none"
        role="img"
        viewBox="0 0 800 240"
      >
        <title>{chartTitle}</title>
        <polyline
          className="chart-line"
          fill="none"
          points={polylinePoints(response)}
          stroke="currentColor"
          strokeWidth="2"
          vectorEffect="non-scaling-stroke"
        />
      </svg>
      <details className="data-table">
        <summary>View accessible data table</summary>
        <table>
          <caption>{label} series data</caption>
          <thead>
            <tr>
              <th scope="col">Bucket time</th>
              <th scope="col">Value</th>
              <th scope="col">Minimum</th>
              <th scope="col">Maximum</th>
              <th scope="col">Sample count</th>
            </tr>
          </thead>
          <tbody>
            {response.points.map((point) => (
              <tr key={point.bucket_start}>
                <td>
                  <time dateTime={point.bucket_start}>
                    {point.bucket_start}
                  </time>
                </td>
                <td>{point.value}</td>
                <td>{point.minimum}</td>
                <td>{point.maximum}</td>
                <td>{point.sample_count}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </section>
  );
}
