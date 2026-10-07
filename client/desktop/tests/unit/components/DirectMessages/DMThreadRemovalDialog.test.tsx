import { act, fireEvent, render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import {
  clearDMHistory,
  hideDMThread,
  type ClearHistoryResult,
} from '@/renderer/services/messaging/dmVisibilityApi';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';
import { vi, describe, beforeEach, afterEach, expect, it } from 'vitest';

// DM Clear on the shared step-up stage (picker PR 2, surface #3). The service
// is mocked, so what the dialog SENDS is observable as `clearDMHistory`'s
// arguments; the requirements read goes through a mocked `apiFetch` and is
// scripted per test. The wire-level behaviour (mint, token, ceremony) lives in
// the sibling `.passwordStepUp`, `.stepUpToken`, `.mintReview` and
// `.securityKey` files.
//
// Decision #3: with purge protection on, "Continue" is a LOCAL stage change
// and the first Clear request carries the factor. With it off, Clear is one
// click with no factor and no read.
//
// "Mutant:" comments name the production change each test exists to turn red.

vi.mock('@/renderer/services/messaging/dmVisibilityApi', () => ({
  clearDMHistory: vi.fn(),
  hideDMThread: vi.fn(),
}));

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

const mockClearDMHistory = vi.mocked(clearDMHistory);
const mockHideDMThread = vi.mocked(hideDMThread);

const READ = '/api/v1/mfa/step-up';
const CODE = '123456';
const PASSWORD_VALUE = 'test-password-123'; // pragma: allowlist secret
const CREDENTIALS = 'Confirm it is you';
const CHECKING = 'Checking your verification methods…';

let readAnswer: () => Response | Promise<Response>;

function readBody(methods: string[], defaultMethod: string | null = methods[0] ?? null): Response {
  return new Response(
    JSON.stringify({ methods, default_method: defaultMethod, backup_code_available: false }),
    { status: 200, headers: { 'Content-Type': 'application/json' } }
  );
}

function readStatus(status: number, body: unknown = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const readHits = () => mockApiFetch.mock.calls.filter(([path]) => path === READ).length;

function setProtection(requireAuthBeforePurge: boolean | undefined) {
  usePrivacyStore.setState((state) => ({
    settings: { ...state.settings, requireAuthBeforePurge: requireAuthBeforePurge as boolean },
  }));
}

const primary = () => screen.getByRole('button', { name: /^(Continue|Verify and clear)$/ });
const stageHeading = () => screen.getByRole('heading', { name: CREDENTIALS });
const codeField = () => screen.findByLabelText('Authenticator app code');
const passwordField = () => screen.findByLabelText('Password');

async function enterCode(value = CODE) {
  const user = userEvent.setup();
  await user.type(await codeField(), value);
}

async function enterPassword(value = PASSWORD_VALUE) {
  fireEvent.change(await passwordField(), { target: { value } });
}

/** Opens the credential stage from "Continue" with protection on. */
async function continueToCredentials() {
  await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
  await screen.findByRole('heading', { name: CREDENTIALS });
}

const verify = () => screen.getByRole('button', { name: 'Verify and clear' });

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

const groupConversation: DMConversation = {
  ...conversation,
  id: 'group-1',
  isGroup: true,
  name: 'The group',
};

const personalConversation: DMConversation = {
  ...conversation,
  id: 'personal-1',
  isPersonal: true,
  name: 'Personal Thread',
};

const onClose = vi.fn();
const onRemoved = vi.fn();
const purgedListeners = new Set<EventListener>();

function renderDialog(action: 'hide' | 'clear' | 'leave', target = conversation) {
  return render(
    <DMThreadRemovalDialog
      target={{ conversation: target, action }}
      onClose={onClose}
      onRemoved={onRemoved}
    />
  );
}

function installStore({
  rows = [conversation],
  active = conversation.id,
  refreshRows = rows,
}: {
  rows?: DMConversation[];
  active?: string | null;
  refreshRows?: DMConversation[];
} = {}) {
  const removeConversation = vi.fn((id: string) => {
    useDMStore.setState((state) => ({
      conversations: state.conversations.filter((row) => row.id !== id),
    }));
  });
  const discardConversationView = vi.fn((id: string) => {
    useDMStore.setState((state) => ({
      conversations: state.conversations.filter((row) => row.id !== id),
    }));
  });
  const fetchConversations = vi.fn(async () => {
    useDMStore.setState({ conversations: refreshRows });
  });
  const leaveGroup = vi.fn(async (id: string) => {
    useDMStore.setState((state) => ({
      conversations: state.conversations.filter((row) => row.id !== id),
    }));
  });
  useDMStore.setState({
    conversations: rows,
    activeConversationId: active,
    removeConversation,
    discardConversationView,
    fetchConversations,
    leaveGroup,
  });
  return { removeConversation, discardConversationView, fetchConversations, leaveGroup };
}

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  installStore();
  mockHideDMThread.mockReset();
  mockClearDMHistory.mockReset();
  mockHideDMThread.mockResolvedValue(true);
  mockClearDMHistory.mockResolvedValue({ kind: 'success' });
  readAnswer = () => readBody(['totp']);
  mockApiFetch.mockReset();
  mockApiFetch.mockImplementation(async (path: string) => {
    if (path === READ) return readAnswer();
    throw new Error(`unexpected request to ${path}`);
  });
});

afterEach(() => {
  for (const listener of purgedListeners) {
    window.removeEventListener('messages-purged', listener);
  }
  purgedListeners.clear();
});

describe('DMThreadRemovalDialog', () => {
  it('renders the action consequence and initially focuses Cancel', async () => {
    renderDialog('hide');

    const dialog = await screen.findByRole('dialog', { name: 'Hide thread' });
    expect(within(dialog).getByText(/you'll still receive new messages/i)).toBeInTheDocument();
    await waitFor(() =>
      expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus()
    );
  });

  it('keeps group Leave distinct and warns about encryption keys', () => {
    renderDialog('leave', groupConversation);

    expect(screen.getByRole('dialog', { name: 'Leave group' })).toHaveTextContent(
      /lose access.*messages and encryption keys/i
    );
    expect(screen.getByRole('button', { name: 'Leave group' })).toBeInTheDocument();
  });

  it('styles reversible Hide as neutral and Clear/Leave as destructive', () => {
    const cases = [
      { action: 'hide', target: conversation, name: 'Hide thread', neutral: true },
      { action: 'clear', target: conversation, name: 'Continue', neutral: false },
      { action: 'leave', target: groupConversation, name: 'Leave group', neutral: false },
    ] as const;
    for (const { action, target, name, neutral } of cases) {
      const { unmount } = renderDialog(action, target);
      const confirm = screen.getByRole('button', { name });
      expect(confirm.classList.contains('dm-removal-neutral')).toBe(neutral);
      expect(document.querySelector('.delete-server-warning.dm-removal-neutral') !== null).toBe(
        neutral
      );
      unmount();
    }
  });

  it('omits every removal action for personal threads', () => {
    renderDialog('hide', personalConversation);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.queryByText(/hide this thread/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/clear history/i)).not.toBeInTheDocument();
  });

  it('removes Hide locally before the request settles and confirms after success', async () => {
    let resolveHide!: (result: boolean) => void;
    mockHideDMThread.mockReturnValue(
      new Promise((resolve) => {
        resolveHide = resolve;
      })
    );
    const { discardConversationView } = installStore();
    renderDialog('hide');

    fireEvent.click(screen.getByRole('button', { name: 'Hide thread' }));
    await waitFor(() => expect(discardConversationView).toHaveBeenCalledWith('dm-1'));
    expect(onClose).not.toHaveBeenCalled();
    expect(onRemoved).not.toHaveBeenCalled();

    await act(async () => resolveHide(true));
    await waitFor(() => expect(onRemoved).toHaveBeenCalledOnce());
  });

  it('refetches after Hide fails and restores the selected row when the server returns it', async () => {
    mockHideDMThread.mockResolvedValue(false);
    const { fetchConversations } = installStore({ refreshRows: [conversation] });
    renderDialog('hide');

    fireEvent.click(screen.getByRole('button', { name: 'Hide thread' }));
    await waitFor(() => expect(fetchConversations).toHaveBeenCalledOnce());
    expect(useDMStore.getState().activeConversationId).toBe('dm-1');
    expect(screen.getByRole('alert')).toHaveTextContent(/could not confirm the hide/i);
    expect(onRemoved).not.toHaveBeenCalled();
  });

  it('does not mutate the successor account after a deferred Hide response', async () => {
    let resolveHide!: (result: boolean) => void;
    mockHideDMThread.mockReturnValue(
      new Promise((resolve) => {
        resolveHide = resolve;
      })
    );
    const { fetchConversations } = installStore();
    renderDialog('hide');

    fireEvent.click(screen.getByRole('button', { name: 'Hide thread' }));
    useAuthStore.getState().setAccessToken('successor-token');
    await act(async () => resolveHide(true));

    expect(onClose).not.toHaveBeenCalled();
    expect(onRemoved).not.toHaveBeenCalled();
    expect(fetchConversations).not.toHaveBeenCalled();
  });

  it.each([
    ['hide', conversation],
    ['leave', groupConversation],
    ['clear', personalConversation],
  ] as const)('%s never reads the step-up requirements', async (action, target) => {
    // Mutant: `enabled` ignoring the action, or a personal thread's removable gate.
    renderDialog(action, target);
    await act(async () => {});

    expect(readHits()).toBe(0);
  });

  describe('Clear with purge protection off', () => {
    beforeEach(() => setProtection(false));

    it('is one click with no factor and no read, then invalidates the scoped history and refetches once', async () => {
      const fetchConversations = installStore().fetchConversations;
      const purged = vi.fn();
      const order: string[] = [];
      fetchConversations.mockImplementationOnce(async () => {
        order.push('refetch');
        useDMStore.setState({ conversations: [conversation] });
      });
      purged.mockImplementation(() => order.push('purged'));
      window.addEventListener('messages-purged', purged);
      purgedListeners.add(purged);
      renderDialog('clear');

      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      await waitFor(() => expect(fetchConversations).toHaveBeenCalledOnce());

      // Mutant: protection off still issuing the read or asking for credentials.
      expect(mockClearDMHistory.mock.calls).toEqual([['dm-1']]);
      expect(readHits()).toBe(0);
      expect(screen.queryByRole('heading', { name: CREDENTIALS })).not.toBeInTheDocument();
      expect(purged).toHaveBeenCalledOnce();
      expect(order).toEqual(['purged', 'refetch']);
      expect(purged.mock.calls[0][0]).toMatchObject({ detail: { scopeId: 'dm-1' } });
      expect(onClose).toHaveBeenCalledOnce();
      expect(onRemoved).not.toHaveBeenCalled();
    });

    it('sends the request once however many times it is clicked', async () => {
      const answer = deferred<ClearHistoryResult>();
      mockClearDMHistory.mockReturnValue(answer.promise);
      renderDialog('clear');

      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      await act(async () => answer.resolve({ kind: 'success' }));

      expect(mockClearDMHistory).toHaveBeenCalledOnce();
    });

    // The server's own answer wins over a stale local setting: it opens the
    // stage and the read starts then, and the next request carries the factor.
    it.each([
      {
        asked: { kind: 'passwordRequired' } as ClearHistoryResult,
        methods: [],
        enter: enterPassword,
        factor: { kind: 'password', value: PASSWORD_VALUE },
      },
      {
        asked: { kind: 'mfaRequired', methods: ['totp'] } as ClearHistoryResult,
        methods: ['totp'],
        enter: enterCode,
        factor: { kind: 'mfa', value: CODE },
      },
    ])(
      'opens the credential stage when the server asks for $asked.kind although the setting was off',
      async ({ asked, methods, enter, factor }) => {
        // Mutant: dropping `setClearStage('credentials')` (the user is stranded).
        readAnswer = () => readBody(methods);
        mockClearDMHistory.mockResolvedValueOnce(asked);
        renderDialog('clear');
        expect(readHits()).toBe(0);

        fireEvent.click(screen.getByRole('button', { name: 'Continue' }));

        expect(await screen.findByRole('heading', { name: CREDENTIALS })).toHaveFocus();
        await waitFor(() => expect(readHits()).toBe(1));
        expect(onClose).not.toHaveBeenCalled();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();

        await enter();
        fireEvent.click(verify());
        await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledTimes(2));
        expect(mockClearDMHistory).toHaveBeenLastCalledWith('dm-1', factor, expect.anything());
        await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
      }
    );

    it('reports a Clear the thread no longer allows without opening the stage', async () => {
      mockClearDMHistory.mockResolvedValueOnce({ kind: 'notFound' });
      renderDialog('clear');

      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));

      expect(await screen.findByRole('alert')).toHaveTextContent(
        'This thread is no longer available.'
      );
      expect(screen.queryByRole('heading', { name: CREDENTIALS })).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Continue' })).toBeEnabled();
      expect(onClose).not.toHaveBeenCalled();
    });

    it('does not dispatch or refetch for a Clear response from the prior account', async () => {
      const answer = deferred<ClearHistoryResult>();
      mockClearDMHistory.mockReturnValue(answer.promise);
      const { fetchConversations } = installStore();
      const purged = vi.fn();
      window.addEventListener('messages-purged', purged);
      purgedListeners.add(purged);
      renderDialog('clear');

      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      useAuthStore.getState().setAccessToken('successor-token');
      await act(async () => answer.resolve({ kind: 'success' }));

      expect(purged).not.toHaveBeenCalled();
      expect(fetchConversations).not.toHaveBeenCalled();
      expect(onClose).not.toHaveBeenCalled();
    });

    it('treats a thrown request as unresolved and never retries it', async () => {
      mockClearDMHistory.mockRejectedValueOnce(new Error('connection reset'));
      const { fetchConversations } = installStore();
      renderDialog('clear');

      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));

      expect(await screen.findByRole('alert')).toHaveTextContent(/could not confirm/i);
      expect(fetchConversations).toHaveBeenCalledOnce();
      expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
      expect(mockClearDMHistory).toHaveBeenCalledOnce();
    });
  });

  describe('Clear with purge protection on', () => {
    it('starts the read when the dialog opens, so the stage is ready on arrival', async () => {
      renderDialog('clear');
      await act(async () => {});

      expect(readHits()).toBe(1);
      expect(mockClearDMHistory).not.toHaveBeenCalled();
    });

    // Decision #3. Mutant: the first Clear request carrying no factor (the
    // old factor-less probe that spent a purge unit to learn the requirement).
    it('Continue is a local stage change that sends nothing and moves focus to the stage heading', async () => {
      renderDialog('clear');

      await continueToCredentials();
      await act(async () => {});

      expect(mockClearDMHistory).not.toHaveBeenCalled();
      // Mutant: no focus move on the stage change (focus stays on Continue).
      expect(stageHeading()).toHaveFocus();
      // The dialog title is constant; the heading is what announces the stage.
      expect(screen.getByRole('dialog', { name: 'Clear history for me' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Verify and clear' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();
    });

    it('fails closed when the setting is missing: an older server omits the field', async () => {
      // Mutant: `requireAuthBeforePurge === true` (a missing field would skip step-up).
      setProtection(undefined);
      renderDialog('clear');

      await continueToCredentials();

      expect(mockClearDMHistory).not.toHaveBeenCalled();
      expect(readHits()).toBe(1);
    });

    it('sends the proven factor in the FIRST Clear request, then clears the thread', async () => {
      const fetchConversations = installStore().fetchConversations;
      const purged = vi.fn();
      window.addEventListener('messages-purged', purged);
      purgedListeners.add(purged);
      renderDialog('clear');

      await continueToCredentials();
      await enterCode();
      fireEvent.click(verify());

      await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
      expect(mockClearDMHistory).toHaveBeenCalledOnce();
      expect(mockClearDMHistory).toHaveBeenCalledWith(
        'dm-1',
        { kind: 'mfa', value: CODE },
        expect.anything()
      );
      expect(purged).toHaveBeenCalledOnce();
      expect(purged.mock.calls[0][0]).toMatchObject({ detail: { scopeId: 'dm-1' } });
      expect(fetchConversations).toHaveBeenCalledOnce();
    });

    it('sends the password, once, when the account has no inline method', async () => {
      readAnswer = () => readBody([]);
      renderDialog('clear');

      await continueToCredentials();
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
      await enterPassword();
      fireEvent.click(verify());

      await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
      expect(mockClearDMHistory.mock.calls).toEqual([
        ['dm-1', { kind: 'password', value: PASSWORD_VALUE }, expect.anything()],
      ]);
    });

    it('offers the password field only to an account with no inline method', async () => {
      renderDialog('clear');

      await continueToCredentials();

      expect(await codeField()).toBeInTheDocument();
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    });

    it('sets the autofill hints and masks the password', async () => {
      readAnswer = () => readBody([]);
      const { unmount } = renderDialog('clear');
      await continueToCredentials();
      const password = await passwordField();
      expect(password).toHaveAttribute('type', 'password');
      expect(password).toHaveAttribute('autocomplete', 'current-password');
      unmount();

      readAnswer = () => readBody(['totp']);
      renderDialog('clear');
      await continueToCredentials();
      expect(await codeField()).toHaveAttribute('autocomplete', 'one-time-code');
    });

    it('does not keep a typed password across a close and reopen', async () => {
      readAnswer = () => readBody([]);
      const first = renderDialog('clear');
      await continueToCredentials();
      await enterPassword();
      first.unmount();

      renderDialog('clear');
      await continueToCredentials();

      expect(await passwordField()).toHaveValue('');
    });

    it('says what is missing instead of sending an empty code or password', async () => {
      renderDialog('clear');
      await continueToCredentials();
      await codeField();

      fireEvent.click(verify());
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Enter the 6-digit code from your authenticator app to continue.'
      );
      expect(await codeField()).toHaveFocus();
      expect(mockClearDMHistory).not.toHaveBeenCalled();
    });

    it('says what is missing for an empty password and focuses the field', async () => {
      readAnswer = () => readBody([]);
      renderDialog('clear');
      await continueToCredentials();
      await passwordField();

      fireEvent.click(verify());

      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Enter your password to continue.'
      );
      expect(await passwordField()).toHaveFocus();
      expect(mockClearDMHistory).not.toHaveBeenCalled();
    });

    it('answers a click made before the read lands with the checking line, and sends nothing', async () => {
      const read = deferred<Response>();
      readAnswer = () => read.promise;
      renderDialog('clear');
      await continueToCredentials();

      fireEvent.click(verify());

      expect(await screen.findByRole('status')).toHaveTextContent(CHECKING);
      expect(mockClearDMHistory).not.toHaveBeenCalled();

      await act(async () => read.resolve(readBody(['totp'])));
      expect(await codeField()).toBeInTheDocument();
    });

    describe('when the requirements read does not land', () => {
      it('blocks on an unavailable read: no password field, no request, Retry re-reads', async () => {
        // Mutant: `readFailure: 'passwordOnly'` (unavailable must block #3).
        readAnswer = () => readStatus(503);
        renderDialog('clear');
        await continueToCredentials();

        expect(await screen.findByRole('status')).toHaveTextContent(
          "We couldn't check your verification methods. Check your connection and try again."
        );
        expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
        expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
        fireEvent.click(verify());
        await act(async () => {});
        expect(mockClearDMHistory).not.toHaveBeenCalled();

        readAnswer = () => readBody(['totp']);
        fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
        expect(await codeField()).toBeInTheDocument();
        expect(readHits()).toBe(2);
      });

      it('falls to the password alone on a server that predates the read', async () => {
        // Mutant: `unsupported` blocking.
        readAnswer = () => readStatus(404);
        renderDialog('clear');
        await continueToCredentials();

        await enterPassword();
        fireEvent.click(verify());

        await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledOnce());
        expect(mockClearDMHistory.mock.calls[0][1]).toEqual({
          kind: 'password',
          value: PASSWORD_VALUE,
        });
      });

      it.each([
        [403, { error_code: 'account_disabled' }, "Your account can't do this right now."],
        [403, { code: 'EMAIL_NOT_VERIFIED' }, 'Verify your email address to do this.'],
        [401, {}, 'Sign in again to clear history.'],
      ])(
        'treats a refused read (%i) as terminal, with its words and no way to send',
        async (status, body, text) => {
          readAnswer = () => readStatus(status, body);
          renderDialog('clear');
          await continueToCredentials();

          expect(await screen.findByRole('status')).toHaveTextContent(text);
          await waitFor(() => expect(stageHeading()).toHaveFocus());
          expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
          expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
          fireEvent.click(verify());
          await act(async () => {});
          expect(mockClearDMHistory).not.toHaveBeenCalled();
        }
      );
    });

    describe("Clear's answers", () => {
      it('reopens the code field, empty and focused, after a refused code', async () => {
        mockClearDMHistory.mockResolvedValueOnce({ kind: 'invalidMfaCode' });
        renderDialog('clear');
        await continueToCredentials();
        await enterCode('000000');
        fireEvent.click(verify());

        expect(await screen.findByRole('alert')).toHaveTextContent(/that code didn't work/i);
        const input = await codeField();
        await waitFor(() => expect(input).toHaveFocus());
        expect(input).toHaveValue('');
        expect(input).toHaveAttribute('aria-invalid', 'true');
        expect(screen.getByRole('dialog')).toBeInTheDocument();

        await enterCode();
        fireEvent.click(verify());
        await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
        expect(mockClearDMHistory).toHaveBeenLastCalledWith(
          'dm-1',
          { kind: 'mfa', value: CODE },
          expect.anything()
        );
      });

      it('puts a refused password on the password field, emptied and focused', async () => {
        readAnswer = () => readBody([]);
        mockClearDMHistory.mockResolvedValueOnce({ kind: 'invalidPassword' });
        renderDialog('clear');
        await continueToCredentials();
        await enterPassword('bad-password-value');
        fireEvent.click(verify());

        expect(await screen.findByRole('alert')).toHaveTextContent('That password is not correct.');
        const input = await passwordField();
        await waitFor(() => expect(input).toHaveFocus());
        expect(input).toHaveValue('');
        expect(input).toHaveAttribute('aria-invalid', 'true');
      });

      it('returns focus to the factor after an in-flight rejection, and locks the stage meanwhile', async () => {
        readAnswer = () => readBody([]);
        const answer = deferred<ClearHistoryResult>();
        mockClearDMHistory.mockReturnValueOnce(answer.promise);
        renderDialog('clear');
        await continueToCredentials();
        const input = await passwordField();
        fireEvent.change(input, { target: { value: 'incorrect' } });
        const submit = verify();
        submit.focus();
        fireEvent.click(submit);

        await waitFor(() => expect(input).toBeDisabled());
        // The primary is locked by `aria-disabled`, never natively disabled: a
        // native disable would drop focus to <body> mid-request.
        const inFlight = screen.getByRole('button', { name: 'Clearing…' });
        expect(inFlight).toBe(submit);
        expect(inFlight).toHaveAttribute('aria-disabled', 'true');
        expect(inFlight).not.toBeDisabled();
        expect(inFlight).toHaveFocus();
        expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();

        await act(async () => answer.resolve({ kind: 'invalidPassword' }));
        await waitFor(() => expect(input).toHaveFocus());
      });

      // Mutant: the spinner restricted to the ceremony phase, so the longest
      // wait (the request itself) shows a label change and nothing moving.
      it('shows the spinner before the label while the request is out, and not before', async () => {
        const answer = deferred<ClearHistoryResult>();
        mockClearDMHistory.mockReturnValueOnce(answer.promise);
        renderDialog('clear');
        await continueToCredentials();
        await enterCode();
        expect(verify().querySelector('.loading-spinner')).toBeNull();

        fireEvent.click(verify());

        const inFlight = await screen.findByRole('button', { name: 'Clearing…' });
        const spinner = inFlight.querySelector('.loading-spinner');
        expect(spinner).not.toBeNull();
        expect(inFlight.firstElementChild).toBe(spinner);

        await act(async () => answer.resolve({ kind: 'success' }));
      });

      it('sends one request however many times the primary is activated', async () => {
        const answer = deferred<ClearHistoryResult>();
        mockClearDMHistory.mockReturnValueOnce(answer.promise);
        renderDialog('clear');
        await continueToCredentials();
        await enterCode();

        act(() => {
          fireEvent.click(verify());
          fireEvent.click(verify());
        });
        await act(async () => answer.resolve({ kind: 'success' }));

        expect(mockClearDMHistory).toHaveBeenCalledOnce();
      });

      it.each([
        [{ kind: 'rateLimited', retryAfterSeconds: 30 }, 'Try again in 30 seconds.'],
        [{ kind: 'rateLimited' }, 'Try again later.'],
        [
          { kind: 'passwordRefused', message: 'That password is not correct.' },
          'That password is not correct.',
        ],
        [{ kind: 'sessionExpired' }, 'Sign in again to clear history.'],
        [{ kind: 'notFound' }, 'This thread is no longer available.'],
        [
          { kind: 'stepUpImpossible' },
          'Set a password, enable MFA, or turn off purge protection in Privacy & Security.',
        ],
        [{ kind: 'refused' }, 'History could not be cleared.'],
      ] as const)('keeps the dialog open and says %j', async (result, text) => {
        mockClearDMHistory.mockResolvedValueOnce(result);
        renderDialog('clear');
        await continueToCredentials();
        await enterCode();
        fireEvent.click(verify());

        expect(await screen.findByRole('alert')).toHaveTextContent(text);
        expect(screen.getByRole('dialog')).toBeInTheDocument();
        expect(onClose).not.toHaveBeenCalled();
        expect(mockClearDMHistory).toHaveBeenCalledOnce();
      });

      // Mutant: `uncertain` retried, or the stage left open to a second submit.
      it.each([
        [
          'an uncertain answer',
          () => mockClearDMHistory.mockResolvedValueOnce({ kind: 'uncertain' }),
        ],
        ['a thrown request', () => mockClearDMHistory.mockRejectedValueOnce(new Error('reset'))],
      ])('does not retry after %s, and leaves only Cancel', async (_name, arrange) => {
        arrange();
        const fetchConversations = installStore().fetchConversations;
        renderDialog('clear');
        await continueToCredentials();
        await enterCode();
        fireEvent.click(verify());

        expect(await screen.findByRole('alert')).toHaveTextContent(/could not confirm/i);
        await waitFor(() => expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus());
        expect(fetchConversations).toHaveBeenCalledOnce();
        expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
        expect(screen.queryByRole('heading', { name: CREDENTIALS })).not.toBeInTheDocument();
        expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
        await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
        expect(mockClearDMHistory).toHaveBeenCalledOnce();
      });

      // Mutant: an unsent request reported as a refusal (it says the password
      // or code was wrong) or the typed password dropped.
      it('says nothing about a request that never left, and keeps what was typed', async () => {
        readAnswer = () => readBody([]);
        mockClearDMHistory.mockResolvedValueOnce({ kind: 'aborted' });
        renderDialog('clear');
        await continueToCredentials();
        await enterPassword();
        fireEvent.click(verify());

        await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledOnce());
        await waitFor(() => expect(verify()).not.toHaveAttribute('aria-disabled', 'true'));
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(await passwordField()).toHaveValue(PASSWORD_VALUE);
        expect(onClose).not.toHaveBeenCalled();

        fireEvent.click(verify());
        await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
        expect(mockClearDMHistory).toHaveBeenCalledTimes(2);
      });

      it('ends the stage when the request never left because the account changed', async () => {
        readAnswer = () => readBody([]);
        const answer = deferred<ClearHistoryResult>();
        mockClearDMHistory.mockReturnValueOnce(answer.promise);
        renderDialog('clear');
        await continueToCredentials();
        await enterPassword();
        fireEvent.click(verify());
        await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledOnce());

        useAuthStore.setState((state) => ({ authGeneration: state.authGeneration + 1 }));
        await act(async () => answer.resolve({ kind: 'aborted' }));

        expect(await screen.findByRole('status')).toHaveTextContent(
          'Sign in again to clear history.'
        );
        expect(onClose).not.toHaveBeenCalled();
      });
    });

    // Mutant (C2): a prefetched TOTP set that receives `passwordRequired` keeps
    // its code box, so the password leg never mounts and the next submit sends
    // a code the account has no use for.
    it('moves a prefetched TOTP set to the password when Clear answers passwordRequired', async () => {
      mockClearDMHistory.mockResolvedValueOnce({ kind: 'passwordRequired' });
      renderDialog('clear');
      await continueToCredentials();
      await enterCode();
      fireEvent.click(verify());

      const password = await passwordField();
      await waitFor(() => expect(password).toHaveFocus());
      expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();

      fireEvent.change(password, { target: { value: PASSWORD_VALUE } });
      fireEvent.click(verify());

      await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledTimes(2));
      expect(mockClearDMHistory.mock.calls[0][1]).toEqual({ kind: 'mfa', value: CODE });
      expect(mockClearDMHistory.mock.calls[1][1]).toEqual({
        kind: 'password',
        value: PASSWORD_VALUE,
      });
    });

    // The only guard on the error banner: the outcome-side guards would let a
    // prior account's refusal text through.
    it.each([[{ kind: 'success' }], [{ kind: 'notFound' }], [{ kind: 'uncertain' }]] as const)(
      'does not act on a %j answer for the prior account',
      async (result) => {
        const answer = deferred<ClearHistoryResult>();
        mockClearDMHistory.mockReturnValueOnce(answer.promise);
        const { fetchConversations } = installStore();
        const purged = vi.fn();
        window.addEventListener('messages-purged', purged);
        purgedListeners.add(purged);
        renderDialog('clear');
        await continueToCredentials();
        await enterCode();
        fireEvent.click(verify());
        await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledOnce());

        useAuthStore.getState().setAccessToken('successor-token');
        await act(async () => answer.resolve(result));

        expect(purged).not.toHaveBeenCalled();
        expect(fetchConversations).not.toHaveBeenCalled();
        expect(onClose).not.toHaveBeenCalled();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      }
    );

    it('Cancel closes without sending anything', async () => {
      renderDialog('clear');
      await continueToCredentials();

      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

      expect(onClose).toHaveBeenCalledOnce();
      expect(mockClearDMHistory).not.toHaveBeenCalled();
    });
  });

  it('leaves a group through leaveGroup and never calls Hide or Clear', async () => {
    const { leaveGroup } = installStore({ rows: [groupConversation], active: 'group-1' });
    renderDialog('leave', groupConversation);
    fireEvent.click(screen.getByRole('button', { name: 'Leave group' }));
    await waitFor(() => expect(leaveGroup).toHaveBeenCalledWith('group-1'));
    expect(mockHideDMThread).not.toHaveBeenCalled();
    expect(mockClearDMHistory).not.toHaveBeenCalled();
    expect(onRemoved).toHaveBeenCalledOnce();
  });
});
