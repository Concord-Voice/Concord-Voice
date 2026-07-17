import {
  Activity,
  ChartLine,
  Gauge,
  ListChecks,
  LogOut,
  RefreshCw,
  Server,
  Settings,
  Users,
  type LucideIcon,
} from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { api } from "./api";
import type {
  AdminCountersResponse,
  AdminCurrentResponse,
  AdminHealthResponse,
  AdminSeriesResponse,
  HealthState,
  MetricKey,
  SeriesWindow,
} from "./contracts";
import { SERVICE_NAMES } from "./contracts";
import {
  DEFAULT_THRESHOLDS,
  THRESHOLD_MAXIMUMS,
  loadFont,
  loadThresholds,
  saveFont,
  saveThresholds,
  validateThresholds,
  type FontChoice,
  type ThresholdName,
  type Thresholds,
} from "./preferences";
import { usePolling, type PollingState } from "./usePolling";
import {
  ChangesWorkspace,
  CountersWorkspace,
  HostWorkspace,
  ServicesWorkspace,
  Status,
  TimeSeriesWorkspace,
  UsersActivityWorkspace,
  type HealthChange,
  type PollingResource,
} from "./workspaces";
import type { TimeMode } from "./time";

export type Workspace =
  "host" | "services" | "users" | "counters" | "series" | "changes";

interface WorkspaceDefinition {
  count: string;
  icon: LucideIcon;
  id: Workspace;
  label: string;
}

const WORKSPACES: readonly WorkspaceDefinition[] = [
  { count: "4", icon: Gauge, id: "host", label: "Host Overview" },
  { count: "7", icon: Server, id: "services", label: "Services" },
  { count: "9", icon: Users, id: "users", label: "Users & Activity" },
  { count: "20", icon: ListChecks, id: "counters", label: "Counters" },
  { count: "1", icon: ChartLine, id: "series", label: "Time Series" },
  { count: "Live", icon: Activity, id: "changes", label: "Health & Changes" },
];

const THRESHOLD_FIELDS: readonly {
  key: ThresholdName;
  label: string;
}[] = [
  { key: "hostCpu", label: "Host CPU" },
  { key: "hostMemory", label: "Host memory" },
  { key: "hostDisk", label: "Host disk" },
  { key: "serviceCpu", label: "Service CPU" },
  { key: "http4xxShare", label: "HTTP client-error share" },
  { key: "http5xxShare", label: "HTTP server-error share" },
];

const warningAfterMs = 25 * 60 * 1_000;
const logoutAfterMs = 30 * 60 * 1_000;
const CURRENT_WORKSPACES = new Set<Workspace>([
  "host",
  "services",
  "users",
  "counters",
]);
const HEALTH_SEED_STATES = new Set<PollingState>([
  "idle",
  "loading",
  "stale",
  "rate-limited",
]);

function reloadDocument(): void {
  globalThis.location.reload();
}

interface PortalProps {
  initialHealth: AdminHealthResponse | null;
  initialState?: PollingState;
  onForbidden?: () => void;
  onSessionEnded: () => void;
}

function copyThresholds(value: Thresholds): Thresholds {
  return Object.fromEntries(
    THRESHOLD_FIELDS.map(({ key }) => [
      key,
      { critical: value[key].critical, warning: value[key].warning },
    ]),
  ) as Thresholds;
}

function pairError(
  name: ThresholdName,
  pair: { critical: number; warning: number },
): string | null {
  if (!Number.isFinite(pair.warning) || !Number.isFinite(pair.critical)) {
    return "Enter finite warning and critical values";
  }
  const maximum = THRESHOLD_MAXIMUMS[name];
  if (pair.warning < 0 || pair.critical > maximum) {
    return `Values must be between 0 and ${maximum.toLocaleString("en-US")}`;
  }
  if (pair.warning >= pair.critical) return "Warning must be below critical";
  return null;
}

function aggregateState(health: AdminHealthResponse | null): HealthState {
  if (!health) return "unknown";
  const states = new Set(health.services.map(({ state }) => state));
  if (states.has("stopped")) return "stopped";
  if (states.has("degraded")) return "degraded";
  if (states.has("unknown")) return "unknown";
  return "healthy";
}

function aggregateCopy(state: HealthState): string {
  switch (state) {
    case "healthy":
      return "All systems healthy";
    case "degraded":
      return "Service degradation observed";
    case "stopped":
      return "Stopped service observed";
    case "unknown":
      return "Health unavailable";
  }
}

function asPollingResource<T>(
  value: Pick<PollingResource<T>, "data" | "retryAt" | "state">,
): PollingResource<T> {
  return value;
}

function resolveHealthResource(
  poll: Pick<
    PollingResource<AdminHealthResponse>,
    "data" | "retryAt" | "state"
  >,
  seed: AdminHealthResponse | null,
  initialState: PollingState,
): PollingResource<AdminHealthResponse> {
  const data = poll.data ?? (HEALTH_SEED_STATES.has(poll.state) ? seed : null);
  let state = poll.state;
  if (poll.data === null && seed === null && poll.state === "loading") {
    state = initialState;
  }
  return { data, retryAt: poll.retryAt, state };
}

interface StatefulResource {
  state: PollingState;
}

function activePollingResources(
  health: StatefulResource,
  optional: readonly (readonly [boolean, StatefulResource])[],
): StatefulResource[] {
  const active = [health];
  for (const [enabled, resource] of optional) {
    if (enabled) active.push(resource);
  }
  return active;
}

interface WorkspaceContentProps {
  counters: PollingResource<AdminCountersResponse>;
  current: PollingResource<AdminCurrentResponse>;
  events: HealthChange[];
  health: PollingResource<AdminHealthResponse>;
  metricKey: MetricKey;
  onMetricKeyChange: (key: MetricKey) => void;
  onWindowChange: (window: SeriesWindow) => void;
  previousCounters: AdminCountersResponse | null;
  series: PollingResource<AdminSeriesResponse>;
  thresholds: Thresholds;
  timeMode: TimeMode;
  window: SeriesWindow;
  workspace: Workspace;
}

function WorkspaceContent({
  counters,
  current,
  events,
  health,
  metricKey,
  onMetricKeyChange,
  onWindowChange,
  previousCounters,
  series,
  thresholds,
  timeMode,
  window,
  workspace,
}: Readonly<WorkspaceContentProps>) {
  switch (workspace) {
    case "host":
      return (
        <HostWorkspace
          current={current}
          health={health}
          series={series}
          thresholds={thresholds}
          timeMode={timeMode}
        />
      );
    case "services":
      return (
        <ServicesWorkspace
          current={current}
          health={health}
          thresholds={thresholds}
          timeMode={timeMode}
        />
      );
    case "users":
      return (
        <UsersActivityWorkspace
          counters={counters}
          current={current}
          previousCounters={previousCounters}
          timeMode={timeMode}
        />
      );
    case "counters":
      return (
        <CountersWorkspace
          counters={counters}
          current={current}
          previousCounters={previousCounters}
          thresholds={thresholds}
          timeMode={timeMode}
        />
      );
    case "series":
      return (
        <TimeSeriesWorkspace
          metricKey={metricKey}
          onMetricKeyChange={onMetricKeyChange}
          onWindowChange={onWindowChange}
          series={series}
          timeMode={timeMode}
          window={window}
        />
      );
    case "changes":
      return (
        <ChangesWorkspace events={events} health={health} timeMode={timeMode} />
      );
  }
}

export function Portal({
  initialHealth,
  initialState = initialHealth === null ? "stale" : "ready",
  onForbidden = reloadDocument,
  onSessionEnded,
}: Readonly<PortalProps>) {
  const [workspace, setWorkspace] = useState<Workspace>("host");
  const [healthSeed, setHealthSeed] = useState(initialHealth);
  const [healthEvents, setHealthEvents] = useState<HealthChange[]>([]);
  const [thresholds, setThresholds] = useState(loadThresholds);
  const [font, setFont] = useState(loadFont);
  const [draftThresholds, setDraftThresholds] = useState(() =>
    copyThresholds(thresholds),
  );
  const [draftFont, setDraftFont] = useState<FontChoice>(font);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [settingsError, setSettingsError] = useState<string | null>(null);
  const [seriesKey, setSeriesKey] = useState<MetricKey>("host_cpu_percent");
  const [seriesWindow, setSeriesWindow] = useState<SeriesWindow>("24h");
  const [timeMode, setTimeMode] = useState<TimeMode>("utc");
  const [warning, setWarning] = useState(false);
  const [ending, setEnding] = useState(false);
  const [logoutError, setLogoutError] = useState(false);
  const lastOperatorActivityRef = useRef(0);
  const logoutStartedRef = useRef(false);
  const terminalTransitionStartedRef = useRef(false);
  const sessionGenerationRef = useRef(0);
  const sessionEndedRef = useRef(onSessionEnded);
  const forbiddenRef = useRef(onForbidden);
  const focusBeforeWarningRef = useRef<HTMLElement | null>(null);
  const warningRef = useRef<HTMLElement>(null);
  const warningActiveRef = useRef(warning);
  const recordActivityRef = useRef<() => void>(() => {});
  const settingsDialogRef = useRef<HTMLDialogElement>(null);
  const settingsTriggerRef = useRef<HTMLButtonElement>(null);
  const previousHealthRef = useRef(initialHealth);
  const healthEventSequenceRef = useRef(0);
  const previousCountersRef = useRef<AdminCountersResponse | null>(null);
  const latestCountersRef = useRef<AdminCountersResponse | null>(null);
  sessionEndedRef.current = onSessionEnded;
  forbiddenRef.current = onForbidden;
  warningActiveRef.current = warning;

  const clearSessionMemory = useCallback(() => {
    setHealthSeed(null);
    setHealthEvents([]);
    previousHealthRef.current = null;
    previousCountersRef.current = null;
    latestCountersRef.current = null;
    focusBeforeWarningRef.current = null;
    setWarning(false);
    setSettingsOpen(false);
    setEnding(true);
  }, []);

  const beginTerminalTransition = useCallback(() => {
    if (terminalTransitionStartedRef.current) return false;
    terminalTransitionStartedRef.current = true;
    sessionGenerationRef.current += 1;
    clearSessionMemory();
    return true;
  }, [clearSessionMemory]);

  const handleUnauthorized = useCallback(() => {
    if (!beginTerminalTransition()) return;
    sessionEndedRef.current();
  }, [beginTerminalTransition]);

  const handleForbidden = useCallback(() => {
    if (!beginTerminalTransition()) return;
    forbiddenRef.current();
  }, [beginTerminalTransition]);

  const loadHealth = useCallback(
    (signal: AbortSignal) => api.getHealth({ signal }),
    [],
  );

  const recordHealth = useCallback((next: AdminHealthResponse) => {
    const previous = previousHealthRef.current;
    if (previous) {
      const changes: HealthChange[] = [];
      for (const service of SERVICE_NAMES) {
        const before = previous.services.find(
          (candidate) => candidate.service === service,
        );
        const after = next.services.find(
          (candidate) => candidate.service === service,
        );
        if (before && after && before.state !== after.state) {
          healthEventSequenceRef.current += 1;
          changes.push({
            current: after.state,
            id: `${service}-${healthEventSequenceRef.current}`,
            observedAt: after.sampled_at ?? new Date().toISOString(),
            previous: before.state,
            service,
          });
        }
      }
      if (changes.length > 0) {
        setHealthEvents((current) => [...changes, ...current].slice(0, 100));
      }
    }
    previousHealthRef.current = next;
  }, []);

  const loadCurrent = useCallback(
    (signal: AbortSignal) => api.getCurrent({ signal }),
    [],
  );

  const loadCounters = useCallback(
    (signal: AbortSignal) => api.getCounters({ signal }),
    [],
  );

  const recordCounters = useCallback((next: AdminCountersResponse) => {
    previousCountersRef.current = latestCountersRef.current;
    latestCountersRef.current = next;
  }, []);

  const activeSeriesKey = workspace === "host" ? "host_cpu_percent" : seriesKey;
  const activeSeriesWindow = workspace === "host" ? "24h" : seriesWindow;
  const loadSeries = useCallback(
    (signal: AbortSignal) =>
      api.getSeries(activeSeriesKey, activeSeriesWindow, { signal }),
    [activeSeriesKey, activeSeriesWindow],
  );

  const currentEnabled = !ending && CURRENT_WORKSPACES.has(workspace);
  const countersEnabled =
    !ending && (workspace === "counters" || workspace === "users");
  const seriesEnabled =
    !ending && (workspace === "host" || workspace === "series");

  const healthPoll = usePolling({
    enabled: !ending,
    intervalMs: 15_000,
    key: "health",
    load: loadHealth,
    onData: recordHealth,
    onForbidden: handleForbidden,
    onUnauthorized: handleUnauthorized,
    sessionGeneration: sessionGenerationRef,
  });
  const currentPoll = usePolling({
    enabled: currentEnabled,
    intervalMs: 15_000,
    key: `current:${workspace}`,
    load: loadCurrent,
    onForbidden: handleForbidden,
    onUnauthorized: handleUnauthorized,
    sessionGeneration: sessionGenerationRef,
  });
  const countersPoll = usePolling({
    enabled: countersEnabled,
    intervalMs: 30_000,
    key: "counters",
    load: loadCounters,
    onData: recordCounters,
    onForbidden: handleForbidden,
    onUnauthorized: handleUnauthorized,
    sessionGeneration: sessionGenerationRef,
  });
  useEffect(() => {
    if (
      countersEnabled &&
      countersPoll.state !== "idle" &&
      countersPoll.state !== "stale" &&
      countersPoll.state !== "rate-limited" &&
      countersPoll.state !== "error"
    ) {
      return;
    }
    previousCountersRef.current = null;
    latestCountersRef.current = null;
  }, [countersEnabled, countersPoll.state]);
  useEffect(() => {
    if (!countersEnabled) return;
    const clearHiddenCounterBaseline = () => {
      if (!document.hidden) return;
      previousCountersRef.current = null;
      latestCountersRef.current = null;
    };
    document.addEventListener("visibilitychange", clearHiddenCounterBaseline);
    return () => {
      document.removeEventListener(
        "visibilitychange",
        clearHiddenCounterBaseline,
      );
    };
  }, [countersEnabled]);
  const seriesPoll = usePolling({
    enabled: seriesEnabled,
    intervalMs: 5 * 60_000,
    key: `series:${workspace}:${activeSeriesKey}:${activeSeriesWindow}`,
    load: loadSeries,
    onForbidden: handleForbidden,
    onUnauthorized: handleUnauthorized,
    sessionGeneration: sessionGenerationRef,
  });

  const healthResource = resolveHealthResource(
    healthPoll,
    healthSeed,
    initialState,
  );
  const healthData = healthResource.data;
  const currentResource = asPollingResource(currentPoll);
  const countersResource = asPollingResource(countersPoll);
  const seriesResource = asPollingResource(seriesPoll);

  const endSession = useCallback(() => {
    if (logoutStartedRef.current) return;
    if (!terminalTransitionStartedRef.current && !beginTerminalTransition()) {
      return;
    }
    logoutStartedRef.current = true;
    setLogoutError(false);
    void api
      .logout()
      .then(() => sessionEndedRef.current())
      .catch(() => setLogoutError(true))
      .finally(() => {
        logoutStartedRef.current = false;
      });
  }, [beginTerminalTransition]);

  useEffect(() => {
    if (!warning) return;
    const activeElement = document.activeElement;
    if (
      activeElement instanceof HTMLElement &&
      activeElement !== document.body &&
      !warningRef.current?.contains(activeElement)
    ) {
      focusBeforeWarningRef.current = activeElement;
    }
    warningRef.current?.focus();
  }, [warning]);

  useEffect(() => {
    if (ending) return;
    if (lastOperatorActivityRef.current === 0) {
      lastOperatorActivityRef.current = Date.now();
    }
    let timer: number | undefined;

    const clearTimer = () => {
      if (timer !== undefined) globalThis.clearTimeout(timer);
      timer = undefined;
    };

    const evaluateIdle = () => {
      clearTimer();
      const elapsed = Math.max(0, Date.now() - lastOperatorActivityRef.current);
      if (elapsed >= logoutAfterMs) {
        endSession();
        return;
      }
      if (elapsed >= warningAfterMs) {
        warningActiveRef.current = true;
        setWarning(true);
        timer = globalThis.setTimeout(evaluateIdle, logoutAfterMs - elapsed);
        return;
      }
      timer = globalThis.setTimeout(evaluateIdle, warningAfterMs - elapsed);
    };

    const recordActivity = () => {
      const restoreFocus = warningActiveRef.current
        ? focusBeforeWarningRef.current
        : null;
      warningActiveRef.current = false;
      focusBeforeWarningRef.current = null;
      lastOperatorActivityRef.current = Date.now();
      setWarning(false);
      evaluateIdle();
      if (restoreFocus?.isConnected) restoreFocus.focus();
    };
    recordActivityRef.current = recordActivity;

    const handleVisibility = () => {
      if (!document.hidden) evaluateIdle();
    };

    globalThis.addEventListener("pointerdown", recordActivity, {
      passive: true,
    });
    globalThis.addEventListener("keydown", recordActivity);
    globalThis.addEventListener("touchstart", recordActivity, {
      passive: true,
    });
    document.addEventListener("visibilitychange", handleVisibility);
    evaluateIdle();

    return () => {
      clearTimer();
      globalThis.removeEventListener("pointerdown", recordActivity);
      globalThis.removeEventListener("keydown", recordActivity);
      globalThis.removeEventListener("touchstart", recordActivity);
      document.removeEventListener("visibilitychange", handleVisibility);
      recordActivityRef.current = () => {};
    };
  }, [ending, endSession]);

  useEffect(() => {
    if (settingsOpen && !settingsDialogRef.current?.open) {
      settingsDialogRef.current?.showModal();
    }
  }, [settingsOpen]);

  const closeSettings = () => {
    if (settingsDialogRef.current?.open) settingsDialogRef.current.close();
    setSettingsOpen(false);
    settingsTriggerRef.current?.focus();
  };

  const openSettings = () => {
    setDraftThresholds(copyThresholds(thresholds));
    setDraftFont(font);
    setSettingsError(null);
    setSettingsOpen(true);
  };

  const saveSettings = () => {
    if (!validateThresholds(draftThresholds)) return;
    if (!saveThresholds(draftThresholds) || !saveFont(draftFont)) {
      setSettingsError("Browser settings storage is unavailable");
      return;
    }
    setThresholds(copyThresholds(draftThresholds));
    setFont(draftFont);
    closeSettings();
  };

  if (ending) {
    if (logoutError) {
      return (
        <section aria-labelledby="logout-error-title" className="shell-notice">
          <h1 id="logout-error-title">Sign out incomplete</h1>
          <p role="alert">
            Could not confirm sign out. Your session may still be active.
          </p>
          <button type="button" onClick={endSession}>
            Retry sign out
          </button>
        </section>
      );
    }
    return <output>Ending Admin Portal session</output>;
  }

  const activeWorkspace =
    WORKSPACES.find((candidate) => candidate.id === workspace) ?? WORKSPACES[0];
  const aggregate = aggregateState(healthData);
  const activeResources = activePollingResources(healthResource, [
    [currentEnabled, currentResource],
    [countersEnabled, countersResource],
    [seriesEnabled, seriesResource],
  ]);
  const refreshDisabled = activeResources.some(
    ({ state }) => state === "loading" || state === "rate-limited",
  );

  const refreshVisible = () => {
    healthPoll.refresh();
    if (currentEnabled) currentPoll.refresh();
    if (countersEnabled) countersPoll.refresh();
    if (seriesEnabled) seriesPoll.refresh();
  };

  return (
    <div className="portal-shell" data-font={font} data-testid="portal-shell">
      <aside className="command-rail-shell">
        <p className="rail-label">Operations</p>
        <nav aria-label="Admin workspaces" className="command-rail">
          {WORKSPACES.map((candidate) => {
            const Icon = candidate.icon;
            const selected = candidate.id === workspace;
            return (
              <button
                key={candidate.id}
                aria-label={candidate.label}
                aria-current={selected ? "page" : undefined}
                aria-pressed={selected}
                className="rail-link"
                type="button"
                onClick={() => setWorkspace(candidate.id)}
              >
                <Icon aria-hidden="true" size={18} />
                <span>{candidate.label}</span>
                <small>{candidate.count}</small>
              </button>
            );
          })}
        </nav>
        <div className="rail-node">
          <span>Current node</span>
          <code>{healthData?.node_id ?? "Unavailable"}</code>
        </div>
      </aside>

      <div className="portal-workspace">
        <header className="workspace-header">
          <div className="workspace-title">
            <h1 id="workspace-title">{activeWorkspace.label}</h1>
            <p>
              {healthData ? (
                <>
                  <code>{healthData.node_id}</code> / aggregate telemetry
                </>
              ) : (
                "Aggregate telemetry unavailable"
              )}
            </p>
          </div>
          <div className="workspace-actions">
            <fieldset
              aria-label="Timestamp display"
              className="segmented-control time-mode-control"
            >
              {(["utc", "local"] as const).map((mode) => (
                <button
                  key={mode}
                  aria-pressed={timeMode === mode}
                  type="button"
                  onClick={() => setTimeMode(mode)}
                >
                  {mode === "utc" ? "UTC" : "Local"}
                </button>
              ))}
            </fieldset>
            <span className="aggregate-status">
              <Status state={aggregate} />
              <span>{aggregateCopy(aggregate)}</span>
            </span>
            <button
              aria-label="Refresh visible workspace"
              className="icon-button"
              disabled={refreshDisabled}
              title="Refresh visible workspace"
              type="button"
              onClick={refreshVisible}
            >
              <RefreshCw aria-hidden="true" size={18} />
            </button>
            <button
              ref={settingsTriggerRef}
              aria-label="Settings"
              className="icon-button"
              title="Settings"
              type="button"
              onClick={openSettings}
            >
              <Settings aria-hidden="true" size={18} />
            </button>
            <span className="session-badge">
              <strong>Admin session</strong>
              <span>Active</span>
              <small>Idle warning after 25 minutes</small>
            </span>
            <button
              aria-label="Sign out"
              className="icon-button"
              title="Sign out"
              type="button"
              onClick={endSession}
            >
              <LogOut aria-hidden="true" size={18} />
            </button>
          </div>
        </header>

        <main aria-labelledby="workspace-title" className="workspace-main">
          {healthData === null ? (
            <output className="shell-notice">
              Live telemetry is temporarily unavailable
              {initialState === "rate-limited" ? "; requests are paused" : ""}.
            </output>
          ) : null}
          <WorkspaceContent
            counters={countersResource}
            current={currentResource}
            events={healthEvents}
            health={healthResource}
            metricKey={seriesKey}
            onMetricKeyChange={setSeriesKey}
            onWindowChange={setSeriesWindow}
            previousCounters={previousCountersRef.current}
            series={seriesResource}
            thresholds={thresholds}
            timeMode={timeMode}
            window={seriesWindow}
            workspace={workspace}
          />
        </main>
      </div>

      <dialog
        ref={settingsDialogRef}
        aria-labelledby="settings-title"
        className="settings-dialog"
        onCancel={(event) => {
          event.preventDefault();
          closeSettings();
        }}
      >
        <form
          onSubmit={(event) => {
            event.preventDefault();
            saveSettings();
          }}
        >
          <div className="dialog-header">
            <div>
              <h2 id="settings-title">Portal settings</h2>
              <p>Presentation thresholds and interface font</p>
            </div>
          </div>
          <div className="threshold-settings">
            {THRESHOLD_FIELDS.map(({ key, label }) => {
              const error = pairError(key, draftThresholds[key]);
              return (
                <fieldset key={key}>
                  <legend>{label}</legend>
                  <label>
                    <span>Warning</span>
                    <input
                      aria-label={`${label} warning`}
                      max={THRESHOLD_MAXIMUMS[key]}
                      min="0"
                      step="0.1"
                      type="number"
                      value={draftThresholds[key].warning}
                      onChange={(event) => {
                        const warningValue = event.currentTarget.valueAsNumber;
                        setDraftThresholds((current) => ({
                          ...current,
                          [key]: {
                            ...current[key],
                            warning: warningValue,
                          },
                        }));
                      }}
                    />
                  </label>
                  <label>
                    <span>Critical</span>
                    <input
                      aria-label={`${label} critical`}
                      max={THRESHOLD_MAXIMUMS[key]}
                      min="0"
                      step="0.1"
                      type="number"
                      value={draftThresholds[key].critical}
                      onChange={(event) => {
                        const criticalValue = event.currentTarget.valueAsNumber;
                        setDraftThresholds((current) => ({
                          ...current,
                          [key]: {
                            ...current[key],
                            critical: criticalValue,
                          },
                        }));
                      }}
                    />
                  </label>
                  {error ? <p role="alert">{error}</p> : null}
                </fieldset>
              );
            })}
          </div>
          <label className="font-setting">
            <span>Interface font</span>
            <select
              aria-label="Interface font"
              value={draftFont}
              onChange={(event) => {
                const next = event.currentTarget.value;
                if (
                  next === "source-sans" ||
                  next === "atkinson" ||
                  next === "open-dyslexic"
                ) {
                  setDraftFont(next);
                }
              }}
            >
              <option value="source-sans">Source Sans</option>
              <option value="atkinson">Atkinson Hyperlegible</option>
              <option value="open-dyslexic">OpenDyslexic</option>
            </select>
          </label>
          {settingsError ? <p role="alert">{settingsError}</p> : null}
          <div className="dialog-actions">
            <button type="button" onClick={closeSettings}>
              Cancel
            </button>
            <button
              type="button"
              onClick={() => {
                setDraftThresholds(copyThresholds(DEFAULT_THRESHOLDS));
                setDraftFont("source-sans");
                setSettingsError(null);
              }}
            >
              Reset defaults
            </button>
            <button type="submit">Save settings</button>
          </div>
        </form>
      </dialog>

      {warning ? (
        <section
          ref={warningRef}
          aria-describedby="idle-warning-copy"
          aria-labelledby="idle-warning-title"
          className="idle-warning"
          role="alertdialog"
          tabIndex={-1}
        >
          <h2 id="idle-warning-title">
            Your Admin Portal session is about to end
          </h2>
          <p id="idle-warning-copy">
            Interact with this page within five minutes to stay signed in.
          </p>
          <button type="button" onClick={() => recordActivityRef.current()}>
            Stay signed in
          </button>
        </section>
      ) : null}
    </div>
  );
}
