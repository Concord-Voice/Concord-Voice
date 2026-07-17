import { describe, expect, it } from "vitest";

import {
  ACCOUNT_ACTIVITY_METRIC_KEYS,
  COUNTER_METRIC_KEYS,
  METRIC_KEYS,
  PRIMARY_METRIC_MAP,
  ContractError,
  parseCounters,
  parseCurrent,
  parseHealth,
  parseSeries,
} from "./contracts";
import {
  countersFixture,
  currentFixture,
  healthFixture,
  seriesFixture,
} from "./test/fixtures";

function clone<T>(value: T): T {
  return structuredClone(value);
}

describe("metric catalog", () => {
  it("assigns every fixed metric key to one primary home", () => {
    expect(PRIMARY_METRIC_MAP.hostOverview).toHaveLength(4);
    expect(PRIMARY_METRIC_MAP.services).toHaveLength(28);
    expect(PRIMARY_METRIC_MAP.control).toHaveLength(7);
    expect(PRIMARY_METRIC_MAP.mediaActivity).toHaveLength(7);
    expect(PRIMARY_METRIC_MAP.mediaEgress).toHaveLength(3);
    expect(PRIMARY_METRIC_MAP.participantHours).toHaveLength(3);
    expect(PRIMARY_METRIC_MAP.usersActivity).toEqual(
      ACCOUNT_ACTIVITY_METRIC_KEYS,
    );

    const assigned = Object.values(PRIMARY_METRIC_MAP).flat();
    expect(assigned).toHaveLength(61);
    expect(new Set(assigned)).toEqual(new Set(METRIC_KEYS));
    expect(COUNTER_METRIC_KEYS).toHaveLength(11);
  });
});

describe("parseHealth", () => {
  it("reconstructs the closed seven-service response", () => {
    expect(parseHealth(healthFixture())).toEqual(healthFixture());
  });

  it.each([
    [
      "unknown root field",
      () => ({ ...healthFixture(), arbitrary: "render me" }),
    ],
    [
      "invalid node",
      () => ({ ...healthFixture(), node_id: "operator@example.com" }),
    ],
    [
      "missing service",
      () => ({
        ...healthFixture(),
        services: healthFixture().services.slice(0, 6),
      }),
    ],
    [
      "duplicate service",
      () => {
        const value = healthFixture();
        value.services[1] = clone(value.services[0]);
        return value;
      },
    ],
    [
      "unknown state",
      () => {
        const value = healthFixture() as unknown as {
          services: Array<Record<string, unknown>>;
        };
        value.services[0].state = "compromised";
        return value;
      },
    ],
    [
      "malformed timestamp",
      () => {
        const value = healthFixture();
        value.services[0].sampled_at = "not-a-time";
        return value;
      },
    ],
  ])("rejects %s", (_name, invalid) => {
    expect(() => parseHealth(invalid())).toThrow(ContractError);
  });
});

describe("parseCurrent", () => {
  it("accepts fixed finite metric points", () => {
    expect(parseCurrent(currentFixture())).toEqual(currentFixture());
  });

  it.each([
    ["unknown metric key", "metric_key", "host_owner_email"],
    ["unknown source", "source", "browser"],
    ["unknown unit", "unit", "username"],
    ["unknown kind", "kind", "histogram"],
    ["object scalar", "value", { nested: "render me" }],
    ["non-finite scalar", "value", Number.POSITIVE_INFINITY],
    ["malformed timestamp", "sampled_at", "yesterday"],
  ])("rejects %s", (_name, field, replacement) => {
    const value = currentFixture() as unknown as {
      metrics: Array<Record<string, unknown>>;
    };
    value.metrics[0][field] = replacement;
    expect(() => parseCurrent(value)).toThrow(ContractError);
  });

  it("rejects duplicate keys and arrays above the contract cap", () => {
    const duplicate = currentFixture();
    duplicate.metrics[1] = clone(duplicate.metrics[0]);
    expect(() => parseCurrent(duplicate)).toThrow(ContractError);

    const oversized = currentFixture();
    oversized.metrics = Array.from({ length: 62 }, () =>
      clone(oversized.metrics[0]),
    );
    expect(() => parseCurrent(oversized)).toThrow(ContractError);
  });
});

describe("parseCounters", () => {
  it("accepts only the eleven fixed counter identifiers", () => {
    expect(parseCounters(countersFixture())).toEqual(countersFixture());

    const gauge = countersFixture() as unknown as {
      counters: Array<Record<string, unknown>>;
    };
    gauge.counters[0].metric_key = "websocket_connections_current";
    expect(() => parseCounters(gauge)).toThrow(ContractError);
  });

  it("rejects duplicate and oversized counter arrays", () => {
    const duplicate = countersFixture();
    duplicate.counters.push(clone(duplicate.counters[0]));
    expect(() => parseCounters(duplicate)).toThrow(ContractError);

    const oversized = countersFixture();
    oversized.counters = Array.from({ length: 12 }, () =>
      clone(oversized.counters[0]),
    );
    expect(() => parseCounters(oversized)).toThrow(ContractError);
  });

  it("accepts the fixed upload counter metadata", () => {
    const value = countersFixture();
    value.counters[0] = {
      metric_key: "media_uploads_total",
      source: "control",
      unit: "count",
      kind: "counter",
      value: 4,
      sampled_at: "2026-07-14T12:00:00Z",
    };

    expect(parseCounters(value)).toEqual(value);
  });
});

describe("parseSeries", () => {
  it.each(["24h", "7d"] as const)(
    "accepts a closed %s series response",
    (window) => {
      expect(parseSeries(seriesFixture(window))).toEqual(seriesFixture(window));
    },
  );

  it.each([
    ["unknown root field", () => ({ ...seriesFixture(), label: "operator" })],
    ["invalid window", () => ({ ...seriesFixture(), window: "30d" })],
    ["invalid bucket size", () => ({ ...seriesFixture(), bucket_seconds: 60 })],
    [
      "non-finite minimum",
      () => {
        const value = seriesFixture();
        value.points[0].minimum = Number.NaN;
        return value;
      },
    ],
    [
      "nonpositive sample count",
      () => {
        const value = seriesFixture();
        value.points[0].sample_count = 0;
        return value;
      },
    ],
    [
      "fractional sample count",
      () => {
        const value = seriesFixture();
        value.points[0].sample_count = 1.5;
        return value;
      },
    ],
    [
      "mismatched metadata",
      () => {
        const value = seriesFixture();
        value.metric.source = "media";
        return value;
      },
    ],
  ])("rejects %s", (_name, invalid) => {
    expect(() => parseSeries(invalid())).toThrow(ContractError);
  });

  it("enforces the window-specific point cap", () => {
    const day = seriesFixture("24h");
    day.points = Array.from({ length: 26 }, (_, index) => ({
      ...clone(day.points[0]),
      bucket_start: new Date(
        Date.parse(day.points[0].bucket_start) + index * 3_600_000,
      ).toISOString(),
    }));
    expect(() => parseSeries(day)).toThrow(ContractError);

    const week = seriesFixture("7d");
    week.points = Array.from({ length: 170 }, (_, index) => ({
      ...clone(week.points[0]),
      bucket_start: new Date(
        Date.parse(week.points[0].bucket_start) + index * 3_600_000,
      ).toISOString(),
    }));
    expect(() => parseSeries(week)).toThrow(ContractError);
  });
});
