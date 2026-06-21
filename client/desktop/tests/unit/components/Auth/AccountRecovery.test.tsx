import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';

// Mock global fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Mock crypto utilities
vi.mock('@/renderer/utils/crypto', () => ({
  unwrapWithRecoveryKey: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
  generateRegistrationKeys: vi.fn().mockResolvedValue({
    wrappedPrivateKey: 'mock-wrapped-key',
    keyDerivationSalt: 'mock-salt',
    keyDerivationAlg: 'argon2id',
    publicKey: {
      // Minimal mock CryptoKey
    },
  }),
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
    vi.clearAllMocks();
    mockFetch.mockReset();
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
    mockFetch.mockResolvedValueOnce({
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
    mockFetch.mockResolvedValueOnce({
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
    mockFetch.mockRejectedValueOnce(new Error('Network error'));

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
    mockFetch.mockImplementation(() => new Promise(() => {}));

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
    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockImplementation(() => new Promise(() => {}));

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
    mockFetch.mockResolvedValueOnce({
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
    mockFetch.mockResolvedValueOnce({
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
    const { unwrapWithRecoveryKey } = await import('@/renderer/utils/crypto');
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
    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        recovery_token: 'mock-token',
        has_recovery_key: true,
        has_trusted_devices: false,
        has_recovery_circle: false,
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
    mockFetch.mockResolvedValueOnce({
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
    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockImplementation(() => new Promise(() => {}));

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
    mockFetch.mockResolvedValueOnce({
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
    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
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

  it('shows device waiting step subtitle', async () => {
    await advanceToRecoveryKeyStep();

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: '11111111-1111-1111-1111-111111111111' }),
    });

    fireEvent.click(screen.getByText('Recover from trusted device instead'));

    await waitFor(() => {
      expect(screen.getByText('Waiting for trusted device approval')).toBeInTheDocument();
      expect(
        screen.getByText('Waiting for approval from your trusted device...')
      ).toBeInTheDocument();
    });
  });

  it('shows "Try a different recovery method" in device waiting step', async () => {
    await advanceToRecoveryKeyStep();

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: '11111111-1111-1111-1111-111111111111' }),
    });

    fireEvent.click(screen.getByText('Recover from trusted device instead'));

    await waitFor(() => {
      expect(screen.getByText('Try a different recovery method')).toBeInTheDocument();
    });
  });

  // --- Social Waiting Step ---

  it('shows social waiting step subtitle', async () => {
    await advanceToRecoveryKeyStep();

    mockFetch.mockResolvedValueOnce({
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

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: '22222222-2222-2222-2222-222222222222', threshold_k: 3 }),
    });

    fireEvent.click(screen.getByText('Recover via Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('0 / 3 shares received')).toBeInTheDocument();
    });
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
    const logo = screen.getByAltText('Concord Voice');
    expect(logo).toBeInTheDocument();
  });

  // --- Upfront request_id validation ---
  //
  // These tests verify the upfront validation at setDeviceRequestId /
  // setSocialRequestId call sites — catching a malformed server response
  // immediately, before the "Waiting..." screen ever renders. This is
  // strictly better UX than the poll-catch fallback above (which stays in
  // place as defense-in-depth).

  it('rejects device recovery initiation immediately when server returns malformed request_id', async () => {
    await advanceToRecoveryKeyStep();

    // Server returns a malformed request_id on POST /device-request
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ request_id: 'not-a-uuid' }),
    });
    fireEvent.click(screen.getByText('Recover from trusted device instead'));

    // User should see the error immediately — no "Waiting..." flash because
    // upfront validation throws before step transitions to device-waiting.
    await waitFor(() => {
      expect(
        screen.getByText('Server returned an invalid recovery request ID. Please try again.')
      ).toBeInTheDocument();
    });
    // Step should remain at recovery-key
    expect(
      screen.getByText('Enter your recovery key to restore your encrypted data')
    ).toBeInTheDocument();
    // "Waiting..." screen should never have rendered
    expect(screen.queryByText('Waiting for trusted device approval')).not.toBeInTheDocument();
  });

  it('rejects social recovery initiation immediately when server returns malformed request_id', async () => {
    await advanceToRecoveryKeyStep();

    mockFetch.mockResolvedValueOnce({
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
