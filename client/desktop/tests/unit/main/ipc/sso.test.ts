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
  runAppleSignIn: vi.fn(async () => ({ kind: 'tokens', accessToken: 'at-1' })),
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
  runGoogleSignIn: vi.fn(async () => ({ kind: 'tokens', accessToken: 'g-at-1' })),
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
  getApiBaseUrl: vi.fn(() => 'http://localhost:8080'),
}));

import { registerSSOIPC } from '@/main/ipc/sso';

interface FakeInvokeEvent {
  senderFrame: { url: string };
}

const TRUSTED = 'http://localhost:3001';
const UNTRUSTED = 'https://attacker.example';

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
  let appleSignInHandler: (event: FakeInvokeEvent) => Promise<unknown>;
  let appleCancelHandler: (event: FakeInvokeEvent) => void;
  let googleSignInHandler: (event: FakeInvokeEvent) => Promise<unknown>;
  let googleCancelHandler: (event: FakeInvokeEvent) => void;

  beforeEach(() => {
    vi.clearAllMocks();
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
      await expect(appleSignInHandler({ senderFrame: { url: UNTRUSTED } })).rejects.toThrow(
        /untrusted/i
      );
      expect(appleMocks.runAppleSignIn).not.toHaveBeenCalled();
    });

    it('sso:appleSignIn relays the orchestrator result for trusted frames', async () => {
      const result = await appleSignInHandler({ senderFrame: { url: TRUSTED } });
      expect(result).toEqual({ kind: 'tokens', accessToken: 'at-1' });
      expect(appleMocks.runAppleSignIn).toHaveBeenCalledTimes(1);
      const deps = appleMocks.runAppleSignIn.mock.calls[0][0];
      expect(deps.apiBase).toBe('http://localhost:8080');
      expect(typeof deps.controlPlaneFetch).toBe('function');
      expect(typeof deps.openExternal).toBe('function');
    });

    it('controlPlaneFetch resolves every input shape to a URL string and pins credentials', async () => {
      // S6551 regression: String(Request) would coerce to '[object Request]'.
      // Each fetch-input shape must reach net.fetch as its URL string, and
      // every call must carry credentials:'include' (the refresh-cookie jar
      // contract, plan deviation D2).
      await appleSignInHandler({ senderFrame: { url: TRUSTED } });
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

    it('sso:appleCancel tears down the active flow for trusted frames', () => {
      appleCancelHandler({ senderFrame: { url: TRUSTED } });
      expect(appleMocks.cancelActiveAppleFlow).toHaveBeenCalledTimes(1);
    });

    it('sso:appleCancel silently no-ops for untrusted frames', () => {
      appleCancelHandler({ senderFrame: { url: UNTRUSTED } });
      expect(appleMocks.cancelActiveAppleFlow).not.toHaveBeenCalled();
    });
  });

  describe('google sign-in channels (#975)', () => {
    it('sso:googleSignIn rejects untrusted sender frames without starting a flow', async () => {
      await expect(googleSignInHandler({ senderFrame: { url: UNTRUSTED } })).rejects.toThrow(
        /untrusted/i
      );
      expect(googleMocks.runGoogleSignIn).not.toHaveBeenCalled();
    });

    it('sso:googleSignIn dispatches runGoogleSignIn with clientSecret for trusted frames', async () => {
      const result = await googleSignInHandler({ senderFrame: { url: TRUSTED } });
      expect(result).toEqual({ kind: 'tokens', accessToken: 'g-at-1' });
      expect(googleMocks.runGoogleSignIn).toHaveBeenCalledTimes(1);
      const deps = googleMocks.runGoogleSignIn.mock.calls[0][0];
      // clientSecret is the non-confidential embedded secret — must be present.
      expect(deps.clientSecret).toBe('test-google-client-secret');
      // client_id is NOT passed — googleFlow parses it from the server-built
      // authorize URL (sourced from the control-plane's GOOGLE_CLIENT_ID config).
      expect(deps).not.toHaveProperty('clientId');
      expect(typeof deps.controlPlaneFetch).toBe('function');
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
