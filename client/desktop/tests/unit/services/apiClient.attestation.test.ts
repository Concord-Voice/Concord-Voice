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
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useAttestationFailureStore } from '@/renderer/stores/auth/attestationFailureStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';

const mockGracefulReset = vi.fn();
const mockNuclearReset = vi.fn();

// Mock fetch globally
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

import {
  apiFetch,
  handleTerminalClientVersionTooOld,
  _resetRefreshState,
  ensureMachineId,
  configureRefreshFailureReset,
} from '@/renderer/services/system/apiClient';
import { _resetClientVersionCache } from '@/renderer/utils/runtime/clientVersion';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';

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
    getVersion?: () => Promise<string>;
  } = {}
) {
  return {
    getMachineId: vi.fn().mockResolvedValue(''),
    getVersion: vi.fn(overrides.getVersion ?? (() => Promise.resolve('0.1.0-test'))),
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
    _resetClientVersionCache();
    _resetRefreshState();
    resetRuntimeServerBase();
    vi.clearAllMocks();
    configureRefreshFailureReset({
      gracefulReset: mockGracefulReset,
      nuclearReset: mockNuclearReset,
    });
  });

  afterEach(() => {
    resetRuntimeServerBase();
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

  it('records the config revision from before terminal failure publication begins', async () => {
    const preCallRevision = useClientConfigStore.getState().beginConfigRequest();
    const pending = handleTerminalClientVersionTooOld(() => true, '1.0.0');
    useClientConfigStore.getState().beginConfigRequest();

    await pending;

    expect(useAttestationFailureStore.getState().observedConfigRequestRevision).toBe(
      preCallRevision
    );
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

  it.each([
    ['the runtime server changes', () => setRuntimeServerBase('https://successor.example')],
    [
      'the auth generation changes',
      () => useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 }),
    ],
  ] as const)(
    'does not dispatch when %s during asynchronous header lookup',
    async (_name, change) => {
      let resolveVersion: (version: string) => void = () => {
        throw new Error('version resolver was not initialized');
      };
      const getVersion = vi.fn(
        () =>
          new Promise<string>((resolve) => {
            resolveVersion = resolve;
          })
      );
      (globalThis as Record<string, unknown>).electron = makeElectronBridge({
        getVersion: () => getVersion(),
      });
      useAuthStore.getState().setAccessToken('original-token');

      const pending = apiFetch('/api/v1/test');
      await vi.waitFor(() => expect(getVersion).toHaveBeenCalledTimes(1));
      change();
      resolveVersion('0.2.18');

      await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
      expect(mockFetch).not.toHaveBeenCalled();
    }
  );

  it('dispatches with refreshed credentials when they rotate during asynchronous header lookup', async () => {
    let resolveAttestation: (token: string | null) => void = () => {
      throw new Error('attestation resolver was not initialized');
    };
    const getToken = vi
      .fn()
      .mockImplementationOnce(
        () =>
          new Promise<string>((resolve) => {
            resolveAttestation = resolve;
          })
      )
      .mockResolvedValueOnce('new-attest');
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      getToken: () => getToken(),
    });
    const generation = useAuthStore
      .getState()
      .beginAuthLifecycle('original-token', 'original-session');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    const pending = apiFetch('/api/v1/test');
    await vi.waitFor(() => expect(getToken).toHaveBeenCalledTimes(1));
    expect(
      useAuthStore
        .getState()
        .rotateAuthCredentials(generation, 'refreshed-token', 'refreshed-session')
    ).toBe(true);
    resolveAttestation('old-attest');

    await expect(pending).resolves.toMatchObject({ status: 200 });
    expect(getToken).toHaveBeenCalledTimes(2);
    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('Authorization')).toBe('Bearer refreshed-token');
    expect(headers.get('X-Session-ID')).toBe('refreshed-session');
    expect(headers.get('X-Attestation-Token')).toBe('new-attest');
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
    _resetClientVersionCache();
    _resetRefreshState();
    resetRuntimeServerBase();
    vi.clearAllMocks();
    configureRefreshFailureReset({
      gracefulReset: mockGracefulReset,
      nuclearReset: mockNuclearReset,
    });
  });

  afterEach(() => {
    resetRuntimeServerBase();
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

  it.each([
    'CLIENT_VERSION_TOO_OLD',
    'ATTESTATION_UNKNOWN_RELEASE',
    'ATTESTATION_REVOKED',
  ] as const)(
    '%s publishes the terminal failure before a never-settling updater check',
    async (code) => {
      const forceCheckMock = vi.fn(() => new Promise<void>(() => {}));
      (globalThis as Record<string, unknown>).electron = makeElectronBridge({
        forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
      });
      useAuthStore.getState().setAccessToken('auth-token');
      mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse(code));

      await expect(apiFetch('/api/v1/test')).resolves.toMatchObject({ status: 403 });
      expect(forceCheckMock).toHaveBeenCalledWith('attestation_required');
      expect(useAttestationFailureStore.getState()).toMatchObject({ visible: true, code });
    }
  );

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

  it.each(['ATTESTATION_UNKNOWN_RELEASE', 'ATTESTATION_REVOKED'] as const)(
    '%s preserves the terminal denial when the updater check rejects',
    async (code) => {
      const forceCheckMock = vi.fn().mockRejectedValue(new Error('updater unavailable'));
      (globalThis as Record<string, unknown>).electron = makeElectronBridge({
        forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
      });
      useAuthStore.getState().setAccessToken('auth-token');
      mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse(code));

      const response = await apiFetch('/api/v1/test');

      expect(response.status).toBe(403);
      expect(forceCheckMock).toHaveBeenCalledWith('attestation_required');
      expect(useAttestationFailureStore.getState()).toMatchObject({ visible: true, code });
    }
  );

  it.each([
    'CLIENT_VERSION_TOO_OLD',
    'ATTESTATION_UNKNOWN_RELEASE',
    'ATTESTATION_REVOKED',
  ] as const)(
    '%s remains terminal after credentials rotate within the same generation',
    async (code) => {
      let resolveResponse: (response: Response) => void = () => {
        throw new Error('response resolver was not initialized');
      };
      const forceCheckMock = vi.fn().mockResolvedValue(undefined);
      (globalThis as Record<string, unknown>).electron = makeElectronBridge({
        forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
      });
      const generation = useAuthStore
        .getState()
        .beginAuthLifecycle('original-token', 'original-session');
      mockFetch.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveResponse = resolve;
          })
      );

      const pending = apiFetch('/api/v1/test');
      await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      expect(
        useAuthStore
          .getState()
          .rotateAuthCredentials(generation, 'rotated-token', 'rotated-session')
      ).toBe(true);
      resolveResponse(makeAttestationFailureResponse(code));

      await expect(pending).resolves.toMatchObject({ status: 403 });
      expect(forceCheckMock).toHaveBeenCalledWith('attestation_required');
      expect(useAttestationFailureStore.getState()).toMatchObject({ visible: true, code });
    }
  );

  it.each([
    ['the runtime server changes', () => setRuntimeServerBase('https://successor.example')],
    [
      'a successor auth generation begins',
      () => useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 }),
    ],
  ] as const)('suppresses a delayed terminal denial when %s', async (_name, changeAuthority) => {
    let resolveResponse: (response: Response) => void = () => {
      throw new Error('response resolver was not initialized');
    };
    const forceCheckMock = vi.fn().mockResolvedValue(undefined);
    (globalThis as Record<string, unknown>).electron = makeElectronBridge({
      forceCheckForUpdates: (reason: string) => forceCheckMock(reason),
    });
    useAuthStore.getState().setAccessToken('original-token');
    mockFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveResponse = resolve;
        })
    );

    const pending = apiFetch('/api/v1/test');
    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
    changeAuthority();
    resolveResponse(makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD'));

    await expect(pending).resolves.toMatchObject({ status: 403 });
    expect(forceCheckMock).not.toHaveBeenCalled();
    expect(useAttestationFailureStore.getState().visible).toBe(false);
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
    const getVersion = vi.fn().mockResolvedValue('0.2.18');

    (globalThis as Record<string, unknown>).electron = {
      getMachineId: vi.fn().mockResolvedValue('machine-uuid-456'),
      getVersion,
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
    expect(retryHeaders.get('X-Concord-Client-Version')).toBe('0.2.18');
    expect(getVersion).toHaveBeenCalledTimes(1);
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

  it('routes a terminal denial from the one ATTESTATION_EXPIRED retry without a second retry', async () => {
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('old-attest')
      .mockResolvedValueOnce('new-attest');
    const forceCheckForUpdates = vi.fn().mockResolvedValue(undefined);
    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({
        getToken: () => getTokenMock(),
        forceCheckForUpdates,
      }),
    } as unknown;
    useAuthStore.getState().setAccessToken('auth-token');
    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
    mockFetch.mockResolvedValueOnce(
      makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD', {
        requiredMinVersion: '0.2.44',
      })
    );

    await expect(apiFetch('/api/v1/test')).resolves.toMatchObject({ status: 403 });

    expect(mockFetch).toHaveBeenCalledTimes(2);
    expect(forceCheckForUpdates).toHaveBeenCalledWith('attestation_required');
    expect(useAttestationFailureStore.getState()).toMatchObject({
      visible: true,
      code: 'CLIENT_VERSION_TOO_OLD',
      requiredMinVersion: '0.2.44',
    });
  });

  it('keeps a terminal re-attest retry denial after credentials rotate within the same generation', async () => {
    let resolveRetry: (response: Response) => void = () => {
      throw new Error('retry resolver was not initialized');
    };
    const getTokenMock = vi
      .fn()
      .mockResolvedValueOnce('old-attest')
      .mockResolvedValueOnce('new-attest');
    const forceCheckForUpdates = vi.fn().mockResolvedValue(undefined);
    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({
        getToken: () => getTokenMock(),
        forceCheckForUpdates,
      }),
    } as unknown;
    const generation = useAuthStore
      .getState()
      .beginAuthLifecycle('original-token', 'original-session');
    mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
    mockFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveRetry = resolve;
        })
    );

    const pending = apiFetch('/api/v1/test');
    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
    expect(
      useAuthStore.getState().rotateAuthCredentials(generation, 'rotated-token', 'rotated-session')
    ).toBe(true);
    resolveRetry(makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD'));

    await expect(pending).resolves.toMatchObject({ status: 403 });
    expect(mockFetch).toHaveBeenCalledTimes(2);
    expect(forceCheckForUpdates).toHaveBeenCalledWith('attestation_required');
    expect(useAttestationFailureStore.getState()).toMatchObject({
      visible: true,
      code: 'CLIENT_VERSION_TOO_OLD',
    });
  });

  it.each([
    ['the runtime server changes', () => setRuntimeServerBase('https://successor.example')],
    [
      'the auth generation changes',
      () => useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 }),
    ],
  ] as const)(
    'suppresses a terminal denial from the re-attest retry when %s',
    async (_name, changeAuthority) => {
      let resolveRetry: (response: Response) => void = () => {
        throw new Error('retry resolver was not initialized');
      };
      const getTokenMock = vi
        .fn()
        .mockResolvedValueOnce('old-attest')
        .mockResolvedValueOnce('new-attest');
      const forceCheckForUpdates = vi.fn().mockResolvedValue(undefined);
      (globalThis as Record<string, unknown>).electron = {
        ...makeElectronBridge({
          getToken: () => getTokenMock(),
          forceCheckForUpdates,
        }),
      } as unknown;
      useAuthStore.getState().setAccessToken('auth-token');
      mockFetch.mockResolvedValueOnce(makeAttestationFailureResponse('ATTESTATION_EXPIRED'));
      mockFetch.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveRetry = resolve;
          })
      );

      const pending = apiFetch('/api/v1/test');
      await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
      changeAuthority();
      resolveRetry(makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD'));

      await expect(pending).resolves.toMatchObject({ status: 403 });
      expect(forceCheckForUpdates).not.toHaveBeenCalled();
      expect(useAttestationFailureStore.getState().visible).toBe(false);
    }
  );

  it.each([
    ['runtime server changes', () => setRuntimeServerBase('https://successor.example')],
    [
      'auth generation changes',
      () => useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 }),
    ],
  ] as const)(
    'does not re-attest against a stale origin when %s during version lookup',
    async (_name, changeAuthority) => {
      let resolveVersion: (version: string) => void = () => {
        throw new Error('version resolver was not initialized');
      };
      const getVersion = vi
        .fn()
        .mockResolvedValueOnce('0.2.18')
        .mockImplementationOnce(
          () =>
            new Promise<string>((resolve) => {
              resolveVersion = resolve;
            })
        );
      const getToken = vi.fn().mockResolvedValueOnce(null).mockResolvedValueOnce('fresh-attest');
      (globalThis as Record<string, unknown>).electron = makeElectronBridge({
        getVersion: () => getVersion(),
        getToken: () => getToken(),
      });
      useAuthStore.getState().setAccessToken('auth-token');
      mockFetch.mockImplementationOnce(async () => {
        _resetClientVersionCache();
        return makeAttestationFailureResponse('ATTESTATION_EXPIRED');
      });

      const pending = apiFetch('/api/v1/test');
      await vi.waitFor(() => expect(getVersion).toHaveBeenCalledTimes(2));
      changeAuthority();
      resolveVersion('0.2.18');

      await expect(pending).resolves.toMatchObject({ status: 403 });
      expect(mockFetch).toHaveBeenCalledTimes(1);
    }
  );

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
    _resetClientVersionCache();
    _resetRefreshState();
    vi.clearAllMocks();
    configureRefreshFailureReset({
      gracefulReset: mockGracefulReset,
      nuclearReset: mockNuclearReset,
    });
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

  it('routes a 403 floor denial from the one 401 retry through the terminal path', async () => {
    const forceCheckForUpdates = vi.fn().mockResolvedValue(undefined);
    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({ forceCheckForUpdates }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
    } as unknown;
    useAuthStore.getState().setAccessToken('expired-jwt');

    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockResolvedValueOnce(
      makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD', {
        requiredMinVersion: '0.2.44',
      })
    );

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(403);
    expect(mockFetch).toHaveBeenCalledTimes(2);
    expect(forceCheckForUpdates).toHaveBeenCalledWith('attestation_required');
    expect(useAttestationFailureStore.getState()).toMatchObject({
      visible: true,
      code: 'CLIENT_VERSION_TOO_OLD',
      requiredMinVersion: '0.2.44',
    });
  });

  it.each([
    ['the runtime server changes', () => setRuntimeServerBase('https://successor.example')],
    [
      'a successor auth generation begins',
      () => useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 }),
    ],
  ] as const)('suppresses a terminal retry denial when %s', async (_name, changeAuthority) => {
    let resolveRetry: (response: Response) => void = () => {
      throw new Error('retry resolver was not initialized');
    };
    const forceCheckForUpdates = vi.fn().mockResolvedValue(undefined);
    (globalThis as Record<string, unknown>).electron = {
      ...makeElectronBridge({ forceCheckForUpdates }),
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-jwt' }),
    } as unknown;
    useAuthStore.getState().setAccessToken('expired-jwt');
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    mockFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveRetry = resolve;
        })
    );

    const pending = apiFetch('/api/v1/test');
    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
    changeAuthority();
    resolveRetry(makeAttestationFailureResponse('CLIENT_VERSION_TOO_OLD'));

    await expect(pending).resolves.toMatchObject({ status: 403 });
    expect(forceCheckForUpdates).not.toHaveBeenCalled();
    expect(useAttestationFailureStore.getState().visible).toBe(false);
  });
});
