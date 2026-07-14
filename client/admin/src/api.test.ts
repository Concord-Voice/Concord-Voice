import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { api, ApiError } from "./api";
import type { MetricKey, SeriesWindow } from "./contracts";
import {
  countersFixture,
  currentFixture,
  healthFixture,
  seriesFixture,
} from "./test/fixtures";
import { server } from "./test/server";

const base = "http://localhost";

function json(data: unknown, status = 200, headers?: Record<string, string>) {
  return HttpResponse.json(data as Parameters<typeof HttpResponse.json>[0], {
    status,
    headers,
  });
}

function rejectWithApiError(promise: Promise<unknown>) {
  return expect(promise).rejects.toMatchObject({
    name: "ApiError",
    message: "Admin Portal request failed",
  });
}

function rejectWithContractError(promise: Promise<unknown>) {
  return expect(promise).rejects.toMatchObject({
    name: "ApiContractError",
    message: "Admin Portal response did not match the request",
  });
}

describe("api", () => {
  it("uses fixed same-origin requests and validates metric responses", async () => {
    const seen: Request[] = [];
    server.use(
      http.get(`${base}/admin/api/v1/health`, ({ request }) => {
        seen.push(request);
        return json(healthFixture());
      }),
      http.get(`${base}/admin/api/v1/metrics/current`, ({ request }) => {
        seen.push(request);
        return json(currentFixture());
      }),
      http.get(`${base}/admin/api/v1/counters`, ({ request }) => {
        seen.push(request);
        return json(countersFixture());
      }),
      http.get(`${base}/admin/api/v1/metrics/series`, ({ request }) => {
        seen.push(request);
        expect(new URL(request.url).search).toBe(
          "?key=host_cpu_percent&window=7d",
        );
        return json(seriesFixture("7d"));
      }),
    );
    const controller = new AbortController();
    const fetchSpy = vi.spyOn(globalThis, "fetch");

    await expect(api.getHealth({ signal: controller.signal })).resolves.toEqual(
      healthFixture(),
    );
    await expect(api.getCurrent()).resolves.toEqual(currentFixture());
    await expect(api.getCounters()).resolves.toEqual(countersFixture());
    await expect(
      api.getSeries("host_cpu_percent", "7d", { signal: controller.signal }),
    ).resolves.toEqual(seriesFixture("7d"));

    expect(fetchSpy.mock.calls.map(([input]) => input)).toEqual([
      "/admin/api/v1/health",
      "/admin/api/v1/metrics/current",
      "/admin/api/v1/counters",
      "/admin/api/v1/metrics/series?key=host_cpu_percent&window=7d",
    ]);
    expect(fetchSpy.mock.calls[0][1]).toMatchObject({
      credentials: "same-origin",
      signal: controller.signal,
    });
    expect(seen).toHaveLength(4);
    expect(seen.every((request) => request.credentials === "same-origin")).toBe(
      true,
    );
    expect(seen.every((request) => !request.headers.has("content-type"))).toBe(
      true,
    );
  });

  it("rejects a series response for a different metric key", async () => {
    server.use(
      http.get(`${base}/admin/api/v1/metrics/series`, () =>
        json(seriesFixture("7d")),
      ),
    );

    await rejectWithContractError(api.getSeries("http_requests_total", "7d"));
  });

  it("rejects a series response for a different window", async () => {
    server.use(
      http.get(`${base}/admin/api/v1/metrics/series`, () =>
        json(seriesFixture("7d")),
      ),
    );

    await rejectWithContractError(api.getSeries("host_cpu_percent", "24h"));
  });

  it("sends only JSON same-origin bodies to fixed auth and enrollment paths", async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, async ({ request }) => {
        bodies.push(await request.json());
        return json({
          handle: "login-handle",
          publicKey: { publicKey: publicKeyRequest() },
        });
      }),
      http.post(`${base}/admin/api/v1/auth/webauthn`, async ({ request }) => {
        bodies.push(await request.json());
        return json({ status: "authenticated" });
      }),
      http.post(`${base}/admin/api/v1/enroll/begin`, async ({ request }) => {
        bodies.push(await request.json());
        return json({
          handle: "enroll-handle",
          publicKey: { publicKey: publicKeyCreation() },
        });
      }),
      http.post(`${base}/admin/api/v1/enroll/finish`, async ({ request }) => {
        bodies.push(await request.json());
        return json({ status: "enrolled" });
      }),
      http.post(`${base}/admin/api/v1/auth/logout`, ({ request }) => {
        bodies.push(request.body);
        return json({ status: "logged out" });
      }),
    );
    const fetchSpy = vi.spyOn(globalThis, "fetch");

    await api.passwordLogin("operator", "correct horse");
    await api.webauthnLogin("login-handle", { id: "cred" });
    await api.enrollBegin("operator", "correct horse", "token");
    await api.enrollFinish("enroll-handle", { id: "cred" }, "primary key");
    await api.logout();

    expect(fetchSpy.mock.calls.map(([input]) => input)).toEqual([
      "/admin/api/v1/auth/password",
      "/admin/api/v1/auth/webauthn",
      "/admin/api/v1/enroll/begin",
      "/admin/api/v1/enroll/finish",
      "/admin/api/v1/auth/logout",
    ]);
    expect(
      fetchSpy.mock.calls.slice(0, 4).every(([, init]) => {
        const headers = new Headers(init?.headers);
        return headers.get("content-type") === "application/json";
      }),
    ).toBe(true);
    expect(bodies).toEqual([
      { username: "operator", password: "correct horse" }, // pragma: allowlist secret
      { handle: "login-handle", assertion: { id: "cred" } },
      { username: "operator", password: "correct horse", token: "token" }, // pragma: allowlist secret
      {
        handle: "enroll-handle",
        attestation: { id: "cred" },
        credential_name: "primary key",
      },
      null,
    ]);
  });

  it("rejects open or malformed auth and enrollment success payloads", async () => {
    server.use(
      http.post(`${base}/admin/api/v1/auth/password`, () =>
        json({
          handle: "login-handle",
          publicKey: { publicKey: publicKeyRequest() },
          unexpected: true,
        }),
      ),
      http.post(`${base}/admin/api/v1/auth/webauthn`, () =>
        json({ status: "authenticated", unexpected: true }),
      ),
      http.post(`${base}/admin/api/v1/enroll/begin`, () =>
        json({
          handle: "enroll-handle",
          publicKey: { publicKey: publicKeyCreation() },
          unexpected: true,
        }),
      ),
      http.post(`${base}/admin/api/v1/enroll/finish`, () =>
        json({ status: "active" }),
      ),
      http.post(`${base}/admin/api/v1/auth/logout`, () =>
        json({ status: "logged out", unexpected: true }),
      ),
    );

    await expect(
      api.passwordLogin("operator", "correct horse"),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(
      api.webauthnLogin("login-handle", { id: "credential" }),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(
      api.enrollBegin("operator", "correct horse", "enroll-token"),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(
      api.enrollFinish("enroll-handle", { id: "credential" }, "primary"),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(api.logout()).rejects.toBeInstanceOf(ApiError);
  });

  it.each([
    [401, null],
    [403, null],
    [429, 12],
    [503, null],
  ])("maps HTTP %i to a fixed local ApiError", async (status, retryAfter) => {
    server.use(
      http.get(`${base}/admin/api/v1/health`, () =>
        json(
          { error: "password token assertion session leaked body" },
          status,
          status === 429 ? { "Retry-After": "12" } : undefined,
        ),
      ),
    );

    await rejectWithApiError(api.getHealth());
    await api.getHealth().catch((error: unknown) => {
      expect(error).toBeInstanceOf(ApiError);
      expect(error).toMatchObject({ status, retryAfter });
      expect(String(error)).not.toMatch(/password|token|assertion|session/i);
    });
  });

  it.each([
    ["-1", null],
    ["2.5", null],
    ["3601", null],
    ["3600", 3600],
  ])("bounds Retry-After %s", async (header, retryAfter) => {
    server.use(
      http.get(`${base}/admin/api/v1/health`, () =>
        json({}, 429, { "Retry-After": header }),
      ),
    );

    await api.getHealth().catch((error: unknown) => {
      expect(error).toMatchObject({ status: 429, retryAfter });
    });
  });

  it.each([
    ["network failure", () => HttpResponse.error()],
    ["malformed JSON", () => new HttpResponse("{", { status: 200 })],
    [
      "contract failure",
      () => json({ ...healthFixture(), node_id: "operator@example.com" }),
    ],
  ])(
    "rejects %s without leaking response material",
    async (_name, response) => {
      server.use(http.get(`${base}/admin/api/v1/health`, response));

      await expect(api.getHealth()).rejects.toBeInstanceOf(ApiError);
      await api.getHealth().catch((error: unknown) => {
        expect(String(error)).not.toMatch(
          /operator@example\.com|password|token/i,
        );
      });
    },
  );
});

function publicKeyRequest() {
  return {
    challenge: "AQID",
    rpId: "localhost",
    allowCredentials: [{ type: "public-key", id: "BAUG" }],
    userVerification: "required",
  };
}

function publicKeyCreation() {
  return {
    challenge: "AQID",
    rp: { id: "localhost", name: "Concord Voice" },
    user: { id: "BAUG", name: "operator", displayName: "operator" },
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
  };
}

function assertMetricKey(_key: MetricKey) {}
function assertSeriesWindow(_window: SeriesWindow) {}

assertMetricKey("host_cpu_percent");
assertSeriesWindow("7d");
