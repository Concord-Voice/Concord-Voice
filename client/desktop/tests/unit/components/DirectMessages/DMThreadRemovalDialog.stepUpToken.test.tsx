import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// DM Clear's password field shows the mint's refusals (#3509), and Clear's own
// refused-token answer re-prompts with the expiry copy. Rewritten for picker
// PR 2: the first Clear already carries the token, so a refused mint means NO
// Clear request at all.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});
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
    const EXPIRED_COPY = 'Your confirmation expired. Enter your password again.';
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

    // T8c: the expiry sits on the password field, never in the dialog's banner.
    const field = screen.getByLabelText('Password');
    await waitFor(() => expect(field).toHaveAccessibleDescription(EXPIRED_COPY));
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveValue('');
    expect(field).toHaveFocus();
    expect(screen.getAllByText(EXPIRED_COPY)).toHaveLength(1);
    expect(document.querySelector('.form-error-banner')).toBeNull();
    expect(onClose).not.toHaveBeenCalled();

    await enterPassword(FIXTURE_PW);
    fireEvent.click(verify());
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(mints).toBe(2);
    expect(clears).toBe(2);
  });
});

// D20 / T10: a runtime-server switch changes the base without touching the
// account, so `authGeneration` alone cannot tell. A credentialed Clear that
// was sent to the old server must not act on its answer for the new one.
describe('DMThreadRemovalDialog runtime-server switch (D20)', () => {
  const SWITCHED_TO = 'https://other-server.example.test';
  const purged = vi.fn();
  let fetchConversations: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    purged.mockClear();
    globalThis.addEventListener('messages-purged', purged);
    fetchConversations = vi.fn(async () => {});
    useDMStore.setState({ fetchConversations });
  });
  afterEach(() => {
    globalThis.removeEventListener('messages-purged', purged);
  });

  function held<T>() {
    let release!: (value: T) => void;
    const promise = new Promise<T>((resolve) => {
      release = resolve;
    });
    return { promise, release };
  }

  const mintsToken = () =>
    http.post(`*${MINT_PATH}`, () =>
      HttpResponse.json({ step_up_token: 'token-1', expires_in: 60 })
    );

  it('a server switch while a credentialed Clear is out drops its answer', async () => {
    const answer = held<Response>();
    let clearRequests = 0;
    server.use(
      readAnswers([]),
      mintsToken(),
      http.post(CLEAR_PATH, async () => {
        clearRequests += 1;
        return answer.promise;
      })
    );

    const onClose = await submitPassword();
    await waitFor(() => expect(clearRequests).toBe(1));

    setRuntimeServerBase(SWITCHED_TO);
    answer.release(cleared());
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(purged).not.toHaveBeenCalled();
    expect(fetchConversations).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('a server switch during the post-Clear refetch neither closes nor reports it', async () => {
    const refetch = held<void>();
    fetchConversations.mockImplementation(() => refetch.promise);
    server.use(
      readAnswers([]),
      mintsToken(),
      http.post(CLEAR_PATH, () => cleared())
    );

    const onClose = await submitPassword();
    // The refetch is the step in flight: the event went out before the switch.
    await waitFor(() => expect(fetchConversations).toHaveBeenCalledOnce());
    expect(purged).toHaveBeenCalledOnce();

    setRuntimeServerBase(SWITCHED_TO);
    refetch.release();
    await new Promise((resolve) => setTimeout(resolve, 50));

    // Still one event, from before the switch; nothing was said or closed for the new server.
    expect(purged).toHaveBeenCalledOnce();
    expect(fetchConversations).toHaveBeenCalledOnce();
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('a server switch while the refetch rejects shows no refetch error', async () => {
    let failRefetch!: (reason: Error) => void;
    fetchConversations.mockImplementation(
      () =>
        new Promise<void>((_, reject) => {
          failRefetch = reject;
        })
    );
    server.use(
      readAnswers([]),
      mintsToken(),
      http.post(CLEAR_PATH, () => cleared())
    );

    const onClose = await submitPassword();
    await waitFor(() => expect(fetchConversations).toHaveBeenCalledOnce());

    setRuntimeServerBase(SWITCHED_TO);
    failRefetch(new Error('refetch failed'));
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(onClose).not.toHaveBeenCalled();
    expect(screen.queryByText(/could not confirm/i)).not.toBeInTheDocument();
  });

  // TA2: the credentialed Clear itself fails in transport, after the switch.
  it.each([
    ['after a server switch says nothing', true],
    ['with no switch is reported as uncertain (control)', false],
  ])('a credentialed Clear that fails in transport %s', async (_name, switched) => {
    const answer = held<void>();
    let clearRequests = 0;
    server.use(
      readAnswers([]),
      mintsToken(),
      http.post(CLEAR_PATH, async () => {
        clearRequests += 1;
        await answer.promise;
        return HttpResponse.error();
      })
    );

    const onClose = await submitPassword();
    await waitFor(() => expect(clearRequests).toBe(1));

    if (switched) setRuntimeServerBase(SWITCHED_TO);
    answer.release();

    if (switched) {
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(screen.queryByText(/could not confirm/i)).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(purged).not.toHaveBeenCalled();
    } else {
      expect(await screen.findByText(/could not confirm/i)).toBeInTheDocument();
    }
    expect(onClose).not.toHaveBeenCalled();
  });

  // TA1: protection off, so Clear goes out with no factor. Its answer belongs
  // to the server that was current when it was sent.
  describe('an uncredentialed Clear', () => {
    beforeEach(() => {
      usePrivacyStore.setState((s) => ({
        settings: { ...s.settings, requireAuthBeforePurge: false },
      }));
    });

    async function sendHeldClear(reply: () => Response) {
      const answer = held<void>();
      let clearRequests = 0;
      server.use(
        readAnswers([]),
        http.post(CLEAR_PATH, async () => {
          clearRequests += 1;
          await answer.promise;
          return reply();
        })
      );
      const onClose = vi.fn();
      render(
        <DMThreadRemovalDialog
          target={{ conversation, action: 'clear' }}
          onClose={onClose}
          onRemoved={vi.fn()}
        />
      );
      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      await waitFor(() => expect(clearRequests).toBe(1));
      return { onClose, release: answer.release, clearRequests: () => clearRequests };
    }

    const REPLIES = [
      ['success', cleared],
      ['a password challenge', passwordRequired],
      ['a refusal with words', () => HttpResponse.json({ error: 'gone' }, { status: 404 })],
    ] as const;

    // Mutant: the account-only lifecycle check, which a server switch passes.
    it.each(REPLIES)('after a server switch, %s applies nothing', async (_name, reply) => {
      const { onClose, release } = await sendHeldClear(reply);

      setRuntimeServerBase(SWITCHED_TO);
      release();

      // The request is released, so the dialog is usable again.
      await waitFor(() => expect(screen.getByRole('button', { name: 'Continue' })).toBeEnabled());
      expect(purged).not.toHaveBeenCalled();
      expect(fetchConversations).not.toHaveBeenCalled();
      expect(onClose).not.toHaveBeenCalled();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    });

    // Mutant: the capture taken but not handed to `clearDMHistory`. A switch
    // that lands before the request leaves would send the old thread's Clear
    // to the new server; the later check would only hide its answer.
    it('a server switch before the request leaves sends no Clear anywhere', async () => {
      let clearRequests = 0;
      server.use(
        readAnswers([]),
        http.post(CLEAR_PATH, () => {
          clearRequests += 1;
          return cleared();
        })
      );
      render(
        <DMThreadRemovalDialog
          target={{ conversation, action: 'clear' }}
          onClose={vi.fn()}
          onRemoved={vi.fn()}
        />
      );
      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      // Same turn as the click: the request has not been dispatched yet.
      setRuntimeServerBase(SWITCHED_TO);

      await waitFor(() => expect(screen.getByRole('button', { name: 'Continue' })).toBeEnabled());
      expect(clearRequests).toBe(0);
      expect(purged).not.toHaveBeenCalled();
    });

    // Positive controls: the same answers with no switch each do their thing.
    it('with no switch, success closes and purges the thread view', async () => {
      const { onClose, release } = await sendHeldClear(cleared);
      release();
      await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
      expect(purged).toHaveBeenCalledOnce();
    });

    it('with no switch, a password challenge opens the credential stage', async () => {
      const { release } = await sendHeldClear(passwordRequired);
      release();
      expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
    });

    it('with no switch, a refusal shows its banner', async () => {
      const { release } = await sendHeldClear(() =>
        HttpResponse.json({ error: 'gone' }, { status: 404 })
      );
      release();
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'This thread is no longer available.'
      );
    });
  });
});
