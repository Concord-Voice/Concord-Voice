import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockMessage, mockUser } from '../../../mocks/fixtures';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import DeleteRefusalModal from '@/renderer/components/Chat/DeleteRefusalModal';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { STEP_UP_TOKEN_EXPIRED_MESSAGE } from '@/renderer/services/system/stepUpToken';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import type { ChatContext } from '@/renderer/types/chat';

// The delete soft-lock's password exchange (#3509), through the dialog the
// person sees and the real controller that fills its slot. FE1: a refused
// exchange the password field cannot answer (a lockout, a server without the
// endpoint, a failed lookup, an MFA requirement naming no method) used to leave
// the password view up with words nothing rendered, and an emptied field. It
// now ends Close-only with the exchange's own words, as the purge soft-lock's
// does. TA3: a refused token on the retry sits on the password field.

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({ getState: () => 'connected' }),
  ConnectionState: { CONNECTED: 'connected', DISCONNECTED: 'disconnected' },
}));
vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: () => ({ sendMessage: vi.fn(), sendDMMessage: vi.fn() }),
}));
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({ e2eeService: { isInitialized: true } }));
vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: vi.fn(),
  unpinMessage: vi.fn(),
}));

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const ctx: ChatContext = { type: 'channel', id: 'channel-1', serverId: 'server-1' };

const PASSWORD_CHALLENGE = {
  error: 'Current password required to keep deleting messages',
  delete_rate_limited: true,
  password_required: true,
};

/** The chat panel's half of the wiring: the controller's slot drives the dialog. */
function Harness() {
  const chat = useChatController(ctx);
  return (
    <>
      <button type="button" onClick={() => void chat.deleteMessage('m1')}>
        Delete
      </button>
      <DeleteRefusalModal
        refusal={chat.deleteRefusal}
        onConfirm={chat.confirmDelete}
        onDismiss={chat.dismissDeleteRefusal}
        purpose="messages.delete"
        surfaceId="surface-main"
      />
    </>
  );
}

/**
 * A password account (the read offers nothing inline), a delete route that
 * answers each request with the next of `routeAnswers`, and a mint endpoint
 * that answers `mint`. Returns the delete route's request count.
 */
function stand(mint: () => Response, routeAnswers: Array<() => Response>) {
  let deletes = 0;
  server.use(
    http.get('*/api/v1/mfa/step-up', () =>
      HttpResponse.json({ methods: [], default_method: null, backup_code_available: false })
    ),
    http.delete('*/api/v1/messages/:id', () => {
      deletes += 1;
      return routeAnswers[Math.min(deletes, routeAnswers.length) - 1]();
    }),
    http.post(`*${MINT_PATH}`, () => mint())
  );
  return () => deletes;
}

const challenge = (body: object) => () => HttpResponse.json(body, { status: 403 });
const minted = () => HttpResponse.json({ step_up_token: 'minted-token', expires_in: 60 });
const confirmButton = () => screen.getByRole('button', { name: /^(Confirm|Waiting|Confirming)/ });
const body = () => document.querySelector('.delete-refusal-modal__body');

/** Deletes m1 into the password challenge, then confirms with a typed password. */
async function confirmWithPassword() {
  render(<Harness />);
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
  fireEvent.click(confirmButton());
}

beforeEach(() => {
  resetAllStores();
  useUserStore.setState({ user: mockUser });
  useChatStore.getState().addMessage(ctx.id, { ...mockMessage, id: 'm1', channel_id: ctx.id });
});

describe('DeleteRefusalModal: a refused password exchange (FE1)', () => {
  // Mutant: the controller leaving these on the password view, whose words no
  // view renders: the sentence never appears and the field just empties.
  it.each([
    [
      'the account lockout (423)',
      () =>
        HttpResponse.json(
          { error_code: 'account_locked' },
          { status: 423, headers: { 'Retry-After': '30' } }
        ),
      'Too many attempts. Try again in 30 seconds.',
    ],
    [
      'a rate limit (429)',
      () => HttpResponse.json({ error: 'Too many requests' }, { status: 429 }),
      'Too many attempts. Try again later.',
    ],
    [
      'a server without the endpoint',
      () => HttpResponse.json({ error: 'Not Found' }, { status: 404 }),
      "This server doesn't support this confirmation yet.",
    ],
    [
      'a failed lookup',
      () => HttpResponse.json({ error: 'boom' }, { status: 500 }),
      "We couldn't check your password. Try again.",
    ],
    [
      'an MFA requirement naming no method',
      () => HttpResponse.json({ error: 'MFA required', mfa_required: true }, { status: 403 }),
      'This account now confirms with an authenticator app or security key. Close this and try again.',
    ],
  ])('%s ends Close-only with its words', async (_name, mint, sentence) => {
    const deletes = stand(mint, [challenge(PASSWORD_CHALLENGE)]);

    await confirmWithPassword();

    await waitFor(() => expect(body()).toHaveTextContent(sentence));
    expect(
      screen.getByRole('dialog', { name: "Couldn't delete that message" })
    ).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Confirm/ })).not.toBeInTheDocument();
    // The body's own Close, beside the header's: the view is Close-only.
    expect(screen.getAllByRole('button', { name: 'Close' })).toHaveLength(2);
    // The route was refused once and never retried.
    expect(deletes()).toBe(1);
  });

  // Positive control: a wrong password IS something the field answers, so the
  // challenge stays, and the harness above did reach the exchange.
  it('a wrong password stays on the password field, worded there', async () => {
    const deletes = stand(challenge({ error: 'Invalid password' }), [
      challenge(PASSWORD_CHALLENGE),
    ]);

    await confirmWithPassword();

    expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
    expect(screen.getByRole('dialog', { name: "Confirm it's you" })).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
    expect(deletes()).toBe(1);
  });
});

describe('DeleteRefusalModal: a refused token on the retry (TA3)', () => {
  it('shows the expiry on the emptied, focused password field, and nowhere else', async () => {
    const deletes = stand(minted, [
      challenge(PASSWORD_CHALLENGE),
      challenge({
        ...PASSWORD_CHALLENGE,
        error: STEP_UP_TOKEN_EXPIRED_MESSAGE,
        step_up_token_invalid: true,
      }),
    ]);

    await confirmWithPassword();

    const field = await screen.findByLabelText('Password');
    await waitFor(() => expect(field).toHaveAccessibleDescription(STEP_UP_TOKEN_EXPIRED_MESSAGE));
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveValue('');
    expect(field).toHaveFocus();
    expect(screen.getAllByText(STEP_UP_TOKEN_EXPIRED_MESSAGE)).toHaveLength(1);
    expect(screen.getByRole('dialog', { name: "Confirm it's you" })).toBeInTheDocument();
    expect(deletes()).toBe(2);
  });
});
