import { StrictMode } from 'react';
import { render as bareRender, screen, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { BrowserRouter } from 'react-router';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import Login from '@/renderer/components/Auth/Login';
import { __resetSignInEmailCodeForTests } from '@/renderer/components/Auth/signInEmailCode';

// Bound to constants so the credential-named fields below are followed by
// identifiers rather than quoted literals (detect-secrets keys on adjacency).
const FIXTURE_EMAIL = 'test@example.com';
const FIXTURE_PW = 'Password123!';
const SEND_PATH = '/api/v1/auth/mfa/email/send';
const GENERIC_SEND_ERROR = "Couldn't send your email code. Check your connection and try again.";

const VERIFY_PATH = '/api/v1/auth/mfa/verify';
const OTHER_SERVER = 'https://other-server.test';
// Login reports a move as it reports a torn-down sign-in.
const ORIGIN_CHANGED_NOTICE =
  'Your session ended before sign-in could finish. Please sign in again.';
const SENT = () =>
  HttpResponse.json({ message: 'Verification code sent to your email', expires_in: 600 });
const tokenOf = (body: unknown) => (body as { mfa_challenge_token: string }).mfa_challenge_token;

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  // Tokens repeat across tests, and the send guard is per token for the life
  // of the module.
  __resetSignInEmailCodeForTests();
});

const props = {
  onBack: vi.fn(),
  onSuccess: vi.fn(),
  onSwitchToRegister: vi.fn(),
  onForgotPassword: vi.fn(),
};

interface Challenge {
  token: string;
  methods: string[];
}

interface SendRecord {
  url: string;
  body: unknown;
}

// StrictMode is the topmost element, as in the dev renderer (main.tsx); nested
// under the providers, it would replay nothing (see
// SettingsPage.focus-request-strict-mode.test.tsx). Login's challenge always
// arrives after mount, so the replay never reaches the request here. The
// mount-time case is pinned in MFAChallengeModal.signInEmailCode.test.tsx.
function renderLogin() {
  return bareRender(
    <StrictMode>
      <BrowserRouter>
        <ModalProvider>
          <Login {...props} />
        </ModalProvider>
      </BrowserRouter>
    </StrictMode>
  );
}

// Each sign-in submission answers with the next challenge in the list.
function serveChallenges(challenges: Challenge[]) {
  let next = 0;
  server.use(
    http.post('*/api/v1/auth/login', () => {
      const challenge = challenges[Math.min(next, challenges.length - 1)];
      next += 1;
      return HttpResponse.json({
        mfa_required: true,
        mfa_challenge_token: challenge.token,
        methods: challenge.methods,
        recovery_only_methods: [],
      });
    })
  );
}

function recordSends(
  respond: (body: unknown) => Response | Promise<Response> = SENT
): SendRecord[] {
  const sends: SendRecord[] = [];
  server.use(
    http.post(`*${SEND_PATH}`, async ({ request }) => {
      const body: unknown = await request.json();
      sends.push({ url: request.url, body });
      return respond(body);
    })
  );
  return sends;
}

async function signIn(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Email'), FIXTURE_EMAIL);
  await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
  await user.click(screen.getByText('Sign In'));
  await screen.findByText('Two-Factor Authentication');
}

// Let any request a render queued reach the handler before counting.
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 20));
  });
}

async function pickMethod(user: ReturnType<typeof userEvent.setup>, label: RegExp) {
  await user.click(screen.getByText('Choose another form of verification'));
  await user.click(screen.getByRole('button', { name: label }));
}

describe('Login two-factor step requests the sign-in email code', () => {
  it('sends once, with the token only in the JSON body, when email is the default', async () => {
    serveChallenges([{ token: 'tok-email-1', methods: ['email'] }]);
    const sends = recordSends();
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);

    await waitFor(() => expect(sends).toHaveLength(1));
    await settle();
    expect(sends).toHaveLength(1);
    expect(sends[0].body).toEqual({ mfa_challenge_token: 'tok-email-1' });
    expect(new URL(sends[0].url).pathname).toBe(SEND_PATH);
    expect(sends[0].url).not.toContain('tok-email-1');
    expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('waits for a switch into email, sends once, and does not resend on switching away and back', async () => {
    serveChallenges([{ token: 'tok-totp-1', methods: ['totp', 'email'] }]);
    const sends = recordSends();
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);

    await settle();
    expect(sends, 'the TOTP default requests no email code').toHaveLength(0);

    await pickMethod(user, /Email \/ SMS Code/);
    await waitFor(() => expect(sends).toHaveLength(1));
    expect(sends[0].body).toEqual({ mfa_challenge_token: 'tok-totp-1' });

    await pickMethod(user, /Authenticator App/);
    await pickMethod(user, /Email \/ SMS Code/);
    await settle();
    expect(sends, 'the same challenge is never sent twice').toHaveLength(1);
  });

  it('never sends for an sms-only challenge', async () => {
    serveChallenges([{ token: 'tok-sms-1', methods: ['sms'] }]);
    const sends = recordSends();
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);

    expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
    await settle();
    expect(sends).toHaveLength(0);
  });

  it.each([
    {
      name: 'a 500 delivery failure',
      status: 500,
      error: 'Failed to send verification code',
    },
    {
      name: 'a 429 for a code already sent',
      status: 429,
      error: 'Email code already sent. Check your inbox or wait for it to expire.',
    },
  ])('shows the server message for $name and does not retry', async ({ status, error }) => {
    serveChallenges([{ token: 'tok-fail-1', methods: ['email'] }]);
    const sends = recordSends(() => HttpResponse.json({ error }, { status }));
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);

    expect(await screen.findByRole('alert')).toHaveTextContent(error);
    await settle();
    expect(sends).toHaveLength(1);
  });

  it('shows a generic message when the failure body is not JSON', async () => {
    serveChallenges([{ token: 'tok-html-1', methods: ['email'] }]);
    recordSends(() => new HttpResponse('<html>Bad gateway</html>', { status: 502 }));
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);

    expect(await screen.findByRole('alert')).toHaveTextContent(GENERIC_SEND_ERROR);
  });

  it('sends once more, for the new token only, when a new challenge replaces the first', async () => {
    serveChallenges([
      { token: 'tok-first', methods: ['email'] },
      { token: 'tok-second', methods: ['email'] },
    ]);
    const sends = recordSends();
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);
    await waitFor(() => expect(sends).toHaveLength(1));

    await user.click(screen.getByText('← Back to login'));
    await user.click(screen.getByText('Sign In'));
    await screen.findByText('Two-Factor Authentication');

    await waitFor(() => expect(sends).toHaveLength(2));
    await settle();
    expect(sends).toHaveLength(2);
    expect(sends.map((s) => s.body)).toEqual([
      { mfa_challenge_token: 'tok-first' },
      { mfa_challenge_token: 'tok-second' },
    ]);
  });

  // The replacement is an email challenge too, so its own panel is on screen
  // when the stale answer lands: only the token scoping can keep it off.
  it("does not put a superseded challenge's late failure on the challenge that replaced it", async () => {
    serveChallenges([
      { token: 'tok-stale', methods: ['email'] },
      { token: 'tok-live', methods: ['email'] },
    ]);
    const stale = deferred();
    const sends = recordSends(async (body) => {
      if (tokenOf(body) !== 'tok-stale') return SENT();
      await stale.promise;
      return HttpResponse.json({ error: 'error-for-the-stale-challenge' }, { status: 500 });
    });
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);
    await waitFor(() => expect(sends).toHaveLength(1));

    await user.click(screen.getByText('← Back to login'));
    await user.click(screen.getByText('Sign In'));
    await screen.findByText('Two-Factor Authentication');
    await waitFor(() => expect(sends).toHaveLength(2));
    await settle();

    stale.resolve();
    await settle();
    expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
    expect(screen.queryByText('error-for-the-stale-challenge')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    expect(sends.map((s) => tokenOf(s.body))).toEqual(['tok-stale', 'tok-live']);
  });

  // Login already binds its challenge to the selection captured at sign-in,
  // so these pin that it stays that way for the send as well as for verify.
  describe('the challenge stays with the server that issued it', () => {
    it('sends no email code and no verify request to a server selected afterwards', async () => {
      serveChallenges([{ token: 'tok-origin', methods: ['totp', 'email'] }]);
      const sends = recordSends();
      const verifies: string[] = [];
      server.use(
        http.post(`*${VERIFY_PATH}`, ({ request }) => {
          verifies.push(request.url);
          return HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 });
        })
      );
      const user = userEvent.setup();
      renderLogin();
      await signIn(user);
      act(() => {
        setRuntimeServerBase(OTHER_SERVER);
      });

      await pickMethod(user, /Email \/ SMS Code/);
      expect(await screen.findByRole('alert')).toHaveTextContent(ORIGIN_CHANGED_NOTICE);
      await settle();
      expect(sends, 'no send leaves for either server').toHaveLength(0);
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();

      const first = document.querySelector<HTMLInputElement>('.totp-digit');
      if (!first) throw new Error('no code input is rendered');
      await user.click(first);
      await user.paste('123456');
      await settle();
      expect(verifies, 'no verify leaves for either server').toHaveLength(0);
      const alerts = screen.getAllByRole('alert').map((a) => a.textContent);
      expect(alerts).toEqual([ORIGIN_CHANGED_NOTICE, ORIGIN_CHANGED_NOTICE]);
    });
  });

  it('shows no email error in another method when the failure arrives after the switch', async () => {
    serveChallenges([{ token: 'tok-left-email', methods: ['totp', 'email'] }]);
    let release!: () => void;
    const released = new Promise<void>((resolve) => {
      release = resolve;
    });
    const sends = recordSends(async () => {
      await released;
      return HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 });
    });
    const user = userEvent.setup();
    renderLogin();
    await signIn(user);
    await pickMethod(user, /Email \/ SMS Code/);
    await waitFor(() => expect(sends).toHaveLength(1));

    await pickMethod(user, /Authenticator App/);
    release();
    await settle();
    expect(
      screen.getByText('Enter the 6-digit code from your authenticator app')
    ).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  describe('Send a new code', () => {
    it('appears after a failed send and asks exactly once more, clearing the error', async () => {
      serveChallenges([{ token: 'tok-retry', methods: ['email'] }]);
      let calls = 0;
      const sends = recordSends(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 })
          : HttpResponse.json({ message: 'Verification code sent to your email', expires_in: 600 });
      });
      const user = userEvent.setup();
      renderLogin();
      await signIn(user);

      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Failed to send verification code'
      );
      await user.click(screen.getByRole('button', { name: 'Send a new code' }));

      await waitFor(() => expect(sends).toHaveLength(2));
      await settle();
      expect(sends).toHaveLength(2);
      expect(sends[1].body).toEqual({ mfa_challenge_token: 'tok-retry' });
      expect(screen.queryByRole('alert')).toBeNull();
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    });

    it.each([
      {
        name: 'a 429 for a code already sent',
        status: 429,
        error: 'Email code already sent. Check your inbox or wait for it to expire.',
      },
      { name: 'a 400 refusal', status: 400, error: 'Email MFA is not enabled for this account' },
    ])('does not appear after $name', async ({ status, error }) => {
      serveChallenges([{ token: 'tok-no-retry', methods: ['email'] }]);
      recordSends(() => HttpResponse.json({ error }, { status }));
      const user = userEvent.setup();
      renderLogin();
      await signIn(user);

      expect(await screen.findByRole('alert')).toHaveTextContent(error);
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    });
  });
});
