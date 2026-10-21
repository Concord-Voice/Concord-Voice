import { render, screen, act, waitFor, userEvent, within } from '../../../test-utils';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { MANAGE_CHANNELS, PIN_MESSAGES } from '@/renderer/utils/policy/permissions';
import { mockUser, mockChannel, mockMessage, mockMessage2 } from '../../../mocks/fixtures';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock apiFetch to prevent hanging fetches
const mockApiFetch = vi.fn().mockResolvedValue({
  ok: true,
  json: async () => ({ messages: [] }),
});
vi.mock('@/renderer/services/system/apiClient', () => ({
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
vi.mock('@/renderer/services/messaging/pinService', () => ({
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

// Record the props the owner hands the panel (the right surface id and jump handler, #3894) and
// render the REAL panel under them: the focus-return tests below press its Escape and Close.
let capturedSearchPanelProps: {
  surfaceId?: string;
  onJump?: (channelId: string, messageId: string, signal?: AbortSignal) => Promise<string>;
} = {};
vi.mock('@/renderer/components/Chat/SearchPanel', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/components/Chat/SearchPanel')>();
  const RealSearchPanel = actual.default;
  return {
    ...actual,
    default: (props: React.ComponentProps<typeof RealSearchPanel>) => {
      capturedSearchPanelProps = props;
      return <RealSearchPanel {...props} />;
    },
  };
});

// Capture MessageInput props so a test can invoke the callbacks the composer owner passes in
// (the real composer is mocked, so ArrowUp cannot be pressed on a textarea here).
let capturedMessageInputProps: { onEditLastMessage?: () => void } = {};

vi.mock('@/renderer/components/Chat/MessageInput', () => ({
  default: (props: {
    channelName?: string;
    disabled: boolean;
    isChannelEncrypted?: boolean;
    serverId?: string;
    channelId?: string;
    onSendMessage?: (...args: unknown[]) => void;
    onEditLastMessage?: () => void;
    // The composer renders this slot, so a mock that drops it makes the
    // indicator invisible to every assertion with no error to explain why.
    expirationClause?: string | null;
  }) => {
    capturedMessageInputProps = props;
    const {
      channelName,
      disabled,
      isChannelEncrypted,
      serverId,
      channelId,
      onSendMessage,
      expirationClause,
    } = props;
    return (
      <div
        data-testid="message-input"
        data-channel={channelName}
        data-disabled={disabled}
        data-encrypted={isChannelEncrypted}
        data-server-id={serverId}
        data-channel-id={channelId}
      >
        <span>{`Messages are Encrypted End-to-End${expirationClause ?? ''}`}</span>
        <button
          data-testid="mock-send-btn"
          onClick={() =>
            onSendMessage?.('hello', undefined, undefined, ['att-1'], [{ id: 'att-1' }])
          }
        >
          Send
        </button>
      </div>
    );
  },
}));

// Mock hooks
vi.mock('@/renderer/hooks/messaging/useChannelSubscription', () => ({
  useChannelSubscription: vi.fn(),
}));

const mockSendMsg = vi.fn();
vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: () => ({
    sendMessage: mockSendMsg,
    markDelivered: vi.fn(),
    sendTyping: vi.fn(),
    getPendingCount: vi.fn(() => 0),
    getPendingMessagesForChannel: vi.fn(() => []),
  }),
}));

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    createChannelOperationGuard: vi.fn(() => ({ assertCurrent: vi.fn() })),
    decryptForChannel: vi.fn(),
    getChannelKey: vi.fn(),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

import ChatView from '@/renderer/components/Chat/ChatView';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('ChatView', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockGetChannelPins.mockReset();
    mockPinMessage.mockReset();
    mockUnpinMessage.mockReset();
    capturedMessageListProps = {};
    capturedMessageInputProps = {};
    capturedSearchPanelProps = {};
    useUserStore.setState({ user: mockUser });
    useChannelStore.setState({ channels: [mockChannel], activeChannelId: null });
    useServerStore.setState({ activeServerId: 'server-1' });
    useChannelStore.setState({ currentServerId: 'server-1' });
    useChatStore.setState({
      messagesByChannel: new Map(),
      isConnected: true,
    });
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
    mockGetChannelPins.mockResolvedValue([]);
    mockPinMessage.mockResolvedValue({
      message_id: 'msg-1',
      pinned_at: '2025-01-01T00:00:00Z',
      pinned_by: 'user-1',
    });
    mockUnpinMessage.mockResolvedValue({ message_id: 'msg-1' });
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

  it('forwards history readiness from loading through settled fetch', async () => {
    const request = deferred<{ ok: boolean; json: () => Promise<unknown> }>();
    mockApiFetch.mockImplementationOnce(() => request.promise);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    expect(capturedMessageListProps).toHaveProperty('isHistoryReady', false);

    await act(async () => {
      request.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    await waitFor(() => expect(capturedMessageListProps).toHaveProperty('isHistoryReady', true));
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

  it('enables server-channel pinning from the channel server id when activeServerId is stale', () => {
    useServerStore.setState({ activeServerId: null });
    usePermissionStore.setState({ serverPermissions: { 'server-1': PIN_MESSAGES } });
    useChannelStore.setState({ activeChannelId: 'channel-1' });

    render(<ChatView />);

    expect(capturedMessageListProps.canPin).toBe(true);
  });

  it('falls back to activeServerId for pin permission when channel metadata is unavailable', () => {
    useChannelStore.setState({ channels: [], activeChannelId: 'channel-unknown' });
    useServerStore.setState({ activeServerId: 'server-1' });
    usePermissionStore.setState({ serverPermissions: { 'server-1': PIN_MESSAGES } });

    render(<ChatView />);

    expect(capturedMessageListProps.canPin).toBe(true);
  });

  it('disables pin permission when no server context is available', () => {
    useChannelStore.setState({ channels: [], activeChannelId: 'channel-unknown' });
    useServerStore.setState({ activeServerId: null });
    usePermissionStore.setState({ serverPermissions: { 'server-1': PIN_MESSAGES } });

    render(<ChatView />);

    expect(capturedMessageListProps.canPin).toBe(false);
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

  it('handleUnseenOnLeave sets unread count and marks an unmuted server unread (precise)', () => {
    useNotificationPrefsStore.getState().clearAll();
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
    // A concrete per-channel count is mute-resolved, so it marks the server
    // PRECISE, so a channel-wins unmute under a muted server still lights the dot.
    expect(markServerUnreadSpy).toHaveBeenCalledWith('server-1', true);

    setUnreadCountSpy.mockRestore();
    markServerUnreadSpy.mockRestore();
  });

  it('handleUnseenOnLeave does not light the server dot when the channel is muted', () => {
    // A muted channel's leftover unread must not light the server (#84 P2).
    useNotificationPrefsStore.getState().clearAll();
    useNotificationPrefsStore.getState().setMute('channel', 'channel-1', true, null);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    useServerStore.setState({ activeServerId: 'server-1' });
    render(<ChatView />);

    const onUnseenOnLeave = capturedMessageListProps.onUnseenOnLeave as (count: number) => void;
    const setUnreadCountSpy = vi.spyOn(useUnreadStore.getState(), 'setUnreadCount');
    const markServerUnreadSpy = vi.spyOn(useUnreadStore.getState(), 'markServerUnread');

    onUnseenOnLeave(5);

    // The per-channel badge is still recorded (revealed on un-mute)...
    expect(setUnreadCountSpy).toHaveBeenCalledWith('channel-1', 5);
    // ...but the server dot stays dark.
    expect(markServerUnreadSpy).not.toHaveBeenCalled();

    useNotificationPrefsStore.getState().clearAll();
    setUnreadCountSpy.mockRestore();
    markServerUnreadSpy.mockRestore();
  });

  // ── onLatestSeen (read marker while viewing, #2006) ──

  it('advances the read marker via useReadMarker when onLatestSeen fires', async () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    const onLatestSeen = capturedMessageListProps.onLatestSeen as (() => void) | undefined;
    expect(onLatestSeen).toBeDefined();

    vi.useFakeTimers();
    try {
      act(() => onLatestSeen?.());
      expect(mockApiFetch).not.toHaveBeenCalledWith(
        expect.stringContaining('/read'),
        expect.anything()
      );

      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/channels/channel-1/read',
        expect.objectContaining({ method: 'POST' })
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('flushes the pending read marker for the channel that was read when the channel changes', async () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    const { rerender } = render(<ChatView />);
    vi.useFakeTimers();
    try {
      act(() => (capturedMessageListProps.onLatestSeen as () => void)?.());
      act(() => {
        useChannelStore.setState({ activeChannelId: 'channel-2' });
      });
      rerender(<ChatView />);
      // The old channel's post fired at once, with the old id captured.
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/channels/channel-1/read',
        expect.objectContaining({ method: 'POST' })
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
      expect(mockApiFetch).not.toHaveBeenCalledWith(
        '/api/v1/channels/channel-2/read',
        expect.anything()
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('flushes the pending read marker at once when onLatestLeft fires', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    vi.useFakeTimers();
    try {
      act(() => (capturedMessageListProps.onLatestSeen as () => void)?.());
      expect(mockApiFetch).not.toHaveBeenCalledWith(
        expect.stringContaining('/read'),
        expect.anything()
      );
      act(() => (capturedMessageListProps.onLatestLeft as () => void)?.());
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/channels/channel-1/read',
        expect.objectContaining({ method: 'POST' })
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('logs a non-2xx read marker response instead of treating it as success', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockApiFetch.mockImplementation((url: string) =>
      url.endsWith('/read')
        ? Promise.resolve({ ok: false, status: 429, json: async () => ({}) })
        : Promise.resolve({ ok: true, json: async () => ({ messages: [] }) })
    );
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    vi.useFakeTimers();
    try {
      act(() => (capturedMessageListProps.onLatestSeen as () => void)?.());
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
      expect(consoleSpy).toHaveBeenCalledWith(
        '[useReadMarker] Failed to post read marker:',
        'read marker rejected: HTTP 429'
      );
    } finally {
      vi.useRealTimers();
      consoleSpy.mockRestore();
    }
  });

  it('clears the local unread count when latest-seen fires with a nonzero count', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    useUnreadStore.getState().setUnreadCount('channel-1', 4);
    render(<ChatView />);

    const clearUnreadSpy = vi.spyOn(useUnreadStore.getState(), 'clearUnread');
    const onLatestSeen = capturedMessageListProps.onLatestSeen as (() => void) | undefined;
    act(() => onLatestSeen?.());

    expect(clearUnreadSpy).toHaveBeenCalledWith('channel-1');
    clearUnreadSpy.mockRestore();
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

  it.each([
    ['matching channel', 'channel-1'],
    ['server-wide purge', null],
  ] as const)('clears stale pin counts for a %s purge', async (_label, scopeId) => {
    const oldLoad = deferred<Array<{ id: string }>>();
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetChannelPins.mockReset();
    mockGetChannelPins.mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(freshLoad.promise);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(1));

    await act(async () => {
      globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId } }));
      oldLoad.resolve([{ id: 'old-pin' }]);
      await oldLoad.promise;
      expect(mockGetChannelPins).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(2));
    expect(document.querySelector('.pin-count-badge')).toBeNull();
    await act(async () => freshLoad.resolve([]));
  });

  it('ignores an unrelated explicit purge scope for the pin count', async () => {
    const oldLoad = deferred<Array<{ id: string }>>();
    mockGetChannelPins.mockReset();
    mockGetChannelPins.mockReturnValueOnce(oldLoad.promise);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(1));
    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'other-channel' } })
      );
    });
    expect(mockGetChannelPins).toHaveBeenCalledTimes(1);
    await act(async () => oldLoad.resolve([{ id: 'old-pin' }]));
    await waitFor(() => expect(document.querySelector('.pin-count-badge')).toHaveTextContent('1'));
  });

  it('clears a loaded pin badge immediately on purge', async () => {
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetChannelPins.mockReset();
    mockGetChannelPins
      .mockReturnValueOnce(Promise.resolve([{ id: 'old-pin' }]))
      .mockReturnValueOnce(freshLoad.promise);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    await waitFor(() => expect(document.querySelector('.pin-count-badge')).toHaveTextContent('1'));
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'channel-1' } })
      )
    );
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(2));
    expect(document.querySelector('.pin-count-badge')).toBeNull();
    await act(async () => freshLoad.resolve([]));
  });

  it('keeps a fresh pin badge when the stale count request rejects afterward', async () => {
    const oldLoad = deferred<Array<{ id: string }>>();
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetChannelPins.mockReset();
    mockGetChannelPins.mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(freshLoad.promise);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'channel-1' } })
      )
    );
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(2));
    await act(async () => freshLoad.resolve([{ id: 'fresh-pin' }]));
    await waitFor(() => expect(document.querySelector('.pin-count-badge')).toHaveTextContent('1'));
    await act(async () => oldLoad.reject(new Error('stale')));
    expect(document.querySelector('.pin-count-badge')).toHaveTextContent('1');
  });

  it('keeps the fresh pin count when a pre-purge pin succeeds late', async () => {
    const pendingPin = deferred<unknown>();
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetChannelPins.mockReset();
    mockGetChannelPins
      .mockReturnValueOnce(Promise.resolve([]))
      .mockReturnValueOnce(freshLoad.promise);
    mockPinMessage.mockReturnValue(pendingPin.promise);
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(1));

    const toggle = capturedMessageListProps.onPinToggle as (msg: unknown) => Promise<void>;
    const mutation = toggle({ ...mockMessage, pinned_at: null });
    await waitFor(() => expect(mockPinMessage).toHaveBeenCalledWith('msg-1'));
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'channel-1' } })
      )
    );
    await waitFor(() => expect(mockGetChannelPins).toHaveBeenCalledTimes(2));
    await act(async () => freshLoad.resolve([{ id: 'fresh-pin' }]));
    await waitFor(() => expect(document.querySelector('.pin-count-badge')).toHaveTextContent('1'));

    await act(async () => pendingPin.resolve({}));
    await mutation;
    expect(document.querySelector('.pin-count-badge')).toHaveTextContent('1');
  });

  // ── handleScrollToMessage ──

  it('handleScrollToMessage is passed to MessageList', () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);

    const onScrollToMessage = capturedMessageListProps.onScrollToMessage as (id: string) => void;
    expect(typeof onScrollToMessage).toBe('function');
  });

  // ── Jump routing (#3894) ──

  describe('jump routing (#3894)', () => {
    const MISSING_COPY = "Couldn't reach that message. It may have been deleted or expired.";
    const FAILED_COPY =
      "Couldn't reach that message. Loading older messages failed. Try again in a moment.";

    /** 50 rows, newest first as the server sends them, so the oldest id is row-49. */
    const initialRows = Array.from({ length: 50 }, (_, i) => ({
      ...mockMessage,
      id: `row-${i}`,
      created_at: new Date(Date.UTC(2025, 0, 1, 12, 0, 0) - i * 60_000).toISOString(),
    }));

    /**
     * A full first page (so more history exists) and a one-row jump page. The fake list handle
     * reports the target as mounted only after the jump page has been requested.
     */
    function installHistory(jumpPageRows: unknown[]) {
      const pageUrls: string[] = [];
      const fakeList = {
        mounted: false,
        scrollToMessage: vi.fn((_id: string) => fakeList.mounted),
        scrollToBottomIfNear: vi.fn(),
      };
      mockApiFetch.mockImplementation(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes('/channels/channel-1/messages')) {
          if (url.includes('before=')) {
            pageUrls.push(url);
            fakeList.mounted = true;
            return { ok: true, json: async () => ({ messages: jumpPageRows }) } as Response;
          }
          return { ok: true, json: async () => ({ messages: initialRows }) } as Response;
        }
        return { ok: true, json: async () => ({ messages: [] }) } as Response;
      });
      useChannelStore.setState({ activeChannelId: 'channel-1' });
      render(<ChatView />);
      return { pageUrls, fakeList };
    }

    async function settleInitialHistory(fakeList: { scrollToMessage: unknown }) {
      await waitFor(() => {
        expect(capturedMessageListProps).toHaveProperty('isHistoryReady', true);
        expect((capturedMessageListProps.messages as unknown[]).length).toBe(50);
      });
      // The list is mocked and holds no handle of its own: hand the owner's ref ours.
      const ref = capturedMessageListProps.ref as { current: unknown } | undefined;
      expect(ref).toBeDefined();
      if (ref) ref.current = fakeList;
    }

    function jumpNotice() {
      const notice = document.querySelector('.jump-notice');
      expect(notice).toBeInTheDocument();
      return notice as HTMLElement;
    }

    // kills: handler not routed through the hook
    it('G1 a reply-quote click asks the list handle, then pages older history through loadUntil', async () => {
      const { pageUrls, fakeList } = installHistory([{ ...mockMessage, id: 'jump-target' }]);
      await settleInitialHistory(fakeList);

      act(() => {
        (capturedMessageListProps.onScrollToMessage as (id: string) => void)('jump-target');
      });

      // Reached: the list handle was asked, and it said the row is not mounted yet.
      await waitFor(() => expect(fakeList.scrollToMessage).toHaveBeenCalledWith('jump-target'));
      expect(fakeList.scrollToMessage.mock.results[0]?.value).toBe(false);
      // Obeyed: loadUntil went out for the older page, from the oldest loaded row, 100 deep.
      await waitFor(() => expect(pageUrls).toHaveLength(1));
      expect(pageUrls[0]).toContain('before=row-49');
      expect(pageUrls[0]).toContain('limit=100');
      // The jump then finds the row mounted and ends without a notice.
      await waitFor(() => expect(fakeList.scrollToMessage.mock.results.at(-1)?.value).toBe(true));
      expect(jumpNotice()).toBeEmptyDOMElement();
    });

    // kills: handler not routed through the hook
    it('G1 a pinned-message Jump click takes the same route', async () => {
      mockGetChannelPins.mockResolvedValue([{ ...mockMessage, id: 'jump-target' }]);
      const { pageUrls, fakeList } = installHistory([{ ...mockMessage, id: 'jump-target' }]);
      await settleInitialHistory(fakeList);
      const user = userEvent.setup();

      await user.click(screen.getByRole('button', { name: 'Pinned messages' }));
      await user.click(await screen.findByRole('button', { name: 'Jump' }));

      await waitFor(() => expect(fakeList.scrollToMessage).toHaveBeenCalledWith('jump-target'));
      await waitFor(() => expect(pageUrls).toHaveLength(1));
      expect(pageUrls[0]).toContain('before=row-49');
      await waitFor(() => expect(fakeList.scrollToMessage.mock.results.at(-1)?.value).toBe(true));
      expect(jumpNotice()).toBeEmptyDOMElement();
    });

    // kills: JumpNotice not rendered by the owner
    it('G1 a jump that cannot finish says why in the notice under the list', async () => {
      const { pageUrls, fakeList } = installHistory([]);
      await settleInitialHistory(fakeList);

      act(() => {
        (capturedMessageListProps.onScrollToMessage as (id: string) => void)('gone');
      });

      await waitFor(() => expect(pageUrls).toHaveLength(1));
      await waitFor(() => expect(jumpNotice()).toHaveTextContent(MISSING_COPY));
    });

    // kills: search jump not marked inline
    it('G5 a same-channel search jump that fails returns the failure and leaves the line to the search card', async () => {
      const { pageUrls, fakeList } = installHistory([]);
      await settleInitialHistory(fakeList);
      let outcome: string | undefined;

      // The jump settles through effects, which React flushes only when an act scope returns, so
      // awaiting it inside an async act deadlocks: start it synchronously and poll for the outcome.
      act(() => {
        void capturedSearchPanelProps
          .onJump?.('channel-1', 'gone', new AbortController().signal)
          ?.then((o) => {
            outcome = o;
          });
      });

      await waitFor(() => expect(outcome).toBe('missing'));
      // Positive control: the jump paged and failed, so a notice was possible.
      expect(pageUrls).toHaveLength(1);
      expect(jumpNotice()).toBeEmptyDOMElement();
    });

    // kills: initial-history-error gate ignores the loaded list
    it('G8 an error beside loaded messages does not block a jump', async () => {
      const { pageUrls, fakeList } = installHistory([{ ...mockMessage, id: 'jump-target' }]);
      await settleInitialHistory(fakeList);
      const base = mockApiFetch.getMockImplementation();
      if (!base) throw new Error('installHistory set no fetch');
      let refused = 0;
      mockApiFetch.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (refused === 0 && String(input).includes('before=')) {
          refused += 1;
          throw new Error('network down');
        }
        return base(input, init);
      });
      // A failed older-page load sets `error` while all 50 rows stay loaded.
      await act(async () => {
        await (capturedMessageListProps.onLoadMore as () => Promise<void>)();
      });
      expect(refused).toBe(1);
      expect(pageUrls).toHaveLength(0);

      act(() => {
        (capturedMessageListProps.onScrollToMessage as (id: string) => void)('jump-target');
      });

      await waitFor(() => expect(pageUrls).toHaveLength(1));
      await waitFor(() => expect(fakeList.scrollToMessage.mock.results.at(-1)?.value).toBe(true));
      expect(jumpNotice()).toBeEmptyDOMElement();
    });

    // kills: initial-history-error gate ignores the empty list
    it('G8 an error with no messages ends the jump failed without paging', async () => {
      const urls: string[] = [];
      mockApiFetch.mockImplementation(async (input: RequestInfo | URL) => {
        urls.push(String(input));
        throw new Error('network down');
      });
      useChannelStore.setState({ activeChannelId: 'channel-1' });
      render(<ChatView />);
      await waitFor(() => expect(capturedMessageListProps).toHaveProperty('isHistoryReady', true));
      expect(capturedMessageListProps.messages as unknown[]).toHaveLength(0);
      const fakeList = { scrollToMessage: vi.fn(() => false), scrollToBottomIfNear: vi.fn() };
      const ref = capturedMessageListProps.ref as { current: unknown } | undefined;
      expect(ref).toBeDefined();
      if (ref) ref.current = fakeList;

      act(() => {
        (capturedMessageListProps.onScrollToMessage as (id: string) => void)('gone');
      });

      await waitFor(() => expect(jumpNotice()).toHaveTextContent(FAILED_COPY));
      expect(urls.filter((u) => u.includes('before='))).toHaveLength(0);
    });

    describe('G2 the search panel and switchChannel', () => {
      const voice = { ...mockChannel, id: 'voice-1', name: 'lounge', type: 'voice' as const };
      const bulletin = {
        ...mockChannel,
        id: 'bulletin-1',
        name: 'news',
        type: 'bulletin' as const,
      };
      const elsewhere = { ...mockChannel, id: 'other-1', name: 'away', server_id: 'server-2' };

      beforeEach(() => {
        useChannelStore.setState({
          channels: [mockChannel, voice, bulletin, elsewhere],
          activeChannelId: 'channel-1',
        });
      });

      // kills: surface id or onJump not passed to the panel
      it('G2 hands the panel this surface id and a jump handler', () => {
        render(<ChatView />);

        expect(typeof capturedSearchPanelProps.onJump).toBe('function');
        expect(capturedSearchPanelProps.surfaceId).toBeTruthy();
        const root = document.querySelector('[data-chat-surface]');
        expect(root).toHaveAttribute('data-chat-surface', capturedSearchPanelProps.surfaceId);
      });

      // kills: type check inverted
      it('G2 accepts a bulletin channel of the active server', async () => {
        render(<ChatView />);
        expect(useChannelStore.getState().activeChannelId).toBe('channel-1');

        await act(async () => {
          void capturedSearchPanelProps.onJump?.('bulletin-1', 'm1');
        });

        expect(useChannelStore.getState().activeChannelId).toBe('bulletin-1');
      });

      // kills: type check inverted
      it.each([
        ['a voice channel', 'voice-1'],
        ["another server's channel", 'other-1'],
        ['a channel that is not loaded', 'nowhere'],
      ])('G2 refuses %s: missing, and the chat stays put', async (_name, targetId) => {
        render(<ChatView />);
        let outcome: string | undefined;

        await act(async () => {
          outcome = await capturedSearchPanelProps.onJump?.(targetId, 'm1');
        });

        expect(outcome).toBe('missing');
        expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
      });
    });
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

  it('returns focus to the Search messages button when the shortcut closes the panel from inside it', async () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    act(() => {
      globalThis.dispatchEvent(new Event('concord:toggle-search'));
    });
    const input = screen.getByPlaceholderText('Search messages...');
    await waitFor(() => expect(input).toHaveFocus());

    act(() => {
      globalThis.dispatchEvent(new Event('concord:toggle-search'));
    });

    expect(screen.queryByPlaceholderText('Search messages...')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Search messages' })).toHaveFocus();
  });

  it('leaves focus where it is when the shortcut closes the panel from outside it', async () => {
    useChannelStore.setState({ activeChannelId: 'channel-1' });
    render(<ChatView />);
    act(() => {
      globalThis.dispatchEvent(new Event('concord:toggle-search'));
    });
    await waitFor(() => expect(screen.getByPlaceholderText('Search messages...')).toHaveFocus());
    const outside = document.createElement('button');
    document.body.append(outside);
    act(() => outside.focus());

    act(() => {
      globalThis.dispatchEvent(new Event('concord:toggle-search'));
    });

    expect(screen.queryByPlaceholderText('Search messages...')).not.toBeInTheDocument();
    expect(outside).toHaveFocus();
    outside.remove();
  });

  // At the CALLER: the panel can return focus only to a control its owner hands it.
  it.each([
    ['Escape', async (user: ReturnType<typeof userEvent.setup>) => user.keyboard('{Escape}')],
    [
      'the Close button',
      async (user: ReturnType<typeof userEvent.setup>) =>
        user.click(screen.getByRole('button', { name: 'Close' })),
    ],
  ])(
    'returns focus to the Search messages button when %s closes the search panel',
    async (_, close) => {
      useChannelStore.setState({ activeChannelId: 'channel-1' });
      const user = userEvent.setup();
      render(<ChatView />);

      const searchBtn = screen.getByRole('button', { name: 'Search messages' });
      await user.click(searchBtn);
      await waitFor(() => expect(screen.getByPlaceholderText('Search messages...')).toHaveFocus());

      await close(user);

      expect(screen.queryByPlaceholderText('Search messages...')).not.toBeInTheDocument();
      expect(searchBtn).toHaveAttribute('aria-expanded', 'false');
      expect(searchBtn).toHaveFocus();
    }
  );

  // #3459 at the CALLER: the clause helper and the panel note are each unit-tested, but only the
  // owner can fail to hand them the policy. Both cells vary the one input (includePinned) through
  // the real hook, store and list-row parser.
  describe.each([
    [
      'kept',
      { expiration_include_pinned: false },
      'Messages are Encrypted End-to-End and expire after 24 hours, except pinned messages',
      true,
    ],
    [
      'expiring',
      { expiration_include_pinned: true },
      'Messages are Encrypted End-to-End and expire after 24 hours',
      false,
    ],
    [
      'unsaid (a server that predates #3459)',
      {},
      'Messages are Encrypted End-to-End and expire after 24 hours',
      false,
    ],
  ])('pin retention when pins are %s (#3459)', (_name, rowFields, sentence, noteShown) => {
    it('reaches the composer sentence and the pinned panel note', async () => {
      const row = {
        ...mockChannel,
        expiration_window_seconds: 86400,
        expiration_updated_at: '2026-09-08T05:00:00.000Z',
        expiration_revision: 4,
        expiration_backfill_pending: false,
        ...rowFields,
      };
      useChannelStore.setState({
        channels: [
          {
            ...mockChannel,
            expirationPolicy: {
              windowSeconds: 86400 as const,
              updatedAt: '2026-09-08T05:00:00.000Z',
              revision: 4,
              backfillPending: false,
              ...('expiration_include_pinned' in rowFields
                ? { includePinned: rowFields.expiration_include_pinned }
                : {}),
            },
          },
        ],
        activeChannelId: mockChannel.id,
      });
      mockApiFetch.mockImplementation(async (input: RequestInfo | URL) => {
        if (String(input).includes('/servers/server-1/channels')) {
          return { ok: true, json: async () => ({ channels: [row] }) } as Response;
        }
        return { ok: true, json: async () => ({ messages: [] }) } as Response;
      });
      render(<ChatView />);
      const user = userEvent.setup();
      await screen.findByText(sentence);

      await user.click(screen.getByRole('button', { name: 'Pinned messages' }));
      const note = /Pinned messages are kept\. Unpinning one puts it back on the timer/;
      if (noteShown) {
        expect(await screen.findByText(note)).toBeInTheDocument();
      } else {
        // Gate the negative on the panel having rendered, so "absent" is not "not rendered yet".
        await screen.findByRole('heading', { name: 'Pinned Messages' });
        expect(screen.queryByText(note)).not.toBeInTheDocument();
      }
    });
  });

  it('opens the header editor for a channel manager and survives a held reopen read', async () => {
    const channel = {
      ...mockChannel,
      expirationPolicy: {
        windowSeconds: 86400 as const,
        updatedAt: '2026-09-08T05:00:00.000Z',
        revision: 4,
        backfillPending: false,
      },
    };
    useChannelStore.setState({ channels: [channel], activeChannelId: channel.id });
    usePermissionStore.setState({
      channelPermissions: { [channel.id]: MANAGE_CHANNELS },
      serverPermissions: { 'server-1': 0n },
    });
    const reopenedRead = deferred<Response>();
    let holdReopenedRead = false;
    let channelReads = 0;
    mockApiFetch.mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input).includes('/servers/server-1/channels')) {
        channelReads += 1;
        if (holdReopenedRead) return reopenedRead.promise;
        return {
          ok: true,
          json: async () => ({
            channels: [
              {
                ...mockChannel,
                expiration_window_seconds: 86400,
                expiration_updated_at: '2026-09-08T05:00:00.000Z',
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
            ],
          }),
        } as Response;
      }
      return { ok: true, json: async () => ({ messages: [] }) } as Response;
    });
    render(<ChatView />);
    const user = userEvent.setup();
    await screen.findByText('Messages are Encrypted End-to-End and expire after 24 hours');
    const baselineReads = channelReads;
    await user.click(await screen.findByRole('button', { name: /^Message expiration:/ }));
    await waitFor(() => expect(channelReads).toBeGreaterThan(baselineReads));
    await user.click(screen.getByRole('button', { name: '7 days' }));
    // Selecting a stop stages it; Apply opens the confirmation (#1351 review).
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    const childDialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(childDialog).toBeInTheDocument();
    await user.click(within(childDialog).getByRole('button', { name: 'Close' }));
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    const parentDialog = screen.getByRole('dialog', { name: 'Message expiration' });
    await user.click(within(parentDialog).getByRole('button', { name: 'Close' }));
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Message expiration' })).not.toBeInTheDocument()
    );

    holdReopenedRead = true;
    await user.click(screen.getByRole('button', { name: /^Message expiration:/ }));
    await waitFor(() => expect(channelReads).toBeGreaterThan(baselineReads + 1));
    expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute('aria-disabled', 'true');
    try {
      await act(async () => {
        reopenedRead.resolve({
          ok: true,
          json: async () => ({
            channels: [
              {
                ...mockChannel,
                expiration_window_seconds: 86400,
                expiration_updated_at: '2026-09-08T05:00:00.000Z',
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
            ],
          }),
        } as Response);
        await reopenedRead.promise;
      });
    } finally {
      reopenedRead.resolve({ ok: true, json: async () => ({ channels: [] }) } as Response);
      await reopenedRead.promise;
    }
    await user.click(screen.getByRole('button', { name: '7 days' }));
    // Selecting a stop stages it; Apply opens the confirmation (#1351 review).
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(
      await screen.findByRole('dialog', { name: 'Change message expiration' })
    ).toBeInTheDocument();
  });

  it('keeps the successor channel editor isolated from a held timer save', async () => {
    const channelA = {
      ...mockChannel,
      id: 'channel-a',
      name: 'alpha',
      expirationPolicy: {
        windowSeconds: 3600 as const,
        updatedAt: '2026-09-08T05:00:00.000Z',
        revision: 4,
        backfillPending: false,
      },
    };
    const channelB = {
      ...mockChannel,
      id: 'channel-b',
      name: 'beta',
      expirationPolicy: {
        windowSeconds: 86400 as const,
        updatedAt: '2026-09-08T05:00:00.000Z',
        revision: 7,
        backfillPending: false,
      },
    };
    const pendingPatch = deferred<Response>();
    let patchBody: unknown;
    mockApiFetch.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/servers/server-1/channels')) {
        return {
          ok: true,
          json: async () => ({
            channels: [
              {
                ...channelA,
                expiration_window_seconds: 3600,
                expiration_updated_at: channelA.expirationPolicy.updatedAt,
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
              {
                ...channelB,
                expiration_window_seconds: 86400,
                expiration_updated_at: channelB.expirationPolicy.updatedAt,
                expiration_revision: 7,
                expiration_backfill_pending: false,
              },
            ],
          }),
        } as Response;
      }
      if (url.includes('/api/v1/channels/channel-a/expiration')) {
        patchBody = JSON.parse(String(init?.body));
        return pendingPatch.promise;
      }
      return { ok: true, json: async () => ({ messages: [] }) } as Response;
    });
    useChannelStore.setState({ channels: [channelA, channelB], activeChannelId: channelA.id });
    usePermissionStore.setState({
      channelPermissions: { [channelA.id]: MANAGE_CHANNELS, [channelB.id]: MANAGE_CHANNELS },
      serverPermissions: { 'server-1': 0n },
    });
    const user = userEvent.setup();
    render(<ChatView />);
    await user.click(await screen.findByRole('button', { name: /^Message expiration:/ }));
    await user.click(await screen.findByRole('button', { name: '7 days' }));
    // Selecting a stop stages it; Apply opens the confirmation (#1351 review).
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    const dialogA = await screen.findByRole('dialog', { name: 'Change message expiration' });
    await user.click(within(dialogA).getByRole('radio', { name: 'Only new messages' }));
    await user.click(within(dialogA).getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(within(dialogA).getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(patchBody).toEqual({ mode: 'set', window_seconds: 604800, retroactive: 'new_only' })
    );

    act(() => useChannelStore.setState({ activeChannelId: channelB.id }));
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    await user.click(await screen.findByRole('button', { name: /^Message expiration:/ }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )
    );
    await user.click(await screen.findByRole('button', { name: '30 days' }));
    // Selecting a stop stages it; Apply opens the confirmation (#1351 review).
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    const dialogB = await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(within(dialogB).getByRole('button', { name: 'Apply timer' })).toBeInTheDocument();
    await user.click(within(dialogB).getByRole('radio', { name: 'Only new messages' }));
    await user.click(within(dialogB).getByRole('checkbox', { name: /cannot be recovered/i }));
    expect(
      useChannelStore.getState().channels.find((item) => item.id === channelB.id)?.expirationPolicy
    ).toMatchObject({
      windowSeconds: 86400,
      revision: 7,
    });
    try {
      await act(async () => {
        pendingPatch.resolve({
          status: 200,
          ok: true,
          json: async () => ({
            window_seconds: 604800,
            updated_at: channelA.expirationPolicy.updatedAt,
            revision: 5,
            backfill_pending: false,
          }),
        } as Response);
        await pendingPatch.promise;
      });
    } finally {
      pendingPatch.resolve({ ok: true, json: async () => ({}) } as Response);
      await pendingPatch.promise;
    }
    expect(screen.getByRole('dialog', { name: 'Change message expiration' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.getByRole('radio', { name: 'Only new messages' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).toBeChecked();
    expect(
      useChannelStore.getState().channels.find((item) => item.id === channelB.id)?.expirationPolicy
    ).toMatchObject({
      windowSeconds: 86400,
      revision: 7,
    });
  });

  it('opens the real channel purge dialog with own-message scope', async () => {
    useChannelStore.setState({
      channels: [{ ...mockChannel, expirationPolicy: undefined }],
      activeChannelId: mockChannel.id,
      currentServerId: 'server-1',
    });
    usePermissionStore.setState({
      channelPermissions: { [mockChannel.id]: 1n << 13n },
      serverPermissions: { 'server-1': 0n },
    });
    render(<ChatView />);
    const manage = await screen.findByRole('button', { name: /^Purge messages in this/ });
    const user = userEvent.setup();
    await user.click(manage);
    const dialog = await screen.findByRole('dialog', { name: 'Purge Messages' });
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last hour');
    expect(dialog).toHaveTextContent('your messages');
  });

  it('keeps the channel purge dialog pinned to the opened channel row', async () => {
    const channel = { ...mockChannel, name: 'general', expirationPolicy: undefined };
    useChannelStore.setState({
      channels: [channel],
      activeChannelId: channel.id,
      currentServerId: 'server-1',
    });
    usePermissionStore.setState({
      channelPermissions: { [channel.id]: 1n << 14n },
      serverPermissions: { 'server-1': 0n },
    });
    render(<ChatView />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /^Purge messages in this/ }));
    const dialog = await screen.findByRole('dialog', { name: 'Purge Messages' });
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last hour');

    act(() =>
      useChannelStore.setState({
        channels: [{ ...channel, name: 'renamed' }],
      })
    );

    expect(dialog).toHaveTextContent('#general');
    expect(dialog).not.toHaveTextContent('#renamed');
  });

  it('closes channel purge consent when the effective purge scope changes', async () => {
    const channel = { ...mockChannel, name: 'general', expirationPolicy: undefined };
    useChannelStore.setState({
      channels: [channel],
      activeChannelId: channel.id,
      currentServerId: 'server-1',
    });
    usePermissionStore.setState({
      channelPermissions: { [channel.id]: 1n << 13n },
      serverPermissions: { 'server-1': 0n },
    });
    render(<ChatView />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /^Purge messages in this/ }));
    expect(await screen.findByRole('dialog', { name: 'Purge Messages' })).toBeInTheDocument();

    act(() =>
      usePermissionStore.setState({
        channelPermissions: { [channel.id]: 1n << 14n },
        serverPermissions: { 'server-1': 0n },
      })
    );

    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Purge Messages' })).not.toBeInTheDocument()
    );
  });
});

describe('ChatView Up Arrow edits the last own message (#1959)', () => {
  // A sibling describe does not inherit the outer beforeEach, so seed what ChatView needs here.
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    capturedMessageListProps = {};
    capturedMessageInputProps = {};
    useUserStore.setState({ user: mockUser });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
      currentServerId: 'server-1',
    });
    useServerStore.setState({ activeServerId: 'server-1' });
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
    mockGetChannelPins.mockReset();
    mockGetChannelPins.mockResolvedValue([]);
  });

  // The surface id the owner handed to MessageList must be the id on its root element: that
  // equality is what ties a row's edit to this panel and no other.
  function listSurfaceId(container: HTMLElement): string {
    const passed = capturedMessageListProps.surfaceId;
    expect(typeof passed).toBe('string');
    expect(passed).not.toBe('');
    expect(container.querySelector('.chat-view')?.getAttribute('data-chat-surface')).toBe(passed);
    return passed as string;
  }

  const olderOwn = { ...mockMessage, id: 'own-older', created_at: '2025-01-01T12:00:00Z' };
  const newestOwn = { ...mockMessage, id: 'own-newest', created_at: '2025-01-01T12:05:00Z' };
  const newestFromOther = {
    ...mockMessage2,
    id: 'other-newest',
    created_at: '2025-01-01T12:06:00Z',
  };

  it('hands the composer a callback that opens the newest own message for editing', () => {
    useChatStore.setState({
      messagesByChannel: new Map([['channel-1', [olderOwn, newestOwn]]]),
      isConnected: true,
    });
    const { container } = render(<ChatView />);

    // Positive gate: the wiring exists before anything is asserted about its effect.
    expect(capturedMessageInputProps.onEditLastMessage).toEqual(expect.any(Function));
    expect(useChatStore.getState().editingMessage).toBeNull();

    act(() => capturedMessageInputProps.onEditLastMessage?.());

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: listSurfaceId(container),
      messageId: 'own-newest',
    });
  });

  it('skips other authors rows and opens the user own newest message', () => {
    useChatStore.setState({
      messagesByChannel: new Map([['channel-1', [olderOwn, newestOwn, newestFromOther]]]),
      isConnected: true,
    });
    const { container } = render(<ChatView />);
    expect(capturedMessageInputProps.onEditLastMessage).toEqual(expect.any(Function));

    act(() => capturedMessageInputProps.onEditLastMessage?.());

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: listSurfaceId(container),
      messageId: 'own-newest',
    });
  });

  it('opens nothing when the thread holds no message by the current user', () => {
    useChatStore.setState({
      messagesByChannel: new Map([['channel-1', [newestFromOther]]]),
      isConnected: true,
    });
    render(<ChatView />);
    expect(capturedMessageInputProps.onEditLastMessage).toEqual(expect.any(Function));

    act(() => capturedMessageInputProps.onEditLastMessage?.());

    expect(useChatStore.getState().editingMessage).toBeNull();
  });
});
