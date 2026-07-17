export const SERVICE_NAMES = [
  "control_plane",
  "media_plane",
  "postgres",
  "redis",
  "nats",
  "minio",
  "coturn",
] as const;

export type ServiceName = (typeof SERVICE_NAMES)[number];

const HOST_METRIC_KEYS = [
  "host_cpu_percent",
  "host_memory_percent",
  "host_disk_percent",
  "host_load_1m",
] as const;

const SERVICE_METRIC_KEYS = [
  "service_control_plane_running",
  "service_control_plane_healthy",
  "service_control_plane_cpu_percent",
  "service_control_plane_memory_bytes",
  "service_media_plane_running",
  "service_media_plane_healthy",
  "service_media_plane_cpu_percent",
  "service_media_plane_memory_bytes",
  "service_postgres_running",
  "service_postgres_healthy",
  "service_postgres_cpu_percent",
  "service_postgres_memory_bytes",
  "service_redis_running",
  "service_redis_healthy",
  "service_redis_cpu_percent",
  "service_redis_memory_bytes",
  "service_nats_running",
  "service_nats_healthy",
  "service_nats_cpu_percent",
  "service_nats_memory_bytes",
  "service_minio_running",
  "service_minio_healthy",
  "service_minio_cpu_percent",
  "service_minio_memory_bytes",
  "service_coturn_running",
  "service_coturn_healthy",
  "service_coturn_cpu_percent",
  "service_coturn_memory_bytes",
] as const;

const CONTROL_METRIC_KEYS = [
  "http_requests_total",
  "http_client_errors_total",
  "http_server_errors_total",
  "websocket_connections_current",
  "channel_messages_total",
  "dm_messages_total",
  "ops_snapshot_rejections_total",
] as const;

export const ACCOUNT_ACTIVITY_METRIC_KEYS = [
  "registered_users_current",
  "pending_registrations_current",
  "users_online_current",
  "active_sessions_current",
  "active_users_24h",
  "active_users_7d",
  "active_users_15d",
  "active_users_30d",
  "media_uploads_total",
] as const;

const MEDIA_ACTIVITY_METRIC_KEYS = [
  "media_rooms_current",
  "media_participants_audio_current",
  "media_participants_webcam_current",
  "media_participants_screenshare_current",
  "media_camera_publishers_current",
  "media_screen_publishers_current",
  "media_peak_video_publishers_per_room",
] as const;

const MEDIA_EGRESS_METRIC_KEYS = [
  "media_egress_current_bps",
  "media_egress_peak_bps",
  "media_egress_cumulative_bytes",
] as const;

const PARTICIPANT_HOUR_METRIC_KEYS = [
  "media_participant_hours_audio",
  "media_participant_hours_webcam",
  "media_participant_hours_screenshare",
] as const;

export const METRIC_KEYS = [
  ...HOST_METRIC_KEYS,
  ...SERVICE_METRIC_KEYS,
  ...CONTROL_METRIC_KEYS,
  ...ACCOUNT_ACTIVITY_METRIC_KEYS,
  ...MEDIA_ACTIVITY_METRIC_KEYS,
  ...MEDIA_EGRESS_METRIC_KEYS,
  ...PARTICIPANT_HOUR_METRIC_KEYS,
] as const;

export type MetricKey = (typeof METRIC_KEYS)[number];

export const PRIMARY_METRIC_MAP = {
  hostOverview: HOST_METRIC_KEYS,
  services: SERVICE_METRIC_KEYS,
  control: CONTROL_METRIC_KEYS,
  usersActivity: ACCOUNT_ACTIVITY_METRIC_KEYS,
  mediaActivity: MEDIA_ACTIVITY_METRIC_KEYS,
  mediaEgress: MEDIA_EGRESS_METRIC_KEYS,
  participantHours: PARTICIPANT_HOUR_METRIC_KEYS,
} as const;

export const COUNTER_METRIC_KEYS = [
  "http_requests_total",
  "http_client_errors_total",
  "http_server_errors_total",
  "channel_messages_total",
  "dm_messages_total",
  "media_uploads_total",
  "ops_snapshot_rejections_total",
  "media_egress_cumulative_bytes",
  "media_participant_hours_audio",
  "media_participant_hours_webcam",
  "media_participant_hours_screenshare",
] as const satisfies readonly MetricKey[];

export type CounterMetricKey = (typeof COUNTER_METRIC_KEYS)[number];
export type MetricSource = "host" | "control" | "media";
export type MetricUnit =
  "percent" | "count" | "bytes" | "bits_per_second" | "load" | "hours";
export type MetricKind = "gauge" | "counter";
export type RollupMode = "average" | "last";
export type HealthState = "healthy" | "degraded" | "stopped" | "unknown";
export type SeriesWindow = "24h" | "7d";

export interface AdminHealthService {
  service: ServiceName;
  state: HealthState;
  running: boolean | null;
  healthy: boolean | null;
  sampled_at: string | null;
}

export interface AdminHealthResponse {
  node_id: string;
  services: AdminHealthService[];
}

export interface AdminMetricPoint {
  metric_key: MetricKey;
  source: MetricSource;
  unit: MetricUnit;
  kind: MetricKind;
  value: number;
  sampled_at: string;
}

export interface AdminCurrentResponse {
  node_id: string;
  metrics: AdminMetricPoint[];
}

export interface AdminCounterPoint {
  metric_key: CounterMetricKey;
  source: MetricSource;
  unit: MetricUnit;
  kind: "counter";
  value: number;
  sampled_at: string;
}

export interface AdminCountersResponse {
  node_id: string;
  counters: AdminCounterPoint[];
}

export interface AdminSeriesMetric {
  metric_key: MetricKey;
  source: MetricSource;
  unit: MetricUnit;
  kind: MetricKind;
  rollup: RollupMode;
}

export interface AdminSeriesPoint {
  bucket_start: string;
  value: number;
  minimum: number;
  maximum: number;
  sample_count: number;
}

export interface AdminSeriesResponse {
  node_id: string;
  metric: AdminSeriesMetric;
  window: SeriesWindow;
  bucket_seconds: 3600;
  points: AdminSeriesPoint[];
}

export class ContractError extends Error {
  constructor() {
    super("Invalid Admin Portal response");
    this.name = "ContractError";
  }
}

const metricKeySet = new Set<string>(METRIC_KEYS);
const counterMetricKeySet = new Set<string>(COUNTER_METRIC_KEYS);
const controlMetricKeySet = new Set<string>([
  ...CONTROL_METRIC_KEYS,
  ...ACCOUNT_ACTIVITY_METRIC_KEYS,
]);
const serviceNameSet = new Set<string>(SERVICE_NAMES);
const sources = new Set<string>(["host", "control", "media"]);
const units = new Set<string>([
  "percent",
  "count",
  "bytes",
  "bits_per_second",
  "load",
  "hours",
]);
const kinds = new Set<string>(["gauge", "counter"]);
const rollups = new Set<string>(["average", "last"]);
const healthStates = new Set<string>([
  "healthy",
  "degraded",
  "stopped",
  "unknown",
]);
const windows = new Set<string>(["24h", "7d"]);
const nodePattern = /^cvn_[a-z2-7]{16}$/;

interface MetricDefinition {
  source: MetricSource;
  unit: MetricUnit;
  kind: MetricKind;
  rollup: RollupMode;
}

function invalid(): never {
  throw new ContractError();
}

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) invalid();
  return value as Record<string, unknown>;
}

function exactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): void {
  const expectedSet = new Set(expected);
  const actual = Object.keys(value);
  if (
    actual.length !== expected.length ||
    actual.some((key) => !expectedSet.has(key))
  )
    invalid();
}

function array(value: unknown, maximum: number, exact?: number): unknown[] {
  if (
    !Array.isArray(value) ||
    value.length > maximum ||
    (exact !== undefined && value.length !== exact)
  ) {
    invalid();
  }
  return value;
}

function fixedString<T extends string>(
  value: unknown,
  allowed: Set<string>,
): T {
  if (typeof value !== "string" || !allowed.has(value)) invalid();
  return value as T;
}

function finite(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value)) invalid();
  return value;
}

function timestamp(value: unknown): string {
  if (typeof value !== "string" || !Number.isFinite(Date.parse(value)))
    invalid();
  return value;
}

function nullableTimestamp(value: unknown): string | null {
  return value === null ? null : timestamp(value);
}

function nullableBoolean(value: unknown): boolean | null {
  if (value === null || typeof value === "boolean") return value;
  return invalid();
}

function nodeID(value: unknown): string {
  if (typeof value !== "string" || !nodePattern.test(value)) invalid();
  return value;
}

function metricKey(value: unknown): MetricKey {
  return fixedString<MetricKey>(value, metricKeySet);
}

function serviceMetricDefinition(key: MetricKey): MetricDefinition {
  if (key.endsWith("_running") || key.endsWith("_healthy")) {
    return { source: "host", unit: "count", kind: "gauge", rollup: "last" };
  }
  if (key.endsWith("_cpu_percent")) {
    return {
      source: "host",
      unit: "percent",
      kind: "gauge",
      rollup: "average",
    };
  }
  return { source: "host", unit: "bytes", kind: "gauge", rollup: "average" };
}

function hostMetricDefinition(key: MetricKey): MetricDefinition {
  return {
    source: "host",
    unit: key === "host_load_1m" ? "load" : "percent",
    kind: "gauge",
    rollup: "average",
  };
}

function controlMetricDefinition(key: MetricKey): MetricDefinition {
  const counter = counterMetricKeySet.has(key);
  return {
    source: "control",
    unit: "count",
    kind: counter ? "counter" : "gauge",
    rollup: counter ? "last" : "average",
  };
}

function mediaMetricDefinition(key: MetricKey): MetricDefinition {
  if (key === "media_egress_current_bps") {
    return {
      source: "media",
      unit: "bits_per_second",
      kind: "gauge",
      rollup: "average",
    };
  }
  if (key === "media_egress_peak_bps") {
    return {
      source: "media",
      unit: "bits_per_second",
      kind: "gauge",
      rollup: "last",
    };
  }
  if (key === "media_egress_cumulative_bytes") {
    return { source: "media", unit: "bytes", kind: "counter", rollup: "last" };
  }
  if (key.startsWith("media_participant_hours_")) {
    return { source: "media", unit: "hours", kind: "counter", rollup: "last" };
  }
  return {
    source: "media",
    unit: "count",
    kind: "gauge",
    rollup: key === "media_peak_video_publishers_per_room" ? "last" : "average",
  };
}

function definitionFor(key: MetricKey): MetricDefinition {
  if (key.startsWith("service_")) return serviceMetricDefinition(key);
  if (key.startsWith("host_")) return hostMetricDefinition(key);
  if (controlMetricKeySet.has(key)) return controlMetricDefinition(key);
  return mediaMetricDefinition(key);
}

function parseHealthService(value: unknown): AdminHealthService {
  const item = record(value);
  exactKeys(item, ["service", "state", "running", "healthy", "sampled_at"]);
  return {
    service: fixedString<ServiceName>(item.service, serviceNameSet),
    state: fixedString<HealthState>(item.state, healthStates),
    running: nullableBoolean(item.running),
    healthy: nullableBoolean(item.healthy),
    sampled_at: nullableTimestamp(item.sampled_at),
  };
}

function parseMetricPoint(value: unknown): AdminMetricPoint {
  const item = record(value);
  exactKeys(item, [
    "metric_key",
    "source",
    "unit",
    "kind",
    "value",
    "sampled_at",
  ]);
  const key = metricKey(item.metric_key);
  const definition = definitionFor(key);
  const point: AdminMetricPoint = {
    metric_key: key,
    source: fixedString<MetricSource>(item.source, sources),
    unit: fixedString<MetricUnit>(item.unit, units),
    kind: fixedString<MetricKind>(item.kind, kinds),
    value: finite(item.value),
    sampled_at: timestamp(item.sampled_at),
  };
  if (
    point.source !== definition.source ||
    point.unit !== definition.unit ||
    point.kind !== definition.kind
  ) {
    invalid();
  }
  return point;
}

function unique<T>(values: T[], key: (value: T) => string): T[] {
  const seen = new Set<string>();
  for (const value of values) {
    const id = key(value);
    if (seen.has(id)) invalid();
    seen.add(id);
  }
  return values;
}

export function parseHealth(value: unknown): AdminHealthResponse {
  const response = record(value);
  exactKeys(response, ["node_id", "services"]);
  const services = unique(
    array(response.services, 7, 7).map(parseHealthService),
    (item) => item.service,
  );
  if (
    SERVICE_NAMES.some(
      (service) => !services.some((item) => item.service === service),
    )
  )
    invalid();
  return { node_id: nodeID(response.node_id), services };
}

export function parseCurrent(value: unknown): AdminCurrentResponse {
  const response = record(value);
  exactKeys(response, ["node_id", "metrics"]);
  const metrics = unique(
    array(response.metrics, 61).map(parseMetricPoint),
    (item) => item.metric_key,
  );
  return { node_id: nodeID(response.node_id), metrics };
}

export function parseCounters(value: unknown): AdminCountersResponse {
  const response = record(value);
  exactKeys(response, ["node_id", "counters"]);
  const counters = unique(
    array(response.counters, 11).map((value): AdminCounterPoint => {
      const point = parseMetricPoint(value);
      if (
        !counterMetricKeySet.has(point.metric_key) ||
        point.kind !== "counter"
      )
        invalid();
      return {
        ...point,
        metric_key: point.metric_key as CounterMetricKey,
        kind: "counter",
      };
    }),
    (item) => item.metric_key,
  );
  return { node_id: nodeID(response.node_id), counters };
}

function parseSeriesMetric(value: unknown): AdminSeriesMetric {
  const item = record(value);
  exactKeys(item, ["metric_key", "source", "unit", "kind", "rollup"]);
  const key = metricKey(item.metric_key);
  const definition = definitionFor(key);
  const metric: AdminSeriesMetric = {
    metric_key: key,
    source: fixedString<MetricSource>(item.source, sources),
    unit: fixedString<MetricUnit>(item.unit, units),
    kind: fixedString<MetricKind>(item.kind, kinds),
    rollup: fixedString<RollupMode>(item.rollup, rollups),
  };
  if (
    metric.source !== definition.source ||
    metric.unit !== definition.unit ||
    metric.kind !== definition.kind ||
    metric.rollup !== definition.rollup
  ) {
    invalid();
  }
  return metric;
}

function parseSeriesPoint(value: unknown): AdminSeriesPoint {
  const item = record(value);
  exactKeys(item, [
    "bucket_start",
    "value",
    "minimum",
    "maximum",
    "sample_count",
  ]);
  const sampleCount = finite(item.sample_count);
  if (!Number.isInteger(sampleCount) || sampleCount < 1) invalid();
  return {
    bucket_start: timestamp(item.bucket_start),
    value: finite(item.value),
    minimum: finite(item.minimum),
    maximum: finite(item.maximum),
    sample_count: sampleCount,
  };
}

export function parseSeries(value: unknown): AdminSeriesResponse {
  const response = record(value);
  exactKeys(response, [
    "node_id",
    "metric",
    "window",
    "bucket_seconds",
    "points",
  ]);
  const window = fixedString<SeriesWindow>(response.window, windows);
  if (response.bucket_seconds !== 3600) invalid();
  const maximum = window === "24h" ? 25 : 169;
  const points = unique(
    array(response.points, maximum).map(parseSeriesPoint),
    (item) => item.bucket_start,
  );
  return {
    node_id: nodeID(response.node_id),
    metric: parseSeriesMetric(response.metric),
    window,
    bucket_seconds: 3600,
    points,
  };
}
