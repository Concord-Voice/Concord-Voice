import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import SSOPassphraseSetup from '@/renderer/components/Auth/SSOPassphraseSetup';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';

// Stub server-session cleanup so abort-path behavior is directly assertable.
vi.mock('@/renderer/services/apiClient', async (orig) => ({
  ...(await orig<typeof import('@/renderer/services/apiClient')>()),
  revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
}));

// Mock crypto utilities so the test does not actually run RSA-4096 keygen
vi.mock('@/renderer/utils/crypto', () => ({
  generateRegistrationKeys: vi.fn().mockResolvedValue({
    wrappedPrivateKey: 'bW9jay13cmFwcGVkLXByaXZhdGUta2V5', // pragma: allowlist secret
    keyDerivationSalt: 'bW9jay1zYWx0', // pragma: allowlist secret
    keyDerivationAlg: 'argon2id',
    publicKey: { __mockKey: 'public' },
    privateKey: { __mockKey: 'private' },
  }),
  exportPublicKey: vi.fn().mockResolvedValue('bW9jay1wdWJsaWMta2V5'), // pragma: allowlist secret
}));

// Mock e2eeService — SSOPassphraseSetup must initialize it with the generated keys
// on a successful SSO registration so a new SSO user has E2EE ready at /app without
// a separate login (otherwise channel creation hits "Setting up secure messaging"
// until logout→login). Mirrors the Register.tsx fix (#1278); see #1287.
//
// Stateful: initialize() flips `initialized` on, clearKeys() off, and
// getSessionKeys() returns keys ONLY while initialized — so the failure-path tests
// can't false-pass on a post-clearKeys persist (getSessionKeys → null after
// clearKeys, so the persist block no-ops). `e2eeState` is reset per test.
const e2eeState = vi.hoisted(() => ({
  initialized: false,
  attempt: 0,
  sessionKeys: {
    wrappingKeyBase64: 'wk',
    preferencesKeyBase64: 'pk',
    wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
  },
}));
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: vi.fn(async () => {
      e2eeState.initialized = true;
      e2eeState.attempt += 1;
      // #2423: initialize() returns THIS invocation's receipt.
      return { sessionKeys: e2eeState.sessionKeys, attempt: e2eeState.attempt };
    }),
    captureTeardownEpoch: vi.fn().mockReturnValue(0),
    wasTornDownSince: vi.fn().mockReturnValue(false),
    getSessionKeys: vi.fn(() => (e2eeState.initialized ? e2eeState.sessionKeys : null)),
    clearKeysIfInitializationCurrent: vi.fn(
      (receipt: { sessionKeys: typeof e2eeState.sessionKeys; attempt: number } | null) => {
        if (
          !e2eeState.initialized ||
          receipt?.sessionKeys !== e2eeState.sessionKeys ||
          receipt?.attempt !== e2eeState.attempt
        ) {
          return false;
        }
        e2eeState.initialized = false;
        return true;
      }
    ),
    clearKeys: vi.fn(() => {
      e2eeState.initialized = false;
    }),
  },
}));

// Mock useSSOFlow so the re-initiate affordance (added for #2045) is assertable:
// clicking "Sign in with Google again" must call begin(provider) to mint a FRESH
// sso_token, rather than leaving the user re-submitting an expired one. The mock
// returns a resolved promise so an awaited-or-.catch'd onClick never rejects.
const mockBegin = vi.hoisted(() => vi.fn(() => Promise.resolve()));
vi.mock('@/renderer/hooks/useSSOFlow', () => ({
  useSSOFlow: () => ({ begin: mockBegin }),
}));

const storeRefreshToken = vi.fn().mockResolvedValue(71);
const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
const storeE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
const originalElectron = globalThis.electron;
const registrationResult = {
  kind: 'tokens' as const,
  accessToken: 'access-xyz',
  sessionId: 'session-xyz',
  credentialOwner: 71,
};
const completeRegistration = vi.fn().mockResolvedValue(registrationResult);

beforeEach(() => {
  vi.clearAllMocks();
  e2eeState.initialized = false;
  e2eeState.attempt = 0;
  vi.mocked(e2eeService.initialize).mockImplementation(async () => {
    e2eeState.initialized = true;
    e2eeState.attempt += 1;
    // #2423: initialize() returns THIS invocation's receipt.
    return { sessionKeys: e2eeState.sessionKeys, attempt: e2eeState.attempt };
  });
  vi.mocked(e2eeService.captureTeardownEpoch).mockReturnValue(0);
  vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(false);
  vi.mocked(e2eeService.getSessionKeys).mockImplementation(() =>
    e2eeState.initialized ? e2eeState.sessionKeys : null
  );
  vi.mocked(e2eeService.clearKeysIfInitializationCurrent).mockImplementation((receipt) => {
    if (
      !e2eeState.initialized ||
      receipt?.sessionKeys !== e2eeState.sessionKeys ||
      receipt?.attempt !== e2eeState.attempt
    ) {
      return false;
    }
    e2eeState.initialized = false;
    return true;
  });
  vi.mocked(e2eeService.clearKeys).mockImplementation(() => {
    e2eeState.initialized = false;
  });
  storeRefreshToken.mockResolvedValue(71);
  completeRegistration.mockResolvedValue(registrationResult);
  clearTokensIfOwner.mockResolvedValue(true);
  storeE2EEKeysIfOwner.mockResolvedValue(true);
  resetAllStores();
  resetRuntimeServerBase();
  Object.defineProperty(globalThis, 'electron', {
    value: {
      ...originalElectron,
      storeRefreshToken,
      clearTokensIfOwner,
      storeE2EEKeysIfOwner,
      sso: { ...originalElectron?.sso, completeRegistration },
    },
    writable: true,
  });
  useSSOStore.getState().setState({
    phase: 'register_required',
    provider: 'google',
    ssoToken: 'tok-fake',
    email: 'new@example.test',
    name: 'New User',
  });
});

afterEach(() => {
  resetRuntimeServerBase();
  Object.defineProperty(globalThis, 'electron', {
    value: originalElectron,
    writable: true,
  });
});

describe('SSOPassphraseSetup', () => {
  it('shows the email and asks for a passphrase', () => {
    render(<SSOPassphraseSetup />);
    expect(screen.getByText('new@example.test')).toBeInTheDocument();
    expect(screen.getByLabelText(/^passphrase$/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/confirm passphrase/i)).toBeInTheDocument();
  });

  it('shows username hint with identity', () => {
    render(<SSOPassphraseSetup />);
    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: 'newcomer' } });
    expect(screen.getByText(/identity: @newcomer/)).toBeInTheDocument();
  });

  it('disables submit until passphrases match and meet strength', () => {
    render(<SSOPassphraseSetup />);
    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: 'newcomer' } });

    fireEvent.change(screen.getByLabelText(/^passphrase$/i), { target: { value: 'short' } });
    expect(screen.getByRole('button', { name: /create account/i })).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/^passphrase$/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.change(screen.getByLabelText(/confirm passphrase/i), {
      target: { value: 'mismatch' },
    });
    expect(screen.getByRole('button', { name: /create account/i })).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/confirm passphrase/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    expect(screen.getByRole('button', { name: /create account/i })).toBeEnabled();
  });

  it('delegates completion and key material to main without renderer refresh-token custody', async () => {
    render(<SSOPassphraseSetup />);
    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: 'newcomer' } });
    fireEvent.change(screen.getByLabelText(/^passphrase$/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.change(screen.getByLabelText(/confirm passphrase/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create account/i }));

    await waitFor(() => expect(completeRegistration).toHaveBeenCalledTimes(1));
    expect(completeRegistration).toHaveBeenCalledWith('http://localhost:8080', {
      provider: 'google',
      ssoToken: 'tok-fake',
      username: 'newcomer',
      passphrase: 'StrongPassphrase!12345', // pragma: allowlist secret
      wrappedPrivateKey: 'bW9jay13cmFwcGVkLXByaXZhdGUta2V5', // pragma: allowlist secret
      keyDerivationSalt: 'bW9jay1zYWx0', // pragma: allowlist secret
      publicKey: 'bW9jay1wdWJsaWMta2V5', // pragma: allowlist secret
    });

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    });
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(useSSOStore.getState().state.phase).toBe('idle');
  });

  it('renders nothing when phase is not register_required (defensive)', () => {
    useSSOStore.getState().reset();
    const { container } = render(<SSOPassphraseSetup />);
    expect(container).toBeEmptyDOMElement();
  });

  it('surfaces username_taken 409 with a friendly inline message', async () => {
    completeRegistration.mockResolvedValueOnce({
      kind: 'error',
      status: 409,
      code: 'username_taken',
      body: { error_code: 'username_taken' },
    });

    render(<SSOPassphraseSetup />);
    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: 'newcomer' } });
    fireEvent.change(screen.getByLabelText(/^passphrase$/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.change(screen.getByLabelText(/confirm passphrase/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create account/i }));

    await waitFor(() => {
      expect(screen.getByText(/this username is already taken/i)).toBeInTheDocument();
    });
  });

  // --- E2EE init on first-run SSO setup (#1287, mirrors Register.tsx #1278) ---

  const fillAndSubmit = () => {
    fireEvent.change(screen.getByLabelText(/username/i), { target: { value: 'newcomer' } });
    fireEvent.change(screen.getByLabelText(/^passphrase$/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.change(screen.getByLabelText(/confirm passphrase/i), {
      target: { value: 'StrongPassphrase!12345' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create account/i }));
  };

  const okRegistrationResult = () => ({ ...registrationResult });

  it('revokes an old-origin response without auth, disk, or E2EE mutation after a server switch', async () => {
    let resolveResponse: (response: ReturnType<typeof okRegistrationResult>) => void = () => {};
    completeRegistration.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveResponse = resolve;
      })
    );

    render(<SSOPassphraseSetup />);
    fillAndSubmit();
    await waitFor(() => expect(completeRegistration).toHaveBeenCalledTimes(1));

    setRuntimeServerBase('https://server-b.example');
    resolveResponse(okRegistrationResult());

    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalledTimes(1));
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'access-xyz',
      sessionId: 'session-xyz',
      apiBase: 'http://localhost:8080',
    });
    expect(clearTokensIfOwner).toHaveBeenCalledWith(71);
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useSSOStore.getState().state.phase).toBe('register_required');
  });

  it('preserves a successor generation and skips stale E2EE work after main completion', async () => {
    completeRegistration.mockImplementationOnce(async () => {
      useAuthStore.getState().beginAuthLifecycle('successor-token', 'successor-session');
      return okRegistrationResult();
    });

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalledTimes(1));
    expect(clearTokensIfOwner).toHaveBeenCalledWith(71);
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('successor-token');
    expect(useAuthStore.getState().sessionId).toBe('successor-session');
    expect(useSSOStore.getState().state.phase).toBe('register_required');
  });

  it('does not clear E2EE keys committed by a successor while initialization resolves', async () => {
    const successorKeys = {
      wrappingKeyBase64: 'successor-wk',
      preferencesKeyBase64: 'successor-pk',
      wrappedPrivateKeyBase64: 'successor-wpk', // pragma: allowlist secret
    };
    vi.mocked(e2eeService.getSessionKeys).mockReturnValue(successorKeys);
    vi.mocked(e2eeService.initialize).mockImplementationOnce(async () => {
      useAuthStore.getState().beginAuthLifecycle('successor-token', 'successor-session');
    });
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalledTimes(1));
    expect(e2eeService.clearKeys).not.toHaveBeenCalled();
    expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('successor-token');
  });

  it('initializes e2eeService with the generated keys and persists them on successful SSO setup', async () => {
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    // Wait for the TERMINAL state first: setSSOState('idle') is the last statement in
    // the handler, so once phase is 'idle' the awaited initialize() AND the subsequent
    // storeE2EEKeys() have both completed. Asserting the call-spies after this wait is
    // race-free — a bare assertion before it could outrun the still-pending persist
    // (Copilot #1289 review).
    await waitFor(() => {
      expect(useSSOStore.getState().state.phase).toBe('idle');
    });

    // Init with the passphrase + the (mocked) generated key material — this is what
    // lets a fresh SSO user create channels / message without a re-login.
    expect(e2eeService.initialize).toHaveBeenCalledWith(
      'StrongPassphrase!12345',
      'bW9jay13cmFwcGVkLXByaXZhdGUta2V5', // pragma: allowlist secret
      'bW9jay1zYWx0', // pragma: allowlist secret
      'argon2id',
      expect.objectContaining({
        signal: expect.any(AbortSignal),
        isCurrent: expect.any(Function),
      }),
      0 // teardownEpoch captured at handleSubmit start (PR #2337)
    );
    // Session keys persisted to the OS keychain so E2EE survives an app restart.
    expect(storeE2EEKeysIfOwner).toHaveBeenCalledWith(
      {
        wrappingKeyBase64: 'wk',
        preferencesKeyBase64: 'pk',
        wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
      },
      71
    );
    expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    expect(useAuthStore.getState().sessionId).toBe('session-xyz');
    expect(storeRefreshToken).not.toHaveBeenCalled();
    // ...and init precedes persistence.
    const initOrder = vi.mocked(e2eeService.initialize).mock.invocationCallOrder[0];
    const persistOrder = storeE2EEKeysIfOwner.mock.invocationCallOrder[0];
    expect(initOrder).toBeLessThan(persistOrder);
  });

  it('completes SSO setup even if e2eeService init fails (non-fatal), rolling back via clearKeys', async () => {
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new Error('init boom'));
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    // A failed E2EE pre-init must NOT discard an otherwise-successful SSO
    // registration. The catch rolls back partial keys and the eager-unlock gate
    // keeps the authenticated shell closed until the user unlocks them again.
    await waitFor(() => {
      expect(useSSOStore.getState().state.phase).toBe('idle');
    });
    expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    expect(e2eeService.clearKeys).toHaveBeenCalled();
  });

  it('aborts SSO setup on a mid-setup teardown — clears the fresh token, never admits (PR #2337)', async () => {
    // A logout-class teardown landing after the teardown epoch is captured
    // makes initialize() reject with the typed E2EEInitTeardownError. Admission
    // is deliberately deferred until setup finishes, so no token is published.
    const { E2EEInitTeardownError } = await import('@/renderer/services/e2eeErrors');
    let rejectInit: (e: unknown) => void = () => {};
    vi.mocked(e2eeService.initialize).mockReturnValueOnce(
      new Promise<void>((_, reject) => {
        rejectInit = reject;
      })
    );
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    await waitFor(() => expect(e2eeService.initialize).toHaveBeenCalled());
    expect(useAuthStore.getState().accessToken).toBeNull();

    // Now the mid-setup teardown makes initialize() reject.
    rejectInit(new E2EEInitTeardownError());

    await waitFor(() => {
      expect(screen.queryByText(/session ended during setup/i)).toBeInTheDocument();
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    // The admit path (phase 'idle') is never reached.
    expect(useSSOStore.getState().state.phase).not.toBe('idle');
    // The freshly-issued server session (HttpOnly refresh cookie + row) is revoked.
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    expect(revokeAbortedSession).toHaveBeenCalled();
    expect(clearTokensIfOwner).toHaveBeenCalledWith(71);
  });

  it('does NOT complete setup when the token was invalidated WITHOUT an E2EE teardown (Codex P1, PR #2337)', async () => {
    // Auth ownership can be lost at a different await boundary than key
    // teardown. This synthetic token-only clear leaves the epoch unchanged,
    // so the token-ownership admit gate must abort instead of setting phase
    // 'idle' with rejected credentials.
    const { useAuthStore } = await import('@/renderer/stores/authStore');
    const { useSSOStore } = await import('@/renderer/stores/ssoStore');
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    // The token-only clear lands while init/persist is in flight.
    vi.mocked(e2eeService.initialize).mockImplementationOnce(async () => {
      useAuthStore.getState().clearAccessToken();
    });
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    expect(useSSOStore.getState().state.phase).not.toBe('idle');
    expect(clearTokensIfOwner).toHaveBeenCalledWith(71);
  });

  it("preserves a successor session's token when the SSO abort fires (Codex P1, PR #2337)", async () => {
    // A successor sign-in can complete while abortSetupForTeardown awaits
    // revokeAbortedSession(). The local clear is identity-guarded: only the
    // aborted flow's own token is stripped, never the successor's — pre-fix
    // the unconditional clearAccessToken logged the NEW session out of the
    // renderer even though the revoke helper correctly declined it.
    const { E2EEInitTeardownError } = await import('@/renderer/services/e2eeErrors');
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    vi.mocked(revokeAbortedSession).mockImplementationOnce(async () => {
      // The successor login lands while the revoke is in flight.
      useAuthStore.getState().setAccessToken('successor-token');
    });
    let rejectInit: (e: unknown) => void = () => {};
    vi.mocked(e2eeService.initialize).mockReturnValueOnce(
      new Promise<void>((_, reject) => {
        rejectInit = reject;
      })
    );
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();
    await waitFor(() => expect(e2eeService.initialize).toHaveBeenCalled());
    expect(useAuthStore.getState().accessToken).toBeNull();
    rejectInit(new E2EEInitTeardownError());

    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    expect(useAuthStore.getState().accessToken).toBe('successor-token');
    expect(screen.queryByText(/session ended during setup/i)).not.toBeInTheDocument();
  });

  it('admits an ordinary init failure only behind owner-scoped SSO eager unlock', async () => {
    // The catch's deliberate rollback clearKeys() bumps the real
    // keyClearGeneration. With a FAITHFUL epoch mock (coupled to clearKeys,
    // unlike the default), the pre-admit check must NOT misread that
    // self-inflicted clear as an external teardown: setup completes
    // non-fatally (phase idle, token kept) per the #1278 contract, but the
    // authenticated shell must remain gated until this credential owner
    // completes SSOEagerUnlock.
    let clears = 0;
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new Error('init boom'));
    vi.mocked(e2eeService.clearKeys).mockImplementation(() => {
      e2eeState.initialized = false;
      clears += 1;
    });
    vi.mocked(e2eeService.captureTeardownEpoch).mockImplementation(() => clears);
    vi.mocked(e2eeService.wasTornDownSince).mockImplementation((epoch: number) => clears !== epoch);
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    await waitFor(() => {
      expect(useSSOStore.getState().state.phase).toBe('idle');
    });
    expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    expect(useE2EEStore.getState()).toMatchObject({
      needsSSOUnlock: true,
      ssoCredentialOwner: 71,
    });
    expect(screen.queryByText(/session ended during setup/i)).not.toBeInTheDocument();
  });

  it('does NOT admit when a teardown lands after initialize resolves (pre-admit check, PR #2337)', async () => {
    vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(true);
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    await waitFor(() => {
      expect(screen.queryByText(/session ended during setup/i)).toBeInTheDocument();
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useSSOStore.getState().state.phase).not.toBe('idle');
  });

  it('keeps the in-memory E2EE session when only keychain persistence fails', async () => {
    storeE2EEKeysIfOwner.mockRejectedValueOnce(new Error('keychain locked'));
    completeRegistration.mockResolvedValueOnce(okRegistrationResult());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    // Wait for the TERMINAL state: phase 'idle' is set after the persist catch runs,
    // so by then storeE2EEKeysIfOwner was called (and rejected, and caught). Asserting after
    // this wait avoids racing the still-pending catch (Copilot #1289 review).
    await waitFor(() => {
      expect(useSSOStore.getState().state.phase).toBe('idle');
    });
    expect(storeE2EEKeysIfOwner).toHaveBeenCalled();
    // The persistence failure must leave the in-memory session intact.
    expect(e2eeService.clearKeys).not.toHaveBeenCalled();
  });

  // --- Expired sso_token recovery (#2045) ---

  it('surfaces an expired sso_token (401) with an actionable message and a working re-initiate affordance (regression #2045)', async () => {
    // The sso_token has a server-side TTL; a user who dwells on this screen past it
    // gets 401 sso_token_invalid on complete-registration. The old behavior mapped
    // that to the generic "Registration failed. Please try again." and left the user
    // re-submitting the same dead token — a permanent dead-end. It must instead show
    // an actionable expiry message AND a one-click path to restart sign-in.
    completeRegistration.mockResolvedValueOnce({
      kind: 'error',
      status: 401,
      code: 'sso_token_invalid',
      body: { error_code: 'sso_token_invalid' },
    });

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    // Actionable expiry copy — NOT the generic dead-end.
    await waitFor(() => {
      expect(screen.getByText(/session expired/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/registration failed\. please try again/i)).not.toBeInTheDocument();

    // A re-initiate affordance that restarts the Google flow (fresh token) instead
    // of re-submitting the expired one. getByRole scopes to the button, so it does
    // not collide with the same phrase inside the alert message.
    const restart = screen.getByRole('button', { name: /sign in with google again/i });
    fireEvent.click(restart);
    expect(mockBegin).toHaveBeenCalledWith('google');
  });

  it('names the correct provider (Apple) in the expiry recovery copy — not hardcoded Google (regression #2045)', async () => {
    // The same screen serves Apple SSO; the recovery copy, intro, and button must
    // say "Apple", and the re-initiate must restart the APPLE flow.
    useSSOStore.getState().setState({
      phase: 'register_required',
      provider: 'apple',
      ssoToken: 'tok-fake',
      email: 'new@example.test',
      name: 'New User',
    });
    completeRegistration.mockResolvedValueOnce({
      kind: 'error',
      status: 401,
      code: 'sso_token_invalid',
      body: { error_code: 'sso_token_invalid' },
    });

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    await waitFor(() => {
      expect(screen.getByText(/session expired/i)).toBeInTheDocument();
    });
    // Provider-aware re-initiate button, and NO "Google" leaks anywhere on the
    // Apple screen (intro + message + button are all provider-derived).
    const restart = screen.getByRole('button', { name: /sign in with apple again/i });
    expect(screen.queryByText(/google/i)).toBeNull();
    fireEvent.click(restart);
    expect(mockBegin).toHaveBeenCalledWith('apple');
  });
});
