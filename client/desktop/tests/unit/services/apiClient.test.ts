import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// We need to test apiClient which uses global fetch and authStore
// Import the store directly
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
import { useMFAChallengeStore } from '@/renderer/stores/auth/mfaChallengeStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';

const mockGracefulReset = vi.fn();
const mockNuclearReset = vi.fn();

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
  revokeAbortedSession,
  configureRefreshFailureReset,
} from '@/renderer/services/apiClient';

describe('apiClient', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
    configureRefreshFailureReset({
      gracefulReset: mockGracefulReset,
      nuclearReset: mockNuclearReset,
    });
    mockFetch.mockReset();
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

  it('does not recover or tear down after an A -> B -> A switch during attestation', async () => {
    const requestApiBase = 'https://old-origin.example';
    setRuntimeServerBase(requestApiBase);
    useAuthStore.getState().setAccessToken('old-token');
    let resolveAttestation: (token: string | null) => void = () => {
      throw new Error('Attestation resolver was not initialized.');
    };
    const refreshToken = vi.fn().mockResolvedValue({ status: 'error' });
    globalThis.electron = {
      refreshToken,
      attestation: {
        getToken: vi.fn().mockReturnValue(
          new Promise<string | null>((resolve) => {
            resolveAttestation = resolve;
          })
        ),
      },
    } as any;
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

    const pending = apiFetch('/api/v1/users/me');
    setRuntimeServerBase('https://successor-origin.example');
    setRuntimeServerBase(requestApiBase);
    resolveAttestation(null);
    const response = await pending;

    expect(response.status).toBe(401);
    expect(mockFetch).toHaveBeenCalledWith(`${requestApiBase}/api/v1/users/me`, expect.any(Object));
    expect(refreshToken).not.toHaveBeenCalled();
    expect(mockGracefulReset).not.toHaveBeenCalled();
    expect(mockNuclearReset).not.toHaveBeenCalled();
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

  // #2425: a delayed pre-rotation 401 must not tear down a session that was
  // rotated (T1/S1/G -> T2/S1/G) after the request was issued. requestLifecycleIsCurrent
  // now requires the exact captured token + session, not merely a live generation.
  describe('#2425 stale pre-refresh 401 teardown guard', () => {
    // Deferred fetch + a "fetch dispatched" barrier so the rotation deterministically
    // lands while the original request is in-flight, then the 401 is delivered AFTER
    // it — independent of apiFetch's internal await ordering (attestation lookup etc.).
    function deferredFetch(): { started: Promise<void>; resolve: (r: Response) => void } {
      let resolve!: (r: Response) => void;
      let markStarted!: () => void;
      const started = new Promise<void>((r) => {
        markStarted = r;
      });
      mockFetch.mockImplementationOnce(() => {
        markStarted();
        return new Promise<Response>((r) => {
          resolve = r;
        });
      });
      // resolve is assigned when the mock is invoked (after `started`); wrap it so
      // callers read the live closure binding rather than its undefined value here.
      return { started, resolve: (r: Response) => resolve(r) };
    }

    it('does not refresh or tear down when the access token rotated after the request', async () => {
      const g = useAuthStore.getState().beginAuthLifecycle('T1', 'S1');
      const refreshToken = vi.fn();
      globalThis.electron = { refreshToken } as any;
      const fetchCtl = deferredFetch();

      const pending = apiFetch('/api/v1/users/me');
      await fetchCtl.started; // request dispatched with T1/S1/g
      // A successful refresh on another path rotated T1 -> T2, preserving generation.
      useAuthStore.getState().rotateAuthCredentials(g, 'T2', 'S1');
      fetchCtl.resolve(new Response('unauthorized', { status: 401 }));
      const response = await pending;

      expect(response.status).toBe(401);
      expect(refreshToken).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(mockNuclearReset).not.toHaveBeenCalled();
      // Rotated credentials + generation are intact.
      expect(useAuthStore.getState().accessToken).toBe('T2');
      expect(useAuthStore.getState().authGeneration).toBe(g);
    });

    it('does not tear down when the session ID changed within the generation', async () => {
      const g = useAuthStore.getState().beginAuthLifecycle('T1', 'S1');
      const refreshToken = vi.fn();
      globalThis.electron = { refreshToken } as any;
      const fetchCtl = deferredFetch();

      const pending = apiFetch('/api/v1/users/me');
      await fetchCtl.started; // request dispatched with T1/S1/g
      // Same token, session S1 -> S2 within the same generation.
      useAuthStore.getState().rotateAuthCredentials(g, 'T1', 'S2');
      fetchCtl.resolve(new Response('unauthorized', { status: 401 }));
      const response = await pending;

      expect(response.status).toBe(401);
      expect(refreshToken).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(mockNuclearReset).not.toHaveBeenCalled();
    });

    it('still tears down when a request with the CURRENT rotated credentials 401s (revocation preserved)', async () => {
      const g = useAuthStore.getState().beginAuthLifecycle('T1', 'S1');
      useAuthStore.getState().rotateAuthCredentials(g, 'T2', 'S1'); // current is now T2/S1/g
      useAuthStore.getState().setRememberMe(false);
      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
      } as any;
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      // A NEW request captures the current T2/S1/g and is not rotated away from.
      const response = await apiFetch('/api/v1/users/me');

      expect(response.status).toBe(401);
      // Current-credential authoritative 401 with a failed refresh still tears down.
      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
    });
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
      const generation = useAuthStore
        .getState()
        .beginAuthLifecycle('pre-mfa-token', 'sess-before-mfa');
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
            previousSessionId: 'sess-before-mfa',
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
      expect(useAuthStore.getState().authGeneration).toBe(generation);
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(2);

      showChallengeSpy.mockRestore();
    });

    it('returns null when MFA challenge is declined', async () => {
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', null);
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
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', null);
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
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', null);
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
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', 'sess-before-divergence');
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
            previousSessionId: 'sess-before-divergence',
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
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', 'sess-before-ipc-only');
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
            previousSessionId: 'sess-before-ipc-only',
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
    const generation = useAuthStore.getState().beginAuthLifecycle('old-token', 'session-111');
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({
        status: 'ok',
        accessToken: 'token-with-session',
        sessionId: 'session-999',
        previousSessionId: 'session-111',
      }),
    } as any;

    const token = await refreshAccessToken();

    expect(token).toBe('token-with-session');
    expect(useAuthStore.getState().sessionId).toBe('session-999');
    expect(useAuthStore.getState().authGeneration).toBe(generation);
  });

  it('rejects S1 -> S2 refresh rotation without matching previousSessionId', async () => {
    const generation = useAuthStore.getState().beginAuthLifecycle('old-token', 'session-111');
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({
        status: 'ok',
        accessToken: 'unproven-token',
        sessionId: 'unproven-session',
      }),
    } as any;

    await expect(refreshAccessToken()).resolves.toBeNull();
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'old-token',
      sessionId: 'session-111',
      authGeneration: generation,
    });
  });

  it('refreshAccessToken succeeds without sessionId', async () => {
    useAuthStore.getState().beginAuthLifecycle('old-token', null);
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
      useAuthStore.getState().setSessionId('same-session');

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'new-token' }),
      } as any;

      // First 401 — triggers refresh
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));
      await apiFetch('/api/v1/test');

      // Second 401 immediately — should be rate-limited (within 10s window)
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

    // #1957 — a non-authoritative request (a third-party content proxy such as
    // the KLIPY GIF proxy) must NEVER tear down the session on a 401. Its 401 is
    // not proof the Concord session is dead; genuine expiry is still caught by
    // authoritative API calls (the tests above, which keep today's behavior).
    it('does NOT tear down the session on a non-authoritative 401 when refresh fails (#1957)', async () => {
      useAuthStore.getState().setAccessToken('valid-token');
      useAuthStore.getState().setRememberMe(false); // worst case — a teardown here would clearTokens

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
        clearTokens: vi.fn(),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      const response = await apiFetch('/api/v1/klipy/gifs/items', undefined, {
        authoritative: false,
      });

      expect(response.status).toBe(401);
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState().accessToken).toBe('valid-token');
    });

    it('does NOT tear down the session on a non-authoritative 401 inside the refresh cooldown (#1957)', async () => {
      useAuthStore.getState().setAccessToken('valid-token');
      useAuthStore.getState().setRememberMe(false);

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'refreshed-token' }),
        clearTokens: vi.fn(),
      } as any;

      // Prime lastRefreshTimestamp with a real authoritative refresh so the next
      // 401 lands inside the ≤10s cooldown "assume revocation" branch.
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      mockFetch.mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));
      await apiFetch('/api/v1/messages');

      // A non-authoritative content-proxy 401 within the cooldown window.
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      const response = await apiFetch('/api/v1/klipy/gifs/items', undefined, {
        authoritative: false,
      });

      expect(response.status).toBe(401);
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState().accessToken).not.toBeNull();
    });

    it('still refreshes and retries a non-authoritative 401 when the token is merely stale (#1957)', async () => {
      useAuthStore.getState().setAccessToken('stale-token');

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: 'fresh-token' }),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      mockFetch.mockResolvedValueOnce(new Response('{"gif":true}', { status: 200 }));

      const response = await apiFetch('/api/v1/klipy/gifs/items', undefined, {
        authoritative: false,
      });

      // Opportunistic refresh+retry still works — a merely-expired access token
      // transparently recovers the GIF, no teardown.
      expect(response.status).toBe(200);
      expect(globalThis.electron!.refreshToken).toHaveBeenCalledTimes(1);
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
    });

    it('does not adopt or retry a held 401 refresh after the auth lifecycle changes', async () => {
      useAuthStore.getState().setAccessToken('old-account-token');
      useAuthStore.getState().setSessionId('old-account-session');

      let releaseRefresh: ((value: unknown) => void) | undefined;
      const refreshToken = vi.fn(
        () =>
          new Promise((resolve) => {
            releaseRefresh = resolve;
          })
      );
      globalThis.electron = { refreshToken } as any;
      mockFetch
        .mockResolvedValueOnce(new Response('unauthorized', { status: 401 }))
        .mockResolvedValueOnce(new Response('{"wrongAccount":true}', { status: 200 }));

      const controller = new AbortController();
      const request = apiFetch(
        '/api/v1/users/me/preferences',
        { method: 'PUT', signal: controller.signal },
        { authoritative: false }
      );
      await vi.waitFor(() => expect(refreshToken).toHaveBeenCalledOnce());

      controller.abort();
      useAuthStore.getState().setAccessToken('new-account-token');
      useAuthStore.getState().setSessionId('new-account-session');
      releaseRefresh?.({
        status: 'ok',
        accessToken: 'refreshed-old-account-token',
        sessionId: 'old-account-session',
      });

      const response = await request;
      expect(response.status).toBe(401);
      expect(mockFetch).toHaveBeenCalledTimes(1);
      expect(useAuthStore.getState()).toMatchObject({
        accessToken: 'new-account-token',
        sessionId: 'new-account-session',
      });
    });

    it('starts a new-account refresh after a held prior-account refresh settles', async () => {
      useAuthStore.getState().setAccessToken('account-a-token');
      useAuthStore.getState().setSessionId('account-a-session');

      let releaseAccountA: ((value: unknown) => void) | undefined;
      const refreshToken = vi
        .fn()
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              releaseAccountA = resolve;
            })
        )
        .mockResolvedValueOnce({
          status: 'ok',
          accessToken: 'account-b-refreshed-token',
          sessionId: 'account-b-session',
        });
      globalThis.electron = { refreshToken } as any;

      const accountARefresh = refreshAccessToken();
      await vi.waitFor(() => expect(refreshToken).toHaveBeenCalledTimes(1));

      useAuthStore.getState().setAccessToken('account-b-token');
      useAuthStore.getState().setSessionId('account-b-session');
      mockFetch
        .mockResolvedValueOnce(new Response('unauthorized', { status: 401 }))
        .mockResolvedValueOnce(new Response('{"account":"b"}', { status: 200 }));

      const accountBRequest = apiFetch('/api/v1/users/me');
      await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
      releaseAccountA?.({
        status: 'ok',
        accessToken: 'account-a-refreshed-token',
        sessionId: 'account-a-session',
      });

      await expect(accountARefresh).resolves.toBeNull();
      const response = await accountBRequest;
      expect(response.status).toBe(200);
      expect(refreshToken).toHaveBeenCalledTimes(2);
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState()).toMatchObject({
        accessToken: 'account-b-refreshed-token',
        sessionId: 'account-b-session',
      });
    });

    it('does not apply a prior account refresh cooldown to the new account', async () => {
      useAuthStore.getState().setAccessToken('account-a-token');
      useAuthStore.getState().setSessionId('account-a-session');

      const refreshToken = vi
        .fn()
        .mockResolvedValueOnce({
          status: 'ok',
          accessToken: 'account-a-refreshed-token',
          sessionId: 'account-a-session',
        })
        .mockResolvedValueOnce({
          status: 'ok',
          accessToken: 'account-b-refreshed-token',
          sessionId: 'account-b-session',
        });
      globalThis.electron = { refreshToken } as any;
      mockFetch
        .mockResolvedValueOnce(new Response('unauthorized', { status: 401 }))
        .mockResolvedValueOnce(new Response('{"account":"a"}', { status: 200 }));
      await expect(apiFetch('/api/v1/users/me')).resolves.toMatchObject({ status: 200 });

      useAuthStore.getState().setAccessToken('account-b-token');
      useAuthStore.getState().setSessionId('account-b-session');
      mockFetch
        .mockResolvedValueOnce(new Response('unauthorized', { status: 401 }))
        .mockResolvedValueOnce(new Response('{"account":"b"}', { status: 200 }));

      const response = await apiFetch('/api/v1/users/me');
      expect(response.status).toBe(200);
      expect(refreshToken).toHaveBeenCalledTimes(2);
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState()).toMatchObject({
        accessToken: 'account-b-refreshed-token',
        sessionId: 'account-b-session',
      });
    });

    it('does not re-attest or retry a held 403 after the auth lifecycle changes', async () => {
      useAuthStore.getState().setAccessToken('old-account-token');
      useAuthStore.getState().setSessionId('old-account-session');

      let releaseClear: (() => void) | undefined;
      const clearToken = vi.fn(
        () =>
          new Promise<void>((resolve) => {
            releaseClear = resolve;
          })
      );
      const getToken = vi
        .fn()
        .mockResolvedValueOnce('cached-old-account-attestation')
        .mockResolvedValueOnce('fresh-old-account-attestation');
      globalThis.electron = { attestation: { clearToken, getToken } } as any;
      mockFetch
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ code: 'ATTESTATION_EXPIRED' }), {
            status: 403,
            headers: { 'Content-Type': 'application/json' },
          })
        )
        .mockResolvedValueOnce(new Response('{"wrongAccount":true}', { status: 200 }));

      const controller = new AbortController();
      const request = apiFetch('/api/v1/users/me/saved-gifs', {
        method: 'PUT',
        signal: controller.signal,
      });
      await vi.waitFor(() => expect(clearToken).toHaveBeenCalledOnce());

      controller.abort();
      useAuthStore.getState().setAccessToken('new-account-token');
      useAuthStore.getState().setSessionId('new-account-session');
      releaseClear?.();

      const response = await request;
      expect(response.status).toBe(403);
      expect(mockFetch).toHaveBeenCalledTimes(1);
      // One lookup is expected for the original request. The lifecycle fence
      // must prevent the second lookup that would mint credentials for a retry.
      expect(getToken).toHaveBeenCalledTimes(1);
      expect(useAuthStore.getState()).toMatchObject({
        accessToken: 'new-account-token',
        sessionId: 'new-account-session',
      });
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
      useAuthStore.getState().beginAuthLifecycle('old-token', null);
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

    // #2415 case 9: the continuation row the password change mints copies
    // `machine_id` straight from this header, and `checkMachineIDTheft` treats an
    // EMPTY machine id as permissive — so a rotating POST that lost the header
    // would silently mint a continuation session with theft detection disabled.
    // This is the pin for that: `apiFetch`'s own header block is URL- and
    // method-agnostic, so the ChangePassword POST takes exactly this path on its
    // first (non-retry) attempt. The 401-refresh retry (`build401RetryHeaders`)
    // and the 403-attestation retry (`buildAttestationRetryHeaders`) build their
    // headers separately and are covered separately.
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

    it('honors an explicit machine-ID base across a runtime server switch', async () => {
      const requestApiBase = 'https://old-origin.example';
      let resolveMachineId: (machineId: string) => void = () => {
        throw new Error('Machine ID resolver was not initialized.');
      };
      const getMachineId = vi.fn().mockReturnValue(
        new Promise<string>((resolve) => {
          resolveMachineId = resolve;
        })
      );
      globalThis.electron = { getMachineId } as any;

      const pending = ensureMachineId(requestApiBase);
      setRuntimeServerBase('https://successor-origin.example');
      resolveMachineId('old-origin-machine');

      await expect(pending).resolves.toBe('old-origin-machine');
      expect(getMachineId).toHaveBeenCalledWith(requestApiBase);
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

    it('runs gracefulReset when authoritative refresh fails during recovery', async () => {
      useAuthStore.getState().setAccessToken('old-token');
      useAuthStore.getState().setSessionId('session-a');
      useAuthStore.getState().setRememberMe(true);
      useConnectionStore.setState({ phase: 'recovery_a' });

      globalThis.electron = {
        refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
      } as any;

      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

      const response = await apiFetch('/api/v1/test');

      expect(response.status).toBe(401);
      // An authoritative failure ends this lifecycle even while transport
      // recovery is active; gracefulReset clears renderer E2EE custody and
      // decrypted account state before auth identity is discarded.
      expect(mockGracefulReset).toHaveBeenCalledOnce();
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState()).toMatchObject({ accessToken: null, sessionId: null });
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

  describe('revokeAbortedSession (#2337)', () => {
    it('revokes a password session with its explicit refresh token and no ambient credentials', async () => {
      mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

      await revokeAbortedSession({
        accessToken: 'aborted-access',
        refreshToken: 'aborted-refresh',
        sessionId: 'aborted-session',
      });

      expect(mockFetch).toHaveBeenCalledTimes(1);
      const [url, init] = mockFetch.mock.calls[0];
      expect(String(url)).toContain('/api/v1/auth/logout');
      expect(init.method).toBe('POST');
      expect(init.credentials).toBe('omit');
      const headers = new Headers(init.headers);
      expect(headers.get('Authorization')).toBe('Bearer aborted-access');
      expect(headers.get('X-Refresh-Token')).toBe('aborted-refresh');
      expect(headers.get('X-Session-ID')).toBeNull();
    });

    it('revokes a cookie-only SSO session by bearer-owned session ID', async () => {
      mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

      await revokeAbortedSession({ accessToken: 'sso-access', sessionId: 'sso-session' });

      const [, init] = mockFetch.mock.calls[0];
      expect(init.credentials).toBe('omit');
      const headers = new Headers(init.headers);
      expect(headers.get('Authorization')).toBe('Bearer sso-access');
      expect(headers.get('X-Session-ID')).toBe('sso-session');
      expect(headers.get('X-Refresh-Token')).toBeNull();
    });

    it('uses the aborted explicit credential even when a successor owns the auth store', async () => {
      useAuthStore.getState().beginAuthLifecycle('successor-access', 'successor-session');
      mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

      await revokeAbortedSession({
        accessToken: 'aborted-access',
        refreshToken: 'aborted-refresh',
        sessionId: 'aborted-session',
      });

      expect(mockFetch).toHaveBeenCalledTimes(1);
      const [, init] = mockFetch.mock.calls[0];
      expect(init.credentials).toBe('omit');
      expect(new Headers(init.headers).get('X-Refresh-Token')).toBe('aborted-refresh');
    });

    it('revokes against the ABORTED origin when the runtime server switched (Codex P1, #2337)', async () => {
      mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

      await revokeAbortedSession({
        accessToken: 'A',
        sessionId: 'S1',
        apiBase: 'https://aborted.example',
      });

      expect(mockFetch).toHaveBeenCalledTimes(1);
      const [url, init] = mockFetch.mock.calls[0];
      expect(String(url)).toBe('https://aborted.example/api/v1/auth/logout');
      expect(init.credentials).toBe('omit');
      expect(new Headers(init.headers).get('Authorization')).toBe('Bearer A');
    });

    it('revokes a header-identified session through its matching ambient cookie', async () => {
      mockFetch.mockResolvedValueOnce(new Response('{}', { status: 200 }));

      await revokeAbortedSession({
        accessToken: null,
        sessionId: 'cookie-bound-session',
        cookieBound: true,
      });

      expect(mockFetch).toHaveBeenCalledTimes(1);
      const [, init] = mockFetch.mock.calls[0];
      expect(init.credentials).toBe('include');
      const headers = new Headers(init.headers);
      expect(headers.get('X-Session-ID')).toBe('cookie-bound-session');
      expect(headers.get('Authorization')).toBeNull();
      expect(headers.get('X-Refresh-Token')).toBeNull();
    });

    it('does not dispatch when no explicit or cookie-bound credential is available', async () => {
      await revokeAbortedSession({ accessToken: null, sessionId: 'cookie-only' });
      await revokeAbortedSession({ accessToken: 'token-only', sessionId: null });
      await revokeAbortedSession({ accessToken: null, sessionId: null, cookieBound: true });

      expect(mockFetch).not.toHaveBeenCalled();
    });

    it('does not route a 401 through authoritative recovery and swallows network failures', async () => {
      mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
      await revokeAbortedSession({ accessToken: 'A', sessionId: 'S1' });

      mockFetch.mockRejectedValueOnce(new Error('network down'));
      await expect(
        revokeAbortedSession({ accessToken: 'A', sessionId: 'S1' })
      ).resolves.toBeUndefined();

      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(mockGracefulReset).not.toHaveBeenCalled();
    });
  });
});
