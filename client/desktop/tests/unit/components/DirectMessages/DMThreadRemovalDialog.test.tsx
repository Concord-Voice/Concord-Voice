import { act, fireEvent, render, screen, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { clearDMHistory, hideDMThread } from '@/renderer/services/messaging/dmVisibilityApi';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';
import { vi, describe, beforeEach, afterEach, expect, it } from 'vitest';

vi.mock('@/renderer/services/messaging/dmVisibilityApi', () => ({
  clearDMHistory: vi.fn(),
  hideDMThread: vi.fn(),
}));

const mockClearDMHistory = vi.mocked(clearDMHistory);
const mockHideDMThread = vi.mocked(hideDMThread);

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
  mockHideDMThread.mockResolvedValue(true);
  mockClearDMHistory.mockResolvedValue({ kind: 'success' });
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

  it('clears without a factor, invalidates the scoped history, then refetches once', async () => {
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
    await waitFor(() => expect(mockClearDMHistory).toHaveBeenCalledWith('dm-1', undefined));
    await waitFor(() => expect(fetchConversations).toHaveBeenCalledOnce());
    expect(purged).toHaveBeenCalledOnce();
    expect(order).toEqual(['purged', 'refetch']);
    expect(purged.mock.calls[0][0]).toMatchObject({ detail: { scopeId: 'dm-1' } });
    expect(onClose).toHaveBeenCalledOnce();
    expect(onRemoved).not.toHaveBeenCalled();
  });

  it('does not dispatch or refetch for a Clear response from the prior account', async () => {
    let resolveClear!: (result: { kind: 'success' }) => void;
    mockClearDMHistory.mockReturnValue(
      new Promise((resolve) => {
        resolveClear = resolve;
      })
    );
    const { fetchConversations } = installStore();
    const purged = vi.fn();
    window.addEventListener('messages-purged', purged);
    purgedListeners.add(purged);
    renderDialog('clear');

    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    useAuthStore.getState().setAccessToken('successor-token');
    await act(async () => resolveClear({ kind: 'success' }));

    expect(purged).not.toHaveBeenCalled();
    expect(fetchConversations).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it.each([
    ['password', 'password_required', 'Password', 'current_password'],
    ['MFA', 'mfa_required', 'Authentication code', 'mfa_code'],
  ] as const)('asks only for the server-selected %s factor', async (_name, kind, label, field) => {
    mockClearDMHistory.mockResolvedValueOnce({
      kind: kind === 'password_required' ? 'passwordRequired' : 'mfaRequired',
    });
    renderDialog('clear');
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));

    const input = await screen.findByLabelText(label);
    expect(input).toBeInTheDocument();
    expect(
      screen.queryByLabelText(field === 'current_password' ? 'Authentication code' : 'Password')
    ).not.toBeInTheDocument();
    fireEvent.change(input, {
      target: { value: field === 'current_password' ? 'test-password-123' : '123456' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));
    await waitFor(() =>
      expect(mockClearDMHistory).toHaveBeenLastCalledWith('dm-1', {
        kind: field === 'current_password' ? 'password' : 'mfa',
        value: field === 'current_password' ? 'test-password-123' : '123456',
      })
    );
  });

  it.each([
    [
      { kind: 'invalidPassword' as const },
      'passwordRequired',
      'Password',
      /password is not correct/i,
    ],
    [
      { kind: 'invalidMfaCode' as const },
      'mfaRequired',
      'Authentication code',
      /code is not correct/i,
    ],
    [
      { kind: 'rateLimited' as const, retryAfterSeconds: 30 },
      'passwordRequired',
      'Password',
      /try again in 30 seconds/i,
    ],
  ] as const)(
    'keeps Clear open with an actionable factor or rate-limit error',
    async (result, requiredKind, label, message) => {
      mockClearDMHistory.mockResolvedValueOnce({
        kind: requiredKind === 'mfaRequired' ? 'mfaRequired' : 'passwordRequired',
      });
      mockClearDMHistory.mockResolvedValueOnce(result);
      renderDialog('clear');
      fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
      if (label) {
        const input = await screen.findByLabelText(label);
        fireEvent.change(input, { target: { value: 'bad-factor' } });
        fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));
        if (result.kind === 'invalidPassword' || result.kind === 'invalidMfaCode') {
          await waitFor(() => {
            expect(input).toHaveFocus();
            expect(input).toHaveValue('');
          });
        }
      }
      await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(message));
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    }
  );

  it('returns focus to the factor after an in-flight rejection', async () => {
    let rejectFactor!: (result: { kind: 'invalidPassword' }) => void;
    mockClearDMHistory.mockResolvedValueOnce({ kind: 'passwordRequired' });
    mockClearDMHistory.mockReturnValueOnce(
      new Promise((resolve) => {
        rejectFactor = resolve;
      })
    );
    renderDialog('clear');
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    const input = await screen.findByLabelText('Password');
    fireEvent.change(input, { target: { value: 'incorrect' } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify and clear' }));
    await waitFor(() => expect(input).toBeDisabled());

    await act(async () => rejectFactor({ kind: 'invalidPassword' }));
    await waitFor(() => expect(input).toHaveFocus());
  });

  it('does not retry Clear after an uncertain response and reports unresolved outcome', async () => {
    mockClearDMHistory.mockResolvedValueOnce({ kind: 'uncertain' });
    const fetchConversations = installStore().fetchConversations;
    renderDialog('clear');
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/could not confirm/i));
    expect(mockClearDMHistory).toHaveBeenCalledTimes(1);
    expect(fetchConversations).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
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
