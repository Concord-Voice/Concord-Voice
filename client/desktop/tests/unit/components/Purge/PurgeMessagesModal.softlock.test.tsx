import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
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
});

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

async function typeCode(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('textbox', { name: 'Digit 1' }));
  await user.keyboard(CODE);
}

const submit = () => screen.getByRole('button', { name: 'Confirm and Purge' });
const digitValues = () =>
  screen.getAllByRole('textbox').map((el) => (el as HTMLInputElement).value);

describe('PurgeMessagesModal — soft-lock stage', () => {
  it('a soft-lock 403 opens the confirm stage instead of a result', async () => {
    scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE)]);
    await startChannelPurge();

    expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toHaveFocus();
    expect(
      screen.getByText("You've deleted several messages quickly. Confirm it's you to keep going.")
    ).toBeInTheDocument();
    expect(screen.getByText('MFA Verification')).toBeInTheDocument();
    // Not an outcome: no permission copy, no Done.
    expect(screen.queryByText(/you may not have permission/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    expect(submit()).toBeDisabled();
  });

  it('re-sends the same range with mfa_code only, then reports the purge', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(MFA_CHALLENGE), ok(4)]);
    const user = await startChannelPurge();
    await screen.findByText('MFA Verification');

    await typeCode(user);
    expect(submit()).toBeEnabled();
    await user.click(submit());

    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(bodies).toEqual([{ range: '7d' }, { range: '7d', mfa_code: CODE }]);
  });

  // Rewritten for #3509: the password goes only to the mint endpoint, and the
  // purge is retried with the token it returned.
  it('a password challenge shows a password field and re-sends a step-up token only', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(PASSWORD_CHALLENGE), ok(2)]);
    const mints = scriptedMint(() =>
      HttpResponse.json({ step_up_token: 'minted-token', expires_in: 60 })
    );
    const user = await startChannelPurge();

    const field = await screen.findByLabelText('Password');
    expect(field).toHaveAttribute('type', 'password');
    expect(screen.queryByText('MFA Verification')).not.toBeInTheDocument();
    expect(submit()).toBeDisabled();

    await user.type(field, FIXTURE_PW);
    await user.click(submit());

    expect(await screen.findByText('Purged 2 messages.')).toBeInTheDocument();
    expect(mints).toEqual([{ current_password: FIXTURE_PW, purpose: 'messages.channel_purge' }]);
    expect(bodies).toEqual([{ range: '7d' }, { range: '7d', step_up_token: 'minted-token' }]);
  });

  it('the server route re-sends to the server endpoint', async () => {
    const bodies = scriptedPurge(SERVER_PATH, [challenge(MFA_CHALLENGE), ok(0)]);
    const user = await startServerPurge();
    await screen.findByText('MFA Verification');

    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByText(/Messages purged\./)).toBeInTheDocument();
    expect(bodies).toEqual([{ range: '7d' }, { range: '7d', mfa_code: CODE }]);
  });

  it('an invalid code remounts the prompt empty, shows the error and disables Confirm', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge(MFA_CHALLENGE),
      challenge({ error: 'Invalid MFA code', delete_rate_limited: true }),
    ]);
    const user = await startChannelPurge();
    await screen.findByText('MFA Verification');
    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent(
      "That didn't work. Try again with a new code."
    );
    expect(digitValues()).toEqual(['', '', '', '', '', '']);
    expect(submit()).toBeDisabled();
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
    await screen.findByText('MFA Verification');
    await typeCode(user);
    await user.click(submit());
    await screen.findByRole('alert');

    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByText('Purged 1 message.')).toBeInTheDocument();
    expect(bodies.at(-1)).toEqual({ range: '7d', mfa_code: CODE });
  });

  // Rewritten for #3509: an invalid password is the mint's refusal now, and
  // the purge route is asked only once.
  it('an invalid password clears the field and keeps the challenge', async () => {
    const bodies = scriptedPurge(CHANNEL_PATH, [challenge(PASSWORD_CHALLENGE)]);
    scriptedMint(() => HttpResponse.json({ error: 'Invalid password' }, { status: 403 }));
    const user = await startChannelPurge();
    await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
    expect(submit()).toBeDisabled();
    expect(bodies).toEqual([{ range: '7d' }]);
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
    await screen.findByText('MFA Verification');
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
    await screen.findByText('MFA Verification');
    await typeCode(user);
    await user.click(submit());

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Purge limit reached. Try again in 15 minutes.'
    );
  });

  it('a flagged non-challenge 403 ends in a result that says nothing was purged', async () => {
    scriptedPurge(CHANNEL_PATH, [
      challenge({
        error: 'Set up an authenticator app or security key to do this.',
        delete_rate_limited: true,
        code: 'mfa_enrollment_required',
      }),
    ]);
    await startChannelPurge();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'Set up an authenticator app or security key to do this. Nothing was purged.'
    );
    expect(screen.queryByText('MFA Verification')).not.toBeInTheDocument();
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
    await screen.findByText('MFA Verification');

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(closed).toEqual([true]);
    expect(bodies).toHaveLength(1);
  });

  describe('purpose', () => {
    // A security-key token is minted for exactly one route: the begin request
    // carries the purpose, so it is the observable place the choice lands.
    async function beginPurpose(
      start: () => Promise<ReturnType<typeof userEvent.setup>>,
      path: string
    ) {
      const purposes: unknown[] = [];
      server.use(
        http.post('*/api/v1/mfa/webauthn/verify-inline/begin', async ({ request }) => {
          purposes.push(((await request.json()) as { purpose?: unknown }).purpose);
          return HttpResponse.json({ error: 'stop here' }, { status: 400 });
        })
      );
      scriptedPurge(path, [challenge({ ...MFA_CHALLENGE, methods: ['webauthn'] })]);
      const user = await start();
      await user.click(await screen.findByRole('button', { name: 'Verify with security key' }));
      await waitFor(() => expect(purposes).toHaveLength(1));
      return purposes[0];
    }

    it('the channel route binds the token to messages.channel_purge', async () => {
      expect(await beginPurpose(startChannelPurge, CHANNEL_PATH)).toBe('messages.channel_purge');
    });

    it('the server route binds the token to messages.server_purge', async () => {
      expect(await beginPurpose(startServerPurge, SERVER_PATH)).toBe('messages.server_purge');
    });
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
    await screen.findByText('MFA Verification');

    rerender(modal(false));
    rerender(modal(true));

    expect(screen.queryByText('MFA Verification')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('');
  });
});
