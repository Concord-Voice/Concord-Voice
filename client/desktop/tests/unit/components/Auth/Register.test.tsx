import { render, screen, userEvent } from '../../../test-utils';
import Register from '@/renderer/components/Auth/Register';
import { vi } from 'vitest';
import { usePendingRegistrationStore } from '@/renderer/stores/pendingRegistrationStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Mock crypto
vi.mock('@/renderer/utils/crypto', () => ({
  generateRegistrationKeys: vi.fn().mockResolvedValue({
    wrappedPrivateKey: 'mock',
    keyDerivationSalt: 'mock',
    keyDerivationAlg: 'argon2id',
    publicKey: {},
  }),
  exportPublicKey: vi.fn().mockResolvedValue('mock-public-key'),
}));

// Mock e2eeService — Register must initialize it with the generated keys on a
// successful registration so a new user has E2EE ready at /app without a
// separate login (otherwise channel creation hits "Setting up secure messaging"
// until logout→login). See #1274.
//
// Stateful (per #1278 review): initialize() flips `initialized` on, clearKeys()
// off, and getSessionKeys() returns keys ONLY while initialized — mirroring the
// real service so the failure-path tests can't false-pass on a post-clearKeys
// persist (getSessionKeys → null after clearKeys, so the persist block no-ops).
// `e2eeState` is reset per test in beforeEach.
const e2eeState = vi.hoisted(() => ({ initialized: false }));
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: vi.fn(async () => {
      e2eeState.initialized = true;
    }),
    getSessionKeys: vi.fn(() =>
      e2eeState.initialized
        ? {
            wrappingKeyBase64: 'wk',
            preferencesKeyBase64: 'pk',
            wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
          }
        : null
    ),
    clearKeys: vi.fn(() => {
      e2eeState.initialized = false;
    }),
  },
}));

// Mock useSSOFlow so we can assert the SSOButton wiring without exercising
// the full loopback flow. The actual flow is covered in ssoService.test.ts.
const beginSSOMock = vi.fn();
vi.mock('@/renderer/hooks/useSSOFlow', () => ({
  useSSOFlow: () => ({ begin: beginSSOMock }),
}));

describe('Register', () => {
  const onBack = vi.fn();
  const onSuccess = vi.fn();
  const onSwitchToLogin = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    e2eeState.initialized = false;
    resetAllStores();
    usePendingRegistrationStore.getState().clearPending();
  });

  afterEach(() => {
    // Tests that exercise E2EE persistence override window.electron.storeE2EEKeys
    // on the shared preload mock (tests/setup.ts). Remove the override so it
    // can't leak into later tests and make them order-dependent. (#1278 review.)
    delete (window.electron as unknown as { storeE2EEKeys?: unknown }).storeE2EEKeys;
  });

  it('renders registration form', () => {
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    expect(screen.getByText('Create Your Account')).toBeInTheDocument();
    expect(screen.getByText(/Join the Concord Voice network/)).toBeInTheDocument();
  });

  it('renders all form fields', () => {
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    expect(screen.getByPlaceholderText('you@example.com')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('your_username')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Create a strong password')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Confirm your password')).toBeInTheDocument();
  });

  it('renders age confirmation checkbox', () => {
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    expect(screen.getByText(/at least 16 years old/)).toBeInTheDocument();
  });

  it('renders E2EE info banner', () => {
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    expect(screen.getByText('End-to-End Encryption Enabled')).toBeInTheDocument();
  });

  it('shows email validation error', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.click(screen.getByText('Create Account'));
    expect(screen.getByText('Email is required')).toBeInTheDocument();
  });

  it('shows username validation error', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.click(screen.getByText('Create Account'));
    expect(screen.getByText('Username is required')).toBeInTheDocument();
  });

  it('shows password validation error for short password', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(screen.getByPlaceholderText('Create a strong password'), 'short');
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'short');
    await user.click(screen.getByText('Create Account'));
    expect(screen.getByText('Password must be at least 12 characters')).toBeInTheDocument();
  });

  it('shows password mismatch error', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'DifferentPassword456!');
    await user.click(screen.getByText('Create Account'));
    expect(screen.getByText('Passwords do not match')).toBeInTheDocument();
  });

  it('shows age confirmation error', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'MySecurePassword123!');
    await user.click(screen.getByText('Create Account'));
    expect(screen.getByText(/must confirm you are at least 16/)).toBeInTheDocument();
  });

  it('shows username hint with identity', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('your_username'), 'myname');
    expect(screen.getByText(/myname@concordvoice\.chat/)).toBeInTheDocument();
  });

  it('calls onBack when back button is clicked', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.click(screen.getByText(/Back to Connection Options/));
    expect(onBack).toHaveBeenCalled();
  });

  it('calls onSwitchToLogin when sign in is clicked', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.click(screen.getByText('Sign in'));
    expect(onSwitchToLogin).toHaveBeenCalled();
  });

  it('renders terms text', () => {
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    expect(screen.getByText(/Terms of Service and Privacy Policy/)).toBeInTheDocument();
  });

  it('on 201 response, writes pending registration to store', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      json: async () => ({
        pending_id: 'mock-pending-id',
        email: 'test@example.com',
        expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
        code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
      }),
    });

    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'MySecurePassword123!');
    await user.click(screen.getByText(/at least 16 years old/));
    await user.click(screen.getByText('Create Account'));

    await vi.waitFor(() => {
      expect(usePendingRegistrationStore.getState().pendingId).toBe('mock-pending-id');
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(onSuccess).toHaveBeenCalledWith({
      pendingId: 'mock-pending-id',
      email: 'test@example.com',
    });
  });

  it('initializes e2eeService with the generated keys on successful registration', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      json: async () => ({
        pending_id: 'mock-pending-id',
        email: 'test@example.com',
        expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
        code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
      }),
    });

    // storeE2EEKeys is not in the default window.electron mock; add a spy so we
    // can assert the session keys are persisted to the OS keychain.
    const storeE2EEKeysMock = vi.fn().mockResolvedValue(undefined);
    (window.electron as unknown as { storeE2EEKeys: typeof storeE2EEKeysMock }).storeE2EEKeys =
      storeE2EEKeysMock;

    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'MySecurePassword123!');
    await user.click(screen.getByText(/at least 16 years old/));
    await user.click(screen.getByText('Create Account'));

    // Mocked crypto returns wrappedPrivateKey/salt='mock', alg='argon2id'; the
    // password typed is 'MySecurePassword123!'. Initializing here is what lets a
    // freshly-registered user create channels / message without a re-login.
    await vi.waitFor(() => {
      expect(e2eeService.initialize).toHaveBeenCalledWith(
        'MySecurePassword123!',
        'mock',
        'mock',
        'argon2id'
      );
    });

    // Session keys must be persisted to the OS keychain (mirrors Login.tsx) so
    // E2EE survives an app restart — otherwise the gate returns on next launch.
    expect(storeE2EEKeysMock).toHaveBeenCalledWith({
      wrappingKeyBase64: 'wk',
      preferencesKeyBase64: 'pk',
      wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
    });

    // Registration still completes, and init must precede navigation (onSuccess)
    // so E2EE is ready by the time the user reaches /app.
    expect(onSuccess).toHaveBeenCalledWith({
      pendingId: 'mock-pending-id',
      email: 'test@example.com',
    });
    const initOrder = vi.mocked(e2eeService.initialize).mock.invocationCallOrder[0];
    const onSuccessOrder = onSuccess.mock.invocationCallOrder[0];
    expect(initOrder).toBeLessThan(onSuccessOrder);
  });

  it('completes registration even if e2eeService init fails (non-fatal)', async () => {
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new Error('init boom'));
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      json: async () => ({
        pending_id: 'mock-pending-id',
        email: 'test@example.com',
        expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
        code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
      }),
    });

    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'MySecurePassword123!');
    await user.click(screen.getByText(/at least 16 years old/));
    await user.click(screen.getByText('Create Account'));

    // A failed E2EE pre-init must NOT break an otherwise-successful registration
    // — the user falls back to the prior behavior (secure messaging needs a
    // manual logout→login). The catch must also clearKeys() to roll back any
    // partial init so isInitialized is honestly false (not a half-init singleton).
    await vi.waitFor(() => {
      expect(onSuccess).toHaveBeenCalledWith({
        pendingId: 'mock-pending-id',
        email: 'test@example.com',
      });
    });
    expect(usePendingRegistrationStore.getState().pendingId).toBe('mock-pending-id');
    expect(e2eeService.clearKeys).toHaveBeenCalled();
  });

  it('keeps the in-memory E2EE session when only keychain persistence fails', async () => {
    // initialize() succeeds but storeE2EEKeys() throws (e.g. keychain locked).
    // clearKeys() must NOT be called — destroying the valid in-memory session
    // over a persistence failure would lose E2EE for the current session, not
    // just on restart (#1278 review, Gitar finding).
    const storeE2EEKeysMock = vi.fn().mockRejectedValue(new Error('keychain locked'));
    (window.electron as unknown as { storeE2EEKeys: typeof storeE2EEKeysMock }).storeE2EEKeys =
      storeE2EEKeysMock;
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      json: async () => ({
        pending_id: 'mock-pending-id',
        email: 'test@example.com',
        expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
        code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
      }),
    });

    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'MySecurePassword123!');
    await user.click(screen.getByText(/at least 16 years old/));
    await user.click(screen.getByText('Create Account'));

    await vi.waitFor(() => {
      expect(storeE2EEKeysMock).toHaveBeenCalled();
    });
    expect(onSuccess).toHaveBeenCalledWith({
      pendingId: 'mock-pending-id',
      email: 'test@example.com',
    });
    // The persistence failure must leave the in-memory session intact.
    expect(e2eeService.clearKeys).not.toHaveBeenCalled();
  });

  it('shows error on registration failure', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Email already registered' }),
    });

    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.type(screen.getByPlaceholderText('you@example.com'), 'test@example.com');
    await user.type(screen.getByPlaceholderText('your_username'), 'testuser');
    await user.type(
      screen.getByPlaceholderText('Create a strong password'),
      'MySecurePassword123!'
    );
    await user.type(screen.getByPlaceholderText('Confirm your password'), 'MySecurePassword123!');
    await user.click(screen.getByText(/at least 16 years old/));
    await user.click(screen.getByText('Create Account'));

    await vi.waitFor(() => {
      expect(screen.getByText('Email already registered')).toBeInTheDocument();
    });
  });

  it('clears field error when user starts typing', async () => {
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    await user.click(screen.getByText('Create Account'));
    expect(screen.getByText('Email is required')).toBeInTheDocument();
    await user.type(screen.getByPlaceholderText('you@example.com'), 't');
    expect(screen.queryByText('Email is required')).not.toBeInTheDocument();
  });

  // ── SSO entry point (#270) ────────────────────────────────────────────
  // The Register form mounts an SSOButton above the email/password form.
  // Clicking it must invoke useSSOFlow().begin('google') so the loopback
  // OAuth flow starts. We mock useSSOFlow at the module level (above) to
  // assert the wiring without exercising the loopback round-trip itself
  // (covered in ssoService.test.ts).

  it('calls useSSOFlow().begin("google") when the Sign in with Google button is clicked', async () => {
    beginSSOMock.mockClear();
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);

    await user.click(screen.getByRole('button', { name: /sign in with google/i }));
    expect(beginSSOMock).toHaveBeenCalledWith('google');
  });

  // ── Apple SSO entry point (#271) ──────────────────────────────────────

  it('renders the Sign in with Apple button alongside Google on the registration form', () => {
    // App Store policy parity: when Google sign-in is offered, Apple must
    // also be offered. Both buttons live in the same .register-sso-row above
    // the email/password form. SSOButton's `provider="apple"` variant
    // renders the Apple logo + "Sign in with Apple" label per Apple HIG
    // (forward-shaped in PR #808).
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);
    expect(screen.getByRole('button', { name: /sign in with apple/i })).toBeInTheDocument();
    // Google still rendered — Apple is additive, not a replacement.
    expect(screen.getByRole('button', { name: /sign in with google/i })).toBeInTheDocument();
  });

  it('calls useSSOFlow().begin("apple") when the Sign in with Apple button is clicked', async () => {
    // Mirror of the Google-button test above. The Apple click handler is
    // the same inline arrow shape — `() => beginSSO('apple')` — so we
    // assert the begin mock is called with the correct provider literal.
    beginSSOMock.mockClear();
    const user = userEvent.setup();
    render(<Register onBack={onBack} onSuccess={onSuccess} onSwitchToLogin={onSwitchToLogin} />);

    await user.click(screen.getByRole('button', { name: /sign in with apple/i }));
    expect(beginSSOMock).toHaveBeenCalledWith('apple');
  });
});
