import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import SSOPassphraseSetup from '@/renderer/components/Auth/SSOPassphraseSetup';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch (matches Register.test.tsx pattern)
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

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

beforeEach(() => {
  vi.clearAllMocks();
  e2eeState.initialized = false;
  resetAllStores();
  useSSOStore.getState().setState({
    phase: 'register_required',
    provider: 'google',
    ssoToken: 'tok-fake',
    email: 'new@example.test',
    name: 'New User',
  });
});

afterEach(() => {
  // E2EE-persistence tests override window.electron.storeE2EEKeys on the shared
  // preload mock (tests/setup.ts). Remove it so it can't leak into later tests and
  // make them order-dependent. (Mirrors Register.test.tsx, #1278 review.)
  delete (window.electron as unknown as { storeE2EEKeys?: unknown }).storeE2EEKeys;
});

describe('SSOPassphraseSetup', () => {
  it('shows the email and asks for a passphrase', () => {
    render(<SSOPassphraseSetup />);
    expect(screen.getByText('new@example.test')).toBeInTheDocument();
    expect(screen.getByLabelText(/^passphrase$/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/confirm passphrase/i)).toBeInTheDocument();
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

  it('posts complete-registration with wrapped key material on submit', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ access_token: 'access-xyz' }),
      text: async () => JSON.stringify({ access_token: 'access-xyz' }),
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

    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

    const [url, init] = mockFetch.mock.calls[0];
    expect(String(url)).toContain('/api/v1/auth/sso/google/complete-registration');
    expect(init?.method).toBe('POST');

    const body = JSON.parse(init.body as string);
    expect(body.sso_token).toBe('tok-fake');
    expect(body.username).toBe('newcomer');
    expect(body.password).toBe('StrongPassphrase!12345');
    // Mocked base64 strings
    expect(body.wrapped_private_key).toMatch(/^[A-Za-z0-9+/=]+$/);
    expect(body.public_key).toMatch(/^[A-Za-z0-9+/=]+$/);
    expect(body.key_derivation_salt).toMatch(/^[A-Za-z0-9+/=]+$/);

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    });
    expect(useSSOStore.getState().state.phase).toBe('idle');
  });

  it('renders nothing when phase is not register_required (defensive)', () => {
    useSSOStore.getState().reset();
    const { container } = render(<SSOPassphraseSetup />);
    expect(container).toBeEmptyDOMElement();
  });

  it('surfaces username_taken 409 with a friendly inline message', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 409,
      // ssoService now parses the body via safeJson, which requires a JSON
      // content-type header to succeed. Include it so the error_code reaches
      // the SSOPassphraseSetup mapper.
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ error_code: 'username_taken' }),
      text: async () => JSON.stringify({ error_code: 'username_taken' }),
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

  const okRegistrationResponse = () => ({
    ok: true,
    status: 201,
    headers: new Headers({ 'Content-Type': 'application/json' }),
    json: async () => ({ access_token: 'access-xyz' }),
    text: async () => JSON.stringify({ access_token: 'access-xyz' }),
  });

  it('initializes e2eeService with the generated keys and persists them on successful SSO setup', async () => {
    mockFetch.mockResolvedValueOnce(okRegistrationResponse());

    // storeE2EEKeys is not in the default window.electron mock; add a spy so we can
    // assert the session keys are persisted to the OS keychain.
    const storeE2EEKeysMock = vi.fn().mockResolvedValue(undefined);
    (window.electron as unknown as { storeE2EEKeys: typeof storeE2EEKeysMock }).storeE2EEKeys =
      storeE2EEKeysMock;

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
      'argon2id'
    );
    // Session keys persisted to the OS keychain so E2EE survives an app restart.
    expect(storeE2EEKeysMock).toHaveBeenCalledWith({
      wrappingKeyBase64: 'wk',
      preferencesKeyBase64: 'pk',
      wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
    });
    expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    // ...and init precedes persistence.
    const initOrder = vi.mocked(e2eeService.initialize).mock.invocationCallOrder[0];
    const persistOrder = storeE2EEKeysMock.mock.invocationCallOrder[0];
    expect(initOrder).toBeLessThan(persistOrder);
  });

  it('completes SSO setup even if e2eeService init fails (non-fatal), rolling back via clearKeys', async () => {
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new Error('init boom'));
    mockFetch.mockResolvedValueOnce(okRegistrationResponse());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    // A failed E2EE pre-init must NOT break an otherwise-successful SSO registration
    // — the user falls back to secure-messaging-needs-a-logout→login. The catch must
    // clearKeys() to roll back any partial init so isInitialized is honestly false.
    await waitFor(() => {
      expect(useSSOStore.getState().state.phase).toBe('idle');
    });
    expect(useAuthStore.getState().accessToken).toBe('access-xyz');
    expect(e2eeService.clearKeys).toHaveBeenCalled();
  });

  it('keeps the in-memory E2EE session when only keychain persistence fails', async () => {
    // initialize() succeeds but the storeE2EEKeys IPC rejects. NOTE: the main-process
    // handler currently swallows keychain-write errors internally and resolves void
    // (tokenManager.ts) — see #1288 — so in production this renderer catch fires on an
    // IPC-transport failure or a future re-throwing handler, NOT on a plain
    // keychain-locked error. Either way clearKeys() must NOT be called: destroying the
    // valid in-memory session over a persistence failure would lose E2EE for the
    // current session, not just on restart (#1278 review, Gitar finding).
    const storeE2EEKeysMock = vi.fn().mockRejectedValue(new Error('keychain locked'));
    (window.electron as unknown as { storeE2EEKeys: typeof storeE2EEKeysMock }).storeE2EEKeys =
      storeE2EEKeysMock;
    mockFetch.mockResolvedValueOnce(okRegistrationResponse());

    render(<SSOPassphraseSetup />);
    fillAndSubmit();

    // Wait for the TERMINAL state: phase 'idle' is set after the persist catch runs,
    // so by then storeE2EEKeys was called (and rejected, and caught). Asserting after
    // this wait avoids racing the still-pending catch (Copilot #1289 review).
    await waitFor(() => {
      expect(useSSOStore.getState().state.phase).toBe('idle');
    });
    expect(storeE2EEKeysMock).toHaveBeenCalled();
    // The persistence failure must leave the in-memory session intact.
    expect(e2eeService.clearKeys).not.toHaveBeenCalled();
  });
});
