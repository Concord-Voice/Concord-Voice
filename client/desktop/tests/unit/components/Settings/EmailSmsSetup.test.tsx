import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import {
  bodiesTo,
  installStepUpApi,
  jsonResponse,
  readCount,
  readOffers,
} from '../../../helpers/stepUpApi';

// Email MFA enrolment (plan 2026-10-07 §3). The credential step is the shared
// factor picker; the emailed code that follows is this wizard's own. The offered
// set never contains email or SMS (G1), and the request names email alone (D14).
//
// `apiFetch` answers by path, so the hook's read and the wizard's own requests
// never compete for a queued response.
//
// "Mutant:" comments name the production change each case exists to turn red.

const mockApiFetch = vi.fn();
const mockRefreshAccessToken = vi.fn(() => Promise.resolve<string | null>(null));
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  refreshAccessToken: () => mockRefreshAccessToken(),
}));

import EmailSmsSetup from '@/renderer/components/Settings/EmailSmsSetup';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Named fixtures: the pre-commit detect-secrets hook flags a credential-shaped
// key beside a quoted literal regardless of the value.
const FIXTURE_PW = 'fixture-password-do-not-persist';
const FIXTURE_OTP = '314159';
const FIXTURE_EMAIL_CODE = '111111';

const SETUP = '/api/v1/mfa/email-sms/setup';
const VERIFY = '/api/v1/mfa/email-sms/verify';
const CODE_LABEL = 'Authenticator app code';
const NO_USABLE =
  "Your account's verification method can't be used here. Add an authenticator app or security key in Settings.";

const onComplete = vi.fn();
const onCancel = vi.fn();

const renderWizard = () => render(<EmailSmsSetup onComplete={onComplete} onCancel={onCancel} />);

const sendButton = () => screen.getByRole('button', { name: 'Send Code' });
const passwordField = () => screen.findByLabelText('Password') as Promise<HTMLInputElement>;
const codeField = () => screen.findByLabelText(CODE_LABEL) as Promise<HTMLInputElement>;
const setupRequests = () => mockApiFetch.mock.calls.filter((c) => c[0] === SETUP);
const waitForSetup = (n = 1) => waitFor(() => expect(setupRequests()).toHaveLength(n));
const setupBodies = () => bodiesTo(mockApiFetch, SETUP);
const verifyBodies = () => bodiesTo(mockApiFetch, VERIFY);

const SENT = { message: 'Verification codes sent', methods: ['email'], expires_in: '10 minutes' };

/** An account with no inline method: the password alone starts setup. */
function installPasswordOnly(route?: () => Response | Promise<Response>) {
  installStepUpApi(mockApiFetch, {
    read: () => readOffers([]),
    route: (path) => {
      if (path === SETUP && route) return route();
      return jsonResponse(200, path === SETUP ? SENT : {});
    },
  });
}

async function sendWithPassword() {
  await userEvent.type(await passwordField(), FIXTURE_PW);
  await userEvent.click(sendButton());
}

/** Gets to the emailed-code step on a password-only account. */
async function reachVerifyStep() {
  installPasswordOnly();
  renderWizard();
  await sendWithPassword();
  await screen.findByLabelText('Email code');
}

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset();
  mockRefreshAccessToken.mockClear();
  onComplete.mockReset();
  onCancel.mockReset();
  useUserStore.setState({ user: { id: 'acct-1' } as never });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('the credential step', () => {
  it('renders the email-only wizard with an inert primary and no SMS or dev copy', async () => {
    installPasswordOnly();
    renderWizard();

    expect(screen.getByText('Set Up Email MFA')).toBeInTheDocument();
    expect(await passwordField()).toBeInTheDocument();
    expect(sendButton()).toHaveAttribute('aria-disabled', 'true');
    // Never natively disabled: it must stay focusable so a click can name what is missing.
    expect(sendButton()).not.toBeDisabled();
    expect(screen.queryByText(/DEV MODE/)).not.toBeInTheDocument();
    expect(screen.queryByText(/sms/i)).not.toBeInTheDocument();
  });

  it('enables the primary once the password is typed, and Cancel calls onCancel', async () => {
    installPasswordOnly();
    renderWizard();

    await userEvent.type(await passwordField(), FIXTURE_PW);
    expect(sendButton()).not.toHaveAttribute('aria-disabled');

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('a click on the inert primary names the missing password and sends nothing', async () => {
    installPasswordOnly();
    renderWizard();

    await passwordField();
    await userEvent.click(sendButton());

    expect(await screen.findByText('Enter your password to continue.')).toBeInTheDocument();
    expect(setupRequests()).toHaveLength(0);
  });

  it('shows the in-flight label and locks Cancel while the request is out', async () => {
    const gate = deferred<Response>();
    installPasswordOnly(() => gate.promise);
    renderWizard();

    await sendWithPassword();

    const busy = await screen.findByRole('button', { name: 'Sending...' });
    expect(busy).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();

    gate.resolve(jsonResponse(200, SENT));
    expect(await screen.findByLabelText('Email code')).toBeInTheDocument();
  });
});

describe('what the setup request sends', () => {
  // Mutant: adding 'sms' to `methods`, or dropping the field.
  it('names email alone beside the password, with no code on a password-only account', async () => {
    installPasswordOnly();
    renderWizard();

    await sendWithPassword();

    await waitForSetup();
    expect(setupBodies()).toEqual([{ methods: ['email'], password: FIXTURE_PW }]);
    expect(setupRequests()[0][1]).toMatchObject({ method: 'POST' });
  });

  // Mutant: the code sent under another key, or a stale/empty one sent beside the password.
  it('an authenticator account sends its code as mfa_code beside the password', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(200, SENT),
    });
    renderWizard();

    await userEvent.type(await passwordField(), FIXTURE_PW);
    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(sendButton());

    await waitForSetup();
    expect(setupBodies()).toEqual([
      { methods: ['email'], password: FIXTURE_PW, mfa_code: FIXTURE_OTP },
    ]);
    expect(await screen.findByLabelText('Email code')).toBeInTheDocument();
  });

  it('keeps the password leg when the read fails, so setup is still possible', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => jsonResponse(503),
      route: () => jsonResponse(200, SENT),
    });
    renderWizard();

    await sendWithPassword();

    await waitForSetup();
    expect(setupBodies()).toEqual([{ methods: ['email'], password: FIXTURE_PW }]);
  });
});

describe('email and SMS are never offered as a way to prove it is you (G1)', () => {
  // Mutant: not filtering the read's list to the inline set.
  it('a read offering totp, email and sms shows the authenticator code and nothing else', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp', 'email', 'sms']),
      route: () => jsonResponse(200, SENT),
    });
    renderWizard();

    expect(await codeField()).toBeInTheDocument();
    expect(screen.queryByLabelText(/email|sms/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /email|sms/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/sms/i)).not.toBeInTheDocument();
  });

  // Mutant: letting a refusal's list add email or SMS to the offered set.
  it('an mfa_required listing totp, email and sms asks for the code and offers no email or SMS', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers([]),
      route: () => jsonResponse(403, { mfa_required: true, methods: ['totp', 'email', 'sms'] }),
    });
    renderWizard();

    await sendWithPassword();

    await waitForSetup();
    expect(await codeField()).toBeInTheDocument();
    expect(screen.queryByLabelText(/email|sms/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /email|sms/i })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Email code')).not.toBeInTheDocument();
  });

  // Mutant: an email-or-SMS-only list rendered as a panel the account can never fill.
  it('an mfa_required listing only email and sms is the no-usable-method state, with no field', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers([]),
      route: () => jsonResponse(403, { mfa_required: true, methods: ['email', 'sms'] }),
    });
    renderWizard();

    await sendWithPassword();

    expect(await screen.findByText(NO_USABLE)).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/email|sms/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /email|sms/i })).not.toBeInTheDocument();
    expect(onComplete).not.toHaveBeenCalled();
  });

  // §1.3: the read is authoritative; an empty set is a password-only account.
  // Mutant: an unconditional code box (the leg derived from the account's mfa_methods).
  it('a read with methods: [] shows no code field', async () => {
    installPasswordOnly();
    renderWizard();

    expect(await passwordField()).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(readCount(mockApiFetch)).toBe(1);
  });
});

describe('what the setup route answers', () => {
  it('a refused password shows on its field and empties it', async () => {
    installPasswordOnly(() => jsonResponse(403, { error: 'Invalid password' }));
    renderWizard();

    const field = await passwordField();
    await userEvent.type(field, FIXTURE_PW);
    await userEvent.click(sendButton());

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    expect(field).toHaveValue('');
    expect(screen.queryByLabelText('Email code')).not.toBeInTheDocument();
  });

  it('a refused code on an authenticator account keeps the password and clears the code', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers(['totp']),
      route: () => jsonResponse(403, { error: 'Invalid MFA code' }),
    });
    renderWizard();

    const pw = await passwordField();
    await userEvent.type(pw, FIXTURE_PW);
    await userEvent.type(await codeField(), FIXTURE_OTP);
    await userEvent.click(sendButton());

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
    expect(pw).toHaveValue(FIXTURE_PW);
    expect(await codeField()).toHaveValue('');
  });

  // Mutant: `enrollmentRequired` advancing the wizard, answered as a banner, or
  // leaving a password field the account can never use.
  it('an account with no inline factor is told to set one up, with the primary inert and no password field', async () => {
    installPasswordOnly(() => jsonResponse(403, { mfa_enrollment_required: true }));
    renderWizard();

    await sendWithPassword();
    await waitForSetup();

    expect(
      await screen.findByText('Set up an authenticator app or security key in Settings to do this.')
    ).toBeInTheDocument();
    // The terminal state ends the attempt: nothing to fill and nothing to press.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(sendButton()).toHaveAttribute('aria-disabled', 'true');
    // Still the credential step, and no banner beside the sentence.
    expect(screen.queryByLabelText('Email code')).not.toBeInTheDocument();
    expect(screen.getByText(/Confirm it.s you to send a verification code/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    // Positive control: the one request went out, so the sentence is its answer.
    expect(setupRequests()).toHaveLength(1);
  });

  it('shows a server error in the banner and stays on the credential step', async () => {
    installPasswordOnly(() => jsonResponse(500, { error: 'Failed to send codes' }));
    renderWizard();

    await sendWithPassword();

    expect(await screen.findByText('Failed to send codes')).toBeInTheDocument();
    expect(screen.queryByLabelText('Email code')).not.toBeInTheDocument();
  });

  // The server accepts each code once and can accept one yet still fail the
  // request, so a code that was sent is never offered again.
  describe('a sent MFA code is never offered again', () => {
    const totpAccount = (route: () => Response) =>
      installStepUpApi(mockApiFetch, { read: () => readOffers(['totp']), route });

    async function fillAndSend() {
      await userEvent.type(await passwordField(), FIXTURE_PW);
      await userEvent.type(await codeField(), FIXTURE_OTP);
      expect(sendButton()).not.toHaveAttribute('aria-disabled');
      await userEvent.click(sendButton());
    }

    it('a 500 clears the code and leaves Send Code inert', async () => {
      totpAccount(() => jsonResponse(500, { error: 'Internal server error' }));
      renderWizard();

      await fillAndSend();

      expect(await screen.findByText('Internal server error')).toBeInTheDocument();
      expect(await codeField()).toHaveValue('');
      await waitFor(() => expect(sendButton()).toHaveAttribute('aria-disabled', 'true'));
    });

    it('Back from the verify step asks for a fresh code', async () => {
      totpAccount(() => jsonResponse(200, SENT));
      renderWizard();

      await fillAndSend();
      await screen.findByLabelText('Email code');
      await userEvent.click(screen.getByRole('button', { name: 'Back' }));

      expect(await codeField()).toHaveValue('');
      expect(sendButton()).toHaveAttribute('aria-disabled', 'true');
    });
  });
});

describe('the emailed-code step', () => {
  // The server accepted the password with the send, so it is spent (FE4/RT3).
  // Mutant: keeping the accepted password in state, so Back returns to the field filled.
  it('Back from the verify step returns to a password field that is empty', async () => {
    await reachVerifyStep();
    await userEvent.click(screen.getByRole('button', { name: 'Back' }));

    const field = await passwordField();
    expect(field).toHaveValue('');
    // Nothing to send, so the primary is inert again; and the wizard did go back.
    expect(sendButton()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.queryByLabelText('Email code')).not.toBeInTheDocument();
    expect(setupRequests()).toHaveLength(1);
  });

  it('asks for the email code only, with Verify & Activate natively disabled until it is typed', async () => {
    await reachVerifyStep();

    expect(screen.getByPlaceholderText('6-digit email code')).toBeInTheDocument();
    expect(screen.queryByPlaceholderText('6-digit SMS code')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Verify & Activate' })).toBeDisabled();
  });

  it('sends the verify request with the email code only', async () => {
    await reachVerifyStep();

    await userEvent.type(screen.getByLabelText('Email code'), FIXTURE_EMAIL_CODE);
    await userEvent.click(screen.getByRole('button', { name: 'Verify & Activate' }));

    await waitFor(() => expect(verifyBodies()).toHaveLength(1));
    expect(verifyBodies()[0]).toEqual({ codes: { email: FIXTURE_EMAIL_CODE } });
  });

  it('shows the verifying label while the request is out', async () => {
    const gate = deferred<Response>();
    installStepUpApi(mockApiFetch, {
      read: () => readOffers([]),
      route: (path) => (path === VERIFY ? gate.promise : jsonResponse(200, SENT)),
    });
    renderWizard();
    await sendWithPassword();
    await userEvent.type(await screen.findByLabelText('Email code'), '000000');
    await userEvent.click(screen.getByRole('button', { name: 'Verify & Activate' }));

    expect(await screen.findByText('Verifying...')).toBeInTheDocument();

    gate.resolve(jsonResponse(200));
    expect(await screen.findByText('Email MFA Activated!')).toBeInTheDocument();
  });

  it('shows a refused code and does not refresh the token', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers([]),
      route: (path) =>
        path === VERIFY ? jsonResponse(400, { error: 'Invalid code' }) : jsonResponse(200, SENT),
    });
    renderWizard();
    await sendWithPassword();
    await userEvent.type(await screen.findByLabelText('Email code'), '000000');
    await userEvent.click(screen.getByRole('button', { name: 'Verify & Activate' }));

    expect(await screen.findByText('Invalid code')).toBeInTheDocument();
    expect(mockRefreshAccessToken).not.toHaveBeenCalled();
    expect(screen.queryByText('Email MFA Activated!')).not.toBeInTheDocument();
  });

  // Mutant: an unguarded `res.json()`. A gateway's HTML page would surface its
  // parse error ("Unexpected token '<'") as the wizard's error.
  it('shows its own failure when a refused answer is not JSON', async () => {
    installStepUpApi(mockApiFetch, {
      read: () => readOffers([]),
      route: (path) =>
        path === VERIFY
          ? new Response('<html><body>Bad gateway</body></html>', {
              status: 502,
              headers: { 'Content-Type': 'text/html' },
            })
          : jsonResponse(200, SENT),
    });
    renderWizard();
    await sendWithPassword();
    await userEvent.type(await screen.findByLabelText('Email code'), '000000');
    await userEvent.click(screen.getByRole('button', { name: 'Verify & Activate' }));

    expect(await screen.findByText('Verification failed')).toBeInTheDocument();
    expect(screen.queryByText(/Unexpected token|not valid JSON/)).not.toBeInTheDocument();
    expect(screen.queryByText('Email MFA Activated!')).not.toBeInTheDocument();
    expect(mockRefreshAccessToken).not.toHaveBeenCalled();
  });

  it('shows the done step, refreshes at once, and calls onComplete', async () => {
    await reachVerifyStep();

    await userEvent.type(screen.getByLabelText('Email code'), FIXTURE_EMAIL_CODE);
    await userEvent.click(screen.getByRole('button', { name: 'Verify & Activate' }));

    expect(await screen.findByText('Email MFA Activated!')).toBeInTheDocument();
    // Refreshes at once so a first activation's exemption is used within its TTL.
    expect(mockRefreshAccessToken).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole('button', { name: 'Done' }));
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  // The refresh is not awaited, so a rejection must be caught where it is made;
  // activation still finishes (frontend review, PR #3437).
  it('finishes activation when the refresh after it rejects', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    mockRefreshAccessToken.mockRejectedValueOnce(new Error('ipc unavailable'));
    await reachVerifyStep();

    await userEvent.type(screen.getByLabelText('Email code'), FIXTURE_EMAIL_CODE);
    await userEvent.click(screen.getByRole('button', { name: 'Verify & Activate' }));

    expect(await screen.findByText('Email MFA Activated!')).toBeInTheDocument();
    await waitFor(() => expect(warn).toHaveBeenCalledWith('[mfa] Refresh after enrollment failed'));
  });
});

describe('an answer for an account that is no longer current', () => {
  // Mutant: advancing to the verify step without the currentness check.
  it('does not advance to the verify step', async () => {
    const gate = deferred<Response>();
    installPasswordOnly(() => gate.promise);
    renderWizard();

    await sendWithPassword();
    await waitForSetup();
    useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
    gate.resolve(jsonResponse(200, SENT));

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Sending...' })).not.toBeInTheDocument()
    );
    expect(screen.queryByLabelText('Email code')).not.toBeInTheDocument();
  });
});
