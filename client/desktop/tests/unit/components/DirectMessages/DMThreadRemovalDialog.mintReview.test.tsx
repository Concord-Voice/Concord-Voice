import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// Reproductions from the #3509 frontend review for DM Clear:
// M1 — a mint refusal with mfa_required moves the dialog to its MFA stage;
// M2 — a mint the server does not have is named as unsupported.

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

async function submitPassword(mint: () => Response) {
  server.use(
    http.post(`*${MINT_PATH}`, () => mint()),
    http.post('*/api/v1/dm/conversations/:id/clear', () =>
      HttpResponse.json(
        { error: 'Current password required to clear history', password_required: true },
        { status: 403 }
      )
    )
  );
  render(
    <DMThreadRemovalDialog
      target={{ conversation, action: 'clear' }}
      onClose={vi.fn()}
      onRemoved={vi.fn()}
    />
  );
  fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  fireEvent.change(await screen.findByLabelText('Password'), { target: { value: FIXTURE_PW } });
  fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));
}

describe('DMThreadRemovalDialog mint refusals (#3509 frontend review)', () => {
  it('M1: an mfa_required mint refusal moves to the MFA stage', async () => {
    await submitPassword(() =>
      HttpResponse.json(
        { error: 'MFA verification required', mfa_required: true, mfa_methods: ['totp'] },
        { status: 403 }
      )
    );

    expect(await screen.findByRole('textbox', { name: 'Digit 1' })).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('M2: a mint the server does not have is named as unsupported', async () => {
    await submitPassword(() => HttpResponse.json({ error: 'Not Found' }, { status: 404 }));

    expect(
      await screen.findByText("This server doesn't support this confirmation yet.")
    ).toBeInTheDocument();
  });
});
