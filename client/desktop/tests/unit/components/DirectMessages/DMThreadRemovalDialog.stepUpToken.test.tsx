import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// DM Clear's password field shows the mint's refusals (#3509), and Clear's own
// refused-token answer re-prompts with the expiry copy.

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

function stand(mint: () => Response, clearAfterToken: () => Response) {
  let clears = 0;
  server.use(
    http.post(`*${MINT_PATH}`, () => mint()),
    http.post('*/api/v1/dm/conversations/:id/clear', () => {
      clears += 1;
      return clears === 1
        ? HttpResponse.json(
            { error: 'Current password required to clear history', password_required: true },
            { status: 403 }
          )
        : clearAfterToken();
    })
  );
  return () => clears;
}

async function submitPassword() {
  render(
    <DMThreadRemovalDialog
      target={{ conversation, action: 'clear' }}
      onClose={vi.fn()}
      onRemoved={vi.fn()}
    />
  );
  fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  const field = await screen.findByLabelText('Password');
  fireEvent.change(field, { target: { value: FIXTURE_PW } });
  fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));
}

describe('DMThreadRemovalDialog step-up token (#3509)', () => {
  it('the account lockout at the mint lands on the password field, and Clear is not retried', async () => {
    const clears = stand(
      () =>
        HttpResponse.json(
          { error_code: 'account_locked' },
          { status: 423, headers: { 'Retry-After': '60' } }
        ),
      () => HttpResponse.json({}, { status: 500 })
    );

    await submitPassword();

    expect(
      await screen.findByText('Too many attempts. Try again in 60 seconds.')
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(clears()).toBe(1);
  });

  it("Clear's refused token re-prompts with the expiry copy", async () => {
    stand(
      () => HttpResponse.json({ step_up_token: 'stale', expires_in: 60 }),
      () =>
        HttpResponse.json(
          {
            error: 'Your confirmation expired. Enter your password again.',
            password_required: true,
            step_up_token_invalid: true,
          },
          { status: 403 }
        )
    );

    await submitPassword();

    expect(
      await screen.findByText('Your confirmation expired. Enter your password again.')
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveValue('');
  });
});
