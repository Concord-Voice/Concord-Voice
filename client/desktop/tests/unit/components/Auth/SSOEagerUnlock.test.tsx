import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import SSOEagerUnlock from '@/renderer/components/Auth/SSOEagerUnlock';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch (matches Register.test.tsx / SSOPassphraseSetup.test.tsx pattern)
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Hoisted mock for e2eeService.initialize — swapped per test (happy vs fail).
const mockE2eeInitialize = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    initialize: (...args: unknown[]) => mockE2eeInitialize(...args),
    getSessionKeys: vi.fn().mockReturnValue({
      wrappingKeyBase64: 'bW9jay13cmFwcGluZw==', // pragma: allowlist secret
      preferencesKeyBase64: 'bW9jay1wcmVmcw==', // pragma: allowlist secret
      wrappedPrivateKeyBase64: 'bW9jay13cmFwcGVk', // pragma: allowlist secret
    }),
  },
}));

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
  resetAllStores();
  // The component talks to /users/me/keys via apiFetch — that requires an
  // access token in authStore for the Authorization header. The post-OAuth
  // login path sets the access token before mounting this gate.
  // We reach it directly via setState rather than re-importing the store
  // to avoid an extra dependency in this fixture.
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
    mockE2eeInitialize.mockResolvedValueOnce(undefined);

    const onUnlock = vi.fn();
    render(<SSOEagerUnlock onUnlock={onUnlock} onSocialRecovery={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/passphrase/i), {
      target: { value: 'CorrectPassphrase!12' },
    });
    fireEvent.click(screen.getByRole('button', { name: /unlock/i }));

    await waitFor(() => expect(onUnlock).toHaveBeenCalledTimes(1));
    expect(mockE2eeInitialize).toHaveBeenCalledTimes(1);
    // Verify the passphrase + wrapped material flowed through correctly.
    const [pw, wrapped, salt, alg] = mockE2eeInitialize.mock.calls[0];
    expect(pw).toBe('CorrectPassphrase!12');
    expect(wrapped).toBe('BASE64WRAPPED'); // pragma: allowlist secret
    expect(salt).toBe('BASE64SALT'); // pragma: allowlist secret
    expect(alg).toBe('argon2id');
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
    expect(onSocialRecovery).toHaveBeenCalledTimes(1);
  });
});
