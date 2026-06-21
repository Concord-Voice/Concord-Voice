import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock electron's `net` module before importing the module under test.
vi.mock('electron', () => {
  const fetch = vi.fn();
  return {
    net: { fetch },
  };
});

// Mock attestationSignals so we control what collectAttestationSignals returns.
vi.mock('@/main/attestationSignals', () => {
  const collectAttestationSignals = vi.fn();
  return { collectAttestationSignals };
});

import { net } from 'electron';
import { collectAttestationSignals } from '@/main/attestationSignals';
import {
  attest,
  getAttestationToken,
  clearAttestationToken,
  AttestationError,
} from '@/main/attestationService';

const mockNet = net as unknown as { fetch: ReturnType<typeof vi.fn> };
const mockCollect = collectAttestationSignals as unknown as ReturnType<typeof vi.fn>;

const BASE_URL = 'https://api.example.com';
const JWT = 'test-jwt-123';
const SESSION_ID = 'test-session-456';

const FAKE_SIGNALS = {
  version: '0.1.48',
  platform: 'macos' as const,
  machine_id: 'machine-abc-123',
  spa_version: 'abc1234',
  spa_hash: 'sha256:deadbeef',
};

const OPTS = {
  apiBaseUrl: BASE_URL,
  jwt: JWT,
  sessionId: SESSION_ID,
  platform: 'macos' as const,
  version: '0.1.48',
};

const FUTURE_EXPIRES_AT = new Date(Date.now() + 2 * 60 * 60 * 1000).toISOString();

function mockSuccessResponse(token = 'attest-token-xyz', expiresAt = FUTURE_EXPIRES_AT) {
  mockNet.fetch.mockResolvedValue({
    ok: true,
    status: 200,
    json: vi.fn().mockResolvedValue({ attestation_token: token, expires_at: expiresAt }),
  });
}

function mockErrorResponse(status: number, body: Record<string, unknown>) {
  mockNet.fetch.mockResolvedValue({
    ok: false,
    status,
    json: vi.fn().mockResolvedValue(body),
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  clearAttestationToken();
  mockCollect.mockResolvedValue(FAKE_SIGNALS);
});

afterEach(() => {
  vi.useRealTimers();
});

// ─── attest — success ────────────────────────────────────────────────────────

describe('attest — success', () => {
  it('returns the attestation_token from the server response', async () => {
    mockSuccessResponse('tok-abc');
    const result = await attest(OPTS);
    expect(result).toBe('tok-abc');
  });

  it('caches the token so getAttestationToken() returns it', async () => {
    mockSuccessResponse('tok-abc');
    await attest(OPTS);
    expect(getAttestationToken()).toBe('tok-abc');
  });

  it('calls net.fetch with the verify URL', async () => {
    mockSuccessResponse();
    await attest(OPTS);
    expect(mockNet.fetch).toHaveBeenCalledWith(
      `${BASE_URL}/api/v1/attestation/verify`,
      expect.any(Object)
    );
  });

  it('sends Authorization header with the JWT', async () => {
    mockSuccessResponse();
    await attest(OPTS);
    const [, init] = mockNet.fetch.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>)['Authorization']).toBe(`Bearer ${JWT}`);
  });

  it('sends X-Session-ID header', async () => {
    mockSuccessResponse();
    await attest(OPTS);
    const [, init] = mockNet.fetch.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>)['X-Session-ID']).toBe(SESSION_ID);
  });

  it('sends X-Machine-Id header from signals', async () => {
    mockSuccessResponse();
    await attest(OPTS);
    const [, init] = mockNet.fetch.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>)['X-Machine-Id']).toBe(FAKE_SIGNALS.machine_id);
  });

  it('sends empty X-Machine-Id when machine_id is absent (web platform)', async () => {
    const webSignals = { ...FAKE_SIGNALS, machine_id: undefined };
    mockCollect.mockResolvedValue(webSignals);
    mockSuccessResponse();
    await attest({ ...OPTS, platform: 'web' });
    const [, init] = mockNet.fetch.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>)['X-Machine-Id']).toBe('');
  });
});

// ─── attest — failure: server returns error code ─────────────────────────────

describe('attest — failure with parseable error body', () => {
  it('throws AttestationError with the server status', async () => {
    mockErrorResponse(403, { code: 'ATTESTATION_UNKNOWN_RELEASE' });
    await expect(attest(OPTS)).rejects.toBeInstanceOf(AttestationError);
  });

  it('throws AttestationError with .status === 403', async () => {
    mockErrorResponse(403, { code: 'ATTESTATION_UNKNOWN_RELEASE' });
    const err = await attest(OPTS).catch((e: unknown) => e);
    expect((err as AttestationError).status).toBe(403);
  });

  it('throws AttestationError with .code from the server body', async () => {
    mockErrorResponse(403, { code: 'ATTESTATION_UNKNOWN_RELEASE' });
    const err = await attest(OPTS).catch((e: unknown) => e);
    expect((err as AttestationError).code).toBe('ATTESTATION_UNKNOWN_RELEASE');
  });

  it('throws AttestationError with .body carrying the parsed body', async () => {
    mockErrorResponse(403, { code: 'ATTESTATION_UNKNOWN_RELEASE', extra: 'detail' });
    const err = await attest(OPTS).catch((e: unknown) => e);
    expect((err as AttestationError).body).toMatchObject({
      code: 'ATTESTATION_UNKNOWN_RELEASE',
      extra: 'detail',
    });
  });
});

// ─── attest — failure: error body has no code field ─────────────────────────

describe('attest — failure with absent code in error body', () => {
  it('falls back to ATTESTATION_ERROR sentinel code', async () => {
    mockErrorResponse(500, { message: 'internal error' });
    const err = await attest(OPTS).catch((e: unknown) => e);
    expect((err as AttestationError).code).toBe('ATTESTATION_ERROR');
  });
});

// ─── attest — failure: error body is unparseable ─────────────────────────────

describe('attest — failure with unparseable error body', () => {
  it('throws AttestationError without crashing when res.json() rejects', async () => {
    mockNet.fetch.mockResolvedValue({
      ok: false,
      status: 503,
      json: vi.fn().mockRejectedValue(new Error('not JSON')),
    });
    const err = await attest(OPTS).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(AttestationError);
    expect((err as AttestationError).status).toBe(503);
    expect((err as AttestationError).code).toBe('ATTESTATION_ERROR');
  });
});

// ─── getAttestationToken ─────────────────────────────────────────────────────

describe('getAttestationToken', () => {
  it('returns null when nothing is cached', () => {
    expect(getAttestationToken()).toBeNull();
  });

  it('returns null after the cached token expires', async () => {
    vi.useFakeTimers();
    const now = Date.now();
    const expiresAt = new Date(now + 100).toISOString();
    mockSuccessResponse('tok-expiring', expiresAt);
    await attest(OPTS);
    expect(getAttestationToken()).toBe('tok-expiring');

    vi.setSystemTime(now + 200);
    expect(getAttestationToken()).toBeNull();
  });

  it('treats the exact expiry instant as expired (fail-closed boundary)', async () => {
    vi.useFakeTimers();
    const now = Date.now();
    const expiresAt = new Date(now + 100).toISOString();
    mockSuccessResponse('tok-boundary', expiresAt);
    await attest(OPTS);
    expect(getAttestationToken()).toBe('tok-boundary');

    // At t === expiresAt the token must read as expired (<=, not <). Under a
    // `<` comparison this returns the token and the test fails — locking the
    // fail-closed boundary against a future regression.
    vi.setSystemTime(now + 100);
    expect(getAttestationToken()).toBeNull();
  });

  it('clears the cache when the token is found to have expired', async () => {
    vi.useFakeTimers();
    const now = Date.now();
    const expiresAt = new Date(now + 100).toISOString();
    mockSuccessResponse('tok-expiring', expiresAt);
    await attest(OPTS);

    vi.setSystemTime(now + 200);
    getAttestationToken(); // triggers expiry eviction
    // A second call should also return null (cache is gone, not stale).
    expect(getAttestationToken()).toBeNull();
  });
});

// ─── clearAttestationToken ───────────────────────────────────────────────────

describe('clearAttestationToken', () => {
  it('causes getAttestationToken() to return null after a successful attest', async () => {
    mockSuccessResponse('tok-to-clear');
    await attest(OPTS);
    expect(getAttestationToken()).toBe('tok-to-clear');

    clearAttestationToken();
    expect(getAttestationToken()).toBeNull();
  });
});

// ─── fail-closed: malformed expires_at ───────────────────────────────────────

describe('attest — malformed expires_at (fail-closed)', () => {
  it('throws AttestationError when expires_at is not a valid date string', async () => {
    mockNet.fetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: vi.fn().mockResolvedValue({ attestation_token: 'tok-x', expires_at: 'not-a-date' }),
    });
    await expect(attest(OPTS)).rejects.toBeInstanceOf(AttestationError);
  });

  it('does not cache anything when expires_at is malformed', async () => {
    mockNet.fetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: vi.fn().mockResolvedValue({ attestation_token: 'tok-x', expires_at: 'not-a-date' }),
    });
    await attest(OPTS).catch(() => {});
    expect(getAttestationToken()).toBeNull();
  });
});
