import type {
  AdminCountersResponse,
  AdminCurrentResponse,
  CounterMetricKey,
  HealthState,
  AdminHealthResponse,
  AdminSeriesResponse,
  MetricKey,
  MetricUnit,
  SeriesWindow,
  ServiceName,
} from "./contracts";
import {
  COUNTER_METRIC_KEYS,
  METRIC_KEYS,
  PRIMARY_METRIC_MAP,
  SERVICE_NAMES,
} from "./contracts";
import { SeriesChart } from "./Chart";
import type { ThresholdStatus, Thresholds } from "./preferences";
import { statusFor } from "./preferences";
import type { PollingState } from "./usePolling";

export type DisplayStatus =
  HealthState | ThresholdStatus | "stale" | "unavailable";

export interface PollingResource<T> {
  data: T | null;
  retryAt: number | null;
  state: PollingState;
}

export interface HealthChange {
  current: HealthState;
  id: string;
  observedAt: string;
  previous: HealthState;
  service: ServiceName;
}

const STATUS_COPY: Record<DisplayStatus, { glyph: string; label: string }> = {
  healthy: { glyph: "✓", label: "Healthy" },
  degraded: { glyph: "!", label: "Degraded" },
  stopped: { glyph: "×", label: "Stopped" },
  unknown: { glyph: "?", label: "Unknown" },
  normal: { glyph: "✓", label: "Normal" },
  warning: { glyph: "!", label: "Warning" },
  critical: { glyph: "×", label: "Critical" },
  stale: { glyph: "↻", label: "Stale" },
  unavailable: { glyph: "?", label: "Unavailable" },
};

const numberFormatter = new Intl.NumberFormat("en-US", {
  maximumFractionDigits: 2,
});

export const SERVICE_LABELS: Record<ServiceName, string> = {
  control_plane: "Control plane",
  media_plane: "Media plane",
  postgres: "PostgreSQL",
  redis: "Redis",
  nats: "NATS",
  minio: "MinIO",
  coturn: "Coturn",
};

const HOST_FIELDS = [
  {
    key: "host_cpu_percent",
    label: "Host CPU",
    threshold: "hostCpu",
    unit: "percent",
  },
  {
    key: "host_memory_percent",
    label: "Host memory",
    threshold: "hostMemory",
    unit: "percent",
  },
  {
    key: "host_disk_percent",
    label: "Host disk",
    threshold: "hostDisk",
    unit: "percent",
  },
  {
    key: "host_load_1m",
    label: "One-minute load",
    threshold: null,
    unit: "load",
  },
] as const;

const OPERATIONAL_NOW = [
  ["websocket_connections_current", "WebSocket connections"],
  ["media_rooms_current", "Media rooms"],
  ["media_participants_audio_current", "Audio participants"],
  ["media_camera_publishers_current", "Camera publishers"],
  ["media_egress_current_bps", "Media egress"],
] as const satisfies readonly (readonly [MetricKey, string])[];

const COUNTER_GROUPS = [
  {
    label: "Control plane",
    keys: PRIMARY_METRIC_MAP.control,
  },
  {
    label: "Media activity",
    keys: PRIMARY_METRIC_MAP.mediaActivity,
  },
  {
    label: "Media egress",
    keys: PRIMARY_METRIC_MAP.mediaEgress,
  },
  {
    label: "Participant hours",
    keys: PRIMARY_METRIC_MAP.participantHours,
  },
] as const;

const METRIC_COPY: Partial<Record<MetricKey, string>> = {
  host_cpu_percent: "Host CPU",
  host_memory_percent: "Host memory",
  host_disk_percent: "Host disk",
  host_load_1m: "One-minute load",
  http_requests_total: "HTTP requests",
  http_client_errors_total: "HTTP client errors",
  http_server_errors_total: "HTTP server errors",
  websocket_connections_current: "WebSocket connections",
  channel_messages_total: "Channel messages",
  dm_messages_total: "Direct messages",
  ops_snapshot_rejections_total: "Rejected operations snapshots",
  media_rooms_current: "Media rooms",
  media_participants_audio_current: "Audio participants",
  media_participants_webcam_current: "Webcam participants",
  media_participants_screenshare_current: "Screenshare participants",
  media_camera_publishers_current: "Camera publishers",
  media_screen_publishers_current: "Screen publishers",
  media_peak_video_publishers_per_room: "Peak video publishers per room",
  media_egress_current_bps: "Current media egress",
  media_egress_peak_bps: "Peak media egress",
  media_egress_cumulative_bytes: "Cumulative media egress",
  media_participant_hours_audio: "Audio participant hours",
  media_participant_hours_webcam: "Webcam participant hours",
  media_participant_hours_screenshare: "Screenshare participant hours",
};

const lifetimeKeys = new Set<MetricKey>(COUNTER_METRIC_KEYS);

const SERIES_PRESETS = [
  ["Host pressure", "host_cpu_percent"],
  ["HTTP traffic", "http_requests_total"],
  ["Realtime activity", "websocket_connections_current"],
  ["Video activity", "media_camera_publishers_current"],
  ["Media egress", "media_egress_current_bps"],
] as const satisfies readonly (readonly [string, MetricKey])[];

function titleCase(value: string): string {
  return `${value.charAt(0).toUpperCase()}${value.slice(1)}`;
}

function serviceMetricSuffix(field: string): string {
  if (field === "cpu_percent") return "CPU";
  if (field === "memory_bytes") return "memory";
  return field;
}

export function metricLabel(key: MetricKey): string {
  const fixed = METRIC_COPY[key];
  if (fixed) return fixed;
  for (const service of SERVICE_NAMES) {
    const prefix = `service_${service}_`;
    if (key.startsWith(prefix)) {
      const field = key.slice(prefix.length);
      return `${SERVICE_LABELS[service]} ${serviceMetricSuffix(field)}`;
    }
  }
  return "Unknown metric";
}

function metricMap(response: AdminCurrentResponse | null) {
  return new Map(
    (response?.metrics ?? []).map((metric) => [metric.metric_key, metric]),
  );
}

function retryTime(retryAt: number): string {
  return new Intl.DateTimeFormat("en-US", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  }).format(new Date(retryAt));
}

interface ResourceNoticeProps<T> {
  name: string;
  resource: PollingResource<T>;
  staleName?: string;
}

function ResourceNotice<T>({
  name,
  resource,
  staleName = name,
}: Readonly<ResourceNoticeProps<T>>) {
  if (resource.state === "loading" && resource.data === null) {
    return (
      <output className="resource-notice resource-loading">
        Loading {name}
      </output>
    );
  }
  if (resource.state === "rate-limited") {
    return (
      <output className="resource-notice">
        Requests paused until{" "}
        {resource.retryAt === null
          ? "the next scheduled refresh"
          : retryTime(resource.retryAt)}
      </output>
    );
  }
  if (resource.state === "stale") {
    return (
      <output className="resource-notice">
        Showing the last {staleName}; live telemetry is unavailable
      </output>
    );
  }
  if (resource.state === "error") {
    return (
      <p className="resource-notice resource-error" role="alert">
        Unable to load {name}
      </p>
    );
  }
  return null;
}

function scaled(value: number, divisor: number, suffix: string): string {
  return `${numberFormatter.format(value / divisor)} ${suffix}`;
}

export function formatScalar(value: number, unit: MetricUnit): string {
  switch (unit) {
    case "percent":
      return `${numberFormatter.format(value)}%`;
    case "bytes":
      if (Math.abs(value) >= 1024 ** 3) return scaled(value, 1024 ** 3, "GB");
      if (Math.abs(value) >= 1024 ** 2) return scaled(value, 1024 ** 2, "MB");
      if (Math.abs(value) >= 1024) return scaled(value, 1024, "KB");
      return `${numberFormatter.format(value)} B`;
    case "bits_per_second":
      if (Math.abs(value) >= 1_000_000_000)
        return scaled(value, 1_000_000_000, "Gb/s");
      if (Math.abs(value) >= 1_000_000) return scaled(value, 1_000_000, "Mb/s");
      if (Math.abs(value) >= 1_000) return scaled(value, 1_000, "Kb/s");
      return `${numberFormatter.format(value)} b/s`;
    case "hours":
      return `${numberFormatter.format(value)} h`;
    case "count":
    case "load":
      return numberFormatter.format(value);
  }
}

export function Status({ state }: Readonly<{ state: DisplayStatus }>) {
  const copy = STATUS_COPY[state];
  return (
    <span className={`status status-${state}`}>
      <span aria-hidden="true">{copy.glyph}</span>
      <span>{copy.label}</span>
    </span>
  );
}

interface ScalarValueProps {
  label: string;
  status: DisplayStatus;
  unit: MetricUnit;
  value: number | null;
}

export function ScalarValue({
  label,
  status,
  unit,
  value,
}: Readonly<ScalarValueProps>) {
  const available = typeof value === "number" && Number.isFinite(value);
  return (
    <article aria-label={label} className="metric-card">
      <div className="metric-card-header">
        <span className="metric-label">{label}</span>
        <Status state={available ? status : "unavailable"} />
      </div>
      <strong className="metric-value">
        {available ? formatScalar(value, unit) : "Unavailable"}
      </strong>
      {available && unit === "percent" ? (
        <meter aria-label={label} max={100} min={0} value={value} />
      ) : null}
    </article>
  );
}

function counterValue(
  response: AdminCountersResponse,
  key: CounterMetricKey,
): number | null {
  return (
    response.counters.find((counter) => counter.metric_key === key)?.value ??
    null
  );
}

export function deriveCounterShare(
  previous: AdminCountersResponse | null,
  current: AdminCountersResponse | null,
  errors: readonly CounterMetricKey[],
): number | null {
  if (!previous || !current || errors.length === 0) return null;

  const previousTotal = counterValue(previous, "http_requests_total");
  const currentTotal = counterValue(current, "http_requests_total");
  if (previousTotal === null || currentTotal === null) return null;

  const totalDelta = currentTotal - previousTotal;
  if (!Number.isFinite(totalDelta) || totalDelta <= 0) return null;

  let errorDelta = 0;
  for (const key of errors) {
    const before = counterValue(previous, key);
    const after = counterValue(current, key);
    if (before === null || after === null || after < before) return null;
    errorDelta += after - before;
  }
  if (
    !Number.isFinite(errorDelta) ||
    errorDelta < 0 ||
    errorDelta > totalDelta
  ) {
    return null;
  }
  return (100 * errorDelta) / totalDelta;
}

interface HostWorkspaceProps {
  current: PollingResource<AdminCurrentResponse>;
  health: PollingResource<AdminHealthResponse>;
  series: PollingResource<AdminSeriesResponse>;
  thresholds: Thresholds;
}

export function HostWorkspace({
  current,
  health,
  series,
  thresholds,
}: Readonly<HostWorkspaceProps>) {
  const metrics = metricMap(current.data);
  const hasHostSample = HOST_FIELDS.some(({ key }) => metrics.has(key));

  return (
    <div className="workspace-content">
      <ResourceNotice
        name="current metrics"
        resource={current}
        staleName="current sample"
      />
      {current.state === "ready" && !hasHostSample ? (
        <p className="empty-state">No recent host sample</p>
      ) : null}

      <section
        aria-labelledby="host-resources-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="host-resources-title">Host resources</h2>
            <p>Current aggregate samples from this operations node</p>
          </div>
        </div>
        <div className="metric-grid host-metric-grid">
          {HOST_FIELDS.map(({ key, label, threshold, unit }) => {
            const value = metrics.get(key)?.value ?? null;
            const status =
              value === null || threshold === null
                ? "normal"
                : statusFor(value, thresholds[threshold]);
            return (
              <div key={key} data-metric-key={key} data-primary-home="host">
                <ScalarValue
                  label={label}
                  status={status}
                  unit={unit}
                  value={value}
                />
                <code className="metric-key">{key}</code>
              </div>
            );
          })}
        </div>
      </section>

      <section
        aria-labelledby="service-summary-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="service-summary-title">Services and containers</h2>
            <p>Fixed seven-service health summary</p>
          </div>
        </div>
        <div className="service-summary-grid">
          {SERVICE_NAMES.map((service) => {
            const sample = health.data?.services.find(
              (candidate) => candidate.service === service,
            );
            return (
              <article className="service-summary" key={service}>
                <strong>{SERVICE_LABELS[service]}</strong>
                <Status state={sample?.state ?? "unknown"} />
              </article>
            );
          })}
        </div>
        <ResourceNotice name="service health" resource={health} />
      </section>

      <div className="workspace-split">
        <section
          aria-labelledby="host-series-title"
          className="workspace-section"
        >
          <div className="section-heading">
            <div>
              <h2 id="host-series-title">Host CPU over 24 hours</h2>
              <p>Hourly aggregate rollup</p>
            </div>
          </div>
          <ResourceNotice name="host series" resource={series} />
          {series.data ? <SeriesChart response={series.data} /> : null}
        </section>

        <section
          aria-labelledby="operational-now-title"
          className="workspace-section"
        >
          <div className="section-heading">
            <div>
              <h2 id="operational-now-title">Operational now</h2>
              <p>Current aggregate gauges</p>
            </div>
          </div>
          <div className="counter-list">
            {OPERATIONAL_NOW.map(([key, label]) => {
              const metric = metrics.get(key);
              return (
                <div className="counter-row" key={key}>
                  <span>{label}</span>
                  <strong>
                    {metric
                      ? formatScalar(metric.value, metric.unit)
                      : "Unavailable"}
                  </strong>
                  <small>Current</small>
                </div>
              );
            })}
          </div>
        </section>
      </div>
    </div>
  );
}

interface ServicesWorkspaceProps {
  current: PollingResource<AdminCurrentResponse>;
  health: PollingResource<AdminHealthResponse>;
  thresholds: Thresholds;
}

function serviceMetricKey(
  service: ServiceName,
  field: "running" | "healthy" | "cpu_percent" | "memory_bytes",
): MetricKey {
  return `service_${service}_${field}` as MetricKey;
}

function booleanMetric(value: number | null, positive: string): string {
  if (value === null) return "Unavailable";
  return value > 0 ? positive : "No";
}

export function ServicesWorkspace({
  current,
  health,
  thresholds,
}: Readonly<ServicesWorkspaceProps>) {
  const metrics = metricMap(current.data);

  return (
    <div className="workspace-content">
      <ResourceNotice
        name="current metrics"
        resource={current}
        staleName="current sample"
      />
      <ResourceNotice name="service health" resource={health} />
      <section
        aria-labelledby="services-table-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="services-table-title">Service resources</h2>
            <p>Health state remains distinct from resource thresholds</p>
          </div>
        </div>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Service</th>
                <th scope="col">Health</th>
                <th scope="col">Running</th>
                <th scope="col">Healthy metric</th>
                <th scope="col">CPU</th>
                <th scope="col">Memory</th>
              </tr>
            </thead>
            <tbody>
              {SERVICE_NAMES.map((service) => {
                const runningKey = serviceMetricKey(service, "running");
                const healthyKey = serviceMetricKey(service, "healthy");
                const cpuKey = serviceMetricKey(service, "cpu_percent");
                const memoryKey = serviceMetricKey(service, "memory_bytes");
                const running = metrics.get(runningKey)?.value ?? null;
                const healthy = metrics.get(healthyKey)?.value ?? null;
                const cpu = metrics.get(cpuKey)?.value ?? null;
                const memory = metrics.get(memoryKey)?.value ?? null;
                const state =
                  health.data?.services.find(
                    (candidate) => candidate.service === service,
                  )?.state ?? "unknown";
                return (
                  <tr key={service}>
                    <th scope="row">{SERVICE_LABELS[service]}</th>
                    <td>
                      <Status state={state} />
                    </td>
                    <td
                      data-metric-key={runningKey}
                      data-primary-home="services"
                    >
                      {booleanMetric(running, "Yes")}
                    </td>
                    <td
                      data-metric-key={healthyKey}
                      data-primary-home="services"
                    >
                      {booleanMetric(healthy, "Yes")}
                    </td>
                    <td data-metric-key={cpuKey} data-primary-home="services">
                      <span className="table-value">
                        {cpu === null
                          ? "Unavailable"
                          : formatScalar(cpu, "percent")}
                      </span>
                      <Status
                        state={
                          cpu === null
                            ? "unavailable"
                            : statusFor(cpu, thresholds.serviceCpu)
                        }
                      />
                    </td>
                    <td
                      data-metric-key={memoryKey}
                      data-primary-home="services"
                    >
                      {memory === null
                        ? "Unavailable"
                        : formatScalar(memory, "bytes")}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}

interface CountersWorkspaceProps {
  counters: PollingResource<AdminCountersResponse>;
  current: PollingResource<AdminCurrentResponse>;
  previousCounters: AdminCountersResponse | null;
  thresholds: Thresholds;
}

export function CountersWorkspace({
  counters,
  current,
  previousCounters,
  thresholds,
}: Readonly<CountersWorkspaceProps>) {
  const metrics = metricMap(current.data);
  for (const counter of counters.data?.counters ?? []) {
    metrics.set(counter.metric_key, counter);
  }
  const clientShare = deriveCounterShare(previousCounters, counters.data, [
    "http_client_errors_total",
  ]);
  const serverShare = deriveCounterShare(previousCounters, counters.data, [
    "http_server_errors_total",
  ]);

  return (
    <div className="workspace-content">
      <ResourceNotice
        name="current metrics"
        resource={current}
        staleName="current sample"
      />
      <ResourceNotice
        name="counters"
        resource={counters}
        staleName="counter sample"
      />
      <section
        aria-labelledby="derived-shares-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="derived-shares-title">HTTP error shares</h2>
            <p>Derived from consecutive process-lifetime counter samples</p>
          </div>
        </div>
        <div className="metric-grid derived-grid">
          <ScalarValue
            label="HTTP client-error share"
            status={
              clientShare === null
                ? "unavailable"
                : statusFor(clientShare, thresholds.http4xxShare)
            }
            unit="percent"
            value={clientShare}
          />
          <ScalarValue
            label="HTTP server-error share"
            status={
              serverShare === null
                ? "unavailable"
                : statusFor(serverShare, thresholds.http5xxShare)
            }
            unit="percent"
            value={serverShare}
          />
        </div>
      </section>

      {COUNTER_GROUPS.map((group) => (
        <section
          aria-labelledby={`counter-group-${group.label.replaceAll(" ", "-")}`}
          className="workspace-section"
          key={group.label}
        >
          <div className="section-heading">
            <div>
              <h2 id={`counter-group-${group.label.replaceAll(" ", "-")}`}>
                {group.label}
              </h2>
            </div>
          </div>
          <div className="counter-list">
            {group.keys.map((key) => {
              const metric = metrics.get(key);
              return (
                <div
                  className="counter-row"
                  data-metric-key={key}
                  data-primary-home="counters"
                  key={key}
                >
                  <span>
                    {metricLabel(key)}
                    <code className="metric-key">{key}</code>
                  </span>
                  <strong>
                    {metric
                      ? formatScalar(metric.value, metric.unit)
                      : "Unavailable"}
                  </strong>
                  <small>
                    {lifetimeKeys.has(key) ? "Process lifetime" : "Current"}
                  </small>
                </div>
              );
            })}
          </div>
        </section>
      ))}
    </div>
  );
}

interface TimeSeriesWorkspaceProps {
  metricKey: MetricKey;
  onMetricKeyChange: (metricKey: MetricKey) => void;
  onWindowChange: (window: SeriesWindow) => void;
  series: PollingResource<AdminSeriesResponse>;
  window: SeriesWindow;
}

export function TimeSeriesWorkspace({
  metricKey,
  onMetricKeyChange,
  onWindowChange,
  series,
  window,
}: Readonly<TimeSeriesWorkspaceProps>) {
  return (
    <div className="workspace-content">
      <section
        aria-labelledby="series-controls-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="series-controls-title">Metric selection</h2>
            <p>One bounded aggregate series at a time</p>
          </div>
        </div>
        <div className="series-controls">
          <label>
            <span>Metric</span>
            <select
              value={metricKey}
              onChange={(event) => {
                const next = event.currentTarget.value as MetricKey;
                if (METRIC_KEYS.includes(next)) onMetricKeyChange(next);
              }}
            >
              {METRIC_KEYS.map((key) => (
                <option key={key} value={key}>
                  {metricLabel(key)}
                </option>
              ))}
            </select>
          </label>
          <fieldset aria-label="Series window" className="segmented-control">
            <button
              aria-pressed={window === "24h"}
              type="button"
              onClick={() => onWindowChange("24h")}
            >
              24 hours
            </button>
            <button
              aria-pressed={window === "7d"}
              type="button"
              onClick={() => onWindowChange("7d")}
            >
              7 days
            </button>
          </fieldset>
        </div>
        <div aria-label="Series presets" className="preset-list">
          {SERIES_PRESETS.map(([label, key]) => (
            <button
              key={key}
              type="button"
              onClick={() => onMetricKeyChange(key)}
            >
              {label}
            </button>
          ))}
        </div>
      </section>
      <section
        aria-labelledby="selected-series-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="selected-series-title">{metricLabel(metricKey)}</h2>
            <p>
              <code>{metricKey}</code> / {window}
            </p>
          </div>
        </div>
        <ResourceNotice name="selected series" resource={series} />
        {series.data ? <SeriesChart response={series.data} /> : null}
      </section>
    </div>
  );
}

interface ChangesWorkspaceProps {
  events: readonly HealthChange[];
  health: PollingResource<AdminHealthResponse>;
}

export function ChangesWorkspace({
  events,
  health,
}: Readonly<ChangesWorkspaceProps>) {
  return (
    <div className="workspace-content">
      <ResourceNotice name="service health" resource={health} />
      <section
        aria-labelledby="current-health-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="current-health-title">Current service health</h2>
            <p>Fixed seven-service aggregate state</p>
          </div>
        </div>
        <div className="service-summary-grid">
          {SERVICE_NAMES.map((service) => {
            const state =
              health.data?.services.find(
                (candidate) => candidate.service === service,
              )?.state ?? "unknown";
            return (
              <article
                className="service-summary"
                data-testid={`health-state-${service}`}
                key={service}
              >
                <strong>{SERVICE_LABELS[service]}</strong>
                <Status state={state} />
              </article>
            );
          })}
        </div>
      </section>
      <section
        aria-labelledby="observed-changes-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="observed-changes-title">Observed since this tab opened</h2>
            <p>Consecutive successful health samples only</p>
          </div>
        </div>
        {events.length === 0 ? (
          <p className="empty-state">
            No changes observed since this tab opened
          </p>
        ) : (
          <ol className="event-list">
            {events.map((event) => (
              <li className="event-row" key={event.id}>
                <time dateTime={event.observedAt}>
                  {new Intl.DateTimeFormat("en-US", {
                    hour: "2-digit",
                    minute: "2-digit",
                    second: "2-digit",
                    timeZone: "UTC",
                  }).format(new Date(event.observedAt))}
                </time>
                <strong>{SERVICE_LABELS[event.service]}</strong>
                <span>
                  {titleCase(event.previous)} to {event.current}
                </span>
              </li>
            ))}
          </ol>
        )}
      </section>
    </div>
  );
}
