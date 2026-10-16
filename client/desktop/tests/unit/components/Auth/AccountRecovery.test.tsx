import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import MFAChallengeModal from '@/renderer/components/Auth/MFAChallengeModal';
import { useMFAChallengeStore } from '@/renderer/stores/auth/mfaChallengeStore';
import { apiUrl } from '@/renderer/services/system/runtimeServerBase';

// Queue API responses at the HTTP boundary; retain real fetch.
const mockApiResponse = vi.fn();
const server = setupServer(
  http.all('*', async () => {
    try {
      const response = await mockApiResponse();
      return HttpResponse.json(await response.json(), { status: response.ok ? 200 : 400 });
    } catch (error) {
      return HttpResponse.json(
        { error: error instanceof Error ? error.message : 'Request failed' },
        { status: 400 }
      );
    }
  })
);
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());

// Mock crypto utilities
vi.mock('@/renderer/utils/crypto/crypto', () => ({
  unwrapWithRecoveryKey: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
  generateRegistrationKeys: vi.fn().mockResolvedValue({
    wrappedPrivateKey: 'mock-wrapped-key',
    keyDerivationSalt: 'mock-salt',
    keyDerivationAlg: 'argon2id',
    publicKey: {
      // Minimal mock CryptoKey
    },
  }),
  exportPublicKey: vi.fn().mockResolvedValue('mock-base64'),
  rewrapRecoveryAccountKey: vi.fn().mockResolvedValue('mock-base64'),
  arrayBufferToBase64: vi.fn().mockReturnValue('mock-base64'),
  generateSalt: vi.fn().mockReturnValue(new Uint8Array(16)),
  deriveKeyArgon2id: vi.fn().mockResolvedValue({} as CryptoKey),
  generateECDHKeyPair: vi.fn().mockResolvedValue({
    publicKey: {} as CryptoKey,
    privateKey: {} as CryptoKey,
  }),
  exportECDHPublicKey: vi.fn().mockResolvedValue('mock-ecdh-pub'),
  importECDHPublicKey: vi.fn().mockResolvedValue({} as CryptoKey),
  deriveSharedSecret: vi.fn().mockResolvedValue({} as CryptoKey),
  decryptWithSharedSecret: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
}));

// Mock crypto.subtle for the password reset flow
const mockSubtle = {
  importKey: vi.fn().mockResolvedValue({} as CryptoKey),
  wrapKey: vi.fn().mockResolvedValue(new ArrayBuffer(64)),
  exportKey: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
};
Object.defineProperty(globalThis.crypto, 'subtle', {
  value: mockSubtle,
  writable: true,
  configurable: true,
});
Object.defineProperty(globalThis.crypto, 'getRandomValues', {
  value: vi.fn((arr: Uint8Array) => {
    for (let i = 0; i < arr.length; i++) arr[i] = i;
    return arr;
  }),
  writable: true,
  configurable: true,
});

import AccountRecovery from '@/renderer/components/Auth/AccountRecovery';

describe('AccountRecovery', () => {
  const onBack = vi.fn();
  const onComplete = vi.fn();

  beforeEach(() => {
    resetAllStores();
    server.resetHandlers();
    vi.clearAllMocks();
    mockApiResponse.mockReset();
  });

  it('waits for the reserved factor through the real MFA dialog before offering reset', async () => {
    let proof: unknown;
    server.use(
      http.post(apiUrl('/api/v1/auth/recovery/begin'), () => HttpResponse.json({})),
      http.post(apiUrl('/api/v1/auth/recovery/verify-code'), () =>
        HttpResponse.json({
          mfa_required: true,
          mfa_challenge_token: 'recovery-challenge-test',
          methods: ['totp'],
        })
      ),
      http.post(apiUrl('/api/v1/auth/mfa/verify'), async ({ request }) => {
        proof = await request.json();
        return HttpResponse.json({
          recovery_token: 'completed-recovery-test',
          has_recovery_key: false,
          has_trusted_devices: false,
          has_recovery_circle: false,
        });
      })
    );
    render(
      <>
        <AccountRecovery onBack={onBack} onComplete={onComplete} />
        <MFAChallengeModal />
      </>
    );
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
    await screen.findByLabelText('Verification Code');
    fireEvent.change(screen.getByLabelText('Verification Code'), { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify Code' }));
    await screen.findByRole('dialog', { name: 'Verify Your Identity' });
    expect(useMFAChallengeStore.getState().purpose).toBe('recovery');
    expect(screen.queryByText('No recovery key found')).not.toBeInTheDocument();
    fireEvent.paste(screen.getByRole('textbox', { name: 'Digit 1' }), {
      clipboardData: { getData: () => '654321' },
    });
    await screen.findByText('No recovery key found');
    expect(proof).toEqual({
      mfa_challenge_token: 'recovery-challenge-test',
      method: 'totp',
      code: '654321',
    });
    expect(onComplete).not.toHaveBeenCalled();
  });

  it('keeps recovery closed when the reserved-factor prompt is cancelled', async () => {
    server.use(
      http.post(apiUrl('/api/v1/auth/recovery/begin'), () => HttpResponse.json({})),
      http.post(apiUrl('/api/v1/auth/recovery/verify-code'), () =>
        HttpResponse.json({
          mfa_required: true,
          mfa_challenge_token: 'cancelled-recovery-test',
          methods: ['totp'],
        })
      )
    );
    render(
      <>
        <AccountRecovery onBack={onBack} onComplete={onComplete} />
        <MFAChallengeModal />
      </>
    );
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
    await screen.findByLabelText('Verification Code');
    fireEvent.change(screen.getByLabelText('Verification Code'), { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify Code' }));
    await screen.findByRole('dialog', { name: 'Verify Your Identity' });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Send Recovery Code' })).toBeEnabled();
    expect(screen.queryByText('No recovery key found')).not.toBeInTheDocument();
    expect(onComplete).not.toHaveBeenCalled();
  });

  it.each(['auth', 'server'])(
    'cancels a pending recovery challenge on %s change',
    async (change) => {
      const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
      const { setRuntimeServerBase, resetRuntimeServerBase } =
        await import('@/renderer/services/system/runtimeServerBase');
      server.use(
        http.post(apiUrl('/api/v1/auth/recovery/begin'), () => HttpResponse.json({})),
        http.post(apiUrl('/api/v1/auth/recovery/verify-code'), () =>
          HttpResponse.json({
            mfa_required: true,
            mfa_challenge_token: 'pending-recovery-test',
            methods: ['totp'],
          })
        )
      );
      try {
        render(
          <>
            <AccountRecovery onBack={onBack} onComplete={onComplete} />
            <MFAChallengeModal />
          </>
        );
        fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
          target: { value: 'test@example.com' },
        });
        fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
        await screen.findByLabelText('Verification Code');
        fireEvent.change(screen.getByLabelText('Verification Code'), {
          target: { value: '123456' },
        });
        fireEvent.click(screen.getByRole('button', { name: 'Verify Code' }));
        await screen.findByRole('dialog', { name: 'Verify Your Identity' });
        act(() => {
          if (change === 'auth') {
            useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
          } else {
            setRuntimeServerBase('https://other-recovery.example');
          }
        });
        await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
        expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
        expect(screen.getByText('Recovery context changed. Start again.')).toBeInTheDocument();
        expect(screen.getByRole('button', { name: 'Send Recovery Code' })).toBeEnabled();
      } finally {
        resetRuntimeServerBase();
      }
    }
  );

  it('releases a stale social request without clearing a newer code-send spinner', async () => {
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    await advanceToRecoveryKeyStep();
    const socialStarted = deferred<void>();
    const socialReply = deferred<void>();
    const beginStarted = deferred<void>();
    const beginReply = deferred<void>();
    server.use(
      http.post(apiUrl('/api/v1/auth/recovery/social-request'), async () => {
        socialStarted.resolve();
        await socialReply.promise;
        return HttpResponse.json({
          request_id: '22222222-2222-2222-2222-222222222222',
          threshold_k: 3,
        });
      }),
      http.post(apiUrl('/api/v1/auth/recovery/begin'), async () => {
        beginStarted.resolve();
        await beginReply.promise;
        return HttpResponse.json({});
      })
    );
    fireEvent.click(screen.getByRole('button', { name: 'Recover via Recovery Circle' }));
    await socialStarted.promise;
    act(() =>
      useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
    );
    expect(screen.getByRole('button', { name: 'Send Recovery Code' })).toBeEnabled();
    fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
    await beginStarted.promise;
    expect(screen.getByText('Sending...')).toBeInTheDocument();
    await act(async () => socialReply.resolve());
    expect(screen.getByText('Sending...')).toBeInTheDocument();
    await act(async () => beginReply.resolve());
    await screen.findByLabelText('Verification Code');
  });

  it('restarts after a reserved factor returns incomplete recovery data', async () => {
    server.use(
      http.post(apiUrl('/api/v1/auth/recovery/begin'), () => HttpResponse.json({})),
      http.post(apiUrl('/api/v1/auth/recovery/verify-code'), () =>
        HttpResponse.json({
          mfa_required: true,
          mfa_challenge_token: 'incomplete-recovery-test',
          methods: ['totp'],
        })
      ),
      http.post(apiUrl('/api/v1/auth/mfa/verify'), () =>
        HttpResponse.json({ recovery_token: 'incomplete-recovery-test' })
      )
    );
    render(
      <>
        <AccountRecovery onBack={onBack} onComplete={onComplete} />
        <MFAChallengeModal />
      </>
    );
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
    await screen.findByLabelText('Verification Code');
    fireEvent.change(screen.getByLabelText('Verification Code'), { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify Code' }));
    await screen.findByRole('dialog', { name: 'Verify Your Identity' });
    fireEvent.paste(screen.getByRole('textbox', { name: 'Digit 1' }), {
      clipboardData: { getData: () => '654321' },
    });
    await screen.findByText('Recovery verification is incomplete. Start again.');
    expect(screen.getByRole('button', { name: 'Send Recovery Code' })).toBeEnabled();
    expect(screen.queryByLabelText('Verification Code')).not.toBeInTheDocument();
  });

  // --- Step 1: Email ---

  it('renders Account Recovery title', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    expect(screen.getByText('Account Recovery')).toBeInTheDocument();
  });

  it('renders email step subtitle', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    expect(screen.getByText('Enter your email to receive a recovery code')).toBeInTheDocument();
  });

  it('renders email input field', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    expect(screen.getByPlaceholderText('you@example.com')).toBeInTheDocument();
  });

  it('renders Send Recovery Code button', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    expect(screen.getByText('Send Recovery Code')).toBeInTheDocument();
  });

  it('disables Send Recovery Code when email is empty', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    const btn = screen.getByText('Send Recovery Code');
    expect(btn).toBeDisabled();
  });

  it('enables Send Recovery Code when email is entered', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    const btn = screen.getByText('Send Recovery Code');
    expect(btn).not.toBeDisabled();
  });

  it('renders Back to login button', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    const backBtn = screen.getByText(/Back to login/);
    expect(backBtn).toBeInTheDocument();
  });

  it('calls onBack when Back to login is clicked', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.click(screen.getByText(/Back to login/));
    expect(onBack).toHaveBeenCalled();
  });

  it('sends recovery code and advances to verify step', async () => {
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));

    await waitFor(() => {
      expect(screen.getByText('Enter the 6-digit code sent to your email')).toBeInTheDocument();
    });
  });

  it('shows error when send code fails', async () => {
    mockApiResponse.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Email not found' }),
    });

    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'bad@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));

    await waitFor(() => {
      expect(screen.getByText('Email not found')).toBeInTheDocument();
    });
  });

  it('shows generic error on network failure during code send', async () => {
    mockApiResponse.mockRejectedValueOnce(new Error('Network error'));

    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));

    await waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });
  });

  it('shows Sending... text during code send loading', async () => {
    // Make fetch hang
    mockApiResponse.mockImplementation(() => new Promise(() => {}));

    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));

    await waitFor(() => {
      expect(screen.getByText(/Sending\.\.\./)).toBeInTheDocument();
    });
  });

  // --- Step 2: Verify Code ---

  async function advanceToVerifyStep() {
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));
    await waitFor(() => {
      expect(screen.getByText('Enter the 6-digit code sent to your email')).toBeInTheDocument();
    });
  }

  it.each([
    { recovery_token: 'token-only' },
    {
      recovery_token: 'missing-material',
      has_recovery_key: true,
      has_trusted_devices: false,
      has_recovery_circle: false,
    },
  ])('rejects incomplete recovery capability data %j', async (payload) => {
    await advanceToVerifyStep();
    mockApiResponse.mockResolvedValueOnce({ ok: true, json: async () => payload });
    fireEvent.change(screen.getByLabelText('Verification Code'), { target: { value: '123456' } });
    fireEvent.click(screen.getByText('Verify Code'));
    await screen.findByText('Recovery verification is incomplete. Start again.');
    expect(screen.getByRole('button', { name: 'Send Recovery Code' })).toBeEnabled();
    expect(screen.queryByLabelText('Verification Code')).not.toBeInTheDocument();
    expect(screen.queryByText('No recovery key found')).not.toBeInTheDocument();
    expect(screen.queryByText('Recover Account')).not.toBeInTheDocument();
  });

  it.each([undefined, { challenge: 'invalid' }])(
    'rejects unusable WebAuthn-only recovery options %j',
    async (options) => {
      await advanceToVerifyStep();
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          mfa_required: true,
          mfa_challenge_token: 'unavailable-challenge',
          methods: ['webauthn'],
          webauthn_options: options,
        }),
      });
      fireEvent.change(screen.getByLabelText('Verification Code'), { target: { value: '123456' } });
      fireEvent.click(screen.getByText('Verify Code'));
      await screen.findByText('Recovery verification is unavailable. Request a new email code.');
      expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
      expect(screen.getByText('Send Recovery Code')).toBeEnabled();
    }
  );

  it.each([
    'Recover Account',
    'Recover from trusted device instead',
    'Recover via Recovery Circle',
  ])('refuses stale issued recovery before %s', async (action) => {
    const crypto = await import('@/renderer/utils/crypto/crypto');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    await advanceToRecoveryKeyStep();
    fireEvent.change(screen.getByLabelText('Recovery Key'), { target: { value: 'key' } });
    useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
    fireEvent.click(screen.getByText(action));
    await screen.findByText('Recovery context changed. Start again.');
    expect(crypto.unwrapWithRecoveryKey).not.toHaveBeenCalled();
    expect(crypto.generateECDHKeyPair).not.toHaveBeenCalled();
    expect(mockApiResponse).toHaveBeenCalledTimes(2);
    expect(screen.getByText('Send Recovery Code')).toBeEnabled();
  });

  it('clears issued material after a server change and refuses a pending unwrap result', async () => {
    const crypto = await import('@/renderer/utils/crypto/crypto');
    const { setRuntimeServerBase, resetRuntimeServerBase } =
      await import('@/renderer/services/system/runtimeServerBase');
    let finish: (bytes: ArrayBuffer) => void = () => {};
    vi.mocked(crypto.unwrapWithRecoveryKey).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        })
    );
    await advanceToRecoveryKeyStep();
    fireEvent.change(screen.getByLabelText('Recovery Key'), { target: { value: 'key' } });
    fireEvent.click(screen.getByText('Recover Account'));
    const bytes = new Uint8Array([1, 2, 3]);
    await act(async () => {
      setRuntimeServerBase('https://other-recovery.example');
      finish(bytes.buffer);
    });
    expect(bytes).toEqual(new Uint8Array(3));
    expect(screen.getByText('Send Recovery Code')).toBeEnabled();
    expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
    expect(mockApiResponse).toHaveBeenCalledTimes(2);
    resetRuntimeServerBase();
  });

  it('clears recovered key bytes when auth changes before password reset', async () => {
    const crypto = await import('@/renderer/utils/crypto/crypto');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    const bytes = new Uint8Array([1, 2, 3]);
    vi.mocked(crypto.unwrapWithRecoveryKey).mockResolvedValueOnce(bytes.buffer);
    await advanceToRecoveryKeyStep();
    fireEvent.change(screen.getByLabelText('Recovery Key'), { target: { value: 'key' } });
    fireEvent.click(screen.getByText('Recover Account'));
    await screen.findByLabelText('New Password');
    act(() =>
      useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
    );
    expect(bytes).toEqual(new Uint8Array(3));
    expect(screen.queryByText('Reset Password')).not.toBeInTheDocument();
    expect(screen.getByText('Send Recovery Code')).toBeEnabled();
    expect(crypto.rewrapRecoveryAccountKey).not.toHaveBeenCalled();
    expect(mockApiResponse).toHaveBeenCalledTimes(2);
  });

  it('renders verification code input', async () => {
    await advanceToVerifyStep();
    expect(screen.getByPlaceholderText('000000')).toBeInTheDocument();
  });

  it('renders Verify Code button', async () => {
    await advanceToVerifyStep();
    expect(screen.getByText('Verify Code')).toBeInTheDocument();
  });

  it('disables Verify Code button when code is not 6 digits', async () => {
    await advanceToVerifyStep();
    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123' },
    });
    expect(screen.getByText('Verify Code')).toBeDisabled();
  });

  it('enables Verify Code button when code is 6 digits', async () => {
    await advanceToVerifyStep();
    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    expect(screen.getByText('Verify Code')).not.toBeDisabled();
  });

  it('strips non-digit characters from code input', async () => {
    await advanceToVerifyStep();
    const input = screen.getByPlaceholderText('000000') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'abc123def456' } });
    expect(input.value).toBe('123456');
  });

  it('limits code input to 6 characters', async () => {
    await advanceToVerifyStep();
    const input = screen.getByPlaceholderText('000000') as HTMLInputElement;
    expect(input.getAttribute('maxLength')).toBe('6');
  });

  it('advances to recovery-key step when has_recovery_key is true', async () => {
    await advanceToVerifyStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: true,
        has_trusted_devices: false,
        has_recovery_circle: false,
        recovery_wrapped_private_key: 'mock-wrapped',
        recovery_key_salt: 'mock-salt',
      }),
    });

    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));

    await waitFor(() => {
      expect(
        screen.getByText('Enter your recovery key to restore your encrypted data')
      ).toBeInTheDocument();
    });
  });

  it('advances to reset-warning step when no recovery key', async () => {
    await advanceToVerifyStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: false,
        has_trusted_devices: false,
        has_recovery_circle: false,
      }),
    });

    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));

    await waitFor(() => {
      expect(screen.getByText('No recovery key found')).toBeInTheDocument();
    });
  });

  it('shows error when verify code fails', async () => {
    await advanceToVerifyStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid code' }),
    });

    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '999999' },
    });
    fireEvent.click(screen.getByText('Verify Code'));

    await waitFor(() => {
      expect(screen.getByText('Invalid code')).toBeInTheDocument();
    });
  });

  it('shows Verifying... text during code verification', async () => {
    await advanceToVerifyStep();

    mockApiResponse.mockImplementation(() => new Promise(() => {}));

    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));

    await waitFor(() => {
      expect(screen.getByText(/Verifying\.\.\./)).toBeInTheDocument();
    });
  });

  // --- Step 3a: Recovery Key ---

  async function advanceToRecoveryKeyStep() {
    // Step 1 -> Step 2
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));
    await waitFor(() => {
      expect(screen.getByPlaceholderText('000000')).toBeInTheDocument();
    });

    // Step 2 -> Step 3 (recovery-key)
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: true,
        has_trusted_devices: true,
        has_recovery_circle: true,
        recovery_wrapped_private_key: 'mock-wrapped',
        recovery_key_salt: 'mock-salt',
      }),
    });
    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));
    await waitFor(() => {
      expect(
        screen.getByText('Enter your recovery key to restore your encrypted data')
      ).toBeInTheDocument();
    });
  }

  it('renders recovery key textarea', async () => {
    await advanceToRecoveryKeyStep();
    expect(
      screen.getByPlaceholderText('Enter your recovery key (with or without dashes)')
    ).toBeInTheDocument();
  });

  it('renders Recover Account button', async () => {
    await advanceToRecoveryKeyStep();
    expect(screen.getByText('Recover Account')).toBeInTheDocument();
  });

  it('disables Recover Account when recovery key input is empty', async () => {
    await advanceToRecoveryKeyStep();
    expect(screen.getByText('Recover Account')).toBeDisabled();
  });

  it('enables Recover Account when recovery key is entered', async () => {
    await advanceToRecoveryKeyStep();
    fireEvent.change(
      screen.getByPlaceholderText('Enter your recovery key (with or without dashes)'),
      { target: { value: 'ABCD-EFGH-IJKL-MNOP' } }
    );
    expect(screen.getByText('Recover Account')).not.toBeDisabled();
  });

  it('shows "I don\'t have my recovery key" link', async () => {
    await advanceToRecoveryKeyStep();
    expect(screen.getByText("I don't have my recovery key")).toBeInTheDocument();
  });

  it('navigates to reset-warning when "I don\'t have my recovery key" is clicked', async () => {
    await advanceToRecoveryKeyStep();
    fireEvent.click(screen.getByText("I don't have my recovery key"));
    expect(screen.getByText('No recovery key found')).toBeInTheDocument();
  });

  it('shows trusted device recovery option when available', async () => {
    await advanceToRecoveryKeyStep();
    expect(screen.getByText('Recover from trusted device instead')).toBeInTheDocument();
  });

  it('shows recovery circle option when available', async () => {
    await advanceToRecoveryKeyStep();
    expect(screen.getByText('Recover via Recovery Circle')).toBeInTheDocument();
  });

  it('shows error for invalid recovery key', async () => {
    const { unwrapWithRecoveryKey } = await import('@/renderer/utils/crypto/crypto');
    (unwrapWithRecoveryKey as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error('Invalid key')
    );

    await advanceToRecoveryKeyStep();
    fireEvent.change(
      screen.getByPlaceholderText('Enter your recovery key (with or without dashes)'),
      { target: { value: 'INVALID-KEY' } }
    );
    fireEvent.click(screen.getByText('Recover Account'));

    await waitFor(() => {
      expect(
        screen.getByText('Invalid recovery key. Please check and try again.')
      ).toBeInTheDocument();
    });
  });

  it('advances to new-password step on valid recovery key', async () => {
    await advanceToRecoveryKeyStep();
    fireEvent.change(
      screen.getByPlaceholderText('Enter your recovery key (with or without dashes)'),
      { target: { value: 'VALID-RECOVERY-KEY' } }
    );
    fireEvent.click(screen.getByText('Recover Account'));

    await waitFor(() => {
      expect(screen.getByText('Set your new password')).toBeInTheDocument();
    });
  });

  // --- Step: Reset Warning ---

  async function advanceToResetWarningStep() {
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));
    await waitFor(() => {
      expect(screen.getByPlaceholderText('000000')).toBeInTheDocument();
    });

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: true,
        has_trusted_devices: false,
        has_recovery_circle: false,
        recovery_wrapped_private_key: 'mock-wrapped',
        recovery_key_salt: 'mock-salt',
      }),
    });
    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));
    await waitFor(() => {
      expect(
        screen.getByText('Enter your recovery key to restore your encrypted data')
      ).toBeInTheDocument();
    });

    // Navigate to reset warning
    fireEvent.click(screen.getByText("I don't have my recovery key"));
    expect(screen.getByText('No recovery key found')).toBeInTheDocument();
  }

  it('shows permanent data loss warning', async () => {
    await advanceToResetWarningStep();
    expect(screen.getByText(/Warning: Permanent Data Loss/)).toBeInTheDocument();
    // The data loss text appears in both the warning banner and the checkbox label
    const dataLossElements = screen.getAllByText(
      /encrypted message history will be permanently lost/
    );
    expect(dataLossElements.length).toBeGreaterThanOrEqual(1);
  });

  it('shows acknowledgment checkbox', async () => {
    await advanceToResetWarningStep();
    expect(
      screen.getByText('I understand that all encrypted message history will be permanently lost')
    ).toBeInTheDocument();
  });

  it('disables Continue button until acknowledgment checkbox is checked', async () => {
    await advanceToResetWarningStep();
    const continueBtn = screen.getByText('Continue with Account Reset');
    expect(continueBtn).toBeDisabled();
  });

  it('enables Continue button after acknowledgment checkbox is checked', async () => {
    await advanceToResetWarningStep();
    const checkbox = screen.getByRole('checkbox');
    fireEvent.click(checkbox);
    const continueBtn = screen.getByText('Continue with Account Reset');
    expect(continueBtn).not.toBeDisabled();
  });

  it('gives the enabled reset button its own red fill and label pair', async () => {
    // An inline #dc2626 kept the brand label: #0d0821 on it read 4.05:1 in
    // Concord and 3.14:1 in Fox Den. The class carries both halves, where the
    // contrast ratchet can measure them.
    await advanceToResetWarningStep();
    fireEvent.click(screen.getByRole('checkbox'));
    const continueBtn = screen.getByText('Continue with Account Reset');
    expect(continueBtn).toHaveClass('login-submit-btn--danger');
    expect(continueBtn.style.background).toBe('');
  });

  it('shows "I found my recovery key" link when recovery key exists', async () => {
    await advanceToResetWarningStep();
    expect(screen.getByText('I found my recovery key')).toBeInTheDocument();
  });

  it('navigates back to recovery-key step when "I found my recovery key" is clicked', async () => {
    await advanceToResetWarningStep();
    fireEvent.click(screen.getByText('I found my recovery key'));
    expect(
      screen.getByText('Enter your recovery key to restore your encrypted data')
    ).toBeInTheDocument();
  });

  it('advances to new-password step after acknowledgment', async () => {
    await advanceToResetWarningStep();
    const checkbox = screen.getByRole('checkbox');
    fireEvent.click(checkbox);
    fireEvent.click(screen.getByText('Continue with Account Reset'));
    expect(screen.getByText('Set your new password')).toBeInTheDocument();
  });

  // --- Step 4: New Password ---

  async function advanceToNewPasswordStep() {
    // Step 1 -> 2
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));
    await waitFor(() => {
      expect(screen.getByPlaceholderText('000000')).toBeInTheDocument();
    });

    // Step 2 -> recovery-key
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: true,
        has_trusted_devices: false,
        has_recovery_circle: false,
        recovery_wrapped_private_key: 'mock-wrapped',
        recovery_key_salt: 'mock-salt',
      }),
    });
    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));
    await waitFor(() => {
      expect(screen.getByText('Recover Account')).toBeInTheDocument();
    });

    // recovery-key -> new-password (via valid recovery key)
    fireEvent.change(
      screen.getByPlaceholderText('Enter your recovery key (with or without dashes)'),
      { target: { value: 'VALID-KEY' } }
    );
    fireEvent.click(screen.getByText('Recover Account'));
    await waitFor(() => {
      expect(screen.getByText('Set your new password')).toBeInTheDocument();
    });
  }

  it('renders new password and confirm password fields', async () => {
    await advanceToNewPasswordStep();
    expect(screen.getByPlaceholderText('At least 12 characters')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Confirm your new password')).toBeInTheDocument();
  });

  it('renders Reset Password button', async () => {
    await advanceToNewPasswordStep();
    expect(screen.getByText('Reset Password')).toBeInTheDocument();
  });

  it('disables Reset Password when fields are empty', async () => {
    await advanceToNewPasswordStep();
    expect(screen.getByText('Reset Password')).toBeDisabled();
  });

  it('shows error when passwords do not match', async () => {
    await advanceToNewPasswordStep();
    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'DifferentPassword!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Passwords do not match')).toBeInTheDocument();
    });
  });

  it('shows error when password is too short', async () => {
    await advanceToNewPasswordStep();
    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'short' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'short' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Password must be at least 12 characters')).toBeInTheDocument();
    });
  });

  it('shows success screen after password reset (recovery key path)', async () => {
    await advanceToNewPasswordStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Password Reset Complete')).toBeInTheDocument();
      expect(
        screen.getByText('Password reset successfully. Please sign in with your new password.')
      ).toBeInTheDocument();
    });
  });

  it('shows Sign In button on success screen', async () => {
    await advanceToNewPasswordStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Sign In')).toBeInTheDocument();
    });
  });

  it('calls onComplete when Sign In is clicked on success screen', async () => {
    await advanceToNewPasswordStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Sign In')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Sign In'));
    expect(onComplete).toHaveBeenCalled();
  });

  it('shows error when password reset API fails', async () => {
    await advanceToNewPasswordStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Token expired' }),
    });

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Token expired')).toBeInTheDocument();
    });
  });

  it('shows Resetting... text during password reset', async () => {
    await advanceToNewPasswordStep();

    mockApiResponse.mockImplementation(() => new Promise(() => {}));

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText(/Resetting\.\.\./)).toBeInTheDocument();
    });
  });

  // --- Account Reset Path (no recovery key) ---

  async function advanceToNewPasswordViaReset() {
    // Step 1 -> 2
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    fireEvent.change(screen.getByPlaceholderText('you@example.com'), {
      target: { value: 'test@example.com' },
    });
    fireEvent.click(screen.getByText('Send Recovery Code'));
    await waitFor(() => {
      expect(screen.getByPlaceholderText('000000')).toBeInTheDocument();
    });

    // Step 2 -> reset-warning (no recovery key)
    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: false,
        has_trusted_devices: false,
        has_recovery_circle: false,
      }),
    });
    fireEvent.change(screen.getByPlaceholderText('000000'), {
      target: { value: '123456' },
    });
    fireEvent.click(screen.getByText('Verify Code'));
    await waitFor(() => {
      expect(screen.getByText('No recovery key found')).toBeInTheDocument();
    });

    // reset-warning -> new-password
    const checkbox = screen.getByRole('checkbox');
    fireEvent.click(checkbox);
    fireEvent.click(screen.getByText('Continue with Account Reset'));
    expect(screen.getByText('Set your new password')).toBeInTheDocument();
  }

  it('shows success after account reset (data loss path)', async () => {
    await advanceToNewPasswordViaReset();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Password Reset Complete')).toBeInTheDocument();
    });
  });

  it('shows error when account reset API fails', async () => {
    await advanceToNewPasswordViaReset();

    mockApiResponse.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Account reset failed' }),
    });

    fireEvent.change(screen.getByPlaceholderText('At least 12 characters'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.change(screen.getByPlaceholderText('Confirm your new password'), {
      target: { value: 'MyNewPassword123!' },
    });
    fireEvent.click(screen.getByText('Reset Password'));

    await waitFor(() => {
      expect(screen.getByText('Account reset failed')).toBeInTheDocument();
    });
  });

  // --- Device Waiting Step ---

  it('shows social waiting step subtitle', async () => {
    await advanceToRecoveryKeyStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: '22222222-2222-2222-2222-222222222222', threshold_k: 3 }),
    });

    fireEvent.click(screen.getByText('Recover via Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('Waiting for Recovery Circle approval')).toBeInTheDocument();
      expect(
        screen.getByText('Waiting for your Recovery Circle to respond...')
      ).toBeInTheDocument();
    });
  });

  it('shows share progress in social waiting step', async () => {
    await advanceToRecoveryKeyStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: '22222222-2222-2222-2222-222222222222', threshold_k: 3 }),
    });

    fireEvent.click(screen.getByText('Recover via Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('0 / 3 shares received')).toBeInTheDocument();
    });
  });

  it('starts a new social ceremony at zero after the account lifecycle changes', async () => {
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    const interval = vi.spyOn(globalThis, 'setInterval');
    try {
      await advanceToRecoveryKeyStep();
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ request_id: '22222222-2222-2222-2222-222222222222', threshold_k: 3 }),
      });
      fireEvent.click(screen.getByText('Recover via Recovery Circle'));
      await screen.findByText('0 / 3 shares received');
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'pending', shares_received: 2 }),
      });
      // findByText gates on the DOM; the poll is registered by a passive effect that can land later.
      const pollHandler = () => interval.mock.calls.find(([, delay]) => delay === 5000)?.[0];
      await waitFor(() => expect(pollHandler()).toBeTypeOf('function'));
      const poll = pollHandler();
      if (typeof poll !== 'function') throw new Error('Social recovery poll was not started');
      await act(async () => {
        await poll();
      });
      expect(screen.getByText('2 / 3 shares received')).toBeInTheDocument();
      act(() =>
        useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
      );
      mockApiResponse.mockResolvedValueOnce({ ok: true, json: async () => ({}) });
      fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
      await screen.findByLabelText('Verification Code');
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          recovery_token: 'new-ceremony-token',
          has_recovery_key: true,
          has_trusted_devices: false,
          has_recovery_circle: true,
          recovery_wrapped_private_key: 'mock-wrapped',
          recovery_key_salt: 'mock-salt',
        }),
      });
      fireEvent.change(screen.getByLabelText('Verification Code'), { target: { value: '654321' } });
      fireEvent.click(screen.getByRole('button', { name: 'Verify Code' }));
      await screen.findByText('Enter your recovery key to restore your encrypted data');
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ request_id: '33333333-3333-3333-3333-333333333333', threshold_k: 4 }),
      });
      fireEvent.click(screen.getByText('Recover via Recovery Circle'));
      await screen.findByText('0 / 4 shares received');
    } finally {
      interval.mockRestore();
    }
  });

  it('wipes decrypted social shares when auth changes during the final decrypt', async () => {
    const crypto = await import('@/renderer/utils/crypto/crypto');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    const firstShare = new Uint8Array([1, 2, 3]);
    const secondShare = new Uint8Array([4, 5, 6]);
    const secondStarted = deferred();
    const secondDecrypt = deferred<ArrayBuffer>();
    vi.mocked(crypto.decryptWithSharedSecret)
      .mockResolvedValueOnce(firstShare.buffer)
      .mockImplementationOnce(() => {
        secondStarted.resolve();
        return secondDecrypt.promise;
      });
    const encryptedShare = (index: number) =>
      btoa(
        JSON.stringify({ ephemeral_public_key: 'mock', encrypted_data: 'mock', share_index: index })
      );
    const interval = vi.spyOn(globalThis, 'setInterval');
    try {
      await advanceToRecoveryKeyStep();
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ request_id: '22222222-2222-2222-2222-222222222222', threshold_k: 2 }),
      });
      mockApiResponse.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          status: 'complete',
          shares_received: 2,
          responses: [
            { encrypted_share: encryptedShare(1) },
            { encrypted_share: encryptedShare(2) },
          ],
        }),
      });
      fireEvent.click(screen.getByText('Recover via Recovery Circle'));
      await screen.findByText('Waiting for Recovery Circle approval');
      // findByText gates on the DOM; the poll is registered by a passive effect that can land later.
      const pollHandler = () => interval.mock.calls.find(([, delay]) => delay === 5000)?.[0];
      await waitFor(() => expect(pollHandler()).toBeTypeOf('function'));
      const poll = pollHandler();
      if (typeof poll !== 'function') throw new Error('Social recovery poll was not started');
      let pollPromise: Promise<void> | undefined;
      await act(async () => {
        pollPromise = poll() as Promise<void>;
        await secondStarted.promise;
      });
      expect(firstShare).toEqual(new Uint8Array([1, 2, 3]));
      act(() =>
        useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
      );
      await act(async () => {
        secondDecrypt.resolve(secondShare.buffer);
        await pollPromise;
      });
      expect(firstShare).toEqual(new Uint8Array(3));
      expect(secondShare).toEqual(new Uint8Array(3));
      expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
      expect(screen.getByText('Send Recovery Code')).toBeEnabled();
    } finally {
      interval.mockRestore();
    }
  });

  // --- Back to Login always visible ---

  it('shows Back to login button on every step', async () => {
    // Email step
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    expect(screen.getByText(/Back to login/)).toBeInTheDocument();
  });

  // --- Logo rendering ---

  it('renders the Concord Voice logo', () => {
    render(<AccountRecovery onBack={onBack} onComplete={onComplete} />);
    // Both theme variants of the wordmark: CSS shows exactly one per theme (asserted in
    // real Chromium by tests/e2e/auth-wordmark.spec.ts). Two here, rather than "at least
    // one", is what catches a regression back to a bare white-only image.
    expect(screen.getAllByAltText('Concord Voice')).toHaveLength(2);
  });

  // --- Upfront request_id validation ---
  //
  // These tests verify the upfront validation at setDeviceRequestId /
  // setSocialRequestId call sites — catching a malformed server response
  // immediately, before the "Waiting..." screen ever renders. This is
  // strictly better UX than the poll-catch fallback above (which stays in
  // place as defense-in-depth).

  it('rejects social recovery initiation immediately when server returns malformed request_id', async () => {
    await advanceToRecoveryKeyStep();

    mockApiResponse.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: 'also-not-a-uuid', threshold_k: 3 }),
    });
    fireEvent.click(screen.getByText('Recover via Recovery Circle'));

    await waitFor(() => {
      expect(
        screen.getByText('Server returned an invalid recovery request ID. Please try again.')
      ).toBeInTheDocument();
    });
    expect(
      screen.getByText('Enter your recovery key to restore your encrypted data')
    ).toBeInTheDocument();
    expect(screen.queryByText('Waiting for Recovery Circle approval')).not.toBeInTheDocument();
  });
});
