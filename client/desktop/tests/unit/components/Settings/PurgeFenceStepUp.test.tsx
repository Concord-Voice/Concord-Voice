import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PrivacySecuritySection from '@/renderer/components/Settings/PrivacySecuritySection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  default: () => null,
  WebAuthnCredential: {},
}));

// Named fixtures, used only by reference: the pre-commit detect-secrets hook
// flags a credential-shaped key beside a quoted literal regardless of the value.
// Both are long and distinctive because they are the NEEDLES in the storage
// sweep below — a short value ('pw') could collide with unrelated text and turn
// a real signal into noise. Mirrors tests/unit/components/Purge/StepUp.test.tsx.
const FIXTURE_PW = 'fixture-password-do-not-persist';
const FIXTURE_OTP = '314159';

const API_BASE = 'http://localhost:8080';
const PRIVACY_ENDPOINT = `${API_BASE}/api/v1/users/me/privacy`;
const HISTORY_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-history`;
const USER_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';

const TOGGLE_LABEL = 'Require authentication before purging';
const DIALOG_TITLE = 'Confirm it is you';
const SUBMIT_LABEL = 'Turn Off';

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

/**
 * Everything PrivacySecuritySection fetches on mount besides the privacy blob.
 * `fence` seeds the toggle's starting position: the OFF-flow cases need it up,
 * the ON case needs it down.
 */
function serveSection(fence: boolean): void {
  server.use(
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

/** Every PATCH body the privacy endpoint received, in order. */
function capturePatches(bodies: unknown[], respond: () => Response): void {
  server.use(
    http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
      bodies.push(await request.json());
      return respond();
    })
  );
}

const toggle = () => screen.getByRole('switch', { name: TOGGLE_LABEL });
const passwordField = () => screen.getByLabelText('Password') as HTMLInputElement;
const codeField = () => screen.getByLabelText('Authentication code') as HTMLInputElement;

/** Render the section and flip the fence toggle, waiting for the fetch first. */
async function flipFence(): Promise<void> {
  render(<PrivacySecuritySection />);
  await userEvent.click(await screen.findByRole('switch', { name: TOGGLE_LABEL }));
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
  sessionStorage.clear();
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
    serveSection(true);
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
    serveSection(true);
    server.use(
      http.patch(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ error: 'Invalid password' }, { status: 403 })
      )
    );

    await flipFence();
    await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    // Nothing changed server-side, so nothing may change client-side either.
    expect(toggle()).toBeChecked();
  });
});

describe('PurgeFenceStepUpDialog — per-field errors (#2765)', () => {
  async function submitBothFactors(error: string): Promise<void> {
    serveSection(true);
    server.use(http.patch(PRIVACY_ENDPOINT, () => HttpResponse.json({ error }, { status: 403 })));

    await flipFence();
    await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));
  }

  it('blames only the password on a wrong password', async () => {
    await submitBothFactors('Invalid password');

    expect(await screen.findByText('That password is not correct.')).toBeInTheDocument();
    // A wrong password says nothing about the code the user typed, and the
    // rejected factor is the only one they have to retype.
    expect(
      screen.queryByText('That code is not correct, or it has expired. Try the next one.')
    ).not.toBeInTheDocument();
    expect(passwordField()).toHaveValue('');
    expect(codeField()).toHaveValue(FIXTURE_OTP);
  });

  it('blames only the code on a wrong code', async () => {
    await submitBothFactors('Invalid MFA code');

    expect(
      await screen.findByText('That code is not correct, or it has expired. Try the next one.')
    ).toBeInTheDocument();
    expect(screen.queryByText('That password is not correct.')).not.toBeInTheDocument();
    expect(codeField()).toHaveValue('');
    expect(passwordField()).toHaveValue(FIXTURE_PW);
  });
});

describe('PurgeFenceStepUpDialog — tightening is never gated (#2765)', () => {
  it('turns the fence back ON immediately, with no challenge', async () => {
    serveSection(false);
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
    const bodies: Array<Record<string, unknown>> = [];
    serveSection(true);
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json(
          { error: 'MFA required', mfa_required: true, methods: ['totp'] },
          { status: 403 }
        );
      })
    );

    await flipFence();
    await screen.findByRole('heading', { name: DIALOG_TITLE });
    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));

    // The field survives the refusal, still holding what the user typed.
    await waitFor(() => expect(screen.getByLabelText('Password')).toBeInTheDocument());
    expect(passwordField().value).toBe(FIXTURE_PW);

    // The retry therefore still carries the password alongside the new code —
    // which is what breaks the loop.
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));

    await waitFor(() => expect(bodies.length).toBe(2));
    expect(bodies[1].current_password).toBe(FIXTURE_PW);
    expect(bodies[1].mfa_code).toBe(FIXTURE_OTP);
  });

  it('accepts an MFA-only submission, which is how an account with no password proceeds', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    serveSection(true);
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ privacy: { require_auth_before_purge: false } });
      })
    );

    await flipFence();
    await screen.findByRole('heading', { name: DIALOG_TITLE });
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));

    // An empty password field is simply omitted; the server accepts MFA alone.
    await waitFor(() => expect(bodies.length).toBe(1));
    expect(bodies[0].current_password).toBeUndefined();
    expect(bodies[0].mfa_code).toBe(FIXTURE_OTP);
  });

  it('offers nothing to retry when the account holds neither factor', async () => {
    const deadEnd = 'Your account signs in without a password and has no authenticator.';
    serveSection(true);
    server.use(
      http.patch(PRIVACY_ENDPOINT, () => HttpResponse.json({ error: deadEnd }, { status: 400 }))
    );

    await flipFence();
    await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));

    // The server's own sentence is the whole answer; the client must not
    // paraphrase it, and nothing the user could type would work.
    const banner = await screen.findByText(deadEnd);
    expect(banner).toHaveClass('purge-modal__deadend');
    expect(banner).toHaveAttribute('role', 'alert');
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authentication code')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: SUBMIT_LABEL })).not.toBeInTheDocument();
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
    serveSection(true);
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
        return HttpResponse.json({
          privacy: { ...basePrivacy, require_auth_before_purge: false },
        });
      })
    );

    await flipFence();
    await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await userEvent.type(codeField(), FIXTURE_OTP);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));
    await screen.findByText('That password is not correct.');

    await userEvent.type(passwordField(), FIXTURE_PW);
    await userEvent.click(screen.getByRole('button', { name: SUBMIT_LABEL }));
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
