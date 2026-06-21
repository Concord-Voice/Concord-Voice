import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { vi } from 'vitest';

// ── Service mock ─────────────────────────────────────────────────────────────
vi.mock('@/renderer/services/apiClient', () => ({
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

vi.mock('@/renderer/components/Auth/MFAVerifyPrompt', () => ({
  default: ({ error }: { error?: string }) => (
    <div data-testid="mfa-verify-prompt">{error && <span>{error}</span>}</div>
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
  onToggleRecoveryOnly: vi.fn(),
  onToggleRecoveryHardened: vi.fn(),
  onResetTOTP: vi.fn().mockResolvedValue(true),
  onRevokeWebAuthnKey: vi.fn().mockResolvedValue(true),
  onDisableEmailSms: vi.fn().mockResolvedValue(true),
  onSetBackupEmail: vi.fn().mockResolvedValue(true),
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

  it('enables Confirm button when password is entered', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypassword' },
    });
    expect(screen.getByText('Confirm')).not.toBeDisabled();
  });

  it('calls onResetTOTP when Confirm is clicked in reset modal', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypassword' },
    });
    await act(async () => {
      fireEvent.click(screen.getByText('Confirm'));
    });
    expect(defaultProps.onResetTOTP).toHaveBeenCalled();
  });

  it('shows error when action fails', async () => {
    const failReset = vi.fn().mockResolvedValue(false);
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} onResetTOTP={failReset} />);
    fireEvent.click(screen.getByText('Reset'));
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'mypassword' },
    });
    await act(async () => {
      fireEvent.click(screen.getByText('Confirm'));
    });
    await vi.waitFor(() =>
      expect(
        screen.getByText('Verification failed. Check your password and try again.')
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

  it('calls onSetBackupEmail with valid email', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} backupEmail="" />);
    fireEvent.click(screen.getByText('Add'));
    fireEvent.change(screen.getByPlaceholderText('backup@example.com'), {
      target: { value: 'valid@example.com' },
    });
    await act(async () => {
      fireEvent.click(screen.getByText('Save'));
    });
    expect(defaultProps.onSetBackupEmail).toHaveBeenCalledWith('valid@example.com');
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
});
