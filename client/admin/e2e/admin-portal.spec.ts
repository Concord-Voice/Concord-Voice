import { generateKeyPairSync, randomBytes } from "node:crypto";

import {
  expect,
  test,
  type CDPSession,
  type Page,
  type Route,
} from "@playwright/test";

import {
  COUNTER_METRIC_KEYS,
  METRIC_KEYS,
  type AdminCountersResponse,
  type AdminCurrentResponse,
  type AdminSeriesResponse,
  type MetricKey,
  type MetricKind,
  type MetricSource,
  type MetricUnit,
  type RollupMode,
  type SeriesWindow,
} from "../src/contracts";
import {
  healthFixture,
  NODE_ID,
  SAMPLED_AT,
  seriesFixture,
} from "../src/test/fixtures";

interface VirtualAuthenticator {
  authenticatorId: string;
  session: CDPSession;
}

test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
});

function metricDefinition(metricKey: MetricKey): {
  kind: MetricKind;
  rollup: RollupMode;
  source: MetricSource;
  unit: MetricUnit;
} {
  if (metricKey.startsWith("service_")) {
    if (metricKey.endsWith("_running") || metricKey.endsWith("_healthy")) {
      return { kind: "gauge", rollup: "last", source: "host", unit: "count" };
    }
    if (metricKey.endsWith("_cpu_percent")) {
      return {
        kind: "gauge",
        rollup: "average",
        source: "host",
        unit: "percent",
      };
    }
    return {
      kind: "gauge",
      rollup: "average",
      source: "host",
      unit: "bytes",
    };
  }
  if (metricKey.startsWith("host_")) {
    return {
      kind: "gauge",
      rollup: "average",
      source: "host",
      unit: metricKey === "host_load_1m" ? "load" : "percent",
    };
  }
  if (
    metricKey.startsWith("http_") ||
    metricKey.startsWith("websocket_") ||
    metricKey.startsWith("channel_") ||
    metricKey.startsWith("dm_") ||
    metricKey.startsWith("ops_")
  ) {
    const counter = COUNTER_METRIC_KEYS.includes(
      metricKey as (typeof COUNTER_METRIC_KEYS)[number],
    );
    return {
      kind: counter ? "counter" : "gauge",
      rollup: counter ? "last" : "average",
      source: "control",
      unit: "count",
    };
  }
  if (metricKey === "media_egress_current_bps") {
    return {
      kind: "gauge",
      rollup: "average",
      source: "media",
      unit: "bits_per_second",
    };
  }
  if (metricKey === "media_egress_peak_bps") {
    return {
      kind: "gauge",
      rollup: "last",
      source: "media",
      unit: "bits_per_second",
    };
  }
  if (metricKey === "media_egress_cumulative_bytes") {
    return {
      kind: "counter",
      rollup: "last",
      source: "media",
      unit: "bytes",
    };
  }
  if (metricKey.startsWith("media_participant_hours_")) {
    return {
      kind: "counter",
      rollup: "last",
      source: "media",
      unit: "hours",
    };
  }
  return {
    kind: "gauge",
    rollup:
      metricKey === "media_peak_video_publishers_per_room" ? "last" : "average",
    source: "media",
    unit: "count",
  };
}

function metricValue(metricKey: MetricKey, index: number): number {
  if (metricKey.endsWith("_running") || metricKey.endsWith("_healthy")) {
    return 1;
  }
  if (metricKey.endsWith("_memory_bytes")) {
    return (index + 2) * 64 * 1024 * 1024;
  }
  if (metricKey.includes("_bps")) return (index + 1) * 125_000;
  if (metricKey === "media_egress_cumulative_bytes") return 8_450_000_000;
  if (metricKey.startsWith("media_participant_hours_")) {
    return 120 + index * 3.5;
  }
  if (metricKey === "host_load_1m") return 1.25;
  if (metricKey.endsWith("_percent")) return 18 + (index % 7) * 6.5;
  return (index + 1) * 17;
}

function currentResponse(): AdminCurrentResponse {
  return {
    node_id: NODE_ID,
    metrics: METRIC_KEYS.map((metricKey, index) => ({
      metric_key: metricKey,
      ...metricDefinition(metricKey),
      value: metricValue(metricKey, index),
      sampled_at: SAMPLED_AT,
    })).map(({ rollup: _rollup, ...metric }) => metric),
  };
}

function countersResponse(): AdminCountersResponse {
  return {
    node_id: NODE_ID,
    counters: COUNTER_METRIC_KEYS.map((metricKey, index) => ({
      metric_key: metricKey,
      ...metricDefinition(metricKey),
      kind: "counter" as const,
      value: 10_000 + index * 713,
      sampled_at: SAMPLED_AT,
    })).map(({ rollup: _rollup, ...counter }) => counter),
  };
}

function fullSeriesResponse(
  metricKey: MetricKey,
  window: SeriesWindow,
): AdminSeriesResponse {
  const definition = metricDefinition(metricKey);
  const pointCount = window === "24h" ? 24 : 28;
  const start = Date.parse(SAMPLED_AT) - (pointCount - 1) * 3_600_000;
  return {
    node_id: NODE_ID,
    metric: {
      metric_key: metricKey,
      kind: definition.kind,
      rollup: definition.rollup,
      source: definition.source,
      unit: definition.unit,
    },
    window,
    bucket_seconds: 3600,
    points: Array.from({ length: pointCount }, (_, index) => {
      const value = 36 + Math.sin(index / 2.4) * 12 + index * 0.35;
      return {
        bucket_start: new Date(start + index * 3_600_000).toISOString(),
        maximum: Number((value + 4).toFixed(2)),
        minimum: Number((value - 5).toFixed(2)),
        sample_count: 12,
        value: Number(value.toFixed(2)),
      };
    }),
  };
}

async function fulfillJson(
  route: Route,
  json: unknown,
  status = 200,
): Promise<void> {
  await route.fulfill({
    body: JSON.stringify(json),
    contentType: "application/json",
    status,
  });
}

async function mockAuthenticatedPortal(page: Page): Promise<void> {
  let healthCalls = 0;
  await page.route("**/admin/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/admin/api/v1/health") {
      healthCalls += 1;
      const health = healthFixture();
      if (healthCalls > 1) {
        health.services[1] = {
          ...health.services[1],
          healthy: false,
          state: "degraded",
        };
      }
      await fulfillJson(route, health);
      return;
    }
    if (url.pathname === "/admin/api/v1/metrics/current") {
      await fulfillJson(route, currentResponse());
      return;
    }
    if (url.pathname === "/admin/api/v1/counters") {
      await fulfillJson(route, countersResponse());
      return;
    }
    if (url.pathname === "/admin/api/v1/metrics/series") {
      const metricKey = (url.searchParams.get("key") ??
        "host_cpu_percent") as MetricKey;
      const window = (url.searchParams.get("window") ?? "24h") as SeriesWindow;
      await fulfillJson(route, fullSeriesResponse(metricKey, window));
      return;
    }
    if (url.pathname === "/admin/api/v1/auth/logout") {
      await fulfillJson(route, { status: "logged out" });
      return;
    }
    await route.abort();
  });
}

function base64url(value: Buffer): string {
  return value
    .toString("base64")
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll(/=+$/g, "");
}

async function addVirtualAuthenticator(
  page: Page,
  credentialId?: Buffer,
): Promise<VirtualAuthenticator> {
  const session = await page.context().newCDPSession(page);
  await session.send("WebAuthn.enable");
  const { authenticatorId } = await session.send(
    "WebAuthn.addVirtualAuthenticator",
    {
      options: {
        automaticPresenceSimulation: true,
        hasResidentKey: true,
        hasUserVerification: true,
        isUserVerified: true,
        protocol: "ctap2",
        transport: "usb",
      },
    },
  );

  if (credentialId) {
    const { privateKey } = generateKeyPairSync("ec", {
      namedCurve: "prime256v1",
      privateKeyEncoding: { format: "der", type: "pkcs8" },
      publicKeyEncoding: { format: "der", type: "spki" },
    });
    await session.send("WebAuthn.addCredential", {
      authenticatorId,
      credential: {
        credentialId: credentialId.toString("base64"),
        isResidentCredential: false,
        privateKey: privateKey.toString("base64"),
        rpId: "localhost",
        signCount: 0,
      },
    });
  }

  return { authenticatorId, session };
}

async function removeVirtualAuthenticator(
  virtual: VirtualAuthenticator,
): Promise<void> {
  await virtual.session.send("WebAuthn.removeVirtualAuthenticator", {
    authenticatorId: virtual.authenticatorId,
  });
  await virtual.session.send("WebAuthn.disable");
  await virtual.session.detach();
}

test("password plus WebAuthn signs in and explicit logout clears the session", async ({
  page,
}) => {
  const credentialId = randomBytes(32);
  const challenge = base64url(randomBytes(32));
  const loginPassword = ["browser", "only", "password"].join("-");
  const virtual = await addVirtualAuthenticator(page, credentialId);
  const requests: Array<{ body: unknown; method: string; path: string }> = [];
  let authenticated = false;

  await page.route("**/admin/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const body = request.postDataJSON() as unknown;
    requests.push({ body, method: request.method(), path });

    if (path === "/admin/api/v1/health") {
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify(
          authenticated ? healthFixture() : { error: "unauthorized" },
        ),
        status: authenticated ? 200 : 401,
      });
      return;
    }
    if (path === "/admin/api/v1/auth/password") {
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({
          handle: "browser-login-handle",
          publicKey: {
            publicKey: {
              allowCredentials: [
                {
                  id: base64url(credentialId),
                  transports: ["usb"],
                  type: "public-key",
                },
              ],
              challenge,
              rpId: "localhost",
              userVerification: "required",
            },
          },
        }),
        status: 200,
      });
      return;
    }
    if (path === "/admin/api/v1/auth/webauthn") {
      authenticated = true;
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({ status: "authenticated" }),
        status: 200,
      });
      return;
    }
    if (path === "/admin/api/v1/auth/logout") {
      authenticated = false;
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({ status: "logged out" }),
        status: 200,
      });
      return;
    }
    if (path === "/admin/api/v1/metrics/current") {
      await route.fulfill({ json: currentResponse() });
      return;
    }
    if (path === "/admin/api/v1/counters") {
      await route.fulfill({ json: countersResponse() });
      return;
    }
    if (path === "/admin/api/v1/metrics/series") {
      await route.fulfill({ json: seriesFixture() });
      return;
    }
    await route.abort();
  });

  try {
    await page.goto("/admin/");
    await expect(
      page.getByRole("heading", { name: "Admin sign in" }),
    ).toBeVisible();

    await page.getByLabel("Username").fill("operator");
    await page.getByLabel("Password").fill(loginPassword);
    await page.getByRole("button", { name: "Continue" }).click();

    await expect
      .poll(
        () =>
          requests.filter(
            (request) => request.path === "/admin/api/v1/auth/webauthn",
          ).length,
      )
      .toBe(1);
    await expect(
      page.getByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    await expect(page.getByText("Admin Portal")).toBeVisible();
    for (const workspace of [
      "Host Overview",
      "Services",
      "Counters",
      "Time Series",
      "Health & Changes",
    ]) {
      await expect(page.getByRole("button", { name: workspace })).toBeVisible();
    }

    const passwordRequest = requests.find(
      (request) => request.path === "/admin/api/v1/auth/password",
    );
    expect(passwordRequest).toEqual({
      body: {
        password: loginPassword,
        username: "operator",
      },
      method: "POST",
      path: "/admin/api/v1/auth/password",
    });
    const assertionRequest = requests.find(
      (request) => request.path === "/admin/api/v1/auth/webauthn",
    );
    expect(assertionRequest?.method).toBe("POST");
    expect(assertionRequest?.body).toMatchObject({
      assertion: {
        id: base64url(credentialId),
        response: {
          authenticatorData: expect.any(String),
          clientDataJSON: expect.any(String),
          signature: expect.any(String),
        },
        type: "public-key",
      },
      handle: "browser-login-handle",
    });
    expect(
      await page.evaluate(() => ({
        local: Object.keys(localStorage),
        session: Object.keys(sessionStorage),
      })),
    ).toEqual({ local: [], session: [] });

    await page.getByRole("button", { name: "Sign out" }).click();
    await expect(
      page.getByRole("heading", { name: "Admin sign in" }),
    ).toBeVisible();
    expect(
      requests.filter(
        (request) => request.path === "/admin/api/v1/auth/logout",
      ),
    ).toEqual([
      {
        body: null,
        method: "POST",
        path: "/admin/api/v1/auth/logout",
      },
    ]);
  } finally {
    await removeVirtualAuthenticator(virtual);
  }
});

test("enrollment creates a browser credential and continues to the Admin Portal", async ({
  page,
}) => {
  const virtual = await addVirtualAuthenticator(page);
  const invitationToken = base64url(randomBytes(24));
  const enrollmentPassword = ["browser", "enrollment", "password"].join("-");
  const requests: Array<{ body: unknown; method: string; path: string }> = [];
  const enrollmentAssetReferrers: string[] = [];

  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/admin/assets/")) {
      enrollmentAssetReferrers.push(request.headers().referer ?? "");
    }
  });

  await page.route("**/admin/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    requests.push({
      body: request.postDataJSON() as unknown,
      method: request.method(),
      path,
    });

    if (path === "/admin/api/v1/enroll/begin") {
      await fulfillJson(route, {
        handle: "browser-enrollment-handle",
        publicKey: {
          publicKey: {
            attestation: "none",
            authenticatorSelection: {
              residentKey: "preferred",
              userVerification: "required",
            },
            challenge: base64url(randomBytes(32)),
            pubKeyCredParams: [{ alg: -7, type: "public-key" }],
            rp: { id: "localhost", name: "Concord Voice" },
            user: {
              displayName: "Operator",
              id: base64url(randomBytes(32)),
              name: "operator",
            },
          },
        },
      });
      return;
    }
    if (path === "/admin/api/v1/enroll/finish") {
      await fulfillJson(route, { status: "enrolled" });
      return;
    }
    if (path === "/admin/api/v1/health") {
      await fulfillJson(route, { error: "unauthorized" }, 401);
      return;
    }
    await route.abort();
  });

  try {
    await page.goto(
      `/admin/enroll?username=operator&token=${encodeURIComponent(invitationToken)}`,
    );
    await expect(page).toHaveURL(/\/admin\/enroll$/);
    await expect(
      page.getByRole("heading", { name: "Enroll administrator" }),
    ).toBeVisible();
    await expect(page.locator("body")).not.toContainText(invitationToken);
    expect(enrollmentAssetReferrers).not.toHaveLength(0);
    for (const referrer of enrollmentAssetReferrers) {
      expect(referrer).toBe("");
    }

    await page.getByLabel("Password").fill(enrollmentPassword);
    await page.getByLabel("Credential name").fill("Primary security key");
    await page.getByRole("button", { name: "Enroll security key" }).click();

    await expect(page.getByText("Security key enrolled")).toBeVisible();
    expect(
      requests.find((request) => request.path === "/admin/api/v1/enroll/begin"),
    ).toEqual({
      body: {
        password: enrollmentPassword,
        token: invitationToken,
        username: "operator",
      },
      method: "POST",
      path: "/admin/api/v1/enroll/begin",
    });
    expect(
      requests.find(
        (request) => request.path === "/admin/api/v1/enroll/finish",
      ),
    ).toMatchObject({
      body: {
        attestation: {
          id: expect.any(String),
          response: {
            attestationObject: expect.any(String),
            clientDataJSON: expect.any(String),
          },
          type: "public-key",
        },
        credential_name: "Primary security key",
        handle: "browser-enrollment-handle",
      },
      method: "POST",
    });
    expect(
      await page.evaluate(() => ({
        local: Object.keys(localStorage),
        session: Object.keys(sessionStorage),
      })),
    ).toEqual({ local: [], session: [] });

    await page
      .getByRole("button", { name: "Continue to Admin Portal" })
      .click();
    await expect(page).toHaveURL(/\/admin\/$/);
    await expect(
      page.getByRole("heading", { name: "Admin sign in" }),
    ).toBeVisible();
  } finally {
    await removeVirtualAuthenticator(virtual);
  }
});

test("a 403 health probe crosses a full-document reload boundary", async ({
  page,
}) => {
  let healthRequests = 0;
  await page.route("**/admin/api/v1/health", async (route) => {
    healthRequests += 1;
    await fulfillJson(
      route,
      { error: healthRequests === 1 ? "forbidden" : "unauthorized" },
      healthRequests === 1 ? 403 : 401,
    );
  });

  await page.goto("/admin/");

  await expect.poll(() => healthRequests).toBe(2);
  await expect(page).toHaveURL(/\/admin\/$/);
  await expect(
    page.getByRole("heading", { name: "Admin sign in" }),
  ).toBeVisible();
});

test("Command Rail stays keyboard-operable, contained, and visibly rendered", async ({
  page,
}, testInfo) => {
  await mockAuthenticatedPortal(page);
  await page.goto("/admin/");
  await expect(
    page.getByRole("heading", { name: "Host Overview" }),
  ).toBeVisible();

  const host = page.getByRole("button", { name: "Host Overview" });
  await page.keyboard.press("Tab");
  await expect(host).toBeFocused();
  const focusOutline = await host.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      style: style.outlineStyle,
      width: Number.parseFloat(style.outlineWidth),
    };
  });
  expect(focusOutline.style).toBe("solid");
  expect(focusOutline.width).toBeGreaterThanOrEqual(2);

  await page.keyboard.press("Tab");
  const services = page.getByRole("button", { name: "Services" });
  await expect(services).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(services).toHaveAttribute("aria-current", "page");

  const settings = page.getByRole("button", { name: "Settings" });
  await settings.focus();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("dialog", { name: "Portal settings" }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(settings).toBeFocused();

  const refresh = page.getByRole("button", {
    name: "Refresh visible workspace",
  });
  await expect(refresh).toBeEnabled();
  await refresh.focus();
  await expect(refresh).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(refresh).toBeEnabled();
  const signOut = page.getByRole("button", { name: "Sign out" });
  await signOut.focus();
  await expect(signOut).toBeFocused();

  await host.click();
  const chart = page.getByRole("img", { name: /host cpu over 24 hours/i });
  await expect(chart).toBeVisible();
  const chartBounds = await chart.boundingBox();
  expect(chartBounds).not.toBeNull();
  expect(chartBounds?.height ?? 0).toBeGreaterThan(0);
  expect(chartBounds?.width ?? 0).toBeGreaterThan(0);
  const lineBounds = await chart.locator("polyline").evaluate((line) => {
    const bounds = (line as SVGGraphicsElement).getBBox();
    return { height: bounds.height, width: bounds.width };
  });
  expect(lineBounds.height).toBeGreaterThan(0);
  expect(lineBounds.width).toBeGreaterThan(0);
  const pixelCheck = await chart.evaluate(async (element) => {
    const clone = element.cloneNode(true) as SVGSVGElement;
    clone.setAttribute("height", "240");
    clone.setAttribute("width", "800");
    clone.style.color = "#fa709a";
    const image = new Image();
    image.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(
      new XMLSerializer().serializeToString(clone),
    )}`;
    await image.decode();
    const canvas = document.createElement("canvas");
    canvas.width = 800;
    canvas.height = 240;
    const context = canvas.getContext("2d");
    if (!context) return { colors: 0, opaque: 0 };
    context.drawImage(image, 0, 0, 800, 240);
    const data = context.getImageData(0, 0, 800, 240).data;
    const colors = new Set<string>();
    let opaque = 0;
    for (let offset = 0; offset < data.length; offset += 4) {
      if (data[offset + 3] === 0) continue;
      opaque += 1;
      colors.add(
        `${data[offset]},${data[offset + 1]},${data[offset + 2]},${data[offset + 3]}`,
      );
    }
    return { colors: colors.size, opaque };
  });
  expect(pixelCheck.opaque).toBeGreaterThan(100);
  expect(pixelCheck.colors).toBeGreaterThan(1);

  const disclosure = page.getByText("View accessible data table");
  await disclosure.focus();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("table", { name: "Host CPU series data" }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () =>
        document.documentElement.scrollWidth <=
        document.documentElement.clientWidth,
    ),
  ).toBe(true);
  const dataTable = page.locator(".data-table");
  expect(
    await dataTable.evaluate((element) => ({
      clientWidth: element.clientWidth,
      overflowX: getComputedStyle(element).overflowX,
      scrollWidth: element.scrollWidth,
    })),
  ).toMatchObject({ overflowX: "auto" });

  const overlaps = await page
    .locator(".workspace-actions")
    .evaluate((actions) => {
      const elements = [...actions.children].filter(
        (child): child is HTMLElement => child instanceof HTMLElement,
      );
      const collisions: string[] = [];
      for (let leftIndex = 0; leftIndex < elements.length; leftIndex += 1) {
        const left = elements[leftIndex].getBoundingClientRect();
        for (
          let rightIndex = leftIndex + 1;
          rightIndex < elements.length;
          rightIndex += 1
        ) {
          const right = elements[rightIndex].getBoundingClientRect();
          const width =
            Math.min(left.right, right.right) - Math.max(left.left, right.left);
          const height =
            Math.min(left.bottom, right.bottom) - Math.max(left.top, right.top);
          if (width > 1 && height > 1) {
            collisions.push(`${leftIndex}:${rightIndex}`);
          }
        }
      }
      return collisions;
    });
  expect(overlaps).toEqual([]);

  await disclosure.click();
  for (const workspace of [
    ["Host Overview", "host"],
    ["Services", "services"],
    ["Counters", "counters"],
    ["Time Series", "time-series"],
    ["Health & Changes", "health-changes"],
  ] as const) {
    await page.getByRole("button", { name: workspace[0] }).click();
    await expect(
      page.getByRole("heading", { name: workspace[0] }),
    ).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 0));
    await expect(page).toHaveScreenshot(`command-rail-${workspace[1]}.png`, {
      fullPage: true,
    });
  }

  expect(testInfo.project.name).toMatch(/^chromium-(desktop|mobile-320)$/);
});
