import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// DM Clear's password field shows the mint's refusals (#3509), and Clear's own
// refused-token answer re-prompts with the expiry copy. Rewritten for picker
// PR 2: the first Clear already carries the token, so a refused mint means NO
// Clear request at all.

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

function stand(mint: () => Response, clear: () => Response = cleared) {
  let clearRequests = 0;
  server.use(
    readAnswers([]),
    http.post(`*${MINT_PATH}`, () => mint()),
    http.post(CLEAR_PATH, () => {
      clearRequests += 1;
      return clear();
    })
  );
  return () => clearRequests;
}

async function submitPassword(value = FIXTURE_PW) {
  const onClose = await openClear();
  await enterPassword(value);
  fireEvent.click(verify());
  return onClose;
}

describe('DMThreadRemovalDialog step-up token (#3509)', () => {
  it('the account lockout at the mint lands on the password field, and Clear is never sent', async () => {
    const clearRequests = stand(() =>
      HttpResponse.json(
        { error_code: 'account_locked' },
        { status: 423, headers: { 'Retry-After': '60' } }
      )
    );

    await submitPassword();

    expect(
      await screen.findByText('Too many attempts. Try again in 60 seconds.')
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(clearRequests()).toBe(0);
  });

  it('a wrong password at the mint is a per-field error on the emptied, focused field', async () => {
    const clearRequests = stand(() =>
      HttpResponse.json({ error: 'Invalid password' }, { status: 403 })
    );

    await submitPassword('wrong-password-value');

    expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
    const field = screen.getByLabelText('Password');
    await waitFor(() => expect(field).toHaveFocus());
    expect(field).toHaveValue('');
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(clearRequests()).toBe(0);
  });

  it('any other mint failure says the password could not be checked', async () => {
    stand(() => HttpResponse.json({ error: 'boom' }, { status: 500 }));

    await submitPassword();

    expect(
      await screen.findByText("We couldn't check your password. Try again.")
    ).toBeInTheDocument();
  });

  it("Clear's refused token re-prompts with the expiry copy, and a second try succeeds", async () => {
    let mints = 0;
    let clears = 0;
    server.use(
      readAnswers([]),
      http.post(`*${MINT_PATH}`, () => {
        mints += 1;
        return HttpResponse.json({ step_up_token: `token-${mints}`, expires_in: 60 });
      }),
      http.post(CLEAR_PATH, () => {
        clears += 1;
        return clears === 1
          ? HttpResponse.json(
              {
                error: 'Your confirmation expired. Enter your password again.',
                password_required: true,
                step_up_token_invalid: true,
              },
              { status: 403 }
            )
          : cleared();
      })
    );

    const onClose = await submitPassword();

    expect(
      await screen.findByText('Your confirmation expired. Enter your password again.')
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(onClose).not.toHaveBeenCalled();

    await enterPassword(FIXTURE_PW);
    fireEvent.click(verify());
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(mints).toBe(2);
    expect(clears).toBe(2);
  });
});
