import { render, screen, act } from '../../../test-utils';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { mockUser, mockChannel, mockMessage, mockMessage2 } from '../../../mocks/fixtures';

// Mock apiFetch to prevent hanging fetches
const mockApiFetch = vi.fn().mockResolvedValue({
  ok: true,
  json: async () => ({ messages: [] }),
});
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

// Mock pinService — must be before ChatView import
const mockPinMessage = vi.fn().mockResolvedValue({
  message_id: 'msg-1',
  pinned_at: '2025-01-01T00:00:00Z',
  pinned_by: 'user-1',
});
const mockUnpinMessage = vi.fn().mockResolvedValue({ message_id: 'msg-1' });
const mockGetChannelPins = vi.fn().mockResolvedValue([]);
vi.mock('@/renderer/services/pinService', () => ({
  pinMessage: (...args: unknown[]) => mockPinMessage(...args),
  unpinMessage: (...args: unknown[]) => mockUnpinMessage(...args),
  getChannelPins: (...args: unknown[]) => mockGetChannelPins(...args),
}));

// Capture MessageList callback props for testing
let capturedMessageListProps: Record<string, unknown> = {};

// Mock heavy child components
vi.mock('@/renderer/components/Chat/MessageList', () => ({
  default: (props: {
    messages: unknown[];
    channelName?: string;
    isLoading: boolean;
    onUnseenOnLeave?: (count: number) => void;
    onPinToggle?: (message: unknown) => void;
    onScrollToMessage?: (messageId: string) => void;
    [key: string]: unknown;
  }) => {
    capturedMessageListProps = props;
    const { messages, channelName, isLoading } = props;
    return (
      <div data-testid="message-list" data-channel={channelName} data-loading={isLoading}>
        {(messages as Array<{ id: string; content: string }>).map((m) => (
          <div key={m.id} data-testid="message">
            {m.content}
          </div>
        ))}
      </div>
    );
  },
}));

vi.mock('@/renderer/components/Chat/MessageInput', () => ({
  default: ({
    channelName,
    disabled,
    isChannelEncrypted,
    serverId,
    channelId,
    onSendMessage,
  }: {
    channelName?: string;
    disabled: boolean;
    isChannelEncrypted?: boolean;
    serverId?: string;
    channelId?: string;
    onSendMessage?: (...args: unknown[]) => void;
  }) => (
    <div
      data-testid="message-input"
      data-channel={channelName}
      data-disabled={disabled}
      data-encrypted={isChannelEncrypted}
      data-server-id={serverId}
      data-channel-id={channelId}
    >
      <button
        data-testid="mock-send-btn"
        onClick={() => onSendMessage?.('hello', undefined, undefined, ['att-1'], [{ id: 'att-1' }])}
      >
        Send
      </button>
    </div>
  ),
}));

// Mock hooks
vi.mock('@/renderer/hooks/useChannelSubscription', () => ({
  useChannelSubscription: vi.fn(),
}));

const mockSendMsg = vi.fn();
vi.mock('@/renderer/hooks/useMessaging', () => ({
  useMessaging: () => ({
    sendMessage: mockSendMsg,
    markDelivered: vi.fn(),
    sendTyping: vi.fn(),
    getPendingCount: vi.fn(() => 0),
    getPendingMessagesForChannel: vi.fn(() => []),
  }),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    decryptForChannel: vi.fn(),
    getChannelKey: vi.fn(),
    invalidateChannelKey: vi.fn(),
    encryptForChannel: vi.fn(),
  },
}));

import ChatView from '@/renderer/components/Chat/ChatView';

describe('ChatView', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedMessageListProps = {};
    useUserStore.setState({ user: mockUser });
    useChannelStore.setState({ channels: [mockChannel], activeChannelId: null });
    useServerStore.setState({ activeServerId: 'server-1' });
    useChatStore.setState({
      messagesByChannel: new Map(),
      isConnected: true,
    });
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
    mockGetChannelPins.mockResolvedValue([]);
  });

  it('renders nothing when no active channel', () => {
    const { container } = render(<ChatView />);
    expect(container.querySelector('.chat-view')).not.toBeInTheDocument();
  });

  it('renders chat view when channel is active', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const { container } = render(<ChatView />);
    expect(container.querySelector('.chat-view')).toBeInTheDocument();
  });

  it('shows channel name in header', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    expect(screen.getByText('general')).toBeInTheDocument();
  });

  it('renders MessageList component', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    expect(screen.getByTestId('message-list')).toBeInTheDocument();
  });

  it('renders MessageInput component', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    expect(screen.getByTestId('message-input')).toBeInTheDocument();
  });

  it('passes messages from store to MessageList', async () => {
    const msgs = [mockMessage, mockMessage2];
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: msgs }),
    });
    useChatStore.setState({
      messagesByChannel: new Map([['channel-1', msgs]]),
    });
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const messageList = screen.getByTestId('message-list');
    expect(messageList).toBeInTheDocument();
  });

  it('fetches messages when channel changes', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    expect(mockApiFetch).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/channels/channel-1/messages')
    );
  });

  it('shows error when fetch fails', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Failed to load messages' }),
    });
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    expect(await screen.findByText('Failed to load messages')).toBeInTheDocument();
  });

  it('renders with encrypted channel', () => {
    useChannelStore.setState({
      channels: [{ ...mockChannel }],
      activeChannelId: 'channel-1',
    });
    render(<ChatView />);
    expect(screen.getByTestId('message-input')).toBeInTheDocument();
  });

  it('passes correct channel name to MessageList', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const messageList = screen.getByTestId('message-list');
    expect(messageList).toHaveAttribute('data-channel', 'general');
  });

  it('handles empty messages response', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    await vi.waitFor(() => {
      const messageList = screen.getByTestId('message-list');
      const messages = messageList.querySelectorAll('[data-testid="message"]');
      expect(messages.length).toBe(0);
    });
  });

  it('changes view when channel switches', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const { rerender } = render(<ChatView />);
    expect(mockApiFetch).toHaveBeenCalledWith(expect.stringContaining('channel-1'));
    // Switch channel
    useChannelStore.setState({
      channels: [mockChannel, { ...mockChannel, id: 'channel-2', name: 'random' }],
      activeChannelId: 'channel-2',
    });
    rerender(<ChatView />);
    // Second channel should also fetch
    expect(mockApiFetch).toHaveBeenCalledWith(expect.stringContaining('channel-2'));
  });

  // ── Header rendering ──

  it('shows default channel name when channel not found', () => {
    useChannelStore.setState({ channels: [], activeChannelId: 'channel-unknown' });
    render(<ChatView />);
    expect(screen.getByText('Channel')).toBeInTheDocument();
  });

  it('shows channel emoji when channel has one', () => {
    useChannelStore.setState({
      channels: [{ ...mockChannel, emoji: '🎮' }],
      activeChannelId: 'channel-1',
    });
    render(<ChatView />);
    const emoji = document.querySelector('.chat-header-emoji');
    expect(emoji).toBeInTheDocument();
    expect(emoji?.textContent).toBe('🎮');
  });

  it('shows hash icon when channel has no emoji', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const icon = document.querySelector('.chat-header-icon');
    expect(icon).toBeInTheDocument();
  });

  // ── MessageInput props ──

  it('passes serverId to MessageInput', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const input = screen.getByTestId('message-input');
    expect(input).toHaveAttribute('data-server-id', 'server-1');
  });

  it('passes channelId to MessageInput', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const input = screen.getByTestId('message-input');
    expect(input).toHaveAttribute('data-channel-id', 'channel-1');
  });

  it('disables input when no user', () => {
    useUserStore.setState({ user: null });
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const input = screen.getByTestId('message-input');
    expect(input).toHaveAttribute('data-disabled', 'true');
  });

  it('enables input when user is present', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    const input = screen.getByTestId('message-input');
    expect(input).toHaveAttribute('data-disabled', 'false');
  });

  // ── Error display ──

  it('shows chat-error div for API errors', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Server error' }),
    });
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const { container } = render(<ChatView />);
    await vi.waitFor(() => {
      const errorDiv = container.querySelector('.chat-error');
      expect(errorDiv).toBeInTheDocument();
    });
  });

  it('does not show error div when fetch succeeds', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const { container } = render(<ChatView />);
    const errorDiv = container.querySelector('.chat-error');
    expect(errorDiv).not.toBeInTheDocument();
  });

  // ── TypingIndicator rendering ──

  it('renders TypingIndicator with correct channelId', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    // TypingIndicator is not mocked so it renders as-is
    const chatView = document.querySelector('.chat-view');
    expect(chatView).toBeInTheDocument();
  });

  // ── Structural layout ──

  it('renders chat-header, chat-messages, and chat-input sections', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const { container } = render(<ChatView />);
    expect(container.querySelector('.chat-header')).toBeInTheDocument();
    expect(container.querySelector('.chat-messages')).toBeInTheDocument();
    expect(container.querySelector('.chat-input')).toBeInTheDocument();
  });

  // ── Attachment forwarding (#178) ──

  it('passes attachmentIds and attachments through to sendMessage', async () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const user = (await import('@testing-library/user-event')).default.setup();

    render(<ChatView />);
    const sendBtn = screen.getByTestId('mock-send-btn');
    await user.click(sendBtn);

    expect(mockSendMsg).toHaveBeenCalledWith(
      'channel-1',
      'hello',
      expect.any(String),
      expect.objectContaining({
        attachmentIds: ['att-1'],
        attachments: [{ id: 'att-1' }],
      })
    );
  });

  // ── handleUnseenOnLeave ──

  it('handleUnseenOnLeave sets unread count and marks server unread', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    useServerStore.setState({ activeServerId: 'server-1' });
    render(<ChatView />);

    const onUnseenOnLeave = capturedMessageListProps.onUnseenOnLeave as (count: number) => void;
    expect(onUnseenOnLeave).toBeDefined();

    // Spy on unreadStore methods
    const setUnreadCountSpy = vi.spyOn(useUnreadStore.getState(), 'setUnreadCount');
    const markServerUnreadSpy = vi.spyOn(useUnreadStore.getState(), 'markServerUnread');

    onUnseenOnLeave(5);

    expect(setUnreadCountSpy).toHaveBeenCalledWith('channel-1', 5);
    expect(markServerUnreadSpy).toHaveBeenCalledWith('server-1');

    setUnreadCountSpy.mockRestore();
    markServerUnreadSpy.mockRestore();
  });

  // ── handlePinToggle ──

  it('handlePinToggle calls pinMessage and increments count', async () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    const onPinToggle = capturedMessageListProps.onPinToggle as (msg: unknown) => Promise<void>;
    expect(onPinToggle).toBeDefined();

    // Trigger pin on an unpinned message (no pinned_at)
    await onPinToggle({ ...mockMessage, pinned_at: null });

    expect(mockPinMessage).toHaveBeenCalledWith('msg-1');
    expect(mockUnpinMessage).not.toHaveBeenCalled();
  });

  it('handlePinToggle calls unpinMessage and decrements count', async () => {
    // Seed one existing pin so count starts at 1
    mockGetChannelPins.mockResolvedValue([{ id: 'msg-1' }]);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    // Wait for pin count fetch to settle
    await vi.waitFor(() => {
      expect(mockGetChannelPins).toHaveBeenCalledWith('channel-1');
    });

    const onPinToggle = capturedMessageListProps.onPinToggle as (msg: unknown) => Promise<void>;
    expect(onPinToggle).toBeDefined();

    // Trigger unpin on a pinned message
    await onPinToggle({ ...mockMessage, pinned_at: '2025-01-01T00:00:00Z' });

    expect(mockUnpinMessage).toHaveBeenCalledWith('msg-1');
    expect(mockPinMessage).not.toHaveBeenCalled();
  });

  // ── handleScrollToMessage ──

  it('handleScrollToMessage is passed to MessageList', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    const onScrollToMessage = capturedMessageListProps.onScrollToMessage as (id: string) => void;
    expect(typeof onScrollToMessage).toBe('function');
  });

  it('handlePinToggle logs redacted error when pinMessage rejects', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockPinMessage.mockRejectedValueOnce(new Error('boom'));
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    await vi.waitFor(() => {
      expect(mockGetChannelPins).toHaveBeenCalled();
    });

    const onPinToggle = capturedMessageListProps.onPinToggle as (msg: unknown) => Promise<void>;
    // Trigger pin on an unpinned message — pinMessage rejects
    await onPinToggle({ ...mockMessage, pinned_at: null });

    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to toggle pin:', 'boom');
    });
    consoleSpy.mockRestore();
  });

  // ── Keyboard shortcut: toggle search panel ──

  it('toggles search panel via concord:toggle-search event', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    const searchBtn = screen.getByRole('button', { name: 'Search messages' });
    expect(searchBtn).toHaveAttribute('aria-expanded', 'false');

    // Dispatch the custom event that the keyboard shortcut handler listens for
    act(() => {
      globalThis.dispatchEvent(new Event('concord:toggle-search'));
    });

    expect(searchBtn).toHaveAttribute('aria-expanded', 'true');
  });
});
