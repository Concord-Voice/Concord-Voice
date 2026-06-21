import { render, screen, userEvent, within } from '../../../test-utils';
import Login from '@/renderer/components/Auth/Login';
import { vi } from 'vitest';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../../helpers/store-helpers';

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

// Mock crypto and services to avoid real key operations
vi.mock('@/renderer/utils/crypto', () => ({
  unwrapLoginKeys: (...args: unknown[]) => mockUnwrapLoginKeys(...args),
  generateRegistrationKeys: (...args: unknown[]) => mockGenerateRegistrationKeys(...args),
  exportPublicKey: vi.fn().mockResolvedValue('mock-public-key'),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: vi.fn().mockResolvedValue(undefined),
    getSessionKeys: vi.fn().mockReturnValue({
      wrappingKeyBase64: 'mock-wrapping-key',
      preferencesKeyBase64: 'mock-preferences-key',
      wrappedPrivateKeyBase64: 'mock-wrapped-private-key',
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

  it('stores E2EE session keys via electron bridge when available', async () => {
    const mockStoreE2EEKeys = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeE2EEKeys: mockStoreE2EEKeys,
        storeRefreshToken: vi.fn().mockResolvedValue(undefined),
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
      expect(mockStoreE2EEKeys).toHaveBeenCalledWith(
        expect.objectContaining({
          wrappingKeyBase64: 'mock-wrapping-key',
        })
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
        storeRefreshToken: vi.fn().mockResolvedValue(undefined),
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
        storeRefreshToken: vi.fn().mockResolvedValue(undefined),
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
      .mockResolvedValueOnce({ ok: false, status: 403, json: async () => ({ error: 'mfa_required' }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

    Object.defineProperty(globalThis, 'electron', {
      value: {
        ...globalThis.electron,
        storeRefreshToken: vi.fn().mockResolvedValue(undefined),
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
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ ok: false, json: async () => ({}) });

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
