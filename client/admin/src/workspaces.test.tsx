import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type {
  AdminCountersResponse,
  AdminCurrentResponse,
  AdminHealthResponse,
  AdminMetricPoint,
  CounterMetricKey,
  MetricKey,
  MetricKind,
  MetricSource,
  MetricUnit,
} from "./contracts";
import {
  COUNTER_METRIC_KEYS,
  METRIC_KEYS,
  PRIMARY_METRIC_MAP,
  SERVICE_NAMES,
} from "./contracts";
import { DEFAULT_THRESHOLDS } from "./preferences";
import {
  ChangesWorkspace,
  CountersWorkspace,
  deriveCounterShare,
  HostWorkspace,
  ScalarValue,
  ServicesWorkspace,
  Status,
  TimeSeriesWorkspace,
  type PollingResource,
} from "./workspaces";
import {
  healthFixture,
  NODE_ID,
  SAMPLED_AT,
  seriesFixture,
} from "./test/fixtures";

function counters(
  values: Partial<Record<CounterMetricKey, number>>,
): AdminCountersResponse {
  return {
    node_id: NODE_ID,
    counters: Object.entries(values).map(([metricKey, value]) => ({
      metric_key: metricKey as CounterMetricKey,
      source: metricKey.startsWith("media_") ? "media" : "control",
      unit: metricKey.startsWith("media_participant_hours_")
        ? "hours"
        : metricKey === "media_egress_cumulative_bytes"
          ? "bytes"
          : "count",
      kind: "counter",
      value: value ?? 0,
      sampled_at: SAMPLED_AT,
    })),
  };
}

function definition(key: MetricKey): {
  kind: MetricKind;
  source: MetricSource;
  unit: MetricUnit;
} {
  if (key.startsWith("host_") || key.startsWith("service_")) {
    return {
      kind: "gauge",
      source: "host",
      unit: key.endsWith("_percent")
        ? "percent"
        : key.endsWith("_memory_bytes")
          ? "bytes"
          : key === "host_load_1m"
            ? "load"
            : "count",
    };
  }
  if (
    key.startsWith("http_") ||
    key.startsWith("websocket_") ||
    key.startsWith("channel_") ||
    key.startsWith("dm_") ||
    key.startsWith("ops_")
  ) {
    return {
      kind: COUNTER_METRIC_KEYS.includes(key as CounterMetricKey)
        ? "counter"
        : "gauge",
      source: "control",
      unit: "count",
    };
  }
  return {
    kind: COUNTER_METRIC_KEYS.includes(key as CounterMetricKey)
      ? "counter"
      : "gauge",
    source: "media",
    unit: key.includes("participant_hours")
      ? "hours"
      : key === "media_egress_cumulative_bytes"
        ? "bytes"
        : key.includes("egress")
          ? "bits_per_second"
          : "count",
  };
}

function fullCurrent(): AdminCurrentResponse {
  return {
    node_id: NODE_ID,
    metrics: METRIC_KEYS.map((metricKey, index): AdminMetricPoint => ({
      metric_key: metricKey,
      ...definition(metricKey),
      value:
        metricKey.endsWith("_running") || metricKey.endsWith("_healthy")
          ? 1
          : index + 1,
      sampled_at: SAMPLED_AT,
    })),
  };
}

function fullCounters(offset = 0): AdminCountersResponse {
  return counters(
    Object.fromEntries(
      COUNTER_METRIC_KEYS.map((key, index) => [
        key,
        offset + (index + 1) * 100,
      ]),
    ) as Record<CounterMetricKey, number>,
  );
}

function resource<T>(
  data: T | null,
  state: PollingResource<T>["state"] = "ready",
  retryAt: number | null = null,
): PollingResource<T> {
  return { data, retryAt, state };
}

describe("ScalarValue", () => {
  it("fails closed when a runtime non-scalar bypasses TypeScript", () => {
    const unsafe = { hiddenValue: "must-not-render" } as unknown as number;

    render(
      <ScalarValue
        label="Host CPU"
        status="normal"
        unit="percent"
        value={unsafe}
      />,
    );

    expect(screen.getAllByText("Unavailable")).toHaveLength(2);
    expect(screen.queryByText("must-not-render")).not.toBeInTheDocument();
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  });

  it("uses a native meter only for finite percentage values", () => {
    const { rerender } = render(
      <ScalarValue
        label="Host CPU"
        status="warning"
        unit="percent"
        value={72.5}
      />,
    );

    const meter = screen.getByRole("meter", { name: "Host CPU" });
    expect(meter).toHaveAttribute("min", "0");
    expect(meter).toHaveAttribute("max", "100");
    expect(meter).toHaveAttribute("value", "72.5");
    expect(screen.getByText("72.5%")).toBeVisible();

    rerender(
      <ScalarValue
        label="Memory"
        status="normal"
        unit="bytes"
        value={1_048_576}
      />,
    );
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
    expect(screen.getByText("1 MB")).toBeVisible();
  });

  it.each([
    ["load", 1.25, "1.25"],
    ["count", 1250, "1,250"],
    ["bits_per_second", 2_500_000, "2.5 Mb/s"],
    ["hours", 12.5, "12.5 h"],
  ] as const)("formats %s values as fixed text", (unit, value, expected) => {
    render(
      <ScalarValue
        label={`Metric ${unit}`}
        status="normal"
        unit={unit}
        value={value}
      />,
    );

    expect(screen.getByText(expected)).toBeVisible();
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  });
});

describe("Status", () => {
  it.each([
    ["healthy", "Healthy"],
    ["degraded", "Degraded"],
    ["stopped", "Stopped"],
    ["unknown", "Unknown"],
    ["normal", "Normal"],
    ["warning", "Warning"],
    ["critical", "Critical"],
    ["stale", "Stale"],
    ["unavailable", "Unavailable"],
  ] as const)("renders %s with a glyph and fixed label", (state, label) => {
    const { container } = render(<Status state={state} />);

    expect(screen.getByText(label)).toBeVisible();
    expect(container.querySelector("[aria-hidden='true']")).not.toBeNull();
  });
});

describe("deriveCounterShare", () => {
  const errors = ["http_client_errors_total"] as const;

  it("requires two samples and a positive request delta", () => {
    const previous = counters({
      http_client_errors_total: 10,
      http_requests_total: 100,
    });

    expect(deriveCounterShare(null, previous, errors)).toBeNull();
    expect(deriveCounterShare(previous, previous, errors)).toBeNull();
  });

  it("derives the share from monotonic deltas", () => {
    const previous = counters({
      http_client_errors_total: 10,
      http_requests_total: 100,
    });
    const current = counters({
      http_client_errors_total: 15,
      http_requests_total: 200,
    });

    expect(deriveCounterShare(previous, current, errors)).toBe(5);
  });

  it("starts a new baseline when any counter decreases", () => {
    const previous = counters({
      http_client_errors_total: 10,
      http_requests_total: 100,
    });
    const reset = counters({
      http_client_errors_total: 1,
      http_requests_total: 4,
    });

    expect(deriveCounterShare(previous, reset, errors)).toBeNull();
    expect(
      deriveCounterShare(
        reset,
        counters({
          http_client_errors_total: 2,
          http_requests_total: 14,
        }),
        errors,
      ),
    ).toBe(10);
  });

  it("rejects a decrease in any contributing error counter", () => {
    const previous = counters({
      http_client_errors_total: 10,
      http_requests_total: 100,
    });
    const current = counters({
      http_client_errors_total: 9,
      http_requests_total: 200,
    });

    expect(deriveCounterShare(previous, current, errors)).toBeNull();
  });
});

describe("HostWorkspace", () => {
  it("renders the four host homes, seven-service summary, series, and operational gauges", () => {
    const { container } = render(
      <HostWorkspace
        current={resource(fullCurrent())}
        health={resource(healthFixture())}
        series={resource(seriesFixture())}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(
      container.querySelectorAll("[data-primary-home='host']"),
    ).toHaveLength(4);
    expect(screen.getAllByRole("meter")).toHaveLength(3);
    expect(screen.getByText("Operational now")).toBeVisible();
    for (const label of [
      "WebSocket connections",
      "Media rooms",
      "Audio participants",
      "Camera publishers",
      "Media egress",
    ]) {
      expect(screen.getByText(label)).toBeVisible();
    }
    for (const service of [
      "Control plane",
      "Media plane",
      "PostgreSQL",
      "Redis",
      "NATS",
      "MinIO",
      "Coturn",
    ]) {
      expect(screen.getByText(service)).toBeVisible();
    }
    expect(screen.getByRole("img", { name: /host cpu/i })).toBeVisible();
  });

  it.each([
    ["loading", null, "Loading current metrics"],
    ["ready", { node_id: NODE_ID, metrics: [] }, "No recent host sample"],
    ["stale", fullCurrent(), "Showing the last current sample"],
    ["rate-limited", null, "Requests paused until"],
    ["error", null, "Unable to load current metrics"],
  ] as const)("renders the %s resource state", (state, data, copy) => {
    render(
      <HostWorkspace
        current={resource(
          data as AdminCurrentResponse | null,
          state,
          state === "rate-limited" ? Date.parse("2026-07-14T12:01:00Z") : null,
        )}
        health={resource(healthFixture())}
        series={resource(seriesFixture())}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(screen.getByText(new RegExp(copy, "i"))).toBeVisible();
  });
});

describe("ServicesWorkspace", () => {
  it("renders all 28 service metric homes in fixed server order", () => {
    const { container } = render(
      <ServicesWorkspace
        current={resource(fullCurrent())}
        health={resource(healthFixture())}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(
      container.querySelectorAll("[data-primary-home='services']"),
    ).toHaveLength(28);
    const rows = screen.getAllByRole("row").slice(1);
    expect(
      rows.map((row) => within(row).getByRole("rowheader").textContent),
    ).toEqual([
      "Control plane",
      "Media plane",
      "PostgreSQL",
      "Redis",
      "NATS",
      "MinIO",
      "Coturn",
    ]);
    expect(within(rows[0]).getByText("Healthy")).toBeVisible();
    expect(within(rows[0]).getByText("Normal")).toBeVisible();
  });

  it.each([
    ["loading", null, "Loading current metrics"],
    ["503/network stale", fullCurrent(), "Showing the last current sample"],
    ["400/error", null, "Unable to load current metrics"],
    ["429", null, "Requests paused until"],
  ] as const)("renders the %s current state", (label, data, copy) => {
    const state = label.startsWith("503")
      ? "stale"
      : label.startsWith("400")
        ? "error"
        : label === "429"
          ? "rate-limited"
          : "loading";
    render(
      <ServicesWorkspace
        current={resource(
          data,
          state,
          state === "rate-limited" ? Date.parse(SAMPLED_AT) + 60_000 : null,
        )}
        health={resource(healthFixture())}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(screen.getByText(new RegExp(copy, "i"))).toBeVisible();
  });

  it("renders a valid empty sample as fixed unavailable rows", () => {
    render(
      <ServicesWorkspace
        current={resource({ node_id: NODE_ID, metrics: [] })}
        health={resource(healthFixture())}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(screen.getAllByText("Unavailable").length).toBeGreaterThan(0);
    expect(screen.getAllByRole("row")).toHaveLength(8);
  });
});

describe("CountersWorkspace", () => {
  it("renders all 20 homes in four groups and labels ten monotonic values", () => {
    const { container } = render(
      <CountersWorkspace
        counters={resource(fullCounters())}
        current={resource(fullCurrent())}
        previousCounters={fullCounters(-10)}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(
      container.querySelectorAll("[data-primary-home='counters']"),
    ).toHaveLength(20);
    for (const group of [
      "Control plane",
      "Media activity",
      "Media egress",
      "Participant hours",
    ]) {
      expect(screen.getByRole("heading", { name: group })).toBeVisible();
    }
    expect(screen.getAllByText("Process lifetime")).toHaveLength(10);
    expect(screen.queryByText(/total participants/i)).not.toBeInTheDocument();
    expect(screen.getByText("HTTP client-error share")).toBeVisible();
    expect(screen.getByText("HTTP server-error share")).toBeVisible();
  });

  it.each([
    ["loading", null, "Loading counters"],
    ["503/network stale", fullCounters(), "Showing the last counter sample"],
    ["400/error", null, "Unable to load counters"],
    ["429", null, "Requests paused until"],
  ] as const)("renders the %s counter state", (label, data, copy) => {
    const state = label.startsWith("503")
      ? "stale"
      : label.startsWith("400")
        ? "error"
        : label === "429"
          ? "rate-limited"
          : "loading";
    render(
      <CountersWorkspace
        counters={resource(
          data,
          state,
          state === "rate-limited" ? Date.parse(SAMPLED_AT) + 60_000 : null,
        )}
        current={resource(fullCurrent())}
        previousCounters={null}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(screen.getByText(new RegExp(copy, "i"))).toBeVisible();
  });

  it("renders a valid empty sample as 20 fixed metric homes", () => {
    const { container } = render(
      <CountersWorkspace
        counters={resource({ node_id: NODE_ID, counters: [] })}
        current={resource({ node_id: NODE_ID, metrics: [] })}
        previousCounters={null}
        thresholds={DEFAULT_THRESHOLDS}
      />,
    );

    expect(
      container.querySelectorAll("[data-primary-home='counters']"),
    ).toHaveLength(20);
    expect(screen.getAllByText("Unavailable").length).toBeGreaterThan(0);
  });
});

describe("TimeSeriesWorkspace", () => {
  it("offers five fixed presets, every allowlisted key, and one bounded selection", () => {
    const onMetricKeyChange = vi.fn();
    const onWindowChange = vi.fn();
    render(
      <TimeSeriesWorkspace
        metricKey="host_cpu_percent"
        onMetricKeyChange={onMetricKeyChange}
        onWindowChange={onWindowChange}
        series={resource(seriesFixture())}
        window="24h"
      />,
    );

    expect(screen.getAllByRole("option")).toHaveLength(52);
    for (const preset of [
      "Host pressure",
      "HTTP traffic",
      "Realtime activity",
      "Video activity",
      "Media egress",
    ]) {
      expect(screen.getByRole("button", { name: preset })).toBeVisible();
    }

    fireEvent.click(screen.getByRole("button", { name: "HTTP traffic" }));
    expect(onMetricKeyChange).toHaveBeenCalledWith("http_requests_total");
    expect(onMetricKeyChange).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole("button", { name: "7 days" }));
    expect(onWindowChange).toHaveBeenCalledWith("7d");
  });

  it.each([
    ["loading", null, "Loading selected series"],
    ["503/network stale", seriesFixture(), "Showing the last selected series"],
    ["400/error", null, "Unable to load selected series"],
    ["429", null, "Requests paused until"],
  ] as const)("renders the %s series state", (label, data, copy) => {
    const state = label.startsWith("503")
      ? "stale"
      : label.startsWith("400")
        ? "error"
        : label === "429"
          ? "rate-limited"
          : "loading";
    render(
      <TimeSeriesWorkspace
        metricKey="host_cpu_percent"
        onMetricKeyChange={vi.fn()}
        onWindowChange={vi.fn()}
        series={resource(
          data,
          state,
          state === "rate-limited" ? Date.parse(SAMPLED_AT) + 60_000 : null,
        )}
        window="24h"
      />,
    );

    expect(screen.getByText(new RegExp(copy, "i"))).toBeVisible();
  });

  it("renders the valid empty-series message", () => {
    render(
      <TimeSeriesWorkspace
        metricKey="host_cpu_percent"
        onMetricKeyChange={vi.fn()}
        onWindowChange={vi.fn()}
        series={resource({ ...seriesFixture(), points: [] })}
        window="24h"
      />,
    );

    expect(screen.getByText("No series data available.")).toBeVisible();
  });
});

describe("ChangesWorkspace", () => {
  it("renders seven current states and tab-local consecutive-success changes", () => {
    render(
      <ChangesWorkspace
        events={[
          {
            current: "degraded",
            id: "control_plane-1",
            observedAt: SAMPLED_AT,
            previous: "healthy",
            service: "control_plane",
          },
        ]}
        health={resource(healthFixture())}
      />,
    );

    expect(screen.getByText("Observed since this tab opened")).toBeVisible();
    expect(screen.getByText("Healthy to degraded")).toBeVisible();
    for (const service of SERVICE_NAMES) {
      expect(screen.getByTestId(`health-state-${service}`)).toBeInTheDocument();
    }
  });

  it.each([
    ["loading", null, "Loading service health"],
    ["503/network stale", healthFixture(), "Showing the last service health"],
    ["400/error", null, "Unable to load service health"],
    ["429", null, "Requests paused until"],
  ] as const)("renders the %s health state", (label, data, copy) => {
    const state = label.startsWith("503")
      ? "stale"
      : label.startsWith("400")
        ? "error"
        : label === "429"
          ? "rate-limited"
          : "loading";
    render(
      <ChangesWorkspace
        events={[]}
        health={resource(
          data,
          state,
          state === "rate-limited" ? Date.parse(SAMPLED_AT) + 60_000 : null,
        )}
      />,
    );

    expect(screen.getByText(new RegExp(copy, "i"))).toBeVisible();
  });

  it("renders valid empty health and change collections without inventing data", () => {
    render(
      <ChangesWorkspace
        events={[]}
        health={resource<AdminHealthResponse>(null, "ready")}
      />,
    );

    expect(
      screen.getByText("No changes observed since this tab opened"),
    ).toBeVisible();
    expect(screen.getAllByText("Unknown")).toHaveLength(7);
  });
});

describe("primary workspace map", () => {
  it("keeps the approved 4/28/20 split totaling 52 keys", () => {
    expect(PRIMARY_METRIC_MAP.hostOverview).toHaveLength(4);
    expect(PRIMARY_METRIC_MAP.services).toHaveLength(28);
    expect([
      ...PRIMARY_METRIC_MAP.control,
      ...PRIMARY_METRIC_MAP.mediaActivity,
      ...PRIMARY_METRIC_MAP.mediaEgress,
      ...PRIMARY_METRIC_MAP.participantHours,
    ]).toHaveLength(20);
    expect(METRIC_KEYS).toHaveLength(52);
  });
});
