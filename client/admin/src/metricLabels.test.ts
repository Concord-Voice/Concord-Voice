import { describe, expect, it } from "vitest";

import { METRIC_KEYS, SERVICE_NAMES, type MetricKey } from "./contracts";
import { METRIC_LABELS, OPERATOR_METRIC_LABELS } from "./metricLabels";
import { metricLabel } from "./workspaces";

const everyKey: readonly MetricKey[] = METRIC_KEYS;

describe("metricLabels", () => {
  // The totality assertion is the one that earns this module its keep. The
  // TYPE already forbids a missing key, but only while both halves are read by
  // a compiler -- a future refactor that widens the annotation, or a key that
  // reaches the console through some path `tsc` does not cover, would put
  // `undefined` in front of an operator with nothing failing. This fails loudly.
  it("names every key in the closed catalog, on both surfaces", () => {
    expect(everyKey.length).toBeGreaterThan(0);
    for (const key of everyKey) {
      expect(METRIC_LABELS[key], `chart label for ${key}`).toBeTruthy();
      expect(
        OPERATOR_METRIC_LABELS[key],
        `operator label for ${key}`,
      ).toBeTruthy();
    }
  });

  it("carries no key the catalog does not declare", () => {
    expect(Object.keys(METRIC_LABELS).sort()).toEqual([...everyKey].sort());
    expect(Object.keys(OPERATOR_METRIC_LABELS).sort()).toEqual(
      [...everyKey].sort(),
    );
  });

  // Inheritance is the property that replaced the second hand-maintained map:
  // every key the operator surface does NOT deliberately reword must read
  // exactly as the chart does, or the two have silently drifted again.
  it("inherits the chart wording everywhere it is not deliberately reworded", () => {
    const reworded = everyKey.filter(
      (key) => OPERATOR_METRIC_LABELS[key] !== METRIC_LABELS[key],
    );
    expect(reworded.sort()).toEqual([
      "host_load_1m",
      "media_participant_hours_screenshare",
      "media_participants_screenshare_current",
      "ops_snapshot_rejections_total",
      "presence_audience_suppressed_total",
      "presence_ttl_lapsed_total",
      "websocket_abnormal_closes_total",
    ]);
  });

  // The ordering inside `metricLabel` is load-bearing and invisible to every
  // other test: the map is total, so it HAS a `service_*` entry, and a reader
  // who moved the lookup above the derivation would flip all 28 of these from
  // "healthy" to "health" with the suite still green.
  it("derives service labels rather than reading them from the map", () => {
    for (const service of SERVICE_NAMES) {
      const healthy = `service_${service}_healthy` as MetricKey;
      expect(metricLabel(healthy)).toMatch(/ healthy$/);
      expect(METRIC_LABELS[healthy]).toMatch(/ health$/);
    }
  });

  it("reads the reworded operator labels for non-service keys", () => {
    expect(metricLabel("host_load_1m")).toBe("One-minute load");
    expect(metricLabel("presence_ttl_lapsed_total")).toBe(
      "Lapsed presence TTLs",
    );
    expect(metricLabel("websocket_abnormal_closes_total")).toBe(
      "Sockets closed abnormally",
    );
    expect(metricLabel("host_cpu_percent")).toBe("Host CPU");
  });
});
