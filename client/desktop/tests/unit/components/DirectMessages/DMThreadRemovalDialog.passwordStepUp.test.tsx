import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen, waitFor } from '../../../test-utils';
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

// Reproduction for #3509 (Codex security P1), desktop half, DM Clear. Design
// spec "Developer decisions, 2026-10-01", T-1, T-2, T-4, T-5: the account
// password goes only to POST /api/v1/auth/step-up/password, and Clear is
// retried with { step_up_token }.
//
// Oracle: after a 403 password_required refusal, no request to the clear route
// ever carries current_password; the password goes only to the mint endpoint,
// and the route is retried with step_up_token.
//
// Expected to FAIL on the current tree: the dialog resends the entered password
// to the clear route. The real dmVisibilityApi runs; only the network is
// stubbed, so the test names no client function that does not exist yet.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const CLEAR_PATH = '*/api/v1/dm/conversations/:id/clear';

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

/**
 * Stands in for the post-fix server, recording every request: the mint
 * endpoint answers a token; a clear carrying step_up_token succeeds; one
 * carrying current_password is a 400 (T-4); any other is the password_required
 * refusal.
 */
function installServer(): Wire[] {
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
    http.post(CLEAR_PATH, async ({ request }) => {
      const { body } = await record(request);
      const verdict = routeVerdict(body);
      if (verdict === 'token') {
        return HttpResponse.json({ conversation_id: 'dm-1', cleared_at: '2026-10-01T00:00:00Z' });
      }
      if (verdict === 'stale-client') {
        return HttpResponse.json({ error: 'Invalid request body' }, { status: 400 });
      }
      return HttpResponse.json(
        { error: 'Current password required to clear history', password_required: true },
        { status: 403 }
      );
    })
  );
  return wire;
}

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

describe('DMThreadRemovalDialog Clear password step-up (#3509)', () => {
  // regression for #3509 (Codex P1)
  it('the password goes only to the mint endpoint and Clear is retried with step_up_token', async () => {
    const wire = installServer();
    render(
      <DMThreadRemovalDialog
        target={{ conversation, action: 'clear' }}
        onClose={vi.fn()}
        onRemoved={vi.fn()}
      />
    );

    // Precondition, and the arm being reached: the first Clear was refused with
    // password_required, which is what puts the password field on screen.
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    const field = await screen.findByLabelText('Password');
    fireEvent.change(field, { target: { value: FIXTURE_PW } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));

    const clears = () => wire.filter((r) => r.path.endsWith('/clear'));
    await waitFor(() => expect(clears().length).toBeGreaterThanOrEqual(2));

    expect
      .soft(
        clears().filter((r) => 'current_password' in r.body),
        'no request to the clear route may carry current_password'
      )
      .toEqual([]);
    const mints = wire.filter((r) => r.path === MINT_PATH);
    expect.soft(mints, 'the password must be sent once, to the mint endpoint').toHaveLength(1);
    expect.soft(mints[0]?.method).toBe('POST');
    expect.soft(mints[0]?.body).toEqual({ current_password: FIXTURE_PW, purpose: 'dm.clear' });
    expect.soft(clears().at(-1)?.body, 'Clear is retried with the minted token').toEqual({
      step_up_token: MINTED_TOKEN,
    });
  });
});
