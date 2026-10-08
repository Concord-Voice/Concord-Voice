import { StrictMode } from 'react';
import { render as bareRender, renderHook, screen, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import {
  useMFAChallengeStore,
  type MFAChallengeResult,
} from '@/renderer/stores/auth/mfaChallengeStore';
import {
  completeSSOMFA,
  startSSOFlow,
  type SSOCompletionResult,
} from '@/renderer/services/system/ssoService';
import {
  captureRuntimeServerSelection,
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import {
  __challengeIssuerCountForTests,
  __resetChallengeIssuersForTests,
  CHALLENGE_TTL_MS,
  challengeIssuerFor,
  recordChallengeIssuer,
} from '@/renderer/services/system/challengeIssuer';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { _resetRefreshState, refreshAccessToken } from '@/renderer/services/system/apiClient';
import type { RefreshResult } from '@/main/ipcContract';
import { useSSOFlow } from '@/renderer/hooks/ui/useSSOFlow';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import MFAChallengeModal from '@/renderer/components/Auth/MFAChallengeModal';
import {
  __resetSignInEmailCodeForTests,
  __signInEmailCodeSendCountForTests,
} from '@/renderer/components/Auth/signInEmailCode';

// Only main's SSO calls are replaced; the email send, the verify POST and the
// logout go through MSW.
vi.mock('@/renderer/services/system/ssoService', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/ssoService')>();
  return {
    ...actual,
    completeSSOMFA: vi.fn(() => new Promise(() => {})),
    startSSOFlow: vi.fn(),
  };
});

const SEND_PATH = '/api/v1/auth/mfa/email/send';
const VERIFY_PATH = '/api/v1/auth/mfa/verify';
const OTHER_SERVER = 'https://other-server.test';
const SERVER_CHANGED_ERROR = 'The server changed. Cancel and try again.';
const ORIGINAL_SERVER = 'http://localhost:8080';
const GENERIC_SEND_ERROR = "Couldn't send your email code. Check your connection and try again.";
const SENT = () =>
  HttpResponse.json({ message: 'Verification code sent to your email', expires_in: 600 });

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  // Tokens repeat across tests, and both guards are per token for the life of
  // the module.
  __resetSignInEmailCodeForTests();
  __resetChallengeIssuersForTests();
  vi.mocked(completeSSOMFA).mockClear();
  vi.mocked(startSSOFlow).mockReset();
});

interface SendRecord {
  url: string;
  body: unknown;
}

// StrictMode is the topmost element, so React replays the mount effects here
// as it does in the dev renderer (main.tsx). Nested under the provider, it
// would replay nothing (see SettingsPage.focus-request-strict-mode.test.tsx).
function renderModal() {
  return bareRender(
    <StrictMode>
      <ModalProvider>
        <MFAChallengeModal />
      </ModalProvider>
    </StrictMode>
  );
}

// Raises a challenge as useSSOFlow and apiClient do: the issuer is recorded
// under the current selection before the challenge is published.
function show(token: string, methods: string[], purpose: 'suspicious_refresh' | 'sso_login') {
  recordChallengeIssuer(token, captureRuntimeServerSelection());
  publish(token, methods, purpose);
}

// Publishes a challenge with no issuer recorded for it.
function publish(token: string, methods: string[], purpose: 'suspicious_refresh' | 'sso_login') {
  act(() => {
    void useMFAChallengeStore
      .getState()
      .showChallenge(
        token,
        methods,
        purpose,
        [],
        purpose === 'sso_login' ? { provider: 'google', credentialOwner: 5 } : undefined
      );
  });
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

// The user moves the app to another server while the challenge is open.
function switchServer() {
  act(() => {
    setRuntimeServerBase(OTHER_SERVER);
  });
}

async function enterCode(user: ReturnType<typeof userEvent.setup>) {
  const first = document.querySelector<HTMLInputElement>('.totp-digit');
  if (!first) throw new Error('no code input is rendered');
  await user.click(first);
  await user.paste('123456');
}

const tokenOf = (body: unknown) => (body as { mfa_challenge_token: string }).mfa_challenge_token;

function recordVerifies(respond: () => Response | Promise<Response>): string[] {
  const verifies: string[] = [];
  server.use(
    http.post(`*${VERIFY_PATH}`, ({ request }) => {
      verifies.push(request.url);
      return respond();
    })
  );
  return verifies;
}

interface LogoutRecord {
  url: string;
  sessionId: string | null;
  authorization: string | null;
}

function recordLogouts(): LogoutRecord[] {
  const logouts: LogoutRecord[] = [];
  server.use(
    http.post('*/api/v1/auth/logout', ({ request }) => {
      logouts.push({
        url: request.url,
        sessionId: request.headers.get('X-Session-ID'),
        authorization: request.headers.get('Authorization'),
      });
      return HttpResponse.json({ message: 'Logged out' });
    })
  );
  return logouts;
}

describe('MFAChallengeModal requests the sign-in email code', () => {
  it.each(['suspicious_refresh', 'sso_login'] as const)(
    'sends once, with the token only in the JSON body, when email is the default (%s)',
    async (purpose) => {
      const sends = recordSends();
      renderModal();
      show('modal-email-1', ['email'], purpose);

      await waitFor(() => expect(sends).toHaveLength(1));
      await settle();
      expect(sends).toHaveLength(1);
      expect(sends[0].body).toEqual({ mfa_challenge_token: 'modal-email-1' });
      expect(new URL(sends[0].url).pathname).toBe(SEND_PATH);
      expect(sends[0].url).not.toContain('modal-email-1');
      expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
      expect(screen.queryByRole('alert')).toBeNull();
    }
  );

  // A challenge can already be open when the modal mounts: App renders it in
  // the restoring tree and again in the main tree. Only then does StrictMode's
  // mount replay reach the request, so these two cases are what pin the
  // once-per-token guard and the result surviving the replay's cleanup.
  describe('a challenge already open at mount (StrictMode replays the mount)', () => {
    function openBeforeMount(token: string) {
      recordChallengeIssuer(token, captureRuntimeServerSelection());
      useMFAChallengeStore.setState({
        challengeToken: token,
        methods: ['email'],
        recoveryOnlyMethods: [],
        purpose: 'suspicious_refresh',
      });
    }

    it('sends once', async () => {
      const sends = recordSends();
      openBeforeMount('modal-at-mount');
      renderModal();

      await waitFor(() => expect(sends).toHaveLength(1));
      await settle();
      expect(sends).toHaveLength(1);
      expect(sends[0].body).toEqual({ mfa_challenge_token: 'modal-at-mount' });
    });

    it('still shows the failure', async () => {
      const error = 'Failed to send verification code';
      recordSends(() => HttpResponse.json({ error }, { status: 500 }));
      openBeforeMount('modal-at-mount-fail');
      renderModal();

      expect(await screen.findByRole('alert')).toHaveTextContent(error);
    });
  });

  it('waits for a switch into email, sends once, and does not resend on switching away and back', async () => {
    const sends = recordSends();
    const user = userEvent.setup();
    renderModal();
    show('modal-totp-1', ['totp', 'email'], 'suspicious_refresh');

    await settle();
    expect(sends, 'the TOTP default requests no email code').toHaveLength(0);

    await pickMethod(user, /Email \/ SMS Code/);
    await waitFor(() => expect(sends).toHaveLength(1));
    expect(sends[0].body).toEqual({ mfa_challenge_token: 'modal-totp-1' });

    await pickMethod(user, /Authenticator App/);
    await pickMethod(user, /Email \/ SMS Code/);
    await settle();
    expect(sends, 'the same challenge is never sent twice').toHaveLength(1);
  });

  it('never sends for an sms-only challenge', async () => {
    const sends = recordSends();
    renderModal();
    show('modal-sms-1', ['sms'], 'suspicious_refresh');

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
    const sends = recordSends(() => HttpResponse.json({ error }, { status }));
    renderModal();
    show('modal-fail-1', ['email'], 'suspicious_refresh');

    expect(await screen.findByRole('alert')).toHaveTextContent(error);
    await settle();
    expect(sends).toHaveLength(1);
  });

  it('shows a generic message when the failure body is not JSON', async () => {
    recordSends(() => new HttpResponse('<html>Bad gateway</html>', { status: 502 }));
    renderModal();
    show('modal-html-1', ['email'], 'suspicious_refresh');

    expect(await screen.findByRole('alert')).toHaveTextContent(
      "Couldn't send your email code. Check your connection and try again."
    );
  });

  it('sends once more, for the new token only, when a new challenge replaces the first', async () => {
    const sends = recordSends();
    renderModal();
    show('modal-first', ['email'], 'suspicious_refresh');
    await waitFor(() => expect(sends).toHaveLength(1));

    show('modal-second', ['email'], 'suspicious_refresh');
    await waitFor(() => expect(sends).toHaveLength(2));
    await settle();
    expect(sends).toHaveLength(2);
    expect(sends.map((s) => s.body)).toEqual([
      { mfa_challenge_token: 'modal-first' },
      { mfa_challenge_token: 'modal-second' },
    ]);
  });

  // The replacement is an email challenge too, so its own panel is on screen
  // when the stale answer lands: only the token scoping can keep it off.
  it("does not put a superseded challenge's late failure on the challenge that replaced it", async () => {
    const stale = deferred();
    const sends = recordSends(async (body) => {
      if (tokenOf(body) !== 'modal-stale') return SENT();
      await stale.promise;
      return HttpResponse.json({ error: 'error-for-the-stale-challenge' }, { status: 500 });
    });
    renderModal();
    show('modal-stale', ['email'], 'suspicious_refresh');
    await waitFor(() => expect(sends).toHaveLength(1));

    show('modal-live', ['email'], 'suspicious_refresh');
    await waitFor(() => expect(sends).toHaveLength(2));
    await settle();

    stale.resolve();
    await settle();
    expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
    expect(screen.queryByText('error-for-the-stale-challenge')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    expect(sends.map((s) => tokenOf(s.body))).toEqual(['modal-stale', 'modal-live']);
  });

  it('sends to the server a challenge arrived under when it arrives after a move', async () => {
    const sends = recordSends();
    const verifies = recordVerifies(() =>
      HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 })
    );
    const user = userEvent.setup();
    switchServer();
    renderModal();
    show('modal-after-move', ['email'], 'suspicious_refresh');

    await waitFor(() => expect(sends).toHaveLength(1));
    await enterCode(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid MFA code');
    expect(sends.map((s) => new URL(s.url).origin)).toEqual([OTHER_SERVER]);
    expect(verifies.map((url) => new URL(url).origin)).toEqual([OTHER_SERVER]);
  });

  // App renders the modal in the restoring tree and again in the main tree,
  // so one challenge can meet a second instance.
  describe('a remount on the same challenge', () => {
    it('does not ask for the code again', async () => {
      const sends = recordSends();
      const first = renderModal();
      show('modal-remount', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(1));
      await settle();

      first.unmount();
      renderModal();
      await settle();
      expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
      expect(sends).toHaveLength(1);
    });

    it('keeps the server the challenge arrived under', async () => {
      const verifies = recordVerifies(() =>
        HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 })
      );
      const user = userEvent.setup();
      const first = renderModal();
      show('modal-remount-origin', ['totp'], 'suspicious_refresh');

      first.unmount();
      switchServer();
      renderModal();
      await enterCode(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      await settle();
      expect(verifies).toEqual([]);
    });
  });

  // The token belongs to the server that issued it, so its traffic stays
  // there even when the app's server selection moves on.
  describe('the challenge stays with the server that issued it', () => {
    it('sends no email code once the selection moves to another server', async () => {
      const sends = recordSends();
      const user = userEvent.setup();
      renderModal();
      show('modal-origin-send', ['totp', 'email'], 'suspicious_refresh');
      switchServer();

      await pickMethod(user, /Email \/ SMS Code/);
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      await settle();
      expect(sends.map((s) => new URL(s.url).origin)).not.toContain(OTHER_SERVER);
      expect(sends).toHaveLength(0);
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    });

    it('sends no verify request once the selection moves to another server', async () => {
      const verifies: string[] = [];
      server.use(
        http.post(`*${VERIFY_PATH}`, ({ request }) => {
          verifies.push(request.url);
          return HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 });
        })
      );
      const user = userEvent.setup();
      renderModal();
      show('modal-origin-verify', ['totp'], 'suspicious_refresh');
      switchServer();

      await enterCode(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      await settle();
      expect(verifies).toEqual([]);
    });

    it('sends no SSO proof once the selection moves to another server', async () => {
      const user = userEvent.setup();
      renderModal();
      show('modal-origin-sso', ['totp'], 'sso_login');
      switchServer();

      await enterCode(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      expect(completeSSOMFA).not.toHaveBeenCalled();
    });

    it('gives the SSO proof the selection the challenge arrived under', async () => {
      const user = userEvent.setup();
      renderModal();
      show('modal-origin-sso-ok', ['totp'], 'sso_login');

      await enterCode(user);
      await waitFor(() => expect(completeSSOMFA).toHaveBeenCalledTimes(1));
      const [, selection] = vi.mocked(completeSSOMFA).mock.calls[0];
      expect(selection.apiBase).toBe('http://localhost:8080');
    });

    it.each([
      { name: 'lands after the move', moved: true },
      { name: 'lands with no move (control)', moved: false },
    ])('a verify answer that issued a session and $name', async ({ moved }) => {
      const released = deferred();
      const verifies = recordVerifies(async () => {
        await released.promise;
        return HttpResponse.json(
          { access_token: 'issued-by-the-first-server', session_id: 'session-in-the-body' },
          { headers: { 'X-Concord-Session-ID': 'session-the-server-issued' } }
        );
      });
      const logouts = recordLogouts();
      const user = userEvent.setup();
      renderModal();
      let result: MFAChallengeResult | undefined;
      recordChallengeIssuer('modal-origin-late', captureRuntimeServerSelection());
      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('modal-origin-late', ['totp'], 'suspicious_refresh')
          .then((settled) => {
            result = settled;
          });
      });

      await enterCode(user);
      await waitFor(() => expect(verifies).toHaveLength(1));
      if (moved) switchServer();
      released.resolve();

      if (!moved) {
        await waitFor(() => expect(result).toEqual(expect.objectContaining({ verified: true })));
        await settle();
        expect(logouts, 'a kept answer is not revoked').toEqual([]);
        return;
      }
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      expect(result, 'the answer must not settle the challenge').toBeUndefined();
      expect(useMFAChallengeStore.getState().challengeToken).toBe('modal-origin-late');
      // Revoked where it was issued, by the server's own session ID.
      await waitFor(() => expect(logouts).toHaveLength(1));
      expect(new URL(logouts[0].url).origin).toBe(ORIGINAL_SERVER);
      expect(logouts[0].sessionId).toBe('session-the-server-issued');
      expect(logouts[0].authorization).toBe('Bearer issued-by-the-first-server');
    });

    // Main stores an SSO completion's refresh credential before the answer
    // reaches the renderer, so the completion has to reach useSSOFlow's fence,
    // which discards it, rather than stop at the modal.
    describe('an SSO completion', () => {
      let originalElectron: typeof globalThis.electron;
      let clearTokensIfOwner: ReturnType<typeof vi.fn>;

      beforeEach(() => {
        originalElectron = globalThis.electron;
        clearTokensIfOwner = vi.fn().mockResolvedValue(true);
        Object.defineProperty(globalThis, 'electron', {
          value: {
            ...originalElectron,
            clearTokens: vi.fn().mockResolvedValue(undefined),
            clearTokensIfOwner,
          },
          writable: true,
        });
      });

      afterEach(() => {
        Object.defineProperty(globalThis, 'electron', { value: originalElectron, writable: true });
      });

      it.each([
        { name: 'that lands after the move is discarded and revoked', moved: true },
        { name: 'that lands with no move is admitted (control)', moved: false },
      ])('$name', async ({ moved }) => {
        const completion = deferred<SSOCompletionResult>();
        vi.mocked(completeSSOMFA).mockReturnValueOnce(completion.promise);
        vi.mocked(startSSOFlow).mockResolvedValueOnce({
          kind: 'mfa_required',
          mfaChallengeToken: 'modal-sso-late',
          methods: ['totp'],
          recoveryOnlyMethods: [],
          credentialOwner: 5,
        });
        const logouts = recordLogouts();
        const user = userEvent.setup();
        renderModal();
        const sso = renderHook(() => useSSOFlow());
        await act(async () => {
          await sso.result.current.begin('google');
        });

        await enterCode(user);
        await waitFor(() => expect(completeSSOMFA).toHaveBeenCalledTimes(1));
        if (moved) switchServer();
        await act(async () => {
          completion.resolve({
            accessToken: 'sso-access-token',
            sessionId: 'sso-session-id',
            credentialOwner: 5,
          });
          await completion.promise;
        });

        if (!moved) {
          await waitFor(() => expect(useAuthStore.getState().accessToken).toBe('sso-access-token'));
          await settle();
          expect(clearTokensIfOwner).not.toHaveBeenCalled();
          expect(logouts).toEqual([]);
          return;
        }
        await waitFor(() => expect(logouts).toHaveLength(1));
        expect(clearTokensIfOwner).toHaveBeenCalledWith(5);
        expect(new URL(logouts[0].url).origin).toBe(ORIGINAL_SERVER);
        expect(logouts[0].sessionId).toBe('sso-session-id');
        expect(logouts[0].authorization).toBe('Bearer sso-access-token');
        expect(useAuthStore.getState().accessToken).toBeNull();
      });
    });

    it('drops an email send failure that lands after the move', async () => {
      let release!: () => void;
      const released = new Promise<void>((resolve) => {
        release = resolve;
      });
      const sends = recordSends(async () => {
        await released;
        return HttpResponse.json({ error: 'error-after-the-move' }, { status: 500 });
      });
      renderModal();
      show('modal-origin-late-send', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(1));

      switchServer();
      release();
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      expect(screen.queryByText('error-after-the-move')).toBeNull();
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    });
  });

  it('asks again on coming back to email after a failure that landed while away', async () => {
    const firstAnswer = deferred();
    let calls = 0;
    const sends = recordSends(async () => {
      calls += 1;
      if (calls > 1) return SENT();
      await firstAnswer.promise;
      return HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 });
    });
    const user = userEvent.setup();
    renderModal();
    show('modal-away-fail', ['totp', 'email'], 'suspicious_refresh');
    await pickMethod(user, /Email \/ SMS Code/);
    await waitFor(() => expect(sends).toHaveLength(1));

    await pickMethod(user, /Authenticator App/);
    firstAnswer.resolve();
    await settle();
    await pickMethod(user, /Email \/ SMS Code/);

    await waitFor(() => expect(sends).toHaveLength(2));
    await settle();
    expect(sends).toHaveLength(2);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('does not show an old failure beside the send that coming back to email starts', async () => {
    const secondAnswer = deferred();
    let calls = 0;
    const sends = recordSends(async () => {
      calls += 1;
      if (calls === 1) {
        return HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 });
      }
      await secondAnswer.promise;
      return SENT();
    });
    const user = userEvent.setup();
    renderModal();
    show('modal-shown-fail', ['totp', 'email'], 'suspicious_refresh');
    await pickMethod(user, /Email \/ SMS Code/);
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to send verification code');

    await pickMethod(user, /Authenticator App/);
    await pickMethod(user, /Email \/ SMS Code/);
    await waitFor(() => expect(sends).toHaveLength(2));
    await settle();
    expect(screen.queryByRole('alert'), 'the new send is still out').toBeNull();
    expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();

    secondAnswer.resolve();
    await settle();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows no email error in another method when the failure arrives after the switch', async () => {
    let release!: () => void;
    const released = new Promise<void>((resolve) => {
      release = resolve;
    });
    const sends = recordSends(async () => {
      await released;
      return HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 });
    });
    const user = userEvent.setup();
    renderModal();
    show('modal-left-email', ['totp', 'email'], 'suspicious_refresh');
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
      let calls = 0;
      const sends = recordSends(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 })
          : HttpResponse.json({ message: 'Verification code sent to your email', expires_in: 600 });
      });
      const user = userEvent.setup();
      renderModal();
      show('modal-retry', ['email'], 'suspicious_refresh');

      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Failed to send verification code'
      );
      await user.click(screen.getByRole('button', { name: 'Send a new code' }));

      await waitFor(() => expect(sends).toHaveLength(2));
      await settle();
      expect(sends).toHaveLength(2);
      expect(sends[1].body).toEqual({ mfa_challenge_token: 'modal-retry' });
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
      recordSends(() => HttpResponse.json({ error }, { status }));
      renderModal();
      show('modal-no-retry', ['email'], 'suspicious_refresh');

      expect(await screen.findByRole('alert')).toHaveTextContent(error);
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    });

    // The rate limiter answers before the handler claims the challenge's
    // send, and only it sets Retry-After.
    it("appears after the rate limiter's 429 and shows its wait", async () => {
      const wait = 'Too many requests. Please try again in 840 seconds.';
      let calls = 0;
      const sends = recordSends(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json(
              { error: 'Rate limit exceeded', message: wait },
              { status: 429, headers: { 'Retry-After': '840' } }
            )
          : SENT();
      });
      const user = userEvent.setup();
      renderModal();
      show('modal-limited', ['email'], 'suspicious_refresh');

      expect(await screen.findByRole('alert')).toHaveTextContent(wait);
      await user.click(screen.getByRole('button', { name: 'Send a new code' }));
      await waitFor(() => expect(sends).toHaveLength(2));
    });

    it('appears with the generic message when the request never reaches the server', async () => {
      recordSends(() => HttpResponse.error());
      renderModal();
      show('modal-network', ['email'], 'suspicious_refresh');

      expect(await screen.findByRole('alert')).toHaveTextContent(GENERIC_SEND_ERROR);
      expect(screen.getByRole('button', { name: 'Send a new code' })).toBeInTheDocument();
    });

    it('appears when a send that never answers times out', async () => {
      const realTimeout = AbortSignal.timeout.bind(AbortSignal);
      const timeout = vi.spyOn(AbortSignal, 'timeout').mockImplementation(() => realTimeout(50));
      try {
        recordSends(() => new Promise<Response>(() => {}));
        renderModal();
        show('modal-hung', ['email'], 'suspicious_refresh');

        expect(await screen.findByRole('alert')).toHaveTextContent(GENERIC_SEND_ERROR);
        expect(screen.getByRole('button', { name: 'Send a new code' })).toBeInTheDocument();
        expect(timeout).toHaveBeenCalledWith(30_000);
      } finally {
        timeout.mockRestore();
      }
    });

    it('asks once for a double click', async () => {
      let calls = 0;
      const sends = recordSends(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 })
          : SENT();
      });
      const user = userEvent.setup();
      renderModal();
      show('modal-double', ['email'], 'suspicious_refresh');

      await user.dblClick(await screen.findByRole('button', { name: 'Send a new code' }));
      await waitFor(() => expect(sends).toHaveLength(2));
      await settle();
      expect(sends).toHaveLength(2);
    });

    it('is disabled while a code is being checked', async () => {
      recordSends(() =>
        HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 })
      );
      const verify = deferred();
      const verifies = recordVerifies(async () => {
        await verify.promise;
        return HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 });
      });
      const user = userEvent.setup();
      renderModal();
      show('modal-busy', ['email'], 'suspicious_refresh');
      const button = await screen.findByRole('button', { name: 'Send a new code' });

      await enterCode(user);
      await waitFor(() => expect(verifies).toHaveLength(1));
      expect(button).toBeDisabled();
      verify.resolve();
      await waitFor(() => expect(button).not.toBeDisabled());
    });

    it('moves focus to the code as it goes away', async () => {
      let calls = 0;
      recordSends(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ error: 'Failed to send verification code' }, { status: 500 })
          : SENT();
      });
      const user = userEvent.setup();
      renderModal();
      show('modal-focus', ['email'], 'suspicious_refresh');

      await user.click(await screen.findByRole('button', { name: 'Send a new code' }));
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
      expect(screen.getByLabelText('Digit 1')).toHaveFocus();
    });
  });
});

// The two producers raise a challenge after an await, and the selection can
// move before the modal first renders the token. Its traffic goes to the
// server that issued it, or nowhere, and never to the server moved to.
describe('MFAChallengeModal sends a challenge only to the server that raised it', () => {
  let originalElectron: typeof globalThis.electron;

  beforeEach(() => {
    originalElectron = globalThis.electron;
    _resetRefreshState();
  });

  afterEach(() => {
    // Settles a challenge a test left open, so its refresh finishes.
    useMFAChallengeStore.getState().clearChallenge();
    Object.defineProperty(globalThis, 'electron', { value: originalElectron, writable: true });
  });

  function stubRefresh(refreshToken: () => Promise<RefreshResult>) {
    Object.defineProperty(globalThis, 'electron', {
      value: { ...originalElectron, refreshToken: vi.fn(refreshToken) },
      writable: true,
    });
  }

  // Moves the selection in the same tick the store publishes the token, so
  // the modal's first render already sees the new server.
  function switchServerWhenPublished(token: string): () => void {
    return useMFAChallengeStore.subscribe((state, prev) => {
      if (state.challengeToken === token && prev.challengeToken !== token) {
        setRuntimeServerBase(OTHER_SERVER);
      }
    });
  }

  // The email panel is on screen, no send left for it, and it says why.
  async function expectRefusedOnScreen(sends: SendRecord[]) {
    await screen.findByText('Enter the verification code sent to you');
    await settle();
    expect(sends.map((s) => new URL(s.url).origin)).toEqual([]);
    expect(screen.getByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
  }

  const refreshChallenge = (token: string): RefreshResult => ({
    status: 'mfa_required',
    mfaChallengeToken: token,
    mfaMethods: ['email'],
  });

  describe('a suspicious-refresh challenge', () => {
    it('sends nothing when the selection moves while the refresh is out', async () => {
      const sends = recordSends();
      const verifies = recordVerifies(() =>
        HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 })
      );
      const refresh = deferred<RefreshResult>();
      stubRefresh(() => refresh.promise);
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', 'sess-before-mfa');
      const user = userEvent.setup();
      renderModal();

      void refreshAccessToken();
      switchServer();
      refresh.resolve(refreshChallenge('refresh-moved-during'));

      await expectRefusedOnScreen(sends);
      await enterCode(user);
      await settle();
      expect(verifies).toEqual([]);
    });

    it('sends nothing when the selection moves as the challenge is published', async () => {
      const sends = recordSends();
      const verifies = recordVerifies(() =>
        HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 })
      );
      stubRefresh(async () => refreshChallenge('refresh-moved-at-publish'));
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', 'sess-before-mfa');
      const unsubscribe = switchServerWhenPublished('refresh-moved-at-publish');
      const user = userEvent.setup();
      renderModal();

      try {
        void refreshAccessToken();
        await expectRefusedOnScreen(sends);
        await enterCode(user);
        await settle();
      } finally {
        unsubscribe();
      }
      expect(verifies).toEqual([]);
    });

    it('sends to the server that raised it when the selection does not move (control)', async () => {
      const sends = recordSends();
      const verifies = recordVerifies(() =>
        HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 })
      );
      stubRefresh(async () => refreshChallenge('refresh-not-moved'));
      useAuthStore.getState().beginAuthLifecycle('pre-mfa-token', 'sess-before-mfa');
      const user = userEvent.setup();
      renderModal();

      void refreshAccessToken();
      await waitFor(() => expect(sends).toHaveLength(1));
      await enterCode(user);
      expect(await screen.findByRole('alert')).toHaveTextContent('Invalid MFA code');
      expect(sends.map((s) => new URL(s.url).origin)).toEqual([ORIGINAL_SERVER]);
      expect(verifies.map((url) => new URL(url).origin)).toEqual([ORIGINAL_SERVER]);
    });
  });

  // Taking the selection at render instead would send the token to whichever
  // server is selected then, so a token with no issuer gets nothing.
  describe('a challenge with no recorded issuer', () => {
    it('sends no email code and says the server changed', async () => {
      const sends = recordSends();
      renderModal();
      publish('no-issuer-email', ['email'], 'suspicious_refresh');

      await expectRefusedOnScreen(sends);
      expect(screen.queryByRole('button', { name: 'Send a new code' })).toBeNull();
    });

    it('sends the email code once its issuer is recorded (control)', async () => {
      const sends = recordSends();
      renderModal();
      show('issuer-email', ['email'], 'suspicious_refresh');

      await waitFor(() => expect(sends).toHaveLength(1));
      expect(screen.queryByRole('alert')).toBeNull();
    });

    it('sends no verify request and says the server changed', async () => {
      const verifies = recordVerifies(() =>
        HttpResponse.json({ error: 'Invalid MFA code' }, { status: 401 })
      );
      const user = userEvent.setup();
      renderModal();
      publish('no-issuer-verify', ['totp'], 'suspicious_refresh');

      await enterCode(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      await settle();
      expect(verifies).toEqual([]);
    });

    it('sends no SSO proof and says the server changed', async () => {
      const user = userEvent.setup();
      renderModal();
      publish('no-issuer-sso', ['totp'], 'sso_login');

      await enterCode(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      expect(completeSSOMFA).not.toHaveBeenCalled();
    });
  });

  // Both records last as long as the server keeps the challenge.
  describe('after the challenge TTL', () => {
    beforeEach(() => {
      // Only Date: MSW and waitFor keep their real timers.
      vi.useFakeTimers({ toFake: ['Date'] });
      vi.setSystemTime(new Date('2026-10-07T12:00:00Z'));
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    function advance(ms: number) {
      vi.setSystemTime(Date.now() + ms);
    }

    it('a remount sends nothing and says the server changed', async () => {
      const sends = recordSends();
      const first = renderModal();
      show('ttl-expired', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(1));
      await settle();

      first.unmount();
      advance(CHALLENGE_TTL_MS);
      renderModal();
      await screen.findByText('Enter the verification code sent to you');
      await settle();
      expect(sends, 'the remount sends nothing').toHaveLength(1);
      expect(screen.getByRole('alert')).toHaveTextContent(SERVER_CHANGED_ERROR);
      expect(challengeIssuerFor('ttl-expired')).toBeNull();
    });

    it('a remount just inside the TTL still asks once (control)', async () => {
      const sends = recordSends();
      const first = renderModal();
      show('ttl-live', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(1));
      await settle();

      first.unmount();
      advance(CHALLENGE_TTL_MS - 1);
      renderModal();
      await screen.findByText('Enter the verification code sent to you');
      await settle();
      expect(sends).toHaveLength(1);
      expect(screen.queryByRole('alert')).toBeNull();
    });

    it('prunes both records for an expired challenge when the next one is raised', async () => {
      const sends = recordSends();
      renderModal();
      show('ttl-pruned', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(1));

      advance(CHALLENGE_TTL_MS);
      show('ttl-next', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(2));
      expect(__challengeIssuerCountForTests()).toBe(1);
      expect(__signInEmailCodeSendCountForTests()).toBe(1);
    });

    it('keeps both records for a live challenge when the next one is raised (control)', async () => {
      const sends = recordSends();
      renderModal();
      show('ttl-kept', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(1));

      advance(CHALLENGE_TTL_MS - 1);
      show('ttl-kept-next', ['email'], 'suspicious_refresh');
      await waitFor(() => expect(sends).toHaveLength(2));
      expect(__challengeIssuerCountForTests()).toBe(2);
      expect(__signInEmailCodeSendCountForTests()).toBe(2);
    });
  });

  describe('an SSO challenge', () => {
    function stubSSOChallenge(token: string) {
      vi.mocked(startSSOFlow).mockResolvedValueOnce({
        kind: 'mfa_required',
        mfaChallengeToken: token,
        methods: ['email'],
        recoveryOnlyMethods: [],
        credentialOwner: 5,
      });
    }

    it('sends nothing when the selection moves as the challenge is published', async () => {
      const sends = recordSends();
      stubSSOChallenge('sso-moved-at-publish');
      const unsubscribe = switchServerWhenPublished('sso-moved-at-publish');
      const user = userEvent.setup();
      renderModal();
      const sso = renderHook(() => useSSOFlow());

      try {
        await act(async () => {
          await sso.result.current.begin('google');
        });
        await expectRefusedOnScreen(sends);
        await enterCode(user);
        await settle();
      } finally {
        unsubscribe();
      }
      expect(completeSSOMFA).not.toHaveBeenCalled();
    });

    it('sends to the server that raised it when the selection does not move (control)', async () => {
      const sends = recordSends();
      stubSSOChallenge('sso-not-moved');
      const user = userEvent.setup();
      renderModal();
      const sso = renderHook(() => useSSOFlow());

      await act(async () => {
        await sso.result.current.begin('google');
      });
      await waitFor(() => expect(sends).toHaveLength(1));
      await enterCode(user);
      await waitFor(() => expect(completeSSOMFA).toHaveBeenCalledTimes(1));
      expect(sends.map((s) => new URL(s.url).origin)).toEqual([ORIGINAL_SERVER]);
      const [, selection] = vi.mocked(completeSSOMFA).mock.calls[0];
      expect(selection.apiBase).toBe(ORIGINAL_SERVER);
    });
  });
});
