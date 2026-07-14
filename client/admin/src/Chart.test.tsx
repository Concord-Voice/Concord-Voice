import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

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
      point("2026-07-14T10:00:00Z", 12.5, 10, 15, 3),
      point("2026-07-14T11:00:00Z", 18, 11, 20, 4),
      point("2026-07-14T12:00:00Z", 14, 12, 16, 5),
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
      screen.getByText("Latest: 14%; minimum: 10%; maximum: 20%."),
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
    ).toEqual(["Bucket time", "Value", "Minimum", "Maximum", "Sample count"]);
    expect(
      within(within(table).getAllByRole("row")[1])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual(["2026-07-14T10:00:00Z", "12.5", "10", "15", "3"]);
    expect(
      within(within(table).getAllByRole("row")[2])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual(["2026-07-14T11:00:00Z", "18", "11", "20", "4"]);
    expect(
      within(within(table).getAllByRole("row")[3])
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual(["2026-07-14T12:00:00Z", "14", "12", "16", "5"]);
  });

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
