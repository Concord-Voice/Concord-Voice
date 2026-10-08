import React from 'react';
import { render, screen, userEvent } from '../../../test-utils';
import { vi } from 'vitest';

// ── Service mock ─────────────────────────────────────────────────────────────
// Routed by path: `GET /api/v1/mfa/step-up` decides which fields the modal
// shows, and every other section read is not a 2xx.
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(async (path: string) =>
    path === '/api/v1/mfa/step-up'
      ? {
          ok: true,
          status: 200,
          json: async () => ({
            methods: ['totp'],
            default_method: 'totp',
            backup_code_available: true,
          }),
        }
      : { json: async () => ({}) }
  ),
}));

// ── Child component mocks ────────────────────────────────────────────────────
// Deliberately mocks neither StepUpCredentials nor the factor picker — this
// suite exercises the real "Use a backup code instead" switch and the value the
// real factor hook hands the handler.
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
    await user.type(screen.getByLabelText('Password'), 'mypassword');
    await screen.findByRole('button', { name: 'Use a backup code instead' });

    expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    expect(defaultProps.onResetTOTP).not.toHaveBeenCalled();
  });

  it('calls onResetTOTP with the typed backup code, never an empty string, after switching to backup code entry', async () => {
    // regression: a typed backup code was never sent unless Enter was pressed
    const user = userEvent.setup();
    render(<MFATierSelector {...defaultProps} />);

    await user.click(screen.getByText('Reset'));
    await user.type(screen.getByLabelText('Password'), 'mypassword');

    await user.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
    await user.type(screen.getByLabelText('Backup code'), 'EXCI3G5F');

    await user.click(screen.getByRole('button', { name: 'Confirm' }));

    expect(defaultProps.onResetTOTP).toHaveBeenCalledWith(
      'mypassword',
      'EXCI3G5F',
      expect.anything()
    );
    expect(defaultProps.onResetTOTP).not.toHaveBeenCalledWith('mypassword', '', expect.anything());
  });
});
