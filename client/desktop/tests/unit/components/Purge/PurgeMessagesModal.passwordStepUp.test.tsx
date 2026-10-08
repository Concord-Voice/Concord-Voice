import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  FIXTURE_PW,
  MINTED_TOKEN,
  MINT_PATH,
  parseBody,
  routeVerdict,
  type Wire,
} from '../../../helpers/stepUpTokenWire';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';

// Reproduction for #3509 (Codex security P1), desktop half, self-purge. Design
// spec "Developer decisions, 2026-10-01", T-2, T-4, T-5: the account password
// goes only to POST /api/v1/auth/step-up/password, and the purge route is
// retried with { step_up_token }.
//
// Oracle: after a 403 password_required refusal, no purge request ever carries
// current_password; the password goes only to the mint endpoint, and the route
// is retried with step_up_token.
//
// Expected to FAIL on the current tree: the modal resends the entered password
// to the purge route. It asserts on the requests the network layer saw, so it
// names no client function that does not exist yet.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  server.use(passwordOnlyRead());
});

// The stage reads the account's inline methods. Stubbed so the result never
// depends on whatever answers an unhandled request: a password-only account,
// as the password_required challenge says.
const passwordOnlyRead = () =>
  http.get('*/api/v1/mfa/step-up', () =>
    HttpResponse.json({ methods: [], default_method: null, backup_code_available: false })
  );

const noop = () => {};

const PASSWORD_CHALLENGE = {
  error: 'Password required',
  delete_rate_limited: true,
  password_required: true,
};

/**
 * Stands in for the post-fix server on one purge route, recording every
 * request: the mint endpoint answers a token; a purge carrying step_up_token
 * succeeds; one carrying current_password is a 400 (T-4); any other is the
 * password_required refusal.
 */
function installServer(purgePath: string): Wire[] {
  const wire: Wire[] = [];
  const record = async (request: Request): Promise<Wire> => {
    const entry: Wire = {
      path: new URL(request.url).pathname,
      method: request.method,
      body: parseBody(await request.text()),
    };
    wire.push(entry);
    return entry;
  };
  server.use(
    http.post(`*${MINT_PATH}`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ step_up_token: MINTED_TOKEN, expires_in: 60 });
    }),
    http.delete(purgePath, async ({ request }) => {
      const { body } = await record(request);
      const verdict = routeVerdict(body);
      if (verdict === 'token') {
        return HttpResponse.json({ deleted_count: 2, hidden_count: 0 });
      }
      if (verdict === 'stale-client') {
        return HttpResponse.json({ error: 'Invalid request body' }, { status: 400 });
      }
      return HttpResponse.json(PASSWORD_CHALLENGE, {
        status: 403,
        headers: { 'Retry-After': '30' },
      });
    })
  );
  return wire;
}

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

const cases = [
  [
    'channel self-purge',
    '*/api/v1/channels/:id/messages',
    startChannelPurge,
    'messages.channel_purge',
  ],
  ['server self-purge', '*/api/v1/servers/:id/messages', startServerPurge, 'messages.server_purge'],
] as const;

describe('PurgeMessagesModal password step-up (#3509)', () => {
  // regression for #3509 (Codex P1)
  it.each(cases)(
    '%s: the password goes only to the mint endpoint and the route is retried with step_up_token',
    async (_name, purgePath, start, purpose) => {
      const wire = installServer(purgePath);
      const user = await start();

      // Precondition, and the arm being reached: the first purge was refused
      // with password_required, which is what puts the password field on screen.
      const field = await screen.findByLabelText('Password');
      await user.type(field, FIXTURE_PW);
      await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

      const purges = () => wire.filter((r) => r.method === 'DELETE');
      await waitFor(() => expect(purges().length).toBeGreaterThanOrEqual(2));

      expect
        .soft(
          purges().filter((r) => 'current_password' in r.body),
          'no request to a purge route may carry current_password'
        )
        .toEqual([]);
      const mints = wire.filter((r) => r.path === MINT_PATH);
      expect.soft(mints, 'the password must be sent once, to the mint endpoint').toHaveLength(1);
      expect.soft(mints[0]?.method).toBe('POST');
      expect.soft(mints[0]?.body).toEqual({ current_password: FIXTURE_PW, purpose });
      expect.soft(purges().at(-1)?.body, 'the route is retried with the minted token').toEqual({
        range: '7d',
        include_pinned: false,
        step_up_token: MINTED_TOKEN,
      });
    }
  );
});
