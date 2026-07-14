import { useCallback, useEffect, useRef, useState } from "react";

import { ApiContractError, ApiError } from "./api";

export type PollingState =
  "idle" | "loading" | "ready" | "stale" | "rate-limited" | "error";

export interface UsePollingOptions<T> {
  enabled: boolean;
  intervalMs: number;
  key: string;
  load: (signal: AbortSignal) => Promise<T>;
  onData?: (data: T) => void;
  onForbidden: () => void;
  onUnauthorized: () => void;
  sessionGeneration: Readonly<{ current: number }>;
}

export interface UsePollingResult<T> {
  data: T | null;
  refresh: () => void;
  retryAt: number | null;
  state: PollingState;
}

interface PollingSnapshot<T> {
  data: T | null;
  retryAt: number | null;
  state: PollingState;
}

export function usePolling<T>({
  enabled,
  intervalMs,
  key,
  load,
  onData,
  onForbidden,
  onUnauthorized,
  sessionGeneration,
}: UsePollingOptions<T>): UsePollingResult<T> {
  const [snapshot, setSnapshot] = useState<PollingSnapshot<T>>({
    data: null,
    retryAt: null,
    state: "idle",
  });
  const loadRef = useRef(load);
  const dataRef = useRef(onData);
  const forbiddenRef = useRef(onForbidden);
  const unauthorizedRef = useRef(onUnauthorized);
  const refreshRef = useRef<() => void>(() => {});

  loadRef.current = load;
  dataRef.current = onData;
  forbiddenRef.current = onForbidden;
  unauthorizedRef.current = onUnauthorized;

  const refresh = useCallback(() => refreshRef.current(), []);

  useEffect(() => {
    const generation = sessionGeneration.current;
    let disposed = false;
    let terminal = false;
    let inFlight = false;
    let timer: number | undefined;
    let controller: AbortController | null = null;
    let retryDeadline: number | null = null;
    let resetPending = true;

    const clearTimer = () => {
      if (timer !== undefined) globalThis.clearTimeout(timer);
      timer = undefined;
    };

    const schedule = (delayMs: number, run: () => void) => {
      clearTimer();
      timer = globalThis.setTimeout(run, Math.max(0, delayMs));
    };

    const isInactive = () =>
      disposed ||
      terminal ||
      generation !== sessionGeneration.current ||
      !enabled ||
      document.hidden;

    const setStale = () => {
      setSnapshot((current) => ({
        ...current,
        retryAt: null,
        state: "stale",
      }));
    };

    const handleFailure = (cause: unknown): void => {
      if (cause instanceof ApiContractError) {
        setSnapshot({ data: null, retryAt: null, state: "error" });
        return;
      }
      if (!(cause instanceof ApiError)) {
        setStale();
        return;
      }

      switch (cause.status) {
        case 401:
          terminal = true;
          setSnapshot({ data: null, retryAt: null, state: "idle" });
          unauthorizedRef.current();
          return;
        case 403:
          terminal = true;
          setSnapshot({ data: null, retryAt: null, state: "idle" });
          forbiddenRef.current();
          return;
        case 429: {
          const retryMs =
            cause.retryAfter === null
              ? intervalMs
              : Math.max(1, cause.retryAfter * 1_000);
          retryDeadline = Date.now() + retryMs;
          setSnapshot((current) => ({
            ...current,
            retryAt: retryDeadline,
            state: "rate-limited",
          }));
          return;
        }
        case 0:
        case 503:
          setStale();
          return;
        default:
          setSnapshot({ data: null, retryAt: null, state: "error" });
      }
    };

    const run = async (): Promise<void> => {
      if (isInactive() || inFlight) return;
      if (retryDeadline !== null && Date.now() < retryDeadline) {
        schedule(retryDeadline - Date.now(), () => void run());
        return;
      }

      clearTimer();
      retryDeadline = null;
      inFlight = true;
      controller = new AbortController();
      const activeController = controller;
      setSnapshot((current) => ({
        data: resetPending ? null : current.data,
        retryAt: null,
        state: "loading",
      }));
      resetPending = false;

      try {
        const data = await loadRef.current(activeController.signal);
        if (isInactive() || activeController.signal.aborted) return;
        dataRef.current?.(data);
        if (isInactive() || activeController.signal.aborted) return;
        setSnapshot({ data, retryAt: null, state: "ready" });
      } catch (cause) {
        if (isInactive() || activeController.signal.aborted) return;
        handleFailure(cause);
      } finally {
        if (controller === activeController) controller = null;
        inFlight = false;
        if (isInactive()) return;
        const delay =
          retryDeadline === null
            ? intervalMs
            : Math.max(0, retryDeadline - Date.now());
        schedule(delay, () => void run());
      }
    };

    refreshRef.current = () => {
      if (
        disposed ||
        terminal ||
        !enabled ||
        document.hidden ||
        inFlight ||
        (retryDeadline !== null && Date.now() < retryDeadline)
      ) {
        return;
      }
      clearTimer();
      void run();
    };

    const handleVisibility = () => {
      clearTimer();
      if (document.hidden || disposed || terminal || !enabled) return;
      if (retryDeadline !== null && Date.now() < retryDeadline) {
        schedule(retryDeadline - Date.now(), () => void run());
        return;
      }
      void run();
    };

    document.addEventListener("visibilitychange", handleVisibility);
    if (enabled && !document.hidden) {
      void run();
    } else {
      queueMicrotask(() => {
        if (disposed || generation !== sessionGeneration.current) return;
        setSnapshot({
          data: null,
          retryAt: null,
          state: enabled ? "loading" : "idle",
        });
      });
    }

    return () => {
      disposed = true;
      clearTimer();
      controller?.abort();
      document.removeEventListener("visibilitychange", handleVisibility);
      refreshRef.current = () => {};
    };
  }, [enabled, intervalMs, key, sessionGeneration]);

  return { ...snapshot, refresh };
}
