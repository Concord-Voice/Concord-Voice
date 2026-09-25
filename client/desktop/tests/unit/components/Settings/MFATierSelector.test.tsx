import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { vi } from 'vitest';

// ── Service mock ─────────────────────────────────────────────────────────────
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    json: async () => ({}),
  }),
}));

// ── Child component mocks ────────────────────────────────────────────────────
vi.mock('@/renderer/components/Settings/ToggleSwitch', () => ({
  default: ({
    checked,
    onChange,
    disabled,
  }: {
    checked: boolean;
    onChange: (v: boolean) => void;
    disabled?: boolean;
  }) => (
    <input
      type="checkbox"
      data-testid="toggle-switch"
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
      disabled={disabled}
    />
  ),
}));

// Captures the methods/excludeBackupCodes props each render passes, and wires
// onVerify to a real input so tests can drive the MFA code field (T14, and the
// Confirm-enablement rule for the two step-up-gated action types).
vi.mock('@/renderer/components/Auth/MFAVerifyPrompt', () => ({
  default: ({
    methods,
    excludeBackupCodes,
    onVerify,
    disabled,
    error,
  }: {
    methods: string[];
    excludeBackupCodes?: boolean;
    onVerify: (code: string) => void;
    disabled?: boolean;
    error?: string;
  }) => (
    <div
      data-testid="mfa-verify-prompt"
      data-methods={methods.join(',')}
      data-exclude-backup-codes={String(!!excludeBackupCodes)}
    >
      <input
        data-testid="mfa-verify-input"
        disabled={disabled}
        onChange={(e) => onVerify(e.target.value)}
      />
      {error && <span>{error}</span>}
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/RecoveryApprovalModal', () => ({
  default: () => <div data-testid="recovery-approval-modal" />,
}));

vi.mock('@/renderer/components/Settings/RecoveryCircle', () => ({
  default: () => <div data-testid="recovery-circle" />,
}));

vi.mock('@/renderer/components/Settings/MFA.css', () => ({}));

import MFATierSelector from '@/renderer/components/Settings/MFATierSelector';
import { apiFetch } from '@/renderer/services/system/apiClient';

// ── Default props ────────────────────────────────────────────────────────────
const defaultProps = {
  activeMethods: [] as string[],
  recoveryOnlyMethods: [] as string[],
  recoveryHardened: false,
  backupCodesRemaining: 0,
  webauthnCredentials: [],
  backupEmail: '',
  onSetupTOTP: vi.fn(),
  onSetupWebAuthn: vi.fn(),
  onSetupEmailSms: vi.fn(),
  onToggleRecoveryOnly: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onToggleRecoveryHardened: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onResetTOTP: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onRevokeWebAuthnKey: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onDisableEmailSms: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onSetBackupEmail: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
};

describe('MFATierSelector', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── Basic rendering ──────────────────────────────────────────────────────

  it('renders all four MFA tier cards', () => {
    render(<MFATierSelector {...defaultProps} />);
    expect(screen.getByText(/Maximum — Hardware Keys/)).toBeInTheDocument();
    expect(screen.getByText(/Strong — Platform Authenticator/)).toBeInTheDocument();
    expect(screen.getByText(/Standard — Authenticator App/)).toBeInTheDocument();
    expect(screen.getByText(/Last Resort — Email/)).toBeInTheDocument();
  });

  it('renders tier descriptions', () => {
    render(<MFATierSelector {...defaultProps} />);
    expect(screen.getByText(/Fort Knox mode/)).toBeInTheDocument();
    expect(screen.getByText(/Your device IS the key/)).toBeInTheDocument();
    expect(screen.getByText(/The classic\. 6 digits/)).toBeInTheDocument();
    expect(screen.getByText(/Better than nothing/)).toBeInTheDocument();
  });

  it('renders method tags for each tier', () => {
    render(<MFATierSelector {...defaultProps} />);
    expect(screen.getByText('YubiKey')).toBeInTheDocument();
    expect(screen.getByText('Windows Hello')).toBeInTheDocument();
    expect(screen.getByText('Google Authenticator')).toBeInTheDocument();
    expect(screen.getByText('Email code')).toBeInTheDocument();
  });

  // ── Locked / unlocked state ──────────────────────────────────────────────

  it('shows lock overlay for Last Resort when no real MFA is set up', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={[]} />);
    expect(screen.getByText('Enable a Standard or higher MFA method first')).toBeInTheDocument();
  });

  it('removes lock overlay for Last Resort when real MFA is active', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    expect(
      screen.queryByText('Enable a Standard or higher MFA method first')
    ).not.toBeInTheDocument();
  });

  // ── Setup buttons ────────────────────────────────────────────────────────

  it('renders Set Up button for standard tier when not active', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={[]} />);
    const setupButtons = screen.getAllByText('Set Up');
    // Maximum, Strong, and Standard should each have Set Up (Last Resort is locked)
    expect(setupButtons.length).toBe(3);
  });

  it('calls onSetupTOTP when standard tier Set Up is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={[]} />);
    // The standard tier is the third Set Up button
    const setupButtons = screen.getAllByText('Set Up');
    // Standard is the 3rd in order (Maximum, Strong, Standard)
    fireEvent.click(setupButtons[2]);
    expect(defaultProps.onSetupTOTP).toHaveBeenCalled();
  });

  it('calls onSetupWebAuthn with hardware when maximum tier Set Up is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={[]} />);
    const setupButtons = screen.getAllByText('Set Up');
    fireEvent.click(setupButtons[0]); // Maximum is first
    expect(defaultProps.onSetupWebAuthn).toHaveBeenCalledWith('hardware');
  });

  it('calls onSetupWebAuthn with platform when strong tier Set Up is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={[]} />);
    const setupButtons = screen.getAllByText('Set Up');
    fireEvent.click(setupButtons[1]); // Strong is second
    expect(defaultProps.onSetupWebAuthn).toHaveBeenCalledWith('platform');
  });

  it('calls onSetupEmailSms when last resort Set Up is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    // Last resort should now have Set Up since totp unlocks it
    const setupButtons = screen.getAllByText('Set Up');
    const lastButton = setupButtons[setupButtons.length - 1];
    fireEvent.click(lastButton);
    expect(defaultProps.onSetupEmailSms).toHaveBeenCalled();
  });

  // ── Active tiers ─────────────────────────────────────────────────────────

  it('shows Configured badge when TOTP is active', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    expect(screen.getByText('Configured')).toBeInTheDocument();
  });

  it('shows Recovery Only badge when tier is recovery-only', () => {
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        recoveryOnlyMethods={['email']}
      />
    );
    expect(screen.getByText('Recovery Only')).toBeInTheDocument();
  });

  it('shows Reset button for active TOTP tier', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    expect(screen.getByText('Reset')).toBeInTheDocument();
  });

  it('shows Disable button for active email/SMS tier', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
    expect(screen.getByText('Disable')).toBeInTheDocument();
  });

  // ── WebAuthn credentials ─────────────────────────────────────────────────

  it('displays WebAuthn hardware credentials in maximum tier', () => {
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['webauthn']}
        webauthnCredentials={[
          {
            id: 'cred-1',
            credential_name: 'My YubiKey',
            credential_type: 'hardware',
            created_at: '2026-01-01T00:00:00Z',
            last_used_at: '2026-03-01T00:00:00Z',
          },
        ]}
      />
    );
    expect(screen.getByText('My YubiKey')).toBeInTheDocument();
    expect(screen.getByText('Revoke')).toBeInTheDocument();
  });

  it('shows + Add Another Key button for WebAuthn tiers', () => {
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['webauthn']}
        webauthnCredentials={[
          {
            id: 'cred-1',
            credential_name: 'Key 1',
            credential_type: 'hardware',
            created_at: '2026-01-01T00:00:00Z',
          },
        ]}
      />
    );
    expect(screen.getByText('+ Add Another Key')).toBeInTheDocument();
  });

  // ── Action modal ─────────────────────────────────────────────────────────

  it('opens action modal when Reset TOTP is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    expect(screen.getByText('Reset TOTP')).toBeInTheDocument();
    expect(
      screen.getByText(/This will remove your authenticator app enrollment/)
    ).toBeInTheDocument();
  });

  it('opens action modal when Disable Email/SMS is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
    fireEvent.click(screen.getByText('Disable'));
    expect(screen.getByText('Disable Email/SMS')).toBeInTheDocument();
  });

  it('opens action modal when Revoke WebAuthn key is clicked', () => {
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['webauthn']}
        webauthnCredentials={[
          {
            id: 'cred-1',
            credential_name: 'My YubiKey',
            credential_type: 'hardware',
            created_at: '2026-01-01T00:00:00Z',
          },
        ]}
      />
    );
    fireEvent.click(screen.getByText('Revoke'));
    expect(screen.getByText(/Revoke "My YubiKey"/)).toBeInTheDocument();
  });

  it('closes action modal when Cancel is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    expect(screen.getByText('Reset TOTP')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Cancel'));
    expect(screen.queryByText('Reset TOTP')).not.toBeInTheDocument();
  });

  it('shows password input in action modal', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument();
  });

  it('disables Confirm button when password is empty', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    expect(screen.getByText('Confirm')).toBeDisabled();
  });

  it('enables Reset TOTP Confirm once the password and an MFA code are entered', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
    expect(screen.getByText('Confirm')).not.toBeDisabled();
  });

  it('calls onResetTOTP when Confirm is clicked in reset modal', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
    await act(async () => {
      fireEvent.click(screen.getByText('Confirm'));
    });
    expect(defaultProps.onResetTOTP).toHaveBeenCalledWith('mypassword', '123456');
  });

  it('shows the server text when an action fails with a body the seam does not classify', async () => {
    // Any body the classifier does not recognise arrives on `failed` with the
    // server's text. Wrong-password and wrong-code refusals do not take this
    // path: every route that gates these actions sends the seam's exact bodies.
    const failReset = vi.fn().mockResolvedValue({
      kind: 'failed',
      message: 'The authenticator app could not be reset. Try again.',
    });
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} onResetTOTP={failReset} />);
    fireEvent.click(screen.getByText('Reset'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
    await act(async () => {
      fireEvent.click(screen.getByText('Confirm'));
    });
    await vi.waitFor(() =>
      expect(
        screen.getByText('The authenticator app could not be reset. Try again.')
      ).toBeInTheDocument()
    );
  });

  // ── Recovery Key section ─────────────────────────────────────────────────

  it('shows Recovery Key section when real MFA is active', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    await vi.waitFor(() => expect(screen.getByText('Recovery Key')).toBeInTheDocument());
  });

  it('does not show Recovery Key section when no real MFA', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={[]} />);
    expect(screen.queryByText('Recovery Key')).not.toBeInTheDocument();
  });

  // ── Trusted Devices section ──────────────────────────────────────────────

  it('shows Trusted Devices section when real MFA is active', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    await vi.waitFor(() => expect(screen.getByText('Trusted Devices')).toBeInTheDocument());
  });

  it('shows Designate This Device button', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    await vi.waitFor(() => expect(screen.getByText('Designate This Device')).toBeInTheDocument());
  });

  // ── Recovery Circle section ──────────────────────────────────────────────

  it('shows Recovery Circle section when real MFA is active', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    await vi.waitFor(() => expect(screen.getByText('Recovery Circle')).toBeInTheDocument());
  });

  it('shows Set Up Recovery Circle button by default', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    await vi.waitFor(() => expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument());
  });

  // ── Sole MFA protection ──────────────────────────────────────────────────

  it('disables Reset button when TOTP is sole MFA and Email/SMS is active', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
    expect(screen.getByText('Reset')).toBeDisabled();
  });

  it('shows warning hint when sole MFA protection is active', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
    expect(
      screen.getByText(/Disable Email\/SMS before resetting your only MFA method/)
    ).toBeInTheDocument();
  });

  // ── Backup email ─────────────────────────────────────────────────────────

  it('shows backup email section for active last-resort tier', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} backupEmail="" />);
    expect(screen.getByText('Backup Email')).toBeInTheDocument();
  });

  it('shows Add button when no backup email is set', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} backupEmail="" />);
    expect(screen.getByText('Add')).toBeInTheDocument();
  });

  it('shows Change button when backup email is set', () => {
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        backupEmail="backup@example.com"
      />
    );
    expect(screen.getByText('Change')).toBeInTheDocument();
  });

  it('shows email input when Add/Change is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} backupEmail="" />);
    fireEvent.click(screen.getByText('Add'));
    expect(screen.getByPlaceholderText('backup@example.com')).toBeInTheDocument();
  });

  it('validates email format on save', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} backupEmail="" />);
    fireEvent.click(screen.getByText('Add'));
    fireEvent.change(screen.getByPlaceholderText('backup@example.com'), {
      target: { value: 'invalid-email' },
    });
    await act(async () => {
      fireEvent.click(screen.getByText('Save'));
    });
    expect(screen.getByText('Please enter a valid email address.')).toBeInTheDocument();
  });

  it('Save opens the step-up modal instead of calling onSetBackupEmail directly', () => {
    const onSetBackupEmail = vi.fn();
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        backupEmail=""
        onSetBackupEmail={onSetBackupEmail}
      />
    );
    fireEvent.click(screen.getByText('Add'));
    fireEvent.change(screen.getByPlaceholderText('backup@example.com'), {
      target: { value: 'valid@example.com' },
    });
    fireEvent.click(screen.getByText('Save'));

    expect(onSetBackupEmail).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Save backup email' })).toBeInTheDocument();
  });

  it('Confirm in the backup-email modal calls onSetBackupEmail with the pending email, password and code', async () => {
    const onSetBackupEmail = vi
      .fn()
      .mockResolvedValue({ kind: 'accepted', data: { backup_email: 'valid@example.com' } });
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        backupEmail=""
        onSetBackupEmail={onSetBackupEmail}
      />
    );
    fireEvent.click(screen.getByText('Add'));
    fireEvent.change(screen.getByPlaceholderText('backup@example.com'), {
      target: { value: 'valid@example.com' },
    });
    fireEvent.click(screen.getByText('Save'));

    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypw' },
    });
    fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '654321' } });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Save backup email' }));
    });

    expect(onSetBackupEmail).toHaveBeenCalledWith('valid@example.com', 'mypw', '654321');
  });

  it('hides email input when Cancel is clicked', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} backupEmail="" />);
    fireEvent.click(screen.getByText('Add'));
    expect(screen.getByPlaceholderText('backup@example.com')).toBeInTheDocument();
    fireEvent.click(screen.getAllByText('Cancel')[0]);
    expect(screen.queryByPlaceholderText('backup@example.com')).not.toBeInTheDocument();
  });

  // ── Hardened mode toggle ─────────────────────────────────────────────────

  it('shows hardened mode toggle for active last-resort tier', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
    expect(screen.getByText('Hardened mode')).toBeInTheDocument();
  });

  it('shows recovery-only preview text for eligible but inactive tiers', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    expect(
      screen.getByText('Once set up, this method can be restricted to account recovery only.')
    ).toBeInTheDocument();
  });

  // ── Action modal — dialog semantics and focus trap (spec §1.1, WCAG 2.4.3) ─

  describe('action modal — dialog role and focus trap', () => {
    it('renders with role dialog and moves initial focus to the password field', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));

      expect(screen.getByRole('dialog')).toBeInTheDocument();
      const password = screen.getByPlaceholderText('Enter your password');
      await vi.waitFor(() => expect(document.activeElement).toBe(password));
    });

    // Confirm starts disabled (no password yet), so `getFocusable` excludes
    // it — the modal's actual first/last focusable elements are the header's
    // Close button and the footer's Cancel button, not the password field
    // (which only holds *initial* focus via `initialFocusRef`, a distinct
    // mechanism from DOM order). Compute first/last from the live DOM rather
    // than assuming the password field occupies either position.
    const focusablesIn = (container: HTMLElement) =>
      Array.from(
        container.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        )
      ).filter((el) => !el.hasAttribute('disabled'));

    it('Shift+Tab from the first focusable element wraps to the last', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));

      const dialog = screen.getByRole('dialog');
      const password = screen.getByPlaceholderText('Enter your password');
      await vi.waitFor(() => expect(document.activeElement).toBe(password));

      const focusables = focusablesIn(dialog);
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      expect(first).not.toBe(last); // otherwise the wrap assertion below is vacuous

      first.focus();
      fireEvent.keyDown(document, { key: 'Tab', shiftKey: true });
      expect(document.activeElement).toBe(last);
    });

    it('Tab from the last focusable element wraps to the first', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));

      const dialog = screen.getByRole('dialog');
      const password = screen.getByPlaceholderText('Enter your password');
      await vi.waitFor(() => expect(document.activeElement).toBe(password));

      const focusables = focusablesIn(dialog);
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      expect(first).not.toBe(last);

      last.focus();
      fireEvent.keyDown(document, { key: 'Tab' });
      expect(document.activeElement).toBe(first);
    });
  });

  // ── Action modal — Confirm-enablement rule for the two step-up-gated types ─
  // (spec R-4: the code is required up front so a password-only submit cannot
  // burn a shared attempt on a request that must fail with mfa_required.)

  describe('action modal — Confirm enablement (step-up-gated types)', () => {
    it('requires password AND an MFA code when the account holds real MFA', () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      const confirm = () => screen.getByRole('button', { name: 'Disable Email/SMS' });

      expect(confirm()).toBeDisabled();
      fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
        target: { value: 'pw' },
      });
      expect(confirm()).toBeDisabled(); // password alone is not enough — a real MFA method exists
      fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
      expect(confirm()).not.toBeDisabled();
    });

    it('enables on password alone when the account has no real MFA method', () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['email']} />);
      fireEvent.click(screen.getByText('Disable'));
      const confirm = () => screen.getByRole('button', { name: 'Disable Email/SMS' });

      expect(confirm()).toBeDisabled();
      fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
        target: { value: 'pw' },
      });
      expect(confirm()).not.toBeDisabled();
    });
  });

  // ── Action modal — MFA methods filter + excludeBackupCodes (spec R-13/T14) ─

  describe('action modal — MFAVerifyPrompt methods filter', () => {
    it('passes only the totp/webauthn subset, excluding email/sms', () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'webauthn', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      const prompt = screen.getByTestId('mfa-verify-prompt');
      expect(prompt.dataset.methods).toBe('totp,webauthn');
      expect(prompt.dataset.excludeBackupCodes).toBe('false');
    });

    it('sets excludeBackupCodes when totp is absent (backup codes verify only via TOTP)', () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['webauthn', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      const prompt = screen.getByTestId('mfa-verify-prompt');
      expect(prompt.dataset.methods).toBe('webauthn');
      expect(prompt.dataset.excludeBackupCodes).toBe('true');
    });
  });

  // ── Action modal — MfaStepUpResult routing (spec §4.3, handoff §1.1) ───────
  //
  // Each `kind` a gated handler can return routes to a distinct, observable
  // target: a specific field error, the general banner, a lock, or the modal
  // closing. The switch in `applyRefusal` has exactly one case per kind and no
  // other code path produces these strings, so each assertion below pins its
  // own case rather than sharing an outcome with a sibling case.

  describe('action modal — MfaStepUpResult kind routing', () => {
    const renderDisableModal = (onDisableEmailSms: ReturnType<typeof vi.fn>) => {
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
    };

    const fillCredentials = () => {
      fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
        target: { value: 'mypassword' },
      });
      fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
    };

    const submit = async () => {
      await act(async () => {
        fireEvent.click(screen.getByRole('button', { name: 'Disable Email/SMS' }));
      });
    };

    it('accepted: closes the modal', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('passwordRequired: shows the field error and clears the password', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'passwordRequired' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
      expect((screen.getByPlaceholderText('Enter your password') as HTMLInputElement).value).toBe(
        ''
      );
    });

    it('invalidPassword: shows the field error and clears the password', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'invalidPassword' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect((screen.getByPlaceholderText('Enter your password') as HTMLInputElement).value).toBe(
        ''
      );
    });

    it('mfaRequired: shows the MFA-prompt error and keeps the password', async () => {
      const onDisableEmailSms = vi
        .fn()
        .mockResolvedValue({ kind: 'mfaRequired', methods: ['totp'] });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(
        await screen.findByText('Verify with your authenticator app or security key to continue.')
      ).toBeInTheDocument();
      expect((screen.getByPlaceholderText('Enter your password') as HTMLInputElement).value).toBe(
        'mypassword'
      );
    });

    it('invalidMfaCode: shows the MFA-prompt error and clears the code field', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'invalidMfaCode' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(
        await screen.findByText('That code is not correct, or it has expired. Try the next one.')
      ).toBeInTheDocument();
      // The prompt remounts (key bump) empty rather than showing the rejected code.
      expect((screen.getByTestId('mfa-verify-input') as HTMLInputElement).value).toBe('');
    });

    it('rateLimited: shows the banner and locks Confirm', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'rateLimited' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(
        await screen.findByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).toBeDisabled();
    });

    it('sessionExpired: shows the banner and locks Confirm', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'sessionExpired' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(
        await screen.findByText(
          'Your session needs to be verified again. Sign in again to continue.'
        )
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).toBeDisabled();
    });

    it('networkError: shows the banner, keeps the fields, and re-enables Confirm', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'networkError' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(
        await screen.findByText("Couldn't reach the server. Check your connection and try again.")
      ).toBeInTheDocument();
      expect((screen.getByPlaceholderText('Enter your password') as HTMLInputElement).value).toBe(
        'mypassword'
      );
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).not.toBeDisabled();
    });

    it('failed: shows the generic banner', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'failed' });
      renderDisableModal(onDisableEmailSms);
      fillCredentials();
      await submit();
      expect(await screen.findByText('Something went wrong. Try again.')).toBeInTheDocument();
    });
  });

  // ── I2 (F3): a stale activeMethods must not hide the code prompt ──────────
  //
  // `activeMethods` is the last status fetch; the server's `methods` on an
  // mfa_required refusal is the present truth. With no real MFA in the stale
  // list the modal rendered no prompt, so an mfa_required answer left the user
  // with a password field, an error they could not act on, and a Confirm that
  // spent another of the five shared attempts on every click.

  describe('action modal — stale methods (I2)', () => {
    it('renders the code prompt from refusal.methods after an mfa_required refusal', async () => {
      const onDisableEmailSms = vi
        .fn()
        .mockResolvedValueOnce({ kind: 'mfaRequired', methods: ['totp', 'email'] })
        .mockResolvedValueOnce({ kind: 'accepted', data: null });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
      expect(screen.queryByTestId('mfa-verify-prompt')).not.toBeInTheDocument();

      fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
        target: { value: 'pw' },
      });
      const confirm = () => screen.getByRole('button', { name: 'Disable Email/SMS' });
      await act(async () => {
        fireEvent.click(confirm());
      });

      const prompt = await screen.findByTestId('mfa-verify-prompt');
      // Only the inline-verifiable subset of the server's list (policy P1).
      expect(prompt.dataset.methods).toBe('totp');
      expect(
        screen.getByText('Verify with your authenticator app or security key to continue.')
      ).toBeInTheDocument();
      // The code is now required before another attempt is spent.
      expect(confirm()).toBeDisabled();

      fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
      expect(confirm()).not.toBeDisabled();
      await act(async () => {
        fireEvent.click(confirm());
      });
      expect(onDisableEmailSms).toHaveBeenLastCalledWith('pw', '123456');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
  });

  // ── I4: every action routes through the step-up result, with its own args ──

  describe('action modal — all six actions route and pass their arguments (I4)', () => {
    const fillPassword = (value = 'pw') =>
      fireEvent.change(screen.getByPlaceholderText('Enter your password'), { target: { value } });
    const fillCode = (value = '123456') =>
      fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value } });
    const confirm = async (name: string) => {
      await act(async () => {
        fireEvent.click(screen.getByRole('button', { name }));
      });
    };

    it('reset-totp → onResetTOTP(password, code), then closes', async () => {
      const onResetTOTP = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      render(
        <MFATierSelector {...defaultProps} activeMethods={['totp']} onResetTOTP={onResetTOTP} />
      );
      fireEvent.click(screen.getByText('Reset'));
      fillPassword();
      fillCode();
      await confirm('Confirm');
      expect(onResetTOTP).toHaveBeenCalledWith('pw', '123456');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('revoke-webauthn → onRevokeWebAuthnKey(credentialId, password) on the password alone', async () => {
      const onRevokeWebAuthnKey = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['webauthn']}
          webauthnCredentials={[
            {
              id: 'cred-1',
              credential_name: 'Key A',
              credential_type: 'hardware',
              created_at: '2026-01-01T00:00:00Z',
            },
          ]}
          onRevokeWebAuthnKey={onRevokeWebAuthnKey}
        />
      );
      fireEvent.click(screen.getByText('Revoke'));
      fillPassword();
      await confirm('Confirm');
      expect(onRevokeWebAuthnKey).toHaveBeenCalledWith('cred-1', 'pw');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('toggle-recovery-only → onToggleRecoveryOnly(method, value, password, code), code required', async () => {
      const onToggleRecoveryOnly = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onToggleRecoveryOnly={onToggleRecoveryOnly}
        />
      );
      fireEvent.click(screen.getAllByTestId('toggle-switch')[0]);
      fillPassword();
      // Now on the step-up seam: a password alone would spend an attempt on a
      // request that must fail with mfa_required.
      expect(screen.getByRole('button', { name: 'Confirm' })).toBeDisabled();
      fillCode();
      await confirm('Confirm');
      expect(onToggleRecoveryOnly).toHaveBeenCalledWith('email', true, 'pw', '123456');
    });

    it('toggle-hardened → onToggleRecoveryHardened(value, password, code)', async () => {
      const onToggleRecoveryHardened = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          recoveryHardened={false}
          onToggleRecoveryHardened={onToggleRecoveryHardened}
        />
      );
      fireEvent.click(screen.getAllByTestId('toggle-switch')[1]);
      fillPassword();
      fillCode();
      await confirm('Confirm');
      expect(onToggleRecoveryHardened).toHaveBeenCalledWith(true, 'pw', '123456');
    });

    it('disable-emailsms → onDisableEmailSms(password, code)', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
      fillPassword();
      fillCode();
      await confirm('Disable Email/SMS');
      expect(onDisableEmailSms).toHaveBeenCalledWith('pw', '123456');
    });

    it('a 409 inline-factor refusal shows its message in the banner and keeps the modal open', async () => {
      const message =
        'Turn off email and text-message codes before removing your last authenticator app or security key.';
      const onResetTOTP = vi.fn().mockResolvedValue({ kind: 'inlineFactorRequired', message });
      render(
        <MFATierSelector {...defaultProps} activeMethods={['totp']} onResetTOTP={onResetTOTP} />
      );
      fireEvent.click(screen.getByText('Reset'));
      fillPassword();
      fillCode();
      await confirm('Confirm');
      expect(await screen.findByText(message)).toBeInTheDocument();
      expect(screen.getByRole('dialog')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Confirm' })).not.toBeDisabled();
    });

    it('unavailable shows the outage copy and does not lock', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'unavailable' });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
      fillPassword();
      fillCode();
      await confirm('Disable Email/SMS');
      expect(
        await screen.findByText(
          'Verification is temporarily unavailable. Try again in a few minutes.'
        )
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).not.toBeDisabled();
    });

    it('a lock survives until the modal is reopened, then clears', async () => {
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'rateLimited' });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
      fillPassword();
      fillCode();
      await confirm('Disable Email/SMS');
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).toBeDisabled();
      fireEvent.click(screen.getByText('Cancel'));
      fireEvent.click(screen.getByText('Disable'));
      fillPassword();
      fillCode();
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).not.toBeDisabled();
    });
  });

  // ── F4: the password field is focused once it is enabled again ──────────

  it('moves focus to the password field after a password refusal (F4)', async () => {
    const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'invalidPassword' });
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        onDisableEmailSms={onDisableEmailSms}
      />
    );
    fireEvent.click(screen.getByText('Disable'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'wrong' },
    });
    const code = screen.getByTestId('mfa-verify-input');
    fireEvent.change(code, { target: { value: '123456' } });
    code.focus();
    expect(document.activeElement).toBe(code);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Disable Email/SMS' }));
    });
    await vi.waitFor(() =>
      expect(document.activeElement).toBe(screen.getByPlaceholderText('Enter your password'))
    );
  });

  // ── I8: removing the backup email ────────────────────────────────────────

  it('clearing the backup email opens the Remove variant and sends an empty email (I8)', async () => {
    const onSetBackupEmail = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        backupEmail="old@example.com"
        onSetBackupEmail={onSetBackupEmail}
      />
    );
    fireEvent.click(screen.getByText('Change'));
    fireEvent.change(screen.getByPlaceholderText('backup@example.com'), { target: { value: '' } });
    fireEvent.click(screen.getByText('Save'));

    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(
      screen.getByText('old@example.com will no longer be able to recover your account.')
    ).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'pw' },
    });
    fireEvent.change(screen.getByTestId('mfa-verify-input'), { target: { value: '123456' } });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Remove backup email' }));
    });
    expect(onSetBackupEmail).toHaveBeenCalledWith('', 'pw', '123456');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    // Accepted leaves edit mode.
    expect(screen.getByText('Change')).toBeInTheDocument();
  });

  // ── F5: the backup-email error is a field error tied to its input ────────

  it('a malformed backup email is an alert the input names and marks invalid (F5)', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
    fireEvent.click(screen.getByText('Add'));
    const input = screen.getByPlaceholderText('backup@example.com');
    fireEvent.change(input, { target: { value: 'not-an-email' } });
    fireEvent.click(screen.getByText('Save'));

    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('Please enter a valid email address.');
    expect(alert.querySelector('svg[aria-hidden="true"]')).not.toBeNull();
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input.getAttribute('aria-describedby')).toBe(alert.id);

    fireEvent.change(input, { target: { value: 'fixed@example.com' } });
    expect(input).toHaveAttribute('aria-invalid', 'false');
  });

  // ── F13: a failed section read is not rendered as the resource ───────────

  describe('section reads check res.ok (F13)', () => {
    afterEach(() => {
      vi.mocked(apiFetch)
        .mockReset()
        .mockResolvedValue({ json: async () => ({}) } as Response);
    });

    it('renders the recovery key date from a 2xx read', async () => {
      vi.mocked(apiFetch).mockImplementation(async (path) =>
        path === '/api/v1/mfa/recovery-key'
          ? ({
              ok: true,
              json: async () => ({ has_recovery_key: true, created_at: '2026-01-02T00:00:00Z' }),
            } as Response)
          : ({ ok: true, json: async () => ({}) } as Response)
      );
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      expect(await screen.findByText(/^Recovery key configured on /)).toBeInTheDocument();
    });

    it('keeps the unknown state on a non-2xx or failed read', async () => {
      vi.mocked(apiFetch).mockImplementation(async (path) => {
        if (path === '/api/v1/mfa/recovery-circle') throw new TypeError('Failed to fetch');
        return {
          ok: false,
          json: async () => ({ has_recovery_key: true, devices: [{ id: 'x' }] }),
        } as Response;
      });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      await vi.waitFor(() =>
        expect(apiFetch).toHaveBeenCalledWith('/api/v1/mfa/recovery-requests')
      );
      expect(screen.getByText(/^No recovery key configured/)).toBeInTheDocument();
      expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
    });
  });
});
