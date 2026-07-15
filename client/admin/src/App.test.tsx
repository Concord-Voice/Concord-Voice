import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "./api";
import { App } from "./App";
import type { AdminHealthResponse } from "./contracts";
import {
  countersFixture,
  currentFixture,
  healthFixture,
  seriesFixture,
} from "./test/fixtures";
import { server } from "./test/server";

const base = "http://localhost";

describe("App", () => {
  beforeEach(() => {
    server.use(
      http.get(`${base}/admin/api/v1/metrics/current`, () =>
        HttpResponse.json(currentFixture()),
      ),
      http.get(`${base}/admin/api/v1/counters`, () =>
        HttpResponse.json(countersFixture()),
      ),
      http.get(`${base}/admin/api/v1/metrics/series`, () =>
        HttpResponse.json(seriesFixture()),
      ),
    );
  });

  it("aborts the initial health probe when the application unmounts", async () => {
    history.replaceState(null, "", "/admin/");
    let signal: AbortSignal | undefined;
    vi.spyOn(api, "getHealth").mockImplementation((options) => {
      signal = options?.signal;
      return new Promise<AdminHealthResponse>(() => undefined);
    });

    const { unmount } = render(<App />);
    await vi.waitFor(() => expect(signal).toBeInstanceOf(AbortSignal));

    unmount();

    expect(signal?.aborted).toBe(true);
  });

  it("labels the authenticated product Admin Portal without a hosted codename", async () => {
    history.replaceState(null, "", "/admin/");
    let requests = 0;
    server.use(
      http.get(`${base}/admin/api/v1/health`, () => {
        requests += 1;
        return HttpResponse.json(healthFixture());
      }),
    );

    render(<App />);

    expect(await screen.findAllByText(healthFixture().node_id)).toHaveLength(2);
    expect(screen.getByText("Admin Portal")).toBeVisible();
    expect(
      screen.getByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    expect(screen.queryByText(/sovereign portal/i)).not.toBeInTheDocument();
    // The portal poll is dispatched from an effect after the commit that renders
    // node_id, so the second request may not be counted yet — wait for it.
    await waitFor(() => expect(requests).toBe(2));
    expect(screen.getAllByRole("main")).toHaveLength(1);
  });

  it("reloads through the App boundary when workspace polling returns 403", async () => {
    history.replaceState(null, "", "/admin/");
    let requests = 0;
    server.use(
      http.get(`${base}/admin/api/v1/health`, () => {
        requests += 1;
        return requests === 1
          ? HttpResponse.json(healthFixture())
          : HttpResponse.json({}, { status: 403 });
      }),
    );
    const reload = vi.fn();

    render(<App reload={reload} />);

    await vi.waitFor(() => expect(reload).toHaveBeenCalledOnce());
    // The 403 path schedules the terminal re-render and calls reload() in the same
    // tick, so the portal DOM can still be mounted here. node_id renders twice, and
    // queryByText throws on multiple matches — wait on the DOM with queryAllByText.
    await waitFor(() =>
      expect(screen.queryAllByText(healthFixture().node_id)).toHaveLength(0),
    );
  });

  it("clears local data and reloads the document when Access returns 403", async () => {
    history.replaceState(null, "", "/admin/");
    server.use(
      http.get(`${base}/admin/api/v1/health`, () =>
        HttpResponse.json({}, { status: 403 }),
      ),
    );
    const reload = vi.fn();

    render(<App reload={reload} />);

    await vi.waitFor(() => expect(reload).toHaveBeenCalledOnce());
    expect(screen.queryByText(healthFixture().node_id)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Admin sign in" }),
    ).not.toBeInTheDocument();
  });

  it.each([
    ["rate limit", () => HttpResponse.json({}, { status: 429 })],
    ["service unavailable", () => HttpResponse.json({}, { status: 503 })],
    ["network error", () => HttpResponse.error()],
    [
      "malformed response",
      () => HttpResponse.json({ ...healthFixture(), services: [] }),
    ],
  ])("fails closed after an initial %s", async (_label, response) => {
    history.replaceState(null, "", "/admin/");
    server.use(http.get(`${base}/admin/api/v1/health`, response));

    render(<App />);

    expect(
      await screen.findByRole("heading", {
        name: "Unable to verify session",
      }),
    ).toBeVisible();
    const verificationStatus = screen.getByText(
      "The Admin Portal could not confirm whether you are signed in.",
    );
    expect(verificationStatus.tagName).toBe("OUTPUT");
    expect(verificationStatus).toHaveRole("status");
    expect(screen.getByRole("button", { name: "Retry" })).toBeVisible();
    expect(
      screen.queryByRole("heading", { name: "Host Overview" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("Active")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Admin sign in" }),
    ).not.toBeInTheDocument();
  });

  it("enters the portal when retry positively verifies the session", async () => {
    history.replaceState(null, "", "/admin/");
    let requests = 0;
    server.use(
      http.get(`${base}/admin/api/v1/health`, () => {
        requests += 1;
        return requests === 1
          ? HttpResponse.error()
          : HttpResponse.json(healthFixture());
      }),
    );

    render(<App />);
    await screen.findByRole("heading", { name: "Unable to verify session" });

    await userEvent.click(screen.getByRole("button", { name: "Retry" }));

    expect(
      await screen.findByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    expect(screen.getByText("Active")).toBeVisible();
    expect(
      screen.queryByRole("heading", { name: "Unable to verify session" }),
    ).not.toBeInTheDocument();
  });

  it("returns to local login only after logout clears portal memory", async () => {
    history.replaceState(null, "", "/admin/");
    let finishLogout: () => void = () => {};
    let markLogoutStarted: () => void = () => {};
    const logoutStarted = new Promise<void>((resolve) => {
      markLogoutStarted = resolve;
    });
    server.use(
      http.get(`${base}/admin/api/v1/health`, () =>
        HttpResponse.json(healthFixture()),
      ),
      http.post(`${base}/admin/api/v1/auth/logout`, async () => {
        markLogoutStarted();
        await new Promise<void>((resolve) => {
          finishLogout = resolve;
        });
        return HttpResponse.json({ status: "logged out" });
      }),
    );

    render(<App />);
    expect(await screen.findAllByText(healthFixture().node_id)).toHaveLength(2);

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await logoutStarted;
    expect(screen.queryByText(healthFixture().node_id)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Admin sign in" }),
    ).not.toBeInTheDocument();

    finishLogout();
    expect(
      await screen.findByRole("heading", { name: "Admin sign in" }),
    ).toBeVisible();
  });

  it("shows local login when the session probe is unauthorized", async () => {
    history.replaceState(null, "", "/admin/");
    server.use(
      http.get(`${base}/admin/api/v1/health`, () =>
        HttpResponse.json({}, { status: 401 }),
      ),
    );

    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Admin sign in" }),
    ).toBeVisible();
  });

  it("selects enrollment only for the fixed enrollment path", async () => {
    history.replaceState(
      null,
      "",
      "/admin/enroll?username=operator&token=secret",
    );

    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Enroll administrator" }),
    ).toBeVisible();
  });

  it("requires a fresh session probe after enrollment instead of trusting local state", async () => {
    history.replaceState(
      null,
      "",
      "/admin/enroll?username=operator&token=enroll-secret",
    );
    vi.spyOn(navigator.credentials, "create").mockResolvedValue(
      attestationCredential(),
    );
    server.use(
      http.post(`${base}/admin/api/v1/enroll/begin`, () =>
        HttpResponse.json({
          handle: "enroll-handle",
          publicKey: { publicKey: creationOptions() },
        }),
      ),
      http.post(`${base}/admin/api/v1/enroll/finish`, () =>
        HttpResponse.json({ status: "enrolled" }),
      ),
    );
    const navigate = vi.fn();

    render(<App navigate={navigate} />);
    await userEvent.type(screen.getByLabelText("Password"), "correct horse");
    await userEvent.type(
      screen.getByLabelText("Credential name"),
      "YubiKey primary",
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Enroll security key" }),
    );
    await userEvent.click(
      await screen.findByRole("button", {
        name: "Continue to Admin Portal",
      }),
    );

    expect(navigate).toHaveBeenCalledOnce();
    expect(navigate).toHaveBeenCalledWith("/admin/");
    expect(location.pathname).toBe("/admin/enroll");
    expect(
      screen.queryByText("Operations command rail"),
    ).not.toBeInTheDocument();
  });
});

function creationOptions() {
  return {
    challenge: "AQID",
    rp: { id: "localhost", name: "Concord Voice" },
    user: { id: "BAUG", name: "operator", displayName: "operator" },
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
    attestation: "direct",
  };
}

function attestationCredential(): PublicKeyCredential {
  return {
    id: "credential",
    rawId: new Uint8Array([1, 2, 3]).buffer,
    type: "public-key",
    response: {
      attestationObject: new Uint8Array([4]).buffer,
      clientDataJSON: new Uint8Array([5]).buffer,
    },
    getClientExtensionResults: () => ({}),
  } as unknown as PublicKeyCredential;
}
