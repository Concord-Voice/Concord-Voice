import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useSSOFlow } from '@/renderer/hooks/ui/useSSOFlow';
import { useSSOStore } from '@/renderer/stores/auth/ssoStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { useMFAChallengeStore } from '@/renderer/stores/auth/mfaChallengeStore';
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
import { deferred } from '../../../helpers/deferred';
const mockedStartSSOFlow = startSSOFlow as unknown as ReturnType<typeof vi.fn>;
const mockedRevokeAbortedSession = vi.mocked(revokeAbortedSession);
const mockedHydratePostLogin = vi.mocked(hydratePostLogin);

let originalElectron: typeof globalThis.electron;

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

  it('logged_in: rolls back the owner-scoped gate when the auth publish fails (AC-8, #2424)', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'jwt-fail',
      sessionId: 'session-fail',
      credentialOwner: 77,
    });
    // Force the auth publish to fail AFTER the gate is armed (the reservation
    // itself is still current), exercising the owner-scoped rollback path.
    const spy = vi
      .spyOn(useAuthStore.getState(), 'beginAuthLifecycleIfCurrent')
      .mockReturnValueOnce(null);

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    expect(spy).toHaveBeenCalled();
    // The gate was armed then rolled back (owner-scoped) — never left true
    // against a session that was not published.
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    expect(useE2EEStore.getState().ssoCredentialOwner).toBeNull();
    expect(useAuthStore.getState().accessToken).toBeNull();
    spy.mockRestore();
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

  // #2346 parallel — SSO ONBOARDING is structurally immune to the password-login
  // strand. That bug needs an EARLY access token (published before E2EE is ready)
  // to trip App's "/" route into /app/dms and unmount the auth UI; a brand-new
  // SSO user in `register_required` never has one — the passphrase is set FIRST,
  // then a token is issued. This guards that precondition: if a future change
  // admits a token (or arms the inline-unlock gate) during register_required, it
  // re-opens the same strand class and must fail here.
  it('register_required: never admits a keyless new user into the app (#2346 parallel)', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'register_required',
      ssoToken: 'sso-tok-onboarding',
      email: 'newuser@example.test',
      name: 'New User',
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    const auth = useAuthStore.getState();
    // No access token → App's `accessToken && emailVerified && !e2eeUnlockPending`
    // gate stays false → the user stays on AuthFlow (SSOPassphraseSetup), never
    // navigated to /app/dms with unready E2EE.
    expect(auth.accessToken).toBeNull();
    // …and no inline-E2EE-unlock hold is armed for a live session either.
    expect(auth.pendingE2EEUnlockGeneration).toBeNull();
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

  it('mfa_required: post-verify hydrates useAuthStore from the main-verified completion and arms SSO unlock gate', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'mfa_required',
      mfaChallengeToken: 'mfa-chal-hydration',
      methods: ['totp'],
      credentialOwner: 71,
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    // SSO is now in mfa_required phase, awaiting modal verification
    expect(useSSOStore.getState().state.phase).toBe('mfa_required');
    expect(useAuthStore.getState().accessToken).toBeNull();

    // #2424: the SSO modal completes MFA in main (sso:completeMFA) and resolves
    // the sanitized ssoCompletion tuple — no refresh token, no renderer store.
    await act(async () => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        ssoCompletion: {
          accessToken: 'jwt-after-sso-mfa',
          sessionId: 'sess-after-sso-mfa',
          credentialOwner: 71,
        },
      });
      // Allow the .then handler to fire
      await Promise.resolve();
    });

    expect(useAuthStore.getState().accessToken).toBe('jwt-after-sso-mfa');
    expect(useAuthStore.getState().sessionId).toBe('sess-after-sso-mfa');
    // The renderer NEVER stores the SSO MFA refresh token (main owns it).
    expect(globalThis.electron.storeRefreshToken).not.toHaveBeenCalled();
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
        credentialOwner: 55,
      })
      .mockResolvedValueOnce({
        kind: 'register_required',
        ssoToken: 'new-registration-token',
        email: 'new@example.test',
      });
    const clearTokensIfOwner = vi.mocked(globalThis.electron.clearTokensIfOwner);

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    let newestPending = Promise.resolve();
    act(() => {
      useMFAChallengeStore.getState().completeChallenge({
        verified: true,
        ssoCompletion: {
          accessToken: 'stale-mfa-access',
          sessionId: 'stale-mfa-session',
          credentialOwner: 55,
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
    // #2424: the stale completion's credential is cleaned OWNER-SCOPED in main;
    // no refresh token exists in the renderer to revoke.
    expect(clearTokensIfOwner).toHaveBeenCalledWith(55);
    expect(useSSOStore.getState().state).toEqual({
      phase: 'register_required',
      provider: 'apple',
      ssoToken: 'new-registration-token',
      email: 'new@example.test',
      name: undefined,
    });
    expect(mockedRevokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'stale-mfa-access',
      sessionId: 'stale-mfa-session',
      apiBase: getApiBase(),
    });
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
