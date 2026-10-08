import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';

// #3455: the 'softlock' stage of a channel or server SELF-purge. The first
// purge request trips the delete-rate soft-lock; the modal then asks for the
// factor the server named and re-sends the same purge with it.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  // The stage reads the account's methods; the refusal that opened it seeds the
  // same set, so a read that agrees changes nothing (G2).
  server.use(readAnswers(['totp']));
});

const READ_PATH = '*/api/v1/mfa/step-up';
function readAnswers(methods: string[], backup = false) {
  return http.get(READ_PATH, () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: backup,
    })
  );
}
const ENROLLMENT_COPY = 'Set up an authenticator app or security key in Settings to do this.';

const noop = () => {};
const CODE = '123456';
// Bound to a constant: detect-secrets flags keyword/literal adjacency.
const FIXTURE_PW = 'hunter2-fixture';

const MFA_CHALLENGE = {
  error: 'Confirm it is you',
  delete_rate_limited: true,
  mfa_required: true,
  methods: ['totp'],
};
const PASSWORD_CHALLENGE = {
  error: 'Password required',
  delete_rate_limited: true,
  password_required: true,
};

const CHANNEL_PATH = '*/api/v1/channels/:id/messages';
const SERVER_PATH = '*/api/v1/servers/:id/messages';

type Sent = Record<string, unknown>;

/** Answers each successive purge request with the next response, recording bodies. */
function scriptedPurge(path: string, responses: Array<() => Response>): Sent[] {
  const bodies: Sent[] = [];
  server.use(
    http.delete(path, async ({ request }) => {
      bodies.push((await request.json()) as Sent);
      const next = responses[Math.min(bodies.length - 1, responses.length - 1)];
      return next();
    })
  );
  return bodies;
}

const challenge = (body: object) => () => HttpResponse.json(body, { status: 403 });

/**
 * Stands up the password step-up mint (#3509) with one scripted answer and
 * records each body it receives.
 */
function scriptedMint(answer: () => Response): Array<Record<string, unknown>> {
  const bodies: Array<Record<string, unknown>> = [];
  server.use(
    http.post('*/api/v1/auth/step-up/password', async ({ request }) => {
      bodies.push((await request.json()) as Record<string, unknown>);
      return answer();
    })
  );
  return bodies;
}
const ok =
  (n = 4) =>
  () =>
    HttpResponse.json({ deleted_count: n, hidden_count: 0 });

async function startChannelPurge() {
  const user = userEvent.setup();
  render(
    <PurgeMessagesModal context="channel" isOpen scopeId="c1" scopeName="general" onClose={noop} />
  );
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  return user;
}

async function startServerPurge() {
  const user = userEvent.setup();
  render(
    <PurgeMessagesModal context="server" isOpen scopeId="s1" scopeName="Guild" onClose={noop} />
  );
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  return user;
}

const codeInput = () => screen.findByLabelText('Authenticator app code');
async function typeCode(user: ReturnType<typeof userEvent.setup>, code = CODE) {
  await user.type(await codeInput(), code);
}

const submit = () => screen.getByRole('button', { name: /^(Confirm and Purge|Waiting|Purging)/ });
const submitIsInert = () => expect(submit()).toHaveAttribute('aria-disabled', 'true');
const submitIsLive = () => expect(submit()).not.toHaveAttribute('aria-disabled');

describe('PurgeMessagesModal — soft-lock stage', () => {
  it('a soft-lock 403 opens the confirm stage instead of a result', async () => {
    scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE)]);
    await startChannelPurge();

    expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toHaveFocus();
    expect(
      screen.getByText("You've deleted several messages quickly. Confirm it's you to keep going.")
    ).toBeInTheDocument();
    expect(await codeInput()).toBeInTheDocument();
    // Not an outcome: no permission copy, no Done.
    expect(screen.queryByText(/you may not have permission/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    submitIsInert();
  });

  it('re-sends the same range with mfa_code only, then reports the purge', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE), ok(4)]);
    const user = await startChannelPurge();

    await typeCode(user);
    submitIsLive();
    await user.click(submit());

    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, mfa_code: CODE },
    ]);
  });

  // The #17 twin: a backup code was refused on the soft-lock before the picker.
  it('a backup code is accepted and travels as mfa_code', async () => {
    server.use(readAnswers(['totp'], true));
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE), ok(1)]);
    const user = await startChannelPurge();

    await user.click(await screen.findByRole('button', { name: 'Use a backup code instead' }));
    await user.type(await screen.findByLabelText('Backup code'), 'abcd1234');
    await user.click(submit());

    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
    expect(bodies.at(-1)).toEqual({ range: '7d', include_pinned: false, mfa_code: 'abcd1234' });
  });

  // Rewritten for #3509: the password goes only to the mint endpoint, and the
  // purge is retried with the token it returned.
  it('a password challenge shows a password field and re-sends a step-up token only', async () => {
    server.use(readAnswers([]));
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(PASSWORD_CHALLENGE), ok(2)]);
    const mints = scriptedMint(() =>
      HttpResponse.json({ step_up_token: 'minted-token', expires_in: 60 })
    );
    const user = await startChannelPurge();

    const field = await screen.findByLabelText('Password');
    expect(field).toHaveAttribute('type', 'password');
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    submitIsInert();

    await user.type(field, FIXTURE_PW);
    await user.click(submit());

    expect(await screen.findByText('Purged 2 messages.')).toBeInTheDocument();
    expect(mints).toEqual([{ current_password: FIXTURE_PW, purpose: 'messages.channel_purge' }]);
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, step_up_token: 'minted-token' },
    ]);
    expect(JSON.stringify(bodies)).not.toContain(FIXTURE_PW);
  });

  it('the server route re-sends to the server endpoint', async () => {
    const bodies = scriptedPurge(SERVER_PATH, [challenge(MFA_CHALLENGE), ok(0)]);
    const user = await startServerPurge();

    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByText(/Messages purged\./)).toBeInTheDocument();
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, mfa_code: CODE },
    ]);
  });

  it('an invalid code is worded in place: the input is emptied and focused, and Confirm is inert', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      challenge({ error: 'Invalid MFA code', delete_rate_limited: true }),
    ]);
    const user = await startChannelPurge();
    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByText(/That code didn't work/)).toBeInTheDocument();
    const input = await codeInput();
    await waitFor(() => expect(input).toHaveFocus());
    expect(input).toHaveValue('');
    submitIsInert();
    // Still on the challenge: nothing was reported as an outcome.
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
  });

  it('a second attempt after an invalid code sends the NEW code', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      challenge({ error: 'Invalid MFA code', delete_rate_limited: true }),
      ok(1),
    ]);
    const user = await startChannelPurge();
    await typeCode(user, '111111');
    await user.click(submit());
    await screen.findByText(/That code didn't work/);

    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
    expect(bodies.at(-1)).toEqual({ range: '7d', include_pinned: false, mfa_code: CODE });
  });

  // Rewritten for #3509: an invalid password is the mint's refusal now, and
  // the purge route is asked only once.
  it('an invalid password clears the field and keeps the challenge', async () => {
    server.use(readAnswers([]));
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(PASSWORD_CHALLENGE)]);
    scriptedMint(() => HttpResponse.json({ error: 'Invalid password' }, { status: 403 }));
    const user = await startChannelPurge();
    await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
    submitIsInert();
    expect(bodies).toEqual([{ range: '7d', include_pinned: false }]);
  });

  // TA3: the retry's route refused the minted token (#3509). The stage stays,
  // and the expiry sits on the password field rather than in a result.
  it('a refused token on the retry shows the expiry on the emptied password field', async () => {
    const EXPIRED_COPY = 'Your confirmation expired. Enter your password again.';
    server.use(readAnswers([]));
    const bodies = scriptedPurge(CHANNEL_PATH, [
      challenge(PASSWORD_CHALLENGE),
      challenge({ ...PASSWORD_CHALLENGE, error: EXPIRED_COPY, step_up_token_invalid: true }),
    ]);
    scriptedMint(() => HttpResponse.json({ step_up_token: 'minted-token', expires_in: 60 }));
    const user = await startChannelPurge();
    await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await user.click(submit());

    const field = await screen.findByLabelText('Password');
    await waitFor(() => expect(field).toHaveAccessibleDescription(EXPIRED_COPY));
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveValue('');
    expect(field).toHaveFocus();
    expect(screen.getAllByText(EXPIRED_COPY)).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    expect(bodies).toEqual([
      { range: '7d', include_pinned: false },
      { range: '7d', include_pinned: false, step_up_token: 'minted-token' },
    ]);
  });

  it('a spent verification budget ends in a result that says nothing was purged', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      () =>
        HttpResponse.json(
          { error: 'Too many verification attempts', step_up_budget_exhausted: true },
          { status: 429, headers: { 'Retry-After': '300' } }
        ),
    ]);
    const user = await startChannelPurge();
    await typeCode(user);
    await user.click(submit());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Too many verification attempts. Nothing was purged.');
    expect(alert).toHaveTextContent('Try again in 5 minutes.');
  });

  it('a plain 429 on the retry is still the purge limit, not a verification limit', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      () =>
        HttpResponse.json(
          { error: 'Rate limit exceeded' },
          { status: 429, headers: { 'Retry-After': '900' } }
        ),
    ]);
    const user = await startChannelPurge();
    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Purge limit reached. Try again in 15 minutes.'
    );
  });

  it('a negative Retry-After on the retry shows no countdown', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      () =>
        HttpResponse.json(
          { error: 'Rate limit exceeded' },
          { status: 429, headers: { 'Retry-After': '-30' } }
        ),
    ]);
    const user = await startChannelPurge();
    await typeCode(user);
    await user.click(submit());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Purge limit reached');
    expect(alert).not.toHaveTextContent(/\d+ (minute|second)/);
  });

  it('a flagged 403 the fields cannot answer ends in a result that says nothing was purged', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge({ error: 'The lock says no', delete_rate_limited: true }),
    ]);
    await startChannelPurge();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('The lock says no Nothing was purged.');
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
  });

  // E8 and the #17 loop, on the purge soft-lock: the stage must end in the
  // enrolment state, never a result carrying a Retry or a countdown.
  describe.each([
    [
      'with delete_rate_limited and a Retry-After',
      { delete_rate_limited: true, mfa_enrollment_required: true },
      { 'Retry-After': '60' },
    ],
    [
      'with delete_rate_limited and no Retry-After',
      { delete_rate_limited: true, mfa_enrollment_required: true },
      {},
    ],
  ])('enrolment %s', (_label, wire, headers) => {
    const enrolment = () =>
      HttpResponse.json(
        { error: 'Set up an authenticator app or security key to do this.', ...wire },
        { status: 403, headers }
      );

    it.each([
      ['channel', startChannelPurge, CHANNEL_PATH],
      ['server', startServerPurge, SERVER_PATH],
    ] as const)('the %s route opens the enrolment state, not a result', async (_c, start, path) => {
      const bodies = scriptedPurge(path, [enrolment]);
      await start();

      expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
      expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
      expect(screen.getByRole('dialog')).not.toHaveTextContent(/try again|\d+ (second|minute)/i);
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(bodies).toHaveLength(1);
    });

    it('its primary can send nothing', async () => {
      const bodies = scriptedPurge(CHANNEL_PATH, [enrolment]);
      const user = await startChannelPurge();
      await screen.findByText(ENROLLMENT_COPY);

      submitIsInert();
      await user.click(submit());

      expect(bodies).toHaveLength(1);
    });

    it('Cancel still closes it', async () => {
      scriptedPurge(CHANNEL_PATH, [enrolment]);
      const closed: boolean[] = [];
      const user = userEvent.setup();
      render(
        <PurgeMessagesModal
          context="channel"
          isOpen
          scopeId="c1"
          scopeName="general"
          onClose={() => closed.push(true)}
        />
      );
      await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
      await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
      await screen.findByText(ENROLLMENT_COPY);

      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(closed).toEqual([true]);
    });
  });

  it('an enrolment answer to the retry ends the stage the same way', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      challenge({ delete_rate_limited: true, mfa_enrollment_required: true }),
    ]);
    const user = await startChannelPurge();
    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByText(ENROLLMENT_COPY)).toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('an enrolment 403 on the FIRST purge without delete_rate_limited is #16’s case: a plain result', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge({ error: 'Set up an app', mfa_enrollment_required: true }),
    ]);
    await startChannelPurge();

    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText(ENROLLMENT_COPY)).not.toBeInTheDocument();
  });

  // G2: the refusal that opened the stage already named the methods.
  it('a failed read keeps the methods the refusal named', async () => {
    server.use(http.get(READ_PATH, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE), ok(3)]);
    const user = await startChannelPurge();

    await typeCode(user);
    expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
    await user.click(submit());

    expect(await screen.findByText('Purged 3 messages.')).toBeInTheDocument();
    expect(bodies.at(-1)).toEqual({ range: '7d', include_pinned: false, mfa_code: CODE });
  });

  // D7: nothing left, so nothing is shown and nothing is called a network failure.
  describe('a request that never left (D7)', () => {
    let spy: ReturnType<typeof vi.spyOn>;

    /** Rejects the request whose URL matches `match` (in order) with an AbortError. */
    function abortWhen(match: (url: string, method: string) => boolean) {
      // Captured now, after MSW patched fetch, so the pass-through still hits the stand-in server.
      const realFetch = globalThis.fetch;
      spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
        const url = input instanceof Request ? input.url : String(input);
        const method = (
          init?.method ?? (input instanceof Request ? input.method : 'GET')
        ).toUpperCase();
        if (match(url, method)) throw new DOMException('aborted', 'AbortError');
        return realFetch(input, init);
      });
    }

    afterEach(() => {
      spy?.mockRestore();
    });

    // The password is dropped as it is sent (#3509: it leaves component state
    // at the mint), so an exchange that never left cannot keep it; what D7
    // promises is that nothing is SAID: no error, no result, the stage stays.
    it('an unsent password exchange shows no password error and stays in the stage', async () => {
      server.use(readAnswers([]));
      const bodies = scriptedPurge(CHANNEL_PATH, [challenge(PASSWORD_CHALLENGE), ok(1)]);
      abortWhen((url) => url.includes('/auth/step-up/password'));
      const user = await startChannelPurge();
      await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
      await user.click(submit());

      await waitFor(() =>
        expect(spy).toHaveBeenCalledWith(
          expect.stringContaining('/auth/step-up/password'),
          expect.anything()
        )
      );
      expect(await screen.findByLabelText('Password')).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(
        screen.queryByText(/couldn't check your password|not correct|network|connection/i)
      ).not.toBeInTheDocument();
      expect(screen.getByLabelText('Password')).not.toHaveAttribute('aria-invalid');
      expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
      expect(screen.getByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
      // Only the unanswered first purge ever reached the server.
      expect(bodies).toEqual([{ range: '7d', include_pinned: false }]);
    });

    it('an unsent retry with a code shows nothing and keeps the code', async () => {
      const bodies = scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE), ok(1)]);
      const user = await startChannelPurge();
      await typeCode(user);
      abortWhen((url, method) => method === 'DELETE' && url.includes('/channels/'));
      await user.click(submit());

      await waitFor(() => submitIsLive());
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.queryByText(/network|connection|couldn't/i)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
      expect(await codeInput()).toHaveValue(CODE);
      expect(bodies).toHaveLength(1);
    });

    it('an unsent FIRST purge leaves the configure stage as it was, with no result', async () => {
      abortWhen((url, method) => method === 'DELETE' && url.includes('/channels/'));
      await startChannelPurge();

      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeEnabled()
      );
      expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('7d');
    });
  });

  it('Cancel closes the dialog without sending anything further', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE)]);
    const closed: boolean[] = [];
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={() => closed.push(true)}
      />
    );
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await codeInput();

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(closed).toEqual([true]);
    expect(bodies).toHaveLength(1);
  });

  describe('security key', () => {
    const BEGIN = '*/api/v1/mfa/webauthn/verify-inline/begin';
    const FINISH = '*/api/v1/mfa/webauthn/verify-inline/finish';
    const WEBAUTHN_TOKEN = 'webauthn-inline-token';

    beforeEach(() => {
      Object.defineProperty(navigator, 'credentials', {
        value: {
          get: vi.fn().mockResolvedValue({
            id: 'credential-id',
            rawId: new Uint8Array([1, 2, 3]).buffer,
            type: 'public-key',
            response: {
              authenticatorData: new Uint8Array([10, 20]).buffer,
              clientDataJSON: new Uint8Array([30, 40]).buffer,
              signature: new Uint8Array([50, 60]).buffer,
              userHandle: null,
            },
          }),
        },
        writable: true,
        configurable: true,
      });
    });

    function ceremony(begins: unknown[]) {
      server.use(
        readAnswers(['webauthn']),
        http.post(BEGIN, async ({ request }) => {
          begins.push(((await request.json()) as { purpose?: unknown }).purpose);
          return HttpResponse.json({
            publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] },
          });
        }),
        http.post(FINISH, () => HttpResponse.json({ mfa_token: WEBAUTHN_TOKEN }))
      );
    }

    // A security-key token is minted for exactly one route: the begin request
    // carries the purpose, so it is the observable place the choice lands.
    it.each([
      ['channel', startChannelPurge, CHANNEL_PATH, 'messages.channel_purge'],
      ['server', startServerPurge, SERVER_PATH, 'messages.server_purge'],
    ] as const)(
      'the %s route binds the token to %s and the token reaches the purge body',
      async (_c, start, path, purpose) => {
        const begins: unknown[] = [];
        ceremony(begins);
        const bodies = scriptedPurge(path, [
          challenge({ ...MFA_CHALLENGE, methods: ['webauthn'] }),
          ok(1),
        ]);
        const user = await start();
        await screen.findByText('Passkey or security key');

        await user.click(submit());

        await waitFor(() => expect(bodies).toHaveLength(2));
        expect(begins).toEqual([purpose]);
        expect(bodies[1]).toEqual({ range: '7d', include_pinned: false, mfa_code: WEBAUTHN_TOKEN });
      }
    );
  });

  it('reopening after a soft-lock returns to the configure stage', async () => {
    scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE)]);
    const user = userEvent.setup();
    const modal = (isOpen: boolean) => (
      <PurgeMessagesModal
        context="channel"
        isOpen={isOpen}
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    const { rerender } = render(modal(true));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await codeInput();

    rerender(modal(false));
    rerender(modal(true));

    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('');
  });
});
