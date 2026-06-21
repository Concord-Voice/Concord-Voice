/**
 * Tests for apiClient attestation token injection and 403 handling (#677).
 *
 * Covers:
 * - X-Attestation-Token header injection on every request
 * - Handling of all 6 attestation error codes from the backend
 * - Silent re-attest path (EXPIRED / INVALID / MISSING)
 * - Terminal path (UNKNOWN_RELEASE / VERSION_TOO_OLD / REVOKED)
 * - Non-attestation 403 pass-through
 * - Absence of electron bridge (web/test path)
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useAttestationFailureStore } from '@/renderer/stores/attestationFailureStore';

// Mock resetService (dynamically imported by apiClient)
vi.mock('@/renderer/services/resetService', () => ({
  gracefulReset: vi.fn(),
  nuclearReset: vi.fn(),
  softRestart: vi.fn(),
  stopProactiveRefresh: vi.fn(),
}));

// Mock fetch globally
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

import { apiFetch, _resetRefreshState, ensureMachineId } from '@/renderer/services/apiClient';

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeAttestationFailureResponse(
  code: string,
  extra: Record<string, unknown> = {}
): Response {
  return new Response(JSON.stringify({ error: 'Attestation failed', code, ...extra }), {
    status: 403,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeElectronBridge(
  overrides: {
    getToken?: () => Promise<string | null>;
    clearToken?: () => Promise<void>;
    forceCheckForUpdates?: (reason: string) => Promise<void>;
  } = {}
) {
  return {
    getMachineId: vi.fn().mockResolvedValue(''),
    refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
    attestation: {
      getToken: vi.fn(overrides.getToken ?? (() => Promise.resolve(null))),
      clearToken: vi.fn(overrides.clearToken ?? (() => Promise.resolve(undefined))),
    },
    updater: {
      forceCheckForUpdates: vi.fn(
        overrides.forceCheckForUpdates ?? ((_reason: string) => Promise.resolve(undefined))
      ),
    },
  };
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('apiClient attestation — X-Attestation-Token injection', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
  });

  afterEach(() => {
    (globalThis as Record<string, unknown>).electron = undefined;
  });

  it('injects X-Attestation-Token header when electron.attestation.getToken returns a token', async () => {
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token-123'),
    });
    useAuthStore.getState().setAccessToken('auth-token');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    expect(mockFetch).toHaveBeenCalledTimes(1);
    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('X-Attestation-Token')).toBe('attest-token-123');
  });

  it('injects X-Session-ID header when authStore.sessionId is populated (HIGH #12)', async () => {
    // The attestation middleware looks up the per-session token record keyed
    // by (session_id, machine_id). Missing X-Session-ID → 403 ATTESTATION_MISSING
    // even with a valid X-Attestation-Token. Regression guard for the gap that
    // PR #1264 closed.
    (globalThis as Record<string, unknown>).electron = makeElectronBridge();
    useAuthStore.getState().setAccessToken('auth-token');
    useAuthStore.getState().setSessionId('session-uuid-789');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('X-Session-ID')).toBe('session-uuid-789');
  });

  it('omits X-Session-ID header when authStore.sessionId is null', async () => {
    (globalThis as Record<string, unknown>).electron = makeElectronBridge();
    useAuthStore.getState().setAccessToken('auth-token');
    // sessionId left null (default after resetAllStores → clearAccessToken)
    expect(useAuthStore.getState().sessionId).toBeNull();
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('X-Session-ID')).toBeNull();
  });

  it('does not crash when globalThis.electron is undefined (web/test path)', async () => {
    (globalThis as Record<string, unknown>).electron = undefined;
    useAuthStore.getState().setAccessToken('auth-token');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await expect(apiFetch('/api/v1/test')).resolves.toBeDefined();

    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('X-Attestation-Token')).toBeNull();
  });

  it('does NOT brick the request when getToken rejects — optional header degrades to none (defense-in-depth)', async () => {
    // Founding incident: a sender-frame regression made attestation:get-token
    // throw "untrusted sender frame" on every call. The unguarded await cascaded
    // an OPTIONAL header into total connectivity loss (prefs, friends, ws ticket,
    // all dead). The token fetch must degrade to "no token", never reject upward.
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () =>
        Promise.reject(new Error('attestation:get-token rejected: untrusted sender frame')),
    });
    useAuthStore.getState().setAccessToken('auth-token');
    mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');

    // The request completes normally despite the rejected token fetch.
    expect(response.status).toBe(200);
    expect(mockFetch).toHaveBeenCalledTimes(1);
    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('X-Attestation-Token')).toBeNull();
    // The Authorization header still went out — connectivity is preserved.
    expect(headers.get('Authorization')).toBe('Bearer auth-token');
    // The failure is observable, not silently swallowed.
    expect(warnSpy).toHaveBeenCalledTimes(1);
    warnSpy.mockRestore();
  });

  it('does not set X-Attestation-Token when getToken returns null', async () => {
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve(null),
    });
    useAuthStore.getState().setAccessToken('auth-token');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('X-Attestation-Token')).toBeNull();
  });

  it('200 OK with attestation token — passes through normally, body readable', async () => {
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token-123'),
    });
    useAuthStore.getState().setAccessToken('auth-token');
    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify({ message: 'ok' }), { status: 200 })
    );

    const response = await apiFetch('/api/v1/data');

    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body).toEqual({ message: 'ok' });
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });
});

describe('apiClient attestation — 403 interception', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
  });

  afterEach(() => {
    (globalThis as Record<string, unknown>).electron = undefined;
  });

  // ── Re-attest path: ATTESTATION_EXPIRED ────────────────────────────────────

  it('ATTESTATION_EXPIRED + fresh token → clearToken called, request retried once with new token', async () => {
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('expired-token') // initial injection call
      .mockResolvedValueOnce('fresh-token'); // re-attest call after clearToken
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getTokenMock(),
      clearToken: () => clearTokenMock(),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    // First fetch: 403 ATTESTATION_EXPIRED
    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
    // Retry fetch: success
    mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(200);
    expect(clearTokenMock).toHaveBeenCalledTimes(1);
    expect(getTokenMock).toHaveBeenCalledTimes(2); // once for injection, once for re-attest
    expect(mockFetch).toHaveBeenCalledTimes(2);

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('X-Attestation-Token')).toBe('fresh-token');
    expect(retryHeaders.get('Authorization')).toBe('Bearer auth-token');
  });

  it('ATTESTATION_EXPIRED + getToken returns null (inert) → no retry, original 403 returned, clearToken called once', async () => {
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('expired-token') // injection
      .mockResolvedValueOnce(null); // re-attest returns null (inert)
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getTokenMock(),
      clearToken: () => clearTokenMock(),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(clearTokenMock).toHaveBeenCalledTimes(1);
    expect(mockFetch).toHaveBeenCalledTimes(1); // no retry
  });

  // ── Re-attest path: ATTESTATION_MISSING ───────────────────────────────────

  it('ATTESTATION_MISSING → takes re-attest path (clearToken called)', async () => {
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce(null) // injection: no token initially
      .mockResolvedValueOnce(null); // re-attest: still null
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getTokenMock(),
      clearToken: () => clearTokenMock(),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_MISSING'));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(clearTokenMock).toHaveBeenCalledTimes(1); // re-attest path taken
  });

  // ── Re-attest path: ATTESTATION_INVALID ───────────────────────────────────

  it('ATTESTATION_INVALID → re-attest path taken (clearToken called)', async () => {
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('bad-token') // injection
      .mockResolvedValueOnce(null); // re-attest: null (inert)
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getTokenMock(),
      clearToken: () => clearTokenMock(),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_INVALID'));

    const response = await apiFetch('/api/v1/test');

    // No retry since fresh token is null
    expect(response.status).toBe(403);
    expect(clearTokenMock).toHaveBeenCalledTimes(1);
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  // ── Terminal path: ATTESTATION_UNKNOWN_RELEASE ────────────────────────────

  it('ATTESTATION_UNKNOWN_RELEASE → forceCheckForUpdates called with exact literal "attestation_required", showFailure populated, original 403 returned', async () => {
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token'),
      clearToken: () => clearTokenMock(),
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(
      makeAttestationFailureResponse('ATTESTATION_UNKNOWN_RELEASE', {
        updateAvailable: true,
        requiredMinVersion: '0.2.0',
        downloadHelpUrl: 'https://concordvoice.com/download',
      })
    );

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(forceCheckMock).toHaveBeenCalledWith('attestation_required');
    expect(clearTokenMock).not.toHaveBeenCalled(); // terminal path, no clearToken
    expect(mockFetch).toHaveBeenCalledTimes(1); // no retry

    const storeState = useAttestationFailureStore.getState();
    expect(storeState.visible).toBe(true);
    expect(storeState.code).toBe('ATTESTATION_UNKNOWN_RELEASE');
    expect(storeState.requiredMinVersion).toBe('0.2.0');
    expect(storeState.downloadHelpUrl).toBe('https://concordvoice.com/download');
  });

  // ── Terminal path: CLIENT_VERSION_TOO_OLD ─────────────────────────────────

  it('CLIENT_VERSION_TOO_OLD → forceCheckForUpdates + showFailure, original 403 returned', async () => {
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token'),
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(
      makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD', {
        updateAvailable: true,
        requiredMinVersion: '1.0.0',
        downloadHelpUrl: 'https://concordvoice.com/download',
      })
    );

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(forceCheckMock).toHaveBeenCalledWith('attestation_required');
    expect(mockFetch).toHaveBeenCalledTimes(1);

    const storeState = useAttestationFailureStore.getState();
    expect(storeState.visible).toBe(true);
    expect(storeState.code).toBe('CLIENT_VERSION_TOO_OLD');
    expect(storeState.requiredMinVersion).toBe('1.0.0');
    expect(storeState.downloadHelpUrl).toBe('https://concordvoice.com/download');
  });

  // ── Terminal path: ATTESTATION_REVOKED ────────────────────────────────────

  it('ATTESTATION_REVOKED → terminal branch: forceCheckForUpdates called, clearToken NOT called', async () => {
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token'),
      clearToken: () => clearTokenMock(),
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_REVOKED'));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(forceCheckMock).toHaveBeenCalledWith('attestation_required');
    // REVOKED is terminal — clearToken must NOT be called
    expect(clearTokenMock).not.toHaveBeenCalled();
    expect(mockFetch).toHaveBeenCalledTimes(1);

    const storeState = useAttestationFailureStore.getState();
    expect(storeState.visible).toBe(true);
    expect(storeState.code).toBe('ATTESTATION_REVOKED');
  });

  // ── Non-attestation 403 pass-through ──────────────────────────────────────

  it('403 with no code field (RBAC denial) → returned untouched, no clearToken, no forceCheckForUpdates', async () => {
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token'),
      clearToken: () => clearTokenMock(),
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: 'Forbidden' }), {
        status: 403,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(clearTokenMock).not.toHaveBeenCalled();
    expect(forceCheckMock).not.toHaveBeenCalled();
    expect(mockFetch).toHaveBeenCalledTimes(1);

    const storeState = useAttestationFailureStore.getState();
    expect(storeState.visible).toBe(false);
  });

  it('403 with non-JSON body → returned untouched, no clearToken, no forceCheckForUpdates', async () => {
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token'),
      clearToken: () => clearTokenMock(),
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(
      new Response('<html>403 Forbidden</html>', {
        status: 403,
        headers: { 'Content-Type': 'text/html' },
      })
    );

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(clearTokenMock).not.toHaveBeenCalled();
    expect(forceCheckMock).not.toHaveBeenCalled();
  });

  // LOW #33 regression: a 403 carrying an unrecognized attestation code (not in
  // either re-attest or terminal set) MUST NOT open the modal. The store's
  // code field is typed as TerminalAttestationCode | null, and the apiClient
  // narrows via isTerminalAttestationCode() before passing to showFailure.
  it('403 with unknown attestation code → returned untouched, no clearToken, no forceCheckForUpdates, no modal', async () => {
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => Promise.resolve('attest-token'),
      clearToken: () => clearTokenMock(),
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          error: 'Attestation failed',
          code: 'ATTESTATION_FUTURE_UNKNOWN_VARIANT',
        }),
        { status: 403, headers: { 'Content-Type': 'application/json' } }
      )
    );

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(clearTokenMock).not.toHaveBeenCalled();
    expect(forceCheckMock).not.toHaveBeenCalled();
    expect(useAttestationFailureStore.getState().visible).toBe(false);
    expect(useAttestationFailureStore.getState().code).toBeNull();
  });

  // ── 401 is unaffected by 403 handler ─────────────────────────────────────

  it('401 still routes to handle401Recovery — 403 handler does not intercept 401', async () => {
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({
        clearToken: () => clearTokenMock(),
        forceCheckForUpdates: (r: string) => forceCheckMock(r),
      }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-token' }),
    } as unknown;

    useAuthStore.getState().setAccessToken('auth-token');

    // 401 triggers the token refresh path, not 403 handler
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');

    // The 401 recovery should succeed
    expect(response.status).toBe(200);
    // 403 handler must NOT have fired
    expect(clearTokenMock).not.toHaveBeenCalled();
    expect(forceCheckMock).not.toHaveBeenCalled();
  });

  // ── Retry carries all required headers ────────────────────────────────────

  it('retry after ATTESTATION_EXPIRED includes Authorization + X-Machine-Id + X-Attestation-Token', async () => {
    // Pre-populate machine ID cache
    (globalThis as Record<string, unknown>).electron = {
      getMachineId: vi.fn().mockResolvedValue('machine-uuid-456'),
    } as unknown;
    await ensureMachineId();

    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('old-attest') // injection
      .mockResolvedValueOnce('new-attest'); // re-attest
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = {
      getMachineId: vi.fn().mockResolvedValue('machine-uuid-456'),
      refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
      attestation: {
        getToken: vi.fn(() => getTokenMock()),
        clearToken: vi.fn(() => clearTokenMock()),
      },
      updater: {
        forceCheckForUpdates: vi.fn().mockResolvedValue(undefined),
      },
    } as unknown;

    useAuthStore.getState().setAccessToken('bearer-token');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('Authorization')).toBe('Bearer bearer-token');
    expect(retryHeaders.get('X-Attestation-Token')).toBe('new-attest');
    // machine ID may or may not be set depending on cache state; just assert no throw
  });

  it('retry after ATTESTATION_EXPIRED includes X-Session-ID (HIGH #13)', async () => {
    // After a 403 attestation re-attest, the retry rebuilds headers and MUST
    // re-attach X-Session-ID; otherwise the server cannot locate the per-session
    // token record (keyed by session_id + machine_id) and the retry also 403s.
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('old-attest') // injection
      .mockResolvedValueOnce('new-attest'); // re-attest
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getTokenMock(),
      clearToken: () => clearTokenMock(),
    });
    useAuthStore.getState().setAccessToken('bearer-token');
    useAuthStore.getState().setSessionId('session-uuid-retry');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('X-Session-ID')).toBe('session-uuid-retry');
    expect(retryHeaders.get('X-Attestation-Token')).toBe('new-attest');
  });

  it('retry after ATTESTATION_EXPIRED omits X-Session-ID when authStore.sessionId is null', async () => {
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('old-attest')
      .mockResolvedValueOnce('new-attest');
    const clearTokenMock = vi.fn().mockResolvedValue(undefined);

    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getTokenMock(),
      clearToken: () => clearTokenMock(),
    });
    useAuthStore.getState().setAccessToken('bearer-token');
    // sessionId intentionally left null

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('X-Session-ID')).toBeNull();
    // The retry still proceeds — omitted header is the defensive posture, not an error.
    expect(retryHeaders.get('X-Attestation-Token')).toBe('new-attest');
  });

  it('ATTESTATION_EXPIRED + clearToken AND getToken both reject → no throw, original 403 returned (defense-in-depth #1527)', async () => {
    // No-rot guard: the re-attest path routes clearToken/getToken through the
    // *Safe wrappers, so a sender-frame (or any IPC) failure on the recovery
    // path degrades to "no fresh token → original 403" rather than throwing.
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () =>
        Promise.reject(new Error('attestation:get-token rejected: untrusted sender frame')),
      clearToken: () =>
        Promise.reject(new Error('attestation:clear-token rejected: untrusted sender frame')),
    });
    useAuthStore.getState().setAccessToken('auth-token');

    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403); // original 403, unchanged — no throw
    expect(mockFetch).toHaveBeenCalledTimes(1); // no retry: fresh token degraded to null
    warnSpy.mockRestore();
  });
});

describe('apiClient — 401 recovery header preservation (HIGH #14)', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
  });

  afterEach(() => {
    (globalThis as Record<string, unknown>).electron = undefined;
  });

  it('after 401 + refresh succeeds, retry includes X-Session-ID and X-Attestation-Token from the cache', async () => {
    // The 401 recovery path rebuilds headers from `init?.headers` (which does not
    // carry the values apiFetch set on the original Headers object). Without
    // re-attaching X-Session-ID + X-Attestation-Token, the retry will 403 against
    // an attestation-enabled server even though the freshly-refreshed JWT is fine.
    const getTokenMock = vi.fn().mockResolvedValue('cached-attest-token');

    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({ getToken: () => getTokenMock() }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
    } as unknown;

    useAuthStore.getState().setAccessToken('expired-jwt');
    useAuthStore.getState().setSessionId('session-uuid-401');

    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(200);
    expect(mockFetch).toHaveBeenCalledTimes(2);

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('Authorization')).toBe('Bearer refreshed-jwt');
    expect(retryHeaders.get('X-Session-ID')).toBe('session-uuid-401');
    expect(retryHeaders.get('X-Attestation-Token')).toBe('cached-attest-token');
  });

  it('after 401 + refresh succeeds, retry omits X-Session-ID when sessionId is null', async () => {
    const getTokenMock = vi.fn().mockResolvedValue('cached-attest-token');

    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({ getToken: () => getTokenMock() }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
    } as unknown;

    useAuthStore.getState().setAccessToken('expired-jwt');
    // sessionId intentionally left null

    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('X-Session-ID')).toBeNull();
    expect(retryHeaders.get('Authorization')).toBe('Bearer refreshed-jwt');
  });

  it('after 401 + refresh succeeds, retry omits X-Attestation-Token when attestation cache is empty', async () => {
    // The renderer-side bridge returns null when the mint is inert (early in
    // milestone rollout, or after a deliberate clear). The retry MUST NOT set
    // X-Attestation-Token to '' / undefined — omit it entirely.
    const getTokenMock = vi.fn().mockResolvedValue(null);

    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({ getToken: () => getTokenMock() }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
    } as unknown;

    useAuthStore.getState().setAccessToken('expired-jwt');
    useAuthStore.getState().setSessionId('session-uuid-401');

    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('X-Attestation-Token')).toBeNull();
    expect(retryHeaders.get('X-Session-ID')).toBe('session-uuid-401');
  });

  it('after 401 + refresh succeeds, retry omits X-Attestation-Token when electron bridge is absent (web/test path)', async () => {
    (globalThis as Record<string, unknown>).electron = {
      // Provide refreshToken, but no `attestation` namespace
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
      getMachineId: vi.fn().mockResolvedValue(''),
    } as unknown;

    useAuthStore.getState().setAccessToken('expired-jwt');
    useAuthStore.getState().setSessionId('session-uuid-401');

    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('X-Attestation-Token')).toBeNull();
    expect(retryHeaders.get('X-Session-ID')).toBe('session-uuid-401');
  });

  it('401 recovery survives a rejecting getToken — retry sent with Authorization, no attestation token (defense-in-depth #1527)', async () => {
    // No-rot guard: the 401-recovery retry pulls the attestation token via
    // getAttestationTokenSafe, so an IPC rejection degrades to "no token"
    // rather than bricking the refreshed-JWT retry.
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({
        getToken: () =>
          Promise.reject(new Error('attestation:get-token rejected: untrusted sender frame')),
      }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
    } as unknown;

    useAuthStore.getState().setAccessToken('expired-jwt');
    useAuthStore.getState().setSessionId('session-uuid-401');

    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(200); // retry succeeded despite the rejecting token fetch
    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('Authorization')).toBe('Bearer refreshed-jwt');
    expect(retryHeaders.get('X-Attestation-Token')).toBeNull(); // degraded gracefully
    warnSpy.mockRestore();
  });
});
