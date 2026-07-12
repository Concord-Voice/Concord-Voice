import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useSSOFlow } from '@/renderer/hooks/useSSOFlow';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { useMFAChallengeStore } from '@/renderer/stores/mfaChallengeStore';
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

// SSO must defer the shared hydration helper until App's E2EE unlock boundary.
vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: vi.fn().mockResolvedValue(undefined),
}));

import { startSSOFlow } from '@/renderer/services/ssoService';
import { hydratePostLogin } from '@/renderer/services/postLoginHydration';
const mockedStartSSOFlow = startSSOFlow as unknown as ReturnType<typeof vi.fn>;
const mockedHydratePostLogin = vi.mocked(hydratePostLogin);

beforeEach(() => {
  // resetAllStores covers all known stores per the [internal]rules/tests.md
  // convention; we still call mockReset() for the spy on startSSOFlow.
  resetAllStores();
  mockedStartSSOFlow.mockReset();
  mockedHydratePostLogin.mockClear();
  mockedHydratePostLogin.mockResolvedValue(undefined);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useSSOFlow', () => {
  it('logged_in: sets access token and returns store to idle', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'jwt-token-abc',
    });

    const { result } = renderHook(() => useSSOFlow());
    await act(async () => {
      await result.current.begin('google');
    });

    expect(useAuthStore.getState().accessToken).toBe('jwt-token-abc');
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    expect(mockedStartSSOFlow).toHaveBeenCalledWith('google');
  });

  it('logged_in: defers encrypted post-login hydration until SSO unlock', async () => {
    mockedStartSSOFlow.mockResolvedValueOnce({
      kind: 'logged_in',
      accessToken: 'jwt-token-abc',
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
          session_id: 'sess-after-sso-mfa',
        },
      });
      // Allow the .then handler to fire
      await Promise.resolve();
    });

    expect(useAuthStore.getState().accessToken).toBe('jwt-after-sso-mfa');
    expect(useAuthStore.getState().sessionId).toBe('sess-after-sso-mfa');
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(true);
    // E2EE is still locked; App's unlock boundary owns encrypted hydration.
    expect(mockedHydratePostLogin).not.toHaveBeenCalled();
  });

  it('mfa_required: post-verify with payload missing session_id leaves sessionId unchanged', async () => {
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
        payload: { access_token: 'tok-no-sess' },
      });
      await Promise.resolve();
    });

    // access_token hydrates as expected.
    expect(useAuthStore.getState().accessToken).toBe('tok-no-sess');
    // session_id was absent — sessionId in the auth store remains null.
    expect(useAuthStore.getState().sessionId).toBeNull();
    expect(useSSOStore.getState().state).toEqual({ phase: 'idle' });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(true);
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
