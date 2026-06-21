import { render, screen, userEvent } from '../../../test-utils';
import { vi } from 'vitest';

// ── Mocks ──────────────────────────────────────────────────────────────

const mockApiFetch = vi.fn();

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

vi.mock('qrcode', () => ({
  default: {
    toDataURL: vi.fn().mockResolvedValue('data:image/png;base64,mockQRCode'),
  },
}));

vi.mock('@/renderer/utils/crypto', () => ({
  generateRecoveryKey: vi.fn().mockReturnValue('AAAA-BBBB-CCCC-DDDD'),
  wrapWithRecoveryKey: vi.fn().mockResolvedValue({
    wrappedKey: 'mock-wrapped-key',
    salt: 'mock-salt',
  }),
  wrapPrefsKeyWithRecoveryKey: vi.fn().mockResolvedValue({
    wrappedKey: 'mock-wrapped-prefs',
    salt: 'mock-prefs-salt',
  }),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getWrappingKey: vi.fn().mockReturnValue('mock-wrapping-key'),
    getWrappedPrivateKey: vi.fn().mockReturnValue('mock-wrapped-private-key'),
    getPreferencesKeyBase64: vi.fn().mockReturnValue('mock-prefs-key'),
  },
}));

vi.mock('@/renderer/components/Auth/TOTPInput', () => ({
  default: ({
    onSubmit,
    disabled,
    error,
  }: {
    onSubmit: (code: string) => void;
    disabled?: boolean;
    error?: string;
  }) => (
    <div data-testid="totp-input">
      <input data-testid="totp-code-input" disabled={disabled} onChange={() => {}} />
      <button data-testid="totp-submit" disabled={disabled} onClick={() => onSubmit('123456')}>
        Verify
      </button>
      {error && <span data-testid="totp-error">{error}</span>}
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/MFAVerifyPrompt', () => ({
  default: ({
    onVerify,
    disabled,
    error,
  }: {
    methods: string[];
    onVerify: (code: string) => void;
    disabled?: boolean;
    error?: string;
    excludeBackupCodes?: boolean;
    recoveryOnlyMethods?: string[];
  }) => (
    <div data-testid="mfa-verify-prompt">
      <input
        data-testid="mfa-verify-input"
        disabled={disabled}
        onChange={(e) => onVerify(e.target.value)}
      />
      {error && <span data-testid="mfa-verify-error">{error}</span>}
    </div>
  ),
}));

vi.mock('@/renderer/components/Settings/BackupCodeDisplay', () => ({
  default: ({
    codes,
    onConfirm,
    disabled,
  }: {
    codes: string[];
    onConfirm: () => void;
    disabled?: boolean;
  }) => (
    <div data-testid="backup-code-display">
      <span data-testid="backup-codes">{codes.join(', ')}</span>
      <button data-testid="backup-confirm" onClick={onConfirm} disabled={disabled}>
        Saved My Codes
      </button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Settings/RecoveryKeyDisplay', () => ({
  default: ({
    recoveryKey,
    onConfirm,
    onSkip,
    disabled,
  }: {
    recoveryKey: string;
    onConfirm: () => void;
    onSkip: () => void;
    disabled?: boolean;
  }) => (
    <div data-testid="recovery-key-display">
      <span data-testid="recovery-key">{recoveryKey}</span>
      <button data-testid="recovery-confirm" onClick={onConfirm} disabled={disabled}>
        Done
      </button>
      <button data-testid="recovery-skip" onClick={onSkip}>
        Skip
      </button>
    </div>
  ),
}));

import { e2eeService as mockE2eeService } from '@/renderer/services/e2eeService';
import MFASetup from '@/renderer/components/Settings/MFASetup';

describe('MFASetup', () => {
  const onComplete = vi.fn();
  const onCancel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── TOTP Flow ──────────────────────────────────────────────────────────

  describe('TOTP Flow', () => {
    it('renders TOTP setup wizard title', () => {
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Set Up Authenticator App')).toBeInTheDocument();
    });

    it('renders password input on initial step', () => {
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
    });

    it('renders continue and cancel buttons', () => {
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Continue')).toBeInTheDocument();
      expect(screen.getByText('Cancel')).toBeInTheDocument();
    });

    it('shows setup prompt for new MFA', () => {
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Enter your password to begin setup.')).toBeInTheDocument();
    });

    it('shows identity verification message when mfaActive', () => {
      render(<MFASetup method="totp" mfaActive onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Verify your identity to add another method.')).toBeInTheDocument();
    });

    it('shows MFA verify prompt when mfaActive', () => {
      render(
        <MFASetup
          method="totp"
          mfaActive
          activeMethods={['totp']}
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      expect(screen.getByTestId('mfa-verify-prompt')).toBeInTheDocument();
    });

    it('disables Continue button when password is empty', () => {
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Continue')).toBeDisabled();
    });

    it('enables Continue button when password is entered', async () => {
      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      expect(screen.getByText('Continue')).not.toBeDisabled();
    });

    it('disables Continue when mfaActive and no mfa code provided', async () => {
      const user = userEvent.setup();
      render(
        <MFASetup
          method="totp"
          mfaActive
          activeMethods={['totp']}
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      // mfaActive but no mfaCode entered yet
      expect(screen.getByText('Continue')).toBeDisabled();
    });

    it('calls onCancel when cancel button is clicked', async () => {
      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.click(screen.getByText('Cancel'));
      expect(onCancel).toHaveBeenCalled();
    });

    it('calls TOTP setup API with password', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          otpauth_url: 'otpauth://totp/Concord:test@example.com?secret=JBSWY3DPEHPK3PXP',
          secret: 'JBSWY3DPEHPK3PXP',
        }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(mockApiFetch).toHaveBeenCalledWith(
          '/api/v1/mfa/totp/setup',
          expect.objectContaining({ method: 'POST' })
        );
      });
    });

    it('includes mfa_code in setup request when mfaActive', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          otpauth_url: 'otpauth://totp/test',
          secret: 'SECRET',
        }),
      });

      const user = userEvent.setup();
      render(
        <MFASetup
          method="totp"
          mfaActive
          activeMethods={['totp']}
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');

      // Simulate MFA code entry via the mock prompt
      const mfaInput = screen.getByTestId('mfa-verify-input');
      await user.type(mfaInput, '654321');

      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        const body = JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);
        expect(body.mfa_code).toBe('654321');
      });
    });

    it('advances to QR step after successful setup', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          otpauth_url: 'otpauth://totp/Concord:test@example.com?secret=JBSWY3DPEHPK3PXP',
          secret: 'JBSWY3DPEHPK3PXP',
        }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(
          screen.getByText(
            'Scan this QR code with your authenticator app, then enter the 6-digit code below.'
          )
        ).toBeInTheDocument();
      });
    });

    it('shows manual secret entry on QR step', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          otpauth_url: 'otpauth://totp/test',
          secret: 'JBSWY3DPEHPK3PXP',
        }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(screen.getByText("Can't scan? Enter manually")).toBeInTheDocument();
        expect(screen.getByText('JBSWY3DPEHPK3PXP')).toBeInTheDocument();
      });
    });

    it('shows error on setup failure', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Incorrect password' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'wrongpassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(screen.getByText('Incorrect password')).toBeInTheDocument();
      });
    });

    it('shows error on TOTP verify failure', async () => {
      // First call: setup succeeds
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'TESTSECRET' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-input')).toBeInTheDocument());

      // Second call: verify fails
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Invalid TOTP code' }),
      });

      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => {
        expect(screen.getByTestId('totp-error')).toHaveTextContent('Invalid TOTP code');
      });
    });

    it('shows error on confirm-setup failure', async () => {
      // Setup
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-input')).toBeInTheDocument());

      // Verify
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ backup_codes: ['CODE1'] }),
      });
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-code-display')).toBeInTheDocument());

      // Confirm fails
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Session expired' }),
      });
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        expect(screen.getByText('Session expired')).toBeInTheDocument();
      });
    });

    it('advances to backup codes after TOTP verification', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'TESTSECRET' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(screen.getByTestId('totp-input')).toBeInTheDocument();
      });

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ backup_codes: ['AAAA1111', 'BBBB2222', 'CCCC3333'] }),
      });

      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => {
        expect(screen.getByTestId('backup-code-display')).toBeInTheDocument();
        expect(screen.getByText('AAAA1111, BBBB2222, CCCC3333')).toBeInTheDocument();
      });
    });

    it('completes full TOTP flow through recovery key', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'TESTSECRET' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-input')).toBeInTheDocument());

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({ backup_codes: ['CODE1'] }),
      });
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-code-display')).toBeInTheDocument());

      mockApiFetch
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) });
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        expect(screen.getByTestId('recovery-key-display')).toBeInTheDocument();
        expect(screen.getByText('AAAA-BBBB-CCCC-DDDD')).toBeInTheDocument();
      });

      await user.click(screen.getByTestId('recovery-confirm'));

      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });

      await user.click(screen.getByText('Done'));
      expect(onComplete).toHaveBeenCalled();
    });

    it('skips recovery key when skip is clicked', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => expect(screen.getByTestId('recovery-skip')).toBeInTheDocument());
      await user.click(screen.getByTestId('recovery-skip'));

      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });
    });

    it('skips to done when wrapping key is null (no recovery key possible)', async () => {
      mockE2eeService.getWrappingKey.mockReturnValueOnce(null);

      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });
    });

    it('skips to done when recovery key store fails', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) })
        // Recovery key store fails
        .mockResolvedValueOnce({ ok: false, json: async () => ({ error: 'storage error' }) });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      // Should skip recovery and go straight to done
      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });
    });

    it('wraps prefs key when available', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      // Check that PUT to recovery-key includes prefs payload
      await vi.waitFor(() => {
        const putCall = mockApiFetch.mock.calls.find(
          (call: unknown[]) => call[0] === '/api/v1/mfa/recovery-key'
        );
        expect(putCall).toBeDefined();
        const body = JSON.parse((putCall![1] as { body: string }).body);
        expect(body.recovery_wrapped_prefs_key).toBe('mock-wrapped-prefs');
        expect(body.recovery_prefs_key_salt).toBe('mock-prefs-salt');
      });
    });

    it('omits prefs payload when prefs key is null', async () => {
      mockE2eeService.getPreferencesKeyBase64.mockReturnValueOnce(null);

      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        const putCall = mockApiFetch.mock.calls.find(
          (call: unknown[]) => call[0] === '/api/v1/mfa/recovery-key'
        );
        if (putCall) {
          const body = JSON.parse((putCall[1] as { body: string }).body);
          expect(body.recovery_wrapped_prefs_key).toBeUndefined();
        }
      });
    });

    it('sets error field to password when error message includes password', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Invalid password provided' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'wrong');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        // The password input should have error class
        const passwordInput = screen.getByPlaceholderText('Your password');
        expect(passwordInput.className).toContain('error');
      });
    });
  });

  // ── ErrorBanner sub-component ───────────────────────────────────────

  describe('ErrorBanner (extracted sub-component)', () => {
    it('shows error banner for general error field', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Server unavailable' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'testpw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(screen.getByText('Server unavailable')).toBeInTheDocument();
      });
      // The error banner should render (general classification, not password/mfa)
      const banner = document.querySelector('.mfa-setup-error-banner');
      expect(banner).toBeInTheDocument();
    });

    it('does not show error banner when errorField is mfa', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Invalid MFA code' }),
      });

      const user = userEvent.setup();
      render(
        <MFASetup
          method="totp"
          mfaActive
          activeMethods={['totp']}
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      await user.type(screen.getByPlaceholderText('Your password'), 'testpw');
      // Type an MFA code
      const mfaInput = screen.getByTestId('mfa-verify-input');
      await user.type(mfaInput, '123456');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        // MFA error should be sent to the MFAVerifyPrompt, not shown as a banner
        expect(screen.getByTestId('mfa-verify-error')).toHaveTextContent('Invalid MFA code');
      });
    });
  });

  // ── Recovery key exception handling ─────────────────────────────────

  describe('Recovery key generation exception', () => {
    it('skips to done when generateAndStoreRecoveryKey throws', async () => {
      // Make crypto functions throw
      const { generateRecoveryKey } = await import('@/renderer/utils/crypto');
      vi.mocked(generateRecoveryKey).mockImplementationOnce(() => {
        throw new Error('crypto failure');
      });

      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      // Should skip recovery and go to done since recovery key generation threw
      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });
    });
  });

  // ── WebAuthn Flow ──────────────────────────────────────────────────────

  describe('WebAuthn Flow', () => {
    it('renders WebAuthn setup wizard title for hardware key', () => {
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Set Up Security Key')).toBeInTheDocument();
    });

    it('renders WebAuthn setup wizard title for platform authenticator', () => {
      render(
        <MFASetup
          method="webauthn"
          credentialType="platform"
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      expect(screen.getByText('Set Up Platform Authenticator')).toBeInTheDocument();
    });

    it('renders password input for WebAuthn', () => {
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
    });

    it('renders key name input with hardware key placeholder', () => {
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      expect(
        screen.getByPlaceholderText('Key name (e.g. YubiKey 5, Google Titan)')
      ).toBeInTheDocument();
    });

    it('renders key name input with platform placeholder', () => {
      render(
        <MFASetup
          method="webauthn"
          credentialType="platform"
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      expect(
        screen.getByPlaceholderText('Key name (e.g. MacBook Touch ID, Windows Hello)')
      ).toBeInTheDocument();
    });

    it('renders Register Key button', () => {
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Register Key')).toBeInTheDocument();
    });

    it('disables Register Key button when password is empty', () => {
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Register Key')).toBeDisabled();
    });

    it('enables Register Key button when password is entered', async () => {
      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      expect(screen.getByText('Register Key')).not.toBeDisabled();
    });

    it('calls onCancel when cancel button is clicked in WebAuthn flow', async () => {
      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.click(screen.getByText('Cancel'));
      expect(onCancel).toHaveBeenCalled();
    });

    it('shows MFA verify prompt when mfaActive in WebAuthn flow', () => {
      render(
        <MFASetup
          method="webauthn"
          mfaActive
          activeMethods={['totp']}
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      expect(screen.getByTestId('mfa-verify-prompt')).toBeInTheDocument();
    });

    it('shows identity verification message for WebAuthn when mfaActive', () => {
      render(<MFASetup method="webauthn" mfaActive onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Verify your identity and name your key.')).toBeInTheDocument();
    });

    it('shows password prompt for WebAuthn when not mfaActive', () => {
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      expect(screen.getByText('Enter your password and name your key.')).toBeInTheDocument();
    });

    it('shows error on WebAuthn begin failure', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Incorrect password' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'wrongpw');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('Incorrect password')).toBeInTheDocument();
      });
    });

    it('transitions to registering step and shows waiting message on success begin', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          publicKey: {
            challenge: 'dGVzdC1jaGFsbGVuZ2U',
            rp: { name: 'Concord', id: 'localhost' },
            user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
            pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
          },
        }),
      });

      // navigator.credentials.create never resolves (simulates waiting for key)
      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockReturnValue(new Promise(() => {})),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('Waiting for your security key...')).toBeInTheDocument();
      });
    });

    it('shows Registering... text while loading', async () => {
      mockApiFetch.mockReturnValue(new Promise(() => {})); // never resolves

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('Registering...')).toBeInTheDocument();
      });
    });

    it('disables Register Key when mfaActive and no mfa code', async () => {
      const user = userEvent.setup();
      render(
        <MFASetup
          method="webauthn"
          mfaActive
          activeMethods={['totp']}
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      expect(screen.getByText('Register Key')).toBeDisabled();
    });

    it('returns to password step on generic credentials.create error', async () => {
      // Begin succeeds, browser credentials.create rejects with a generic error.
      // Because the React state closure captures webauthnStep='password',
      // shouldResetToPasswordStep returns true, resetting to the password step.
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          publicKey: {
            challenge: 'dGVzdC1jaGFsbGVuZ2U',
            rp: { name: 'Concord', id: 'localhost' },
            user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
            pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
          },
        }),
      });

      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockRejectedValue(new Error('Something went wrong')),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        // Returns to password step with the error banner shown
        expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
        expect(screen.getByText('Something went wrong')).toBeInTheDocument();
      });
    });

    it('shows Cancel button on registering step when no error (key waiting)', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          publicKey: {
            challenge: 'dGVzdC1jaGFsbGVuZ2U',
            rp: { name: 'Concord', id: 'localhost' },
            user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
            pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
          },
        }),
      });

      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockReturnValue(new Promise(() => {})),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('Waiting for your security key...')).toBeInTheDocument();
        // In waiting state (no error), Cancel button should be shown, not Try Again
        expect(screen.queryByText('Try Again')).not.toBeInTheDocument();
        expect(screen.getByText('Cancel')).toBeInTheDocument();
      });
    });

    it('returns to password step when NotAllowedError occurs (user cancelled)', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          publicKey: {
            challenge: 'dGVzdC1jaGFsbGVuZ2U',
            rp: { name: 'Concord', id: 'localhost' },
            user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
            pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
          },
        }),
      });

      const notAllowedError = new DOMException('User cancelled', 'NotAllowedError');
      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockRejectedValue(notAllowedError),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        // NotAllowedError classifies to "Registration cancelled or timed out"
        // and shouldResetToPasswordStep returns true, so we're back to password step
        expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
        expect(
          screen.getByText('Registration cancelled or timed out. Try again.')
        ).toBeInTheDocument();
      });
    });

    it('shows WebAuthn done step with Security Key Registered message', async () => {
      // Simulate full WebAuthn success — need to complete the begin+finish flow
      const mockCredential = {
        id: 'mock-cred-id',
        rawId: new ArrayBuffer(16),
        type: 'public-key',
        response: {
          attestationObject: new ArrayBuffer(32),
          clientDataJSON: new ArrayBuffer(32),
        },
      };

      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({
            publicKey: {
              challenge: 'dGVzdC1jaGFsbGVuZ2U',
              rp: { name: 'Concord', id: 'localhost' },
              user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
              pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
            },
          }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({}),
        });

      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockResolvedValue(mockCredential),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('Security Key Registered!')).toBeInTheDocument();
        expect(
          screen.getByText('Your security key is now active and protecting your account.')
        ).toBeInTheDocument();
      });

      await user.click(screen.getByText('Done'));
      expect(onComplete).toHaveBeenCalled();
    });

    it('renders platform authenticator title for WebAuthn done step', async () => {
      const mockCredential = {
        id: 'mock-cred-id',
        rawId: new ArrayBuffer(16),
        type: 'public-key',
        response: {
          attestationObject: new ArrayBuffer(32),
          clientDataJSON: new ArrayBuffer(32),
        },
      };

      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({
            publicKey: {
              challenge: 'dGVzdC1jaGFsbGVuZ2U',
              rp: { name: 'Concord', id: 'localhost' },
              user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
              pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
            },
          }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({}),
        });

      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockResolvedValue(mockCredential),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(
        <MFASetup
          method="webauthn"
          credentialType="platform"
          onComplete={onComplete}
          onCancel={onCancel}
        />
      );
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('Security Key Registered!')).toBeInTheDocument();
      });
    });
  });
});
