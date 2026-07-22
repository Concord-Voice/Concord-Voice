/**
 * Unit tests for the sso:* IPC handlers (#270 review).
 *
 * Coverage:
 *   - sender-frame validation rejects untrusted origins on all three channels
 *   - active-map cleanup removes entries on awaitCallback / cancelLoopback /
 *     promise-settle
 *   - awaitCallback against an unknown port throws a stable error rather than
 *     hanging
 *
 * The loopback server itself is mocked — these tests exercise the IPC plumbing,
 * not the HTTP listener (that's covered by ssoLoopback.test.ts). The mock lets
 * us verify the cleanup contract deterministically without binding real ports.
 */
import { net } from 'electron';
import { describe, expect, it, vi, beforeEach } from 'vitest';

// vi.mock is hoisted above ALL `const` declarations, so any references to
// outer-scope state from inside a vi.mock factory must come from vi.hoisted —
// regular module-scope values are not yet initialized when the factory runs.
const mocked = vi.hoisted(() => {
  interface FakeHandle {
    port: number;
    redirectURI: string;
    bindAddress: string;
    promise: Promise<{ code: string; state: string }>;
    close: () => void;
    __resolve: (v: { code: string; state: string }) => void;
    __reject: (e: Error) => void;
    __closeCalls: number;
  }
  const handleSpy = vi.fn();
  const onSpy = vi.fn();
  const fakeHandles: FakeHandle[] = [];
  let nextPort = 51000;
  const startLoopback = vi.fn(async () => {
    let resolveFn!: (v: { code: string; state: string }) => void;
    let rejectFn!: (e: Error) => void;
    const promise = new Promise<{ code: string; state: string }>((res, rej) => {
      resolveFn = res;
      rejectFn = rej;
    });
    // Suppress unhandled-rejection noise — close() may reject, and tests
    // that don't await the promise would otherwise pollute test output.
    promise.catch(() => undefined);
    const port = nextPort++;
    const handle: FakeHandle = {
      port,
      redirectURI: `http://127.0.0.1:${port}/oauth/callback`,
      bindAddress: '127.0.0.1',
      promise,
      close: () => {
        handle.__closeCalls += 1;
        rejectFn(new Error('oauth_cancelled'));
      },
      __resolve: resolveFn,
      __reject: rejectFn,
      __closeCalls: 0,
    };
    fakeHandles.push(handle);
    return handle;
  });
  return { handleSpy, onSpy, fakeHandles, startLoopback };
});

vi.mock('electron', () => ({
  ipcMain: {
    handle: mocked.handleSpy,
    on: mocked.onSpy,
  },
  net: { fetch: vi.fn() },
  shell: { openExternal: vi.fn() },
}));

vi.mock('../../../../src/main/ssoLoopback', () => ({
  startLoopback: mocked.startLoopback,
}));

const appleMocks = vi.hoisted(() => ({
  runAppleSignIn: vi.fn(async () => ({
    kind: 'tokens' as const,
    accessToken: 'at-1',
    refreshToken: 'rt-1',
    sessionId: 'sid-1',
  })),
  cancelActiveAppleFlow: vi.fn(),
}));

vi.mock('../../../../src/main/oauth/apple/appleFlow', () => ({
  runAppleSignIn: appleMocks.runAppleSignIn,
  cancelActiveAppleFlow: appleMocks.cancelActiveAppleFlow,
}));

// idTokenVerifier pulls in jose (ESM/WebCrypto) — stub it so this jsdom
// suite never loads it; the real module has its own node-env suite.
vi.mock('../../../../src/main/oauth/apple/idTokenVerifier', () => ({
  verifyAppleIDToken: vi.fn(),
}));

const googleMocks = vi.hoisted(() => ({
  runGoogleSignIn: vi.fn(async () => ({
    kind: 'tokens' as const,
    accessToken: 'g-at-1',
    refreshToken: 'g-rt-1',
    sessionId: 'g-sid-1',
  })),
  cancelActiveGoogleFlow: vi.fn(),
}));

vi.mock('../../../../src/main/oauth/google/googleFlow', () => ({
  runGoogleSignIn: googleMocks.runGoogleSignIn,
  cancelActiveGoogleFlow: googleMocks.cancelActiveGoogleFlow,
}));

// Google idTokenVerifier also pulls in jose — stub for jsdom.
vi.mock('../../../../src/main/oauth/google/idTokenVerifier', () => ({
  verifyGoogleIDToken: vi.fn(),
}));

// loadGoogleClientSecret reads a bundled resource — stub so the test never
// touches the filesystem. Returns a test placeholder.
vi.mock('../../../../src/main/oauth/google/clientSecret', () => ({
  loadGoogleClientSecret: vi.fn(() => 'test-google-client-secret'),
}));

vi.mock('../../../../src/main/apiBaseUrl', () => ({
  PRODUCTION_API_BASE: 'https://api.concordvoice.chat',
  getApiBaseUrl: vi.fn(() => 'http://localhost:8080'),
}));

const profileMocks = vi.hoisted(() => ({
  isValidatedSelfHostedApiBase: vi.fn((apiBase: string) => apiBase === 'https://voice.example'),
}));

vi.mock('../../../../src/main/selfHostedProfile', () => ({
  isValidatedSelfHostedApiBase: profileMocks.isValidatedSelfHostedApiBase,
  rememberValidatedSelfHostedApiBase: vi.fn(),
}));

const tokenMocks = vi.hoisted(() => {
  let currentOwner = 40;
  let reservedOwner: number | null = null;
  let hasToken = false;

  const reserveCredentialOwner = vi.fn(() => {
    currentOwner += 1;
    reservedOwner = currentOwner;
    hasToken = false;
    return currentOwner;
  });
  const credentialOwnerIsCurrent = vi.fn((owner: number) => owner === currentOwner);
  const storeRefreshTokenIfOwner = vi.fn((_data: unknown, owner: number) => {
    if (owner !== currentOwner || reservedOwner !== owner || hasToken) return null;
    hasToken = true;
    reservedOwner = null;
    return owner;
  });
  const clearTokensIfOwner = vi.fn((owner: number) => {
    if (owner !== currentOwner) return false;
    currentOwner += 1;
    reservedOwner = null;
    hasToken = false;
    return true;
  });

  return {
    reserveCredentialOwner,
    credentialOwnerIsCurrent,
    storeRefreshTokenIfOwner,
    clearTokensIfOwner,
    reset: () => {
      currentOwner = 40;
      reservedOwner = null;
      hasToken = false;
    },
    supersedeCredential: () => {
      currentOwner += 1;
      reservedOwner = null;
      hasToken = true;
      return currentOwner;
    },
  };
});

vi.mock('../../../../src/main/tokenManager', () => ({
  reserveCredentialOwner: tokenMocks.reserveCredentialOwner,
  credentialOwnerIsCurrent: tokenMocks.credentialOwnerIsCurrent,
  storeRefreshTokenIfOwner: tokenMocks.storeRefreshTokenIfOwner,
  clearTokensIfOwner: tokenMocks.clearTokensIfOwner,
}));

import { registerSSOIPC } from '@/main/ipc/sso';

interface FakeInvokeEvent {
  senderFrame: { url: string };
}

const TRUSTED = 'http://localhost:3001';
const UNTRUSTED = 'https://attacker.example';
const SAAS_API_BASE = 'https://api.concordvoice.chat';
const DEV_API_BASE = 'http://localhost:8080';
const SELF_HOSTED_API_BASE = 'https://voice.example';
const SESSION_ID = '11111111-1111-4111-8111-111111111111';
const SUCCESS_SESSION_ID = '22222222-2222-4222-8222-222222222222';

const registrationPayload = {
  provider: 'apple' as const,
  ssoToken: 'sso-token-1',
  username: 'alice',
  passphrase: 'correct horse battery staple', // pragma: allowlist secret
  wrappedPrivateKey: 'wrapped-key', // pragma: allowlist secret
  keyDerivationSalt: 'salt',
  publicKey: 'public-key',
};

const linkPayload = {
  provider: 'apple' as const,
  ssoToken: 'sso-token-1',
  password: 'account-password', // pragma: allowlist secret
};

const getSpaBaseUrl = () => null;

describe('sso IPC handlers', () => {
  let startLoopbackHandler: (event: FakeInvokeEvent) => Promise<{
    port: number;
    redirectURI: string;
  }>;
  let awaitCallbackHandler: (
    event: FakeInvokeEvent,
    port: number
  ) => Promise<{ code: string; state: string }>;
  let cancelLoopbackHandler: (event: FakeInvokeEvent, port: number) => void;
  let appleSignInHandler: (event: FakeInvokeEvent, apiBase: unknown) => Promise<unknown>;
  let appleCancelHandler: (event: FakeInvokeEvent) => void;
  let googleSignInHandler: (event: FakeInvokeEvent, apiBase: unknown) => Promise<unknown>;
  let googleCancelHandler: (event: FakeInvokeEvent) => void;
  let completeRegistrationHandler: (
    event: FakeInvokeEvent,
    apiBase: unknown,
    payload: unknown
  ) => Promise<unknown>;
  let completeLinkHandler: (
    event: FakeInvokeEvent,
    apiBase: unknown,
    payload: unknown
  ) => Promise<unknown>;

  beforeEach(() => {
    vi.clearAllMocks();
    tokenMocks.reset();
    mocked.fakeHandles.length = 0;
    registerSSOIPC(getSpaBaseUrl);

    // Pull each handler out of the mocked ipcMain.handle / on calls.
    const startCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'sso:startLoopback');
    const awaitCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'sso:awaitCallback');
    const cancelCall = mocked.onSpy.mock.calls.find((c) => c[0] === 'sso:cancelLoopback');
    if (!startCall || !awaitCall || !cancelCall) {
      throw new Error('IPC handlers not registered');
    }
    startLoopbackHandler = startCall[1];
    awaitCallbackHandler = awaitCall[1];
    cancelLoopbackHandler = cancelCall[1];

    const appleSignInCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'sso:appleSignIn');
    const appleCancelCall = mocked.onSpy.mock.calls.find((c) => c[0] === 'sso:appleCancel');
    if (!appleSignInCall || !appleCancelCall) {
      throw new Error('apple IPC handlers not registered');
    }
    appleSignInHandler = appleSignInCall[1];
    appleCancelHandler = appleCancelCall[1];

    const googleSignInCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'sso:googleSignIn');
    const googleCancelCall = mocked.onSpy.mock.calls.find((c) => c[0] === 'sso:googleCancel');
    if (!googleSignInCall || !googleCancelCall) {
      throw new Error('google IPC handlers not registered');
    }
    googleSignInHandler = googleSignInCall[1];
    googleCancelHandler = googleCancelCall[1];

    const completeRegistrationCall = mocked.handleSpy.mock.calls.find(
      (c) => c[0] === 'sso:completeRegistration'
    );
    const completeLinkCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'sso:completeLink');
    if (!completeRegistrationCall || !completeLinkCall) {
      throw new Error('SSO completion IPC handlers not registered');
    }
    completeRegistrationHandler = completeRegistrationCall[1];
    completeLinkHandler = completeLinkCall[1];
  });

  describe('sender-frame validation', () => {
    it('sso:startLoopback rejects untrusted sender frames', async () => {
      await expect(startLoopbackHandler({ senderFrame: { url: UNTRUSTED } })).rejects.toThrow(
        /untrusted/i
      );
      // Loopback must not even be started — the active map is empty.
      expect(mocked.fakeHandles.length).toBe(0);
    });

    it('sso:awaitCallback rejects untrusted sender frames', async () => {
      // Need a real entry in `active` first so we can prove the rejection
      // happens before the lookup; start a trusted loopback then attack
      // awaitCallback from a different frame URL.
      const { port } = await startLoopbackHandler({ senderFrame: { url: TRUSTED } });
      await expect(awaitCallbackHandler({ senderFrame: { url: UNTRUSTED } }, port)).rejects.toThrow(
        /untrusted/i
      );
    });

    it('sso:cancelLoopback silently no-ops for untrusted sender frames', async () => {
      const { port } = await startLoopbackHandler({ senderFrame: { url: TRUSTED } });
      // Capture pre-call state.
      const before = mocked.fakeHandles[0].__closeCalls;
      cancelLoopbackHandler({ senderFrame: { url: UNTRUSTED } }, port);
      // Untrusted cancel must not call close() on the active handle.
      expect(mocked.fakeHandles[0].__closeCalls).toBe(before);
    });
  });

  describe('active-map lifecycle', () => {
    it('sso:awaitCallback returns the loopback result and removes the active entry', async () => {
      const { port } = await startLoopbackHandler({ senderFrame: { url: TRUSTED } });
      const handle = mocked.fakeHandles[0];

      // Simulate the OAuth provider redirect resolving the loopback promise.
      handle.__resolve({ code: 'auth-code', state: 'state-x' });

      const result = await awaitCallbackHandler({ senderFrame: { url: TRUSTED } }, port);
      expect(result).toEqual({ code: 'auth-code', state: 'state-x' });

      // After awaitCallback returns, a second await on the same port should
      // fail with "unknown port" — the active map entry is gone. (Re-issuing
      // a fresh attempt would create a new entry under a new ephemeral port.)
      await expect(awaitCallbackHandler({ senderFrame: { url: TRUSTED } }, port)).rejects.toThrow(
        /unknown port/i
      );
    });

    it('sso:cancelLoopback closes the handle and removes the active entry', async () => {
      const { port } = await startLoopbackHandler({ senderFrame: { url: TRUSTED } });
      cancelLoopbackHandler({ senderFrame: { url: TRUSTED } }, port);
      expect(mocked.fakeHandles[0].__closeCalls).toBe(1);
      // Subsequent await on the cancelled port → unknown.
      await expect(awaitCallbackHandler({ senderFrame: { url: TRUSTED } }, port)).rejects.toThrow(
        /unknown port/i
      );
    });

    it('cancelLoopback for an unknown port is a silent no-op', () => {
      // No-op behavior matters: a renderer that races cancel after the
      // promise has auto-cleaned up must not crash the main process.
      expect(() => cancelLoopbackHandler({ senderFrame: { url: TRUSTED } }, 99999)).not.toThrow();
    });

    it('auto-cleans the active entry when the loopback promise settles outside awaitCallback', async () => {
      const { port } = await startLoopbackHandler({ senderFrame: { url: TRUSTED } });
      const handle = mocked.fakeHandles[0];

      // Reject the loopback promise (e.g. timeout) before awaitCallback is invoked.
      handle.__reject(new Error('oauth_timeout'));
      // Allow the .finally on the handler's auto-cleanup to flush.
      await new Promise((r) => setImmediate(r));

      // The active map entry must have been removed.
      await expect(awaitCallbackHandler({ senderFrame: { url: TRUSTED } }, port)).rejects.toThrow(
        /unknown port/i
      );
    });
  });

  describe('unknown-port rejection', () => {
    it('sso:awaitCallback throws for a port that was never started', async () => {
      await expect(awaitCallbackHandler({ senderFrame: { url: TRUSTED } }, 12345)).rejects.toThrow(
        /unknown port/i
      );
    });
  });

  describe('apple sign-in channels (#974)', () => {
    it('sso:appleSignIn rejects untrusted sender frames without starting a flow', async () => {
      await expect(
        appleSignInHandler({ senderFrame: { url: UNTRUSTED } }, SAAS_API_BASE)
      ).rejects.toThrow(/untrusted/i);
      expect(appleMocks.runAppleSignIn).not.toHaveBeenCalled();
    });

    it('stores the refresh credential before returning a sanitized SaaS result', async () => {
      const result = await appleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
      expect(tokenMocks.reserveCredentialOwner).toHaveBeenCalledWith(SAAS_API_BASE);
      expect(tokenMocks.storeRefreshTokenIfOwner).toHaveBeenCalledWith(
        {
          refreshToken: 'rt-1',
          rememberMe: true,
          apiBase: SAAS_API_BASE,
          accessToken: 'at-1',
        },
        41
      );
      expect(result).toEqual({
        kind: 'tokens',
        accessToken: 'at-1',
        sessionId: 'sid-1',
        credentialOwner: 41,
      });
      expect(result).not.toHaveProperty('refreshToken');
      expect(appleMocks.runAppleSignIn).toHaveBeenCalledTimes(1);
      const deps = appleMocks.runAppleSignIn.mock.calls[0][0];
      expect(deps.apiBase).toBe(SAAS_API_BASE);
      expect(typeof deps.controlPlaneFetch).toBe('function');
      expect(typeof deps.openExternal).toBe('function');
    });

    it.each([undefined, 3, 'not a URL', `${SAAS_API_BASE}/`, 'https://unapproved.example'])(
      'rejects invalid or unapproved API origin %j',
      async (apiBase) => {
        await expect(
          appleSignInHandler({ senderFrame: { url: TRUSTED } }, apiBase)
        ).rejects.toThrow(/unapproved API origin/i);
        expect(appleMocks.runAppleSignIn).not.toHaveBeenCalled();
      }
    );

    it('dispatches an already probe-approved self-hosted origin exactly', async () => {
      await appleSignInHandler({ senderFrame: { url: TRUSTED } }, SELF_HOSTED_API_BASE);

      expect(appleMocks.runAppleSignIn.mock.calls[0][0].apiBase).toBe(SELF_HOSTED_API_BASE);
      expect(tokenMocks.storeRefreshTokenIfOwner).toHaveBeenCalledWith(
        expect.objectContaining({ apiBase: SELF_HOSTED_API_BASE }),
        41
      );
    });

    it('allows the exact main-configured development origin', async () => {
      await appleSignInHandler({ senderFrame: { url: TRUSTED } }, DEV_API_BASE);

      expect(appleMocks.runAppleSignIn.mock.calls[0][0].apiBase).toBe(DEV_API_BASE);
    });

    it('controlPlaneFetch resolves every input shape to a URL string and pins credentials', async () => {
      // S6551 regression: String(Request) would coerce to '[object Request]'.
      // Each fetch-input shape must reach net.fetch as its URL string, and
      // every call must carry credentials:'include' (the refresh-cookie jar
      // contract, plan deviation D2).
      await appleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
      const deps = appleMocks.runAppleSignIn.mock.calls[0][0];
      const netFetch = vi.mocked(net.fetch);

      await deps.controlPlaneFetch('https://api.test/a');
      await deps.controlPlaneFetch(new URL('https://api.test/b'));
      await deps.controlPlaneFetch({ url: 'https://api.test/c' }); // Request-shaped

      expect(netFetch.mock.calls.map((c) => c[0])).toEqual([
        'https://api.test/a',
        'https://api.test/b',
        'https://api.test/c',
      ]);
      for (const call of netFetch.mock.calls) {
        expect((call[1] as RequestInit).credentials).toBe('include');
      }
    });

    it('revokes a malformed successful /session response by captured cookie-bound session ID', async () => {
      vi.mocked(net.fetch)
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ unexpected: true }), {
            status: 200,
            headers: {
              'Content-Type': 'application/json',
              'X-Concord-Session-ID': SESSION_ID,
            },
          })
        )
        .mockResolvedValueOnce(new Response(null, { status: 204 }));
      appleMocks.runAppleSignIn.mockImplementationOnce(async (deps) => {
        await deps.controlPlaneFetch(`${SAAS_API_BASE}/api/v1/auth/sso/apple/session`, {
          method: 'POST',
        });
        return { kind: 'error', code: 'sso_session_rejected' } as never;
      });

      await expect(
        appleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE)
      ).resolves.toEqual({ kind: 'error', code: 'sso_session_rejected' });

      expect(net.fetch).toHaveBeenLastCalledWith(`${SAAS_API_BASE}/api/v1/auth/logout`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'X-Session-ID': SESSION_ID },
      });
      expect(tokenMocks.storeRefreshTokenIfOwner).not.toHaveBeenCalled();
      expect(tokenMocks.clearTokensIfOwner).toHaveBeenCalledWith(41);
    });

    it('does not let a delayed SSO token overwrite a password credential and keys', async () => {
      let finishApple!: (result: {
        kind: 'tokens';
        accessToken: string;
        refreshToken: string;
        sessionId: string;
      }) => void;
      appleMocks.runAppleSignIn.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            finishApple = resolve;
          })
      );

      const delayedSSO = appleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
      await vi.waitFor(() => expect(appleMocks.runAppleSignIn).toHaveBeenCalledTimes(1));
      tokenMocks.supersedeCredential();
      finishApple({
        kind: 'tokens',
        accessToken: 'stale-access',
        refreshToken: 'stale-refresh',
        sessionId: 'stale-session',
      });

      await expect(delayedSSO).resolves.toEqual({ kind: 'error', code: 'sso_cancelled' });
      expect(tokenMocks.storeRefreshTokenIfOwner).not.toHaveBeenCalled();
      expect(net.fetch).toHaveBeenCalledWith(`${SAAS_API_BASE}/api/v1/auth/logout`, {
        method: 'POST',
        credentials: 'omit',
        headers: {
          Authorization: 'Bearer stale-access',
          'X-Refresh-Token': 'stale-refresh',
        },
      });
    });

    it('sso:appleCancel tears down the active flow for trusted frames', () => {
      appleCancelHandler({ senderFrame: { url: TRUSTED } });
      expect(appleMocks.cancelActiveAppleFlow).toHaveBeenCalledTimes(1);
    });

    it('sso:appleCancel silently no-ops for untrusted frames', () => {
      appleCancelHandler({ senderFrame: { url: UNTRUSTED } });
      expect(appleMocks.cancelActiveAppleFlow).not.toHaveBeenCalled();
    });
  });

  describe('SSO completion channels', () => {
    async function beginAppleCompletion(branch: 'new_user' | 'account_link' = 'new_user') {
      appleMocks.runAppleSignIn.mockImplementationOnce(
        async () =>
          ({
            kind: 'sso_token',
            branch,
            ssoToken: 'sso-token-1',
            ...(branch === 'new_user'
              ? { email: 'alice@example.com', name: 'Alice' }
              : { maskedEmail: 'a***@example.com' }),
          }) as never
      );
      return appleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
    }

    it('completes registration in main, stores the owner-bound refresh credential, and sanitizes IPC', async () => {
      await expect(beginAppleCompletion()).resolves.toMatchObject({
        kind: 'sso_token',
        branch: 'new_user',
      });
      vi.mocked(net.fetch).mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            access_token: 'completion-access',
            refresh_token: 'completion-refresh',
            session_id: SUCCESS_SESSION_ID,
            remember_me: true,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );

      const result = await completeRegistrationHandler(
        { senderFrame: { url: TRUSTED } },
        SAAS_API_BASE,
        registrationPayload
      );

      expect(result).toEqual({
        kind: 'tokens',
        accessToken: 'completion-access',
        sessionId: SUCCESS_SESSION_ID,
        credentialOwner: 41,
      });
      expect(result).not.toHaveProperty('refreshToken');
      expect(tokenMocks.storeRefreshTokenIfOwner).toHaveBeenCalledWith(
        {
          accessToken: 'completion-access',
          refreshToken: 'completion-refresh',
          rememberMe: true,
          apiBase: SAAS_API_BASE,
        },
        41
      );
      expect(net.fetch).toHaveBeenCalledWith(
        `${SAAS_API_BASE}/api/v1/auth/sso/apple/complete-registration`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({
            sso_token: registrationPayload.ssoToken,
            username: registrationPayload.username,
            password: registrationPayload.passphrase,
            wrapped_private_key: registrationPayload.wrappedPrivateKey,
            key_derivation_salt: registrationPayload.keyDerivationSalt,
            public_key: registrationPayload.publicKey,
          }),
        }
      );
    });

    it('completes account linking only for the matching pending branch', async () => {
      await beginAppleCompletion('account_link');
      vi.mocked(net.fetch).mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            access_token: 'link-access',
            refresh_token: 'link-refresh',
            session_id: SUCCESS_SESSION_ID,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );

      await expect(
        completeRegistrationHandler(
          { senderFrame: { url: TRUSTED } },
          SAAS_API_BASE,
          registrationPayload
        )
      ).resolves.toEqual({ kind: 'error', status: 409, code: 'sso_cancelled' });
      await expect(
        completeLinkHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE, linkPayload)
      ).resolves.toEqual({
        kind: 'tokens',
        accessToken: 'link-access',
        sessionId: SUCCESS_SESSION_ID,
        credentialOwner: 41,
      });
    });

    it('returns only allowlisted server error fields and never leaks refresh material', async () => {
      await beginAppleCompletion();
      vi.mocked(net.fetch).mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error_code: 'password_invalid',
            detail: 'Try again',
            attempts_remaining: 2,
            refresh_token: 'must-not-cross-ipc',
            access_token: 'must-not-cross-ipc',
            arbitrary: { secret: true },
          }),
          { status: 401, headers: { 'Content-Type': 'application/json' } }
        )
      );

      const result = await completeRegistrationHandler(
        { senderFrame: { url: TRUSTED } },
        SAAS_API_BASE,
        registrationPayload
      );
      expect(result).toEqual({
        kind: 'error',
        status: 401,
        code: 'password_invalid',
        body: {
          error_code: 'password_invalid',
          detail: 'Try again',
          attempts_remaining: 2,
        },
      });
      expect(JSON.stringify(result)).not.toContain('must-not-cross-ipc');
    });

    it('revokes a cookie-bound session when completion returns malformed 2xx JSON', async () => {
      await beginAppleCompletion();
      vi.mocked(net.fetch)
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ access_token: 'partial' }), {
            status: 200,
            headers: {
              'Content-Type': 'application/json',
              'X-Concord-Session-ID': SESSION_ID,
            },
          })
        )
        .mockResolvedValueOnce(new Response(null, { status: 204 }));

      await expect(
        completeRegistrationHandler(
          { senderFrame: { url: TRUSTED } },
          SAAS_API_BASE,
          registrationPayload
        )
      ).resolves.toEqual({ kind: 'error', status: 502, code: 'sso_session_rejected' });
      expect(net.fetch).toHaveBeenLastCalledWith(`${SAAS_API_BASE}/api/v1/auth/logout`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'X-Session-ID': SESSION_ID },
      });
      expect(tokenMocks.storeRefreshTokenIfOwner).not.toHaveBeenCalled();
      expect(tokenMocks.clearTokensIfOwner).toHaveBeenCalledWith(41);
    });

    it('revokes a delayed completion that loses the credential-owner race', async () => {
      await beginAppleCompletion();
      let resolveCompletion!: (response: Response) => void;
      vi.mocked(net.fetch)
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              resolveCompletion = resolve;
            })
        )
        .mockResolvedValueOnce(new Response(null, { status: 204 }));

      const delayedCompletion = completeRegistrationHandler(
        { senderFrame: { url: TRUSTED } },
        SAAS_API_BASE,
        registrationPayload
      );
      await vi.waitFor(() => expect(net.fetch).toHaveBeenCalledTimes(1));
      tokenMocks.supersedeCredential();
      resolveCompletion(
        new Response(
          JSON.stringify({
            access_token: 'stale-access',
            refresh_token: 'stale-refresh',
            session_id: SUCCESS_SESSION_ID,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );

      await expect(delayedCompletion).resolves.toEqual({
        kind: 'error',
        status: 409,
        code: 'sso_cancelled',
      });
      expect(tokenMocks.storeRefreshTokenIfOwner).toHaveReturnedWith(null);
      expect(net.fetch).toHaveBeenLastCalledWith(`${SAAS_API_BASE}/api/v1/auth/logout`, {
        method: 'POST',
        credentials: 'omit',
        headers: {
          Authorization: 'Bearer stale-access',
          'X-Refresh-Token': 'stale-refresh',
        },
      });
    });

    it('rejects untrusted completion senders before network or credential access', async () => {
      await expect(
        completeRegistrationHandler(
          { senderFrame: { url: UNTRUSTED } },
          SAAS_API_BASE,
          registrationPayload
        )
      ).rejects.toThrow(/untrusted/i);
      expect(net.fetch).not.toHaveBeenCalled();
      expect(tokenMocks.storeRefreshTokenIfOwner).not.toHaveBeenCalled();
    });
  });

  describe('google sign-in channels (#975)', () => {
    it('sso:googleSignIn rejects untrusted sender frames without starting a flow', async () => {
      await expect(
        googleSignInHandler({ senderFrame: { url: UNTRUSTED } }, SAAS_API_BASE)
      ).rejects.toThrow(/untrusted/i);
      expect(googleMocks.runGoogleSignIn).not.toHaveBeenCalled();
    });

    it('sso:googleSignIn dispatches runGoogleSignIn with clientSecret for trusted frames', async () => {
      const result = await googleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
      expect(result).toEqual({
        kind: 'tokens',
        accessToken: 'g-at-1',
        sessionId: 'g-sid-1',
        credentialOwner: 41,
      });
      expect(googleMocks.runGoogleSignIn).toHaveBeenCalledTimes(1);
      const deps = googleMocks.runGoogleSignIn.mock.calls[0][0];
      // clientSecret is the non-confidential embedded secret — must be present.
      expect(deps.clientSecret).toBe('test-google-client-secret');
      // client_id is NOT passed — googleFlow parses it from the server-built
      // authorize URL (sourced from the control-plane's GOOGLE_CLIENT_ID config).
      expect(deps).not.toHaveProperty('clientId');
      expect(typeof deps.controlPlaneFetch).toBe('function');
    });

    it('does not let an older provider completion overwrite a newer credential', async () => {
      let finishApple!: (result: {
        kind: 'tokens';
        accessToken: string;
        refreshToken: string;
        sessionId: string;
      }) => void;
      appleMocks.runAppleSignIn.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            finishApple = resolve;
          })
      );

      const older = appleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
      await vi.waitFor(() => expect(appleMocks.runAppleSignIn).toHaveBeenCalled());
      await googleSignInHandler({ senderFrame: { url: TRUSTED } }, SAAS_API_BASE);
      finishApple({
        kind: 'tokens',
        accessToken: 'old-at',
        refreshToken: 'old-rt',
        sessionId: 'old-sid',
      });

      await expect(older).resolves.toEqual({ kind: 'error', code: 'sso_cancelled' });
      expect(tokenMocks.storeRefreshTokenIfOwner).toHaveBeenCalledTimes(1);
      expect(tokenMocks.storeRefreshTokenIfOwner).toHaveBeenCalledWith(
        expect.objectContaining({ refreshToken: 'g-rt-1' }),
        42
      );
      expect(net.fetch).toHaveBeenCalledWith(`${SAAS_API_BASE}/api/v1/auth/logout`, {
        method: 'POST',
        credentials: 'omit',
        headers: {
          Authorization: 'Bearer old-at',
          'X-Refresh-Token': 'old-rt',
        },
      });
    });

    it('sso:googleCancel tears down the active flow for trusted frames', () => {
      googleCancelHandler({ senderFrame: { url: TRUSTED } });
      expect(googleMocks.cancelActiveGoogleFlow).toHaveBeenCalledTimes(1);
    });

    it('sso:googleCancel silently no-ops for untrusted frames', () => {
      googleCancelHandler({ senderFrame: { url: UNTRUSTED } });
      expect(googleMocks.cancelActiveGoogleFlow).not.toHaveBeenCalled();
    });
  });
});
