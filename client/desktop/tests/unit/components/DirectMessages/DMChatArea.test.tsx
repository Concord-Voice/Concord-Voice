import { render, screen, waitFor, fireEvent, act, userEvent, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { mockUser } from '../../../mocks/fixtures';
import { vi } from 'vitest';

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

// Mock heavy child components — expose callbacks so tests can invoke them
let capturedMLProps: Record<string, unknown> = {};
vi.mock('@/renderer/components/Chat/MessageList', () => ({
  default: (props: {
    messages: unknown[];
    channelName?: string;
    isLoading: boolean;
    onEditMessage?: (id: string, content: string) => void;
    onDeleteMessage?: (id: string) => void;
    onUnseenOnLeave?: (count: number) => void;
    onLatestSeen?: () => void;
    onReply?: (message: unknown) => void;
    onPinToggle?: (message: unknown) => void;
    canPin?: boolean;
  }) => {
    capturedMLProps = props;
    return (
      <div
        data-testid="message-list"
        data-channel={props.channelName}
        data-loading={props.isLoading}
      >
        {(props.messages as Array<{ id: string; content: string }>).map((m) => (
          <div key={m.id} data-testid="message">
            {m.content}
          </div>
        ))}
        <button
          data-testid="trigger-edit"
          onClick={() => props.onEditMessage?.('msg-1', 'edited text')}
        >
          Edit
        </button>
        <button data-testid="trigger-delete" onClick={() => props.onDeleteMessage?.('msg-1')}>
          Delete
        </button>
        <button data-testid="trigger-unseen" onClick={() => props.onUnseenOnLeave?.(3)}>
          unseen
        </button>
        <button data-testid="trigger-unseen-zero" onClick={() => props.onUnseenOnLeave?.(0)}>
          Unseen
        </button>
        <button data-testid="trigger-latest-seen" onClick={() => props.onLatestSeen?.()}>
          latest seen
        </button>
        <button data-testid="trigger-latest-left" onClick={() => props.onLatestLeft?.()}>
          left
        </button>
      </div>
    );
  },
}));

let capturedMIProps: Record<string, unknown> = {};
vi.mock('@/renderer/components/Chat/MessageInput', () => ({
  default: (props: {
    channelName?: string;
    disabled: boolean;
    isChannelEncrypted: boolean;
    onSendMessage?: (
      content: string,
      mentionMeta?: string,
      replyToId?: string,
      attachmentIds?: string[],
      attachments?: unknown[],
      gifSlug?: string
    ) => void;
    onTyping?: (isTyping: boolean) => void;
    replyingTo?: unknown;
    onCancelReply?: () => void;
    // The composer renders this slot, so a mock that drops it makes the
    // indicator invisible to every assertion with no error to explain why.
    expirationClause?: string | null;
  }) => {
    capturedMIProps = props;
    return (
      <div
        data-testid="message-input"
        data-channel={props.channelName}
        data-disabled={props.disabled}
        data-encrypted={props.isChannelEncrypted}
      >
        <span>{`Messages are Encrypted End-to-End${props.expirationClause ?? ''}`}</span>
        <button data-testid="trigger-send" onClick={() => props.onSendMessage?.('Hello world')}>
          Send
        </button>
        <button
          data-testid="trigger-send-gif"
          onClick={() =>
            props.onSendMessage?.(' ', undefined, undefined, undefined, undefined, 'cat-wave')
          }
        >
          SendGif
        </button>
        <button
          data-testid="trigger-send-with-reply"
          onClick={() => props.onSendMessage?.('Reply msg', undefined, 'parent-msg-1')}
        >
          SendReply
        </button>
        <button data-testid="trigger-typing" onClick={() => props.onTyping?.(true)}>
          Typing
        </button>
      </div>
    );
  },
}));

vi.mock('@/renderer/components/Chat/TypingIndicator', () => ({
  default: ({ channelId }: { channelId: string }) => (
    <div data-testid="typing-indicator" data-channel={channelId} />
  ),
}));

// Mock GroupInfoPanel
vi.mock('@/renderer/components/DirectMessages/GroupInfoPanel', () => ({
  default: ({ conversation, onClose }: { conversation: { id: string }; onClose: () => void }) => (
    <div data-testid="group-info-panel" data-conv-id={conversation.id}>
      <button onClick={onClose}>Close Panel</button>
    </div>
  ),
}));

// Mock hooks
vi.mock('@/renderer/hooks/messaging/useDMSubscription', () => ({
  useDMSubscription: vi.fn(),
}));

const mockSendMessage = vi.fn();
const mockSendDMMessage = vi.fn();
vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: vi.fn(() => ({
    sendMessage: mockSendMessage,
    sendDMMessage: mockSendDMMessage,
    markDelivered: vi.fn(),
    sendTyping: vi.fn(),
    getPendingCount: vi.fn(() => 0),
    getPendingMessagesForChannel: vi.fn(() => []),
  })),
}));

const mockGetPins = vi.fn().mockResolvedValue([]);
const mockPinMessage = vi.fn().mockResolvedValue({});
const mockUnpinMessage = vi.fn().mockResolvedValue({});
vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: (...args: unknown[]) => mockPinMessage(...args),
  unpinMessage: (...args: unknown[]) => mockUnpinMessage(...args),
  getPins: (...args: unknown[]) => mockGetPins(...args),
  getChannelPins: vi.fn().mockResolvedValue([]),
}));

const mockEncryptForChannelWithVersion = vi.fn().mockResolvedValue({
  ciphertext: 'encrypted-edited',
  keyVersion: 2,
});

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    createChannelOperationGuard: vi.fn(() => ({ assertCurrent: vi.fn() })),
    decryptForChannel: vi.fn(),
    encryptForChannel: vi.fn(),
    encryptForChannelWithVersion: (...args: unknown[]) => mockEncryptForChannelWithVersion(...args),
    getChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

// Mock voiceService — the "Join voice call" affordance (#1219 R5) calls
// voiceService.joinChannel on click.
const mockJoinChannel = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/voice/voiceService', () => ({
  voiceService: {
    joinChannel: (...args: unknown[]) => mockJoinChannel(...args),
  },
}));

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: vi.fn().mockReturnValue({
    getState: vi.fn().mockReturnValue('disconnected'),
    sendMessage: vi.fn(),
    sendDMMessage: vi.fn(),
    sendTypingIndicator: vi.fn(),
    sendDMTypingIndicator: vi.fn(),
    onConnectionChange: vi.fn(() => vi.fn()),
    isSubscribed: vi.fn(() => true),
    isDMSubscribed: vi.fn(() => true),
  }),
  ConnectionState: {
    CONNECTED: 'connected',
    DISCONNECTED: 'disconnected',
    CONNECTING: 'connecting',
  },
}));

vi.mock('@/renderer/services/messaging/messageQueue', () => ({
  getMessageQueue: vi.fn().mockReturnValue({
    enqueue: vi.fn().mockReturnValue('client-msg-1'),
    markAsSent: vi.fn(),
    remove: vi.fn(),
    markAsFailed: vi.fn(),
  }),
}));

// Stub VoiceView so the in-call routing test asserts routing, not VoiceView internals (#1873).
vi.mock('@/renderer/components/Voice/VoiceView', () => ({
  default: ({ channelName }: { channelName: string }) => (
    <div data-testid="voice-view">{channelName}</div>
  ),
}));

import DMChatArea from '@/renderer/components/DirectMessages/DMChatArea';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// --- Test fixtures ---

const makeConversation = (overrides: Partial<DMConversation> = {}): DMConversation => ({
  id: 'conv-1',
  isGroup: false,
  isPersonal: false,
  name: null,
  participants: [
    { userId: 'user-1', username: 'me', displayName: 'Me', status: 'online' },
    { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'online' },
  ],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-01T00:00:00Z',
  ...overrides,
});

describe('DMChatArea', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockGetPins.mockReset();
    mockPinMessage.mockReset();
    mockUnpinMessage.mockReset();
    mockGetPins.mockResolvedValue([]);
    mockPinMessage.mockResolvedValue({});
    mockUnpinMessage.mockResolvedValue({});
    capturedMLProps = {};
    capturedMIProps = {};
    useUserStore.setState({ user: mockUser });
    useDMStore.setState({
      conversations: [],
      clearUnread: vi.fn(),
    });
    useChatStore.setState({
      messagesByChannel: new Map(),
      isConnected: true,
      addMessage: vi.fn(),
      updateMessageStatus: vi.fn(),
      updateMessage: vi.fn(),
      deleteMessage: vi.fn(),
    });
    usePrivacyStore.setState({
      settings: {
        dmPrivacyLevel: 2,
        messagesFriendsOnly: true,
        messagesServerMembers: true,
        dmFriendsOfFriends: false,
        autoAcceptFriendCodes: false,
        searchableByUsername: false,
        searchableByEmail: false,
        searchableByPhone: false,
        allowEmbeddedContent: false,
      },
    });
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
  });

  // --- Empty State (no thread selected) ---

  it('shows empty state when no thread is selected', () => {
    render(<DMChatArea selectedThreadId={null} />);
    expect(screen.getByText('Welcome to Concord Voice')).toBeInTheDocument();
    expect(
      screen.getByText('Select a message thread or server to get started')
    ).toBeInTheDocument();
  });

  it('does not render MessageList when no thread is selected', () => {
    render(<DMChatArea selectedThreadId={null} />);
    expect(screen.queryByTestId('message-list')).not.toBeInTheDocument();
  });

  it('does not render MessageInput when no thread is selected', () => {
    render(<DMChatArea selectedThreadId={null} />);
    expect(screen.queryByTestId('message-input')).not.toBeInTheDocument();
  });

  // --- Active Conversation ---

  it('renders chat header with participant name for 1:1 conversation', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('keeps a non-admin group policy review read-only', async () => {
    const group = makeConversation({
      id: 'group-1',
      isGroup: true,
      name: 'Team',
      participants: [
        { userId: 'user-1', username: 'me', role: 'member' },
        { userId: 'user-2', username: 'alice', role: 'member' },
      ],
      expirationPolicy: {
        windowSeconds: 86400,
        updatedAt: '2026-09-08T05:00:00.000Z',
        revision: 4,
        backfillPending: false,
      },
    });
    useDMStore.setState({
      conversations: [group],
    });
    mockApiFetch.mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input).includes('/dm/conversations')) {
        return {
          ok: true,
          json: async () => ({
            conversations: [
              {
                id: 'group-1',
                is_group: true,
                is_personal: false,
                name: 'Team',
                participants: [
                  { user_id: 'user-1', username: 'me', role: 'member' },
                  { user_id: 'user-2', username: 'alice', role: 'member' },
                ],
                last_message: null,
                unread_count: 0,
                created_at: '2026-09-08T05:00:00.000Z',
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
    const user = userEvent.setup();
    render(<DMChatArea selectedThreadId="group-1" />);

    // Awareness is universal, capability is permission-gated: a non-admin group
    // member reads the live window off the composer indicator and is offered no
    // entry point at all. The old read-only "Review policy" surface is gone, so
    // the absence of the header control IS the read-only guarantee.
    expect(
      await screen.findByText('Messages are Encrypted End-to-End and expire after 24 hours')
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Message expiration:/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Message expiration' })).not.toBeInTheDocument();
  });

  it('keeps a successor DM editor isolated from a held timer save', async () => {
    const policyA = {
      windowSeconds: 86400 as const,
      updatedAt: '2026-09-08T05:00:00.000Z',
      revision: 4,
      backfillPending: false,
    };
    const policyB = {
      windowSeconds: 3600 as const,
      updatedAt: '2026-09-08T05:00:00.000Z',
      revision: 6,
      backfillPending: false,
    };
    const conversationA = makeConversation({
      id: 'conv-a',
      isGroup: true,
      name: 'Alpha',
      participants: [
        { userId: 'user-1', username: 'me', displayName: 'Me', role: 'admin' },
        { userId: 'user-2', username: 'alice', displayName: 'Alice', role: 'member' },
      ],
      expirationPolicy: policyA,
    });
    const conversationB = makeConversation({
      id: 'conv-b',
      isGroup: true,
      name: 'Beta',
      participants: [
        { userId: 'user-1', username: 'me', displayName: 'Me', role: 'admin' },
        { userId: 'user-2', username: 'alice', displayName: 'Alice', role: 'member' },
      ],
      expirationPolicy: policyB,
    });
    const pendingPatch = deferred<Response>();
    let patchBody: unknown;
    mockApiFetch.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/v1/dm/conversations/conv-a/expiration')) {
        patchBody = JSON.parse(String(init?.body));
        return pendingPatch.promise;
      }
      if (url.endsWith('/api/v1/dm/conversations')) {
        return {
          ok: true,
          json: async () => ({
            conversations: [
              {
                id: 'conv-a',
                is_group: true,
                is_personal: false,
                name: 'Alpha',
                participants: [
                  { user_id: 'user-1', username: 'me', display_name: 'Me', role: 'admin' },
                  { user_id: 'user-2', username: 'alice', display_name: 'Alice', role: 'member' },
                ],
                expiration_window_seconds: 86400,
                expiration_updated_at: policyA.updatedAt,
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
              {
                id: 'conv-b',
                is_group: true,
                is_personal: false,
                name: 'Beta',
                participants: [
                  { user_id: 'user-1', username: 'me', display_name: 'Me', role: 'admin' },
                  { user_id: 'user-2', username: 'alice', display_name: 'Alice', role: 'member' },
                ],
                expiration_window_seconds: 3600,
                expiration_updated_at: policyB.updatedAt,
                expiration_revision: 6,
                expiration_backfill_pending: false,
              },
            ],
          }),
        } as Response;
      }
      return { ok: true, json: async () => ({ messages: [] }) } as Response;
    });
    useDMStore.setState({
      conversations: [conversationA, conversationB],
    });
    const user = userEvent.setup();
    const { rerender } = render(<DMChatArea selectedThreadId="conv-a" />);
    await waitFor(() =>
      expect(
        screen.getByText('Messages are Encrypted End-to-End and expire after 24 hours')
      ).toBeInTheDocument()
    );
    act(() =>
      useDMStore.getState().applyExpirationPolicy('conv-a', {
        ...policyA,
        windowSeconds: 604800,
        revision: 5,
      })
    );
    await user.click(await screen.findByRole('button', { name: /^Message expiration:/ }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '7 days' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    await user.click(screen.getByRole('button', { name: '30 days' }));
    // Selecting a stop stages it; Apply opens the confirmation (#1351 review).
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    const dialogA = await screen.findByRole('dialog', { name: 'Change message expiration' });
    await user.click(within(dialogA).getByRole('radio', { name: 'Only new messages' }));
    await user.click(within(dialogA).getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(within(dialogA).getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(patchBody).toEqual({ mode: 'set', window_seconds: 2592000, retroactive: 'new_only' })
    );

    rerender(<DMChatArea selectedThreadId="conv-b" />);
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    await user.click(await screen.findByRole('button', { name: /^Message expiration:/ }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    await user.click(screen.getByRole('button', { name: '30 days' }));
    // Selecting a stop stages it; Apply opens the confirmation (#1351 review).
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    const dialogB = await screen.findByRole('dialog', { name: 'Change message expiration' });
    await user.click(within(dialogB).getByRole('radio', { name: 'Only new messages' }));
    await user.click(within(dialogB).getByRole('checkbox', { name: /cannot be recovered/i }));
    try {
      await act(async () => {
        pendingPatch.resolve({
          status: 200,
          ok: true,
          json: async () => ({
            window_seconds: 2592000,
            updated_at: policyA.updatedAt,
            revision: 5,
            backfill_pending: false,
          }),
        } as Response);
        await pendingPatch.promise;
      });
    } finally {
      pendingPatch.resolve({ status: 200, ok: true, json: async () => ({}) } as Response);
      await pendingPatch.promise;
    }
    expect(screen.getByRole('dialog', { name: 'Change message expiration' })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'Only new messages' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).toBeChecked();
    expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute('aria-pressed', 'true');
    expect(
      useDMStore.getState().conversations.find((item) => item.id === 'conv-b')?.expirationPolicy
    ).toMatchObject({
      windowSeconds: 3600,
      revision: 6,
    });
  });

  it('renders "Conversation" when conversation is not found in store', () => {
    useDMStore.setState({ conversations: [] });
    render(<DMChatArea selectedThreadId="non-existent" />);
    // The header initial and name
    const headerName = document.querySelector('.dm-chat-header-name');
    expect(headerName?.textContent).toBe('Conversation');
  });

  it('renders MessageList when thread is selected', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByTestId('message-list')).toBeInTheDocument();
  });

  it('forwards history readiness from loading through settled fetch', async () => {
    const request = deferred<{ ok: boolean; json: () => Promise<unknown> }>();
    mockApiFetch.mockImplementationOnce(() => request.promise);
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(capturedMLProps).toHaveProperty('isHistoryReady', false);

    await act(async () => {
      request.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    await waitFor(() => expect(capturedMLProps).toHaveProperty('isHistoryReady', true));
  });

  it('renders MessageInput when thread is selected', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByTestId('message-input')).toBeInTheDocument();
  });

  it('renders TypingIndicator for active thread', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const typingIndicator = screen.getByTestId('typing-indicator');
    expect(typingIndicator).toBeInTheDocument();
    expect(typingIndicator).toHaveAttribute('data-channel', 'conv-1');
  });

  it('passes correct channel name to MessageList for 1:1 conversation', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const messageList = screen.getByTestId('message-list');
    expect(messageList).toHaveAttribute('data-channel', 'Alice');
  });

  // --- Group Conversation ---

  it('shows group name in header for group conversation', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByText('Study Group')).toBeInTheDocument();
  });

  it('shows participant names for unnamed group', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: null,
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByText('Me, Alice, Bob')).toBeInTheDocument();
  });

  it('shows member count for group conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Team',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
            { userId: 'user-3', username: 'bob' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const statusDiv = document.querySelector('.dm-chat-header-status');
    expect(statusDiv?.textContent).toBe('3 members');
  });

  // --- Personal Thread ---

  it('shows "Personal Thread" for personal conversation', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isPersonal: true,
          participants: [{ userId: 'user-1', username: 'me', displayName: 'Me' }],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByText('Personal Thread')).toBeInTheDocument();
  });

  it('shows "Your personal notes" status for personal thread', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isPersonal: true,
          participants: [{ userId: 'user-1', username: 'me' }],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const statusDiv = document.querySelector('.dm-chat-header-status');
    expect(statusDiv?.textContent).toBe('Your personal notes');
  });

  // --- Status Display ---

  it('shows participant status for 1:1 conversation', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me', status: 'online' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'idle' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const statusDiv = document.querySelector('.dm-chat-header-status');
    expect(statusDiv?.textContent).toBe('idle');
  });

  // --- Header Avatar ---

  it('renders avatar initial in header', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    const avatarInitial = container.querySelector('.conversation-avatar-initial');
    expect(avatarInitial).toBeInTheDocument();
    expect(avatarInitial?.textContent).toBe('A');
  });

  // --- Error State ---

  it('shows error banner when fetch fails', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Failed to load messages' }),
    });
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(await screen.findByText('Failed to load messages')).toBeInTheDocument();
  });

  // --- DM Privacy Level ---

  it('shows disabled notice when dmPrivacyLevel is 0', () => {
    usePrivacyStore.setState({
      settings: {
        dmPrivacyLevel: 0,
        messagesFriendsOnly: true,
        messagesServerMembers: true,
        dmFriendsOfFriends: false,
        autoAcceptFriendCodes: false,
        searchableByUsername: false,
        searchableByEmail: false,
        searchableByPhone: false,
        allowEmbeddedContent: false,
      },
    });
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(
      screen.getByText('All DMs have been disabled. Change your privacy settings to restore DMs.')
    ).toBeInTheDocument();
    // Should NOT show MessageInput
    expect(screen.queryByTestId('message-input')).not.toBeInTheDocument();
  });

  it('shows MessageInput when dmPrivacyLevel is not 0', () => {
    usePrivacyStore.setState({
      settings: {
        dmPrivacyLevel: 2,
        messagesFriendsOnly: true,
        messagesServerMembers: true,
        dmFriendsOfFriends: false,
        autoAcceptFriendCodes: false,
        searchableByUsername: false,
        searchableByEmail: false,
        searchableByPhone: false,
        allowEmbeddedContent: false,
      },
    });
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByTestId('message-input')).toBeInTheDocument();
    expect(screen.queryByText(/All DMs have been disabled/)).not.toBeInTheDocument();
  });

  // --- Encrypted Conversation ---

  // --- Message Fetch ---

  it('fetches messages for the selected thread', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(mockApiFetch).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/dm/conversations/conv-1/messages')
    );
  });

  it('marks conversation as read after fetch completes', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
    const mockClearUnread = vi.fn();
    useDMStore.setState({
      conversations: [makeConversation()],
      clearUnread: mockClearUnread,
    });
    render(<DMChatArea selectedThreadId="conv-1" />);

    await waitFor(() => {
      // Should call the read endpoint
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/dm/conversations/conv-1/read'),
        expect.objectContaining({ method: 'POST' })
      );
    });
  });

  it('logs redacted error when mark-as-read fetch rejects', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    // The POST /read call rejects; all other calls (messages fetch, the
    // /voice/participants hydration probe added in #1219 R4) succeed. Route by
    // URL so the rejection isn't coupled to call ordering (the hydration probe
    // fires alongside the messages fetch and would otherwise shift the slots).
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        return Promise.reject(new Error('boom'));
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({
      conversations: [makeConversation()],
      clearUnread: vi.fn(),
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        '[DMChatArea] Failed to mark conversation as read:',
        'boom'
      );
    });
    consoleSpy.mockRestore();
  });

  // --- Username fallback in header ---

  it('falls back to username when participant has no display name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'raw_username' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const headerName = document.querySelector('.dm-chat-header-name');
    expect(headerName?.textContent).toBe('raw_username');
  });

  // --- MessageInput disabled state ---

  it('disables MessageInput when no current user', () => {
    useUserStore.setState({ user: null });
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const messageInput = screen.getByTestId('message-input');
    expect(messageInput).toHaveAttribute('data-disabled', 'true');
  });

  // --- Group Info Toggle ---

  it('shows group info toggle button for group conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByLabelText('Toggle group info')).toBeInTheDocument();
  });

  it('does NOT show group info toggle button for 1:1 conversations', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.queryByLabelText('Toggle group info')).not.toBeInTheDocument();
  });

  it('does NOT show group info toggle button for personal threads', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isPersonal: true,
          participants: [{ userId: 'user-1', username: 'me' }],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.queryByLabelText('Toggle group info')).not.toBeInTheDocument();
  });

  it('shows GroupInfoPanel when toggle button is clicked', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);

    // Panel should not be visible initially
    expect(screen.queryByTestId('group-info-panel')).not.toBeInTheDocument();

    // Click the toggle button
    fireEvent.click(screen.getByLabelText('Toggle group info'));

    // Panel should now be visible
    expect(screen.getByTestId('group-info-panel')).toBeInTheDocument();
  });

  it('hides GroupInfoPanel when toggle button is clicked again', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);

    const toggleBtn = screen.getByLabelText('Toggle group info');

    // Open
    fireEvent.click(toggleBtn);
    expect(screen.getByTestId('group-info-panel')).toBeInTheDocument();

    // Close
    fireEvent.click(toggleBtn);
    expect(screen.queryByTestId('group-info-panel')).not.toBeInTheDocument();
  });

  it('hides GroupInfoPanel when onClose callback is invoked', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);

    // Open the panel
    fireEvent.click(screen.getByLabelText('Toggle group info'));
    expect(screen.getByTestId('group-info-panel')).toBeInTheDocument();

    // Close via the panel's own close button (which calls onClose)
    fireEvent.click(screen.getByText('Close Panel'));
    expect(screen.queryByTestId('group-info-panel')).not.toBeInTheDocument();
  });

  // --- No conversation found for selected thread ---

  it('renders header with "Conversation" and 0 members when activeConv is undefined', () => {
    useDMStore.setState({ conversations: [] });
    render(<DMChatArea selectedThreadId="non-existent-id" />);

    // Should still render chat UI (not empty state) since selectedThreadId is set
    expect(screen.getByTestId('message-list')).toBeInTheDocument();
    // Header should show fallback "Conversation"
    const headerName = document.querySelector('.dm-chat-header-name');
    expect(headerName?.textContent).toBe('Conversation');
    // Status should show "0 members" for undefined conv (falls to default branch)
    const statusDiv = document.querySelector('.dm-chat-header-status');
    expect(statusDiv?.textContent).toBe('0 members');
  });

  it('does not show group info toggle when activeConv is undefined', () => {
    useDMStore.setState({ conversations: [] });
    render(<DMChatArea selectedThreadId="non-existent-id" />);
    expect(screen.queryByLabelText('Toggle group info')).not.toBeInTheDocument();
  });

  // --- Toggle button aria attributes ---

  it('group info toggle button is labelled by aria-label alone, with no duplicate title', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);

    const btn = screen.getByLabelText('Toggle group info');
    expect(btn).toHaveAttribute('aria-label', 'Toggle group info');
    // Inverted deliberately: this asserted title="Group Info" until the header became a
    // pill row. Every direct button child of .chat-header-actions grows a
    // `content: attr(aria-label)` pill on hover, so a native title showed the same control
    // a second time ~1s later — and with DIFFERENT wording, since the two strings had
    // drifted. The ABSENCE is the invariant now.
    expect(btn).not.toHaveAttribute('title');
    expect(btn.tagName).toBe('BUTTON');
    expect(btn).toHaveAttribute('type', 'button');
  });

  // --- handleSendMessage ---

  it('sends a message via useChatController.sendMessage (delegates to useMessaging.sendDMMessage)', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-send'));

    // useChatController routes DM sends to useMessaging.sendDMMessage
    expect(mockSendDMMessage).toHaveBeenCalledWith(
      'conv-1',
      'Hello world',
      'testuser',
      expect.objectContaining({})
    );
  });

  it('preserves selected GIF slugs when sending DMs', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-send-gif'));

    expect(mockSendDMMessage).toHaveBeenCalledWith(
      'conv-1',
      ' ',
      'testuser',
      expect.objectContaining({ gifSlug: 'cat-wave' })
    );
  });

  it('does not send message when no thread is selected', async () => {
    const { getMessageQueue } = await import('@/renderer/services/messaging/messageQueue');
    const mockQueue = getMessageQueue({}) as ReturnType<typeof getMessageQueue>;

    // Render with a thread first to get the input, then we need to test null guard
    // Actually the null case means no MessageInput is rendered (empty state)
    // The guard in handleSendMessage is for safety. Test it via a group conv then deselect.
    // The simplest: render with a conversation but no user
    useUserStore.setState({ user: null });
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-send'));

    // Should not enqueue since user is null
    expect(mockQueue.enqueue).not.toHaveBeenCalled();
  });

  // Send failure/offline/encryption tests have moved to useChatController.test.ts and useMessaging.test.ts
  // since DMChatArea now delegates all send logic to useChatController → useMessaging.

  // --- handleEditMessage ---

  it('edits a message via REST API', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({
        message: { content: 'edited text', edited_at: '2025-01-02T00:00:00Z' },
      }),
    });
    const mockUpdateMessage = vi.fn();
    useChatStore.setState({ updateMessage: mockUpdateMessage });
    useDMStore.setState({ conversations: [makeConversation()] });

    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-edit'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/conv-1/messages/msg-1',
        expect.objectContaining({ method: 'PATCH' })
      );
    });
  });

  it('handles edit failure gracefully', async () => {
    // The PATCH edit call fails; all other calls (messages fetch, read mark,
    // the /voice/participants hydration probe added in #1219 R4) succeed.
    // Route by method so the failure isn't coupled to call ordering.
    mockApiFetch.mockImplementation((_url: string, opts?: { method?: string }) => {
      if (opts?.method === 'PATCH') {
        return Promise.resolve({ ok: false, json: async () => ({ error: 'Edit not allowed' }) });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });

    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useDMStore.setState({ conversations: [makeConversation()] });

    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-edit'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to edit DM message:', expect.any(String));
    });
    consoleSpy.mockRestore();
  });

  // --- handleDeleteMessage ---

  it('deletes a message via REST API', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({}),
    });
    const mockDeleteMessage = vi.fn();
    useChatStore.setState({ deleteMessage: mockDeleteMessage });
    useDMStore.setState({ conversations: [makeConversation()] });

    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-delete'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/conv-1/messages/msg-1',
        expect.objectContaining({ method: 'DELETE' })
      );
    });
  });

  it('handles delete failure gracefully', async () => {
    // Use a dynamic mock that fails on DELETE requests
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (opts?.method === 'DELETE') {
        return Promise.resolve({
          ok: false,
          json: async () => ({ error: 'Delete not allowed' }),
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });

    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useDMStore.setState({ conversations: [makeConversation()] });

    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-delete'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to delete DM message:', expect.any(String));
    });
    consoleSpy.mockRestore();
  });

  // --- handleUnseenOnLeave ---

  const readPosted = () =>
    waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/dm/conversations/conv-1/read'),
        expect.objectContaining({ method: 'POST' })
      )
    );

  it('adds the unseen count to the conversation when leaving', async () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await readPosted(); // open-time clear has happened
    fireEvent.click(screen.getByTestId('trigger-unseen'));
    // The trigger reports 3 unseen; the sidebar must show 3, not 1. The count
    // was cleared on open, and the list reports how many arrived while
    // scrolled up, so "+1" under-reports.
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(3);
  });

  it('adds over a count restored by a failed open-time read instead of replacing it', async () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await readPosted();
    // The open-time /read failed and the five the server still counts were
    // put back; three more arrived while scrolled up. The badge owes eight.
    act(() => useDMStore.getState().updateConversation('conv-1', { unreadCount: 5 }));
    fireEvent.click(screen.getByTestId('trigger-unseen'));
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(8);
  });

  it('a late open-time read failure adds the restored count over the leave-time count', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    let rejectRead: (e: Error) => void = () => {};
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        return new Promise((_resolve, reject) => {
          rejectRead = reject;
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    // The file-level beforeEach stubs clearUnread to a no-op; this scenario
    // needs the open-time clear to actually happen (5 → 0) before the restore.
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      clearUnread: (id: string) => useDMStore.getState().updateConversation(id, { unreadCount: 0 }),
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(useDMStore.getState().conversations[0].unreadCount).toBe(0));

    fireEvent.click(screen.getByTestId('trigger-unseen')); // left with 3 unseen
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(3);
    await act(async () => {
      rejectRead(new Error('boom')); // the open-time read fails only now
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(8);
    consoleSpy.mockRestore();
  });

  it('does not touch the unread count when nothing was unseen', () => {
    // clearUnread is stubbed file-wide, so the open-time clear cannot move it.
    useDMStore.setState({ conversations: [makeConversation({ unreadCount: 2 })] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    fireEvent.click(screen.getByTestId('trigger-unseen-zero'));
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(2);
  });

  // --- onLatestSeen (read marker while viewing, #2006) ---

  it('advances the read marker via useReadMarker when onLatestSeen fires', async () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);

    // Let the open-time read (handleFetchComplete) land first so it isn't
    // mistaken for the debounced post under test.
    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/dm/conversations/conv-1/read'),
        expect.objectContaining({ method: 'POST' })
      );
    });
    mockApiFetch.mockClear();

    vi.useFakeTimers();
    try {
      fireEvent.click(screen.getByTestId('trigger-latest-seen'));
      expect(mockApiFetch).not.toHaveBeenCalled();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });

      expect(mockApiFetch).toHaveBeenCalledTimes(1);
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/conv-1/read',
        expect.objectContaining({ method: 'POST' })
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('flushes the pending read marker at once when onLatestLeft fires', async () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/dm/conversations/conv-1/read'),
        expect.objectContaining({ method: 'POST' })
      );
    });
    mockApiFetch.mockClear();
    vi.useFakeTimers();
    try {
      fireEvent.click(screen.getByTestId('trigger-latest-seen'));
      expect(mockApiFetch).not.toHaveBeenCalled();
      fireEvent.click(screen.getByTestId('trigger-latest-left'));
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/conv-1/read',
        expect.objectContaining({ method: 'POST' })
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('logs a non-2xx read marker response instead of treating it as success', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    let readCalls = 0;
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        readCalls += 1;
        // The open-time read succeeds; the debounced marker is rate limited.
        return Promise.resolve({
          ok: readCalls === 1,
          status: readCalls === 1 ? 200 : 429,
          json: async () => ({}),
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(readCalls).toBe(1));
    vi.useFakeTimers();
    try {
      fireEvent.click(screen.getByTestId('trigger-latest-seen'));
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

  it('does not restore the open-time count when a later read marker already succeeded', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    let rejectOpenRead: (e: Error) => void = () => {};
    let readCalls = 0;
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        readCalls += 1;
        // The open-time read hangs and fails late; the live marker succeeds first.
        if (readCalls === 1) {
          return new Promise((_resolve, reject) => {
            rejectOpenRead = reject;
          });
        }
        return Promise.resolve({ ok: true, status: 200, json: async () => ({}) });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      clearUnread: (id: string) => useDMStore.getState().updateConversation(id, { unreadCount: 0 }),
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(useDMStore.getState().conversations[0].unreadCount).toBe(0));
    await waitFor(() => expect(readCalls).toBe(1));

    fireEvent.click(screen.getByTestId('trigger-latest-seen'));
    await act(async () => {
      fireEvent.click(screen.getByTestId('trigger-latest-left')); // flush: the live marker posts and succeeds
    });
    expect(readCalls).toBe(2);
    await act(async () => {
      rejectOpenRead(new Error('boom')); // the open-time read fails only now
    });
    // The server is current as of the live marker; nothing is owed locally.
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    consoleSpy.mockRestore();
  });

  it('posts the open-time read once per thread, not again on a reconnect refetch', async () => {
    let readCalls = 0;
    let messageFetches = 0;
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        readCalls += 1;
        return Promise.resolve({ ok: true, status: 200, json: async () => ({}) });
      }
      if (url.includes('/messages')) messageFetches += 1;
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(readCalls).toBe(1));
    const fetchesBefore = messageFetches;
    await act(async () => {
      globalThis.dispatchEvent(new Event('connection-recovered'));
    });
    await waitFor(() => expect(messageFetches).toBeGreaterThan(fetchesBefore));
    await new Promise((r) => setTimeout(r, 50));
    expect(readCalls).toBe(1);
  });

  it('does not restore the open-time count when the open-time read fails while a later marker is in flight', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const pending: Array<{ resolve: (v: unknown) => void; reject: (e: Error) => void }> = [];
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        return new Promise((resolve, reject) => {
          pending.push({ resolve, reject });
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      clearUnread: (id: string) => useDMStore.getState().updateConversation(id, { unreadCount: 0 }),
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(useDMStore.getState().conversations[0].unreadCount).toBe(0));
    await waitFor(() => expect(pending.length).toBe(1)); // the open-time read, hanging

    fireEvent.click(screen.getByTestId('trigger-latest-seen'));
    fireEvent.click(screen.getByTestId('trigger-latest-left')); // the live marker goes out, also hanging
    expect(pending.length).toBe(2);
    await act(async () => {
      pending[0].reject(new Error('boom')); // the open-time read fails first
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    await act(async () => {
      pending[1].resolve({ ok: true, status: 200, json: async () => ({}) });
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    consoleSpy.mockRestore();
  });

  it('does not restore the open-time count when it fails inside the debounce window after a seen event', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    let rejectOpenRead: (e: Error) => void = () => {};
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        return new Promise((_resolve, reject) => {
          rejectOpenRead = reject;
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      clearUnread: (id: string) => useDMStore.getState().updateConversation(id, { unreadCount: 0 }),
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(useDMStore.getState().conversations[0].unreadCount).toBe(0));
    fireEvent.click(screen.getByTestId('trigger-latest-seen')); // queued, not yet dispatched
    await act(async () => {
      rejectOpenRead(new Error('boom'));
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    consoleSpy.mockRestore();
  });

  it('rolls the local count back when the open-time read returns a non-2xx', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        return Promise.resolve({ ok: false, status: 429, json: async () => ({}) });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      clearUnread: (id: string) => useDMStore.getState().updateConversation(id, { unreadCount: 0 }),
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(useDMStore.getState().conversations[0].unreadCount).toBe(5));
    expect(consoleSpy).toHaveBeenCalledWith(
      '[DMChatArea] Failed to mark conversation as read:',
      'open-time read rejected: HTTP 429'
    );
    consoleSpy.mockRestore();
  });

  it("ignores an older open-time read's late failure once a quick return posted a newer attempt", async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    type ReadResult = { ok: boolean; status: number; json: () => Promise<unknown> };
    const reads: Array<{ url: string; resolve: (r: ReadResult) => void }> = [];
    mockApiFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (url.endsWith('/read') && opts?.method === 'POST') {
        return new Promise<ReadResult>((resolve) => {
          reads.push({ url, resolve });
        });
      }
      return Promise.resolve({ ok: true, json: async () => ({ messages: [] }) });
    });
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 }), makeConversation({ id: 'conv-2' })],
      clearUnread: (id: string) => useDMStore.getState().updateConversation(id, { unreadCount: 0 }),
    });
    // This component keeps its instance across threads: away and back before
    // the first read returns posts a second attempt for the same thread.
    const { rerender } = render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(reads.length).toBe(1));
    rerender(<DMChatArea selectedThreadId="conv-2" />);
    await waitFor(() => expect(reads.length).toBe(2));
    rerender(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(reads.length).toBe(3));
    expect(reads[2].url).toBe('/api/v1/dm/conversations/conv-1/read');
    await act(async () => {
      reads[2].resolve({ ok: true, status: 200, json: async () => ({}) }); // the newer attempt wins
    });
    await act(async () => {
      reads[0].resolve({ ok: false, status: 500, json: async () => ({}) }); // the older fails late
    });
    // Its baseline of 5 is stale: the server marker is current.
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    consoleSpy.mockRestore();
  });

  it('clears the local unread count when latest-seen fires with a nonzero count', () => {
    const mockClearUnread = vi.fn();
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 3 })],
      clearUnread: mockClearUnread,
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    mockClearUnread.mockClear();

    fireEvent.click(screen.getByTestId('trigger-latest-seen'));

    expect(mockClearUnread).toHaveBeenCalledWith('conv-1');
  });

  it('does not call clearUnread again when latest-seen fires with a zero count', () => {
    const mockClearUnread = vi.fn();
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 0 })],
      clearUnread: mockClearUnread,
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    mockClearUnread.mockClear();

    fireEvent.click(screen.getByTestId('trigger-latest-seen'));

    expect(mockClearUnread).not.toHaveBeenCalled();
  });

  // --- handleTyping ---

  it('passes sendTyping from useChatController to MessageInput as onTyping', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);

    // MessageInput should receive onTyping prop from useChatController.sendTyping
    expect(capturedMIProps.onTyping).toBeInstanceOf(Function);
  });

  // --- handleFetchComplete ---

  it('calls clearUnread after fetch completes', async () => {
    const mockClearUnread = vi.fn();
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [] }),
    });
    useDMStore.setState({
      conversations: [makeConversation()],
      clearUnread: mockClearUnread,
    });

    render(<DMChatArea selectedThreadId="conv-1" />);

    await waitFor(() => {
      expect(mockClearUnread).toHaveBeenCalledWith('conv-1');
    });
  });

  // --- 1:1 status fallback ---

  it('shows "offline" when participant has no status', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const statusDiv = document.querySelector('.dm-chat-header-status');
    expect(statusDiv?.textContent).toBe('offline');
  });

  // --- Unnamed group fallback to usernames ---

  it('falls back to usernames in unnamed group when displayName is missing', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          isGroup: true,
          name: null,
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.getByText('me, alice')).toBeInTheDocument();
  });

  // --- Edit with E2EE ---

  it('encrypts content when editing an encrypted conversation message', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({
        message: {
          content: 'encrypted-edited',
          edited_at: '2025-01-02T00:00:00Z',
        },
      }),
    });

    useDMStore.setState({
      conversations: [makeConversation()],
    });
    const mockUpdateMessage = vi.fn();
    useChatStore.setState({ updateMessage: mockUpdateMessage });

    render(<DMChatArea selectedThreadId="conv-1" />);

    fireEvent.click(screen.getByTestId('trigger-edit'));

    await waitFor(() => {
      expect(mockEncryptForChannelWithVersion).toHaveBeenCalledWith('conv-1', 'edited text');
      const patchCall = mockApiFetch.mock.calls.find(
        ([, options]) => (options as { method?: string } | undefined)?.method === 'PATCH'
      );
      expect(JSON.parse((patchCall?.[1] as { body: string }).body)).toEqual({
        content: 'encrypted-edited',
        key_version: 2,
      });
    });
  });

  // --- Reply support ---

  it('passes onReply and replyingTo to child components', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(capturedMLProps.onReply).toBeInstanceOf(Function);
    expect(capturedMIProps.replyingTo).toBeNull();
    expect(capturedMIProps.onCancelReply).toBeInstanceOf(Function);
  });

  it('passes replyToId when replying', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);

    // Trigger send with reply
    fireEvent.click(screen.getByTestId('trigger-send-with-reply'));

    expect(mockSendDMMessage).toHaveBeenCalledWith(
      'conv-1',
      'Reply msg',
      'testuser',
      expect.objectContaining({
        replyToId: 'parent-msg-1',
      })
    );
  });

  // --- Pin support ---

  it('passes onPinToggle and canPin to MessageList', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
    });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(capturedMLProps.onPinToggle).toBeInstanceOf(Function);
    expect(capturedMLProps.canPin).toBe(true);
  });

  // --- pinnedCount badge ---

  it('does not show pin badge when pinnedCount is 0', () => {
    mockGetPins.mockResolvedValue([]);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    expect(container.querySelector('.pin-count-badge')).toBeNull();
  });

  it('shows pin badge when getPins returns items', async () => {
    mockGetPins.mockResolvedValue([{ id: 'p1' }, { id: 'p2' }]);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => {
      expect(container.querySelector('.pin-count-badge')).toBeInTheDocument();
      expect(container.querySelector('.pin-count-badge')?.textContent).toBe('2');
    });
  });

  it.each([
    ['matching conversation', 'conv-1'],
    ['server-wide purge', null],
  ] as const)('clears stale pin counts for a %s purge', async (_label, scopeId) => {
    const oldLoad = deferred<Array<{ id: string }>>();
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(freshLoad.promise);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(1));

    act(() => {
      globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId } }));
    });
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    expect(container.querySelector('.pin-count-badge')).toBeNull();

    await act(async () => oldLoad.resolve([{ id: 'old-pin' }]));
    expect(container.querySelector('.pin-count-badge')).toBeNull();
    await act(async () => freshLoad.resolve([]));
  });

  it('ignores an unrelated explicit purge scope for the pin count', async () => {
    const oldLoad = deferred<Array<{ id: string }>>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(oldLoad.promise);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(1));
    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'other-conversation' } })
      );
    });
    expect(mockGetPins).toHaveBeenCalledTimes(1);
    await act(async () => oldLoad.resolve([{ id: 'old-pin' }]));
    await waitFor(() => expect(container.querySelector('.pin-count-badge')).toHaveTextContent('1'));
  });

  it('clears a loaded pin badge immediately on purge', async () => {
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetPins.mockReset();
    mockGetPins
      .mockReturnValueOnce(Promise.resolve([{ id: 'old-pin' }]))
      .mockReturnValueOnce(freshLoad.promise);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(container.querySelector('.pin-count-badge')).toHaveTextContent('1'));
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    expect(container.querySelector('.pin-count-badge')).toBeNull();
    await act(async () => freshLoad.resolve([]));
  });

  it('keeps a fresh pin badge when the stale count request rejects afterward', async () => {
    const oldLoad = deferred<Array<{ id: string }>>();
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(freshLoad.promise);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    await act(async () => freshLoad.resolve([{ id: 'fresh-pin' }]));
    await waitFor(() => expect(container.querySelector('.pin-count-badge')).toHaveTextContent('1'));
    await act(async () => oldLoad.reject(new Error('stale')));
    expect(container.querySelector('.pin-count-badge')).toHaveTextContent('1');
  });

  it('keeps the fresh pin count when a pre-purge pin succeeds late', async () => {
    const pendingPin = deferred<unknown>();
    const freshLoad = deferred<Array<{ id: string }>>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(Promise.resolve([])).mockReturnValueOnce(freshLoad.promise);
    mockPinMessage.mockReturnValue(pendingPin.promise);
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(1));

    const toggle = capturedMLProps.onPinToggle as (msg: unknown) => Promise<void>;
    const mutation = toggle({ id: 'msg-1', pinned_at: null });
    await waitFor(() => expect(mockPinMessage).toHaveBeenCalledWith('msg-1'));
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    await act(async () => freshLoad.resolve([{ id: 'fresh-pin' }]));
    await waitFor(() => expect(container.querySelector('.pin-count-badge')).toHaveTextContent('1'));

    await act(async () => pendingPin.resolve({}));
    await mutation;
    expect(container.querySelector('.pin-count-badge')).toHaveTextContent('1');
  });

  it('keeps the group purge dialog pinned to the opened conversation row', async () => {
    const group = makeConversation({
      id: 'group-1',
      isGroup: true,
      name: 'Team',
      participants: [
        { userId: 'user-1', username: 'me', role: 'admin' },
        { userId: 'user-2', username: 'alice', role: 'member' },
      ],
    });
    useDMStore.setState({ conversations: [group] });
    const user = userEvent.setup();
    render(<DMChatArea selectedThreadId="group-1" />);
    await user.click(await screen.findByRole('button', { name: /^Purge messages in this/ }));
    const dialog = await screen.findByRole('dialog', { name: 'Purge Messages' });
    await user.click(screen.getByLabelText('Last hour'));

    act(() =>
      useDMStore.setState({
        conversations: [{ ...group, name: 'Renamed team' }],
      })
    );

    expect(dialog).toHaveTextContent('Team');
    expect(dialog).not.toHaveTextContent('Renamed team');
  });

  it('closes group purge consent when the local role changes', async () => {
    const group = makeConversation({
      id: 'group-1',
      isGroup: true,
      name: 'Team',
      participants: [
        { userId: 'user-1', username: 'me', role: 'member' },
        { userId: 'user-2', username: 'alice', role: 'member' },
      ],
    });
    useDMStore.setState({ conversations: [group] });
    const user = userEvent.setup();
    render(<DMChatArea selectedThreadId="group-1" />);
    await user.click(await screen.findByRole('button', { name: /^Purge messages in this/ }));
    expect(await screen.findByRole('dialog', { name: 'Purge Messages' })).toBeInTheDocument();

    act(() =>
      useDMStore.setState({
        conversations: [
          {
            ...group,
            participants: group.participants.map((participant) =>
              participant.userId === 'user-1' ? { ...participant, role: 'admin' } : participant
            ),
          },
        ],
      })
    );

    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Purge Messages' })).not.toBeInTheDocument()
    );
  });

  it('pin toggle on an unpinned message increments pinnedCount badge', async () => {
    mockGetPins.mockResolvedValue([]);
    mockPinMessage.mockResolvedValue({});
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);

    // Trigger pin on a message that is NOT currently pinned (pinned_at is null)
    const unpinnedMsg = { id: 'msg-1', pinned_at: null };
    await (capturedMLProps.onPinToggle as (m: unknown) => Promise<void>)(unpinnedMsg);

    await waitFor(() => {
      expect(container.querySelector('.pin-count-badge')).toBeInTheDocument();
      expect(container.querySelector('.pin-count-badge')?.textContent).toBe('1');
    });
  });

  it('pin toggle on a pinned message decrements pinnedCount badge', async () => {
    mockGetPins.mockResolvedValue([{ id: 'p1' }]);
    mockUnpinMessage.mockResolvedValue({});
    useDMStore.setState({ conversations: [makeConversation()] });
    const { container } = render(<DMChatArea selectedThreadId="conv-1" />);

    // Wait for initial count to load
    await waitFor(() => {
      expect(container.querySelector('.pin-count-badge')?.textContent).toBe('1');
    });

    // Trigger unpin on a message that IS currently pinned (pinned_at is set)
    const pinnedMsg = { id: 'msg-1', pinned_at: '2025-01-01T00:00:00Z' };
    await (capturedMLProps.onPinToggle as (m: unknown) => Promise<void>)(pinnedMsg);

    await waitFor(() => {
      expect(container.querySelector('.pin-count-badge')).toBeNull();
    });
  });

  // --- Pinned panel open/close ---

  it('pinned panel is closed by default', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const pinBtn = screen.getByLabelText('Pinned messages');
    expect(pinBtn).toHaveAttribute('aria-expanded', 'false');
  });

  it('clicking pin button opens the pinned panel', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    fireEvent.click(screen.getByLabelText('Pinned messages'));
    expect(screen.getByLabelText('Pinned messages')).toHaveAttribute('aria-expanded', 'true');
  });

  it('clicking pin button again closes the pinned panel', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const pinBtn = screen.getByLabelText('Pinned messages');
    fireEvent.click(pinBtn);
    fireEvent.click(pinBtn);
    expect(pinBtn).toHaveAttribute('aria-expanded', 'false');
  });

  // --- Join voice call header affordance (#1219 R5) ---

  const makeGroupConversation = (overrides: Partial<DMConversation> = {}): DMConversation =>
    makeConversation({
      id: 'grp-1',
      isGroup: true,
      name: 'Squad',
      participants: [
        { userId: 'user-1', username: 'me', displayName: 'Me', status: 'online' },
        { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'online' },
        { userId: 'user-3', username: 'bob', displayName: 'Bob', status: 'online' },
      ],
      ...overrides,
    });

  it('shows Join voice call when a group call is active and user is not in it', () => {
    useDMStore.setState({ conversations: [makeGroupConversation()] });
    useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-3'], 3);
    render(<DMChatArea selectedThreadId="grp-1" />);
    expect(screen.getByRole('button', { name: /join voice call/i })).toBeInTheDocument();
  });

  it('clicking Join voice call joins the DM channel', () => {
    useDMStore.setState({ conversations: [makeGroupConversation()] });
    useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-3'], 3);
    render(<DMChatArea selectedThreadId="grp-1" />);
    fireEvent.click(screen.getByRole('button', { name: /join voice call/i }));
    expect(mockJoinChannel).toHaveBeenCalledWith('grp-1', 'dm');
  });

  it('hides Join voice call when no group call is active', () => {
    useDMStore.setState({ conversations: [makeGroupConversation()] });
    render(<DMChatArea selectedThreadId="grp-1" />);
    expect(screen.queryByRole('button', { name: /join voice call/i })).not.toBeInTheDocument();
  });

  it('hides Join voice call when the local user is already in that call', () => {
    useDMStore.setState({ conversations: [makeGroupConversation()] });
    useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-1', 'user-3'], 3);
    useVoiceStore.getState().setDMCall(true, 'grp-1');
    useVoiceStore.getState().setCallState({ kind: 'in-call' });
    render(<DMChatArea selectedThreadId="grp-1" />);
    expect(screen.queryByRole('button', { name: /join voice call/i })).not.toBeInTheDocument();
  });

  it('does not show Join voice call for a 1:1 conversation', () => {
    useDMStore.setState({ conversations: [makeConversation()] });
    useVoiceStore.getState().seedActiveDMCall('conv-1', ['user-2'], 2);
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.queryByRole('button', { name: /join voice call/i })).not.toBeInTheDocument();
  });
});

// ── Active DM call → VoiceView surface (#1873) ────────────────────────────────

describe('DMChatArea — active DM call (#1873)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockGetPins.mockResolvedValue([]);
    mockPinMessage.mockResolvedValue({});
    mockUnpinMessage.mockResolvedValue({});
    useUserStore.setState({ user: mockUser });
    useDMStore.setState({ conversations: [makeConversation()] });
  });

  it('renders the VoiceView surface when the local user is in this conversation call', () => {
    useVoiceStore.setState({ isDMCall: true, dmConversationId: 'conv-1' });
    render(<DMChatArea selectedThreadId="conv-1" />);
    const voiceView = screen.getByTestId('voice-view');
    expect(voiceView).toBeInTheDocument();
    // channelName is the resolved thread name (the other 1:1 participant).
    expect(voiceView).toHaveTextContent('Alice');
  });

  it('renders the normal text view (no VoiceView) when not in any call', () => {
    useVoiceStore.setState({ isDMCall: false, dmConversationId: null });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.queryByTestId('voice-view')).not.toBeInTheDocument();
  });

  it('does not render VoiceView while in a DIFFERENT conversation call', () => {
    useVoiceStore.setState({ isDMCall: true, dmConversationId: 'other-conv' });
    render(<DMChatArea selectedThreadId="conv-1" />);
    expect(screen.queryByTestId('voice-view')).not.toBeInTheDocument();
  });
});
