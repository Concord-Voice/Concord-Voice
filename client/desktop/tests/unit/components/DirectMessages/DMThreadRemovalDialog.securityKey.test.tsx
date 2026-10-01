import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// Codex on #3509: DM Clear's MFA stage dropped the methods the server named
// and rendered a numeric code field only, so an account whose one inline
// factor is a security key could never finish a Clear — whether Clear itself
// asked for MFA or the password mint did.

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

const CLEAR_PATH = '*/api/v1/dm/conversations/:id/clear';
const BEGIN_PATH = '*/api/v1/mfa/webauthn/verify-inline/begin';

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

function renderClear() {
  render(
    <DMThreadRemovalDialog
      target={{ conversation, action: 'clear' }}
      onClose={vi.fn()}
      onRemoved={vi.fn()}
    />
  );
  fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
}

function clearAsksForMfa(methods: string[]) {
  server.use(
    http.post(CLEAR_PATH, () =>
      HttpResponse.json(
        { error: 'MFA verification required', mfa_required: true, methods },
        { status: 403 }
      )
    )
  );
}

describe('DMThreadRemovalDialog security-key confirmation (#3509 review)', () => {
  it("offers the security key when Clear's own refusal names webauthn", async () => {
    clearAsksForMfa(['webauthn']);
    renderClear();

    expect(
      await screen.findByRole('button', { name: 'Verify with security key' })
    ).toBeInTheDocument();
  });

  it('begins the security-key ceremony for dm.clear, the one route that can spend it', async () => {
    clearAsksForMfa(['webauthn']);
    let beginBody: unknown;
    server.use(
      http.post(BEGIN_PATH, async ({ request }) => {
        beginBody = await request.json();
        return HttpResponse.json({ error: 'stop here' }, { status: 400 });
      })
    );
    renderClear();

    fireEvent.click(await screen.findByRole('button', { name: 'Verify with security key' }));
    await vi.waitFor(() => expect(beginBody).toEqual({ purpose: 'dm.clear' }));
  });

  it('offers the security key when the password mint names webauthn', async () => {
    server.use(
      http.post(CLEAR_PATH, () =>
        HttpResponse.json(
          { error: 'Current password required to clear history', password_required: true },
          { status: 403 }
        )
      ),
      http.post(`*${MINT_PATH}`, () =>
        HttpResponse.json(
          { error: 'MFA verification required', mfa_required: true, mfa_methods: ['webauthn'] },
          { status: 403 }
        )
      )
    );
    renderClear();
    fireEvent.change(await screen.findByLabelText('Password'), { target: { value: FIXTURE_PW } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));

    expect(
      await screen.findByRole('button', { name: 'Verify with security key' })
    ).toBeInTheDocument();
  });

  // Control: the harness reaches the MFA stage, and an authenticator-app
  // account is offered a code, not a security key.
  it('offers a code and no security key when only totp is named', async () => {
    clearAsksForMfa(['totp']);
    renderClear();

    expect((await screen.findAllByRole('textbox')).length).toBeGreaterThan(0);
    expect(screen.queryByRole('button', { name: 'Verify with security key' })).toBeNull();
  });
});
