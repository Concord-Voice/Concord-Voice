import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useSSOFlow } from '@/renderer/hooks/useSSOFlow';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { useMFAChallengeStore } from '@/renderer/stores/mfaChallengeStore';
import {
  getApiBase,
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock the service so we drive the hook through every SSOResult shape
// without exercising the network or window.electron loopback.
vi.mock('@/renderer/services/ssoService', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/ssoService')>();
  return {
    ...actual,
    startSSOFlow: vi.fn(),
  };
});

vi.mock('@/renderer/services/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/apiClient')>();
  return {
    ...actual,
    revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
  };
});

// SSO must defer the shared hydration helper until App's E2EE unlock boundary.
vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: vi.fn().mockResolvedValue(undefined),
}));

import { startSSOFlow, type SSOResult } from '@/renderer/services/ssoService';
import { revokeAbortedSession } from '@/renderer/services/apiClient';
import { hydratePostLogin } from '@/renderer/services/postLoginHydration';
const mockedStartSSOFlow = startSSOFlow as unknown as ReturnType<typeof vi.fn>;
const mockedRevokeAbortedSession = vi.mocked(revokeAbortedSession);
const mockedHydratePostLogin = vi.mocked(hydratePostLogin);

let originalElectron: typeof globalThis.electron;

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let finish: ((value: T) => void) | null = null;
  const promise = new Promise<T>((resolve) => {
    finish = resolve;
  });
  return {
    promise,
    resolve: (value) => {
      if (finish === null) throw new Error('deferred resolver was not initialized');
      finish(value);
    },
  };
}

beforeEach(() => {
  // resetAllStores covers all known stores per the [internal]rules/tests.md
  // convention; we still call mockReset() for the spy on startSSOFlow.
  resetAllStores();
  resetRuntimeServerBase();
  originalElectron = globalThis.electron;
  Object.defineProperty(globalThis, 'electron', {
    value: {
      ...originalElectron,
      clearTokens: vi.fn().mockResolvedValue(undefined),
      clearTokensIfOwner: vi.fn().mockResolvedValue(true),
      storeRefreshToken: vi.fn().mockResolvedValue(71),
    },
    writable: true,
  });
  mockedStartSSOFlow.mockReset();
  mockedRevokeAbortedSession.mockClear();
  mockedRevokeAbortedSession.mockResolvedValue(undefined);
  mockedHydratePostLogin.mockClear();
  mockedHydratePostLogin.mockResolvedValue(undefined);
});

afterEach(() => {
  resetRuntimeServerBase();
  Object.defineProperty(globalThis, 'electron', {
    value: originalElectron,
    writable: true,
  });
  vi.restoreAllMocks();
});

describe('useSSOFlow', () => {
  it('reserves auth and clears password credentials before starting SSO', async () => {
    useAuthStore.getState().beginAuthLifecycle('password-token', 'password-session');
    const passwordGeneration = useAuthStore.getState().authGeneration;
    let finishClear: () => void = () => {
      throw new Error('clearTokens resolver was not initialized');
    };
    const clearTokens = vi.fn().mockReturnValue(
      new Promise<void>((resolve) => {
        finishClear = resolve;
      })
    );
    const originalElectron = globalThis.electron;
    Object.defineProperty(globalThis, 'electron', {
      value: { ...originalElectron, clearTokens },
      writable: true,
    });
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'sso-token',
      sessionId: 'sso-session',
      credentialOwner: 11,
    });

    try {
      const { result } = renderHook(() => useSSOFlow());
      let pending: Promise<void> | undefined;
      act(() => {
        pending = result.current.begin('google');
      });

      expect(useAuthStore.getState()).toMatchObject({
        accessToken: null,
        sessionId: null,
        authGeneration: passwordGeneration + 1,
      });
      expect(clearTokens).toHaveBeenCalledOnce();
      expect(mockedStartSSOFlow).not.toHaveBeenCalled();

      finishClear();
      await act(async () => {
        await pending;
      });

      expect(mockedStartSSOFlow).toHaveBeenCalledWith('google', getApiBase());
      expect(useAuthStore.getState()).toMatchObject({
        accessToken: 'sso-token',
        sessionId: 'sso-session',
      });
    } finally {
      Object.defineProperty(globalThis, 'electron', {
        value: originalElectron,
        writable: true,
      });
    }
  });

  it('admits only the newest of two out-of-order direct SSO completions', async () => {
    const first = deferred<SSOResult>();
    const second = deferred<SSOResult>();
    mockedStartSSOFlow
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);
    const clearTokensIfOwner = vi.mocked(globalThis.electron.clearTokensIfOwner);

    const { result } = renderHook(() => useSSOFlow());
    let firstPending = Promise.resolve();
    act(() => {
      firstPending = result.current.begin('google');
    });
    await vi.waitFor(() => expect(mockedStartSSOFlow).toHaveBeenCalledTimes(1));

    let secondPending = Promise.resolve();
    act(() => {
      secondPending = result.current.begin('apple');
    });
    await vi.waitFor(() => expect(mockedStartSSOFlow).toHaveBeenCalledTimes(2));

    second.resolve({
      kind: 'logged_in',
      accessToken: 'new-access-token',
      sessionId: 'new-session',
      credentialOwner: 22,
    });
    await act(async () => {
      await secondPending;
    });

    first.resolve({
      kind: 'logged_in',
      accessToken: 'stale-access-token',
      sessionId: 'stale-session',
      credentialOwner: 21,
    });
    await act(async () => {
      await firstPending;
    });

    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'new-access-token',
      sessionId: 'new-session',
    });
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    expect(clearTokensIfOwner).toHaveBeenCalledOnce();
    expect(clearTokensIfOwner).toHaveBeenCalledWith(21);
    expect(mockedRevokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'stale-access-token',
      sessionId: 'stale-session',
      apiBase: getApiBase(),
    });
  });

  it('does not let a stale register result replace the newer link phase', async () => {
    const first = deferred<SSOResult>();
    const second = deferred<SSOResult>();
    mockedStartSSOFlow
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() => useSSOFlow());
    let firstPending = Promise.resolve();
    act(() => {
      firstPending = result.current.begin('google');
    });
    await vi.waitFor(() => expect(mockedStartSSOFlow).toHaveBeenCalledTimes(1));

    let secondPending = Promise.resolve();
    act(() => {
      secondPending = result.current.begin('apple');
    });
    await vi.waitFor(() => expect(mockedStartSSOFlow).toHaveBeenCalledTimes(2));

    second.resolve({
      kind: 'link_available',
      ssoToken: 'new-link-token',
      maskedEmail: 'n***@example.test',
    });
    await act(async () => {
      await secondPending;
    });

    first.resolve({
      kind: 'register_required',
      ssoToken: 'stale-register-token',
      email: 'stale@example.test',
    });
    await act(async () => {
      await firstPending;
    });

    expect(useSSOStore.getState().state).toEqual({
      phase: 'link_required',
      provider: 'apple',
      ssoToken: 'new-link-token',
      maskedEmail: 'n***@example.test',
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(mockedRevokeAbortedSession).not.toHaveBeenCalled();
  });

  it('rejects a direct SSO result after an A-to-B-to-A server selection change', async () => {
    const requestApiBase = 'https://origin-a.example';
    setRuntimeServerBase(requestApiBase);
    const pendingResult = deferred<SSOResult>();
    mockedStartSSOFlow.mockImplementationOnce(() => pendingResult.promise);
    const clearTokensIfOwner = vi.mocked(globalThis.electron.clearTokensIfOwner);

    const { result } = renderHook(() => useSSOFlow());
    let pending = Promise.resolve();
    act(() => {
      pending = result.current.begin('google');
    });
    await vi.waitFor(() =>
      expect(mockedStartSSOFlow).toHaveBeenCalledWith('google', requestApiBase)
    );

    setRuntimeServerBase('https://origin-b.example');
    setRuntimeServerBase(requestApiBase);
    pendingResult.resolve({
      kind: 'logged_in',
      accessToken: 'origin-a-access',
      sessionId: 'origin-a-session',
      credentialOwner: 31,
    });
    await act(async () => {
      await pending;
    });

    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    expect(globalThis.electron.storeRefreshToken).not.toHaveBeenCalled();
    expect(useSSOStore.getState().state).toEqual({
      phase: 'authenticating',
      provider: 'google',
    });
    expect(clearTokensIfOwner).toHaveBeenCalledWith(31);
    expect(mockedRevokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'origin-a-access',
      sessionId: 'origin-a-session',
      apiBase: requestApiBase,
    });
  });

  it('logged_in: sets access token and returns store to idle', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'jwt-token-abc',
      sessionId: 'session-abc',
      credentialOwner: 12,
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    expect(useAuthStore.getState().accessToken).toBe('jwt-token-abc');
    expect(useAuthStore.getState().sessionId).toBe('session-abc');
    expect(useE2EEStore.getState().ssoCredentialOwner).toBe(12);
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    expect(mockedStartSSOFlow).toHaveBeenCalledWith('google', getApiBase());
  });

  it('logged_in: defers encrypted post-login hydration until SSO unlock', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'jwt-token-abc',
      sessionId: 'session-abc',
      credentialOwner: 13,
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    expect(mockedHydratePostLogin).not.toHaveBeenCalled();
  });

  it('logged_in: arms the SSO eager-unlock gate (#270 Task 21b)', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'jwt-token-abc',
      sessionId: 'session-abc',
      credentialOwner: 14,
    });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    // The SSO callback completed but no E2EE keys are unwrapped on this
    // device — the gate flag tells AuthenticatedLayout to mount
    // SSOEagerUnlock until e2eeService.initialize flips ready=true.
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(true);
    expect(useE2EEStore.getState().ready).toBe(false);
  });

  it('register_required: dispatches register_required phase with ssoToken and email', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'register_required',
      ssoToken: 'sso-tok-1',
      email: 'new@example.test',
      name: 'New User',
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    const state = useSSOStore.getState().state;
    expect(state.phase).toBe('register_required');
    if (state.phase === 'register_required') {
      expect(state.provider).toBe('google');
      expect(state.ssoToken).toBe('sso-tok-1');
      expect(state.email).toBe('new@example.test');
      expect(state.name).toBe('New User');
    }
    // No access token should be set yet — registration is incomplete.
    expect(useAuthStore.getState().accessToken).toBeNull();
  });

  it('link_available: dispatches link_required phase with maskedEmail', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'link_available',
      ssoToken: 'sso-tok-2',
      maskedEmail: 'm***@example.test',
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    const state = useSSOStore.getState().state;
    expect(state.phase).toBe('link_required');
    if (state.phase === 'link_required') {
      expect(state.maskedEmail).toBe('m***@example.test');
      expect(state.ssoToken).toBe('sso-tok-2');
    }
  });

  it('mfa_required: dispatches mfa_required phase with challenge token', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'mfa_required',
      mfaChallengeToken: 'mfa-chal-1',
      methods: ['totp'],
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    const state = useSSOStore.getState().state;
    expect(state.phase).toBe('mfa_required');
    if (state.phase === 'mfa_required') {
      expect(state.mfaChallengeToken).toBe('mfa-chal-1');
    }
    expect(useAuthStore.getState().accessToken).toBeNull();
  });

  it('mfa_required: post-verify hydrates useAuthStore from payload and arms SSO unlock gate', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'mfa_required',
      mfaChallengeToken: 'mfa-chal-hydration',
      methods: ['totp'],
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    // SSO is now in mfa_required phase, awaiting modal verification
    expect(useSSOStore.getState().state.phase).toBe('mfa_required');
    expect(useAuthStore.getState().accessToken).toBeNull();

    // Simulate user completing MFA — modal calls completeChallenge with payload
    await act(async () => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        payload: {
          access_token: 'jwt-after-sso-mfa',
          refresh_token: 'refresh-after-sso-mfa',
          session_id: 'sess-after-sso-mfa',
          remember_me: false,
        },
      });
      // Allow the .then handler to fire
      await Promise.resolve();
    });

    expect(useAuthStore.getState().accessToken).toBe('jwt-after-sso-mfa');
    expect(useAuthStore.getState().sessionId).toBe('sess-after-sso-mfa');
    expect(globalThis.electron.storeRefreshToken).toHaveBeenCalledWith({
      refreshToken: 'refresh-after-sso-mfa',
      rememberMe: false,
      apiBase: getApiBase(),
      accessToken: 'jwt-after-sso-mfa',
    });
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(true);
    expect(useE2EEStore.getState().ssoCredentialOwner).toBe(71);
    // E2EE is still locked; App's unlock boundary owns encrypted hydration.
    expect(mockedHydratePostLogin).not.toHaveBeenCalled();
  });

  it('mfa_required: ignores and revokes a successful completion superseded by a new flow', async () => {
    mockedStartSSOFlow
      .mockResolvedValueOnce({
        kind: 'mfa_required',
        mfaChallengeToken: 'stale-mfa-challenge',
        methods: ['totp'],
      })
      .mockResolvedValueOnce({
        kind: 'register_required',
        ssoToken: 'new-registration-token',
        email: 'new@example.test',
      });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    let newestPending = Promise.resolve();
    act(() => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        payload: {
          access_token: 'stale-mfa-access',
          refresh_token: 'stale-mfa-refresh',
          session_id: 'stale-mfa-session',
        },
      });
      newestPending = result.current.begin('apple');
    });
    await act(async () => {
      await newestPending;
    });
    await vi.waitFor(() => expect(mockedRevokeAbortedSession).toHaveBeenCalledOnce());

    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    expect(globalThis.electron.storeRefreshToken).not.toHaveBeenCalled();
    expect(useSSOStore.getState().state).toEqual({
      phase: 'register_required',
      provider: 'apple',
      ssoToken: 'new-registration-token',
      email: 'new@example.test',
      name: undefined,
    });
    expect(mockedRevokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'stale-mfa-access',
      refreshToken: 'stale-mfa-refresh',
      sessionId: 'stale-mfa-session',
      apiBase: getApiBase(),
    });
  });

  it('mfa_required: owner-cleans credentials when superseded during persistence', async () => {
    mockedStartSSOFlow
      .mockResolvedValueOnce({
        kind: 'mfa_required',
        mfaChallengeToken: 'persisting-mfa-challenge',
        methods: ['totp'],
      })
      .mockResolvedValueOnce({
        kind: 'register_required',
        ssoToken: 'successor-registration-token',
        email: 'successor@example.test',
      });
    const storedOwner = deferred<number>();
    const storeRefreshToken = vi.fn().mockReturnValue(storedOwner.promise);
    const clearTokensIfOwner = vi.mocked(globalThis.electron.clearTokensIfOwner);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });
    act(() => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        payload: {
          access_token: 'persisting-access',
          refresh_token: 'persisting-refresh',
          session_id: 'persisting-session',
        },
      });
    });
    await vi.waitFor(() => expect(storeRefreshToken).toHaveBeenCalledOnce());

    await act(async () => {
      await result.current.begin('apple');
    });
    storedOwner.resolve(81);
    await vi.waitFor(() => expect(clearTokensIfOwner).toHaveBeenCalledWith(81));

    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useE2EEStore.getState().ssoCredentialOwner).toBeNull();
    expect(useSSOStore.getState().state).toEqual({
      phase: 'register_required',
      provider: 'apple',
      ssoToken: 'successor-registration-token',
      email: 'successor@example.test',
      name: undefined,
    });
    expect(mockedRevokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'persisting-access',
      refreshToken: 'persisting-refresh',
      sessionId: 'persisting-session',
      apiBase: getApiBase(),
    });
  });

  it('mfa_required: rejects and revokes an incomplete credential tuple', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'mfa_required',
      mfaChallengeToken: 'mfa-chal-no-session',
      methods: ['totp'],
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    await act(async () => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        payload: {
          access_token: 'tok-no-sess',
          refresh_token: 'refresh-no-sess',
        },
      });
      await Promise.resolve();
    });

    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useAuthStore.getState().sessionId).toBeNull();
    expect(useSSOStore.getState().state).toEqual({
      phase: 'error',
      message: 'mfa_verify_missing_token',
    });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    expect(globalThis.electron.storeRefreshToken).not.toHaveBeenCalled();
    expect(mockedRevokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'tok-no-sess',
      refreshToken: 'refresh-no-sess',
      sessionId: null,
      apiBase: getApiBase(),
    });
  });

  it('mfa_required: post-verify with verified=true but missing access_token surfaces an error', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'mfa_required',
      mfaChallengeToken: 'mfa-chal-no-token',
      methods: ['totp'],
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    // Simulate the unexpected case: server returned verified=true but no
    // access_token (e.g., the suspicious_refresh shape). The SSO path
    // expects PurposeLogin to be encoded in the challenge token and a full
    // payload to come back; if it doesn't, surface as an error rather than
    // silently dropping the user at idle.
    await act(async () => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        payload: {},
      });
      await Promise.resolve();
    });

    const state = useSSOStore.getState().state;
    expect(state.phase).toBe('error');
    if (state.phase === 'error') {
      expect(state.message).toBe('mfa_verify_missing_token');
    }
    // Auth store remains uncorrupted.
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
  });

  it('mfa_required: cancellation resets SSO state without hydrating useAuthStore', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'mfa_required',
      mfaChallengeToken: 'mfa-chal-cancel',
      methods: ['totp'],
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    expect(useSSOStore.getState().state.phase).toBe('mfa_required');

    // Simulate user cancelling — modal calls clearChallenge which resolves
    // with { verified: false }
    await act(async () => {
      useMFAChallengeStore.getState().clearChallenge();
      await Promise.resolve();
    });

    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    // needsSSOUnlock should NOT be flipped on cancellation
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
  });

  it('error: sets phase to error with message when service throws', async () => {
    mockedStartSSOFlow.mockRejectedValueOnce(new Error('oauth_state_mismatch'));

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    const state = useSSOStore.getState().state;
    expect(state.phase).toBe('error');
    if (state.phase === 'error') {
      expect(state.message).toBe('oauth_state_mismatch');
    }
  });

  it('error: handles non-Error throws with default message', async () => {
    mockedStartSSOFlow.mockRejectedValueOnce('plain string');

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    const state = useSSOStore.getState().state;
    expect(state.phase).toBe('error');
    if (state.phase === 'error') {
      expect(state.message).toBe('sso_failed');
    }
  });
});
