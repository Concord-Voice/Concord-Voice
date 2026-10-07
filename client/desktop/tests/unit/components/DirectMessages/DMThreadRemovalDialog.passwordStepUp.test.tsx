import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { act, fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
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
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// DM Clear password step-up on the wire (#3509, rewritten for picker PR 2).
// The real dmVisibilityApi runs; only the network is stubbed. The account
// password goes only to POST /api/v1/auth/step-up/password, and the FIRST Clear
// request already carries { step_up_token }: with purge protection on,
// "Continue" is a local stage change, so no factor-less Clear is sent to find
// out what the account needs (the read has already said).
//
// "Mutant:" comments name the production change each test exists to turn red.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const conversation: DMConversation = {
  id: 'dm-1',
  isGroup: false,
  isPersonal: false,
  name: 'Momo',
  participants: [
    { userId: 'user-1', username: 'alice' },
    { userId: 'user-2', username: 'momo' },
  ],
  lastMessage: { content: 'hello', userId: 'user-2', createdAt: '2026-09-23T12:00:00Z' },
  unreadCount: 0,
  createdAt: '2026-09-23T11:00:00Z',
};

const READ_PATH = '*/api/v1/mfa/step-up';
const CLEAR_PATH = '*/api/v1/dm/conversations/:id/clear';
const CODE = '123456';

beforeEach(() => {
  resetAllStores();
  useDMStore.setState({
    conversations: [conversation],
    activeConversationId: conversation.id,
    removeConversation: vi.fn(),
    discardConversationView: vi.fn(),
    fetchConversations: vi.fn(async () => {}),
    leaveGroup: vi.fn(),
  });
});

/** What the account's step-up read answers: its inline methods, strongest first. */
function readAnswers(methods: string[]) {
  return http.get(READ_PATH, () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: false,
    })
  );
}

const passwordRequired = () =>
  HttpResponse.json(
    { error: 'Current password required to clear history', password_required: true },
    { status: 403 }
  );

const cleared = () =>
  HttpResponse.json({ conversation_id: 'dm-1', cleared_at: '2026-10-01T00:00:00Z' });

/** Opens the credential stage; with protection on, "Continue" sends nothing. */
async function openClear(onClose = vi.fn()) {
  render(
    <DMThreadRemovalDialog
      target={{ conversation, action: 'clear' }}
      onClose={onClose}
      onRemoved={vi.fn()}
    />
  );
  await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
  await screen.findByRole('heading', { name: 'Confirm it is you' });
  return onClose;
}

const verify = () => screen.getByRole('button', { name: 'Verify and clear' });
const enterPassword = async (value: string) =>
  fireEvent.change(await screen.findByLabelText('Password'), { target: { value } });
const enterCode = async (value = CODE) =>
  userEvent.setup().type(await screen.findByLabelText('Authenticator app code'), value);

/**
 * Stands in for the server, recording every request: the mint endpoint
 * answers a token; a clear carrying that token succeeds; one carrying
 * current_password is a 400 (T-4); one carrying a code is refused for the
 * password when `codeRefusal` says the account has no inline method.
 */
function installServer(methods: string[], codeRefusal = false): Wire[] {
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
    readAnswers(methods),
    http.post(`*${MINT_PATH}`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ step_up_token: MINTED_TOKEN, expires_in: 60 });
    }),
    http.post(CLEAR_PATH, async ({ request }) => {
      const { body } = await record(request);
      const verdict = routeVerdict(body);
      if (verdict === 'token') return cleared();
      if (verdict === 'stale-client') {
        return HttpResponse.json({ error: 'Invalid request body' }, { status: 400 });
      }
      return codeRefusal || !('mfa_code' in body) ? passwordRequired() : cleared();
    })
  );
  return wire;
}

const clears = (wire: Wire[]) => wire.filter((r) => r.path.endsWith('/clear'));

describe('DMThreadRemovalDialog Clear password step-up (#3509)', () => {
  // regression for #3509 (Codex P1)
  it('the password goes only to the mint endpoint and the first Clear carries step_up_token', async () => {
    const wire = installServer([]);
    const onClose = await openClear();
    await enterPassword(FIXTURE_PW);
    fireEvent.click(verify());

    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());

    // Mutant: a factor-less probe first (two Clear requests, the first bodyless).
    expect(clears(wire)).toHaveLength(1);
    expect
      .soft(
        clears(wire).filter((r) => 'current_password' in r.body),
        'no request to the clear route may carry current_password'
      )
      .toEqual([]);
    const mints = wire.filter((r) => r.path === MINT_PATH);
    expect.soft(mints, 'the password must be sent once, to the mint endpoint').toHaveLength(1);
    expect.soft(mints[0]?.method).toBe('POST');
    expect.soft(mints[0]?.body).toEqual({ current_password: FIXTURE_PW, purpose: 'dm.clear' });
    expect.soft(clears(wire)[0]?.body, 'Clear carries the minted token').toEqual({
      step_up_token: MINTED_TOKEN,
    });
    expect(
      JSON.stringify(clears(wire)),
      'the password text is nowhere in a Clear request'
    ).not.toContain(FIXTURE_PW);
    // The mint is sent before the Clear that spends its token.
    expect(wire.map((r) => r.path.split('/').pop())).toEqual(['password', 'clear']);
  });

  it('sends nothing but the read while the password is empty', async () => {
    const wire = installServer([]);
    await openClear();
    await screen.findByLabelText('Password');

    fireEvent.click(verify());

    expect(await screen.findByRole('alert')).toHaveTextContent('Enter your password to continue.');
    await act(async () => {});
    expect(wire).toEqual([]);
  });

  // Mutant (C2): `passwordRequired` not emptying a prefetched TOTP set. The
  // password leg would never mount, and the next submit would send the stale
  // code again instead of minting a token.
  it('a prefetched TOTP set that receives password_required moves to the password and mints a token', async () => {
    const wire = installServer(['totp'], true);
    const onClose = await openClear();
    await enterCode();
    fireEvent.click(verify());

    const password = await screen.findByLabelText('Password');
    await waitFor(() => expect(password).toHaveFocus());
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(clears(wire).map((r) => r.body)).toEqual([{ mfa_code: CODE }]);

    fireEvent.change(password, { target: { value: FIXTURE_PW } });
    fireEvent.click(verify());
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());

    expect(wire.filter((r) => r.path === MINT_PATH).map((r) => r.body)).toEqual([
      { current_password: FIXTURE_PW, purpose: 'dm.clear' },
    ]);
    expect(clears(wire).map((r) => r.body)).toEqual([
      { mfa_code: CODE },
      { step_up_token: MINTED_TOKEN },
    ]);
    expect(JSON.stringify(wire.filter((r) => r.path.endsWith('/clear')))).not.toContain(
      'current_password'
    );
  });

  // Purge protection off locally, on at the server: the factor-less Clear is
  // refused, the stage opens, and the NEXT Clear carries the token.
  it('a Clear the server refuses for the password although the setting was off still ends in a token', async () => {
    const wire = installServer([]);
    const { usePrivacyStore } = await import('@/renderer/stores/ui/privacyStore');
    usePrivacyStore.setState((s) => ({
      settings: { ...s.settings, requireAuthBeforePurge: false },
    }));
    const onClose = vi.fn();
    render(
      <DMThreadRemovalDialog
        target={{ conversation, action: 'clear' }}
        onClose={onClose}
        onRemoved={vi.fn()}
      />
    );

    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await enterPassword(FIXTURE_PW);
    fireEvent.click(verify());
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());

    expect(clears(wire).map((r) => r.body)).toEqual([{}, { step_up_token: MINTED_TOKEN }]);
  });
});
