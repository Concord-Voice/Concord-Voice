import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { useDMStore, type DMConversation, type DMLastMessage } from '@/renderer/stores/dmStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { API_BASE } from '@/renderer/config';
import { vi } from 'vitest';

// Mock e2eeService
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    decryptForChannel: vi.fn().mockResolvedValue(null),
  },
}));

import ConversationList from '@/renderer/components/DirectMessages/ConversationList';
import ContextMenuProvider from '@/renderer/components/ui/ContextMenuProvider';
import { e2eeService } from '@/renderer/services/e2eeService';

const makeConversation = (overrides: Partial<DMConversation> = {}): DMConversation => ({
  id: 'conv-1',
  isGroup: false,
  isPersonal: false,
  name: null,
  participants: [
    { userId: 'user-1', username: 'me', displayName: 'Me' },
    { userId: 'user-2', username: 'alice', displayName: 'Alice' },
  ],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-01T00:00:00Z',
  ...overrides,
});

const personalThread: DMConversation = {
  id: 'personal-1',
  isGroup: false,
  isPersonal: true,
  name: 'Personal',
  participants: [{ userId: 'user-1', username: 'me', displayName: 'Me' }],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-01T00:00:00Z',
};

describe('ConversationList', () => {
  const mockOnSelectThread = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    useVoiceStore.getState().reset();
    useUserStore.setState({
      user: {
        id: 'user-1',
        username: 'me',
        display_name: 'Me',
        email: 'me@test.com',
        email_verified: true,
        age_verified: true,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      },
    });
    useDMStore.setState({
      conversations: [],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
      openPersonalThread: vi.fn().mockResolvedValue(personalThread),
    });
    useFriendStore.setState({ friends: [] });
  });

  it('renders conversation list container', () => {
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    expect(container.querySelector('.conversation-list')).toBeInTheDocument();
  });

  it('renders search input', () => {
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByPlaceholderText('Search conversations...')).toBeInTheDocument();
  });

  it('renders Personal Thread button', () => {
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Personal Thread')).toBeInTheDocument();
  });

  it('fetches conversations on mount', () => {
    const mockFetch = vi.fn().mockResolvedValue(undefined);
    useDMStore.setState({ fetchConversations: mockFetch });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(mockFetch).toHaveBeenCalled();
  });

  it('shows empty state when no conversations exist', () => {
    useDMStore.setState({ conversations: [] });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('No conversations yet')).toBeInTheDocument();
  });

  it('renders 1:1 conversation with other users name', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('updates a 1:1 DM row from live friend presence', async () => {
    useFriendStore.getState().addFriend({
      id: 'friendship-1',
      userId: 'user-2',
      username: 'alice',
      displayName: 'Alice',
      status: 'offline',
    });
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'offline' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    expect(
      container.querySelector('.conversation-avatar .member-status-dot.offline')
    ).toBeInTheDocument();

    act(() => {
      useFriendStore.getState().updateFriendPresence('user-2', 'online');
    });

    await waitFor(() =>
      expect(
        container.querySelector('.conversation-avatar .member-status-dot.online')
      ).toBeInTheDocument()
    );
  });

  it('uses live friend presence over a stale DM participant snapshot', () => {
    useFriendStore.getState().addFriend({
      id: 'friendship-1',
      userId: 'user-2',
      username: 'alice',
      displayName: 'Alice',
      status: 'offline',
    });
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'online' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    expect(
      container.querySelector('.conversation-avatar .member-status-dot.offline')
    ).toBeInTheDocument();
  });

  it('renders group conversation with group name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Study Group')).toBeInTheDocument();
  });

  it('renders group with participant names when no group name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: null,
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Me, Alice, Bob')).toBeInTheDocument();
  });

  it('renders conversation with username when no display name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'charlie' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('charlie')).toBeInTheDocument();
  });

  it('shows last message preview after decryption', async () => {
    // All DM messages are E2EE — preview renders after e2eeService decrypts
    (e2eeService as any).isInitialized = true;
    (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('Hey there!');
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-ciphertext',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    await waitFor(() => expect(screen.getByText('Hey there!')).toBeInTheDocument());
    (e2eeService as any).isInitialized = false;
  });

  it('shows a friendly GIF label for a decrypted GIF-only last message preview', async () => {
    // regression for #1991
    (e2eeService as any).isInitialized = true;
    (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce(
      '{"text":"","gif_slug":"night-sleep-18"}'
    );
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-gif-envelope',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

    await waitFor(() => expect(screen.getByText('GIF')).toBeInTheDocument());
    expect(screen.queryByText(/gif_slug/)).not.toBeInTheDocument();
    (e2eeService as any).isInitialized = false;
  });

  it('shows "Encrypted message" for encrypted last message', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-ciphertext',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Encrypted message')).toBeInTheDocument();
  });

  it('does not show GIF from undecrypted server gif_slug metadata', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-gif-envelope',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
            gifSlug: 'night-sleep-18',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

    expect(screen.getByText('Encrypted message')).toBeInTheDocument();
    expect(screen.queryByText('GIF')).not.toBeInTheDocument();
  });

  it('prefers an optimistic plaintext preview over a stale decrypted cache', async () => {
    (e2eeService as any).isInitialized = true;
    const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
    decryptMock.mockResolvedValueOnce('First sent message');

    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'first-ciphertext',
            userId: 'user-1',
            username: 'me',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    await waitFor(() => expect(screen.getByText('First sent message')).toBeInTheDocument());

    decryptMock.mockRejectedValueOnce(new Error('optimistic content is not ciphertext'));
    const optimisticLastMessage: DMLastMessage & { plaintextPreview: string } = {
      content: 'Second sent message',
      plaintextPreview: 'Second sent message',
      userId: 'user-1',
      username: 'me',
      createdAt: '2025-01-01T12:01:00Z',
    };

    await act(async () => {
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: optimisticLastMessage,
          }),
        ],
      });
    });

    await waitFor(() => expect(screen.getByText('Second sent message')).toBeInTheDocument());
    expect(screen.queryByText('First sent message')).not.toBeInTheDocument();
    (e2eeService as any).isInitialized = false;
  });

  it('calls onSelectThread on conversation click', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    fireEvent.click(screen.getByText('Alice'));
    expect(mockOnSelectThread).toHaveBeenCalledWith('conv-1');
  });

  it('applies active class to selected conversation', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId="conv-1" onSelectThread={mockOnSelectThread} />
    );
    const activeItem = container.querySelector('.conversation-item.active');
    expect(activeItem).toBeInTheDocument();
  });

  it('highlights personal thread when selected', () => {
    useDMStore.setState({
      conversations: [personalThread],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId="personal-1" onSelectThread={mockOnSelectThread} />
    );
    const activePersonal = container.querySelector('.personal-thread.active');
    expect(activePersonal).toBeInTheDocument();
  });

  it('shows unread badge on conversation with unreads', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('5')).toBeInTheDocument();
  });

  it('shows 99+ for large unread counts', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 200 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('99+')).toBeInTheDocument();
  });

  it('does not show unread badge when count is 0', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 0 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    expect(container.querySelector('.conversation-unread-badge')).not.toBeInTheDocument();
  });

  it('applies unread class to conversation with unreads', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 3 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    const unreadItem = container.querySelector('.conversation-item.unread');
    expect(unreadItem).toBeInTheDocument();
  });

  it('filters conversations by search query', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({ id: 'conv-1' }),
        makeConversation({
          id: 'conv-2',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const searchInput = screen.getByPlaceholderText('Search conversations...');
    fireEvent.change(searchInput, { target: { value: 'alice' } });
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.queryByText('Bob')).not.toBeInTheDocument();
  });

  it('search is case insensitive', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const searchInput = screen.getByPlaceholderText('Search conversations...');
    fireEvent.change(searchInput, { target: { value: 'ALICE' } });
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('opens personal thread on click', async () => {
    const mockOpen = vi.fn().mockResolvedValue(personalThread);
    useDMStore.setState({ openPersonalThread: mockOpen });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    fireEvent.click(screen.getByText('Personal Thread'));
    await vi.waitFor(() => {
      expect(mockOpen).toHaveBeenCalled();
      expect(mockOnSelectThread).toHaveBeenCalledWith('personal-1');
    });
  });

  it('does not show personal thread in filtered conversation list', () => {
    useDMStore.setState({
      conversations: [personalThread, makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const personalButtons = screen.getAllByText('Personal Thread');
    expect(personalButtons.length).toBe(1);
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('shows avatar initial for 1:1 conversations', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    const initial = container.querySelector('.conversation-avatar-initial');
    expect(initial).toBeInTheDocument();
    expect(initial?.textContent).toBe('A');
  });

  it('shows the other users avatar for 1:1 conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            {
              userId: 'user-2',
              username: 'alice',
              displayName: 'Alice',
              avatarUrl: '/api/v1/media/avatars/alice.png',
            },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    const image = container.querySelector('.conversation-avatar-img');
    expect(image).toHaveAttribute('src', `${API_BASE}/api/v1/media/avatars/alice.png`);
    expect(container.querySelector('.conversation-avatar-initial')).not.toBeInTheDocument();
  });

  it('falls back to initials when a 1:1 avatar image fails', async () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            {
              userId: 'user-2',
              username: 'alice',
              displayName: 'Alice',
              avatarUrl: 'https://example.com/broken.png',
            },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    fireEvent.error(container.querySelector('.conversation-avatar-img') as HTMLImageElement);

    await waitFor(() => {
      expect(container.querySelector('.conversation-avatar-img')).not.toBeInTheDocument();
      expect(container.querySelector('.conversation-avatar-initial')).toHaveTextContent('A');
    });
  });

  it('shows group avatar icon for group conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Team',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    const groupAvatar = container.querySelector('.conversation-avatar.group');
    expect(groupAvatar).toBeInTheDocument();
  });

  it('shows group conversation icons when set', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Team',
          iconUrl: '/api/v1/media/dm-icons/group-1.png',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    expect(container.querySelector('.conversation-avatar-img')).toHaveAttribute(
      'src',
      `${API_BASE}/api/v1/media/dm-icons/group-1.png`
    );
  });

  it('falls back to the group icon when a group image fails', async () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Team',
          iconUrl: 'https://example.com/broken-group.png',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    fireEvent.error(container.querySelector('.conversation-avatar-img') as HTMLImageElement);

    await waitFor(() => {
      expect(container.querySelector('.conversation-avatar-img')).not.toBeInTheDocument();
      expect(container.querySelector('.conversation-avatar.group svg')).toBeInTheDocument();
    });
  });

  it('opens context menu on right-click', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const item = screen.getByLabelText('Alice');
    fireEvent.contextMenu(item);
    // Issue #84 added Mute Conversation as the always-present menu item;
    // it replaces the prior "No actions available" fallback. Asserting on
    // Mute is the most robust marker that the context menu opened — it's
    // there regardless of encryption state, while Rotate Encryption Key
    // depends on the conversation being encrypted.
    expect(screen.getByText('Mute Conversation')).toBeInTheDocument();
  });

  it('renders multiple conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({ id: 'conv-1' }),
        makeConversation({
          id: 'conv-2',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-3', username: 'charlie', displayName: 'Charlie' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('Charlie')).toBeInTheDocument();
  });

  it('logs redacted error when openPersonalThread fails', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useDMStore.setState({
      conversations: [],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
      openPersonalThread: vi.fn().mockRejectedValueOnce(new Error('boom')),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    fireEvent.click(screen.getByText('Personal Thread'));
    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to open personal thread:', 'boom');
    });
    consoleSpy.mockRestore();
  });

  // ─── #1219 R6: multi-participant "N of M in call" list indicator ──────

  describe('in-call indicator (#1219 R6)', () => {
    const groupConv = makeConversation({
      id: 'grp-1',
      isGroup: true,
      name: 'Squad',
      participants: [
        { userId: 'user-1', username: 'me', displayName: 'Me' },
        { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        { userId: 'user-3', username: 'bob', displayName: 'Bob' },
        { userId: 'user-4', username: 'carol', displayName: 'Carol' },
        { userId: 'user-5', username: 'dan', displayName: 'Dan' },
      ],
    });

    it('shows "N of M in call" for a group with an active call I am not in', () => {
      useDMStore.setState({
        conversations: [groupConv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-2', 'user-3', 'user-4'], 5);
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(screen.getByText('3 of 5 in call')).toBeInTheDocument();
    });

    it('does not show the group indicator when no call is active', () => {
      useDMStore.setState({
        conversations: [groupConv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(screen.queryByText(/in call/)).not.toBeInTheDocument();
    });

    it('keeps the 1:1 🔊 badge when locally in a 1:1 call', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useVoiceStore.getState().setDMCall(true, 'conv-1');
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      const { container } = render(
        <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );
      const badge = container.querySelector('.conversation-in-call-badge');
      expect(badge).toBeInTheDocument();
      expect(badge?.textContent).toBe('🔊');
    });

    it('shows "N of M in call" even on a group call I AM in (group always shows roster)', () => {
      useDMStore.setState({
        conversations: [groupConv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-1', 'user-2'], 5);
      useVoiceStore.getState().setDMCall(true, 'grp-1');
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(screen.getByText('2 of 5 in call')).toBeInTheDocument();
    });
  });

  // ─── #984 expansion: Block / Unfriend modal flow coverage ─────────────

  describe('Block / Unfriend modal flows (#984)', () => {
    // Helper: import friendStore lazily so the test file doesn't reach for it
    // unless these tests run.
    async function setupFriendStore(opts: {
      blockUser?: ReturnType<typeof vi.fn>;
      removeFriend?: ReturnType<typeof vi.fn>;
      isFriend?: boolean;
    }) {
      const { useFriendStore } = await import('@/renderer/stores/friendStore');
      useFriendStore.setState({
        friends: opts.isFriend
          ? [
              {
                userId: 'user-2',
                username: 'alice',
                displayName: 'Alice',
                status: 'online',
                avatarUrl: undefined,
                createdAt: '2025-01-01T00:00:00Z',
                accentColor: undefined,
              },
            ]
          : [],
        blockUser: opts.blockUser ?? vi.fn().mockResolvedValue(undefined),
        removeFriend: opts.removeFriend ?? vi.fn().mockResolvedValue(undefined),
      });
    }

    it('shows only the DM row menu when wrapped by the global channel-area context provider (#1712)', async () => {
      await setupFriendStore({ isFriend: true });
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      fireEvent.contextMenu(screen.getByLabelText('Alice'));

      expect(await screen.findByText('Block User')).toBeInTheDocument();
      expect(screen.getByText('Unfriend')).toBeInTheDocument();
      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('does not fall through to the channel menu when right-clicking empty DM list space (#1712)', () => {
      useDMStore.setState({
        conversations: [],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      fireEvent.contextMenu(screen.getByText('No conversations yet'));

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('does not fall through to the channel menu from the DM row keyboard context-menu path (#1712)', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const convButton = screen.getByLabelText('Alice');
      convButton.focus();
      fireEvent.keyDown(convButton, { key: 'ContextMenu' });

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('does not fall through to the channel menu from Shift+F10 on a DM row (#1712)', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const convButton = screen.getByLabelText('Alice');
      convButton.focus();
      fireEvent.keyDown(convButton, { key: 'F10', shiftKey: true });

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('ignores non-context keyboard events inside the DM list boundary (#1712)', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const convButton = screen.getByLabelText('Alice');
      convButton.focus();
      fireEvent.keyDown(convButton, { key: 'ArrowDown' });

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('keeps the global text-input context menu available from the DM search field (#1712)', () => {
      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      fireEvent.contextMenu(screen.getByPlaceholderText('Search conversations...'));

      expect(screen.getByText('Paste')).toBeInTheDocument();
      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
    });

    it('keeps the global keyboard context menu available from the DM search field (#1712)', () => {
      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const searchInput = screen.getByPlaceholderText('Search conversations...');
      searchInput.focus();
      fireEvent.keyDown(searchInput, { key: 'ContextMenu' });

      expect(screen.getByText('Paste')).toBeInTheDocument();
      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
    });

    it('Block User flow: right-click → Block User → modal → Confirm → friendStore.blockUser called', async () => {
      const blockSpy = vi.fn().mockResolvedValue(undefined);
      await setupFriendStore({ blockUser: blockSpy });

      const conv = {
        ...makeConversation(),
        id: 'conv-block',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      // Right-click the conversation row to open the context menu.
      const convButton = screen.getByLabelText('Alice');
      fireEvent.contextMenu(convButton);

      // Click "Block User" in the menu → ConversationList opens the modal.
      const blockItem = await screen.findByText('Block User');
      fireEvent.click(blockItem);

      // ConfirmActionModal opens with the peer's display name in the title.
      const modalTitle = await screen.findByText(/Block Alice/);
      expect(modalTitle).toBeInTheDocument();

      // Click the "Block" confirm button inside the modal. The modal has
      // confirmLabel="Block", so look for that specific button.
      const confirmButton = screen.getByRole('button', { name: /^Block$/ });
      fireEvent.click(confirmButton);

      await waitFor(() => {
        expect(blockSpy).toHaveBeenCalledWith('user-2');
      });
    });

    it('Unfriend flow: right-click → Unfriend (only when friend) → modal → Confirm → friendStore.removeFriend called', async () => {
      const removeSpy = vi.fn().mockResolvedValue(undefined);
      // isFriend: true so the Unfriend item is visible.
      await setupFriendStore({ removeFriend: removeSpy, isFriend: true });

      const conv = {
        ...makeConversation(),
        id: 'conv-unfriend',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      const convButton = screen.getByLabelText('Alice');
      fireEvent.contextMenu(convButton);

      const unfriendItem = await screen.findByText('Unfriend');
      fireEvent.click(unfriendItem);

      const modalTitle = await screen.findByText(/Unfriend Alice/);
      expect(modalTitle).toBeInTheDocument();

      const confirmButton = screen.getByRole('button', { name: /^Unfriend$/ });
      fireEvent.click(confirmButton);

      await waitFor(() => {
        expect(removeSpy).toHaveBeenCalledWith('user-2');
      });
    });

    it('Block modal Cancel closes without calling blockUser (#984)', async () => {
      const blockSpy = vi.fn().mockResolvedValue(undefined);
      await setupFriendStore({ blockUser: blockSpy });

      const conv = {
        ...makeConversation(),
        id: 'conv-cancel',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('Block User'));
      await screen.findByText(/Block Alice/);

      const cancelButton = screen.getByRole('button', { name: /Cancel/i });
      fireEvent.click(cancelButton);

      // Modal closes, no API call fires.
      await waitFor(() => {
        expect(screen.queryByText(/Block Alice/)).not.toBeInTheDocument();
      });
      expect(blockSpy).not.toHaveBeenCalled();
    });
  });

  describe('View Profile modal flow (#1208)', () => {
    async function setupFriendStore(opts: { isFriend?: boolean } = {}) {
      const { useFriendStore } = await import('@/renderer/stores/friendStore');
      useFriendStore.setState({
        friends: opts.isFriend
          ? [
              {
                id: 'f-1',
                userId: 'user-2',
                username: 'alice',
                displayName: 'Alice',
                status: 'online',
                createdAt: '2025-01-01T00:00:00Z',
              },
            ]
          : [],
        blockUser: vi.fn().mockResolvedValue(undefined),
        removeFriend: vi.fn().mockResolvedValue(undefined),
      });
    }

    function setupConversation(id = 'conv-vp') {
      const conv = {
        ...makeConversation(),
        id,
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });
      return conv;
    }

    it('right-click → View Profile opens DMProfileModal with peer identity', async () => {
      await setupFriendStore();
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));

      // Modal renders with peer's @username
      expect(await screen.findByText('@alice')).toBeInTheDocument();
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    });

    it('View Profile → Send Message: invokes onSelectThread and closes modal', async () => {
      await setupFriendStore();
      const conv = setupConversation('conv-send');

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByRole('button', { name: 'Send Message' }));

      expect(mockOnSelectThread).toHaveBeenCalledWith(conv.id);
      await waitFor(() => {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      });
    });

    it('View Profile → Block: closes modal and opens ConfirmActionModal', async () => {
      await setupFriendStore();
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByRole('button', { name: 'Block' }));

      // ConfirmActionModal opens with peer name in title
      expect(await screen.findByText(/Block Alice/)).toBeInTheDocument();
    });

    it('View Profile → Unfriend (when friend): closes modal and opens ConfirmActionModal', async () => {
      await setupFriendStore({ isFriend: true });
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByRole('button', { name: 'Unfriend' }));

      expect(await screen.findByText(/Unfriend Alice/)).toBeInTheDocument();
    });

    it('View Profile → ✕ button: closes modal without side effects', async () => {
      await setupFriendStore();
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByLabelText('Close profile'));

      await waitFor(() => {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      });
      expect(mockOnSelectThread).not.toHaveBeenCalled();
    });
  });
});
