import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, api } from "./api";
import type { AdminCountersResponse } from "./contracts";
import {
  DEFAULT_THRESHOLDS,
  FONT_STORAGE_KEY,
  THRESHOLD_STORAGE_KEY,
} from "./preferences";
import { Portal } from "./Portal";
import {
  countersFixture,
  currentFixture,
  healthFixture,
  NODE_ID,
  seriesFixture,
} from "./test/fixtures";

const minute = 60_000;

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

function throughputCounters(
  sampledAt: string,
  messages: number,
  uploads: number,
): AdminCountersResponse {
  return {
    node_id: NODE_ID,
    counters: [
      {
        metric_key: "channel_messages_total",
        source: "control",
        unit: "count",
        kind: "counter",
        value: messages,
        sampled_at: sampledAt,
      },
      {
        metric_key: "dm_messages_total",
        source: "control",
        unit: "count",
        kind: "counter",
        value: 0,
        sampled_at: sampledAt,
      },
      {
        metric_key: "media_uploads_total",
        source: "control",
        unit: "count",
        kind: "counter",
        value: uploads,
        sampled_at: sampledAt,
      },
    ],
  };
}

function messageRateCard(): HTMLElement {
  return screen.getByRole("article", { name: "Message rate" });
}

describe("Portal", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-14T12:00:00Z"));
    setHidden(false);
    vi.spyOn(api, "getHealth").mockResolvedValue(healthFixture());
    vi.spyOn(api, "getCurrent").mockResolvedValue(currentFixture());
    vi.spyOn(api, "getCounters").mockResolvedValue(countersFixture());
    vi.spyOn(api, "getSeries").mockResolvedValue(seriesFixture());
    vi.spyOn(api, "logout").mockResolvedValue();
  });

  afterEach(() => {
    vi.useRealTimers();
    setHidden(false);
  });

  it("starts with usable health and a fixed six-workspace command rail", () => {
    vi.spyOn(api, "logout").mockResolvedValue();
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);

    expect(screen.getAllByText(NODE_ID)).toHaveLength(2);
    expect(
      screen.getByRole("navigation", { name: "Admin workspaces" }),
    ).toBeVisible();
    for (const workspace of [
      "Host Overview",
      "Services",
      "Users & Activity",
      "Counters",
      "Time Series",
      "Health & Changes",
    ]) {
      expect(screen.getByRole("button", { name: workspace })).toBeVisible();
    }
    expect(
      screen.getByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "Services" }));
    expect(screen.getByRole("heading", { name: "Services" })).toBeVisible();
  });

  it.each(["pointerdown", "keydown", "touchstart"])(
    "%s resets operator idle without a browser-storage write",
    async (eventName) => {
      const logout = vi.spyOn(api, "logout").mockResolvedValue();
      render(
        <Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />,
      );

      await act(async () => {
        vi.advanceTimersByTime(24 * minute);
      });
      window.dispatchEvent(new Event(eventName));
      await act(async () => {
        vi.advanceTimersByTime(6 * minute);
      });

      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
      expect(logout).not.toHaveBeenCalled();
      expect(localStorage).toHaveLength(0);
      expect(sessionStorage).toHaveLength(0);
    },
  );

  it("warns at 25 minutes and interaction dismisses the accessible warning", async () => {
    vi.spyOn(api, "logout").mockResolvedValue();
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);

    await act(async () => {
      vi.advanceTimersByTime(25 * minute);
    });
    expect(screen.getByRole("alertdialog")).toHaveTextContent(
      "Your Admin Portal session is about to end",
    );

    fireEvent.click(screen.getByRole("button", { name: "Stay signed in" }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(6 * minute);
    });
    expect(api.logout).not.toHaveBeenCalled();
  });

  it("restores prior focus when the operator stays signed in", async () => {
    vi.spyOn(api, "logout").mockResolvedValue();
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);
    const services = screen.getByRole("button", { name: "Services" });
    services.focus();

    await act(async () => {
      vi.advanceTimersByTime(25 * minute);
    });
    const warning = screen.getByRole("alertdialog");
    expect(warning).toHaveFocus();
    expect(warning).not.toHaveAttribute("aria-modal");

    fireEvent.click(screen.getByRole("button", { name: "Stay signed in" }));

    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(services).toHaveFocus();
  });

  it("ignores background activity and logs out exactly once at 30 minutes", async () => {
    const logout = vi.spyOn(api, "logout").mockResolvedValue();
    const onSessionEnded = vi.fn();
    render(
      <Portal
        initialHealth={healthFixture()}
        onSessionEnded={onSessionEnded}
      />,
    );

    await act(async () => {
      vi.advanceTimersByTime(24 * minute);
    });
    window.dispatchEvent(new Event("admin-operator-activity"));
    await act(async () => {
      vi.advanceTimersByTime(6 * minute);
    });
    await flush();

    expect(logout).toHaveBeenCalledOnce();
    expect(onSessionEnded).toHaveBeenCalledOnce();
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(30 * minute);
    });
    expect(logout).toHaveBeenCalledOnce();
  });

  it("compares timestamps and logs out when a hidden tab returns after deadline", async () => {
    const logout = vi.spyOn(api, "logout").mockResolvedValue();
    const onSessionEnded = vi.fn();
    setHidden(true);
    render(
      <Portal
        initialHealth={healthFixture()}
        onSessionEnded={onSessionEnded}
      />,
    );

    vi.setSystemTime(new Date("2026-07-14T12:31:00Z"));
    setHidden(false);
    await flush();

    expect(logout).toHaveBeenCalledOnce();
    expect(onSessionEnded).toHaveBeenCalledOnce();
  });

  it("clears health before an explicit logout request completes", async () => {
    let finishLogout: () => void = () => {};
    vi.spyOn(api, "logout").mockReturnValue(
      new Promise<void>((resolve) => {
        finishLogout = resolve;
      }),
    );
    const onSessionEnded = vi.fn();
    render(
      <Portal
        initialHealth={healthFixture()}
        onSessionEnded={onSessionEnded}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();
    expect(onSessionEnded).not.toHaveBeenCalled();

    finishLogout();
    await flush();
    expect(onSessionEnded).toHaveBeenCalledOnce();
  });

  it.each([
    ["service failure", new ApiError(503, null)],
    ["network failure", new ApiError(0, null)],
  ])("keeps logout retryable after a %s", async (_label, cause) => {
    vi.mocked(api.logout).mockRejectedValueOnce(cause).mockResolvedValueOnce();
    const onSessionEnded = vi.fn();
    render(
      <Portal
        initialHealth={healthFixture()}
        onSessionEnded={onSessionEnded}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await flush();

    expect(onSessionEnded).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Could not confirm sign out. Your session may still be active.",
    );
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Retry sign out" }));
    await flush();

    expect(api.logout).toHaveBeenCalledTimes(2);
    expect(onSessionEnded).toHaveBeenCalledOnce();
  });

  it("renders an authenticated unavailable shell without inventing health", () => {
    vi.spyOn(api, "logout").mockResolvedValue();
    render(
      <Portal
        initialHealth={null}
        initialState="stale"
        onSessionEnded={vi.fn()}
      />,
    );

    expect(
      screen.getByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    expect(
      screen.getByText("Live telemetry is temporarily unavailable."),
    ).toBeVisible();
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();
  });

  it("polls only resources used by the visible workspace at fixed cadences", async () => {
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);
    await flush();

    expect(api.getHealth).toHaveBeenCalledOnce();
    expect(api.getCurrent).toHaveBeenCalledOnce();
    expect(api.getSeries).toHaveBeenCalledOnce();
    expect(api.getSeries).toHaveBeenCalledWith(
      "host_cpu_percent",
      "24h",
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(api.getCounters).not.toHaveBeenCalled();

    await act(async () => {
      vi.advanceTimersByTime(15_000);
      await Promise.resolve();
    });
    expect(api.getHealth).toHaveBeenCalledTimes(2);
    expect(api.getCurrent).toHaveBeenCalledTimes(2);
    expect(api.getSeries).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole("button", { name: "Counters" }));
    await flush();
    expect(api.getCounters).toHaveBeenCalledOnce();

    await act(async () => {
      vi.advanceTimersByTime(30_000);
      await Promise.resolve();
    });
    expect(api.getCounters).toHaveBeenCalledTimes(2);
    expect(api.getSeries).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole("button", { name: "Time Series" }));
    await flush();
    expect(api.getSeries).toHaveBeenCalledTimes(2);
  });

  it("polls current metrics and counters only while Users & Activity is visible", async () => {
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);
    await flush();
    vi.mocked(api.getCurrent).mockClear();
    vi.mocked(api.getCounters).mockClear();
    vi.mocked(api.getSeries).mockClear();

    fireEvent.click(screen.getByRole("button", { name: "Users & Activity" }));
    await flush();

    expect(api.getCurrent).toHaveBeenCalledOnce();
    expect(api.getCounters).toHaveBeenCalledOnce();
    expect(api.getSeries).not.toHaveBeenCalled();
  });

  it.each([
    ["stale", new ApiError(503, null)],
    ["rate-limited", new ApiError(429, 30)],
    ["error", new ApiError(400, null)],
  ])(
    "requires a fresh counter baseline after polling enters %s state",
    async (_state, cause) => {
      vi.mocked(api.getCounters)
        .mockResolvedValueOnce(
          throughputCounters("2026-07-14T12:00:00Z", 100, 10),
        )
        .mockResolvedValueOnce(
          throughputCounters("2026-07-14T12:00:30Z", 160, 40),
        )
        .mockRejectedValueOnce(cause)
        .mockResolvedValueOnce(
          throughputCounters("2026-07-14T12:01:30Z", 280, 100),
        )
        .mockResolvedValueOnce(
          throughputCounters("2026-07-14T12:02:00Z", 340, 130),
        );
      render(
        <Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />,
      );

      fireEvent.click(screen.getByRole("button", { name: "Users & Activity" }));
      await flush();
      expect(
        within(messageRateCard()).getByText("Unavailable", {
          selector: "strong",
        }),
      ).toBeVisible();

      await act(async () => {
        vi.advanceTimersByTime(30_000);
        await Promise.resolve();
      });
      expect(within(messageRateCard()).getByText("120 / minute")).toBeVisible();

      await act(async () => {
        vi.advanceTimersByTime(30_000);
        await Promise.resolve();
      });
      expect(
        within(messageRateCard()).getByText("Unavailable", {
          selector: "strong",
        }),
      ).toBeVisible();

      await act(async () => {
        vi.advanceTimersByTime(30_000);
        await Promise.resolve();
      });
      expect(
        within(messageRateCard()).getByText("Unavailable", {
          selector: "strong",
        }),
      ).toBeVisible();

      expect(api.getCounters).toHaveBeenCalledTimes(4);
      await act(async () => {
        vi.advanceTimersByTime(30_000);
        await Promise.resolve();
      });
      expect(api.getCounters).toHaveBeenCalledTimes(5);
      expect(within(messageRateCard()).getByText("120 / minute")).toBeVisible();
    },
  );

  it("requires a fresh counter baseline after reopening the workspace", async () => {
    vi.mocked(api.getCounters)
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:00:00Z", 100, 10),
      )
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:00:30Z", 160, 40),
      )
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:01:00Z", 220, 70),
      )
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:01:30Z", 280, 100),
      );
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);

    fireEvent.click(screen.getByRole("button", { name: "Users & Activity" }));
    await flush();
    await act(async () => {
      vi.advanceTimersByTime(30_000);
      await Promise.resolve();
    });
    expect(within(messageRateCard()).getByText("120 / minute")).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "Host Overview" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Users & Activity" }));
    await flush();

    expect(api.getCounters).toHaveBeenCalledTimes(3);
    expect(
      within(messageRateCard()).getByText("Unavailable", {
        selector: "strong",
      }),
    ).toBeVisible();

    await act(async () => {
      vi.advanceTimersByTime(30_000);
      await Promise.resolve();
    });
    expect(api.getCounters).toHaveBeenCalledTimes(4);
    expect(within(messageRateCard()).getByText("120 / minute")).toBeVisible();
  });

  it("requires a fresh counter baseline after returning to a hidden tab", async () => {
    vi.mocked(api.getCounters)
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:00:00Z", 100, 10),
      )
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:00:30Z", 160, 40),
      )
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:01:30Z", 280, 100),
      )
      .mockResolvedValueOnce(
        throughputCounters("2026-07-14T12:02:00Z", 340, 130),
      );
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);

    fireEvent.click(screen.getByRole("button", { name: "Users & Activity" }));
    await flush();
    await act(async () => {
      vi.advanceTimersByTime(30_000);
      await Promise.resolve();
    });
    expect(within(messageRateCard()).getByText("120 / minute")).toBeVisible();

    setHidden(true);
    await act(async () => {
      vi.advanceTimersByTime(60_000);
      await Promise.resolve();
    });
    expect(api.getCounters).toHaveBeenCalledTimes(2);

    setHidden(false);
    await flush();
    expect(api.getCounters).toHaveBeenCalledTimes(3);
    expect(
      within(messageRateCard()).getByText("Unavailable", {
        selector: "strong",
      }),
    ).toBeVisible();

    await act(async () => {
      vi.advanceTimersByTime(30_000);
      await Promise.resolve();
    });
    expect(api.getCounters).toHaveBeenCalledTimes(4);
    expect(within(messageRateCard()).getByText("120 / minute")).toBeVisible();
  });

  it("keeps timestamp display in memory and resets to UTC on remount", () => {
    const { unmount } = render(
      <Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />,
    );
    const utc = screen.getByRole("button", { name: "UTC" });
    const local = screen.getByRole("button", { name: "Local" });
    expect(utc).toHaveAttribute("aria-pressed", "true");

    fireEvent.click(local);
    expect(local).toHaveAttribute("aria-pressed", "true");
    expect(localStorage).toHaveLength(0);
    unmount();

    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);
    expect(screen.getByRole("button", { name: "UTC" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  it("routes polling 401 to local login without an extra logout request", async () => {
    vi.mocked(api.getCurrent).mockRejectedValueOnce(new ApiError(401, null));
    const onSessionEnded = vi.fn();

    render(
      <Portal
        initialHealth={healthFixture()}
        onSessionEnded={onSessionEnded}
      />,
    );
    await flush();

    expect(onSessionEnded).toHaveBeenCalledOnce();
    expect(api.logout).not.toHaveBeenCalled();
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();
  });

  it("dispatches one terminal boundary when visible pollers fail together", async () => {
    vi.mocked(api.getHealth).mockRejectedValueOnce(new ApiError(401, null));
    vi.mocked(api.getCurrent).mockRejectedValueOnce(new ApiError(401, null));
    vi.mocked(api.getSeries).mockRejectedValueOnce(new ApiError(401, null));
    const onSessionEnded = vi.fn();

    render(
      <Portal
        initialHealth={healthFixture()}
        onSessionEnded={onSessionEnded}
      />,
    );
    await flush();

    expect(onSessionEnded).toHaveBeenCalledOnce();
    expect(api.logout).not.toHaveBeenCalled();
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();
  });

  it("routes polling 403 to a document reload boundary", async () => {
    vi.mocked(api.getCurrent).mockRejectedValueOnce(new ApiError(403, null));
    const onForbidden = vi.fn();

    render(
      <Portal
        initialHealth={healthFixture()}
        onForbidden={onForbidden}
        onSessionEnded={vi.fn()}
      />,
    );
    await flush();

    expect(onForbidden).toHaveBeenCalledOnce();
    expect(screen.queryByText(NODE_ID)).not.toBeInTheDocument();
  });

  it("shows the absolute retry boundary and disables visible refresh on 429", async () => {
    vi.mocked(api.getCurrent).mockRejectedValueOnce(new ApiError(429, 60));
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);
    await flush();

    expect(screen.getByText(/requests paused until/i)).toBeVisible();
    expect(
      screen.getByRole("button", { name: "Refresh visible workspace" }),
    ).toBeDisabled();
  });

  it("uses semantic selected navigation and a single visible workspace heading", async () => {
    const { container } = render(
      <Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />,
    );
    await flush();

    const host = screen.getByRole("button", { name: "Host Overview" });
    expect(host).toHaveAttribute("aria-current", "page");
    expect(container.querySelector("header")).not.toBeNull();
    expect(
      screen.getByRole("navigation", { name: "Admin workspaces" }),
    ).toBeVisible();
    expect(screen.getByRole("main")).toBeVisible();
    expect(container.querySelectorAll("h1")).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: "Services" }));
    expect(host).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("button", { name: "Services" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("heading", { name: "Services" })).toBeVisible();
  });

  it("validates, resets, saves settings, and restores trigger focus", async () => {
    Object.defineProperties(HTMLDialogElement.prototype, {
      close: {
        configurable: true,
        value(this: HTMLDialogElement) {
          this.open = false;
        },
      },
      showModal: {
        configurable: true,
        value(this: HTMLDialogElement) {
          this.open = true;
        },
      },
    });
    render(<Portal initialHealth={healthFixture()} onSessionEnded={vi.fn()} />);
    await flush();
    const settings = screen.getByRole("button", { name: "Settings" });
    settings.focus();
    fireEvent.click(settings);

    const dialog = screen.getByRole("dialog", { name: "Portal settings" });
    const hostWarning = within(dialog).getByLabelText("Host CPU warning");
    fireEvent.change(hostWarning, { target: { value: "95" } });
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Save settings" }),
    );

    expect(within(dialog).getByRole("alert")).toHaveTextContent(
      "Warning must be below critical",
    );
    expect(localStorage.getItem(THRESHOLD_STORAGE_KEY)).toBeNull();

    fireEvent.click(
      within(dialog).getByRole("button", { name: "Reset defaults" }),
    );
    expect(hostWarning).toHaveValue(DEFAULT_THRESHOLDS.hostCpu.warning);
    const serviceWarning = within(dialog).getByLabelText("Service CPU warning");
    const serviceCritical = within(dialog).getByLabelText(
      "Service CPU critical",
    );
    expect(serviceWarning).toHaveAttribute("max", "1000000");
    expect(serviceCritical).toHaveAttribute("max", "1000000");
    fireEvent.change(serviceWarning, { target: { value: "150" } });
    fireEvent.change(serviceCritical, { target: { value: "250" } });
    fireEvent.change(within(dialog).getByLabelText("Interface font"), {
      target: { value: "open-dyslexic" },
    });
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Save settings" }),
    );

    expect(
      JSON.parse(localStorage.getItem(THRESHOLD_STORAGE_KEY) ?? "null"),
    ).toEqual({
      ...DEFAULT_THRESHOLDS,
      serviceCpu: { warning: 150, critical: 250 },
    });
    expect(localStorage.getItem(FONT_STORAGE_KEY)).toBe("open-dyslexic");
    expect(screen.getByTestId("portal-shell")).toHaveAttribute(
      "data-font",
      "open-dyslexic",
    );
    expect(settings).toHaveFocus();
  });
});
