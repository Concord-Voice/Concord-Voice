import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import SSOEagerUnlock from '@/renderer/components/Auth/SSOEagerUnlock';
import { e2eeService } from '@/renderer/services/e2eeService';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch (matches Register.test.tsx / SSOPassphraseSetup.test.tsx pattern)
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Hoisted mock for e2eeService.initialize — swapped per test (happy vs fail).
const mockE2eeInitialize = vi.fn();
const mockE2eeClearKeys = vi.fn();
const { mockSessionKeys, mockInitializationReceipt } = vi.hoisted(() => {
  const sessionKeys = {
    wrappingKeyBase64: 'bW9jay13cmFwcGluZw==', // pragma: allowlist secret
    preferencesKeyBase64: 'bW9jay1wcmVmcw==', // pragma: allowlist secret
    wrappedPrivateKeyBase64: 'bW9jay13cmFwcGVk', // pragma: allowlist secret
  };
  return {
    mockSessionKeys: sessionKeys,
    mockInitializationReceipt: { sessionKeys, attempt: 1 },
  };
});

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: (...args: unknown[]) => mockE2eeInitialize(...args),
    captureTeardownEpoch: vi.fn().mockReturnValue(0),
    wasTornDownSince: vi.fn().mockReturnValue(false),
    getSessionKeys: vi.fn().mockReturnValue(mockSessionKeys),
    clearKeysIfInitializationCurrent: (...args: unknown[]) => mockE2eeClearKeys(...args),
    clearKeys: (...args: unknown[]) => mockE2eeClearKeys(...args),
  },
}));

// Partial-mock apiClient: keep the real apiFetch/safeJson the component uses for
// /users/me/keys, but stub revokeAbortedSession so we can assert it fires on a
// teardown-abort without a real /auth/logout round-trip (PR #2337).
vi.mock('@/renderer/services/apiClient', async (orig) => ({
  ...(await orig<typeof import('@/renderer/services/apiClient')>()),
  revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
}));

const mockStoreE2EEKeys = vi.fn().mockResolvedValue(true);
const mockStoreE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
const mockClearTokens = vi.fn().mockResolvedValue(undefined);
const mockClearTokensIfOwner = vi.fn().mockResolvedValue(true);

/** Helper: server response payload with the same shape userStore.changePassword reads */
function makeKeysResponse() {
  return {
    e2ee_keys: {
      wrapped_private_key: 'BASE64WRAPPED', // pragma: allowlist secret
      key_derivation_salt: 'BASE64SALT', // pragma: allowlist secret
      key_derivation_alg: 'argon2id',
    },
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(false);
  resetAllStores();
  useAuthStore.getState().beginAuthLifecycle('sso-token-A', 'sso-session-A');
  useE2EEStore.getState().setNeedsSSOUnlock(true, 41);
  mockStoreE2EEKeys.mockResolvedValue(true);
  mockStoreE2EEKeysIfOwner.mockResolvedValue(true);
  mockClearTokens.mockResolvedValue(undefined);
  mockClearTokensIfOwner.mockResolvedValue(true);
  Object.defineProperty(globalThis, 'electron', {
    value: {
      ...globalThis.electron,
      storeE2EEKeys: mockStoreE2EEKeys,
      storeE2EEKeysIfOwner: mockStoreE2EEKeysIfOwner,
      clearTokens: mockClearTokens,
      clearTokensIfOwner: mockClearTokensIfOwner,
    },
    writable: true,
  });
  // The component talks to /users/me/keys via apiFetch and binds every async
  // continuation to the SSO auth lifecycle + opaque main-process owner.
});

describe('SSOEagerUnlock', () => {
  it('renders prompt and unlocks on correct passphrase', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => makeKeysResponse(),
      text: async () => JSON.stringify(makeKeysResponse()),
    });
    // #2423: initialize() resolves THIS invocation's receipt.
    mockE2eeInitialize.mockResolvedValueOnce(mockInitializationReceipt);

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => expect(onUnlock).toHaveBeenCalledTimes(1));
    expect(mockE2eeInitialize).toHaveBeenCalledTimes(1);
    // Verify the passphrase + wrapped material AND the teardown epoch flowed
    // through — the guard and 6th arg (sinceEpoch) are load-bearing lifecycle
    // fences, so
    // assert the full call shape (CodeRabbit, PR #2337).
    const [pw, wrapped, salt, alg, guard, epoch] = mockE2eeInitialize.mock.calls[0];
    expect(pw).toBe('CorrectPassphrase!12');
    expect(wrapped).toBe('BASE64WRAPPED'); // pragma: allowlist secret
    expect(salt).toBe('BASE64SALT'); // pragma: allowlist secret
    expect(alg).toBe('argon2id');
    expect(guard).toEqual({
      signal: expect.any(AbortSignal),
      isCurrent: expect.any(Function),
    });
    expect(guard.isCurrent()).toBe(true);
    expect(epoch).toBe(0); // captureTeardownEpoch() mock returns 0
  });

  it('does NOT count a mid-unlock teardown against the passphrase lockout (PR #2337)', async () => {
    // A 401 -> nuclearReset landing during the Argon2id unlock window makes
    // initialize() reject with the typed E2EEInitTeardownError. That is a
    // dead-session signal, NOT a wrong passphrase: it must not increment the
    // lockout counter or render "Incorrect passphrase".
    const { E2EEInitTeardownError } = await import('@/renderer/services/e2eeErrors');
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => makeKeysResponse(),
      text: async () => JSON.stringify(makeKeysResponse()),
    });
    mockE2eeInitialize.mockRejectedValueOnce(new E2EEInitTeardownError());

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => {
      expect(screen.queryByText(/session expired/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/incorrect passphrase/i)).not.toBeInTheDocument();
    expect(onUnlock).not.toHaveBeenCalled();
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    expect(revokeAbortedSession).toHaveBeenCalled();
  });

  it('does NOT admit when a teardown lands after initialize resolves (pre-admit check, PR #2337)', async () => {
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(true);
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => makeKeysResponse(),
      text: async () => JSON.stringify(makeKeysResponse()),
    });

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => {
      expect(screen.queryByText(/session expired/i)).toBeInTheDocument();
    });
    expect(onUnlock).not.toHaveBeenCalled();
  });

  it('does NOT admit when the token was invalidated WITHOUT an E2EE teardown (Codex P1, PR #2337)', async () => {
    // Auth ownership can be lost at a different await boundary than key
    // teardown. This synthetic token-only clear leaves the epoch unchanged,
    // so the token-lifecycle admit gate must catch the loss before onUnlock().
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    useAuthStore.getState().setAccessToken('sso-token-A'); // SSO session: token, no session ID
    mockE2eeInitialize.mockImplementationOnce(async () => {
      // The token-only clear lands while init/persist is in flight.
      useAuthStore.getState().clearAccessToken();
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => makeKeysResponse(),
      text: async () => JSON.stringify(makeKeysResponse()),
    });

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => {
      expect(screen.queryByText(/session expired/i)).toBeInTheDocument();
    });
    expect(onUnlock).not.toHaveBeenCalled();
    expect(revokeAbortedSession).toHaveBeenCalled();
  });

  it('wipes re-persisted E2EE keys on a nuclear-class teardown abort — but never for rememberMe (Codex P1/#1768, PR #2337)', async () => {
    // The persist IPC can re-write E2EE keys AFTER a teardown wiped the main
    // process. On a !rememberMe (nuclear) teardown the abort wipes again; on
    // a rememberMe (graceful) teardown the disk state is deliberately
    // preserved for next-launch restore (#1768) and must NOT be touched.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const runAttempt = async () => {
      useAuthStore.getState().beginAuthLifecycle('sso-token-A', 'sso-session-A');
      useE2EEStore.getState().setNeedsSSOUnlock(true, 41);
      vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(true);
      mockE2eeInitialize.mockResolvedValueOnce(undefined);
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        headers: new Headers({ 'Content-Type': 'application/json' }),
        json: async () => makeKeysResponse(),
        text: async () => JSON.stringify(makeKeysResponse()),
      });
      const onUnlock = vi.fn();
      const view = render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);
      fireEvent.change(screen.getByLabelText(/passphrase/i), {
        target: { value: 'CorrectPassphrase!12' },
      });
      fireEvent.click(screen.getByRole('button', { name: /unlock/i }));
      await waitFor(() => {
        expect(screen.queryByText(/session expired/i)).toBeInTheDocument();
      });
      expect(onUnlock).not.toHaveBeenCalled();
      view.unmount();
    };

    // Nuclear-class teardown (!rememberMe): wipe fires.
    useAuthStore.getState().setRememberMe(false);
    await runAttempt();
    expect(mockClearTokensIfOwner).toHaveBeenCalledOnce();
    expect(mockClearTokensIfOwner).toHaveBeenCalledWith(41);
    expect(mockClearTokens).not.toHaveBeenCalled();

    // Graceful (rememberMe) teardown: disk state preserved (#1768).
    mockClearTokensIfOwner.mockClear();
    resetAllStores();
    useAuthStore.getState().setRememberMe(true);
    await runAttempt();
    expect(mockClearTokensIfOwner).not.toHaveBeenCalled();
    expect(mockClearTokens).not.toHaveBeenCalled();
  });

  it('does NOT admit when a token-only clear lands during the keys fetch/parse (Codex P1, PR #2337)', async () => {
    // A token-only teardown during the fetch/parse leaves an EMPTY store. The
    // post-fetch re-capture must not adopt {null, null} — that would make the
    // ownership gate vacuously pass (null === null) and admit a dead session.
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    useAuthStore.getState().setAccessToken('sso-token-A');
    mockE2eeInitialize.mockResolvedValueOnce(undefined);
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => {
        // The token-only clear lands while safeJson() is parsing.
        useAuthStore.getState().clearAccessToken();
        return makeKeysResponse();
      },
      text: async () => JSON.stringify(makeKeysResponse()),
    });

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => {
      expect(screen.queryByText(/session expired/i)).toBeInTheDocument();
    });
    expect(onUnlock).not.toHaveBeenCalled();
    expect(revokeAbortedSession).toHaveBeenCalled();
  });

  it('still admits when the keys fetch transparently refreshed the session (Codex P1, PR #2337)', async () => {
    // A 401 on /users/me/keys that apiFetch recovers installs a rotated token
    // + session ID for the SAME session. The ownership snapshot is re-captured
    // after the fetch, so the admit gate must accept the refreshed identity —
    // pre-fix the stale bootstrap snapshot {A, null} rejected the valid session.
    const authGeneration = useAuthStore.getState().authGeneration;
    mockE2eeInitialize.mockResolvedValueOnce(undefined);
    mockFetch.mockImplementationOnce(async () => {
      // The transparent refresh atomically rotates both server credentials
      // while preserving the renderer-owned auth lifecycle generation.
      expect(
        useAuthStore
          .getState()
          .rotateAuthCredentials(authGeneration, 'sso-token-B', 'sso-session-B')
      ).toBe(true);
      return {
        ok: true,
        status: 200,
        headers: new Headers({ 'Content-Type': 'application/json' }),
        json: async () => makeKeysResponse(),
        text: async () => JSON.stringify(makeKeysResponse()),
      };
    });

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => expect(onUnlock).toHaveBeenCalledTimes(1));
  });

  it('after 3 wrong attempts, offers Social Recovery', async () => {
    // The component fetches keys lazily on each submit so the failing unwrap
    // can re-fetch — three rounds of /users/me/keys then a unwrap rejection.
    for (let i = 0; i < 3; i++) {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        headers: new Headers({ 'Content-Type': 'application/json' }),
        json: async () => makeKeysResponse(),
        text: async () => JSON.stringify(makeKeysResponse()),
      });
    }
    mockE2eeInitialize.mockRejectedValue(new Error('decrypt_failed'));

    const onSocialRecovery = vi.fn();
    render(<SSOEagerUnlock onUnlock={vi.fn()} onSocialRecovery={onSocialRecovery} />);

    // First two wrong attempts: stay on the prompt with "Incorrect passphrase".
    for (let i = 0; i < 2; i++) {
      fireEvent.change(screen.getByLabelText(/passphrase/i), {
        target: { value: 'wrong' + i },
      });
      fireEvent.click(screen.getByRole('button', { name: /unlock/i }));
      await waitFor(() => {
        expect(screen.queryByText(/incorrect passphrase/i)).toBeInTheDocument();
      });
    }

    // Third wrong attempt flips the locked-out branch — the prompt goes away
    // and the Social Recovery offer appears.
    fireEvent.change(screen.getByLabelText(/passphrase/i), { target: { value: 'wrong-3rd' } });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    const recoveryBtn = await screen.findByRole('button', { name: /social recovery/i });
    expect(recoveryBtn).toBeInTheDocument();

    fireEvent.click(recoveryBtn);
    await waitFor(() => expect(onSocialRecovery).toHaveBeenCalledTimes(1));
    expect(mockClearTokensIfOwner).toHaveBeenCalledWith(41);
    expect(mockClearTokens).not.toHaveBeenCalled();
  });
});
