import React from 'react';
import { render, screen, fireEvent, act, waitFor, within } from '../../../test-utils';
import { vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { apiRequestContextIsCurrent } from '@/renderer/services/system/requestContext';

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
    'aria-disabled': ariaDisabled,
    'aria-describedby': ariaDescribedBy,
  }: {
    checked: boolean;
    onChange: (v: boolean) => void;
    disabled?: boolean;
    'aria-disabled'?: boolean;
    'aria-describedby'?: string;
  }) => (
    <input
      type="checkbox"
      data-testid="toggle-switch"
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
      disabled={disabled}
      aria-disabled={ariaDisabled}
      aria-describedby={ariaDescribedBy}
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
import { apiFetch } from '@/renderer/services/system/apiClient';

// ── API double ───────────────────────────────────────────────────────────────
//
// Routed by path, never by call order: the requirements read
// (`GET /api/v1/mfa/step-up`) opens each modal and changes the order of
// everything after it. "Mutant:" comments name the production change a case
// exists to turn red.

const READ_PATH = '/api/v1/mfa/step-up';
const BEGIN_PATH = '/api/v1/mfa/webauthn/verify-inline/begin';

interface Call {
  path: string;
  body: Record<string, unknown> | null;
  /** The `ApiRequestContext` the request was admitted against, if any. */
  context: unknown;
}
const calls: Call[] = [];

function json(body: unknown, status = 200): Response {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as Response;
}

/** A read answering `methods`, the first of them the server's default. */
const readOffers =
  (methods: string[], backup = false) =>
  (): Response =>
    json({ methods, default_method: methods[0] ?? null, backup_code_available: backup });
const readStatus = (status: number) => (): Response => json({}, status);

interface Scenario {
  read?: () => Response | Promise<Response>;
  begin?: () => Response | Promise<Response>;
}

function serveApi(scenario: Scenario = {}) {
  vi.mocked(apiFetch).mockImplementation(
    async (path: string, init?: RequestInit, opts?: { context?: unknown }) => {
      calls.push({
        path,
        body: typeof init?.body === 'string' ? JSON.parse(init.body) : null,
        context: opts?.context,
      });
      if (path === READ_PATH) return (scenario.read ?? readOffers(['totp']))();
      if (path === BEGIN_PATH) return (scenario.begin ?? (() => json({ error: 'x' }, 500)))();
      // Every other section read: not a 2xx, so it keeps its unknown state.
      return { json: async () => ({}) } as Response;
    }
  );
}

const readCalls = () => calls.filter((c) => c.path === READ_PATH);
const beginCalls = () => calls.filter((c) => c.path === BEGIN_PATH);

const typePassword = (value = 'pw') =>
  fireEvent.change(screen.getByLabelText('Password'), { target: { value } });
const typeCode = (input: HTMLElement, value = '123456') =>
  fireEvent.change(input, { target: { value } });
const submitConfirm = async (name = 'Confirm') => {
  await act(async () => {
    fireEvent.click(screen.getByRole('button', { name }));
  });
};
/** Resolves once the primary can act: the read landed and everything is filled. */
const untilActionable = (name = 'Confirm') =>
  waitFor(() => expect(screen.getByRole('button', { name })).not.toHaveAttribute('aria-disabled'));

// ── Default props ────────────────────────────────────────────────────────────
const defaultProps = {
  activeMethods: [] as string[],
  recoveryOnlyMethods: [] as string[],
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
    resetAllStores();
    vi.clearAllMocks();
    calls.length = 0;
    useAuthStore.getState().setAccessToken('mock-token');
    serveApi();
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

  it('shows a labelled password field in the action modal', () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    expect(screen.getByLabelText('Password')).toHaveAttribute('type', 'password');
  });

  it('keeps Confirm aria-disabled, not natively disabled, while the password is empty', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    const confirm = screen.getByRole('button', { name: 'Confirm' });
    expect(confirm).toHaveAttribute('aria-disabled', 'true');
    expect(confirm).not.toBeDisabled();
    await act(async () => {
      fireEvent.click(confirm);
    });
    expect(defaultProps.onResetTOTP).not.toHaveBeenCalled();
  });

  it('enables Reset TOTP Confirm once the password and an authenticator code are entered', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    typePassword('mypassword');
    const code = await screen.findByLabelText('Authenticator app code');
    expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    fireEvent.change(code, { target: { value: '123456' } });
    expect(screen.getByRole('button', { name: 'Confirm' })).not.toHaveAttribute('aria-disabled');
  });

  it('calls onResetTOTP with the password, the code and the run capture when Confirm is clicked', async () => {
    render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
    fireEvent.click(screen.getByText('Reset'));
    typePassword('mypassword');
    typeCode(await screen.findByLabelText('Authenticator app code'));
    await submitConfirm();
    expect(defaultProps.onResetTOTP).toHaveBeenCalledWith(
      'mypassword',
      '123456',
      expect.anything()
    );
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
    typePassword('mypassword');
    typeCode(await screen.findByLabelText('Authenticator app code'));
    await submitConfirm();
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

    typePassword('mypw');
    typeCode(await screen.findByLabelText('Authenticator app code'), '654321');
    await submitConfirm('Save backup email');

    expect(onSetBackupEmail).toHaveBeenCalledWith(
      'valid@example.com',
      'mypw',
      '654321',
      expect.anything()
    );
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
    expect(screen.getByText(/Hardened mode/)).toBeInTheDocument();
  });

  // SMS is not offered yet: the server refuses SMS enrolment outside dev/test.
  it('tags SMS as coming soon and leaves email untagged', () => {
    render(<MFATierSelector {...defaultProps} />);
    // Exact text, so the space a screen reader needs between the two is pinned.
    expect(screen.getByText('SMS code').textContent).toBe('SMS code Coming soon');
    expect(screen.getByText('Email code')).not.toHaveTextContent('Coming soon');
  });

  // Hardened mode needs an SMS code, so its toggle is dormant: focusable,
  // aria-disabled, described by its tag, never checked, and inert on click.
  it('keeps the hardened toggle dormant until SMS ships', () => {
    const onToggleRecoveryHardened = vi.fn();
    render(
      <MFATierSelector
        {...defaultProps}
        activeMethods={['totp', 'email']}
        onToggleRecoveryHardened={onToggleRecoveryHardened}
      />
    );
    const toggle = screen.getAllByTestId('toggle-switch')[1];
    expect(toggle).not.toBeChecked();
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).not.toBeDisabled();
    const describedBy = toggle.getAttribute('aria-describedby');
    expect(describedBy && document.getElementById(describedBy)).toHaveTextContent('Coming soon');

    fireEvent.click(toggle);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(onToggleRecoveryHardened).not.toHaveBeenCalled();
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
      const password = screen.getByLabelText('Password');
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
      const password = screen.getByLabelText('Password');
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
      const password = screen.getByLabelText('Password');
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

  // ── Action modal — which credential fields the read decides (plan §1.3, §3) ──
  //
  // The modal asks for what `GET /api/v1/mfa/step-up` says the account can use.
  // `activeMethods` is the last status fetch and never decides a field.

  describe('action modal — credential fields follow the read (§1.3)', () => {
    type Opener = { name: string; confirm: string; open: () => void; handler: string };
    const openers: Opener[] = [
      {
        name: 'disable email/SMS',
        confirm: 'Disable Email/SMS',
        handler: 'onDisableEmailSms',
        open: () => fireEvent.click(screen.getByText('Disable')),
      },
      {
        name: 'set backup email',
        confirm: 'Save backup email',
        handler: 'onSetBackupEmail',
        open: () => {
          fireEvent.click(screen.getByText('Add'));
          fireEvent.change(screen.getByPlaceholderText('backup@example.com'), {
            target: { value: 'valid@example.com' },
          });
          fireEvent.click(screen.getByText('Save'));
        },
      },
      {
        name: 'recovery-only toggle',
        confirm: 'Confirm',
        handler: 'onToggleRecoveryOnly',
        open: () => fireEvent.click(screen.getAllByTestId('toggle-switch')[0]),
      },
    ];

    describe.each(openers)('$name', ({ confirm, open, handler }) => {
      // Mutant: the code field renders unconditionally. With `methods: []` the
      // account has no inline factor, so a required code box can never be filled.
      it('shows no code field and sends the password alone when the read returns no methods', async () => {
        serveApi({ read: readOffers([]) });
        const spy = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
        render(
          <MFATierSelector
            {...defaultProps}
            activeMethods={['totp', 'email']}
            {...{ [handler]: spy }}
          />
        );
        open();
        typePassword('pw');
        await untilActionable(confirm);

        expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
        expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
        expect(screen.queryByText(/Passkey or security key/)).not.toBeInTheDocument();
        await submitConfirm(confirm);
        const call = spy.mock.calls[0];
        expect(call.at(-3)).toBe('pw');
        expect(call.at(-2)).toBeUndefined();
      });

      it('shows the code field, and sends the code, when the read returns TOTP', async () => {
        serveApi({ read: readOffers(['totp']) });
        const spy = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
        render(
          <MFATierSelector {...defaultProps} activeMethods={['email']} {...{ [handler]: spy }} />
        );
        open();
        typePassword('pw');
        typeCode(await screen.findByLabelText('Authenticator app code'));
        await untilActionable(confirm);
        await submitConfirm(confirm);
        expect(spy.mock.calls[0].at(-2)).toBe('123456');
      });
    });

    // Mutant: the field set derived from `activeMethods` rather than the read.
    it('ignores a stale activeMethods that names TOTP when the read returns none', async () => {
      serveApi({ read: readOffers([]) });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      typePassword('pw');
      await untilActionable('Disable Email/SMS');
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    });

    it('shows the code field for an account the stale activeMethods says has none', async () => {
      serveApi({ read: readOffers(['totp']) });
      render(<MFATierSelector {...defaultProps} activeMethods={['email']} />);
      fireEvent.click(screen.getByText('Disable'));
      expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
    });

    it('reads once per open, and not before the modal opens', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
      expect(readCalls()).toHaveLength(0);
      fireEvent.click(screen.getByText('Disable'));
      await waitFor(() => expect(readCalls()).toHaveLength(1));
    });

    it('does not offer email or SMS, even when the read lists them', async () => {
      serveApi({
        read: () =>
          json({
            methods: ['totp', 'email', 'sms'],
            default_method: 'totp',
            backup_code_available: false,
          }),
      });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      await screen.findByLabelText('Authenticator app code');
      const dialog = within(screen.getByRole('dialog'));
      expect(dialog.queryByLabelText(/email|sms/i)).not.toBeInTheDocument();
      expect(dialog.queryByRole('button', { name: /instead/i })).not.toBeInTheDocument();
    });
  });

  // ── Action modal — backup codes (plan §3: backup offered on #4) ──────────

  describe('action modal — backup code', () => {
    it('offers a backup code beside TOTP when the read reports one', async () => {
      serveApi({ read: readOffers(['totp'], true) });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));
      expect(
        await screen.findByRole('button', { name: 'Use a backup code instead' })
      ).toBeInTheDocument();
    });

    it('does not offer one when the read reports none', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));
      await screen.findByLabelText('Authenticator app code');
      expect(
        screen.queryByRole('button', { name: 'Use a backup code instead' })
      ).not.toBeInTheDocument();
    });

    // regression: a typed backup code was never sent unless Enter was pressed
    it('keeps Confirm down when only the password has been entered', async () => {
      serveApi({ read: readOffers(['totp'], true) });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));
      typePassword('mypassword');
      await screen.findByLabelText('Authenticator app code');
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
    });

    it('calls onResetTOTP with the typed backup code, never an empty string', async () => {
      serveApi({ read: readOffers(['totp'], true) });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));
      typePassword('mypassword');
      fireEvent.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
      fireEvent.change(screen.getByLabelText('Backup code'), { target: { value: 'EXCI3G5F' } });
      await submitConfirm();

      expect(defaultProps.onResetTOTP).toHaveBeenCalledWith(
        'mypassword',
        'EXCI3G5F',
        expect.anything()
      );
      expect(defaultProps.onResetTOTP).not.toHaveBeenCalledWith(
        'mypassword',
        '',
        expect.anything()
      );
    });
  });

  // ── Action modal — reset-totp floors TOTP (D10, Q3, C26) ──────────────────
  //
  // The route binds a TOTP `code` as required, so TOTP is offered whatever the
  // read says; a failed read must not cost it.

  describe('action modal — reset-totp keeps TOTP when the read gives none', () => {
    // Mutant: the TOTP floor removed from reset-totp.
    it.each([
      ['an empty method list', readOffers([])],
      ['a failed read', readStatus(503)],
      ['a server without the route', readStatus(404)],
    ])('still asks for the authenticator code after %s', async (_name, read) => {
      serveApi({ read });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));
      typePassword('pw');
      const code = await screen.findByLabelText('Authenticator app code');
      await waitFor(() => expect(readCalls()).toHaveLength(1));
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );

      fireEvent.change(code, { target: { value: '123456' } });
      await submitConfirm();
      expect(defaultProps.onResetTOTP).toHaveBeenCalledWith('pw', '123456', expect.anything());
    });

    it('does not floor any other action: a failed read leaves the password alone', async () => {
      serveApi({ read: readStatus(503) });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      typePassword('pw');
      await untilActionable('Disable Email/SMS');
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    });

    it('adds to the floor when the read offers a security key, and keeps both reachable', async () => {
      serveApi({ read: readOffers(['webauthn']) });
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'webauthn']} />);
      fireEvent.click(screen.getByText('Reset'));
      fireEvent.click(await screen.findByRole('button', { name: 'Use authenticator app instead' }));
      expect(screen.getByLabelText('Authenticator app code')).toBeInTheDocument();
      expect(
        screen.getByRole('button', { name: 'Use passkey or security key instead' })
      ).toBeInTheDocument();
    });
  });

  // ── Action modal — revoke-webauthn is password only (D10) ─────────────────

  describe('action modal — revoke-webauthn takes the password alone', () => {
    const key = {
      id: 'cred-1',
      credential_name: 'Key A',
      credential_type: 'hardware',
      created_at: '2026-01-01T00:00:00Z',
    };
    const renderRevoke = (
      onRevokeWebAuthnKey = vi.fn().mockResolvedValue({ kind: 'accepted', data: null })
    ) => {
      serveApi({ read: readOffers(['totp', 'webauthn'], true) });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'webauthn']}
          webauthnCredentials={[key]}
          onRevokeWebAuthnKey={onRevokeWebAuthnKey}
        />
      );
      fireEvent.click(screen.getByText('Revoke'));
      return onRevokeWebAuthnKey;
    };

    // Mutant: revoke-webauthn runs the factor hook, so it reads and shows a factor.
    it('issues no requirements read and shows no factor, whatever the account holds', async () => {
      renderRevoke();
      typePassword('pw');
      await untilActionable('Confirm');
      // Let any read that was wrongly started land before asserting it did not.
      await act(async () => {
        await Promise.resolve();
      });
      expect(readCalls()).toHaveLength(0);
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
      expect(screen.queryByText('Passkey or security key')).not.toBeInTheDocument();
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
    });

    it('sends (credentialId, password, undefined, context) on the password alone', async () => {
      const onRevoke = renderRevoke();
      typePassword('pw');
      await submitConfirm();
      expect(onRevoke).toHaveBeenCalledWith('cred-1', 'pw', undefined, expect.anything());
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    // Mutant: the revoke capturing at the click. revoke-webauthn runs no
    // factor hook, so nothing else binds the typed password to the account the
    // modal opened under; it would go out under the next one.
    it('sends nothing, and says so, when the account changed after the modal opened', async () => {
      const onRevoke = renderRevoke();
      typePassword('pw');
      act(() => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 })));
      await submitConfirm();

      expect(await screen.findByText(/Sign in again to continue/)).toBeInTheDocument();
      expect(onRevoke).not.toHaveBeenCalled();
    });

    // The key revoke has no stage to end, so its banner says it. Mutant: the
    // stage-less path using `stepUpBanner`, which leaves a dead session unsaid.
    it('a dead session is said in the banner and locks Confirm', async () => {
      const onRevoke = renderRevoke(vi.fn().mockResolvedValue({ kind: 'sessionExpired' }));
      typePassword('pw');
      await submitConfirm();
      expect(
        await screen.findByText('Your session has expired. Sign in again to continue.')
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      expect(onRevoke).toHaveBeenCalledTimes(1);
    });

    it('keeps Confirm aria-disabled and sends nothing without a password', async () => {
      const onRevoke = renderRevoke();
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await submitConfirm();
      expect(onRevoke).not.toHaveBeenCalled();
    });

    it('a refused password is a field error, empties the field and takes focus back', async () => {
      const onRevoke = renderRevoke(vi.fn().mockResolvedValue({ kind: 'invalidPassword' }));
      typePassword('wrong');
      await submitConfirm();
      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toHaveValue('');
      await vi.waitFor(() =>
        expect(document.activeElement).toBe(screen.getByLabelText('Password'))
      );
      expect(onRevoke).toHaveBeenCalledTimes(1);
    });

    // `revoking` is state, so two activations in one tick both read it false.
    // Each is dispatched natively inside ONE act, so no render runs between them.
    // Mutant: the single-flight latch never set.
    it.each([
      ['a double click', (field: HTMLElement, button: HTMLElement) => [button, button]],
      ['Enter then a click', (field: HTMLElement, button: HTMLElement) => [field, button]],
    ])('%s sends exactly one revoke', async (_name, targets) => {
      const gate = deferred<{ kind: 'accepted'; data: null }>();
      const onRevoke = renderRevoke(vi.fn(() => gate.promise));
      typePassword('pw');
      const field = screen.getByLabelText('Password');
      const button = screen.getByRole('button', { name: 'Confirm' });

      act(() => {
        for (const target of targets(field, button)) {
          if (target === field) {
            target.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
          } else {
            target.click();
          }
        }
      });

      expect(onRevoke).toHaveBeenCalledTimes(1);
      await act(async () => gate.resolve({ kind: 'accepted', data: null }));
    });

    // The password is this modal's only input, so Enter must submit it as the
    // stage's fields do (`isPlainEnter`).
    // Mutant: the field's Enter handler not pressing the primary.
    it('a plain Enter in the password field presses Confirm', async () => {
      const onRevoke = renderRevoke();
      typePassword('pw');
      await act(async () => {
        fireEvent.keyDown(screen.getByLabelText('Password'), { key: 'Enter' });
      });
      expect(onRevoke).toHaveBeenCalledWith('cred-1', 'pw', undefined, expect.anything());
    });

    // Positive control: the case above shows a plain Enter on this field does press.
    it.each([
      ['Shift', { shiftKey: true }],
      ['Ctrl', { ctrlKey: true }],
      ['Alt', { altKey: true }],
      ['Meta', { metaKey: true }],
      ['an auto-repeat', { repeat: true }],
    ])('%s Enter does not press Confirm', async (_name, modifiers) => {
      const onRevoke = renderRevoke();
      typePassword('pw');
      await act(async () => {
        fireEvent.keyDown(screen.getByLabelText('Password'), { key: 'Enter', ...modifiers });
      });
      expect(onRevoke).not.toHaveBeenCalled();
    });

    // Revoking a key verifies the password alone, so nothing on it may mint a
    // security-key token or name a purpose a token could be spent on.
    it('sends no security-key begin and names no purpose, even for a key-only account', async () => {
      const onRevoke = vi.fn().mockResolvedValue({ kind: 'accepted', data: null });
      serveApi({ read: readOffers(['webauthn']), begin: () => json({ publicKey: {} }) });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['webauthn']}
          webauthnCredentials={[key]}
          onRevokeWebAuthnKey={onRevoke}
        />
      );
      fireEvent.click(screen.getByText('Revoke'));
      typePassword('pw');
      await submitConfirm();

      expect(onRevoke).toHaveBeenCalledWith('cred-1', 'pw', undefined, expect.anything());
      expect(beginCalls()).toHaveLength(0);
      expect(readCalls()).toHaveLength(0);
      expect(calls.some((c) => c.body !== null && 'purpose' in c.body)).toBe(false);
    });

    it('a rate limit locks Confirm', async () => {
      renderRevoke(vi.fn().mockResolvedValue({ kind: 'rateLimited' }));
      typePassword('pw');
      await submitConfirm();
      expect(
        await screen.findByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
    });
  });

  // ── Action modal — the route each code is minted for ──────────────────────
  //
  // A WebAuthn inline token is bound to the purpose its begin request names, so
  // each action must name the route its code is actually sent with (#3453 RS11).

  describe('action modal — security-key purpose', () => {
    const purposeOf = async (open: () => void, confirm: string) => {
      serveApi({ read: readOffers(['webauthn']) });
      open();
      typePassword('pw');
      await untilActionable(confirm);
      await submitConfirm(confirm);
      await waitFor(() => expect(beginCalls()).toHaveLength(1));
      return beginCalls()[0].body?.purpose;
    };

    it('names the email/SMS disable route for Disable', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'webauthn', 'email']} />);
      expect(
        await purposeOf(() => fireEvent.click(screen.getByText('Disable')), 'Disable Email/SMS')
      ).toBe('mfa_settings.email_sms_disable');
    });

    it('names the TOTP disable route for Reset', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'webauthn']} />);
      expect(await purposeOf(() => fireEvent.click(screen.getByText('Reset')), 'Confirm')).toBe(
        'mfa_settings.totp_disable'
      );
    });

    it('names the recovery-only route for the recovery-only toggle', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp', 'webauthn', 'email']} />);
      expect(
        await purposeOf(() => fireEvent.click(screen.getAllByTestId('toggle-switch')[0]), 'Confirm')
      ).toBe('mfa_settings.recovery_only_set');
    });

    it('names the backup-email route for the backup email', async () => {
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'webauthn', 'email']}
          backupEmail=""
        />
      );
      const open = () => {
        fireEvent.click(screen.getByText('Add'));
        fireEvent.change(screen.getByPlaceholderText('backup@example.com'), {
          target: { value: 'valid@example.com' },
        });
        fireEvent.click(screen.getByText('Save'));
      };
      expect(await purposeOf(open, 'Save backup email')).toBe('mfa_settings.backup_email_set');
    });
  });

  // ── Action modal — MfaStepUpResult routing (spec §4.3, handoff §1.1) ───────
  //
  // Each `kind` a gated handler can return routes to a distinct, observable
  // target: a field error, the general banner, a lock, or the modal closing.

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

    const fillCredentials = async () => {
      typePassword('mypassword');
      typeCode(await screen.findByLabelText('Authenticator app code'));
    };

    const submit = () => submitConfirm('Disable Email/SMS');
    const confirm = () => screen.getByRole('button', { name: 'Disable Email/SMS' });
    const codeValue = () =>
      (screen.getByLabelText('Authenticator app code') as HTMLInputElement).value;
    const passwordValue = () => (screen.getByLabelText('Password') as HTMLInputElement).value;

    it('accepted: closes the modal', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'accepted', data: null }));
      await fillCredentials();
      await submit();
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('passwordRequired: asks for the password and clears it', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'passwordRequired' }));
      await fillCredentials();
      await submit();
      expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
      expect(passwordValue()).toBe('');
    });

    it('invalidPassword: shows the field error, clears the password and keeps the code the server never read', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'invalidPassword' }));
      await fillCredentials();
      await submit();
      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect(passwordValue()).toBe('');
      expect(codeValue()).toBe('123456');
    });

    it('mfaRequired: asks for the code the server named and keeps the password', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'mfaRequired', methods: ['totp'] }));
      await fillCredentials();
      await submit();
      expect(
        await screen.findByText('Enter the 6-digit code from your authenticator app to continue.')
      ).toBeInTheDocument();
      expect(passwordValue()).toBe('mypassword');
    });

    it('invalidMfaCode: marks the code and empties the field', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'invalidMfaCode' }));
      await fillCredentials();
      await submit();
      expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
      expect(codeValue()).toBe('');
    });

    it('rateLimited: shows the banner and locks Confirm', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'rateLimited' }));
      await fillCredentials();
      await submit();
      expect(
        await screen.findByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
      expect(confirm()).toHaveAttribute('aria-disabled', 'true');
    });

    // Mutant: a 401 as `answered`, which left the stage live with the password
    // and an active Confirm under a dead session (picker PR 3 review).
    it('sessionExpired: the stage ends, drops the password and locks Confirm', async () => {
      const onDisable = vi.fn().mockResolvedValue({ kind: 'sessionExpired' });
      renderDisableModal(onDisable);
      await fillCredentials();
      await submit();
      expect(await screen.findByText('Sign in again to continue.')).toBeInTheDocument();
      // One sentence: the banner leaves the dead session to the stage.
      expect(screen.getAllByText(/Sign in again/)).toHaveLength(1);
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(confirm()).toHaveAttribute('aria-disabled', 'true');
      await submit();
      expect(onDisable).toHaveBeenCalledTimes(1);
    });

    it('networkError: shows the banner, keeps the password and asks for a fresh code', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'networkError' }));
      await fillCredentials();
      await submit();
      expect(
        await screen.findByText("Couldn't reach the server. Check your connection and try again.")
      ).toBeInTheDocument();
      expect(passwordValue()).toBe('mypassword');
      // The request may have reached the server, which accepts each code once.
      expect(codeValue()).toBe('');
      expect(confirm()).toHaveAttribute('aria-disabled', 'true');
      typeCode(screen.getByLabelText('Authenticator app code'), '654321');
      expect(confirm()).not.toHaveAttribute('aria-disabled');
    });

    it('failed: shows the generic banner', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'failed' }));
      await fillCredentials();
      await submit();
      expect(await screen.findByText('Something went wrong. Try again.')).toBeInTheDocument();
    });

    // The server accepts each code once and can accept one yet still fail the
    // request (a 500 after the code verified), so the code is never re-offered.
    it('failed (a 500 after the code was sent): clears the code and holds Confirm', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'failed' }));
      await fillCredentials();
      await submit();
      await screen.findByText('Something went wrong. Try again.');
      expect(codeValue()).toBe('');
      expect(confirm()).toHaveAttribute('aria-disabled', 'true');
    });

    it('enrollmentRequired: shows the set-up sentence and nothing to fill', async () => {
      renderDisableModal(vi.fn().mockResolvedValue({ kind: 'enrollmentRequired' }));
      await fillCredentials();
      await submit();
      expect(
        await screen.findByText(
          'Set up an authenticator app or security key in Settings to do this.'
        )
      ).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(confirm()).toHaveAttribute('aria-disabled', 'true');
    });
  });

  // ── I2 (F3): a stale activeMethods must not hide the code prompt ──────────
  //
  // The server's `methods` on an mfa_required refusal is the present truth. A
  // modal that read a password-only account and is then told otherwise shows
  // the code field, with only the inline-verifiable subset of the list.

  describe('action modal — a refusal naming methods the read did not (I2)', () => {
    it('renders the code field from refusal.methods after an mfa_required refusal', async () => {
      serveApi({ read: readOffers([]) });
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
      typePassword('pw');
      await untilActionable('Disable Email/SMS');
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      await submitConfirm('Disable Email/SMS');

      const code = await screen.findByLabelText('Authenticator app code');
      // Only the inline-verifiable subset of the server's list (policy P1).
      const dialog = within(screen.getByRole('dialog'));
      expect(dialog.queryByLabelText(/email|sms/i)).not.toBeInTheDocument();
      expect(dialog.queryByRole('button', { name: /instead/i })).not.toBeInTheDocument();
      const confirm = () => screen.getByRole('button', { name: 'Disable Email/SMS' });
      // The code is now required before another attempt is spent.
      expect(confirm()).toHaveAttribute('aria-disabled', 'true');

      fireEvent.change(code, { target: { value: '123456' } });
      expect(confirm()).not.toHaveAttribute('aria-disabled');
      await submitConfirm('Disable Email/SMS');
      expect(onDisableEmailSms).toHaveBeenLastCalledWith('pw', '123456', expect.anything());
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
  });

  // ── I4: every action routes through the step-up result, with its own args ──

  describe('action modal — all six actions route and pass their arguments (I4)', () => {
    const accepted = () => vi.fn().mockResolvedValue({ kind: 'accepted', data: null });

    it('reset-totp → onResetTOTP(password, code, context), then closes', async () => {
      const onResetTOTP = accepted();
      render(
        <MFATierSelector {...defaultProps} activeMethods={['totp']} onResetTOTP={onResetTOTP} />
      );
      fireEvent.click(screen.getByText('Reset'));
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      await submitConfirm();
      expect(onResetTOTP).toHaveBeenCalledWith('pw', '123456', expect.anything());
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('revoke-webauthn → onRevokeWebAuthnKey(credentialId, password, undefined, context) on the password alone', async () => {
      const onRevokeWebAuthnKey = accepted();
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
      typePassword();
      await submitConfirm();
      expect(onRevokeWebAuthnKey).toHaveBeenCalledWith(
        'cred-1',
        'pw',
        undefined,
        expect.anything()
      );
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('toggle-recovery-only → onToggleRecoveryOnly(method, value, password, code, context), code required', async () => {
      const onToggleRecoveryOnly = accepted();
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onToggleRecoveryOnly={onToggleRecoveryOnly}
        />
      );
      fireEvent.click(screen.getAllByTestId('toggle-switch')[0]);
      typePassword();
      const code = await screen.findByLabelText('Authenticator app code');
      // A password alone would spend an attempt on a request that must fail.
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      fireEvent.change(code, { target: { value: '123456' } });
      await submitConfirm();
      expect(onToggleRecoveryOnly).toHaveBeenCalledWith(
        'email',
        true,
        'pw',
        '123456',
        expect.anything()
      );
    });

    it('disable-emailsms → onDisableEmailSms(password, code, context)', async () => {
      const onDisableEmailSms = accepted();
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['totp', 'email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      await submitConfirm('Disable Email/SMS');
      expect(onDisableEmailSms).toHaveBeenCalledWith('pw', '123456', expect.anything());
    });

    it('a 409 inline-factor refusal shows its message in the banner and keeps the modal open', async () => {
      const message =
        'Turn off email and text-message codes before removing your last authenticator app or security key.';
      const onResetTOTP = vi.fn().mockResolvedValue({ kind: 'inlineFactorRequired', message });
      render(
        <MFATierSelector {...defaultProps} activeMethods={['totp']} onResetTOTP={onResetTOTP} />
      );
      fireEvent.click(screen.getByText('Reset'));
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      await submitConfirm();
      expect(await screen.findByText(message)).toBeInTheDocument();
      expect(screen.getByRole('dialog')).toBeInTheDocument();
      // Not a lock: a fresh code (the sent one may be spent) re-enables Confirm.
      expect(screen.getByRole('button', { name: 'Confirm' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      typeCode(screen.getByLabelText('Authenticator app code'));
      expect(screen.getByRole('button', { name: 'Confirm' })).not.toHaveAttribute('aria-disabled');
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
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      await submitConfirm('Disable Email/SMS');
      expect(
        await screen.findByText(
          'Verification is temporarily unavailable. Try again in a few minutes.'
        )
      ).toBeInTheDocument();
      // Not a lock: a fresh code (the sent one may be spent) re-enables Confirm.
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      typeCode(screen.getByLabelText('Authenticator app code'));
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).not.toHaveAttribute(
        'aria-disabled'
      );
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
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      await submitConfirm('Disable Email/SMS');
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      fireEvent.click(screen.getByText('Cancel'));
      fireEvent.click(screen.getByText('Disable'));
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      expect(screen.getByRole('button', { name: 'Disable Email/SMS' })).not.toHaveAttribute(
        'aria-disabled'
      );
    });
  });

  // ── The run's capture (C82) ───────────────────────────────────────────────

  describe('action modal — one capture per activation', () => {
    // Mutant: a request sent outside the run's capture. The handler must be
    // handed the capture `run` worked against, so its request is admitted
    // against the same account and server as the proof.
    it('admits the security-key begin request against a capture', async () => {
      serveApi({ read: readOffers(['webauthn']) });
      const onDisableEmailSms = vi.fn().mockResolvedValue({ kind: 'failed' });
      render(
        <MFATierSelector
          {...defaultProps}
          activeMethods={['webauthn', 'email']}
          onDisableEmailSms={onDisableEmailSms}
        />
      );
      fireEvent.click(screen.getByText('Disable'));
      typePassword();
      await untilActionable('Disable Email/SMS');
      // A begin that fails ends the run before the handler is called; the
      // capture it used is then the only one there is.
      await submitConfirm('Disable Email/SMS');
      await waitFor(() => expect(beginCalls()).toHaveLength(1));
      expect(beginCalls()[0].context).toBeDefined();
      expect(onDisableEmailSms).not.toHaveBeenCalled();
    });

    // A change before the click is caught by the capture the stage took when
    // it opened (useStepUpFactor's tests). A change DURING the run, here while
    // the begin request is out, must stop it before anything carrying the
    // proof is sent.
    it('sends nothing, and says so, when the account changed during the run', async () => {
      const begin = deferred<Response>();
      serveApi({ read: readOffers(['webauthn']), begin: () => begin.promise });
      render(<MFATierSelector {...defaultProps} activeMethods={['webauthn', 'email']} />);
      fireEvent.click(screen.getByText('Disable'));
      typePassword();
      await untilActionable('Disable Email/SMS');
      await submitConfirm('Disable Email/SMS');
      await waitFor(() => expect(beginCalls()).toHaveLength(1));

      act(() => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 })));
      await act(async () => begin.resolve(json({ publicKey: {} })));

      await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Sign in again'));
      expect(defaultProps.onDisableEmailSms).not.toHaveBeenCalled();
      expect(calls.filter((c) => c.path.endsWith('/verify-inline/finish'))).toHaveLength(0);
    });

    it('hands the handler a live ApiRequestContext', async () => {
      render(<MFATierSelector {...defaultProps} activeMethods={['totp']} />);
      fireEvent.click(screen.getByText('Reset'));
      typePassword();
      typeCode(await screen.findByLabelText('Authenticator app code'));
      await submitConfirm();
      const context = defaultProps.onResetTOTP.mock.calls[0][2];
      expect(apiRequestContextIsCurrent(context)).toBe(true);
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
    typePassword('wrong');
    const code = await screen.findByLabelText('Authenticator app code');
    typeCode(code);
    code.focus();
    expect(document.activeElement).toBe(code);
    await submitConfirm('Disable Email/SMS');
    await vi.waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText('Password')));
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
    typePassword();
    typeCode(await screen.findByLabelText('Authenticator app code'));
    await submitConfirm('Remove backup email');
    expect(onSetBackupEmail).toHaveBeenCalledWith('', 'pw', '123456', expect.anything());
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
      useUserStore.setState({
        user: {
          id: '11111111-2222-4333-8444-555555555555',
          username: 'local',
          email: 'local@example.test',
        },
      });
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
