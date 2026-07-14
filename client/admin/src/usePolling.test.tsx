import { useRef } from "react";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiContractError, ApiError } from "./api";
import { usePolling } from "./usePolling";

interface HarnessProps {
  enabled?: boolean;
  intervalMs?: number;
  load: (signal: AbortSignal) => Promise<string>;
  onData?: (data: string) => void;
  onForbidden?: () => void;
  onUnauthorized?: () => void;
  resourceKey?: string;
  sessionGeneration?: { current: number };
}

function Harness({
  enabled = true,
  intervalMs = 15_000,
  load,
  onData,
  onForbidden = () => {},
  onUnauthorized = () => {},
  resourceKey = "health",
  sessionGeneration,
}: HarnessProps) {
  const localSessionGenerationRef = useRef(0);
  const polling = usePolling({
    key: resourceKey,
    intervalMs,
    load,
    enabled,
    onData,
    onForbidden,
    onUnauthorized,
    sessionGeneration: sessionGeneration ?? localSessionGenerationRef,
  });

  return (
    <>
      <output data-testid="state">{polling.state}</output>
      <output data-testid="data">{polling.data ?? "none"}</output>
      <output data-testid="retry">{polling.retryAt ?? "none"}</output>
      <button type="button" onClick={polling.refresh}>
        Refresh
      </button>
    </>
  );
}

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

function setHidden(hidden: boolean): void {
  Object.defineProperty(document, "hidden", {
    configurable: true,
    value: hidden,
  });
  document.dispatchEvent(new Event("visibilitychange"));
}

describe("usePolling", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-14T12:00:00Z"));
    setHidden(false);
  });

  afterEach(() => {
    vi.useRealTimers();
    setHidden(false);
  });

  it("loads immediately and schedules exact cadence only after settlement", async () => {
    let settleFirst: (value: string) => void = () => {};
    const load = vi
      .fn<(signal: AbortSignal) => Promise<string>>()
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            settleFirst = resolve;
          }),
      )
      .mockResolvedValue("second");

    render(<Harness load={load} />);
    expect(load).toHaveBeenCalledOnce();

    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(load).toHaveBeenCalledOnce();

    settleFirst("first");
    await flush();
    expect(screen.getByTestId("data")).toHaveTextContent("first");

    await act(async () => {
      vi.advanceTimersByTime(14_999);
    });
    expect(load).toHaveBeenCalledOnce();
    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("pauses while hidden and performs one fresh request when visible", async () => {
    setHidden(true);
    const load = vi.fn().mockResolvedValue("visible");
    render(<Harness load={load} />);
    expect(load).not.toHaveBeenCalled();

    setHidden(false);
    await flush();
    expect(load).toHaveBeenCalledOnce();

    setHidden(true);
    await act(async () => {
      vi.advanceTimersByTime(120_000);
    });
    expect(load).toHaveBeenCalledOnce();

    setHidden(false);
    await flush();
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("aborts requests on key changes and unmount", async () => {
    const signals: AbortSignal[] = [];
    const load = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return new Promise<string>(() => {});
    });
    const view = render(<Harness load={load} resourceKey="health" />);
    expect(signals[0]?.aborted).toBe(false);

    view.rerender(<Harness load={load} resourceKey="current" />);
    expect(signals[0]?.aborted).toBe(true);
    expect(load).toHaveBeenCalledTimes(2);

    view.unmount();
    expect(signals[1]?.aborted).toBe(true);
  });

  it("drops a deferred response after logout advances the session generation", async () => {
    const sessionGeneration = { current: 0 };
    const onData = vi.fn();
    let settle: (value: string) => void = () => {};
    const load = vi.fn(
      () =>
        new Promise<string>((resolve) => {
          settle = resolve;
        }),
    );
    render(
      <Harness
        load={load}
        onData={onData}
        sessionGeneration={sessionGeneration}
      />,
    );

    sessionGeneration.current += 1;
    settle("late-session-data");
    await flush();

    expect(onData).not.toHaveBeenCalled();
    expect(screen.getByTestId("data")).toHaveTextContent("none");
  });

  it("drops a deferred sibling response after a 401 terminal cleanup", async () => {
    const sessionGeneration = { current: 0 };
    const onData = vi.fn();
    let settle: (value: string) => void = () => {};
    const deferredLoad = vi.fn(
      () =>
        new Promise<string>((resolve) => {
          settle = resolve;
        }),
    );
    const onUnauthorized = vi.fn(() => {
      sessionGeneration.current += 1;
    });

    render(
      <>
        <Harness
          load={deferredLoad}
          onData={onData}
          resourceKey="health"
          sessionGeneration={sessionGeneration}
        />
        <Harness
          load={() => Promise.reject(new ApiError(401, null))}
          onUnauthorized={onUnauthorized}
          resourceKey="current"
          sessionGeneration={sessionGeneration}
        />
      </>,
    );
    await flush();
    expect(onUnauthorized).toHaveBeenCalledOnce();

    settle("late-session-data");
    await flush();

    expect(onData).not.toHaveBeenCalled();
    expect(screen.getAllByTestId("data")).toHaveLength(2);
    for (const data of screen.getAllByTestId("data")) {
      expect(data).toHaveTextContent("none");
    }
  });

  it("deduplicates manual refresh while a request is in flight", async () => {
    let settle: (value: string) => void = () => {};
    const load = vi.fn(
      () =>
        new Promise<string>((resolve) => {
          settle = resolve;
        }),
    );
    render(<Harness load={load} />);

    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(load).toHaveBeenCalledOnce();

    settle("ready");
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("honors Retry-After and disables refresh until the absolute deadline", async () => {
    const load = vi
      .fn()
      .mockRejectedValueOnce(new ApiError(429, 10))
      .mockResolvedValueOnce("recovered");
    render(<Harness load={load} />);
    await flush();

    expect(screen.getByTestId("state")).toHaveTextContent("rate-limited");
    expect(screen.getByTestId("retry")).toHaveTextContent(
      String(Date.now() + 10_000),
    );
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(load).toHaveBeenCalledOnce();

    await act(async () => {
      vi.advanceTimersByTime(9_999);
    });
    expect(load).toHaveBeenCalledOnce();
    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    expect(load).toHaveBeenCalledTimes(2);
    await flush();
    expect(screen.getByTestId("state")).toHaveTextContent("ready");
  });

  it.each([
    [503, "service unavailable"],
    [0, "network failure"],
  ])("retains stale in-memory data after %i %s", async (status) => {
    const load = vi
      .fn()
      .mockResolvedValueOnce("last-good")
      .mockRejectedValueOnce(new ApiError(status, null));
    render(<Harness load={load} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await flush();

    expect(screen.getByTestId("state")).toHaveTextContent("stale");
    expect(screen.getByTestId("data")).toHaveTextContent("last-good");
  });

  it("treats a 400 as a closed-contract error without stale data", async () => {
    const load = vi.fn().mockRejectedValue(new ApiError(400, null));
    render(<Harness load={load} />);
    await flush();

    expect(screen.getByTestId("state")).toHaveTextContent("error");
    expect(screen.getByTestId("data")).toHaveTextContent("none");
  });

  it("surfaces a response-contract mismatch as an error instead of stale data", async () => {
    const load = vi.fn().mockRejectedValue(new ApiContractError());
    render(<Harness load={load} />);
    await flush();

    expect(screen.getByTestId("state")).toHaveTextContent("error");
    expect(screen.getByTestId("data")).toHaveTextContent("none");
  });

  it.each([
    [401, "unauthorized"],
    [403, "forbidden"],
  ])("clears data and dispatches the %i %s boundary", async (status) => {
    const onUnauthorized = vi.fn();
    const onForbidden = vi.fn();
    const load = vi
      .fn()
      .mockResolvedValueOnce("sensitive-metrics")
      .mockRejectedValueOnce(new ApiError(status, null));
    render(
      <Harness
        load={load}
        onForbidden={onForbidden}
        onUnauthorized={onUnauthorized}
      />,
    );
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await flush();

    expect(screen.getByTestId("data")).toHaveTextContent("none");
    expect(onUnauthorized).toHaveBeenCalledTimes(status === 401 ? 1 : 0);
    expect(onForbidden).toHaveBeenCalledTimes(status === 403 ? 1 : 0);
  });

  it("never treats polling as operator activity", async () => {
    const activity = vi.fn();
    window.addEventListener("admin-operator-activity", activity);
    const load = vi.fn().mockResolvedValue("healthy");
    render(<Harness intervalMs={1_000} load={load} />);
    await flush();
    await act(async () => {
      vi.advanceTimersByTime(3_000);
    });
    await flush();

    expect(load.mock.calls.length).toBeGreaterThan(1);
    expect(activity).not.toHaveBeenCalled();
    window.removeEventListener("admin-operator-activity", activity);
  });
});
