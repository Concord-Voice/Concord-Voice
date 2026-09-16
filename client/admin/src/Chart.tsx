import type {
  AdminSeriesResponse,
  RollupMode,
  SeriesWindow,
} from "./contracts";
import { formatScalar } from "./formatMetric";
import { METRIC_LABELS } from "./metricLabels";
import { formatTimestamp, type TimeMode } from "./time";

const WIDTH = 800;
const HEIGHT = 240;
const PADDING = 16;

const WINDOW_LABELS: Record<SeriesWindow, string> = {
  "24h": "24 hours",
  "7d": "7 days",
};

const ROLLUP_LABELS: Record<RollupMode, { summary: string; column: string }> = {
  average: {
    summary: "Latest hourly average",
    column: "Hourly average",
  },
  last: {
    summary: "Latest hourly value",
    column: "Hourly value",
  },
};

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
  timeMode = "utc",
}: Readonly<{ response: AdminSeriesResponse; timeMode?: TimeMode }>) {
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
  const rollupLabels = ROLLUP_LABELS[response.metric.rollup];
  const latest = latestPoint.value;
  const minimum = Math.min(...response.points.map((point) => point.minimum));
  const maximum = Math.max(...response.points.map((point) => point.maximum));
  const summary = `${rollupLabels.summary}: ${formatScalar(latest, response.metric.unit)}; minimum: ${formatScalar(minimum, response.metric.unit)}; maximum: ${formatScalar(maximum, response.metric.unit)}.`;

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
              <th scope="col">{rollupLabels.column}</th>
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
                    {formatTimestamp(point.bucket_start, timeMode)}
                  </time>
                </td>
                <td>{formatScalar(point.value, response.metric.unit)}</td>
                <td>{formatScalar(point.minimum, response.metric.unit)}</td>
                <td>{formatScalar(point.maximum, response.metric.unit)}</td>
                <td>{formatScalar(point.sample_count, "count")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </section>
  );
}
