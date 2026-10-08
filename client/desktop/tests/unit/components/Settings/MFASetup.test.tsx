import { act, fireEvent, render, screen, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import {
  bodiesTo,
  installStepUpApi,
  jsonResponse,
  readCount,
  readOffers,
} from '../../../helpers/stepUpApi';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  apiRequestContextIsCurrent,
  type ApiRequestContext,
} from '@/renderer/services/system/requestContext';

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

// ── API double ─────────────────────────────────────────────────────────
//
// Routed by path, never by call order: the requirements read
// (`GET /api/v1/mfa/step-up`) opens each credentials stage, so it lands among
// the wizard's own requests at a point no case should depend on. "Mutant:"
// comments name the production change a case exists to turn red.

const PATHS = {
  totpSetup: '/api/v1/mfa/totp/setup',
  verifySetup: '/api/v1/mfa/totp/verify-setup',
  confirmSetup: '/api/v1/mfa/totp/confirm-setup',
  recoveryKey: '/api/v1/mfa/recovery-key',
  keyBegin: '/api/v1/mfa/webauthn/register/begin',
  inlineBegin: '/api/v1/mfa/webauthn/verify-inline/begin',
  keyFinish: '/api/v1/mfa/webauthn/register/finish',
} as const;

const FIXTURE_PW = 'mypassword';
const FIXTURE_OTP = '654321';
const REPLACE_LABEL = 'Replace recovery key';
const CODE_LABEL = 'Authenticator app code';

const SETUP_BODY = {
  otpauth_url: 'otpauth://totp/Concord:test@example.com?secret=JBSWY3DPEHPK3PXP',
  secret: 'JBSWY3DPEHPK3PXP',
};
const CREATION_OPTIONS = {
  publicKey: {
    challenge: 'dGVzdC1jaGFsbGVuZ2U',
    rp: { name: 'Concord', id: 'localhost' },
    user: { id: 'dXNlci0x', name: 'test@example.com', displayName: 'Test' },
    pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
  },
};

type Reply = Response | Promise<Response>;
/** A route's answer. `n` is how many times that path has been asked, from 1. */
type Route = (n: number) => Reply;

interface Scenario {
  /** What the requirements read offers. Set on the returned handle to change it mid-case. */
  offers?: string[];
  backup?: boolean;
  totpSetup?: Route;
  verifySetup?: Route;
  confirmSetup?: Route;
  recoveryKey?: Route;
  keyBegin?: Route;
  keyFinish?: Route;
  /** The step-up's own security-key begin (`verify-inline`), not the registration's. */
  inlineBegin?: Route;
}

interface Api {
  offers: string[];
  backup: boolean;
}

function serve(scenario: Scenario = {}): Api {
  const api: Api = { offers: scenario.offers ?? [], backup: scenario.backup ?? false };
  const asked = new Map<string, number>();
  const ok = (): Reply => jsonResponse(200, {});
  const routes: Record<string, Route> = {
    [PATHS.totpSetup]: scenario.totpSetup ?? (() => jsonResponse(200, SETUP_BODY)),
    [PATHS.verifySetup]:
      scenario.verifySetup ?? (() => jsonResponse(200, { backup_codes: ['CODE1'] })),
    [PATHS.confirmSetup]: scenario.confirmSetup ?? ok,
    [PATHS.recoveryKey]: scenario.recoveryKey ?? ok,
    [PATHS.keyBegin]: scenario.keyBegin ?? (() => jsonResponse(200, CREATION_OPTIONS)),
    [PATHS.keyFinish]: scenario.keyFinish ?? ok,
    ...(scenario.inlineBegin ? { [PATHS.inlineBegin]: scenario.inlineBegin } : {}),
  };
  installStepUpApi(mockApiFetch, {
    read: () => readOffers(api.offers, api.backup),
    route: (path) => {
      const route = routes[path];
      if (!route) throw new Error(`unexpected request ${path}`);
      const n = (asked.get(path) ?? 0) + 1;
      asked.set(path, n);
      return route(n);
    },
  });
  return api;
}

/** A first store that finds a key already held, then whatever `later` answers. */
function keptThen(later: Route = () => jsonResponse(200, {})): Route {
  return (n) => (n === 1 ? jsonResponse(403, { password_required: true }) : later(n));
}

const networkLoss = (): never => {
  throw new TypeError('Failed to fetch');
};

const callsTo = (path: string) => mockApiFetch.mock.calls.filter((c) => c[0] === path);
/** The `ApiRequestContext` each request to `path` was admitted against, if any. */
const contextsOf = (path: string) =>
  callsTo(path).map((c) => (c[2] as { context?: unknown } | undefined)?.context);

// ── Drivers ────────────────────────────────────────────────────────────

const onComplete = vi.fn();
const onCancel = vi.fn();

type SetupProps = Partial<React.ComponentProps<typeof MFASetup>>;

const renderTotp = (props: SetupProps = {}) =>
  render(<MFASetup method="totp" onComplete={onComplete} onCancel={onCancel} {...props} />);
const renderKey = (props: SetupProps = {}) =>
  render(<MFASetup method="webauthn" onComplete={onComplete} onCancel={onCancel} {...props} />);

const button = (name: string) => screen.getByRole('button', { name });
const click = (name: string) => fireEvent.click(button(name));
const typePassword = (value = FIXTURE_PW) =>
  fireEvent.change(screen.getByLabelText('Password'), { target: { value } });
/** Waits for the field: it exists only once the read has offered the method. */
const typeCode = async (value = FIXTURE_OTP, label = CODE_LABEL) =>
  fireEvent.change(await screen.findByLabelText(label), { target: { value } });
const valueOf = (label: string) => (screen.getByLabelText(label) as HTMLInputElement).value;
/** Resolves once the primary can act: the read landed and everything is filled. */
const untilActionable = (name: string) =>
  waitFor(() => expect(button(name)).not.toHaveAttribute('aria-disabled'));

/** Password (and code, when the read offers TOTP) typed and Continue pressed. */
async function beginTotp(withCode = false) {
  typePassword();
  if (withCode) await typeCode();
  await untilActionable('Continue');
  click('Continue');
}

/** Drives the authenticator flow to the QR step. */
async function toQr() {
  renderTotp();
  await beginTotp();
  await screen.findByTestId('totp-submit');
}

async function toBackupCodes() {
  await toQr();
  fireEvent.click(screen.getByTestId('totp-submit'));
  await screen.findByTestId('backup-confirm');
}

/** Confirms the backup codes and so starts the first recovery-key store. */
async function toRecoveryStep() {
  await toBackupCodes();
  fireEvent.click(screen.getByTestId('backup-confirm'));
}

/** Through a first store that finds a key held, and into the replace step. */
async function toReplaceStep(api: Api, offers: string[] = []) {
  await toKeptStep();
  api.offers = offers;
  click(REPLACE_LABEL);
  await screen.findByText('Your old recovery key will stop working.');
}

/** Reaches the "kept" step without opening the replace step. */
async function toKeptStep() {
  await toRecoveryStep();
  await screen.findByRole('button', { name: REPLACE_LABEL });
}

const recoveryText = (text: string) => screen.findByText(text);
const FAILED_COPY =
  "We couldn't create your recovery key. Without one, you'll lose access to your encrypted message history if you forget your password.";
const UNAVAILABLE_COPY =
  "We couldn't create your recovery key because your encryption keys aren't unlocked on this device. Without one, you'll lose access to your encrypted message history if you forget your password.";

/** Stubs `navigator.credentials.create` for the security-key ceremony. */
function stubCreate(create: () => Promise<unknown>) {
  Object.defineProperty(navigator, 'credentials', {
    value: { create: vi.fn(create) },
    writable: true,
    configurable: true,
  });
}

const mockCredential = () => {
  const buffer = new Uint8Array([1, 2, 3]).buffer;
  return {
    id: 'credential-id',
    rawId: buffer,
    type: 'public-key',
    response: { attestationObject: buffer, clientDataJSON: buffer },
  };
};

/** Password (and code) typed and Register Key pressed. */
async function beginKey(withCode = false) {
  typePassword();
  if (withCode) await typeCode();
  await untilActionable('Register Key');
  click('Register Key');
}

describe('MFASetup', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    useAuthStore.getState().setAccessToken('mock-token');
    serve();
  });

  // ── TOTP Flow (#5) ─────────────────────────────────────────────────────

  describe('TOTP Flow', () => {
    it('renders TOTP setup wizard title', () => {
      renderTotp();
      expect(screen.getByText('Set Up Authenticator App')).toBeInTheDocument();
    });

    it('labels the password field', () => {
      renderTotp();
      expect(screen.getByLabelText('Password')).toBeInTheDocument();
    });

    it('renders continue and cancel buttons', () => {
      renderTotp();
      expect(button('Continue')).toBeInTheDocument();
      expect(button('Cancel')).toBeInTheDocument();
    });

    it('shows setup prompt for new MFA', () => {
      renderTotp();
      expect(screen.getByText('Enter your password to begin setup.')).toBeInTheDocument();
    });

    it('shows identity verification message when mfaActive', () => {
      renderTotp({ mfaActive: true });
      expect(screen.getByText('Verify your identity to add another method.')).toBeInTheDocument();
    });

    it('calls onCancel when cancel button is clicked', () => {
      renderTotp();
      click('Cancel');
      expect(onCancel).toHaveBeenCalled();
    });

    // §1.3. Mutant: the code field renders unconditionally. An account the read
    // says has no inline method would be asked for a code it cannot produce.
    it('asks for no code when the read offers no method', async () => {
      serve({ offers: [] });
      renderTotp();
      typePassword();
      await untilActionable('Continue');

      expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
    });

    it('asks for the authenticator code when the read offers TOTP, with or without mfaActive', async () => {
      serve({ offers: ['totp'] });
      renderTotp();

      expect(await screen.findByLabelText(CODE_LABEL)).toBeInTheDocument();
      // The read decides, not the prop: the wording follows what it found.
      expect(screen.getByText('Verify your identity to add another method.')).toBeInTheDocument();
    });

    // Mutant: `allowBackup: true` on the setup stage. Enrolment must stay on the
    // real factor, so a backup code is never offered here even when the account
    // holds some (design §6).
    it.each([['totp'], ['webauthn']] as const)(
      'never offers a backup code on the %s flow, though the account has them',
      async (method) => {
        serve({ offers: ['totp'], backup: true });
        render(<MFASetup method={method} onComplete={onComplete} onCancel={onCancel} />);
        await screen.findByLabelText(CODE_LABEL);

        expect(
          screen.queryByRole('button', { name: 'Use a backup code instead' })
        ).not.toBeInTheDocument();
        expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
      }
    );

    it('keeps Continue aria-disabled, not natively disabled, until the password is entered', async () => {
      renderTotp();
      expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');
      expect(button('Continue')).not.toBeDisabled();

      click('Continue');
      expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
      expect(callsTo(PATHS.totpSetup)).toHaveLength(0);
    });

    it('enables Continue once the password is entered and the read landed', async () => {
      renderTotp();
      typePassword();
      await untilActionable('Continue');
      expect(button('Continue')).not.toBeDisabled();
    });

    it('keeps Continue down, saying what is missing, until the offered code is entered', async () => {
      serve({ offers: ['totp'] });
      renderTotp({ mfaActive: true });
      typePassword();
      await screen.findByLabelText(CODE_LABEL);
      expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');

      click('Continue');
      expect(
        await screen.findByText('Enter the 6-digit code from your authenticator app to continue.')
      ).toBeInTheDocument();
      expect(callsTo(PATHS.totpSetup)).toHaveLength(0);
    });

    it('calls TOTP setup with the password, under the capture the run took', async () => {
      renderTotp();
      await beginTotp();

      await waitFor(() => expect(callsTo(PATHS.totpSetup)).toHaveLength(1));
      const [body] = bodiesTo(mockApiFetch, PATHS.totpSetup);
      expect(body).toEqual({ password: FIXTURE_PW });
      expect(callsTo(PATHS.totpSetup)[0][1]).toMatchObject({ method: 'POST' });
      const [context] = contextsOf(PATHS.totpSetup);
      expect(context).toBeDefined();
      expect(apiRequestContextIsCurrent(context as never)).toBe(true);
    });

    it('includes mfa_code in the setup request when the read offers TOTP', async () => {
      serve({ offers: ['totp'] });
      renderTotp({ mfaActive: true });
      await beginTotp(true);

      await waitFor(() => expect(callsTo(PATHS.totpSetup)).toHaveLength(1));
      expect(bodiesTo(mockApiFetch, PATHS.totpSetup)[0]).toEqual({
        password: FIXTURE_PW,
        mfa_code: FIXTURE_OTP,
      });
    });

    it('advances to QR step after successful setup', async () => {
      await toQr();
      expect(
        screen.getByText(
          'Scan this QR code with your authenticator app, then enter the 6-digit code below.'
        )
      ).toBeInTheDocument();
    });

    it('shows manual secret entry on QR step', async () => {
      await toQr();
      expect(screen.getByText("Can't scan? Enter manually")).toBeInTheDocument();
      expect(screen.getByText('JBSWY3DPEHPK3PXP')).toBeInTheDocument();
    });

    it('treats an accepted setup answer without a secret as a failed begin', async () => {
      serve({ totpSetup: () => jsonResponse(200, { unexpected: true }) });
      renderTotp();
      await beginTotp();

      expect(await screen.findByText('Something went wrong. Try again.')).toBeInTheDocument();
      expect(screen.queryByTestId('totp-input')).not.toBeInTheDocument();
    });

    it('shows the server text on a setup failure', async () => {
      serve({ totpSetup: () => jsonResponse(500, { error: 'Server unavailable' }) });
      renderTotp();
      await beginTotp();

      expect(await screen.findByText('Server unavailable')).toBeInTheDocument();
      expect(document.querySelector('.mfa-setup-error-banner')).toBeInTheDocument();
    });

    it('shows the shared copy against the password field for an invalidPassword refusal', async () => {
      serve({ totpSetup: () => jsonResponse(403, { error: 'Invalid password' }) });
      renderTotp();
      await beginTotp();

      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
      // The server read the password and refused it: it is not offered again.
      expect(valueOf('Password')).toBe('');
    });

    // The code was never read when the password failed, so it is still good.
    it('keeps the code through a password refusal and drops only the password', async () => {
      serve({
        offers: ['totp'],
        totpSetup: () => jsonResponse(403, { error: 'Invalid password' }),
      });
      renderTotp({ mfaActive: true });
      await beginTotp(true);

      await screen.findByText('That password is not correct.');
      expect(valueOf(CODE_LABEL)).toBe(FIXTURE_OTP);
      expect(valueOf('Password')).toBe('');
    });

    it('shows the shared code copy, and no banner, for an invalidMfaCode refusal', async () => {
      serve({
        offers: ['totp'],
        totpSetup: () => jsonResponse(403, { error: 'Invalid MFA code' }),
      });
      renderTotp({ mfaActive: true });
      await beginTotp(true);

      expect(
        await screen.findByText(
          "That code didn't work. It may be mistyped or already used. Enter the next code your app shows."
        )
      ).toBeInTheDocument();
      expect(document.querySelector('.mfa-setup-error-banner')).not.toBeInTheDocument();
    });

    // F3's twin. The read said no method was needed; the server then named one.
    it('shows the code field when a setup answer names a method the read did not offer', async () => {
      serve({
        offers: [],
        totpSetup: () =>
          jsonResponse(403, {
            error: 'MFA verification required',
            mfa_required: true,
            methods: ['totp', 'email'],
          }),
      });
      renderTotp();
      await beginTotp();

      const field = await screen.findByLabelText(CODE_LABEL);
      expect(field).toBeInTheDocument();
      expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');
      await typeCode();
      expect(button('Continue')).not.toHaveAttribute('aria-disabled');
    });

    // Server budget: a spent budget names itself, and the code is untouched.
    it('shows the shared rate-limit copy for a 429 setup refusal', async () => {
      serve({
        totpSetup: () => jsonResponse(429, { error: 'Too many verification attempts' }),
      });
      renderTotp();
      await beginTotp();

      expect(
        await screen.findByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
    });

    // A sent code is never offered again. The server accepts each TOTP code once
    // and can accept it yet still fail the request.
    it('a failed setup request clears the code and puts Continue back down', async () => {
      serve({
        offers: ['totp'],
        totpSetup: () => jsonResponse(500, { error: 'Internal server error' }),
      });
      renderTotp({ mfaActive: true });
      await beginTotp(true);

      expect(await screen.findByText('Internal server error')).toBeInTheDocument();
      expect(valueOf(CODE_LABEL)).toBe('');
      expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');
    });

    it('shows error on TOTP verify failure', async () => {
      serve({ verifySetup: () => jsonResponse(400, { error: 'Invalid TOTP code' }) });
      await toQr();
      fireEvent.click(screen.getByTestId('totp-submit'));

      expect(await screen.findByTestId('totp-error')).toHaveTextContent('Invalid TOTP code');
    });

    it('advances to backup codes after TOTP verification', async () => {
      serve({
        verifySetup: () =>
          jsonResponse(200, { backup_codes: ['AAAA1111', 'BBBB2222', 'CCCC3333'] }),
      });
      await toBackupCodes();
      expect(screen.getByText('AAAA1111, BBBB2222, CCCC3333')).toBeInTheDocument();
    });

    it('shows error on confirm-setup failure and does not refresh the session', async () => {
      serve({ confirmSetup: () => jsonResponse(500, { error: 'Session expired' }) });
      await toBackupCodes();
      fireEvent.click(screen.getByTestId('backup-confirm'));

      expect(await screen.findByText('Session expired')).toBeInTheDocument();
      expect(
        mockRefreshAccessToken,
        'MFA did not activate, so there is no grant to use'
      ).not.toHaveBeenCalled();
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(0);
    });

    it('completes full TOTP flow through recovery key', async () => {
      await toRecoveryStep();
      await screen.findByText('AAAA-BBBB-CCCC-DDDD');
      // The server exempts this session from the pre-MFA challenge for 30 s
      // after enrollment; refreshing now uses that grant instead of prompting
      // for the code again at the next token refresh.
      expect(mockRefreshAccessToken).toHaveBeenCalledTimes(1);

      fireEvent.click(screen.getByTestId('recovery-confirm'));
      expect(await screen.findByText('MFA Activated!')).toBeInTheDocument();

      click('Done');
      expect(onComplete).toHaveBeenCalled();
    });

    // The refresh is not awaited, so a rejection must be caught where it is
    // made; setup still finishes (frontend review, PR #3437).
    it('finishes TOTP setup when the refresh after enrollment rejects', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      mockRefreshAccessToken.mockRejectedValueOnce(new Error('ipc unavailable'));

      await toRecoveryStep();
      await screen.findByTestId('recovery-key-display');

      await waitFor(() =>
        expect(warn).toHaveBeenCalledWith('[mfa] Refresh after enrollment failed')
      );
      warn.mockRestore();
    });

    it('skips recovery key when skip is clicked', async () => {
      await toRecoveryStep();
      fireEvent.click(await screen.findByTestId('recovery-skip'));
      expect(await screen.findByText('MFA Activated!')).toBeInTheDocument();
    });

    it('routes to recovery-failed (unavailable) when wrapping key is null — never silently to done', async () => {
      mockE2eeService.getWrappingKey.mockReturnValueOnce(null);
      await toRecoveryStep();

      expect(await recoveryText(UNAVAILABLE_COPY)).toBeInTheDocument();
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
      // unavailable never offers a retry — retrying cannot succeed.
      expect(screen.queryByText('Try again')).not.toBeInTheDocument();
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(0);

      click('Continue');
      expect(await screen.findByText('MFA Activated!')).toBeInTheDocument();
    });

    it('routes to recovery-failed (failed) when the recovery-key store call fails — never silently to done', async () => {
      // A plain 500 — not 403, so this is 'failed' rather than 'kept'.
      serve({ recoveryKey: () => jsonResponse(500, { error: 'storage error' }) });
      await toRecoveryStep();

      expect(await recoveryText(FAILED_COPY)).toBeInTheDocument();
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
      expect(screen.getByText('Try again')).toBeInTheDocument();

      click('Continue without a recovery key');
      expect(await screen.findByText('MFA Activated!')).toBeInTheDocument();
    });

    it('wraps the prefs key into the recovery-key upload when available', async () => {
      await toRecoveryStep();
      await screen.findByTestId('recovery-key-display');

      const [body] = bodiesTo(mockApiFetch, PATHS.recoveryKey);
      expect(body.recovery_wrapped_prefs_key).toBe('mock-wrapped-prefs');
      expect(body.recovery_prefs_key_salt).toBe('mock-prefs-salt');
    });

    it('omits the prefs payload when there is no prefs key', async () => {
      mockE2eeService.getPreferencesKeyBase64.mockReturnValueOnce(null);
      await toRecoveryStep();
      await screen.findByTestId('recovery-key-display');

      const [body] = bodiesTo(mockApiFetch, PATHS.recoveryKey);
      expect(body.recovery_wrapped_private_key).toBe('mock-wrapped-key');
      expect(body).not.toHaveProperty('recovery_wrapped_prefs_key');
      expect(body).not.toHaveProperty('recovery_prefs_key_salt');
    });

    // The first store needs no credentials: the server inserts only when no key exists.
    it('sends the first recovery-key store without credentials', async () => {
      await toRecoveryStep();
      await screen.findByTestId('recovery-key-display');

      const [body] = bodiesTo(mockApiFetch, PATHS.recoveryKey);
      expect(body).not.toHaveProperty('password');
      expect(body).not.toHaveProperty('mfa_code');
    });
  });

  // ── Recovery key generation exception ───────────────────────────────

  describe('Recovery key generation exception', () => {
    it('routes to recovery-failed when generateRecoveryKey throws — never silently to done', async () => {
      vi.mocked(generateRecoveryKey).mockImplementationOnce(() => {
        throw new Error('crypto failure');
      });
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      await toRecoveryStep();

      expect(await recoveryText(FAILED_COPY)).toBeInTheDocument();
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(0);
      warn.mockRestore();

      click('Continue without a recovery key');
      expect(await screen.findByText('MFA Activated!')).toBeInTheDocument();
    });
  });

  // ── Recovery-key outcome routing (spec §4.6.3 — the 'kept' branch) ───────

  describe('recovery-key outcome routing — kept', () => {
    it('routes to recovery-kept via a 403 password_required body', async () => {
      serve({ recoveryKey: () => jsonResponse(403, { password_required: true }) });
      await toRecoveryStep();

      expect(
        await screen.findByText(/A recovery key is already saved for your account/)
      ).toBeInTheDocument();
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
    });

    it('routes to recovery-kept via an mfa_required-only body (SSO-shaped, R-11)', async () => {
      serve({ recoveryKey: () => jsonResponse(403, { mfa_required: true }) });
      await toRecoveryStep();

      expect(
        await screen.findByText(/A recovery key is already saved for your account/)
      ).toBeInTheDocument();
      expect(screen.queryByText('MFA Activated!')).not.toBeInTheDocument();
    });
  });

  // ── Retry never re-calls confirm-setup (spec §4.6.3, "upload-only retry") ─

  describe('retry after recovery-failed', () => {
    it('calls only the key upload, never confirm-setup again', async () => {
      serve({
        recoveryKey: (n) =>
          n === 1 ? jsonResponse(500, { error: 'storage error' }) : jsonResponse(200),
      });
      await toRecoveryStep();
      await screen.findByText('Try again');
      expect(callsTo(PATHS.confirmSetup)).toHaveLength(1);

      click('Try again');
      // A successful retry routes to 'created', the same target as the
      // first-try happy path — the recovery-key display, not straight to 'done'.
      await screen.findByTestId('recovery-key-display');

      // The retry must not have called confirm-setup a second time — it had
      // already committed before the recovery-key upload ever ran.
      expect(callsTo(PATHS.confirmSetup)).toHaveLength(1);
    });

    // regression: repeat retry failure re-rendered an identical screen
    it('changes the alert text on every repeat failure so a retry never looks inert', async () => {
      serve({ recoveryKey: () => jsonResponse(500, { error: 'storage error' }) });
      await toRecoveryStep();

      await screen.findByText('Try again');
      const firstFailureText = screen.getByRole('alert').textContent;
      expect(firstFailureText).toContain('recovery key');
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(1);

      // Retry #1 fails again.
      click('Try again');
      await waitFor(() => expect(callsTo(PATHS.recoveryKey)).toHaveLength(2));
      await waitFor(() => expect(button('Try again')).toBeEnabled());
      const retry1Text = screen.getByRole('alert').textContent;
      expect(retry1Text).toContain('recovery key');

      // Retry #2 fails again.
      click('Try again');
      await waitFor(() => expect(callsTo(PATHS.recoveryKey)).toHaveLength(3));
      await waitFor(() => expect(button('Try again')).toBeEnabled());
      const retry2Text = screen.getByRole('alert').textContent;
      expect(retry2Text).toContain('recovery key');

      expect(retry1Text, 'a repeat failure must change the alert text').not.toBe(firstFailureText);
      expect(retry2Text, 'a repeat failure must change the alert text').not.toBe(retry1Text);
    });
  });

  // F16: the done screen says plainly when there is no usable key.
  it('continuing without a recovery key says so on the done screen (F16)', async () => {
    serve({ recoveryKey: () => jsonResponse(500, {}) });
    await toRecoveryStep();

    await screen.findByText('Continue without a recovery key');
    click('Continue without a recovery key');
    expect(await screen.findByText('MFA Activated!')).toBeInTheDocument();
    expect(screen.getByText(/This account has no recovery key you can use\./)).toBeInTheDocument();
  });

  // ── Replace step (#6; spec §4.6.4, R-9) ───────────────────────────────────

  /** The recovery-* fields of every recovery-key PUT, in call order. */
  const recoveryBodies = () =>
    bodiesTo(mockApiFetch, PATHS.recoveryKey).map((body) => ({
      recovery_wrapped_private_key: body.recovery_wrapped_private_key,
      recovery_key_salt: body.recovery_key_salt,
      recovery_wrapped_prefs_key: body.recovery_wrapped_prefs_key,
      recovery_prefs_key_salt: body.recovery_prefs_key_salt,
    }));

  const replaceButton = () => button(REPLACE_LABEL);

  describe('recovery-key replace step', () => {
    it('Back returns to recovery-kept without submitting a request', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toReplaceStep(api);

      click('Back');
      expect(
        await screen.findByText(/A recovery key is already saved for your account/)
      ).toBeInTheDocument();
      // Only the first store: opening and leaving the step sent no key.
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(1);
    });

    it('happy path: submits the new key and lands on the recovery step showing it', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toReplaceStep(api, ['totp']);

      typePassword('freshpw');
      await typeCode();
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      await screen.findByTestId('recovery-key-display');
      expect(screen.getByText('AAAA-BBBB-CCCC-DDDD')).toBeInTheDocument();
      const [, body] = bodiesTo(mockApiFetch, PATHS.recoveryKey);
      expect(body).toMatchObject({
        password: 'freshpw', // pragma: allowlist secret
        mfa_code: FIXTURE_OTP,
        recovery_wrapped_private_key: 'mock-wrapped-key', // pragma: allowlist secret
      });
    });

    // §1.3. Mutant: the code field renders unconditionally on the replace step.
    it('asks for the password alone when the read offers no method', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toReplaceStep(api, []);

      typePassword('freshpw');
      await untilActionable(REPLACE_LABEL);
      expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();

      click(REPLACE_LABEL);
      await screen.findByTestId('recovery-key-display');
      const [, body] = bodiesTo(mockApiFetch, PATHS.recoveryKey);
      expect(body).toHaveProperty('password', 'freshpw');
      expect(body).not.toHaveProperty('mfa_code');
    });

    // The replace step is the one #5/#6 surface that does take a backup code.
    it('offers a backup code beside TOTP when the read reports one, and sends it as mfa_code', async () => {
      const api = serve({ recoveryKey: keptThen(), backup: true });
      await toReplaceStep(api, ['totp']);

      typePassword('freshpw');
      fireEvent.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
      fireEvent.change(screen.getByLabelText('Backup code'), { target: { value: 'EXCI3G5F' } });
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      await screen.findByTestId('recovery-key-display');
      expect(bodiesTo(mockApiFetch, PATHS.recoveryKey)[1]).toMatchObject({ mfa_code: 'EXCI3G5F' });
    });

    it('refusal routing: invalidPassword shows the field error and clears the password', async () => {
      const api = serve({
        recoveryKey: keptThen(() => jsonResponse(403, { error: 'Invalid password' })),
      });
      await toReplaceStep(api, ['totp']);
      typePassword('wrongpw');
      await typeCode('000000');
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect(valueOf('Password')).toBe('');
      // Refusal keeps the wizard on the replace step, not the recovery step.
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
    });

    it('refusal routing: invalidMfaCode shows the code error and clears the code', async () => {
      const api = serve({
        recoveryKey: keptThen(() => jsonResponse(403, { error: 'Invalid MFA code' })),
      });
      await toReplaceStep(api, ['totp']);
      typePassword('freshpw');
      await typeCode('000000');
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      expect(
        await screen.findByText(
          "That code didn't work. It may be mistyped or already used. Enter the next code your app shows."
        )
      ).toBeInTheDocument();
      expect(valueOf(CODE_LABEL)).toBe('');
    });

    // The server accepts each code once and can accept it yet still fail the
    // request, so a code that reached it is never offered again.
    it('a 500 after the code was sent clears the code and puts Replace back down', async () => {
      const api = serve({
        recoveryKey: keptThen(() => jsonResponse(500, { error: 'Internal server error' })),
      });
      await toReplaceStep(api, ['totp']);
      typePassword('freshpw');
      await typeCode();
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      expect(await screen.findByText('Internal server error')).toBeInTheDocument();
      expect(valueOf(CODE_LABEL)).toBe('');
      expect(replaceButton()).toHaveAttribute('aria-disabled', 'true');
    });

    it('a wrong password keeps the code, which the server never read', async () => {
      const api = serve({
        recoveryKey: keptThen(() => jsonResponse(403, { error: 'Invalid password' })),
      });
      await toReplaceStep(api, ['totp']);
      typePassword('wrongpw');
      await typeCode();
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      await screen.findByText('That password is not correct.');
      expect(valueOf(CODE_LABEL)).toBe(FIXTURE_OTP);
    });
  });

  // ── Preparation before the activation (#6; C9, C82, D11) ──────────────────

  describe('recovery-key replace step — preparation', () => {
    // Mutant: the stage drops `preparing`. The primary would be live while the
    // key is still being wrapped, and a click would run before there is
    // anything to send.
    it('keeps the primary down and sends no PUT until the new key is prepared', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toKeptStep();
      const wrapping = deferred<{ wrappedKey: string; salt: string }>();
      vi.mocked(wrapWithRecoveryKey).mockImplementationOnce(() => wrapping.promise);

      api.offers = [];
      click(REPLACE_LABEL);
      await screen.findByText('Your old recovery key will stop working.');
      typePassword('freshpw');
      // The read has landed and the password is in: only preparation is missing.
      await waitFor(() => expect(readCount(mockApiFetch)).toBe(2));
      await act(async () => {});

      expect(replaceButton()).toHaveAttribute('aria-disabled', 'true');
      click(REPLACE_LABEL);
      expect(screen.getByRole('status')).toHaveTextContent('Getting things ready…');
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(1);

      await act(async () => wrapping.resolve({ wrappedKey: 'late-wrapped', salt: 'late-salt' }));
      await untilActionable(REPLACE_LABEL);
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(1);

      click(REPLACE_LABEL);
      await screen.findByTestId('recovery-key-display');
      expect(bodiesTo(mockApiFetch, PATHS.recoveryKey)[1]).toMatchObject({
        recovery_wrapped_private_key: 'late-wrapped', // pragma: allowlist secret
      });
    });

    // C82. Mutant: the activation passes no capture, so `run` takes its own at
    // the click. A change of account after the step opened would then go
    // unnoticed, and key material wrapped for one account would be sent as
    // another.
    it('sends nothing when the account changed after the step opened', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toReplaceStep(api);
      typePassword('freshpw');
      await untilActionable(REPLACE_LABEL);

      act(() => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 })));
      click(REPLACE_LABEL);

      await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Sign in again'));
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(1);
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
    });

    it('sends the PUT under a live ApiRequestContext', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toReplaceStep(api);
      typePassword('freshpw');
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);
      await screen.findByTestId('recovery-key-display');

      const [, replaceContext] = contextsOf(PATHS.recoveryKey);
      expect(replaceContext).toBeDefined();
      expect(apiRequestContextIsCurrent(replaceContext as never)).toBe(true);
    });

    it('stays in the step, saying so, when this device holds no keys to wrap', async () => {
      const api = serve({ recoveryKey: keptThen() });
      await toKeptStep();
      mockE2eeService.getWrappingKey.mockReturnValueOnce(null);
      api.offers = [];
      click(REPLACE_LABEL);
      await screen.findByText('Your old recovery key will stop working.');
      typePassword('freshpw');
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      expect(
        await screen.findByText(
          "Your encryption keys aren't unlocked on this device, so a new recovery key can't be made here."
        )
      ).toBeInTheDocument();
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(1);
    });
  });

  // ── I1: the replace step routes every refusal kind ────────────────────────

  describe('recovery-key replace step — refusal table (I1)', () => {
    const attempt = async () => {
      typePassword('freshpw');
      await typeCode();
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);
      await waitFor(() => expect(screen.queryByText('Replacing...')).not.toBeInTheDocument());
    };

    const toReplace = async (response: 'network' | { status: number; body: unknown }) => {
      const api = serve({
        recoveryKey: keptThen(() =>
          response === 'network' ? networkLoss() : jsonResponse(response.status, response.body)
        ),
      });
      await toReplaceStep(api, ['totp']);
      return api;
    };

    it.each([
      {
        name: 'passwordRequired → password field error, code kept',
        response: { status: 403, body: { password_required: true } },
        text: 'Enter your password to continue.',
        keepsCode: true,
        locks: false,
      },
      {
        name: 'mfaRequired → code field asks again',
        response: { status: 403, body: { mfa_required: true, methods: ['totp'] } },
        text: 'Enter the 6-digit code from your authenticator app to continue.',
        locks: false,
      },
      {
        // The budget answers before anything is read (C30), so the code is kept
        // for when the limit lifts. Mutant: the hook's default clearing it.
        name: 'rateLimited → banner and lock, code kept',
        response: { status: 429, body: { error: 'Too many verification attempts' } },
        text: 'Too many attempts. Try again in a few minutes.',
        keepsCode: true,
        locks: true,
      },
      {
        name: 'unavailable → outage banner, no lock',
        response: { status: 503, body: {} },
        text: 'Verification is temporarily unavailable. Try again in a few minutes.',
        locks: false,
      },
      {
        name: 'failed → the server text in the banner',
        response: { status: 500, body: { error: 'Verification failed' } },
        text: 'Verification failed',
        locks: false,
      },
    ])('$name', async ({ response, text, keepsCode, locks }) => {
      await toReplace(response);
      await attempt();

      expect(await screen.findByText(text)).toBeInTheDocument();
      // None of these refusals names the password wrong, so it stays.
      expect(valueOf('Password')).toBe('freshpw');
      // A password refusal and a rate limit leave the code unread; every other
      // answer may have spent it, so the field comes back empty.
      expect(valueOf(CODE_LABEL)).toBe(keepsCode ? FIXTURE_OTP : '');
      // Re-entering what was cleared re-enables Replace unless the refusal locked it.
      if (!keepsCode) await typeCode('765432');
      if (locks) expect(replaceButton()).toHaveAttribute('aria-disabled', 'true');
      else expect(replaceButton()).not.toHaveAttribute('aria-disabled');
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
    });

    // Mutant: a 401 mapped to `answered`, which left the stage live with the
    // password and an active Replace under a dead session (picker PR 3 review).
    it('sessionExpired → the terminal state: one sentence, no fields, Replace inert', async () => {
      await toReplace({ status: 401, body: {} });
      await attempt();

      expect(await screen.findByText('Sign in again to continue.')).toBeInTheDocument();
      expect(screen.getAllByText(/Sign in again/)).toHaveLength(1);
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
      expect(replaceButton()).toHaveAttribute('aria-disabled', 'true');
      click(REPLACE_LABEL);
      // The first store and the one refused attempt; the inert click added none.
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(2);
    });

    it('a locked primary sends nothing when activated', async () => {
      await toReplace({ status: 429, body: {} });
      await attempt();
      await screen.findByText('Too many attempts. Try again in a few minutes.');

      typePassword('freshpw');
      await typeCode('765432');
      click(REPLACE_LABEL);
      // The first store and the one refused attempt; the locked click added none.
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(2);
    });

    it('a lock survives Back and reopening the step (F10)', async () => {
      const api = await toReplace({ status: 429, body: {} });
      await attempt();
      click('Back');
      api.offers = ['totp'];
      click(REPLACE_LABEL);
      typePassword('freshpw');
      await typeCode();

      expect(replaceButton()).toHaveAttribute('aria-disabled', 'true');
      expect(
        screen.getByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
    });

    it('a password refusal focuses the password field once it is enabled again (F4)', async () => {
      await toReplace({ status: 403, body: { error: 'Invalid password' } });
      await attempt();
      await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText('Password')));
    });

    it('after an ambiguous outcome, Back no longer claims the old key was left in place', async () => {
      await toReplace('network');
      await attempt();
      await screen.findByText("Couldn't reach the server. Check your connection and try again.");
      click('Back');

      expect(screen.queryByText(/so we left it in place/)).not.toBeInTheDocument();
      expect(
        screen.getByText(/couldn't confirm whether your recovery key was replaced/)
      ).toBeInTheDocument();
      expect(button('Finish replacing')).toBeInTheDocument();

      click('Continue');
      expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      expect(
        screen.getByText(/couldn't confirm your recovery key was replaced/)
      ).toBeInTheDocument();
    });

    it('Finish replacing reopens the step and a success lands on the new key', async () => {
      const api = serve({
        recoveryKey: (n) =>
          n === 1
            ? jsonResponse(403, { password_required: true })
            : n === 2
              ? networkLoss()
              : jsonResponse(200),
      });
      await toReplaceStep(api, ['totp']);
      await attempt();
      await screen.findByText("Couldn't reach the server. Check your connection and try again.");
      click('Back');
      click('Finish replacing');
      await attempt();

      await screen.findByTestId('recovery-key-display');
      fireEvent.click(screen.getByTestId('recovery-confirm'));
      expect(screen.getByText('MFA Activated!')).toBeInTheDocument();
      expect(screen.queryByText(/couldn't confirm/)).not.toBeInTheDocument();
    });

    it('the lead line and the consequence come before the fields (F16)', async () => {
      await toReplace({ status: 500, body: {} });
      const consequence = screen.getByText('Your old recovery key will stop working.');
      const password = screen.getByLabelText('Password');
      expect(screen.getByText(/^Make a new recovery key\./)).toBeInTheDocument();
      expect(
        consequence.compareDocumentPosition(password) & Node.DOCUMENT_POSITION_FOLLOWING
      ).toBeTruthy();
    });
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

    /** The key a body wraps, read back out of `wrapped(<key>)`. */
    const keyWrappedBy = (wrapped: string) => /^wrapped\((.+)\)$/.exec(wrapped)?.[1];

    it('first store: a retry after a lost response resends identical bytes and shows the key they wrap', async () => {
      // PUT #1: response lost. The retry's idempotent re-store answers 200.
      serve({ recoveryKey: (n) => (n === 1 ? networkLoss() : jsonResponse(200)) });
      await toRecoveryStep();
      await screen.findByText('Try again');

      click('Try again');
      await screen.findByTestId('recovery-key-display');

      const [first, retry] = recoveryBodies();
      expect(retry, 'the retry must resend the exact bytes of the first attempt').toEqual(first);
      expect(screen.getByTestId('recovery-key').textContent).toBe(
        keyWrappedBy(retry.recovery_wrapped_private_key as string)
      );
    });

    it('replace: a retry after a network error resends identical bytes and shows the key they wrap', async () => {
      // PUT #1 is the first store (kept), #2 the lost replace, #3 its retry.
      const api = serve({
        recoveryKey: (n) =>
          n === 1
            ? jsonResponse(403, { password_required: true })
            : n === 2
              ? networkLoss()
              : jsonResponse(200),
      });
      await toReplaceStep(api, ['totp']);
      typePassword('freshpw');
      await typeCode();
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);
      await screen.findByText("Couldn't reach the server. Check your connection and try again.");

      // The first code may have been spent by the lost request, so the retry
      // needs a fresh one; the key material must still be byte-identical.
      await typeCode('765432');
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);
      await screen.findByTestId('recovery-key-display');

      const [, attempt, retry] = recoveryBodies();
      expect(retry, 'the retry must resend the exact bytes of the first attempt').toEqual(attempt);
      expect(screen.getByTestId('recovery-key').textContent).toBe(
        keyWrappedBy(retry.recovery_wrapped_private_key as string)
      );
    });
  });

  // ── WebAuthn Flow (#5) ─────────────────────────────────────────────────

  describe('WebAuthn Flow', () => {
    it('renders WebAuthn setup wizard title for hardware key', () => {
      renderKey();
      expect(screen.getByText('Set Up Security Key')).toBeInTheDocument();
    });

    it('renders WebAuthn setup wizard title for platform authenticator', () => {
      renderKey({ credentialType: 'platform' });
      expect(screen.getByText('Set Up Platform Authenticator')).toBeInTheDocument();
    });

    it('labels the password field', () => {
      renderKey();
      expect(screen.getByLabelText('Password')).toBeInTheDocument();
    });

    // Mutant: the label's `htmlFor` or the input's `id` is dropped. The field
    // would be unnamed to a screen reader, with only the placeholder left.
    it('gives the key name field an accessible label, with the hardware placeholder', () => {
      renderKey();
      const field = screen.getByLabelText('Key name');
      expect(field).toHaveAttribute('placeholder', 'Key name (e.g. YubiKey 5, Google Titan)');
    });

    it('gives the key name field an accessible label, with the platform placeholder', () => {
      renderKey({ credentialType: 'platform' });
      expect(screen.getByLabelText('Key name')).toHaveAttribute(
        'placeholder',
        'Key name (e.g. MacBook Touch ID, Windows Hello)'
      );
    });

    it('renders Register Key button', () => {
      renderKey();
      expect(button('Register Key')).toBeInTheDocument();
    });

    it('keeps Register Key aria-disabled, not natively disabled, until the password is entered', async () => {
      renderKey();
      expect(button('Register Key')).toHaveAttribute('aria-disabled', 'true');
      expect(button('Register Key')).not.toBeDisabled();

      typePassword();
      await untilActionable('Register Key');
      expect(button('Register Key')).not.toBeDisabled();
    });

    it('calls onCancel when cancel button is clicked in WebAuthn flow', () => {
      renderKey();
      click('Cancel');
      expect(onCancel).toHaveBeenCalled();
    });

    it('shows identity verification message for WebAuthn when mfaActive', () => {
      renderKey({ mfaActive: true });
      expect(screen.getByText('Verify your identity and name your key.')).toBeInTheDocument();
    });

    it('shows password prompt for WebAuthn when not mfaActive', () => {
      renderKey();
      expect(screen.getByText('Enter your password and name your key.')).toBeInTheDocument();
    });

    // §1.3.
    it('asks for no code when the read offers no method', async () => {
      renderKey();
      typePassword();
      await untilActionable('Register Key');
      expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    });

    it('keeps Register Key down until the offered code is entered', async () => {
      serve({ offers: ['totp'] });
      renderKey({ mfaActive: true });
      typePassword();
      await screen.findByLabelText(CODE_LABEL);
      expect(button('Register Key')).toHaveAttribute('aria-disabled', 'true');
      expect(callsTo(PATHS.keyBegin)).toHaveLength(0);
    });

    // The activation captured the name, as it does the password, so an edit
    // while the step-up's security-key prompt is open would be shown but not
    // sent. Mutant: the field disabled for `submitting` only (picker PR 3 review).
    it('freezes the key name while the step-up security-key prompt is open', async () => {
      const begin = deferred<Response>();
      serve({ offers: ['webauthn'], inlineBegin: () => begin.promise });
      renderKey({ mfaActive: true });
      fireEvent.change(screen.getByLabelText('Key name'), { target: { value: 'Key A' } });
      typePassword();
      await untilActionable('Register Key');
      // Positive control: editable before the activation.
      expect(screen.getByLabelText('Key name')).not.toHaveAttribute('readonly');

      click('Register Key');
      await waitFor(() => expect(callsTo(PATHS.inlineBegin)).toHaveLength(1));
      expect(screen.getByLabelText('Key name')).toHaveAttribute('readonly');

      // The prompt ends without a token: the field is editable again.
      await act(async () => begin.resolve(jsonResponse(500, {})));
      await waitFor(() =>
        expect(screen.getByLabelText('Key name')).not.toHaveAttribute('readonly')
      );
      expect(callsTo(PATHS.keyBegin)).toHaveLength(0);
    });

    it('sends the typed key name, the credential type and the code on begin and finish', async () => {
      serve({ offers: ['totp'] });
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey({ mfaActive: true, credentialType: 'platform' });
      fireEvent.change(screen.getByLabelText('Key name'), { target: { value: 'My Touch ID' } });
      await beginKey(true);

      await screen.findByText('Security Key Registered!');
      expect(bodiesTo(mockApiFetch, PATHS.keyBegin)[0]).toEqual({
        credential_name: 'My Touch ID',
        credential_type: 'platform',
        password: FIXTURE_PW,
        mfa_code: FIXTURE_OTP,
      });
      expect(bodiesTo(mockApiFetch, PATHS.keyFinish)[0]).toMatchObject({
        credential_name: 'My Touch ID',
      });
    });

    it('names the key "Security Key" when none is typed', async () => {
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();

      await screen.findByText('Security Key Registered!');
      expect(bodiesTo(mockApiFetch, PATHS.keyBegin)[0]).toMatchObject({
        credential_name: 'Security Key',
      });
    });

    it('admits both registration requests against the one capture', async () => {
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();
      await screen.findByText('Security Key Registered!');

      const [begin] = contextsOf(PATHS.keyBegin);
      const [finish] = contextsOf(PATHS.keyFinish);
      expect(begin).toBeDefined();
      expect(finish).toBe(begin);
    });

    it('shows the server text on a begin failure', async () => {
      serve({ keyBegin: () => jsonResponse(500, { error: 'Incorrect password' }) });
      renderKey();
      await beginKey();
      expect(await screen.findByText('Incorrect password')).toBeInTheDocument();
    });

    // Wrong-password and rate-limited begin refusals route through the shared
    // step-up classifier and copy (mfaStepUp.ts), not the server's raw text.
    it('shows the shared copy against the password field for an invalidPassword begin refusal', async () => {
      serve({ keyBegin: () => jsonResponse(403, { error: 'Invalid password' }) });
      renderKey();
      await beginKey();

      expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
    });

    it('shows the shared rate-limit copy for a 429 begin refusal', async () => {
      serve({
        keyBegin: () => jsonResponse(429, { error: 'Too many verification attempts' }),
      });
      renderKey();
      await beginKey();

      expect(
        await screen.findByText('Too many attempts. Try again in a few minutes.')
      ).toBeInTheDocument();
    });

    it('treats an accepted begin answer without creation options as a failed begin', async () => {
      serve({ keyBegin: () => jsonResponse(200, { nothing: true }) });
      renderKey();
      await beginKey();

      expect(await screen.findByText('Something went wrong. Try again.')).toBeInTheDocument();
      expect(screen.queryByText('Waiting for your security key...')).not.toBeInTheDocument();
    });

    it('transitions to the registering step and shows the waiting message on a successful begin', async () => {
      stubCreate(() => new Promise(() => {}));
      renderKey();
      await beginKey();

      expect(await screen.findByText('Waiting for your security key...')).toBeInTheDocument();
      // Waiting, not failed: Cancel is on offer and there is no retry yet.
      expect(screen.queryByText('Try Again')).not.toBeInTheDocument();
      expect(button('Cancel')).toBeInTheDocument();
    });

    it('shows Registering... while the begin request is out', async () => {
      serve({ keyBegin: () => new Promise<Response>(() => {}) });
      renderKey();
      await beginKey();
      expect(await screen.findByText('Registering...')).toBeInTheDocument();
    });

    it('shows the done step, and Done completes the wizard', async () => {
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();

      expect(await screen.findByText('Security Key Registered!')).toBeInTheDocument();
      expect(
        screen.getByText('Your security key is now active and protecting your account.')
      ).toBeInTheDocument();
      click('Done');
      expect(onComplete).toHaveBeenCalled();
    });

    it('reaches the done step for a platform authenticator too', async () => {
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey({ credentialType: 'platform' });
      await beginKey();
      expect(await screen.findByText('Security Key Registered!')).toBeInTheDocument();
    });

    // The server exempts the registering session from the pre-MFA challenge for
    // 30 s after a first enrollment; refreshing now uses that grant instead of
    // prompting for the new factor again at the next token refresh.
    it.each([
      [true, 1],
      [false, 0],
    ] as const)(
      'refreshes the session right after registration only when it succeeds (finish ok: %s)',
      async (finishOk, refreshes) => {
        serve({
          keyFinish: () =>
            finishOk ? jsonResponse(200, {}) : jsonResponse(400, { error: 'Registration failed' }),
        });
        stubCreate(() => Promise.resolve(mockCredential()));
        renderKey();
        await beginKey();
        await screen.findByText(finishOk ? 'Security Key Registered!' : 'Registration failed');
        expect(mockRefreshAccessToken).toHaveBeenCalledTimes(refreshes);
      }
    );

    it('finishes registration when the refresh after it rejects', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      mockRefreshAccessToken.mockRejectedValueOnce(new Error('ipc unavailable'));
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();

      await screen.findByText('Security Key Registered!');
      await waitFor(() =>
        expect(warn).toHaveBeenCalledWith('[mfa] Refresh after enrollment failed')
      );
      warn.mockRestore();
    });
  });

  // ── A failed key ceremony returns to the credentials step ───────────────
  //
  // Begin spent the code or token that gated it, so a retry has to start at the
  // credentials step, with its banner. The stage remounts there: its read runs
  // again and the password and code it held are gone.

  describe('a failed key ceremony returns to the password step', () => {
    it('on a generic credentials.create error', async () => {
      stubCreate(() => Promise.reject(new Error('Something went wrong')));
      renderKey();
      await beginKey();

      expect(await screen.findByText('Something went wrong')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toBeInTheDocument();
      expect(button('Register Key')).toBeInTheDocument();
      expect(callsTo(PATHS.keyFinish)).toHaveLength(0);
    });

    it('on NotAllowedError (the user cancelled)', async () => {
      stubCreate(() => Promise.reject(new DOMException('User cancelled', 'NotAllowedError')));
      renderKey();
      await beginKey();

      expect(
        await screen.findByText('Registration cancelled or timed out. Try again.')
      ).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toBeInTheDocument();
    });

    it('on a rejected finish request', async () => {
      serve({ keyFinish: () => jsonResponse(400, { error: 'Registration failed' }) });
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();

      expect(await screen.findByText('Registration failed')).toBeInTheDocument();
      expect(screen.getByLabelText('Password')).toBeInTheDocument();
      expect(screen.queryByText('Security Key Registered!')).not.toBeInTheDocument();
    });

    it('with the spent code gone and Register Key back down', async () => {
      serve({ offers: ['totp'] });
      stubCreate(() => Promise.reject(new DOMException('User cancelled', 'NotAllowedError')));
      renderKey({ mfaActive: true });
      await beginKey(true);

      await screen.findByText('Registration cancelled or timed out. Try again.');
      expect(valueOf(CODE_LABEL)).toBe('');
      expect(button('Register Key')).toHaveAttribute('aria-disabled', 'true');
      expect(callsTo(PATHS.keyBegin)).toHaveLength(1);
    });

    it('when Cancel is pressed while the key dialog is open and the dialog then times out', async () => {
      serve({ offers: ['totp'] });
      // The OS dialog stays open until the test closes it.
      let closeDialog: (err: unknown) => void = () => {};
      stubCreate(
        () =>
          new Promise((_, reject) => {
            closeDialog = reject;
          })
      );
      renderKey({ mfaActive: true });
      await beginKey(true);
      await screen.findByText('Waiting for your security key...');

      click('Cancel');
      await act(async () => {
        closeDialog(new DOMException('Timed out', 'NotAllowedError'));
      });

      await waitFor(() => expect(button('Register Key')).toHaveAttribute('aria-disabled', 'true'));
      expect(await screen.findByLabelText(CODE_LABEL)).toHaveValue('');
      // Only the begin request was sent; nothing re-sent the spent code.
      expect(callsTo(PATHS.keyBegin)).toHaveLength(1);
      expect(callsTo(PATHS.keyFinish)).toHaveLength(0);
      expect(onCancel).not.toHaveBeenCalled();
    });

    it('a failed begin clears the code and puts Register Key back down', async () => {
      serve({
        offers: ['totp'],
        keyBegin: () => jsonResponse(500, { error: 'Internal server error' }),
      });
      renderKey({ mfaActive: true });
      await beginKey(true);

      expect(await screen.findByText('Internal server error')).toBeInTheDocument();
      expect(valueOf(CODE_LABEL)).toBe('');
      expect(button('Register Key')).toHaveAttribute('aria-disabled', 'true');
    });
  });

  // ── The first store is bound to a capture (EE3) ──────────────────────────
  //
  // The double lacks apiFetch's pre-dispatch fence, so `fenceStaleCaptures`
  // adds it: a request admitted against a capture that is no longer current is
  // refused before it reaches the route, as apiFetch refuses it.

  describe('first recovery-key store under a capture (EE3)', () => {
    function fenceStaleCaptures() {
      const dispatch = mockApiFetch.getMockImplementation();
      if (dispatch === undefined) throw new Error('serve() first');
      mockApiFetch.mockImplementation(
        (path: string, init?: RequestInit, opts?: { context?: ApiRequestContext }) =>
          opts?.context !== undefined && !apiRequestContextIsCurrent(opts.context)
            ? Promise.reject(
                new DOMException('Request lifecycle changed before dispatch', 'AbortError')
              )
            : dispatch(path, init, opts)
      );
    }

    /** Confirms the codes with the key wrapping held; `switchAccount` lands while it is held. */
    async function storeAcrossPreparation(switchAccount: boolean) {
      const stored = vi.fn(() => jsonResponse(200, {}));
      serve({ recoveryKey: stored });
      fenceStaleCaptures();
      const wrapping = deferred<{ wrappedKey: string; salt: string }>();
      vi.mocked(wrapWithRecoveryKey).mockImplementationOnce(() => wrapping.promise);

      await toRecoveryStep();
      // The keys have been read: the capture was taken before that.
      await waitFor(() => expect(wrapWithRecoveryKey).toHaveBeenCalled());
      if (switchAccount) {
        act(() => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 })));
      }
      await act(async () => wrapping.resolve({ wrappedKey: 'wrapped', salt: 'salt' }));
      return stored;
    }

    // Positive control: the same held preparation, with no switch, is stored.
    it('stores the prepared key under the capture when nothing changed', async () => {
      const stored = await storeAcrossPreparation(false);

      await screen.findByTestId('recovery-key-display');
      expect(stored).toHaveBeenCalledTimes(1);
      const [context] = contextsOf(PATHS.recoveryKey);
      expect(apiRequestContextIsCurrent(context as ApiRequestContext)).toBe(true);
    });

    // Mutant: the capture taken after the preparation, or none at all. The key
    // wrapped for the old account would then go out as the new one.
    it('sends nothing when the account changed while the key was being prepared', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      const stored = await storeAcrossPreparation(true);

      expect(await recoveryText(FAILED_COPY)).toBeInTheDocument();
      expect(stored).not.toHaveBeenCalled();
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
      warn.mockRestore();
    });
  });

  // ── A refused answer that is not JSON (CR4) ───────────────────────────────
  //
  // A proxy or gateway answers with an HTML page. Its parse error is not the
  // wizard's to show: each step falls back to its own sentence.

  describe('a refused answer whose body is not JSON (CR4)', () => {
    const htmlPage = () =>
      new Response('<html><body>Bad gateway</body></html>', {
        status: 502,
        headers: { 'Content-Type': 'text/html' },
      });
    const parseError = () => screen.queryByText(/Unexpected token|not valid JSON/);

    it('verify-setup shows its own failure', async () => {
      serve({ verifySetup: htmlPage });
      await toQr();
      fireEvent.click(screen.getByTestId('totp-submit'));

      expect(await screen.findByTestId('totp-error')).toHaveTextContent('Verification failed');
      expect(parseError()).not.toBeInTheDocument();
      expect(screen.queryByTestId('backup-confirm')).not.toBeInTheDocument();
    });

    it('confirm-setup shows its own failure', async () => {
      serve({ confirmSetup: htmlPage });
      await toBackupCodes();
      fireEvent.click(screen.getByTestId('backup-confirm'));

      expect(await screen.findByText('Confirmation failed')).toBeInTheDocument();
      expect(parseError()).not.toBeInTheDocument();
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(0);
    });

    it('register finish shows its own failure', async () => {
      serve({ keyFinish: htmlPage });
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();

      expect(await screen.findByText('Registration failed')).toBeInTheDocument();
      expect(parseError()).not.toBeInTheDocument();
      expect(callsTo(PATHS.keyFinish)).toHaveLength(1);
    });
  });

  // ── Cancel ends the key ceremony (CR6) ────────────────────────────────────

  describe('Cancel during the key ceremony (CR6)', () => {
    /** Begins registration with the browser's prompt held open until `touch` settles. */
    async function toHeldCeremony() {
      const touch = deferred<unknown>();
      stubCreate(() => touch.promise);
      const view = renderKey();
      await beginKey();
      await screen.findByText('Waiting for your security key...');
      return { touch, view };
    }
    const ceremonySignal = () =>
      (vi.mocked(navigator.credentials.create).mock.calls[0][0] as CredentialCreationOptions)
        .signal;
    /** Two macrotask turns: enough for the credential to reach the finish request. */
    const settle = async () => {
      for (let i = 0; i < 2; i++) {
        await act(async () => {
          await new Promise((resolve) => setTimeout(resolve, 0));
        });
      }
    };

    // Positive control: released without Cancel, the same held ceremony
    // finishes within the same settling the cases below allow.
    it('a ceremony released without Cancel sends the finish and lands on done', async () => {
      const { touch } = await toHeldCeremony();
      expect(ceremonySignal()?.aborted).toBe(false);

      touch.resolve(mockCredential());
      await settle();

      expect(callsTo(PATHS.keyFinish)).toHaveLength(1);
      expect(screen.getByText('Security Key Registered!')).toBeInTheDocument();
    });

    // Mutant: Cancel only resets the step. The prompt stays open, and a touch
    // after it registers the key and jumps the wizard to done.
    it('a key touched after Cancel sends no finish and leaves the wizard on the credentials step', async () => {
      const { touch } = await toHeldCeremony();
      click('Cancel');
      // The browser's prompt is told to close.
      expect(ceremonySignal()?.aborted).toBe(true);

      touch.resolve(mockCredential());
      await settle();

      expect(callsTo(PATHS.keyFinish)).toHaveLength(0);
      expect(screen.queryByText('Security Key Registered!')).not.toBeInTheDocument();
      expect(button('Register Key')).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('a key touched after the wizard closed sends no finish', async () => {
      const { touch, view } = await toHeldCeremony();
      view.unmount();

      touch.resolve(mockCredential());
      await settle();

      expect(callsTo(PATHS.keyFinish)).toHaveLength(0);
    });

    // The credential lands first, so the abort comes too late to end the wait:
    // only the dropped ceremony stops the finish. Mutant: unmount aborts but
    // does not drop it.
    it('a key touched in the same turn the wizard closes sends no finish', async () => {
      const { touch, view } = await toHeldCeremony();
      touch.resolve(mockCredential());
      view.unmount();
      await settle();

      expect(callsTo(PATHS.keyFinish)).toHaveLength(0);
    });

    // Mutant: the 60 s registration timeout never cleared. Every ceremony that
    // ended early would leave it running for a minute. Spies, not fake timers:
    // once the wait has ended the stray timer changes nothing anyone can see,
    // and a timer count would also count React's, RTL's and the hook's own.
    it.each([
      ['answered', 'answer'],
      ['cancelled', 'cancel'],
      ['closed with the wizard', 'unmount'],
    ] as const)('clears the registration timeout once the ceremony is %s', async (_name, end) => {
      const setSpy = vi.spyOn(globalThis, 'setTimeout');
      const clearSpy = vi.spyOn(globalThis, 'clearTimeout');
      try {
        const { touch, view } = await toHeldCeremony();
        const index = setSpy.mock.calls.findIndex(([, ms]) => ms === 60000);
        expect(index).toBeGreaterThanOrEqual(0);
        const timer = setSpy.mock.results[index].value;
        // Positive control: the wait is still on, so nothing has cleared it.
        expect(clearSpy).not.toHaveBeenCalledWith(timer);

        if (end === 'answer') touch.resolve(mockCredential());
        else if (end === 'cancel') click('Cancel');
        else view.unmount();
        await settle();

        expect(clearSpy).toHaveBeenCalledWith(timer);
      } finally {
        setSpy.mockRestore();
        clearSpy.mockRestore();
      }
    });

    // Mutant: the timeout only rejecting the race. The browser's prompt would
    // stay open behind a wizard that has given up, and a later touch could
    // mint a credential nothing finishes.
    it('aborts the browser ceremony when the registration times out', async () => {
      const setSpy = vi.spyOn(globalThis, 'setTimeout');
      try {
        await toHeldCeremony();
        const index = setSpy.mock.calls.findIndex(([, ms]) => ms === 60000);
        expect(index).toBeGreaterThanOrEqual(0);
        // Positive control: the ceremony is live until the timeout fires.
        expect(ceremonySignal()?.aborted).toBe(false);

        const fire = setSpy.mock.calls[index][0] as () => void;
        await act(async () => fire());

        expect(ceremonySignal()?.aborted).toBe(true);
        expect(callsTo(PATHS.keyFinish)).toHaveLength(0);
      } finally {
        setSpy.mockRestore();
      }
    });

    // The key was touched before Cancel, so the account holds it: done says so.
    it('a finish already under way when Cancel lands still reports the registered key', async () => {
      const finish = deferred<Response>();
      serve({ keyFinish: () => finish.promise });
      const { touch } = await toHeldCeremony();
      touch.resolve(mockCredential());
      await waitFor(() => expect(callsTo(PATHS.keyFinish)).toHaveLength(1));

      click('Cancel');
      await act(async () => finish.resolve(jsonResponse(200, {})));

      expect(await screen.findByText('Security Key Registered!')).toBeInTheDocument();
    });
  });

  // ── Enrolment required (TB1) ──────────────────────────────────────────────
  //
  // A 403 with `mfa_enrollment_required` is the hook's terminal state: the
  // stage says so, keeps the primary down, and drops the password field. No
  // banner says it a second time.

  describe('an account that must enrol first (TB1)', () => {
    const ENROLLMENT_TEXT = 'Set up an authenticator app or security key in Settings to do this.';
    const enrollmentRequired = () =>
      jsonResponse(403, { error: 'Set up MFA first', mfa_enrollment_required: true });

    /** The terminal state as every surface below shows it. */
    function expectEnrolmentTerminal(primary: string, heading: string) {
      expect(button(primary)).toHaveAttribute('aria-disabled', 'true');
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.getByRole('heading', { name: heading })).toHaveFocus();
    }

    // Mutant: `enrollmentRequired` answered as a banner or as `failed`.
    it('#5 TOTP setup: the sentence, Continue down, and the wizard stays on credentials', async () => {
      serve({ totpSetup: enrollmentRequired });
      renderTotp();
      await beginTotp();

      expect(await screen.findByText(ENROLLMENT_TEXT)).toBeInTheDocument();
      expectEnrolmentTerminal('Continue', 'Set Up Authenticator App');
      expect(screen.queryByTestId('totp-submit')).not.toBeInTheDocument();
      // Positive control: the one request went out, so the sentence is its answer.
      expect(callsTo(PATHS.totpSetup)).toHaveLength(1);
      click('Continue');
      expect(callsTo(PATHS.totpSetup)).toHaveLength(1);
    });

    it('#5 security-key begin: the sentence, Register Key down, and no ceremony', async () => {
      serve({ keyBegin: enrollmentRequired });
      stubCreate(() => Promise.resolve(mockCredential()));
      renderKey();
      await beginKey();

      expect(await screen.findByText(ENROLLMENT_TEXT)).toBeInTheDocument();
      expectEnrolmentTerminal('Register Key', 'Set Up Security Key');
      expect(navigator.credentials.create).not.toHaveBeenCalled();
      expect(callsTo(PATHS.keyBegin)).toHaveLength(1);
    });

    it('#6 recovery-key replace: the sentence, Replace down, and no new key shown', async () => {
      const api = serve({ recoveryKey: keptThen(enrollmentRequired) });
      await toReplaceStep(api);
      typePassword('freshpw');
      await untilActionable(REPLACE_LABEL);
      click(REPLACE_LABEL);

      expect(await screen.findByText(ENROLLMENT_TEXT)).toBeInTheDocument();
      expectEnrolmentTerminal(REPLACE_LABEL, 'Set Up Authenticator App');
      expect(screen.queryByTestId('recovery-key-display')).not.toBeInTheDocument();
      // The first store and the one refused replace; the inert primary adds none.
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(2);
      click(REPLACE_LABEL);
      expect(callsTo(PATHS.recoveryKey)).toHaveLength(2);
    });
  });
});
