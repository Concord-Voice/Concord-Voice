import type {
  AdminCountersResponse,
  AdminCurrentResponse,
  AdminHealthResponse,
  AdminSeriesResponse,
} from "../contracts";

export const NODE_ID = "cvn_abcdefghijklmnop";
export const SAMPLED_AT = "2026-07-14T12:00:00Z";

export function healthFixture(): AdminHealthResponse {
  return {
    node_id: NODE_ID,
    services: [
      "control_plane",
      "media_plane",
      "postgres",
      "redis",
      "nats",
      "minio",
      "coturn",
    ].map((service) => ({
      service,
      state: "healthy",
      running: true,
      healthy: true,
      sampled_at: SAMPLED_AT,
    })) as AdminHealthResponse["services"],
  };
}

export function currentFixture(): AdminCurrentResponse {
  return {
    node_id: NODE_ID,
    metrics: [
      {
        metric_key: "host_cpu_percent",
        source: "host",
        unit: "percent",
        kind: "gauge",
        value: 12.5,
        sampled_at: SAMPLED_AT,
      },
      {
        metric_key: "http_requests_total",
        source: "control",
        unit: "count",
        kind: "counter",
        value: 42,
        sampled_at: SAMPLED_AT,
      },
    ],
  };
}

export function countersFixture(): AdminCountersResponse {
  return {
    node_id: NODE_ID,
    counters: [
      {
        metric_key: "http_requests_total",
        source: "control",
        unit: "count",
        kind: "counter",
        value: 42,
        sampled_at: SAMPLED_AT,
      },
    ],
  };
}

export function seriesFixture(
  window: "24h" | "7d" = "24h",
): AdminSeriesResponse {
  return {
    node_id: NODE_ID,
    metric: {
      metric_key: "host_cpu_percent",
      source: "host",
      unit: "percent",
      kind: "gauge",
      rollup: "average",
    },
    window,
    bucket_seconds: 3600,
    points: [
      {
        bucket_start: SAMPLED_AT,
        value: 12.5,
        minimum: 10,
        maximum: 15,
        sample_count: 3,
      },
    ],
  };
}
