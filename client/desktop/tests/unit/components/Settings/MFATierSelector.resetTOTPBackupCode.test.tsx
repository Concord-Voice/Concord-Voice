import React from 'react';
import { render, screen, userEvent } from '../../../test-utils';
import { vi } from 'vitest';

// ── Service mock ─────────────────────────────────────────────────────────────
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    json: async () => ({}),
  }),
}));

// ── Child component mocks ────────────────────────────────────────────────────
// Deliberately does NOT mock MFAVerifyPrompt/TOTPInput/BackupCodeInput — this
// suite exercises the real "Use a backup code instead" switch and the real
// BackupCodeInput auto-report behavior, which a mocked MFAVerifyPrompt (as
// used by MFATierSelector.test.tsx) would hide entirely.
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

vi.mock('@/renderer/components/Auth/RecoveryApprovalModal', () => ({
  default: () => <div data-testid="recovery-approval-modal" />,
}));

vi.mock('@/renderer/components/Settings/RecoveryCircle', () => ({
  default: () => <div data-testid="recovery-circle" />,
}));

vi.mock('@/renderer/components/Settings/MFA.css', () => ({}));

import MFATierSelector from '@/renderer/components/Settings/MFATierSelector';

const defaultProps = {
  activeMethods: ['totp'] as string[],
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
  onResetTOTP: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onRevokeWebAuthnKey: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onDisableEmailSms: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
  onSetBackupEmail: vi.fn().mockResolvedValue({ kind: 'accepted', data: null }),
};

describe('MFATierSelector Reset TOTP modal — backup code path (regression)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('leaves Confirm disabled when only the password has been entered', async () => {
    // regression: a typed backup code was never sent unless Enter was pressed
    const user = userEvent.setup();
    render(<MFATierSelector {...defaultProps} />);

    await user.click(screen.getByText('Reset'));
    await user.type(screen.getByPlaceholderText('Enter your password'), 'mypassword');

    expect(screen.getByText('Confirm')).toBeDisabled();
  });

  it('calls onResetTOTP with the typed backup code, never an empty string, after switching to backup code entry', async () => {
    // regression: a typed backup code was never sent unless Enter was pressed
    const user = userEvent.setup();
    render(<MFATierSelector {...defaultProps} />);

    await user.click(screen.getByText('Reset'));
    await user.type(screen.getByPlaceholderText('Enter your password'), 'mypassword');

    await user.click(screen.getByText('Use a backup code instead'));
    await user.type(screen.getByPlaceholderText('XXXXXXXX'), 'exci3g5f');

    await user.click(screen.getByText('Confirm'));

    expect(defaultProps.onResetTOTP).toHaveBeenCalledWith('mypassword', 'EXCI3G5F');
    expect(defaultProps.onResetTOTP).not.toHaveBeenCalledWith('mypassword', '');
  });
});
