import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// Reproductions from the #3509 frontend review for DM Clear, on the shared
// stage:
// M1 - a mint refusal with mfa_required moves the dialog to its MFA stage;
// M2 - a mint the server does not have is named as unsupported.
// Plus the D2 pin: a refusal that names NO usable method is the
// no-usable-method state, never a substituted code box.

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

async function submitPassword(mint: () => Response) {
  server.use(
    readAnswers([]),
    http.post(`*${MINT_PATH}`, () => mint())
  );
  await openClear();
  await enterPassword(FIXTURE_PW);
  fireEvent.click(verify());
}

const NO_USABLE =
  "Your account's verification method can't be used here. Add an authenticator app or security key in Settings.";

describe('DMThreadRemovalDialog mint refusals (#3509 frontend review)', () => {
  it('M1: an mfa_required mint refusal moves to the MFA stage', async () => {
    await submitPassword(() =>
      HttpResponse.json(
        { error: 'MFA verification required', mfa_required: true, mfa_methods: ['totp'] },
        { status: 403 }
      )
    );

    const code = await screen.findByLabelText('Authenticator app code');
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    await waitFor(() => expect(code).toHaveFocus());
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Enter the 6-digit code from your authenticator app to continue.'
    );
  });

  it('M1: the code typed there is what the next Clear carries', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post(CLEAR_PATH, async ({ request }) => {
        bodies.push(await request.json());
        return cleared();
      })
    );
    await submitPassword(() =>
      HttpResponse.json(
        { error: 'MFA verification required', mfa_required: true, mfa_methods: ['totp'] },
        { status: 403 }
      )
    );

    await enterCode();
    fireEvent.click(verify());

    await waitFor(() => expect(bodies).toEqual([{ mfa_code: CODE }]));
  });

  it('M2: a mint the server does not have is named as unsupported', async () => {
    await submitPassword(() => HttpResponse.json({ error: 'Not Found' }, { status: 404 }));

    expect(
      await screen.findByText("This server doesn't support this confirmation yet.")
    ).toBeInTheDocument();
  });

  // Mutant (D2): reinstating the ['totp'] fallback for a refusal naming nothing.
  it.each([
    ['names no methods', {}],
    ['names an empty list', { mfa_methods: [] }],
    ['names only email', { mfa_methods: ['email'] }],
  ])(
    'a mint refusal that %s is the no-usable-method state, not a code box',
    async (_name, extra) => {
      await submitPassword(() =>
        HttpResponse.json(
          { error: 'MFA verification required', mfa_required: true, ...extra },
          { status: 403 }
        )
      );

      expect(await screen.findByRole('status')).toHaveTextContent(NO_USABLE);
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      await waitFor(() =>
        expect(screen.getByRole('heading', { name: 'Confirm it is you' })).toHaveFocus()
      );
    }
  );

  it("Clear's own mfa_required with no methods is the same state, not a code box", async () => {
    server.use(
      readAnswers(['totp']),
      http.post(CLEAR_PATH, () =>
        HttpResponse.json(
          { error: 'MFA verification required', mfa_required: true },
          { status: 403 }
        )
      )
    );
    await openClear();
    await enterCode();
    fireEvent.click(verify());

    expect(await screen.findByRole('status')).toHaveTextContent(NO_USABLE);
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
  });
});
