import { render, screen, userEvent } from '../../../test-utils';
import { vi } from 'vitest';

// ── Mocks ──────────────────────────────────────────────────────────────

const mockApiFetch = vi.fn();
const mockRefreshAccessToken = vi.fn(() => Promise.resolve<string | null>(null));

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  refreshAccessToken: () => mockRefreshAccessToken(),
}));

vi.mock('qrcode', () => ({
  default: {
    toDataURL: vi.fn().mockResolvedValue('data:image/png;base64,mockQRCode'),
  },
}));

vi.mock('@/renderer/utils/crypto/crypto', () => ({
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

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
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
    methods,
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
    <div data-testid="mfa-verify-prompt" data-methods={methods.join(',')}>
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

import { e2eeService as mockE2eeService } from '@/renderer/services/e2ee/e2eeService';
import {
  generateRecoveryKey,
  wrapWithRecoveryKey,
  wrapPrefsKeyWithRecoveryKey,
} from '@/renderer/utils/crypto/crypto';
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
      expect(
        mockRefreshAccessToken,
        'MFA did not activate, so there is no grant to use'
      ).not.toHaveBeenCalled();
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

    // Drives TOTP setup through confirm-setup to the recovery-key step.
    async function completeTOTPToRecoveryKey() {
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
      return user;
    }

    it('completes full TOTP flow through recovery key', async () => {
      const user = await completeTOTPToRecoveryKey();
      // The server exempts this session from the pre-MFA challenge for 30 s
      // after enrollment; refreshing now uses that grant instead of prompting
      // for the code again at the next token refresh.
      expect(mockRefreshAccessToken).toHaveBeenCalledTimes(1);

      await user.click(screen.getByTestId('recovery-confirm'));

      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });

      await user.click(screen.getByText('Done'));
      expect(onComplete).toHaveBeenCalled();
    });

    // The refresh is not awaited, so a rejection must be caught where it is
    // made; setup still finishes (frontend review, PR #3437).
    it('finishes TOTP setup when the refresh after enrollment rejects', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      mockRefreshAccessToken.mockRejectedValueOnce(new Error('ipc unavailable'));

      await completeTOTPToRecoveryKey();

      await vi.waitFor(() =>
        expect(warn).toHaveBeenCalledWith('[mfa] Refresh after enrollment failed')
      );
      warn.mockRestore();
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

    it('routes to recovery-failed (unavailable) when wrapping key is null — never silently to done', async () => {
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
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }); // confirm-setup

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        expect(
          screen.getByText(
            "We couldn't create your recovery key because your encryption keys aren't unlocked on this device. Without one, you'll lose access to your encrypted message history if you forget your password."
          )
        ).toBeInTheDocument();
      });
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
      // unavailable never offers a retry — retrying cannot succeed.
      expect(screen.queryByText('Try again')).not.toBeInTheDocument();

      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });
    });

    it('routes to recovery-failed (failed) when the recovery-key store call fails — never silently to done', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        // Recovery key store fails with a plain 500 — not 403, so this is
        // 'failed' rather than 'kept'.
        .mockResolvedValueOnce({
          ok: false,
          status: 500,
          json: async () => ({ error: 'storage error' }),
        });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        expect(
          screen.getByText(
            "We couldn't create your recovery key. Without one, you'll lose access to your encrypted message history if you forget your password."
          )
        ).toBeInTheDocument();
      });
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
      expect(screen.getByText('Try again')).toBeInTheDocument();

      await user.click(screen.getByText('Continue without a recovery key'));
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

    it('sets error field to password and shows the shared copy for an invalidPassword refusal', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: async () => ({ error: 'Invalid password' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'wrong');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => {
        expect(screen.getByText('That password is not correct.')).toBeInTheDocument();
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
        status: 403,
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
        // MFA error should be sent to the MFAVerifyPrompt (the shared copy,
        // not the server's raw text), not shown as a banner.
        expect(screen.getByTestId('mfa-verify-error')).toHaveTextContent(
          'That code is not correct, or it has expired. Try the next one.'
        );
      });
    });
  });

  // ── Recovery key exception handling ─────────────────────────────────

  describe('Recovery key generation exception', () => {
    it('routes to recovery-failed when generateAndStoreRecoveryKey throws — never silently to done', async () => {
      // Make crypto functions throw
      const { generateRecoveryKey } = await import('@/renderer/utils/crypto/crypto');
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
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }); // confirm-setup

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));

      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));

      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => {
        expect(
          screen.getByText(
            "We couldn't create your recovery key. Without one, you'll lose access to your encrypted message history if you forget your password."
          )
        ).toBeInTheDocument();
      });
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();

      await user.click(screen.getByText('Continue without a recovery key'));
      await vi.waitFor(() => {
        expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      });
    });
  });

  // ── Recovery-key outcome routing (spec §4.6.3 — the 'kept' branch) ───────

  describe('recovery-key outcome routing — kept', () => {
    const setupThroughBackupConfirm = async (user: ReturnType<typeof userEvent.setup>) => {
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));
    };

    it('routes to recovery-kept via a 403 password_required body', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockResolvedValueOnce({
          ok: false,
          status: 403,
          json: async () => ({ password_required: true }),
        });

      const user = userEvent.setup();
      await setupThroughBackupConfirm(user);

      await vi.waitFor(() => {
        expect(
          screen.getByText(/A recovery key is already saved for your account/)
        ).toBeInTheDocument();
      });
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
    });

    it('routes to recovery-kept via an mfa_required-only body (SSO-shaped, R-11)', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockResolvedValueOnce({
          ok: false,
          status: 403,
          json: async () => ({ mfa_required: true }),
        });

      const user = userEvent.setup();
      await setupThroughBackupConfirm(user);

      await vi.waitFor(() => {
        expect(
          screen.getByText(/A recovery key is already saved for your account/)
        ).toBeInTheDocument();
      });
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
    });
  });

  // ── Retry never re-calls confirm-setup (spec §4.6.3, "upload-only retry") ─

  describe('retry after recovery-failed', () => {
    it('calls only the key upload, never confirm-setup again', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup (#1)
        .mockResolvedValueOnce({
          ok: false,
          status: 500,
          json: async () => ({ error: 'storage error' }),
        }); // recovery-key PUT #1 fails

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => expect(screen.getByText('Try again')).toBeInTheDocument());

      const confirmSetupCallCount = () =>
        mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/totp/confirm-setup').length;
      expect(confirmSetupCallCount()).toBe(1);

      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) }); // retry succeeds
      await user.click(screen.getByText('Try again'));

      // A successful retry routes to 'created', the same target as the
      // first-try happy path — the recovery-key display, not straight to
      // 'done'.
      await vi.waitFor(() => {
        expect(screen.getByTestId('recovery-key-display')).toBeInTheDocument();
      });

      // The retry must not have called confirm-setup a second time — it had
      // already committed before the recovery-key upload ever ran.
      expect(confirmSetupCallCount()).toBe(1);
    });

    // regression: repeat retry failure re-rendered an identical screen
    it('changes the alert text on every repeat failure so a retry never looks inert', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockResolvedValueOnce({
          ok: false,
          status: 500,
          json: async () => ({ error: 'storage error' }),
        }); // recovery-key PUT #1 fails

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));

      await vi.waitFor(() => expect(screen.getByText('Try again')).toBeInTheDocument());
      const firstFailureText = screen.getByRole('alert').textContent;
      expect(firstFailureText).toContain('recovery key');
      const recoveryPutCount = () =>
        mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/recovery-key').length;
      expect(recoveryPutCount()).toBe(1);

      // Retry #1 fails again.
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        json: async () => ({ error: 'storage error' }),
      });
      await user.click(screen.getByText('Try again'));
      await vi.waitFor(() => expect(recoveryPutCount()).toBe(2));
      await vi.waitFor(() =>
        expect(screen.getByRole('button', { name: 'Try again' })).toBeEnabled()
      );
      const retry1Text = screen.getByRole('alert').textContent;
      expect(retry1Text).toContain('recovery key');

      // Retry #2 fails again.
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        json: async () => ({ error: 'storage error' }),
      });
      await user.click(screen.getByText('Try again'));
      await vi.waitFor(() => expect(recoveryPutCount()).toBe(3));
      await vi.waitFor(() =>
        expect(screen.getByRole('button', { name: 'Try again' })).toBeEnabled()
      );
      const retry2Text = screen.getByRole('alert').textContent;
      expect(retry2Text).toContain('recovery key');

      expect(retry1Text, 'a repeat failure must change the alert text').not.toBe(firstFailureText);
      expect(retry2Text, 'a repeat failure must change the alert text').not.toBe(retry1Text);
    });
  });

  // ── Replace step (spec §4.6.4, R-9) ───────────────────────────────────────

  describe('recovery-key replace step', () => {
    const setupToKept = async (user: ReturnType<typeof userEvent.setup>) => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ backup_codes: ['CODE1'] }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockResolvedValueOnce({
          ok: false,
          status: 403,
          json: async () => ({ password_required: true }),
        });

      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));
      await vi.waitFor(() =>
        expect(screen.getByRole('button', { name: 'Replace recovery key' })).toBeInTheDocument()
      );
    };

    it('Back returns to recovery-kept without submitting a request', async () => {
      const user = userEvent.setup();
      await setupToKept(user);
      const callsBefore = mockApiFetch.mock.calls.length;

      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() =>
        expect(screen.getByText('Your old recovery key will stop working.')).toBeInTheDocument()
      );

      await user.click(screen.getByText('Back'));
      await vi.waitFor(() => {
        expect(
          screen.getByText(/A recovery key is already saved for your account/)
        ).toBeInTheDocument();
      });
      expect(mockApiFetch.mock.calls.length).toBe(callsBefore);
    });

    it('happy path: submits the new key and lands on the recovery step showing it', async () => {
      const user = userEvent.setup();
      await setupToKept(user);

      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() =>
        expect(screen.getByText('Your old recovery key will stop working.')).toBeInTheDocument()
      );

      await user.type(screen.getByPlaceholderText('Your password'), 'freshpw');
      await user.type(screen.getByTestId('mfa-verify-input'), '654321');

      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) }); // overwrite PUT
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));

      await vi.waitFor(() => {
        expect(screen.getByTestId('recovery-key-display')).toBeInTheDocument();
        expect(screen.getByText('AAAA-BBBB-CCCC-DDDD')).toBeInTheDocument();
      });

      const call = [...mockApiFetch.mock.calls]
        .reverse()
        .find((c) => c[0] === '/api/v1/mfa/recovery-key');
      expect(call).toBeDefined();
      const body = JSON.parse((call![1] as { body: string }).body);
      expect(body.password).toBe('freshpw');
      expect(body.mfa_code).toBe('654321');
    });

    it('refusal routing: invalidPassword shows the field error and clears the password', async () => {
      const user = userEvent.setup();
      await setupToKept(user);

      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() =>
        expect(screen.getByText('Your old recovery key will stop working.')).toBeInTheDocument()
      );

      await user.type(screen.getByPlaceholderText('Your password'), 'wrongpw');
      await user.type(screen.getByTestId('mfa-verify-input'), '000000');

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: async () => ({ error: 'Invalid password' }),
      });
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));

      await vi.waitFor(() => {
        expect(screen.getByText('That password is not correct.')).toBeInTheDocument();
      });
      expect((screen.getByPlaceholderText('Your password') as HTMLInputElement).value).toBe('');
      // Refusal keeps the wizard on the replace step, not the recovery step.
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
    });

    it('refusal routing: invalidMfaCode shows the prompt error and clears the code', async () => {
      const user = userEvent.setup();
      await setupToKept(user);

      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() =>
        expect(screen.getByText('Your old recovery key will stop working.')).toBeInTheDocument()
      );

      await user.type(screen.getByPlaceholderText('Your password'), 'freshpw');
      await user.type(screen.getByTestId('mfa-verify-input'), '000000');

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: async () => ({ error: 'Invalid MFA code' }),
      });
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));

      await vi.waitFor(() => {
        expect(
          screen.getByText('That code is not correct, or it has expired. Try the next one.')
        ).toBeInTheDocument();
      });
      expect((screen.getByTestId('mfa-verify-input') as HTMLInputElement).value).toBe('');
    });
  });

  // ── I1: the replace step routes every refusal kind ────────────────────────

  describe('recovery-key replace step — refusal table (I1)', () => {
    const toReplaceStep = async (user: ReturnType<typeof userEvent.setup>) => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ backup_codes: ['CODE1'] }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockResolvedValueOnce({
          ok: false,
          status: 403,
          json: async () => ({ password_required: true }),
        }); // first store → kept
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));
      await vi.waitFor(() =>
        expect(screen.getByRole('button', { name: 'Replace recovery key' })).toBeInTheDocument()
      );
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() => expect(screen.getByLabelText('Password')).toBeInTheDocument());
    };

    const attempt = async (
      user: ReturnType<typeof userEvent.setup>,
      response: { ok: boolean; status: number; body: unknown } | 'network'
    ) => {
      await user.type(screen.getByLabelText('Password'), 'freshpw');
      await user.type(screen.getByTestId('mfa-verify-input'), '654321');
      if (response === 'network') {
        mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
      } else {
        mockApiFetch.mockResolvedValueOnce({
          ok: response.ok,
          status: response.status,
          json: async () => response.body,
        });
      }
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() => expect(screen.queryByText('Replacing...')).not.toBeInTheDocument());
    };

    const replaceButton = () => screen.getByRole('button', { name: 'Replace recovery key' });

    it.each([
      {
        name: 'passwordRequired → password field error, password cleared',
        response: { ok: false, status: 403, body: { password_required: true } },
        text: 'Enter your password to continue.',
        clearsPassword: true,
        locks: false,
      },
      {
        name: 'mfaRequired → code prompt error',
        response: { ok: false, status: 403, body: { mfa_required: true, methods: ['totp'] } },
        text: 'Verify with your authenticator app or security key to continue.',
        clearsPassword: false,
        locks: false,
      },
      {
        name: 'rateLimited → banner and lock',
        response: { ok: false, status: 429, body: { error: 'Too many verification attempts' } },
        text: 'Too many attempts. Try again in a few minutes.',
        clearsPassword: false,
        locks: true,
      },
      {
        name: 'sessionExpired → banner and lock',
        response: { ok: false, status: 401, body: {} },
        text: 'Your session needs to be verified again. Sign in again to continue.',
        clearsPassword: false,
        locks: true,
      },
      {
        name: 'unavailable → outage banner, no lock',
        response: { ok: false, status: 503, body: {} },
        text: 'Verification is temporarily unavailable. Try again in a few minutes.',
        clearsPassword: false,
        locks: false,
      },
      {
        name: 'failed → the server text in the banner',
        response: { ok: false, status: 500, body: { error: 'Verification failed' } },
        text: 'Verification failed',
        clearsPassword: false,
        locks: false,
      },
    ])('$name', async ({ response, text, clearsPassword, locks }) => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      await attempt(user, response);
      expect(await screen.findByText(text)).toBeInTheDocument();
      expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe(
        clearsPassword ? '' : 'freshpw'
      );
      if (locks) expect(replaceButton()).toBeDisabled();
      else if (!clearsPassword) expect(replaceButton()).toBeEnabled();
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
    });

    it('an mfa_required refusal prompts for the methods the server named', async () => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      await attempt(user, {
        ok: false,
        status: 403,
        body: { mfa_required: true, methods: ['webauthn', 'email'] },
      });
      await vi.waitFor(() =>
        expect(screen.getByTestId('mfa-verify-prompt').dataset.methods).toBe('webauthn')
      );
    });

    it('a lock survives Back and reopening the step (F10)', async () => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      await attempt(user, { ok: false, status: 429, body: {} });
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await user.type(screen.getByLabelText('Password'), 'freshpw');
      await user.type(screen.getByTestId('mfa-verify-input'), '654321');
      expect(replaceButton()).toBeDisabled();
      expect(
        screen.getByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
    });

    it('a password refusal focuses the password field once it is enabled again (F4)', async () => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      await attempt(user, { ok: false, status: 403, body: { error: 'Invalid password' } });
      await vi.waitFor(() =>
        expect(document.activeElement).toBe(screen.getByLabelText('Password'))
      );
    });

    it('after an ambiguous outcome, Back no longer claims the old key was left in place', async () => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      await attempt(user, 'network');
      await user.click(screen.getByRole('button', { name: 'Back' }));

      expect(screen.queryByText(/so we left it in place/)).not.toBeInTheDocument();
      expect(
        screen.getByText(/couldn't confirm whether your recovery key was replaced/)
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Finish replacing' })).toBeInTheDocument();

      await user.click(screen.getByRole('button', { name: 'Continue' }));
      expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      expect(
        screen.getByText(/couldn't confirm your recovery key was replaced/)
      ).toBeInTheDocument();
    });

    it('Finish replacing reopens the step and a success lands on the new key', async () => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      await attempt(user, 'network');
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await user.click(screen.getByRole('button', { name: 'Finish replacing' }));
      await attempt(user, { ok: true, status: 200, body: {} });
      await vi.waitFor(() =>
        expect(screen.getByTestId('recovery-key-display')).toBeInTheDocument()
      );
      await user.click(screen.getByTestId('recovery-confirm'));
      expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      expect(screen.queryByText(/couldn't confirm/)).not.toBeInTheDocument();
    });

    it('the lead line and the consequence come before the fields (F16)', async () => {
      const user = userEvent.setup();
      await toReplaceStep(user);
      const consequence = screen.getByText('Your old recovery key will stop working.');
      const password = screen.getByLabelText('Password');
      expect(screen.getByText(/^Make a new recovery key\./)).toBeInTheDocument();
      expect(
        consequence.compareDocumentPosition(password) & Node.DOCUMENT_POSITION_FOLLOWING
      ).toBeTruthy();
    });
  });

  // ── F16: the done screen says plainly when there is no usable key ────────

  it('continuing without a recovery key says so on the done screen (F16)', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ backup_codes: ['CODE1'] }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
      .mockResolvedValueOnce({ ok: false, status: 500, json: async () => ({}) });
    const user = userEvent.setup();
    render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
    await user.type(screen.getByPlaceholderText('Your password'), 'pw');
    await user.click(screen.getByText('Continue'));
    await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
    await user.click(screen.getByTestId('totp-submit'));
    await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
    await user.click(screen.getByTestId('backup-confirm'));
    await vi.waitFor(() =>
      expect(screen.getByText('Continue without a recovery key')).toBeInTheDocument()
    );
    await user.click(screen.getByText('Continue without a recovery key'));
    expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
    expect(screen.getByText(/This account has no recovery key you can use\./)).toBeInTheDocument();
  });

  // ── F3's twin on the password step ───────────────────────────────────────

  it('an mfa_required answer on the password step shows the prompt even when mfaActive was false', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({
        error: 'MFA verification required',
        mfa_required: true,
        methods: ['totp', 'email'],
      }),
    });
    const user = userEvent.setup();
    render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.queryByTestId('mfa-verify-prompt')).not.toBeInTheDocument();
    await user.type(screen.getByPlaceholderText('Your password'), 'pw');
    await user.click(screen.getByText('Continue'));

    const prompt = await screen.findByTestId('mfa-verify-prompt');
    expect(prompt.dataset.methods).toBe('totp');
    expect(screen.getByText('Continue')).toBeDisabled();
    await user.type(screen.getByTestId('mfa-verify-input'), '123456');
    expect(screen.getByText('Continue')).toBeEnabled();
  });

  // ── Recovery-key identity across an ambiguous retry (C4, F1/F2) ─────────
  //
  // The module mock returns ONE key for every call, which makes "reused the
  // prepared key" and "minted a new key per attempt" indistinguishable. These
  // cases hand out a DISTINCT key per call and derive every wrapped field from
  // it, so the uploaded body names the key it wraps. A lost response after the
  // server committed is the ambiguous outcome: the retry must resend the same
  // bytes, and the key shown must be the key those bytes wrap.

  describe('recovery-key identity across an ambiguous retry (C4)', () => {
    let minted = 0;

    beforeEach(() => {
      minted = 0;
      vi.mocked(generateRecoveryKey).mockImplementation(() => {
        minted += 1;
        return `KEY-${minted}`;
      });
      vi.mocked(wrapWithRecoveryKey).mockImplementation(async (_blob, _wrapping, key) => ({
        wrappedKey: `wrapped(${key})`,
        salt: `salt(${key})`,
      }));
      vi.mocked(wrapPrefsKeyWithRecoveryKey).mockImplementation(async (_prefs, key) => ({
        wrappedKey: `prefs(${key})`,
        salt: `prefs-salt(${key})`,
      }));
    });

    afterEach(() => {
      vi.mocked(generateRecoveryKey).mockReset().mockReturnValue('AAAA-BBBB-CCCC-DDDD');
      vi.mocked(wrapWithRecoveryKey)
        .mockReset()
        .mockResolvedValue({ wrappedKey: 'mock-wrapped-key', salt: 'mock-salt' });
      vi.mocked(wrapPrefsKeyWithRecoveryKey)
        .mockReset()
        .mockResolvedValue({ wrappedKey: 'mock-wrapped-prefs', salt: 'mock-prefs-salt' });
    });

    /** The recovery-* fields of every recovery-key PUT, in call order. */
    const recoveryBodies = () =>
      mockApiFetch.mock.calls
        .filter((c) => c[0] === '/api/v1/mfa/recovery-key')
        .map((c) => {
          const body = JSON.parse((c[1] as { body: string }).body) as Record<string, string>;
          return {
            recovery_wrapped_private_key: body.recovery_wrapped_private_key,
            recovery_key_salt: body.recovery_key_salt,
            recovery_wrapped_prefs_key: body.recovery_wrapped_prefs_key,
            recovery_prefs_key_salt: body.recovery_prefs_key_salt,
          };
        });

    /** The key a body wraps, read back out of `wrapped(<key>)`. */
    const keyWrappedBy = (wrapped: string) => /^wrapped\((.+)\)$/.exec(wrapped)?.[1];

    it('first store: a retry after a lost response resends identical bytes and shows the key they wrap', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ backup_codes: ['CODE1'] }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockRejectedValueOnce(new TypeError('Failed to fetch')); // PUT #1: response lost

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));
      await vi.waitFor(() => expect(screen.getByText('Try again')).toBeInTheDocument());

      // The server committed PUT #1 and the idempotent re-store answers 200.
      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });
      await user.click(screen.getByText('Try again'));
      await vi.waitFor(() =>
        expect(screen.getByTestId('recovery-key-display')).toBeInTheDocument()
      );

      const [first, retry] = recoveryBodies();
      expect(retry, 'the retry must resend the exact bytes of the first attempt').toEqual(first);
      expect(screen.getByTestId('recovery-key').textContent).toBe(
        keyWrappedBy(retry.recovery_wrapped_private_key)
      );
    });

    it('replace: a retry after a network error resends identical bytes and shows the key they wrap', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ otpauth_url: 'otpauth://totp/test', secret: 'S' }),
        })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ backup_codes: ['CODE1'] }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({}) }) // confirm-setup
        .mockResolvedValueOnce({
          ok: false,
          status: 403,
          json: async () => ({ password_required: true }),
        }); // first store: a key already exists → kept

      const user = userEvent.setup();
      render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'pw');
      await user.click(screen.getByText('Continue'));
      await vi.waitFor(() => expect(screen.getByTestId('totp-submit')).toBeInTheDocument());
      await user.click(screen.getByTestId('totp-submit'));
      await vi.waitFor(() => expect(screen.getByTestId('backup-confirm')).toBeInTheDocument());
      await user.click(screen.getByTestId('backup-confirm'));
      await vi.waitFor(() =>
        expect(screen.getByRole('button', { name: 'Replace recovery key' })).toBeInTheDocument()
      );

      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() => expect(screen.getByLabelText('Password')).toBeInTheDocument());
      await user.type(screen.getByLabelText('Password'), 'freshpw');
      await user.type(screen.getByTestId('mfa-verify-input'), '654321');

      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch')); // replace #1 lost
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() =>
        expect(
          screen.getByText("Couldn't reach the server. Check your connection and try again.")
        ).toBeInTheDocument()
      );

      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) }); // replace #2
      await user.click(screen.getByRole('button', { name: 'Replace recovery key' }));
      await vi.waitFor(() =>
        expect(screen.getByTestId('recovery-key-display')).toBeInTheDocument()
      );

      // Body [0] is the credential-less first store (kept); [1] and [2] are the
      // two replace attempts.
      const [, attempt, retry] = recoveryBodies();
      expect(retry, 'the retry must resend the exact bytes of the first attempt').toEqual(attempt);
      expect(screen.getByTestId('recovery-key').textContent).toBe(
        keyWrappedBy(retry.recovery_wrapped_private_key)
      );
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

    // Wrong-password and rate-limited begin refusals now route through the
    // shared step-up classifier and copy (mfaStepUp.ts) instead of showing
    // the server's raw text, matching every other step-up surface.
    it('shows the shared copy against the password field for an invalidPassword begin refusal', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: async () => ({ error: 'Invalid password' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'wrongpw');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(screen.getByText('That password is not correct.')).toBeInTheDocument();
        expect(screen.getByPlaceholderText('Your password').className).toContain('error');
      });
    });

    it('shows the shared rate-limit copy for a 429 begin refusal', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        json: async () => ({ error: 'Too many verification attempts' }),
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'wrongpw');
      await user.click(screen.getByText('Register Key'));

      await vi.waitFor(() => {
        expect(
          screen.getByText('Too many attempts. Try again in a few minutes.')
        ).toBeInTheDocument();
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

    // Registers a security key whose finish request answers finishOk.
    async function registerSecurityKey(finishOk: boolean) {
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
          ok: finishOk,
          json: async () => (finishOk ? {} : { error: 'Registration failed' }),
        });
      const buffer = new Uint8Array([1, 2, 3]).buffer;
      Object.defineProperty(navigator, 'credentials', {
        value: {
          create: vi.fn().mockResolvedValue({
            id: 'credential-id',
            rawId: buffer,
            type: 'public-key',
            response: { attestationObject: buffer, clientDataJSON: buffer },
          }),
        },
        writable: true,
        configurable: true,
      });

      const user = userEvent.setup();
      render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} />);
      await user.type(screen.getByPlaceholderText('Your password'), 'mypassword');
      await user.click(screen.getByText('Register Key'));
      await vi.waitFor(() =>
        expect(
          screen.getByText(finishOk ? 'Security Key Registered!' : 'Registration failed')
        ).toBeInTheDocument()
      );
    }

    // The server exempts the registering session from the pre-MFA challenge for
    // 30 s after a first enrollment; refreshing now uses that grant instead of
    // prompting for the new factor again at the next token refresh.
    it.each([
      [true, 1],
      [false, 0],
    ] as const)(
      'refreshes the session right after registration only when it succeeds (finish ok: %s)',
      async (finishOk, refreshes) => {
        await registerSecurityKey(finishOk);
        expect(mockRefreshAccessToken).toHaveBeenCalledTimes(refreshes);
      }
    );

    it('finishes registration when the refresh after it rejects', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      mockRefreshAccessToken.mockRejectedValueOnce(new Error('ipc unavailable'));

      await registerSecurityKey(true);

      await vi.waitFor(() =>
        expect(warn).toHaveBeenCalledWith('[mfa] Refresh after enrollment failed')
      );
      warn.mockRestore();
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
