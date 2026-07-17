import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { SeriesChart } from "./Chart";
import type { AdminSeriesPoint } from "./contracts";
import { seriesFixture } from "./test/fixtures";

function point(
  bucketStart: string,
  value: number,
  minimum = value,
  maximum = value,
  sampleCount = 1,
): AdminSeriesPoint {
  return {
    bucket_start: bucketStart,
    value,
    minimum,
    maximum,
    sample_count: sampleCount,
  };
}

describe("SeriesChart", () => {
  it("renders a fixed empty-series message", () => {
    const response = seriesFixture();
    response.points = [];

    render(<SeriesChart response={response} />);

    expect(screen.getByRole("heading", { name: "Host CPU" })).toBeVisible();
    expect(screen.getByText("No series data available.")).toBeVisible();
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("renders an accessible chart, text summary, and every point in a semantic table", () => {
    const response = seriesFixture();
    response.points = [
      point("2026-07-14T10:00:00Z", 12.3456, 10.126, 15.678, 3),
      point("2026-07-14T11:00:00Z", 18.234, 11.111, 20.999, 4),
      point("2026-07-14T12:00:00Z", 14.126, 12.555, 16.444, 5),
    ];

    render(<SeriesChart response={response} />);

    const chart = screen.getByRole("img", {
      name: "Host CPU over 24 hours",
    });
    expect(chart).toHaveAttribute("viewBox", "0 0 800 240");
    expect(chart.querySelector("title")).toHaveTextContent(
      "Host CPU over 24 hours",
    );
    expect(
      screen.getByText(
        "Latest hourly average: 14.13%; minimum: 10.13%; maximum: 21%.",
      ),
    ).toBeVisible();
    expect(screen.queryByText("host_cpu_percent")).not.toBeInTheDocument();

    const disclosure = screen.getByText("View accessible data table");
    const details = disclosure.closest("details");
    expect(details).not.toHaveAttribute("open");
    fireEvent.click(disclosure);
    expect(details).toHaveAttribute("open");

    const table = screen.getByRole("table", { name: "Host CPU series data" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((header) => header.textContent),
    ).toEqual([
      "Bucket time",
      "Hourly average",
      "Minimum",
      "Maximum",
      "Sample count",
    ]);
    expect(
      within(within(table).getAllByRole("row")[1])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual([
      "Jul 14, 2026, 10:00:00 AM UTC",
      "12.35%",
      "10.13%",
      "15.68%",
      "3",
    ]);
    expect(
      within(within(table).getAllByRole("row")[2])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual([
      "Jul 14, 2026, 11:00:00 AM UTC",
      "18.23%",
      "11.11%",
      "21%",
      "4",
    ]);
    expect(
      within(within(table).getAllByRole("row")[3])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual([
      "Jul 14, 2026, 12:00:00 PM UTC",
      "14.13%",
      "12.56%",
      "16.44%",
      "5",
    ]);
    expect(
      within(table).getAllByText(/UTC/)[0].closest("time"),
    ).toHaveAttribute("datetime", "2026-07-14T10:00:00Z");
  });

  it("labels and formats last-value rollups honestly", () => {
    const response = seriesFixture();
    response.metric = {
      metric_key: "http_requests_total",
      source: "control",
      unit: "count",
      kind: "counter",
      rollup: "last",
    };
    response.points = [
      point("2026-07-14T12:00:00Z", 1234.567, 1000, 1250, 240),
    ];

    render(<SeriesChart response={response} />);

    expect(
      screen.getByText(
        "Latest hourly value: 1,234.57; minimum: 1,000; maximum: 1,250.",
      ),
    ).toBeVisible();
    fireEvent.click(screen.getByText("View accessible data table"));
    const table = screen.getByRole("table", {
      name: "HTTP requests series data",
    });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((header) => header.textContent),
    ).toEqual([
      "Bucket time",
      "Hourly value",
      "Minimum",
      "Maximum",
      "Sample count",
    ]);
    expect(
      within(within(table).getAllByRole("row")[1])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual([
      "Jul 14, 2026, 12:00:00 PM UTC",
      "1,234.57",
      "1,000",
      "1,250",
      "240",
    ]);
  });

  it("converts display text to local time without changing the timestamp", async () => {
    vi.stubEnv("TZ", "America/New_York");
    vi.resetModules();
    try {
      const { formatTimestamp } = await import("./time");

      expect(formatTimestamp("2026-07-14T12:00:00Z", "local")).toBe(
        "Jul 14, 2026, 08:00:00 AM EDT",
      );
    } finally {
      vi.unstubAllEnvs();
      vi.resetModules();
    }
  });

  it("rejects invalid timestamps", async () => {
    const { formatTimestamp } = await import("./time");

    expect(() => formatTimestamp("not-a-timestamp", "utc")).toThrow(RangeError);
  });

  it.each([
    {
      expected: "1 GB",
      kind: "gauge",
      metricKey: "service_control_plane_memory_bytes",
      rollup: "average",
      source: "host",
      unit: "bytes",
      value: 1024 ** 3,
    },
    {
      expected: "2.5 Mb/s",
      kind: "gauge",
      metricKey: "media_egress_current_bps",
      rollup: "average",
      source: "media",
      unit: "bits_per_second",
      value: 2_500_000,
    },
    {
      expected: "12.5 h",
      kind: "counter",
      metricKey: "media_participant_hours_audio",
      rollup: "last",
      source: "media",
      unit: "hours",
      value: 12.5,
    },
  ] as const)(
    "formats $unit series values with the portal unit scale",
    ({ expected, kind, metricKey, rollup, source, unit, value }) => {
      const response = seriesFixture();
      response.metric = {
        metric_key: metricKey,
        source,
        unit,
        kind,
        rollup,
      };
      response.points = [point("2026-07-14T12:00:00Z", value)];

      render(<SeriesChart response={response} />);

      const summaryLabel =
        rollup === "average" ? "Latest hourly average" : "Latest hourly value";
      expect(
        screen.getByText(
          `${summaryLabel}: ${expected}; minimum: ${expected}; maximum: ${expected}.`,
        ),
      ).toBeVisible();
      fireEvent.click(screen.getByText("View accessible data table"));
      expect(
        within(screen.getByRole("table")).getAllByText(expected),
      ).toHaveLength(3);
    },
  );

  it.each([
    ["a single flat point", [42]],
    ["opposite finite extremes", [-Number.MAX_VALUE, Number.MAX_VALUE]],
  ])("keeps all generated coordinates finite for %s", (_name, values) => {
    const response = seriesFixture();
    response.points = values.map((value, index) =>
      point(`2026-07-14T${String(10 + index).padStart(2, "0")}:00:00Z`, value),
    );

    const { container } = render(<SeriesChart response={response} />);

    const coordinates =
      container.querySelector("polyline")?.getAttribute("points") ?? "";
    expect(coordinates).not.toBe("");
    expect(
      coordinates
        .trim()
        .split(/[\s,]+/)
        .map(Number)
        .every(Number.isFinite),
    ).toBe(true);
  });
});
