import {
  parseCounters,
  parseCurrent,
  parseHealth,
  parseSeries,
} from "./contracts";
import type {
  AdminCountersResponse,
  AdminCurrentResponse,
  AdminHealthResponse,
  AdminSeriesResponse,
  MetricKey,
  SeriesWindow,
} from "./contracts";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly retryAfter: number | null,
  ) {
    super("Admin Portal request failed");
    this.name = "ApiError";
  }
}

export class ApiContractError extends Error {
  constructor() {
    super("Admin Portal response did not match the request");
    this.name = "ApiContractError";
  }
}

interface CredentialOptionsResponse {
  handle: string;
  publicKey: unknown;
}

export type PasswordLoginResponse = CredentialOptionsResponse;
export type EnrollBeginResponse = CredentialOptionsResponse;

interface RequestOptions {
  signal?: AbortSignal;
}

interface RequestInitOptions extends RequestOptions {
  body?: unknown;
  method?: "GET" | "POST";
}

function retryAfter(value: string | null): number | null {
  if (value === null || !/^\d+$/.test(value)) return null;
  const seconds = Number(value);
  return Number.isSafeInteger(seconds) && seconds <= 3600 ? seconds : null;
}

async function request<T>(
  path: string,
  parser: (value: unknown) => T,
  init: RequestInitOptions = {},
): Promise<T> {
  const headers = new Headers();
  let body: string | undefined;
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.body);
  }

  let response: Response;
  try {
    response = await fetch(path, {
      method: init.method ?? (init.body === undefined ? "GET" : "POST"),
      credentials: "same-origin",
      headers,
      body,
      signal: init.signal,
    });
  } catch {
    throw new ApiError(0, null);
  }

  if (!response.ok) {
    throw new ApiError(
      response.status,
      retryAfter(response.headers.get("Retry-After")),
    );
  }

  if (response.status === 204) return parser(null);

  let value: unknown;
  try {
    value = await response.json();
  } catch {
    throw new ApiError(0, null);
  }
  try {
    return parser(value);
  } catch (cause) {
    if (cause instanceof ApiContractError) throw cause;
    throw new ApiError(0, null);
  }
}

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new ApiError(0, null);
  }
  return value as Record<string, unknown>;
}

function exactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): void {
  const actual = Object.keys(value);
  const expectedSet = new Set(expected);
  if (
    actual.length !== expected.length ||
    actual.some((key) => !expectedSet.has(key))
  ) {
    throw new ApiError(0, null);
  }
}

function nonEmptyString(value: unknown): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new ApiError(0, null);
  }
  return value;
}

function credentialOptionsResponse(value: unknown): CredentialOptionsResponse {
  const data = record(value);
  exactKeys(data, ["handle", "publicKey"]);
  record(data.publicKey);
  return {
    handle: nonEmptyString(data.handle),
    publicKey: data.publicKey,
  };
}

function statusResponse(expected: string): (value: unknown) => void {
  return (value) => {
    const data = record(value);
    exactKeys(data, ["status"]);
    if (data.status !== expected) throw new ApiError(0, null);
  };
}

export const api = {
  getHealth(options?: RequestOptions): Promise<AdminHealthResponse> {
    return request("/admin/api/v1/health", parseHealth, options);
  },
  getCurrent(options?: RequestOptions): Promise<AdminCurrentResponse> {
    return request("/admin/api/v1/metrics/current", parseCurrent, options);
  },
  getCounters(options?: RequestOptions): Promise<AdminCountersResponse> {
    return request("/admin/api/v1/counters", parseCounters, options);
  },
  getSeries(
    metricKey: MetricKey,
    window: SeriesWindow,
    options?: RequestOptions,
  ): Promise<AdminSeriesResponse> {
    const query = new URLSearchParams({ key: metricKey, window });
    return request(
      `/admin/api/v1/metrics/series?${query.toString()}`,
      (value) => {
        const response = parseSeries(value);
        if (
          response.metric.metric_key !== metricKey ||
          response.window !== window
        ) {
          throw new ApiContractError();
        }
        return response;
      },
      options,
    );
  },
  passwordLogin(
    username: string,
    password: string,
    options?: RequestOptions,
  ): Promise<PasswordLoginResponse> {
    return request("/admin/api/v1/auth/password", credentialOptionsResponse, {
      ...options,
      body: { username, password },
    });
  },
  webauthnLogin(
    handle: string,
    assertion: unknown,
    options?: RequestOptions,
  ): Promise<void> {
    return request(
      "/admin/api/v1/auth/webauthn",
      statusResponse("authenticated"),
      {
        ...options,
        body: { handle, assertion },
      },
    );
  },
  enrollBegin(
    username: string,
    password: string,
    token: string,
    options?: RequestOptions,
  ): Promise<EnrollBeginResponse> {
    return request("/admin/api/v1/enroll/begin", credentialOptionsResponse, {
      ...options,
      body: { username, password, token },
    });
  },
  enrollFinish(
    handle: string,
    attestation: unknown,
    credentialName: string,
    options?: RequestOptions,
  ): Promise<void> {
    return request("/admin/api/v1/enroll/finish", statusResponse("enrolled"), {
      ...options,
      body: { handle, attestation, credential_name: credentialName },
    });
  },
  logout(options?: RequestOptions): Promise<void> {
    return request("/admin/api/v1/auth/logout", statusResponse("logged out"), {
      ...options,
      method: "POST",
    });
  },
};
