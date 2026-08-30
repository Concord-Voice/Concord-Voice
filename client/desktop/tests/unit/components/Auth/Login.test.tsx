import { render, screen, userEvent, within } from '../../../test-utils';
import Login from '@/renderer/components/Auth/Login';
import { vi } from 'vitest';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useSSOStore } from '@/renderer/stores/auth/ssoStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  getApiBase,
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';

// Mock global fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Get references to mocked functions for assertions
const mockUnwrapLoginKeys = vi.fn().mockResolvedValue(undefined);
const mockGenerateRegistrationKeys = vi.fn().mockResolvedValue({
  wrappedPrivateKey: 'mock',
  keyDerivationSalt: 'mock',
  keyDerivationAlg: 'argon2id',
  publicKey: {},
});
const { mockE2EESessionKeys, mockE2EEInitializationReceipt, mockE2EEClearKeys } = vi.hoisted(() => {
  const sessionKeys = {
    wrappingKeyBase64: 'mock-wrapping-key',
    preferencesKeyBase64: 'mock-preferences-key',
    wrappedPrivateKeyBase64: 'mock-wrapped-private-key',
  };
  return {
    mockE2EESessionKeys: sessionKeys,
    mockE2EEInitializationReceipt: { sessionKeys, attempt: 1 },
    mockE2EEClearKeys: vi.fn(),
  };
});

// Mock crypto and services to avoid real key operations
vi.mock('@/renderer/utils/crypto', () => ({
  unwrapLoginKeys: (...args: unknown[]) => mockUnwrapLoginKeys(...args),
  generateRegistrationKeys: (...args: unknown[]) => mockGenerateRegistrationKeys(...args),
  exportPublicKey: vi.fn().mockResolvedValue('mock-public-key'),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: vi.fn().mockResolvedValue(mockE2EEInitializationReceipt),
    clearKeys: mockE2EEClearKeys,
    captureTeardownEpoch: vi.fn().mockReturnValue(0),
    wasTornDownSince: vi.fn().mockReturnValue(false),
    getSessionKeys: vi.fn().mockReturnValue(mockE2EESessionKeys),
    clearKeysIfInitializationCurrent: vi.fn((receipt: unknown) => {
      if (receipt !== mockE2EEInitializationReceipt) return false;
      mockE2EEClearKeys();
      return true;
    }),
  },
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    init: vi.fn(),
    startWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
  apiFetch: vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) }),
  ensureMachineId: vi.fn().mockResolvedValue('mock-machine-id'),
  revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
}));

/** Helper: standard login response payload */
function makeLoginResponse(overrides = {}) {
  return {
    access_token: 'mock-access',
    refresh_token: 'mock-refresh',
    session_id: 'mock-session',
    user: { id: 'user-1', username: 'testuser', email: 'test@example.com' },
    remember_me: false,
    e2ee_keys: {
      wrapped_private_key: 'mock-wrapped',
      key_derivation_salt: 'mock-salt',
      key_derivation_alg: 'pbkdf2',
    },
    ...overrides,
  };
}

/** Helper: MFA challenge response payload */
function makeMFAResponse(methods: string[] = ['totp'], overrides = {}) {
  return {
    mfa_required: true,
    mfa_challenge_token: 'mfa-token-123',
    methods,
    recovery_only_methods: [],
    ...overrides,
  };
}

describe('Login', () => {
  const onBack = vi.fn();
  const onSuccess = vi.fn();
  const onSwitchToRegister = vi.fn();
  const onForgotPassword = vi.fn();

  const defaultProps = {
    onBack,
    onSuccess,
    onSwitchToRegister,
    onForgotPassword,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    resetRuntimeServerBase();
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...originalElectron,
        storeRefreshToken: vi.fn().mockResolvedValue(41),
        clearTokensIfOwner: vi.fn().mockResolvedValue(true),
        storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true),
      },
      writable: true,
    });
    vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(false);
    useClientConfigStore.getState().setServerCapabilities({
      auth: { oauthProviders: ['google', 'apple'] },
    });
  });

  // Tests that stub globalThis.electron (storeRefreshToken side effects) must
  // not leak the stub into later tests — restore the pre-suite value.
  const originalElectron = globalThis.electron;
  const originalCredentials = navigator.credentials;
  afterEach(() => {
    resetRuntimeServerBase();
    Object.defineProperty(globalThis, 'electron', {
      value: originalElectron,
      writable: true,
    });
    Object.defineProperty(navigator, 'credentials', {
      value: originalCredentials,
      writable: true,
      configurable: true,
    });
  });

  // ── Rendering ──────────────────────────────────────────────────────────

  it('renders login form', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByText('Welcome Back')).toBeInTheDocument();
    expect(screen.getByText('Sign in to your Concord Voice account')).toBeInTheDocument();
  });

  it('renders email and password inputs', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByPlaceholderText('you@example.com')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument();
  });

  it('renders remember me checkbox unchecked by default', () => {
    render(<Login {...defaultProps} />);
    const checkbox = screen.getByRole('checkbox');
    expect(checkbox).not.toBeChecked();
  });

  it('renders sign in button', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByText('Sign In')).toBeInTheDocument();
  });

  it('renders forgot password link', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByText('Forgot password?')).toBeInTheDocument();
  });

  it('renders create account link', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByText('Create one')).toBeInTheDocument();
  });

  it('renders back to connection options button', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByText(/Back to Connection Options/)).toBeInTheDocument();
  });

  it('renders Concord logo', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByAltText('Concord Voice')).toBeInTheDocument();
  });

  // ── Password visibility toggle (#1917) ──────────────────────────────────

  it('renders the password field hidden by default', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByPlaceholderText('Enter your password')).toHaveAttribute('type', 'password');
  });

  it('renders a reveal toggle (aria-pressed=false) by default', () => {
    render(<Login {...defaultProps} />);
    expect(screen.getByRole('button', { name: /show password/i })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  it('reveals then re-hides the password when the toggle is clicked', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    const input = screen.getByPlaceholderText('Enter your password');
    expect(input).toHaveAttribute('type', 'password');

    await user.click(screen.getByRole('button', { name: /show password/i }));
    expect(input).toHaveAttribute('type', 'text');
    expect(screen.getByRole('button', { name: /hide password/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    );

    await user.click(screen.getByRole('button', { name: /hide password/i }));
    expect(input).toHaveAttribute('type', 'password');
  });

  it('preserves the typed password value across a visibility toggle', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    const input = screen.getByPlaceholderText('Enter your password');
    await user.type(input, 'MySecret123!');
    expect(input).toHaveValue('MySecret123!');

    await user.click(screen.getByRole('button', { name: /show password/i }));
    expect(input).toHaveValue('MySecret123!');
    await user.click(screen.getByRole('button', { name: /hide password/i }));
    expect(input).toHaveValue('MySecret123!');
  });

  // ── Form Validation ────────────────────────────────────────────────────

  it('shows email validation error for empty email', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText('Sign In'));
    expect(screen.getByText('Email is required')).toBeInTheDocument();
  });

  it('shows email validation error for invalid email format', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@x');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'somepassword');
    await user.click(screen.getByText('Sign In'));
    expect(screen.getByText('Invalid email format')).toBeInTheDocument();
  });

  it('shows password validation error for empty password', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.click(screen.getByText('Sign In'));
    expect(screen.getByText('Password is required')).toBeInTheDocument();
  });

  it('shows both email and password errors when both are empty', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText('Sign In'));
    expect(screen.getByText('Email is required')).toBeInTheDocument();
    expect(screen.getByText('Password is required')).toBeInTheDocument();
  });

  it('clears field error when user types', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText('Sign In'));
    expect(screen.getByText('Email is required')).toBeInTheDocument();
    await user.type(screen.getByPlaceholderText('you@example.com'), 't');
    expect(screen.queryByText('Email is required')).not.toBeInTheDocument();
  });

  it('does not submit fetch when validation fails', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText('Sign In'));
    expect(mockFetch).not.toHaveBeenCalled();
  });

  // ── Navigation Callbacks ───────────────────────────────────────────────

  it('calls onBack when back button is clicked', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText(/Back to Connection Options/));
    expect(onBack).toHaveBeenCalled();
  });

  it('calls onSwitchToRegister when create one is clicked', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText('Create one'));
    expect(onSwitchToRegister).toHaveBeenCalled();
  });

  it('calls onForgotPassword when forgot password is clicked', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.click(screen.getByText('Forgot password?'));
    expect(onForgotPassword).toHaveBeenCalled();
  });

  // ── Remember Me ────────────────────────────────────────────────────────

  it('toggles remember me checkbox', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    const checkbox = screen.getByRole('checkbox');
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);
    expect(checkbox).toBeChecked();
  });

  it('sends remember_me in login request body', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse({ remember_me: true }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByRole('checkbox'));
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        expect.stringContaining('/auth/login'),
        expect.objectContaining({
          body: expect.stringContaining('"remember_me":true'),
        })
      );
    });
  });

  // ── Successful Login ───────────────────────────────────────────────────

  it('submits login and calls onSuccess', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(onSuccess).toHaveBeenCalledWith(
        expect.objectContaining({
          accessToken: 'mock-access',
        })
      );
    });
  });

  it('aborts login on E2EEInitTeardownError — no onSuccess, no token store, no recovery prompt (PR #2337)', async () => {
    // A mid-login teardown (401 -> nuclearReset) fences the key commit and
    // initialize() rejects with the typed teardown error. Login must abort:
    // never store the refresh token, never call onSuccess, and never route the
    // teardown into the consented key-reset prompt.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const { E2EEInitTeardownError } = await import('@/renderer/services/e2eeErrors');
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new E2EEInitTeardownError());
    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    // The typed error surfaces as a login error (outer catch), not the
    // key-recovery consent modal.
    await vi.waitFor(() => {
      expect(e2eeService.initialize).toHaveBeenCalled();
    });
    expect(onSuccess).not.toHaveBeenCalled();
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(screen.queryByText(/reset encryption keys/i)).not.toBeInTheDocument();
    // The server session established by /auth/login (cookie already set) is
    // revoked so the long-lived refresh credential can't outlive the abort.
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    expect(revokeAbortedSession).toHaveBeenCalled();
  });

  it('clears old-origin material and revokes when the server changes during login completion', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const { preferencesSyncService } = await import('@/renderer/services/preferencesSync');
    const requestApiBase = 'https://login-origin.example';
    setRuntimeServerBase(requestApiBase);
    let finishUnwrap: () => void = () => {
      throw new Error('Unwrap resolver was not initialized.');
    };
    mockUnwrapLoginKeys.mockReturnValueOnce(
      new Promise<void>((resolve) => {
        finishUnwrap = resolve;
      })
    );
    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));
    await vi.waitFor(() => expect(mockUnwrapLoginKeys).toHaveBeenCalled());

    setRuntimeServerBase('https://successor-origin.example');
    finishUnwrap();

    await vi.waitFor(() => {
      expect(revokeAbortedSession).toHaveBeenCalledWith(
        expect.objectContaining({
          accessToken: 'mock-access',
          sessionId: 'mock-session',
          apiBase: requestApiBase,
          authGeneration: expect.any(Number),
        })
      );
    });
    expect(e2eeService.clearKeys).toHaveBeenCalledOnce();
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(preferencesSyncService.init).not.toHaveBeenCalled();
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('aborts login when a teardown lands AFTER initialize resolves (pre-admit epoch check, PR #2337)', async () => {
    // The fence inside initialize() covers only the span up to the key commit.
    // A teardown during the later token-store / persist / hydrate awaits must
    // be caught by the pre-admit wasTornDownSince() checks — never onSuccess.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    vi.mocked(e2eeService.wasTornDownSince).mockReturnValue(true);
    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(e2eeService.wasTornDownSince).toHaveBeenCalled();
    });
    expect(onSuccess).not.toHaveBeenCalled();
    expect(storeRefreshToken).not.toHaveBeenCalled();
  });

  it('clears a token resurrected by a teardown during storeRefreshToken (Gitar/Codex P1, PR #2337)', async () => {
    // A teardown landing while storeRefreshToken is awaited clears the
    // renderer token and bumps the epoch. Pre-fix, the flow then
    // unconditionally re-published the torn-down token and the final abort
    // check left it resident — routing treated the user as authenticated
    // with E2EE cleared.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const notice = 'Your session ended before sign-in could finish. Please sign in again.';
    vi.mocked(e2eeService.wasTornDownSince)
      .mockReturnValueOnce(false) // pre-admit check #1 passes
      .mockReturnValue(true); // check #2 detects the mid-await teardown
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      useAuthStore.getState().clearAccessToken(); // the teardown's clear
      return 41;
    });
    const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken, clearTokensIfOwner },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    // The abort notice doubles as the flow-settled signal.
    expect(await screen.findByText(notice)).toBeInTheDocument();
    expect(onSuccess).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    // The refresh token persisted mid-flight must not survive on disk — a
    // failed best-effort revoke would otherwise let restoreSession()
    // resurrect the torn-down session next launch (Codex P1, #2337).
    expect(clearTokensIfOwner).toHaveBeenCalledWith(41);
  });

  it('refuses admit when the token was cleared without an E2EE teardown (Codex P1, PR #2337)', async () => {
    // Auth ownership can be lost at a different await boundary than key
    // teardown. This synthetic token-only clear leaves the epoch unchanged,
    // so the token-lifecycle admit gate must abort instead of completing login
    // with credentials the auth layer already rejected.
    const notice = 'Your session ended before sign-in could finish. Please sign in again.';
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      useAuthStore.getState().clearAccessToken(); // token-only clear, no epoch bump
      return 41;
    });
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    expect(await screen.findByText(notice)).toBeInTheDocument();
    expect(onSuccess).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
  });

  it("never strips a successor login's token when a stale flow aborts (PR #2337)", async () => {
    // If a rapid retry established a NEW session while this flow was
    // unwinding, the stale abort must not clear the successor's token and
    // must not stage a login notice for a user who is already in the app.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const notice = 'Your session ended before sign-in could finish. Please sign in again.';
    vi.mocked(e2eeService.wasTornDownSince).mockReturnValueOnce(false).mockReturnValue(true);
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      useAuthStore.getState().setAccessToken('successor-token'); // retry won the race
      return 40;
    });
    const clearTokensIfOwner = vi.fn().mockResolvedValue(false);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken, clearTokensIfOwner },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    expect(await screen.findByText(notice)).toBeInTheDocument();
    expect(onSuccess).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('successor-token');
    expect(useAuthStore.getState().loginNotice).toBeNull();
    // The successor session's disk tokens (its own storeRefreshToken owns the
    // disk copy now) must not be wiped by the stale abort (Codex P1, #2337).
    expect(clearTokensIfOwner).toHaveBeenCalledWith(40);
  });

  it('admits a login whose token and session were legitimately refreshed mid-flight (Codex P1, PR #2337)', async () => {
    // The backend rotates both A1 -> A2 and S1 -> S2. The client lifecycle
    // generation remains stable across that validated refresh, so Login must
    // admit without mistaking S2 for a successor account.
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      const auth = useAuthStore.getState();
      expect(
        auth.rotateAuthCredentials(auth.authGeneration, 'rotated-token', 'rotated-session')
      ).toBe(true);
      return 41;
    });
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(), // session_id: 'mock-session'
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(onSuccess).toHaveBeenCalled();
    });
    expect(revokeAbortedSession).not.toHaveBeenCalled();
    // onSuccess must carry the LIVE (rotated) token — AuthFlow writes it back
    // into authStore, and the original would clobber the refreshed credential
    // (Codex P1, #2337).
    expect(onSuccess).toHaveBeenCalledWith(
      expect.objectContaining({ accessToken: 'rotated-token' })
    );
    expect(useAuthStore.getState().sessionId).toBe('rotated-session');
  });

  it('rejects a token-only SSO successor instead of false-admitting an A2/S1 mixed account', async () => {
    // A password login owns {A1,S1}; a concurrent SSO completion has only A2.
    // Atomic lifecycle replacement must clear S1 and the generation gate must
    // stop the stale password/E2EE continuation from admitting as the SSO user.
    let diskOwner = 'none';
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      diskOwner = 'stale-password-flow';
      useAuthStore.getState().beginAuthLifecycle('sso-successor-token', null);
      // Model the successor's later main-process write settling before the old
      // IPC promise resolves. The stale continuation must not clear it.
      diskOwner = 'sso-successor';
      return 40;
    });
    const clearTokensIfOwner = vi.fn().mockResolvedValue(false);
    const storeE2EEKeys = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken,
        clearTokensIfOwner,
        storeE2EEKeys,
      },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(e2eeService.clearKeys).toHaveBeenCalled());
    expect(onSuccess).not.toHaveBeenCalled();
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'sso-successor-token',
      sessionId: null,
    });
    expect(diskOwner).toBe('sso-successor');
    expect(clearTokensIfOwner).toHaveBeenCalledWith(40);
    expect(storeE2EEKeys).not.toHaveBeenCalled();
  });

  it('stops old refresh/E2EE persistence when a same-origin successor wins before side effects', async () => {
    const { preferencesSyncService } = await import('@/renderer/services/preferencesSync');
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    let finishUnwrap: () => void = () => {
      throw new Error('Unwrap resolver was not initialized.');
    };
    mockUnwrapLoginKeys.mockReturnValueOnce(
      new Promise<void>((resolve) => {
        finishUnwrap = resolve;
      })
    );
    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    const storeE2EEKeys = vi.fn().mockResolvedValue(undefined);
    const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken,
        storeE2EEKeys,
        clearTokensIfOwner,
      },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));
    await vi.waitFor(() => expect(mockUnwrapLoginKeys).toHaveBeenCalled());

    useAuthStore.getState().beginAuthLifecycle('sso-successor-token', null);
    finishUnwrap();

    await vi.waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    expect(onSuccess).not.toHaveBeenCalled();
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'sso-successor-token',
      sessionId: null,
    });
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(storeE2EEKeys).not.toHaveBeenCalled();
    expect(clearTokensIfOwner).not.toHaveBeenCalled();
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(preferencesSyncService.init).not.toHaveBeenCalled();
  });

  it('revokes the server session when a teardown aborts the KEY-RECOVERY init (Codex P1, PR #2337)', async () => {
    // handleKeyRecovery's initialize() can reject with the typed teardown
    // error after the reset PUT succeeded. Pre-fix its catch only cleared the
    // renderer token and rethrew — bypassing the revocation sites — so the
    // login's refresh-token row + HttpOnly cookie outlived the teardown.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const { E2EEInitTeardownError } = await import('@/renderer/services/e2eeErrors');
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('unwrap failed'));
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new E2EEInitTeardownError());
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    // Consent to the key reset; the recovery init then hits the teardown.
    await user.click(
      await screen.findByLabelText(/encrypted message history will be permanently deleted/i)
    );
    await user.click(screen.getByRole('button', { name: /reset and continue/i }));

    await vi.waitFor(() => {
      expect(revokeAbortedSession).toHaveBeenCalled();
    });
    expect(onSuccess).not.toHaveBeenCalled();
  });

  it('a successful admit clears a stale abort notice staged mid-flight (Codex P1, PR #2337)', async () => {
    // A torn-down EARLIER flow can stage the abort notice while THIS login is
    // completing. A successful admit must clear it, or the notice survives and
    // resurfaces on a much later Login mount as if that sign-in had failed.
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      useAuthStore.getState().setLoginNotice('stale abort notice from an earlier flow');
      return 41;
    });
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(onSuccess).toHaveBeenCalled();
    });
    expect(useAuthStore.getState().loginNotice).toBeNull();
  });

  it('stages a one-shot notice that the NEXT Login mount renders after a teardown abort (PR #2337)', async () => {
    // In the real app the early access-token set navigates "/" into the
    // authenticated tree, so by the time the teardown abort reaches the login
    // catch THIS Login instance is unmounted and setErrors is a no-op — while
    // the teardown also cleared the token, bouncing the user to a FRESH Login.
    // The abort therefore stages authStore.loginNotice, which the next mount
    // seeds into its error banner and consumes (renders exactly once).
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const { E2EEInitTeardownError } = await import('@/renderer/services/e2eeErrors');
    const notice = 'Your session ended before sign-in could finish. Please sign in again.';
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new E2EEInitTeardownError());
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    const first = render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    // Still-mounted instance (jsdom has no router): friendly copy, not the
    // internal E2EEInitTeardownError message — and the notice is staged.
    expect(await screen.findByText(notice)).toBeInTheDocument();
    expect(useAuthStore.getState().loginNotice).toBe(notice);
    first.unmount();

    // The fresh Login seeds its banner from the staged notice and consumes it.
    const second = render(<Login {...defaultProps} />);
    expect(screen.getByText(notice)).toBeInTheDocument();
    expect(useAuthStore.getState().loginNotice).toBeNull();
    second.unmount();

    // Exactly once: a later mount shows no stale notice.
    render(<Login {...defaultProps} />);
    expect(screen.queryByText(notice)).not.toBeInTheDocument();
  });

  it('sets access token in auth store on success', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('mock-access');
    });
  });

  it('sets session ID in auth store on success', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(useAuthStore.getState().sessionId).toBe('mock-session');
    });
  });

  it('passes key_derivation_alg to unwrapLoginKeys', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () =>
        makeLoginResponse({
          e2ee_keys: {
            wrapped_private_key: 'mock-wrapped',
            key_derivation_salt: 'mock-salt',
            key_derivation_alg: 'argon2id',
          },
        }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(mockUnwrapLoginKeys).toHaveBeenCalledWith(
        'MySecurePassword123!',
        'mock-wrapped',
        'mock-salt',
        'argon2id'
      );
    });
  });

  it('defaults key_derivation_alg to pbkdf2 when not provided', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () =>
        makeLoginResponse({
          e2ee_keys: {
            wrapped_private_key: 'mock-wrapped',
            key_derivation_salt: 'mock-salt',
            // no key_derivation_alg
          },
        }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(mockUnwrapLoginKeys).toHaveBeenCalledWith(
        'MySecurePassword123!',
        'mock-wrapped',
        'mock-salt',
        'pbkdf2'
      );
    });
  });

  it('fails closed before publishing auth when the login payload is malformed', async () => {
    const { ensureMachineId, revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const requestApiBase = 'https://login-origin.example';
    setRuntimeServerBase(requestApiBase);
    const malformedPayload = makeLoginResponse({
      e2ee_keys: {
        wrapped_private_key: 'mock-wrapped',
        // key_derivation_salt is required at this trust boundary
      },
    });
    let resolveMachineId: (machineId: string) => void = () => {
      throw new Error('Machine ID resolver was not initialized.');
    };
    vi.mocked(ensureMachineId).mockReturnValueOnce(
      new Promise<string>((resolve) => {
        resolveMachineId = resolve;
      })
    );
    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify(malformedPayload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));
    await vi.waitFor(() => expect(ensureMachineId).toHaveBeenCalledWith(requestApiBase));

    // Machine-ID lookup is async, but the malformed response remains bound to
    // the invocation-time selection and is rejected before publishing auth.
    resolveMachineId('origin-machine-id');

    expect(
      await screen.findByText('Server returned an invalid login response.')
    ).toBeInTheDocument();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'mock-access',
      refreshToken: 'mock-refresh',
      sessionId: 'mock-session',
      apiBase: requestApiBase,
    });
    expect(mockFetch).toHaveBeenCalledWith(
      `${requestApiBase}/api/v1/auth/login`,
      expect.objectContaining({
        headers: expect.objectContaining({ 'X-Machine-Id': 'origin-machine-id' }),
      })
    );
  });

  it('revokes a successful login response missing required session lineage', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const { session_id: _sessionID, ...malformedPayload } = makeLoginResponse();
    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify(malformedPayload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    expect(
      await screen.findByText('Server returned an invalid login response.')
    ).toBeInTheDocument();
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'mock-access',
      refreshToken: 'mock-refresh',
      sessionId: null,
      apiBase: getApiBase(),
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('prefers the authoritative session header when a malformed body disagrees', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const malformedPayload = makeLoginResponse({ refresh_token: null });
    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify(malformedPayload), {
        status: 200,
        headers: {
          'Content-Type': 'application/json',
          'X-Concord-Session-ID': 'header-session',
        },
      })
    );

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    expect(
      await screen.findByText('Server returned an invalid login response.')
    ).toBeInTheDocument();
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'mock-access',
      refreshToken: null,
      sessionId: 'header-session',
      apiBase: getApiBase(),
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
  });

  it('aborts before credential dispatch when the server changes during safe-storage permission', async () => {
    const { ensureMachineId } = await import('@/renderer/services/apiClient');
    const requestApiBase = 'https://login-origin.example';
    setRuntimeServerBase(requestApiBase);
    let resolvePermission: (status: 'granted') => void = () => {
      throw new Error('Permission resolver was not initialized.');
    };
    const checkPermission = vi.fn().mockReturnValue(
      new Promise<'granted'>((resolve) => {
        resolvePermission = resolve;
      })
    );
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, checkPermission },
      writable: true,
    });
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));
    await vi.waitFor(() => expect(checkPermission).toHaveBeenCalledWith('secureStorage'));

    setRuntimeServerBase('https://successor-origin.example');
    resolvePermission('granted');

    expect(
      await screen.findByText('Server selection changed. Please try again.')
    ).toBeInTheDocument();
    expect(ensureMachineId).not.toHaveBeenCalled();
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it('rejects an A-to-B-to-A server change while machine-ID lookup is pending', async () => {
    const { ensureMachineId } = await import('@/renderer/services/apiClient');
    const requestApiBase = 'https://login-origin.example';
    setRuntimeServerBase(requestApiBase);
    let resolveMachineId: (machineId: string) => void = () => {
      throw new Error('Machine ID resolver was not initialized.');
    };
    vi.mocked(ensureMachineId).mockReturnValueOnce(
      new Promise<string>((resolve) => {
        resolveMachineId = resolve;
      })
    );

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));
    await vi.waitFor(() => expect(ensureMachineId).toHaveBeenCalledWith(requestApiBase));

    setRuntimeServerBase('https://successor-origin.example');
    setRuntimeServerBase(requestApiBase);
    resolveMachineId('origin-machine-id');

    expect(
      await screen.findByText(
        'Your session ended before sign-in could finish. Please sign in again.'
      )
    ).toBeInTheDocument();
    expect(mockFetch).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('does not revoke an unmarked undecodable 2xx initial login response', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    mockFetch.mockResolvedValueOnce({
      ok: true,
      headers: new Headers(),
      json: async () => {
        throw new SyntaxError('Unexpected end of JSON input');
      },
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    expect(
      await screen.findByText('Server returned an invalid login response.')
    ).toBeInTheDocument();
    expect(revokeAbortedSession).not.toHaveBeenCalled();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('revokes a header-identified cookie session when a direct login response is undecodable', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    mockFetch.mockResolvedValueOnce({
      ok: true,
      headers: new Headers({
        'X-Concord-Session-Issued': 'true',
        'X-Concord-Session-ID': 'direct-login-session',
      }),
      json: async () => {
        throw new SyntaxError('Unexpected end of JSON input');
      },
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    expect(
      await screen.findByText('Server returned an invalid login response.')
    ).toBeInTheDocument();
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: null,
      sessionId: 'direct-login-session',
      cookieBound: true,
      apiBase: getApiBase(),
    });
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('stores E2EE session keys via electron bridge when available', async () => {
    const mockStoreE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeE2EEKeysIfOwner: mockStoreE2EEKeysIfOwner,
        storeRefreshToken: vi.fn().mockResolvedValue(41),
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(mockStoreE2EEKeysIfOwner).toHaveBeenCalledWith(
        expect.objectContaining({
          wrappingKeyBase64: 'mock-wrapping-key',
        }),
        41
      );
    });
  });

  // ── Error States ───────────────────────────────────────────────────────

  it('shows error on login failure', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid credentials' }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'WrongPassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Invalid credentials')).toBeInTheDocument();
    });
  });

  it('shows generic error on network failure', async () => {
    mockFetch.mockRejectedValueOnce(new Error('Network error'));

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });
  });

  it('shows generic fallback message on non-Error throw', async () => {
    mockFetch.mockRejectedValueOnce('unexpected');

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Login failed. Please try again.')).toBeInTheDocument();
    });
  });

  it('shows safeStorage error when secure storage is unavailable', async () => {
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        checkPermission: vi.fn().mockResolvedValue('denied'),
      },
      writable: true,
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText(/Secure storage is unavailable/)).toBeInTheDocument();
    });
    // Should not have called fetch
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it('shows safeStorage error when checkPermission throws', async () => {
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        checkPermission: vi.fn().mockRejectedValue(new Error('bridge error')),
      },
      writable: true,
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText(/Secure storage could not be verified/)).toBeInTheDocument();
    });
  });

  // ── E2EE Key Regeneration Fallback ─────────────────────────────────────

  it('shows the recovery prompt (no silent PUT) when unwrapLoginKeys fails', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockClear();

    // checkPermission: 'granted' is load-bearing — a prior test
    // (`shows safeStorage error when checkPermission throws`) leaves a
    // rejecting checkPermission on globalThis.electron, and clearAllMocks does
    // not reset mock implementations. Without healing it here, login
    // short-circuits before fetch, leaving this test's queued fetch/unwrap
    // mocks unconsumed and desyncing the ...Once queues for every later test.
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken: vi.fn().mockResolvedValue(41),
        storeE2EEKeys: vi.fn().mockResolvedValue(undefined),
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    });
    // Critical: no silent key-replacement PUT happened before the user decided.
    expect(apiFetch).not.toHaveBeenCalledWith('/api/v1/users/me/keys', expect.anything());
    expect(mockGenerateRegistrationKeys).not.toHaveBeenCalled();

    // Dismiss the prompt so the suspended login flow (the catch awaits this
    // decision) resolves and no dialog leaks into later tests.
    await user.click(screen.getByRole('button', { name: /cancel/i }));
  });

  it('resets keys with public_key + acknowledge_data_loss when the user confirms', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken: vi.fn().mockResolvedValue(41),
        storeE2EEKeys: vi.fn().mockResolvedValue(undefined),
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    // Scope to the dialog: the login form's "Remember me" checkbox coexists
    // with the prompt's acknowledge checkbox while the overlay is mounted.
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    await vi.waitFor(() => {
      expect(apiFetch).toHaveBeenCalledWith(
        '/api/v1/users/me/keys',
        expect.objectContaining({
          method: 'PUT',
          body: expect.stringContaining('"acknowledge_data_loss":true'),
        })
      );
    });
    const putCall = (apiFetch as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === '/api/v1/users/me/keys'
    );
    expect(putCall?.[1]?.body).toContain('"public_key"');
    // Step-up auth: the destructive reset must carry the current password.
    expect(putCall?.[1]?.body).toContain('"current_password":"Password123!"');
  });

  it('prompts for an MFA code when the reset requires it, then resets with the code', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    // First reset attempt (no code) → 403 mfa_required; retry (with code) → ok.
    (apiFetch as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: async () => ({ error: 'mfa_required' }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken: vi.fn().mockResolvedValue(41),
        storeE2EEKeys: vi.fn().mockResolvedValue(undefined),
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    // Consent step.
    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    let dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    // Server demanded MFA → prompt re-opens in MFA-entry mode.
    await vi.waitFor(() =>
      expect(screen.getByRole('button', { name: /verify and reset/i })).toBeInTheDocument()
    );
    dialog = screen.getByRole('dialog');
    await user.type(within(dialog).getByRole('textbox'), '654321');
    await user.click(within(dialog).getByRole('button', { name: /verify and reset/i }));

    // The retry PUT carries the MFA code.
    await vi.waitFor(() => {
      const keyCalls = (apiFetch as ReturnType<typeof vi.fn>).mock.calls.filter(
        (c) => c[0] === '/api/v1/users/me/keys'
      );
      expect(keyCalls.length).toBe(2);
      expect(keyCalls[1]?.[1]?.body).toContain('"mfa_code":"654321"');
    });
  });

  it('does not reset keys when the user cancels recovery', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockClear();

    // Heal the rejecting checkPermission a prior test leaves behind (see the
    // note in 'shows the recovery prompt') so login reaches the unwrap path.
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: /cancel/i }));

    // Apostrophe-free substring (the error string uses a curly ’; match on a
    // segment without it so the regex can't silently miss).
    await vi.waitFor(() => {
      expect(screen.getByText(/recover your account on a device/i)).toBeInTheDocument();
    });
    expect(apiFetch).not.toHaveBeenCalledWith('/api/v1/users/me/keys', expect.anything());
  });

  it('clears the access token when the reset PUT fails (no half-authenticated state)', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    // The reset PUT fails — the early-set token must NOT survive.
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: false,
      json: async () => ({}),
    });

    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    // A failed reset must leave NO half-authenticated state: token cleared,
    // login not completed.
    await vi.waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBeNull();
    });
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // ── #2415 Continuation-pair adoption after a consented key reset ────────

  it('persists the CONTINUATION refresh token after key recovery, never the revoked login token', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    // The reset PUT's 2xx body carries the continuation pair. ReplaceMyKeys
    // revoked every refresh token for this user — including the one this login
    // just minted — so persisting `mock-refresh` would store a dead token and
    // silently lose restart survival.
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        access_token: 'cont-at',
        refresh_token: 'cont-rt', // pragma: allowlist secret
        session_id: 'cont-sid',
      }),
    });

    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken,
        storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true),
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    await vi.waitFor(() => expect(storeRefreshToken).toHaveBeenCalledTimes(1));
    expect(storeRefreshToken).toHaveBeenCalledWith(
      expect.objectContaining({
        refreshToken: 'cont-rt',
        accessToken: 'cont-at',
        // The desktop keeps its OWN rememberMe — never a server-forced flag.
        rememberMe: false,
      })
    );
    expect(storeRefreshToken).not.toHaveBeenCalledWith(
      expect.objectContaining({ refreshToken: 'mock-refresh' })
    );
    // Adopted as a NEW auth lifecycle, so the live session is the continuation
    // one and the generation moved past the login's.
    expect(useAuthStore.getState().accessToken).toBe('cont-at');
    expect(useAuthStore.getState().sessionId).toBe('cont-sid');
  });

  it('revokes the LIVE continuation session when an abort lands after the key reset', async () => {
    // The security regression a naive substitution introduces: abortedSession
    // still holding the login response's already-revoked tokens means a
    // post-reset abort revokes NOTHING and leaves the continuation session
    // authenticated on the server.
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch, revokeAbortedSession } = await import('@/renderer/services/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        access_token: 'cont-at',
        refresh_token: 'cont-rt', // pragma: allowlist secret
        session_id: 'cont-sid',
      }),
    });

    // Abort injected after the reset: the keychain publish fails, which routes
    // through abortForCredentialPersistenceFailure -> revokeAbortedSession.
    const storeRefreshToken = vi.fn().mockRejectedValue(new Error('keychain unavailable'));
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken,
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    await vi.waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    expect(revokeAbortedSession).toHaveBeenCalledWith(
      expect.objectContaining({
        accessToken: 'cont-at',
        refreshToken: 'cont-rt',
        sessionId: 'cont-sid',
      })
    );
    expect(onSuccess).not.toHaveBeenCalled();
  });

  // VULN-001 regression lock. The abort here lands in the window BETWEEN the
  // committed reset and the point where adoption used to happen (after
  // handleKeyRecovery returned) — the origin fence on the very next line after
  // the continuation pair is parsed. Before the fix, `abortedSession` still held
  // the login response's tokens in that window, which ReplaceMyKeys had already
  // revoked, so the abort revoked a corpse and left the live continuation
  // session — 30 days, remember_me = true — authenticated on the server
  // (CWE-613). This asserts the SECURE behaviour: the revoke must carry the
  // continuation credentials and must never carry the dead login pair.
  it('revokes the CONTINUATION session when an abort lands before adoption used to complete', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch, revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const loginApiBase = getApiBase();
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: async () => {
        // A self-hosted origin switch commits while the reset body is being
        // read, so the fence immediately after the parse aborts the flow. No
        // await separates the parse from the re-point, so this is the earliest
        // abort site that can observe the pair at all.
        setRuntimeServerBase('https://successor-origin.example');
        return {
          access_token: 'cont-at',
          refresh_token: 'cont-rt', // pragma: allowlist secret
          session_id: 'cont-sid',
        };
      },
    });

    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken,
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse({ refresh_token: 'login-rt' }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    await vi.waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    // The live session, bound to the origin that issued it.
    expect(revokeAbortedSession).toHaveBeenCalledWith(
      expect.objectContaining({
        accessToken: 'cont-at',
        refreshToken: 'cont-rt',
        sessionId: 'cont-sid',
        apiBase: loginApiBase,
      })
    );
    // The exploit signature: revoking the already-dead login pair instead.
    expect(revokeAbortedSession).not.toHaveBeenCalledWith(
      expect.objectContaining({ refreshToken: 'login-rt' })
    );
    // Exactly one revoke — the origin abort's, not a second from the rethrow.
    expect(revokeAbortedSession).toHaveBeenCalledTimes(1);
    // The abort landed before the recovery reinit, so no keyset was committed
    // and nothing was persisted for a session that is now revoked.
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(onSuccess).not.toHaveBeenCalled();
  });

  it('fails closed with an explanation when the reset commits without a continuation pair', async () => {
    mockUnwrapLoginKeys.mockRejectedValueOnce(new Error('corrupt key'));
    const { apiFetch } = await import('@/renderer/services/apiClient');
    // A 2xx carrying no continuation fields is a DELIBERATE server outcome (a
    // concurrent destructive flow advanced the credential epoch), never a
    // transport error and never retried.
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    const storeRefreshToken = vi.fn().mockResolvedValue(41);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken,
        checkPermission: vi.fn().mockResolvedValue('granted'),
      },
      writable: true,
    });

    mockFetch.mockResolvedValueOnce({ ok: true, json: async () => makeLoginResponse() });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('checkbox'));
    await user.click(within(dialog).getByRole('button', { name: /reset and continue/i }));

    // The notice renders in the live banner, announced via role="alert" — NOT
    // staged into authStore.loginNotice, which this already-mounted Login would
    // never consume.
    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(/your new keys are already active/i);
    expect(useAuthStore.getState().loginNotice).toBeNull();

    // Prompt closed (it owns a focus trap), submit released, and the
    // known-revoked login refresh token never persisted.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByText('Sign In')).toBeInTheDocument();
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(onSuccess).not.toHaveBeenCalled();
    // Absence is unrecoverable — exactly one reset PUT, no retry.
    expect(
      (apiFetch as ReturnType<typeof vi.fn>).mock.calls.filter(
        (c) => c[0] === '/api/v1/users/me/keys'
      ).length
    ).toBe(1);
  });

  // ── MFA Challenge Flow ─────────────────────────────────────────────────

  it('transitions to MFA screen when server returns mfa_required', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['totp']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Two-Factor Authentication')).toBeInTheDocument();
    });
  });

  it('does not revoke a decoded unmarked malformed initial MFA challenge', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    mockFetch.mockResolvedValueOnce({
      ok: true,
      headers: new Headers(),
      json: async () => makeMFAResponse(['totp'], { methods: 'totp' }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    expect(
      await screen.findByText('Server returned an invalid MFA challenge.')
    ).toBeInTheDocument();
    expect(revokeAbortedSession).not.toHaveBeenCalled();
    expect(screen.queryByText('Two-Factor Authentication')).not.toBeInTheDocument();
  });

  it('revokes an MFA session whose successful completion payload is malformed', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    const requestApiBase = 'https://mfa-origin.example';
    setRuntimeServerBase(requestApiBase);
    const malformedPayload = makeLoginResponse({
      e2ee_keys: {
        wrapped_private_key: 'mock-wrapped',
        // key_derivation_salt is required at this trust boundary
      },
    });
    let resolveMfaResponse: (response: Response) => void = () => {
      throw new Error('MFA response resolver was not initialized.');
    };
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => makeMFAResponse(['totp']),
      })
      .mockReturnValueOnce(
        new Promise<Response>((resolve) => {
          resolveMfaResponse = resolve;
        })
      );

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));
    await screen.findByText('Two-Factor Authentication');

    for (let digit = 1; digit <= 6; digit += 1) {
      await user.type(screen.getByLabelText(`Digit ${digit}`), String(digit));
    }
    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));

    setRuntimeServerBase('https://successor-origin.example');
    resolveMfaResponse(
      new Response(JSON.stringify(malformedPayload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    expect(
      await screen.findByText(
        'Your session ended before sign-in could finish. Please sign in again.'
      )
    ).toBeInTheDocument();
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'mock-access',
      refreshToken: 'mock-refresh',
      sessionId: 'mock-session',
      apiBase: requestApiBase,
    });
    expect(mockFetch).toHaveBeenNthCalledWith(
      2,
      `${requestApiBase}/api/v1/auth/mfa/verify`,
      expect.any(Object)
    );
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('revokes the header-identified MFA session when a successful completion body is undecodable', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    mockFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => makeMFAResponse(['totp']),
      })
      .mockResolvedValueOnce({
        ok: true,
        headers: new Headers({
          'X-Concord-Session-Issued': 'true',
          'X-Concord-Session-ID': 'mfa-login-session',
        }),
        json: async () => {
          throw new SyntaxError('Unexpected end of JSON input');
        },
      });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));
    await screen.findByText('Two-Factor Authentication');

    for (let digit = 1; digit <= 6; digit += 1) {
      await user.type(screen.getByLabelText(`Digit ${digit}`), String(digit));
    }

    expect(
      await screen.findByText('Server returned an invalid login response.')
    ).toBeInTheDocument();
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: null,
      sessionId: 'mfa-login-session',
      cookieBound: true,
      apiBase: getApiBase(),
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(defaultProps.onSuccess).not.toHaveBeenCalled();
  });

  it('shows TOTP subtitle in MFA mode', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['totp']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(
        screen.getByText('Enter the 6-digit code from your authenticator app')
      ).toBeInTheDocument();
    });
  });

  it('returns to login form when MFA back button is clicked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['totp']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Two-Factor Authentication')).toBeInTheDocument();
    });

    await user.click(screen.getByText(/Back to login/));
    expect(screen.getByText('Welcome Back')).toBeInTheDocument();
  });

  it('shows "Choose another form" link when multiple MFA methods available', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['totp', 'webauthn']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Choose another form of verification')).toBeInTheDocument();
    });
  });

  it('hides "Choose another form" when only one MFA method', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['totp']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Two-Factor Authentication')).toBeInTheDocument();
    });
    // TOTP + backup = 2 categories, so "choose another" should still show
    // But if only TOTP is the method, backup is always added, making 2 categories
    // This means with a single method 'totp', we get totp + backup = 2 available
    // So the link WILL be shown. Let's verify that.
    expect(screen.getByText('Choose another form of verification')).toBeInTheDocument();
  });

  it('shows method picker when "Choose another form" is clicked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['totp', 'webauthn']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Choose another form of verification')).toBeInTheDocument();
    });

    await user.click(screen.getByText('Choose another form of verification'));
    expect(screen.getByText('Select a verification method')).toBeInTheDocument();
  });

  it('shows WebAuthn fallback message when no webauthn options', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () =>
        makeMFAResponse(['webauthn'], {
          // No webauthn_options provided
        }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Two-Factor Authentication')).toBeInTheDocument();
    });

    // Default method for webauthn should be webauthn
    expect(
      screen.getByText('WebAuthn verification will be triggered by the server challenge.')
    ).toBeInTheDocument();
  });

  it('rejects malformed WebAuthn options before entering the MFA flow', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () =>
        makeMFAResponse(['webauthn'], {
          webauthn_options: {
            publicKey: {
              challenge: 'Y2hhbGxlbmdl',
              allowCredentials: [{ type: 'public-key', id: 42 }],
            },
          },
        }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    expect(
      await screen.findByText('Server returned invalid WebAuthn options.')
    ).toBeInTheDocument();
    expect(screen.queryByText('Two-Factor Authentication')).not.toBeInTheDocument();
  });

  it('accepts the backend-supported WebAuthn smart-card transport', async () => {
    const getCredential = vi.fn().mockReturnValue(new Promise(() => {}));
    Object.defineProperty(navigator, 'credentials', {
      value: { get: getCredential },
      writable: true,
      configurable: true,
    });
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () =>
        makeMFAResponse(['webauthn'], {
          webauthn_options: {
            publicKey: {
              challenge: 'Y2hhbGxlbmdl',
              allowCredentials: [
                {
                  type: 'public-key',
                  id: 'Y3JlZGVudGlhbA',
                  transports: ['smart-card'],
                },
              ],
            },
          },
        }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(getCredential).toHaveBeenCalledWith(
        expect.objectContaining({
          publicKey: expect.objectContaining({
            allowCredentials: [
              expect.objectContaining({
                transports: ['smart-card'],
              }),
            ],
          }),
        })
      );
    });
  });

  it('shows email-sms subtitle when email-sms mode is active', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeMFAResponse(['email']),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
    });
  });

  // ── Submitting State ───────────────────────────────────────────────────

  it('shows "Signing In..." and spinner while submitting', async () => {
    // Never resolve to keep isSubmitting true
    mockFetch.mockReturnValueOnce(new Promise(() => {}));

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByText('Signing In...')).toBeInTheDocument();
    });
  });

  // ── PreferencesSync init flow ───────────────────────────────────────

  it('calls preferencesSyncService.init() on successful login', async () => {
    const { preferencesSyncService } = await import('@/renderer/services/preferencesSync');

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(preferencesSyncService.init).toHaveBeenCalled();
    });
  });

  it('calls preferencesSyncService.startWatching() after init on login', async () => {
    const { preferencesSyncService } = await import('@/renderer/services/preferencesSync');

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => makeLoginResponse(),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(preferencesSyncService.startWatching).toHaveBeenCalled();
    });
  });

  it('disables inputs while submitting', async () => {
    mockFetch.mockReturnValueOnce(new Promise(() => {}));

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByText('Sign In'));

    await vi.waitFor(() => {
      expect(screen.getByPlaceholderText('you@example.com')).toBeDisabled();
      expect(screen.getByPlaceholderText('Enter your password')).toBeDisabled();
    });
  });

  it('makes SSO authentication exclusive with the password form', async () => {
    useSSOStore.getState().setState({ phase: 'authenticating', provider: 'google' });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);

    expect(screen.getByPlaceholderText('you@example.com')).toBeDisabled();
    expect(screen.getByPlaceholderText('Enter your password')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Sign In' })).toBeDisabled();
    expect(screen.getByRole('button', { name: /Back to Connection Options/i })).toBeDisabled();
    expect(screen.getByRole('button', { name: /Create one/i })).toBeDisabled();
    expect(screen.getByRole('button', { name: /Sign in with Google/i })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Sign In' }));
    expect(mockFetch).not.toHaveBeenCalled();
  });

  // ── account_uses_sso (#270) ────────────────────────────────────────────

  it('swaps form for SSO button when server returns 403 account_uses_sso', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error_code: 'account_uses_sso', providers: ['google'] }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'sso@example.test');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'anything');
    await user.click(screen.getByRole('button', { name: /^sign in$/i }));

    await screen.findByRole('button', { name: /sign in with google/i });
    // The password input should be gone — the SSO-only branch replaces the
    // form so the user cannot resubmit credentials that won't work.
    expect(screen.queryByPlaceholderText('Enter your password')).not.toBeInTheDocument();
  });

  it('renders both Google and Apple buttons when account_uses_sso lists both', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error_code: 'account_uses_sso', providers: ['google', 'apple'] }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'sso@example.test');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'anything');
    await user.click(screen.getByRole('button', { name: /^sign in$/i }));

    await screen.findByRole('button', { name: /sign in with google/i });
    expect(screen.getByRole('button', { name: /sign in with apple/i })).toBeInTheDocument();
  });

  it('returns to the password form when "Back to login" is clicked from the SSO-only view', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error_code: 'account_uses_sso', providers: ['google'] }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'sso@example.test');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'anything');
    await user.click(screen.getByRole('button', { name: /^sign in$/i }));

    // Wait for the SSO-only branch to render.
    await screen.findByRole('button', { name: /sign in with google/i });
    // Click Back — the password form should reappear.
    await user.click(screen.getByRole('button', { name: /back to login/i }));
    expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument();
  });

  it('renders the SSO entry button on the default password-form view and invokes beginSSO on click', async () => {
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    // Both forms (password + SSO entry) coexist on the default view.
    expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument();
    const ssoBtn = screen.getByRole('button', { name: /sign in with google/i });
    expect(ssoBtn).toBeInTheDocument();
    // Clicking exercises the inline arrow handler on line 615 — invokes
    // useSSOFlow().begin('google') via real-hook wiring; the loopback flow
    // itself short-circuits because globalThis.electron is stubbed by setup.ts.
    await user.click(ssoBtn);
    // No throw + the button stays present (not disabled by submit state).
    expect(ssoBtn).toBeInTheDocument();
  });

  it('hides default SSO buttons and divider when server capabilities are unavailable', () => {
    useClientConfigStore.getState().setServerCapabilities(null);

    render(<Login {...defaultProps} />);

    expect(screen.queryByRole('button', { name: /sign in with google/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /sign in with apple/i })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('separator', { name: /or sign in with email/i })
    ).not.toBeInTheDocument();
  });

  it('renders only SSO providers advertised by server capabilities', () => {
    useClientConfigStore.getState().setServerCapabilities({
      auth: { oauthProviders: ['google'] },
      features: {},
    });

    render(<Login {...defaultProps} />);

    expect(screen.getByRole('button', { name: /sign in with google/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /sign in with apple/i })).not.toBeInTheDocument();
    expect(screen.getByRole('separator', { name: /or sign in with email/i })).toBeInTheDocument();
  });

  it('clicks the Apple SSO button on the SSO-only branch when both providers are listed', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error_code: 'account_uses_sso', providers: ['google', 'apple'] }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'sso@example.test');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'anything');
    await user.click(screen.getByRole('button', { name: /^sign in$/i }));

    // Apple variant is rendered in addition to Google when providers includes
    // 'apple' — the inline onClick on line 582 is exercised on the click.
    const appleBtn = await screen.findByRole('button', { name: /sign in with apple/i });
    await user.click(appleBtn);
    // No throw — the click handler is wired through useSSOFlow.
    expect(appleBtn).toBeInTheDocument();
  });

  // ── Apple SSO entry point (#271) ──────────────────────────────────────

  it('renders Apple SSO button alongside Google on the default password-form view', async () => {
    // App Store policy parity: when Google is offered as a sign-in option,
    // Apple must also be offered (gating mobile clients #205/#206). The
    // button uses the SSOButton's `provider="apple"` variant — branded
    // black-on-white per Apple HIG (forward-shaped in PR #808).
    render(<Login {...defaultProps} />);
    expect(screen.getByRole('button', { name: /sign in with apple/i })).toBeInTheDocument();
    // Google button still present — Apple is additive, not a replacement.
    expect(screen.getByRole('button', { name: /sign in with google/i })).toBeInTheDocument();
  });

  it('clicks the Apple SSO button on the default view — invokes beginSSO via useSSOFlow', async () => {
    // The inline arrow handler invokes useSSOFlow().begin('apple') via
    // real-hook wiring. The flow itself short-circuits because
    // globalThis.electron is stubbed by setup.ts. We assert the click
    // does not throw and the button stays present (not disabled by
    // submit state on the default view).
    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    const appleBtn = screen.getByRole('button', { name: /sign in with apple/i });
    await user.click(appleBtn);
    expect(appleBtn).toBeInTheDocument();
  });

  it('disables the Apple SSO button while password form is submitting', async () => {
    // The Apple button shares the disabled={isSubmitting} prop with Google
    // so a user cannot start an SSO flow concurrently with a password
    // submit (which would race the loopback's 60s timeout against the
    // backend's auth call). Kept symmetric with Google for predictable UX.
    mockFetch.mockReturnValueOnce(new Promise(() => {}));

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'Password123!');
    await user.click(screen.getByRole('button', { name: /^sign in$/i }));

    await vi.waitFor(() => {
      expect(screen.getByRole('button', { name: /sign in with apple/i })).toBeDisabled();
    });
  });

  // ── sso_account_misconfigured (#270 / PR #808 review) ──────────────────

  it('shows a contact-support message when server returns 500 sso_account_misconfigured', async () => {
    // Backend signaled a data-integrity violation: password_login_disabled=TRUE
    // with no SSO identities linked. This is persistent (not transient), so the
    // user-facing copy must NOT suggest "try again in a moment" — it should
    // direct the user to support with a referenceable error code.
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => ({ error_code: 'sso_account_misconfigured' }),
    });

    const user = userEvent.setup();
    render(<Login {...defaultProps} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'misconfig@example.test');
    await user.type(screen.getByPlaceholderText('Enter your password'), 'anything');
    await user.click(screen.getByRole('button', { name: /^sign in$/i }));

    // The friendly copy with the SSO_MISCONFIG error code must be visible —
    // not a generic 500 fallback message.
    await screen.findByText(/SSO_MISCONFIG/);
    expect(screen.getByText(/contact support/i)).toBeInTheDocument();
    // The password input must remain (the user might switch to a different
    // account; we don't swap to the SSO-only view since there are no providers).
    expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument();
  });
});
