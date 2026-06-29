import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// We need to test apiClient which uses global fetch and authStore
// Import the store directly
import { useAuthStore } from '@/renderer/stores/authStore';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useMFAChallengeStore } from '@/renderer/stores/mfaChallengeStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';

// Mock resetService (dynamically imported by apiClient)
const mockGracefulReset = vi.fn();
const mockNuclearReset = vi.fn();
vi.mock('@/renderer/services/resetService', () => ({
  gracefulReset: mockGracefulReset,
  nuclearReset: mockNuclearReset,
  softRestart: vi.fn(),
  stopProactiveRefresh: vi.fn(),
}));

// Mock fetch globally — vi.stubGlobal is hoisted by vitest, so static import works
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

import {
  apiFetch,
  API_BASE,
  _resetRefreshState,
  refreshAccessToken,
  safeJson,
  ensureMachineId,
} from '@/renderer/services/apiClient';

describe('apiClient', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
    resetRuntimeServerBase();
    // Reset connection store to stable (default)
    useConnectionStore.getState().reset();
  });

  afterEach(() => {
    resetRuntimeServerBase();
    // Clean up electron mock
    (globalThis as any).electron = undefined;
  });

  it('injects Authorization header when token exists', async () => {
    useAuthStore.getState().setAccessToken('test-token');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    expect(mockFetch).toHaveBeenCalledWith(
      `${API_BASE}/api/v1/test`,
      expect.objectContaining({
        headers: expect.any(Headers),
        credentials: 'include',
      })
    );

    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('Authorization')).toBe('Bearer test-token');
  });

  it('uses the active runtime API base for requests', async () => {
    setRuntimeServerBase('https://homelab.lan:8443');
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    expect(mockFetch).toHaveBeenCalledWith(
      'https://homelab.lan:8443/api/v1/test',
      expect.objectContaining({ credentials: 'include' })
    );
  });

  it('does not inject Authorization header when no token', async () => {
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    const headers = mockFetch.mock.calls[0][1].headers as Headers;
    expect(headers.get('Authorization')).toBeNull();
  });

  it('returns response as-is for non-401 status', async () => {
    useAuthStore.getState().setAccessToken('test-token');
    mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');
    expect(response.status).toBe(200);
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it('attempts token refresh on 401 via IPC', async () => {
    useAuthStore.getState().setAccessToken('old-token');

    // Mock the electron IPC refresh — returns a new access token
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'new-token' }),
    } as any;

    // First call: 401
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    // Retry call after IPC refresh: success
    mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(200);
    expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(1);
    expect(mockFetch).toHaveBeenCalledTimes(2);
    // Verify retry has new token
    const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
    expect(retryHeaders.get('Authorization')).toBe('Bearer new-token');
    // Verify new token is stored in authStore
    expect(useAuthStore.getState().accessToken).toBe('new-token');
  });

  it('calls nuclearReset when IPC refresh fails and rememberMe is off', async () => {
    useAuthStore.getState().setAccessToken('old-token');
    useAuthStore.getState().setRememberMe(false);

    // Mock the electron IPC refresh — returns failure
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
    } as any;

    // First call: 401
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(401);
    expect(useAuthStore.getState().accessToken).toBeNull();
    // nuclearReset calls clearTokens internally
    expect(mockNuclearReset).toHaveBeenCalledTimes(1);
    expect(mockGracefulReset).not.toHaveBeenCalled();
    // Should not retry — only the original fetch
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it('calls gracefulReset when IPC refresh fails and rememberMe is on', async () => {
    useAuthStore.getState().setAccessToken('old-token');
    useAuthStore.getState().setRememberMe(true);

    // Mock the electron IPC refresh — returns failure
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
    } as any;

    // First call: 401
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

    const response = await apiFetch('/api/v1/test');

    expect(response.status).toBe(401);
    expect(useAuthStore.getState().accessToken).toBeNull();
    // gracefulReset preserves disk tokens for Remember Me session restore
    expect(mockGracefulReset).toHaveBeenCalledTimes(1);
    expect(mockNuclearReset).not.toHaveBeenCalled();
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  it('passes through request init options', async () => {
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test', {
      method: 'POST',
      body: JSON.stringify({ data: 'test' }),
    });

    expect(mockFetch.mock.calls[0][1].method).toBe('POST');
    expect(mockFetch.mock.calls[0][1].body).toBe(JSON.stringify({ data: 'test' }));
  });

  it('always includes credentials', async () => {
    mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

    await apiFetch('/api/v1/test');

    expect(mockFetch.mock.calls[0][1].credentials).toBe('include');
  });

  // ─── MFA challenge flow in refreshAccessToken ──────────────────────

  describe('refreshAccessToken MFA challenge', () => {
    it('handles MFA challenge → showChallenge → retry refresh', async () => {
      globalThis.electron = {
        refreshToken: vi
          .fn()
          // First call: MFA required
          .mockResolvedValueOnce({
            status: 'mfa_required',
            mfaChallengeToken: 'challenge-abc',
            mfaMethods: ['totp', 'webauthn'],
            mfaRecoveryOnlyMethods: ['recovery'],
          })
          // Second call (after MFA verified): success
          .mockResolvedValueOnce({
            status: 'ok',
            accessToken: 'mfa-refreshed-token',
            sessionId: 'sess-123',
          }),
      } as any;

      // Mock MFA challenge store to auto-complete verification
      const showChallengeSpy = vi
        .spyOn(useMFAChallengeStore.getState(), 'showChallenge')
        .mockResolvedValue({ verified: true, payload: {} });

      const token = await refreshAccessToken();

      expect(token).toBe('mfa-refreshed-token');
      expect(showChallengeSpy).toHaveBeenCalledWith(
        'challenge-abc',
        ['totp', 'webauthn'],
        'suspicious_refresh',
        ['recovery']
      );
      expect(useAuthStore.getState().accessToken).toBe('mfa-refreshed-token');
      expect(useAuthStore.getState().sessionId).toBe('sess-123');
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(2);

      showChallengeSpy.mockRestore();
    });

    it('returns null when MFA challenge is declined', async () => {
      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValueOnce({
          status: 'mfa_required',
          mfaChallengeToken: 'challenge-xyz',
          mfaMethods: ['totp'],
        }),
      } as any;

      // User declines MFA
      const showChallengeSpy = vi
        .spyOn(useMFAChallengeStore.getState(), 'showChallenge')
        .mockResolvedValue({ verified: false });

      const token = await refreshAccessToken();

      expect(token).toBeNull();
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(1);

      showChallengeSpy.mockRestore();
    });

    it('returns null when MFA retry refresh succeeds without sessionId', async () => {
      globalThis.electron = {
        refreshToken: vi
          .fn()
          .mockResolvedValueOnce({
            status: 'mfa_required',
            mfaChallengeToken: 'challenge-no-session',
            mfaMethods: ['totp'],
          })
          .mockResolvedValueOnce({
            status: 'ok',
            accessToken: 'mfa-token-no-session',
            // No sessionId
          }),
      } as any;

      const showChallengeSpy = vi
        .spyOn(useMFAChallengeStore.getState(), 'showChallenge')
        .mockResolvedValue({ verified: true, payload: {} });

      const token = await refreshAccessToken();

      expect(token).toBe('mfa-token-no-session');
      expect(useAuthStore.getState().accessToken).toBe('mfa-token-no-session');

      showChallengeSpy.mockRestore();
    });

    it('returns null when MFA retry refresh fails', async () => {
      globalThis.electron = {
        refreshToken: vi
          .fn()
          .mockResolvedValueOnce({
            status: 'mfa_required',
            mfaChallengeToken: 'challenge-retry',
            mfaMethods: ['totp'],
          })
          .mockResolvedValueOnce({ status: 'error' }),
      } as any;

      const showChallengeSpy = vi
        .spyOn(useMFAChallengeStore.getState(), 'showChallenge')
        .mockResolvedValue({ verified: true, payload: {} });

      const token = await refreshAccessToken();

      expect(token).toBeNull();
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(2);

      showChallengeSpy.mockRestore();
    });

    it('uses the IPC retry access_token even when the verify body carries a different access_token', async () => {
      globalThis.electron = {
        refreshToken: vi
          .fn()
          .mockResolvedValueOnce({
            status: 'mfa_required',
            mfaChallengeToken: 'challenge-divergence',
            mfaMethods: ['totp'],
          })
          .mockResolvedValueOnce({
            status: 'ok',
            accessToken: 'ipc-token-authoritative',
            sessionId: 'sess-ipc',
          }),
      } as any;

      // Body carries a different access_token. The IPC path is authoritative;
      // the body's value must NOT reach useAuthStore. A console.warn is emitted
      // so future incidents are debuggable, but token VALUES are never logged.
      const showChallengeSpy = vi
        .spyOn(useMFAChallengeStore.getState(), 'showChallenge')
        .mockResolvedValue({
          verified: true,
          payload: { access_token: 'body-token-discarded' },
        });
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

      const token = await refreshAccessToken();

      expect(token).toBe('ipc-token-authoritative');
      // Critically: the body's access_token must NOT have stomped the IPC token.
      expect(useAuthStore.getState().accessToken).toBe('ipc-token-authoritative');
      expect(useAuthStore.getState().accessToken).not.toBe('body-token-discarded');
      expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('MFA verify token divergence'));
      // Crucially: the warn message must NOT contain either token value.
      const warnMessage = warnSpy.mock.calls[0]?.[0] as string | undefined;
      expect(warnMessage).not.toContain('ipc-token-authoritative');
      expect(warnMessage).not.toContain('body-token-discarded');

      showChallengeSpy.mockRestore();
      warnSpy.mockRestore();
    });

    it('does NOT emit a divergence warn when the body has no access_token (suspicious_refresh shape)', async () => {
      globalThis.electron = {
        refreshToken: vi
          .fn()
          .mockResolvedValueOnce({
            status: 'mfa_required',
            mfaChallengeToken: 'challenge-no-body-token',
            mfaMethods: ['totp'],
          })
          .mockResolvedValueOnce({
            status: 'ok',
            accessToken: 'ipc-token-only',
            sessionId: 'sess-ipc-only',
          }),
      } as any;

      // The wire shape for PurposeSuspiciousRefresh is { verified, purpose,
      // user_id } — no access_token in the body. The divergence check
      // short-circuits via optional chaining and no warn is emitted.
      const showChallengeSpy = vi
        .spyOn(useMFAChallengeStore.getState(), 'showChallenge')
        .mockResolvedValue({ verified: true, payload: {} });
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

      await refreshAccessToken();

      expect(useAuthStore.getState().accessToken).toBe('ipc-token-only');
      expect(warnSpy).not.toHaveBeenCalled();

      showChallengeSpy.mockRestore();
      warnSpy.mockRestore();
    });
  });

  it('refreshAccessToken returns null when electron.refreshToken is unavailable', async () => {
    globalThis.electron = {} as any;

    const token = await refreshAccessToken();

    expect(token).toBeNull();
  });

  it('refreshAccessToken stores sessionId when refresh succeeds with one', async () => {
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({
        status: 'ok',
        accessToken: 'token-with-session',
        sessionId: 'session-999',
      }),
    } as any;

    const token = await refreshAccessToken();

    expect(token).toBe('token-with-session');
    expect(useAuthStore.getState().sessionId).toBe('session-999');
  });

  it('refreshAccessToken succeeds without sessionId', async () => {
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({
        status: 'ok',
        accessToken: 'token-no-session',
      }),
    } as any;

    const token = await refreshAccessToken();

    expect(token).toBe('token-no-session');
    expect(useAuthStore.getState().accessToken).toBe('token-no-session');
  });

  // ─── 401 recovery edge cases ──────────────────────────────────────

  describe('401 recovery', () => {
    it('returns 401 without refresh when auth already cleared', async () => {
      // No token set — auth already cleared
      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'new' }),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      const response = await apiFetch('/api/v1/test');

      expect(response.status).toBe(401);
      // refresh should NOT be called since no token was set
      expect(globalThis.electron!.refreshToken).not.toHaveBeenCalled();
      expect(mockFetch).toHaveBeenCalledTimes(1);
    });

    it('rate-limits refresh initiation', async () => {
      useAuthStore.getState().setAccessToken('old-token');

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'new-token' }),
      } as any;

      // First 401 — triggers refresh
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));
      await apiFetch('/api/v1/test');

      // Second 401 immediately — should be rate-limited (within 10s window)
      useAuthStore.getState().setAccessToken('old-token-2');
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      const response = await apiFetch('/api/v1/test2');

      expect(response.status).toBe(401);
      // refreshToken should only have been called once (rate-limited second time)
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(1);
    });

    it('calls gracefulReset when refresh fails and rememberMe is true', async () => {
      useAuthStore.getState().setAccessToken('old-token');
      useAuthStore.getState().setRememberMe(true);

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      await apiFetch('/api/v1/test');

      expect(mockGracefulReset).toHaveBeenCalledTimes(1);
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

    it('calls nuclearReset when refresh fails and rememberMe is false', async () => {
      useAuthStore.getState().setAccessToken('old-token');
      useAuthStore.getState().setRememberMe(false);

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
        clearTokens: vi.fn(),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      await apiFetch('/api/v1/test');

      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

    it('piggybacks on in-flight refresh when concurrent 401s occur', async () => {
      useAuthStore.getState().setAccessToken('old-token');

      let resolveRefresh!: (value: any) => void;
      globalThis.electron = {
        refreshToken: vi.fn().mockReturnValue(
          new Promise((resolve) => {
            resolveRefresh = resolve;
          })
        ),
      } as any;

      // Two concurrent 401 responses
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      // Two retry responses after refresh
      mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));
      mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

      // Start two concurrent apiFetch calls
      const p1 = apiFetch('/api/v1/test1');
      const p2 = apiFetch('/api/v1/test2');

      // Resolve the shared refresh
      resolveRefresh({ status: 'ok', accessToken: 'shared-token' });

      const [r1, r2] = await Promise.all([p1, p2]);

      expect(r1.status).toBe(200);
      expect(r2.status).toBe(200);
      // refreshToken should be called only once — second 401 piggybacks
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(1);
    });

    it('deduplicates concurrent refreshAccessToken calls', async () => {
      let resolveRefresh!: (value: any) => void;
      globalThis.electron = {
        refreshToken: vi.fn().mockReturnValue(
          new Promise((resolve) => {
            resolveRefresh = resolve;
          })
        ),
      } as any;

      // Start two concurrent refreshAccessToken calls
      const p1 = refreshAccessToken();
      const p2 = refreshAccessToken();

      resolveRefresh({ status: 'ok', accessToken: 'deduped-token' });

      const [t1, t2] = await Promise.all([p1, p2]);

      expect(t1).toBe('deduped-token');
      expect(t2).toBe('deduped-token');
      // Only one actual IPC call
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(1);
    });

    it('includes X-Machine-Id header when machine ID is cached', async () => {
      // Set up machine ID via ensureMachineId
      globalThis.electron = {
        getMachineId: vi.fn().mockResolvedValue('machine-uuid-123'),
      } as any;
      await ensureMachineId();

      useAuthStore.getState().setAccessToken('test-token');
      mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

      await apiFetch('/api/v1/test');

      const headers = mockFetch.mock.calls[0][1].headers as Headers;
      expect(headers.get('X-Machine-Id')).toBe('machine-uuid-123');
    });

    it('caches machine IDs per active runtime API base', async () => {
      const getMachineId = vi
        .fn()
        .mockResolvedValueOnce('saas-machine')
        .mockResolvedValueOnce('self-machine');
      globalThis.electron = { getMachineId } as any;

      await expect(ensureMachineId()).resolves.toBe('saas-machine');
      setRuntimeServerBase('https://homelab.lan:8443');
      await expect(ensureMachineId()).resolves.toBe('self-machine');

      expect(getMachineId).toHaveBeenNthCalledWith(1, API_BASE);
      expect(getMachineId).toHaveBeenNthCalledWith(2, 'https://homelab.lan:8443');
    });

    it('includes X-Machine-Id on retry after 401 refresh', async () => {
      // Ensure machine ID is cached for this test (self-contained)
      globalThis.electron = {
        getMachineId: vi.fn().mockResolvedValue('machine-uuid-123'),
      } as any;
      await ensureMachineId();

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'new-token' }),
      } as any;

      useAuthStore.getState().setAccessToken('old-token');
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));

      await apiFetch('/api/v1/test');

      // Verify retry request includes machine ID
      const retryHeaders = mockFetch.mock.calls[1][1].headers as Headers;
      expect(retryHeaders.get('X-Machine-Id')).toBe('machine-uuid-123');
      expect(retryHeaders.get('Authorization')).toBe('Bearer new-token');
    });

    it('skips handleRefreshFailure when token already cleared by concurrent handler', async () => {
      useAuthStore.getState().setAccessToken('old-token');

      // refreshToken clears the access token mid-flight (simulating concurrent handler)
      globalThis.electron = {
        refreshToken: vi.fn().mockImplementation(async () => {
          useAuthStore.getState().clearAccessToken();
          return { status: 'error' };
        }),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      const response = await apiFetch('/api/v1/test');

      expect(response.status).toBe(401);
      // handleRefreshFailure should skip since token was already cleared
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(mockNuclearReset).not.toHaveBeenCalled();
    });

    it('only clears accessToken when connection phase is not stable', async () => {
      useAuthStore.getState().setAccessToken('old-token');
      useAuthStore.getState().setRememberMe(true);
      // Simulate recovery in progress
      useConnectionStore.setState({ phase: 'reconnecting' as any });

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      const response = await apiFetch('/api/v1/test');

      expect(response.status).toBe(401);
      expect(useAuthStore.getState().accessToken).toBeNull();
      // Should NOT call gracefulReset or nuclearReset during recovery
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(mockNuclearReset).not.toHaveBeenCalled();
    });
  });

  // ─── safeJson ─────────────────────────────────────────────────────

  describe('safeJson', () => {
    it('parses valid JSON response', async () => {
      const res = new Response(JSON.stringify({ name: 'test', value: 42 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });

      const data = await safeJson<{ name: string; value: number }>(res);
      expect(data).toEqual({ name: 'test', value: 42 });
    });

    it('parses application/problem+json content type', async () => {
      const res = new Response(JSON.stringify({ error: 'not found' }), {
        status: 404,
        headers: { 'Content-Type': 'application/problem+json' },
      });

      const data = await safeJson(res);
      expect(data).toEqual({ error: 'not found' });
    });

    it('throws descriptive error for non-JSON content type', async () => {
      const res = new Response('<html>Cloudflare error</html>', {
        status: 502,
        headers: { 'Content-Type': 'text/html' },
      });

      await expect(safeJson(res)).rejects.toThrow(
        'Expected JSON but got text/html (HTTP 502): <html>Cloudflare error</html>'
      );
    });

    it('throws descriptive error when Content-Type is non-JSON', async () => {
      const res = new Response('not json', {
        status: 500,
        headers: { 'Content-Type': 'text/plain' },
      });

      await expect(safeJson(res)).rejects.toThrow(
        'Expected JSON but got text/plain (HTTP 500): not json'
      );
    });

    it('throws descriptive error for invalid JSON body with json content type', async () => {
      const res = new Response('not valid json {{{', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });

      await expect(safeJson(res)).rejects.toThrow(
        'Invalid JSON in response (HTTP 200, Content-Type: application/json)'
      );
    });
  });
});
