import { render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PrivacySecuritySection from '@/renderer/components/Settings/PrivacySecuritySection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';

vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  default: () => null,
  WebAuthnCredential: {},
}));

// The turn-off prompt for `require_auth_before_purge` (#2765) on the shared
// step-up stage (plan 2026-10-07 §2, T13). Every case serves
// `GET /api/v1/mfa/step-up` explicitly through `serveSection`: the read decides
// which fields exist, so a case that left it unserved would be testing the
// blocked/unsupported fallback by accident.
//
// "Mutant:" comments name the production change each case exists to turn red.

// Named fixtures, used only by reference: the pre-commit detect-secrets hook
// flags a credential-shaped key beside a quoted literal regardless of the value.
// Both are long and distinctive because they are the NEEDLES in the storage
// sweep below — a short value ('pw') could collide with unrelated text and turn
// a real signal into noise. Mirrors tests/unit/components/Purge/StepUp.test.tsx.
const FIXTURE_PW = 'fixture-password-do-not-persist';
const FIXTURE_OTP = '314159';
const WEBAUTHN_TOKEN = 'fixture-inline-webauthn-token';

const API_BASE = 'http://localhost:8080';
const PRIVACY_ENDPOINT = `${API_BASE}/api/v1/users/me/privacy`;
const HISTORY_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-history`;
const READ_ENDPOINT = `${API_BASE}/api/v1/mfa/step-up`;
const BEGIN_ENDPOINT = `${API_BASE}/api/v1/mfa/webauthn/verify-inline/begin`;
const FINISH_ENDPOINT = `${API_BASE}/api/v1/mfa/webauthn/verify-inline/finish`;
const USER_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';

const TOGGLE_LABEL = 'Require authentication before purging';
const DIALOG_TITLE = 'Confirm it is you';
const SUBMIT_LABEL = 'Turn Off';
const CODE_LABEL = 'Authenticator app code';
const KEY_LABEL = 'Passkey or security key';

const MISSING_PASSWORD = 'Enter your password to continue.'; // pragma: allowlist secret
const MISSING_CODE = 'Enter the 6-digit code from your authenticator app to continue.';
const WRONG_CODE = "That code didn't work.";
const NO_USABLE_METHOD =
  "Your account's verification method can't be used here. Add an authenticator app or security key in Settings.";

const basePrivacy = {
  messages_friends_only: true,
  messages_server_members: true,
  dm_privacy_level: 2,
  dm_friends_of_friends: false,
  auto_accept_friend_codes: false,
  searchable_by_username: false,
  searchable_by_email: false,
  searchable_by_phone: false,
  allow_embedded_content: false,
  load_gifs_automatically: true,
  share_personalization_with_gif_provider: true,
  require_auth_before_purge: true,
};

/** What `GET /api/v1/mfa/step-up` answers: a scripted response, built per request. */
type ReadScript = () => Response;

/** The account can use these inline methods; `defaultMethod` is the server's pick. */
function readOffers(methods: string[], defaultMethod: string | null = methods[0] ?? null) {
  return (): Response =>
    HttpResponse.json({
      methods,
      default_method: defaultMethod,
      backup_code_available: false,
    });
}

/** The read fails with `status` and no body, as a 404 from an old server or a 5xx. */
function readStatus(status: number, body?: unknown): ReadScript {
  return () =>
    body === undefined
      ? new HttpResponse(null, { status })
      : HttpResponse.json(body as Record<string, unknown>, { status });
}

/** How many times the dialog read the requirements, so "no read" is assertable. */
let readHits = 0;

/**
 * Everything PrivacySecuritySection fetches on mount besides the privacy blob,
 * plus the step-up read. `fence` seeds the toggle's starting position: the
 * OFF-flow cases need it up, the ON case needs it down. `read` is required so
 * a case cannot forget to say what the account can use.
 */
function serveSection(fence: boolean, read: ReadScript): void {
  server.use(
    http.get(READ_ENDPOINT, () => {
      readHits += 1;
      return read();
    }),
    http.get(`${API_BASE}/api/v1/sessions`, () =>
      HttpResponse.json({ sessions: [], past_sessions: [], revocation_mode: 'secure' })
    ),
    http.get(`${API_BASE}/api/v1/mfa/status`, () =>
      HttpResponse.json({
        methods: [],
        recovery_only_methods: [],
        recovery_hardened: false,
        backup_codes_remaining: 0,
        backup_email: '',
      })
    ),
    http.get(`${API_BASE}/api/v1/mfa/webauthn/credentials`, () =>
      HttpResponse.json({ credentials: [] })
    ),
    http.get(`${API_BASE}/api/v1/users/me/security`, () =>
      HttpResponse.json({ password_login_disabled: false, trust_sso_security: false })
    ),
    http.get(`${API_BASE}/api/v1/users/me/sso-identities`, () =>
      HttpResponse.json({ identities: [] })
    ),
    http.get(`${API_BASE}/api/v1/users/me/presence-settings`, () =>
      HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
    ),
    http.get(`${HISTORY_ENDPOINT}/settings`, () => new HttpResponse(null, { status: 404 })),
    http.get(HISTORY_ENDPOINT, () => new HttpResponse(null, { status: 404 })),
    http.get(PRIVACY_ENDPOINT, () =>
      HttpResponse.json({ privacy: { ...basePrivacy, require_auth_before_purge: fence } })
    )
  );
}

type Body = Record<string, unknown>;

/** Every PATCH body the privacy endpoint received, in order. */
function capturePatches(bodies: unknown[], respond: () => Response): void {
  server.use(
    http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
      bodies.push(await request.json());
      return respond();
    })
  );
}

const refuse =
  (body: Record<string, unknown>, status = 403) =>
  () =>
    HttpResponse.json(body, { status });
const accepted = () =>
  HttpResponse.json({ privacy: { ...basePrivacy, require_auth_before_purge: false } });

const toggle = () => screen.getByRole('switch', { name: TOGGLE_LABEL });
const primary = () => screen.getByRole('button', { name: SUBMIT_LABEL });
const passwordField = () => screen.getByLabelText('Password') as HTMLInputElement;
const codeField = () => screen.getByLabelText(CODE_LABEL) as HTMLInputElement;
const findCode = () => screen.findByLabelText(CODE_LABEL) as Promise<HTMLInputElement>;

/** Render the section and flip the fence toggle, waiting for the fetch first. */
async function flipFence(): Promise<void> {
  render(<PrivacySecuritySection />);
  await userEvent.click(await screen.findByRole('switch', { name: TOGGLE_LABEL }));
}

/**
 * Open the dialog and wait for the read to land. A password-only account has
 * no field that appears when it does, so readiness is read off the primary:
 * with a password typed, `aria-disabled` is dropped only once the read is
 * `ready` with nothing else to supply (it stays "true" while `reading`).
 */
async function openPasswordOnly(): Promise<void> {
  await flipFence();
  await screen.findByRole('heading', { name: DIALOG_TITLE });
  await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
  await waitFor(() => expect(primary()).not.toHaveAttribute('aria-disabled'));
}

/** Open the dialog for an account that holds a TOTP app, waiting for its field. */
async function openWithCode(): Promise<void> {
  await flipFence();
  await findCode();
}

/** Stand in for a security key: `navigator.credentials.get` answers with an assertion. */
const CREDENTIAL = {
  id: 'credential-id',
  rawId: new Uint8Array([1, 2, 3]).buffer,
  type: 'public-key',
  response: {
    authenticatorData: new Uint8Array([10, 20]).buffer,
    clientDataJSON: new Uint8Array([30, 40]).buffer,
    signature: new Uint8Array([50, 60]).buffer,
    userHandle: null,
  },
};

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
  sessionStorage.clear();
  readHits = 0;
  useAuthStore.setState({ accessToken: 'mock-token', sessionId: 'session-a' });
  useUserStore.setState({
    user: { id: USER_ID, username: 'pilot' },
    isLoading: false,
    error: null,
  });
});

afterEach(() => server.resetHandlers());

describe('PurgeFenceStepUpDialog — the toggle never moves ahead of the server (#2765)', () => {
  it('opens the dialog, spends no request, and leaves the switch on', async () => {
    serveSection(true, readOffers(['totp']));
    const bodies: unknown[] = [];
    capturePatches(bodies, () => HttpResponse.json({ privacy: basePrivacy }));

    await flipFence();

    expect(await screen.findByRole('heading', { name: DIALOG_TITLE })).toBeInTheDocument();
    // The switch is a view of the stored setting, not of the user's click: an
    // optimistic flip would show the fence down while it is still up.
    expect(toggle()).toBeChecked();
    expect(bodies).toEqual([]);
  });

  it('leaves the switch on after the server refuses the step-up', async () => {
    serveSection(true, readOffers([]));
    server.use(http.patch(PRIVACY_ENDPOINT, refuse({ error: 'Invalid password' })));

    await openPasswordOnly();
    await userEvent.click(primary());

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    // Nothing changed server-side, so nothing may change client-side either.
    expect(toggle()).toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — the read decides which fields exist (§1.3)', () => {
  // Mutant: an unconditional code field (the offered set ignored, or an empty
  // set replaced by ['totp']) puts a box on screen an account with no
  // authenticator can never fill.
  it('shows no code field for an account with no usable inline method', async () => {
    serveSection(true, readOffers([]));

    await openPasswordOnly();

    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByRole('group', { name: KEY_LABEL })).not.toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
  });

  it('shows the code field only for the methods the read named', async () => {
    serveSection(true, readOffers(['totp']));

    await openWithCode();

    expect(screen.getByLabelText('Password')).toBeInTheDocument();
    expect(screen.queryByRole('group', { name: KEY_LABEL })).not.toBeInTheDocument();
  });

  it('sends the password alone from a password-only account, with no code on the wire', async () => {
    serveSection(true, readOffers([]));
    const bodies: Body[] = [];
    capturePatches(bodies, accepted);

    await openPasswordOnly();
    await userEvent.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0].current_password).toBe(FIXTURE_PW);
    expect(bodies[0]).not.toHaveProperty('mfa_code');
  });

  // The route is `passwordOnly`: the submit is the check that counts, so a read
  // that cannot answer must never lock the user out of a setting the server
  // would accept.
  // Mutant: `readFailure: 'block'` on this route.
  it.each([
    ['503 (unavailable)', readStatus(503)],
    ['429 (unavailable)', readStatus(429)],
    ['404 (an old server)', readStatus(404)],
  ])('keeps the password leg usable when the read fails with %s', async (_name, script) => {
    serveSection(true, script);
    const bodies: Body[] = [];
    capturePatches(bodies, accepted);

    await openPasswordOnly();

    // Not blocked: no Retry, no "couldn't check" sentence, and the primary acts.
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    expect(document.querySelector('output.step-up__status')).toBeEmptyDOMElement();
    await userEvent.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0].current_password).toBe(FIXTURE_PW);
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
  });

  it('stops on a refused read, naming the reason and sending nothing', async () => {
    serveSection(true, readStatus(403, { error_code: 'account_disabled' }));
    const bodies: unknown[] = [];
    capturePatches(bodies, accepted);

    await flipFence();

    expect(await screen.findByText("Your account can't do this right now.")).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(primary());
    expect(bodies).toEqual([]);
  });

  it('does not read the requirements until the dialog opens', async () => {
    serveSection(true, readOffers(['totp']));

    render(<PrivacySecuritySection />);
    await screen.findByRole('switch', { name: TOGGLE_LABEL });

    expect(readHits).toBe(0);
    await userEvent.click(toggle());
    await findCode();
    expect(readHits).toBe(1);
  });
});

describe('PurgeFenceStepUpDialog — an empty password is never excused by a factor', () => {
  // Every account holds a password hash (`users.password_hash` is NOT NULL, SSO
  // accounts included), so no account proceeds on MFA alone: an empty password
  // is missing whatever else is supplied. This replaces the old "accepts an
  // MFA-only submission, which is how an account with no password proceeds".
  // Mutant: a "factor supplied" shortcut in `firstMissingIn` that lets a typed
  // code excuse the empty password sends this request.
  it('blocks an empty password beside a typed code, focuses the password, and sends nothing', async () => {
    const bodies: unknown[] = [];
    serveSection(true, readOffers(['totp']));
    capturePatches(bodies, accepted);

    await openWithCode();
    await userEvent.type(codeField(), FIXTURE_OTP);
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(primary());

    expect(await screen.findByText(MISSING_PASSWORD)).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveFocus());
    expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
    expect(screen.queryByText(MISSING_CODE)).not.toBeInTheDocument();
    // The code the user typed is kept, not cleared, by the guard.
    expect(codeField()).toHaveValue(FIXTURE_OTP);
    expect(bodies).toEqual([]);
    expect(toggle()).toBeChecked();
  });

  it('blocks an empty password with nothing else supplied, and focuses the password field', async () => {
    const bodies: unknown[] = [];
    serveSection(true, readOffers(['totp']));
    capturePatches(bodies, accepted);

    await openWithCode();
    await userEvent.click(primary());

    expect(await screen.findByText(MISSING_PASSWORD)).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveFocus());
    expect(passwordField()).toHaveAttribute('aria-invalid', 'true');
    expect(screen.queryByText(MISSING_CODE)).not.toBeInTheDocument();
    expect(bodies).toEqual([]);
  });

  it('asks for the code, not the password, once the password is typed alone', async () => {
    const bodies: unknown[] = [];
    serveSection(true, readOffers(['totp']));
    capturePatches(bodies, accepted);

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.click(primary());

    expect(await screen.findByText(MISSING_CODE)).toBeInTheDocument();
    await waitFor(() => expect(codeField()).toHaveFocus());
    expect(codeField()).toHaveAttribute('aria-invalid', 'true');
    expect(bodies).toEqual([]);
  });

  it('sends both factors together once both are present', async () => {
    const bodies: Body[] = [];
    serveSection(true, readOffers(['totp']));
    capturePatches(bodies, accepted);

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0].current_password).toBe(FIXTURE_PW);
    expect(bodies[0].mfa_code).toBe(FIXTURE_OTP);
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );
    expect(toggle()).not.toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — the security key (§3)', () => {
  let get: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    get = vi.fn().mockResolvedValue(CREDENTIAL);
    Object.defineProperty(navigator, 'credentials', {
      value: { get },
      writable: true,
      configurable: true,
    });
  });

  function serveCeremony(beginBodies: unknown[] = []): void {
    server.use(
      http.post(BEGIN_ENDPOINT, async ({ request }) => {
        beginBodies.push(await request.json());
        return HttpResponse.json({
          publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] },
        });
      }),
      http.post(FINISH_ENDPOINT, () => HttpResponse.json({ mfa_token: WEBAUTHN_TOKEN }))
    );
  }

  // Mutant: the token dropped, or sent under another key. A WebAuthn token rides
  // as `mfa_code`, exactly as a typed code does.
  it('sends the minted token as mfa_code, with the password, for the purge-fence purpose', async () => {
    const begins: unknown[] = [];
    const bodies: Body[] = [];
    serveSection(true, readOffers(['webauthn']));
    serveCeremony(begins);
    capturePatches(bodies, accepted);

    await flipFence();
    await screen.findByRole('group', { name: KEY_LABEL });
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0].mfa_code).toBe(WEBAUTHN_TOKEN);
    expect(bodies[0].current_password).toBe(FIXTURE_PW);
    // The token is spendable on this route only; begin must name it.
    expect(begins).toEqual([{ purpose: 'privacy.purge_fence_disable' }]);
    expect(get).toHaveBeenCalledTimes(1);
  });

  // The key replaces the typed code, never the password.
  it('needs no typed code on the security-key panel, but still needs the password', async () => {
    const bodies: Body[] = [];
    serveSection(true, readOffers(['webauthn']));
    serveCeremony();
    capturePatches(bodies, accepted);

    await flipFence();
    await screen.findByRole('group', { name: KEY_LABEL });
    expect(primary()).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(primary());

    expect(await screen.findByText(MISSING_PASSWORD)).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveFocus());
    // Nothing left: no ceremony started, no request sent.
    expect(get).not.toHaveBeenCalled();
    expect(bodies).toEqual([]);

    await userEvent.type(passwordField(), FIXTURE_PW);
    expect(primary()).not.toHaveAttribute('aria-disabled');
  });

  it('sends no request when the ceremony is cancelled, and says so', async () => {
    const bodies: unknown[] = [];
    serveSection(true, readOffers(['webauthn']));
    serveCeremony();
    capturePatches(bodies, accepted);
    get.mockRejectedValue(new DOMException('cancelled', 'NotAllowedError'));

    await flipFence();
    await screen.findByRole('group', { name: KEY_LABEL });
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.click(primary());

    expect(await screen.findByText(/cancelled or timed out/i)).toBeInTheDocument();
    expect(bodies).toEqual([]);
    expect(toggle()).toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — per-field errors (#2765)', () => {
  async function submitBothFactors(error: string): Promise<void> {
    serveSection(true, readOffers(['totp']));
    server.use(http.patch(PRIVACY_ENDPOINT, refuse({ error })));

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());
  }

  it('blames only the password on a wrong password', async () => {
    await submitBothFactors('Invalid password');

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    // A wrong password says nothing about the code the user typed, and the
    // rejected factor is the only one they have to retype.
    expect(screen.queryByText(WRONG_CODE, { exact: false })).not.toBeInTheDocument();
    expect(passwordField()).toHaveValue('');
    expect(codeField()).toHaveValue(FIXTURE_OTP);
    // #3466: the password refusal is the one that does not spend the code.
    await waitFor(() => expect(passwordField()).toHaveFocus());
  });

  it('blames only the code on a wrong code', async () => {
    await submitBothFactors('Invalid MFA code');

    expect(await screen.findByText(WRONG_CODE, { exact: false })).toBeInTheDocument();
    expect(screen.queryByText('That password is not correct.')).not.toBeInTheDocument();
    expect(codeField()).toHaveValue('');
    expect(passwordField()).toHaveValue(FIXTURE_PW);
  });

  // The server accepts each code once and can accept one yet still refuse the
  // change, so a code that reached it past the password check is never re-sent.
  it('drops the code after a refusal that may have spent it (a 500)', async () => {
    serveSection(true, readOffers(['totp']));
    server.use(
      http.patch(PRIVACY_ENDPOINT, refuse({ error: 'Failed to update privacy settings' }, 500))
    );

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());

    expect(await screen.findByText('Failed to update privacy settings')).toBeInTheDocument();
    expect(codeField()).toHaveValue('');
    expect(passwordField()).toHaveValue(FIXTURE_PW);
  });
});

describe('PurgeFenceStepUpDialog — a request that was never sent (#2765)', () => {
  // `resetAllStores` restores data, not actions, so the stub is put back by hand.
  const realDisablePurgeFence = usePrivacyStore.getState().disablePurgeFence;
  afterEach(() => usePrivacyStore.setState({ disablePurgeFence: realDisablePurgeFence }));

  // `apiFetch` refuses before dispatch when the account or server changed since
  // the run captured its context; the store reports that as `aborted`.
  // Mutant: `aborted` mapped to an answered outcome, which shows a banner and
  // spends the typed code; or the run's context not passed through.
  it('shows no banner, keeps the typed code, and hands the store the run context', async () => {
    serveSection(true, readOffers(['totp']));
    const disablePurgeFence = vi.fn().mockResolvedValue({ kind: 'aborted' });
    usePrivacyStore.setState({ disablePurgeFence });

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());

    await waitFor(() => expect(disablePurgeFence).toHaveBeenCalledOnce());
    const [credentials, context] = disablePurgeFence.mock.calls[0];
    expect(credentials).toEqual({ currentPassword: FIXTURE_PW, mfaCode: FIXTURE_OTP });
    expect(context).toBeDefined();
    await waitFor(() => expect(primary()).not.toHaveAttribute('aria-disabled'));
    expect(within(screen.getByRole('dialog')).queryByRole('alert')).not.toBeInTheDocument();
    expect(codeField()).toHaveValue(FIXTURE_OTP);
    expect(passwordField()).toHaveValue(FIXTURE_PW);
    expect(screen.getByRole('heading', { name: DIALOG_TITLE })).toBeInTheDocument();
    expect(toggle()).toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — an answer for an account that is gone (C57)', () => {
  /**
   * Holds the PATCH until the account changes, then releases `respond`. The
   * generation bump stands in for a sign-out or account switch landing while
   * the request is out: apiFetch fences only requests not yet dispatched.
   */
  async function answerAfterSwitch(respond: () => Response): Promise<void> {
    serveSection(true, readOffers(['totp']));
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const bodies: unknown[] = [];
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        bodies.push(await request.json());
        await gate;
        return respond();
      })
    );

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));

    useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
    release();
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: /Turning off/ })).not.toBeInTheDocument()
    );
  }

  // Mutant: the submit's context recheck removed, so the old account's
  // refusal is rendered for whoever is signed in now.
  it("does not show the old account's refusal", async () => {
    await answerAfterSwitch(refuse({ error: 'Failed to update privacy settings' }, 500));

    expect(screen.queryByText('Failed to update privacy settings')).not.toBeInTheDocument();
  });

  // Mutant: the same recheck removed, so the old account's acceptance closes
  // the dialog it no longer owns.
  it("does not close on the old account's acceptance", async () => {
    await answerAfterSwitch(accepted);

    expect(screen.getByRole('heading', { name: DIALOG_TITLE })).toBeInTheDocument();
  });
});

describe('PurgeFenceStepUpDialog — tightening is never gated (#2765)', () => {
  it('turns the fence back ON immediately, with no challenge and no read', async () => {
    serveSection(false, readOffers(['totp']));
    const bodies: unknown[] = [];
    capturePatches(bodies, () =>
      HttpResponse.json({ privacy: { ...basePrivacy, require_auth_before_purge: true } })
    );

    render(<PrivacySecuritySection />);
    const control = await screen.findByRole('switch', { name: TOGGLE_LABEL });
    expect(control).not.toBeChecked();
    await userEvent.click(control);

    // The asymmetry is the whole point of the issue: raising a protection must
    // never cost the user more than lowering it.
    await waitFor(() => expect(bodies).toEqual([{ require_auth_before_purge: true }]));
    expect(screen.queryByRole('heading', { name: DIALOG_TITLE })).not.toBeInTheDocument();
    expect(toggle()).toBeChecked();
    expect(readHits).toBe(0);
  });
});

describe('PurgeFenceStepUpDialog — accounts that hold only some factors (#2765)', () => {
  it('keeps the password field on mfa_required, so a correct password is not dropped', async () => {
    // Regression for a CodeRabbit finding on #2792. `mfa_required` WITHOUT
    // `password_required` does not mean the account is passwordless: the server
    // verifies the password factor FIRST, so an MFA-enabled account that sent a
    // correct password and no code receives exactly this shape.
    //
    // The dialog used to hide the field here, so the retry sent no password,
    // the server answered `password_required`, and the accepted password had to
    // be retyped — a loop costing two step-up attempts per cycle, which could
    // rate-limit an actor holding BOTH correct factors.
    //
    // The read offers nothing here (a stale or unsupported read), so the
    // refusal's own `methods` are what bring the code field up: the refusal is
    // authoritative over the read.
    const bodies: Body[] = [];
    serveSection(true, readOffers([]));
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        bodies.push((await request.json()) as Body);
        return HttpResponse.json(
          { error: 'MFA required', mfa_required: true, methods: ['totp'] },
          { status: 403 }
        );
      })
    );

    await openPasswordOnly();
    await userEvent.click(primary());

    // The field survives the refusal, still holding what the user typed.
    await findCode();
    expect(passwordField().value).toBe(FIXTURE_PW);

    // The retry therefore still carries the password alongside the new code —
    // which is what breaks the loop.
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());

    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1].current_password).toBe(FIXTURE_PW);
    expect(bodies[1].mfa_code).toBe(FIXTURE_OTP);
  });

  // Both fields are on screen from the start, so a refusal that names a MISSING
  // factor changes nothing visible unless the field says so.
  it('asks for the code when a correct password was sent alone, and focuses it', async () => {
    serveSection(true, readOffers([]));
    server.use(
      http.patch(
        PRIVACY_ENDPOINT,
        refuse({ error: 'MFA required', mfa_required: true, methods: ['totp'] })
      )
    );

    await openPasswordOnly();
    await userEvent.click(primary());

    expect(await screen.findByText(MISSING_CODE)).toBeInTheDocument();
    await waitFor(() => expect(codeField()).toHaveAttribute('aria-invalid', 'true'));
    await waitFor(() => expect(codeField()).toHaveFocus());
    expect(passwordField()).toHaveValue(FIXTURE_PW);
  });

  it('asks for the password when a code was sent alone, and focuses it', async () => {
    serveSection(true, readOffers(['totp']));
    server.use(
      http.patch(PRIVACY_ENDPOINT, refuse({ error: 'Password required', password_required: true }))
    );

    await openWithCode();
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());

    expect(await screen.findByText(MISSING_PASSWORD)).toBeInTheDocument();
    await waitFor(() => expect(passwordField()).toHaveAttribute('aria-invalid', 'true'));
    await waitFor(() => expect(passwordField()).toHaveFocus());
    expect(codeField()).toHaveValue(FIXTURE_OTP);
  });

  // §1.3 again, from the other side: a refusal naming only methods this app
  // cannot collect leaves no code box to fill.
  // Mutant: the refusal's `methods` not intersected with the inline set.
  it('shows no code field when mfa_required names only methods that cannot be collected here', async () => {
    serveSection(true, readOffers([]));
    server.use(
      http.patch(
        PRIVACY_ENDPOINT,
        refuse({ error: 'MFA required', mfa_required: true, methods: ['email'] })
      )
    );

    await openPasswordOnly();
    await userEvent.click(primary());

    expect(await screen.findByText(NO_USABLE_METHOD)).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(toggle()).toBeChecked();
  });

  it('offers nothing to retry when the account holds neither factor', async () => {
    const deadEnd = 'Your account signs in without a password and has no authenticator.';
    serveSection(true, readOffers([]));
    server.use(http.patch(PRIVACY_ENDPOINT, refuse({ error: deadEnd }, 400)));

    await openPasswordOnly();
    await userEvent.click(primary());

    // The server's own sentence is the whole answer; the client must not
    // paraphrase it, and nothing the user could type would work.
    const banner = await screen.findByText(deadEnd);
    expect(banner).toHaveClass('purge-modal__deadend');
    expect(banner).toHaveAttribute('role', 'alert');
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: SUBMIT_LABEL })).not.toBeInTheDocument();
    expect(toggle()).toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — one request, and nothing carried over (#2765)', () => {
  it('sends one request however many times the primary is activated while it is out', async () => {
    serveSection(true, readOffers(['totp']));
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const bodies: unknown[] = [];
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        bodies.push(await request.json());
        await gate;
        return accepted();
      })
    );

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));
    // The primary is now the in-flight button: `aria-disabled`, not natively
    // disabled, so activating it again reaches a guard that sends nothing.
    const inFlight = screen.getByRole('button', { name: /Turning off/ });
    expect(inFlight).toHaveAttribute('aria-disabled', 'true');
    expect(inFlight).not.toBeDisabled();
    await userEvent.click(inFlight);
    await userEvent.click(inFlight);

    release();
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );
    expect(bodies).toHaveLength(1);
  });

  // Mutant: `disabled={submitting}` re-added to the primary. A natively
  // disabled button loses focus, which falls to <body> mid-request.
  it('keeps the primary focused and not natively disabled while the request is out', async () => {
    serveSection(true, readOffers(['totp']));
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const bodies: unknown[] = [];
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        bodies.push(await request.json());
        await gate;
        return accepted();
      })
    );

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());
    await waitFor(() => expect(bodies).toHaveLength(1));

    const inFlight = screen.getByRole('button', { name: /Turning off/ });
    expect(inFlight).toHaveAttribute('aria-disabled', 'true');
    expect(inFlight).not.toBeDisabled();
    expect(inFlight).toHaveFocus();

    release();
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );
  });

  it('drops the password and the code when the dialog is closed', async () => {
    serveSection(true, readOffers(['totp']));

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );

    await userEvent.click(toggle());
    await findCode();
    expect(passwordField()).toHaveValue('');
    expect(codeField()).toHaveValue('');
    expect(toggle()).toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — credential containment (#2765)', () => {
  /**
   * Enumerate through the Storage API, never by spreading. jsdom keeps entries
   * in an internal slot rather than as own enumerable properties, so
   * `{ ...localStorage }` yields `{}` and the sweep would pass on an empty
   * string no matter what the dialog wrote — a vacuous assertion wearing the
   * shape of a real one.
   */
  function dumpStorage(store: Storage): string {
    const entries: Array<[string, string]> = [];
    for (let i = 0; i < store.length; i += 1) {
      const key = store.key(i);
      if (key !== null) entries.push([key, store.getItem(key) ?? '']);
    }
    return JSON.stringify(entries);
  }

  it('writes neither factor into local or session storage', async () => {
    serveSection(true, readOffers(['totp']));
    let attempts = 0;
    server.use(
      http.patch(PRIVACY_ENDPOINT, () => {
        attempts += 1;
        // The first attempt fails, so the sweep covers the error path too — a
        // refusal is exactly where a well-meaning "remember what they typed"
        // would land.
        if (attempts === 1) {
          return HttpResponse.json({ error: 'Invalid password' }, { status: 403 });
        }
        return accepted();
      })
    );

    await openWithCode();
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(primary());
    await screen.findByText('That password is not correct.');

    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.click(primary());
    // The dialog closes on acceptance and the fence is down — the end of the
    // OFF flow, so the sweep below runs over everything it could have written.
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: DIALOG_TITLE })).not.toBeInTheDocument()
    );
    expect(toggle()).not.toBeChecked();

    // Positive control: prove the enumeration can see a value at all, so a
    // future regression that empties it cannot masquerade as "no leak found".
    localStorage.setItem('purge-fence-storage-probe', FIXTURE_PW);
    expect(dumpStorage(localStorage)).toContain(FIXTURE_PW);
    localStorage.removeItem('purge-fence-storage-probe');

    const storage = `${dumpStorage(localStorage)}${dumpStorage(sessionStorage)}`;
    expect(storage).not.toContain(FIXTURE_PW);
    expect(storage).not.toContain(FIXTURE_OTP);
  });
});
