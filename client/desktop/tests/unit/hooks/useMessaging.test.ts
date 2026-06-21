import { renderHook, act } from '@testing-library/react';
import { useMessaging } from '@/renderer/hooks/useMessaging';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { mockUser, mockChannel, mockEncryptedChannel } from '../../mocks/fixtures';
import { ConnectionState } from '@/renderer/services/websocketService';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';

// Mock websocketService
const mockSendMessage = vi.fn();
const mockSendDMMessage = vi.fn();
const mockSendTypingIndicator = vi.fn();
const mockOnConnectionChange = vi.fn(() => vi.fn()); // returns unsubscribe
const mockGetState = vi.fn(() => ConnectionState.CONNECTED);
const mockIsSubscribed = vi.fn(() => true);
const mockIsDMSubscribed = vi.fn(() => true);
const mockWhenConnectionReady = vi.fn(() => Promise.resolve());

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    sendMessage: mockSendMessage,
    sendDMMessage: mockSendDMMessage,
    sendTypingIndicator: mockSendTypingIndicator,
    onConnectionChange: mockOnConnectionChange,
    getState: mockGetState,
    isSubscribed: mockIsSubscribed,
    isDMSubscribed: mockIsDMSubscribed,
    whenConnectionReady: mockWhenConnectionReady,
  }),
  ConnectionState: {
    CONNECTED: 'connected',
    DISCONNECTED: 'disconnected',
    CONNECTING: 'connecting',
  },
}));

// Mock messageQueue
const mockEnqueue = vi.fn(() => 'client-msg-1');
const mockMarkAsSent = vi.fn();
const mockMarkAsDelivered = vi.fn();
const mockMarkAsFailed = vi.fn();
const mockMarkAsTerminallyFailed = vi.fn();
const mockRemove = vi.fn();
const mockStartProcessing = vi.fn();
const mockStopProcessing = vi.fn();
const mockSize = vi.fn(() => 0);
const mockGetMessagesForChannel = vi.fn(() => []);

vi.mock('@/renderer/services/messageQueue', () => ({
  getMessageQueue: () => ({
    enqueue: mockEnqueue,
    markAsSent: mockMarkAsSent,
    markAsDelivered: mockMarkAsDelivered,
    markAsFailed: mockMarkAsFailed,
    markAsTerminallyFailed: mockMarkAsTerminallyFailed,
    remove: mockRemove,
    startProcessing: mockStartProcessing,
    stopProcessing: mockStopProcessing,
    size: mockSize,
    getMessagesForChannel: mockGetMessagesForChannel,
  }),
}));

// Mock e2eeService
const mockEncryptForChannel = vi.fn();
const mockGetCurrentKeyVersion = vi.fn(() => undefined);
const mockInvalidateChannelKey = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    encryptForChannel: (...args: unknown[]) => mockEncryptForChannel(...args),
    getCurrentKeyVersion: (...args: unknown[]) => mockGetCurrentKeyVersion(...args),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    isInitialized: true,
  },
}));

describe('useMessaging', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useChatStore.setState({
      messagesByChannel: new Map(),
      isConnected: true,
    });
    useUserStore.setState({ user: mockUser });
    useChannelStore.setState({ channels: [mockChannel, mockEncryptedChannel] });
    mockGetState.mockReturnValue(ConnectionState.CONNECTED);
    mockIsSubscribed.mockReturnValue(true);
    mockIsDMSubscribed.mockReturnValue(true);
  });

  it('returns sendMessage, sendDMMessage, markDelivered, sendTyping, getPendingCount, getPendingMessagesForChannel', () => {
    const { result } = renderHook(() => useMessaging());
    expect(result.current.sendMessage).toBeInstanceOf(Function);
    expect(result.current.sendDMMessage).toBeInstanceOf(Function);
    expect(result.current.markDelivered).toBeInstanceOf(Function);
    expect(result.current.sendTyping).toBeInstanceOf(Function);
    expect(result.current.getPendingCount).toBeInstanceOf(Function);
    expect(result.current.getPendingMessagesForChannel).toBeInstanceOf(Function);
  });

  it('registers connection change handler on mount', () => {
    renderHook(() => useMessaging());
    expect(mockOnConnectionChange).toHaveBeenCalledWith(expect.any(Function));
  });

  it('unsubscribes and stops processing on unmount', () => {
    const unsubscribe = vi.fn();
    mockOnConnectionChange.mockReturnValue(unsubscribe);
    const { unmount } = renderHook(() => useMessaging());
    unmount();
    expect(unsubscribe).toHaveBeenCalled();
    expect(mockStopProcessing).toHaveBeenCalled();
  });

  it('sendMessage enqueues and adds optimistic message for unencrypted channel', () => {
    const { result } = renderHook(() => useMessaging());
    vi.spyOn(useChatStore.getState(), 'addMessage');

    let clientId: string;
    act(() => {
      clientId = result.current.sendMessage('channel-1', 'Hello world', 'testuser');
    });

    expect(mockEnqueue).toHaveBeenCalledWith(
      'channel-1',
      'Hello world',
      'message',
      undefined,
      undefined
    );
    expect(clientId!).toBe('client-msg-1');
  });

  it('sendMessage sends via websocket when connected', async () => {
    const encryptedContent = 'encrypted-base64-content-that-is-long-enough-for-validation';
    mockEncryptForChannel.mockResolvedValue(encryptedContent);
    mockGetCurrentKeyVersion.mockReturnValue(undefined);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-1', 'Hello world', 'testuser');
    });

    // Wait for async doSend
    await vi.waitFor(() => {
      expect(mockSendMessage).toHaveBeenCalledWith('channel-1', encryptedContent, {
        nonce: 'client-msg-1',
        keyVersion: undefined,
        mentionMeta: undefined,
        replyToId: undefined,
        attachmentIds: undefined,
      });
    });
  });

  it('sendMessage encrypts for E2EE channels', async () => {
    mockEncryptForChannel.mockResolvedValue(
      'encrypted-base64-content-that-is-long-enough-for-validation'
    );
    mockGetCurrentKeyVersion.mockReturnValue(3);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-2', 'Secret message', 'testuser');
    });

    await vi.waitFor(() => {
      expect(mockEncryptForChannel).toHaveBeenCalledWith('channel-2', 'Secret message');
      expect(mockSendMessage).toHaveBeenCalledWith(
        'channel-2',
        'encrypted-base64-content-that-is-long-enough-for-validation',
        {
          nonce: 'client-msg-1',
          keyVersion: 3,
          mentionMeta: undefined,
          replyToId: undefined,
          attachmentIds: undefined,
        }
      );
    });
  });

  it('sendMessage marks as failed when encryption fails', async () => {
    mockEncryptForChannel.mockRejectedValue(new Error('Key not available'));

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-2', 'Secret message', 'testuser');
    });

    await vi.waitFor(() => {
      // Non-typed errors are classified as retryable; uxMessage is the
      // generic "Unable to send message" copy from classifyError.
      expect(mockMarkAsFailed).toHaveBeenCalledWith('client-msg-1', 'Unable to send message');
    });
  });

  it('sendMessage terminally fails on NOT_MEMBER with code-specific copy', async () => {
    mockEncryptForChannel.mockRejectedValue(new E2EEKeyUnavailableError('NOT_MEMBER'));

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-2', 'Forbidden', 'testuser');
    });

    await vi.waitFor(() => {
      expect(mockMarkAsTerminallyFailed).toHaveBeenCalledWith(
        'client-msg-1',
        expect.stringContaining("don't have access")
      );
    });
    expect(mockMarkAsFailed).not.toHaveBeenCalled();
  });

  it('sendMessage invalidates cache and terminally fails on REVOKED_EPOCH', async () => {
    mockEncryptForChannel.mockRejectedValue(new E2EEKeyUnavailableError('REVOKED_EPOCH'));

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-2', 'Stale', 'testuser');
    });

    await vi.waitFor(() => {
      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('channel-2');
      expect(mockMarkAsTerminallyFailed).toHaveBeenCalledWith(
        'client-msg-1',
        expect.stringContaining('re-establishing')
      );
    });
  });

  it('sendMessage does not send via websocket when disconnected', () => {
    mockGetState.mockReturnValue(ConnectionState.DISCONNECTED);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-1', 'Hello offline', 'testuser');
    });

    expect(mockSendMessage).not.toHaveBeenCalled();
    // But should still enqueue
    expect(mockEnqueue).toHaveBeenCalled();
  });

  it('markDelivered updates queue and store', () => {
    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.markDelivered('client-msg-1', 'server-msg-1', 'channel-1');
    });

    expect(mockMarkAsDelivered).toHaveBeenCalledWith('client-msg-1', 'server-msg-1');
  });

  it('sendTyping sends indicator when connected', () => {
    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendTyping('channel-1', true);
    });

    expect(mockSendTypingIndicator).toHaveBeenCalledWith('channel-1', true);
  });

  it('sendTyping does nothing when disconnected', () => {
    mockGetState.mockReturnValue(ConnectionState.DISCONNECTED);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendTyping('channel-1', true);
    });

    expect(mockSendTypingIndicator).not.toHaveBeenCalled();
  });

  it('sendMessage passes mentionMeta and replyToId to enqueue', () => {
    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-1', 'Reply msg', 'testuser', {
        mentionMeta: 'mention-meta',
        replyToId: 'reply-target-id',
      });
    });

    expect(mockEnqueue).toHaveBeenCalledWith(
      'channel-1',
      'Reply msg',
      'message',
      'mention-meta',
      'reply-target-id'
    );
  });

  it('sendMessage populates replied_to from store for optimistic message', () => {
    // Seed the target message in the store
    useChatStore.getState().addMessage('channel-1', {
      id: 'target-msg',
      channel_id: 'channel-1',
      user_id: 'user-2',
      username: 'otheruser',
      display_name: 'Other User',
      content: 'Original message content',
      created_at: '2025-01-01T12:00:00Z',
      updated_at: '2025-01-01T12:00:00Z',
      status: 'delivered',
    });

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendMessage('channel-1', 'My reply', 'testuser', {
        replyToId: 'target-msg',
      });
    });

    // Find the optimistic reply in the store (it has the client-generated ID)
    const messages = useChatStore.getState().messagesByChannel.get('channel-1');
    const optimistic = messages?.find((m) => m.content === 'My reply');
    expect(optimistic).toBeDefined();
    expect(optimistic?.reply_to_id).toBe('target-msg');
    expect(optimistic?.replied_to).toBeDefined();
    expect(optimistic?.replied_to?.id).toBe('target-msg');
    expect(optimistic?.replied_to?.content).toBe('Original message content');
  });

  it('getPendingCount returns queue size', () => {
    mockSize.mockReturnValue(3);
    const { result } = renderHook(() => useMessaging());
    expect(result.current.getPendingCount()).toBe(3);
  });

  it('getPendingMessagesForChannel delegates to queue', () => {
    const msgs = [{ id: '1', channelId: 'channel-1', content: 'test' }];
    mockGetMessagesForChannel.mockReturnValue(msgs);

    const { result } = renderHook(() => useMessaging());
    expect(result.current.getPendingMessagesForChannel('channel-1')).toEqual(msgs);
    expect(mockGetMessagesForChannel).toHaveBeenCalledWith('channel-1');
  });

  // --- sendDMMessage tests ---

  it('sendDMMessage enqueues with dm_message type', () => {
    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'Hello DM', 'testuser');
    });

    expect(mockEnqueue).toHaveBeenCalledWith(
      'dm-conv-1',
      'Hello DM',
      'dm_message',
      undefined,
      undefined
    );
  });

  it('sendDMMessage sends via websocket sendDMMessage when connected', async () => {
    const encryptedContent = 'encrypted-base64-content-that-is-long-enough-for-validation';
    mockEncryptForChannel.mockResolvedValue(encryptedContent);
    mockGetCurrentKeyVersion.mockReturnValue(undefined);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'Hello DM', 'testuser');
    });

    await vi.waitFor(() => {
      expect(mockSendDMMessage).toHaveBeenCalledWith('dm-conv-1', encryptedContent, {
        nonce: 'client-msg-1',
        keyVersion: undefined,
        mentionMeta: undefined,
        attachmentIds: undefined,
        replyToId: undefined,
      });
    });
  });

  it('sendDMMessage encrypts for E2EE conversations and includes keyVersion', async () => {
    mockEncryptForChannel.mockResolvedValue(
      'encrypted-base64-content-that-is-long-enough-for-validation'
    );
    mockGetCurrentKeyVersion.mockReturnValue(5);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'Secret DM', 'testuser');
    });

    await vi.waitFor(() => {
      expect(mockEncryptForChannel).toHaveBeenCalledWith('dm-conv-1', 'Secret DM');
      expect(mockSendDMMessage).toHaveBeenCalledWith(
        'dm-conv-1',
        'encrypted-base64-content-that-is-long-enough-for-validation',
        {
          nonce: 'client-msg-1',
          keyVersion: 5,
          mentionMeta: undefined,
          attachmentIds: undefined,
          replyToId: undefined,
        }
      );
    });
  });

  it('sendDMMessage marks as failed when encryption produces invalid output', async () => {
    mockEncryptForChannel.mockResolvedValue('short');
    mockGetCurrentKeyVersion.mockReturnValue(2);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'Secret DM', 'testuser');
    });

    await vi.waitFor(() => {
      // Encryption-output validation throws a plain Error ("Encryption produced
      // invalid output"), which classifyError treats as retryable with the
      // generic uxMessage.
      expect(mockMarkAsFailed).toHaveBeenCalledWith('client-msg-1', 'Unable to send message');
    });
  });

  it('sendDMMessage rolls back bumpConversation when send fails on new conversation', async () => {
    mockEncryptForChannel.mockResolvedValue(
      'encrypted-base64-content-that-is-long-enough-for-validation'
    );
    useDMStore.setState({
      conversations: [
        {
          id: 'dm-conv-new',
          participants: [],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2026-01-01T00:00:00Z',
          updatedAt: '2026-01-01T00:00:00Z',
        },
      ] as ReturnType<typeof useDMStore.getState>['conversations'],
    });
    mockSendDMMessage.mockImplementationOnce(() => {
      throw new Error('send failed');
    });

    const { result } = renderHook(() => useMessaging());
    act(() => {
      result.current.sendDMMessage('dm-conv-new', 'First message', 'testuser');
    });

    await vi.waitFor(() => {
      expect(mockMarkAsFailed).toHaveBeenCalled();
    });

    const conv = useDMStore.getState().conversations.find((c) => c.id === 'dm-conv-new');
    expect(conv?.lastMessage).toBeNull();
  });

  it('sendDMMessage does not send via websocket when disconnected', () => {
    mockGetState.mockReturnValue(ConnectionState.DISCONNECTED);

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'Hello offline DM', 'testuser');
    });

    expect(mockSendDMMessage).not.toHaveBeenCalled();
    // But should still enqueue
    expect(mockEnqueue).toHaveBeenCalled();
  });

  it('sendDMMessage adds optimistic message to store', () => {
    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'Optimistic DM', 'testuser');
    });

    const messages = useChatStore.getState().messagesByChannel.get('dm-conv-1');
    const optimistic = messages?.find((m) => m.content === 'Optimistic DM');
    expect(optimistic).toBeDefined();
    expect(optimistic?.channel_id).toBe('dm-conv-1');
    expect(optimistic?.status).toBe('pending');
  });

  it('sendDMMessage populates replied_to from store for optimistic display', () => {
    // Seed the target message in the store
    useChatStore.getState().addMessage('dm-conv-1', {
      id: 'dm-target-msg',
      channel_id: 'dm-conv-1',
      user_id: 'user-2',
      username: 'otheruser',
      display_name: 'Other User',
      content: 'Original DM content',
      created_at: '2025-01-01T12:00:00Z',
      updated_at: '2025-01-01T12:00:00Z',
      status: 'delivered',
    });

    const { result } = renderHook(() => useMessaging());

    act(() => {
      result.current.sendDMMessage('dm-conv-1', 'My DM reply', 'testuser', {
        replyToId: 'dm-target-msg',
      });
    });

    const messages = useChatStore.getState().messagesByChannel.get('dm-conv-1');
    const optimistic = messages?.find((m) => m.content === 'My DM reply');
    expect(optimistic).toBeDefined();
    expect(optimistic?.reply_to_id).toBe('dm-target-msg');
    expect(optimistic?.replied_to).toBeDefined();
    expect(optimistic?.replied_to?.id).toBe('dm-target-msg');
    expect(optimistic?.replied_to?.content).toBe('Original DM content');
  });

  // --- processQueuedMessage tests (via startProcessing callback) ---

  it('processQueuedMessage routes dm_message to sendDMMessage', async () => {
    const encryptedDM = 'encrypted-dm-base64-content-that-is-long-enough-for-validation';
    mockEncryptForChannel.mockResolvedValue(encryptedDM);
    mockGetCurrentKeyVersion.mockReturnValue(undefined);

    renderHook(() => useMessaging());

    // Capture the connection change handler
    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    // Trigger CONNECTED to start processing — captures processQueuedMessage callback
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    // Invoke processQueuedMessage with a DM queued message
    await processCallback({
      id: 'queued-dm-1',
      channelId: 'dm-conv-1',
      content: 'DM via queue',
      type: 'dm_message',
      status: 'pending',
      createdAt: Date.now(),
      retries: 0,
    });

    expect(mockSendDMMessage).toHaveBeenCalledWith('dm-conv-1', encryptedDM, {
      nonce: 'queued-dm-1',
      keyVersion: undefined,
      mentionMeta: undefined,
      replyToId: undefined,
    });
    expect(mockSendMessage).not.toHaveBeenCalled();
  });

  it('processQueuedMessage routes regular message to sendMessage', async () => {
    const encryptedMsg = 'encrypted-msg-base64-content-that-is-long-enough-for-validation';
    mockEncryptForChannel.mockResolvedValue(encryptedMsg);
    mockGetCurrentKeyVersion.mockReturnValue(undefined);

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    await processCallback({
      id: 'queued-msg-1',
      channelId: 'channel-1',
      content: 'Regular via queue',
      type: 'message',
      status: 'pending',
      createdAt: Date.now(),
      retries: 0,
    });

    expect(mockSendMessage).toHaveBeenCalledWith('channel-1', encryptedMsg, {
      nonce: 'queued-msg-1',
      keyVersion: undefined,
      mentionMeta: undefined,
      replyToId: undefined,
    });
    expect(mockSendDMMessage).not.toHaveBeenCalled();
  });

  it('processQueuedMessage throws when not subscribed (DM) so queue retries', async () => {
    mockIsDMSubscribed.mockReturnValue(false);

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    await expect(
      processCallback({
        id: 'queued-dm-2',
        channelId: 'dm-conv-1',
        content: 'Should be retried',
        type: 'dm_message',
        status: 'pending',
        createdAt: Date.now(),
        retries: 0,
      })
    ).rejects.toThrow('Subscription not available');

    expect(mockSendDMMessage).not.toHaveBeenCalled();
    expect(mockSendMessage).not.toHaveBeenCalled();
  });

  it('processQueuedMessage encrypts DM messages via encryptQueuedContent', async () => {
    mockEncryptForChannel.mockResolvedValue(
      'encrypted-base64-content-that-is-long-enough-for-validation'
    );
    mockGetCurrentKeyVersion.mockReturnValue(7);

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    await act(async () => {
      await processCallback({
        id: 'queued-dm-enc',
        channelId: 'dm-conv-1',
        content: 'Encrypted DM via queue',
        type: 'dm_message',
        status: 'pending',
        createdAt: Date.now(),
        retries: 0,
      });
    });

    expect(mockEncryptForChannel).toHaveBeenCalledWith('dm-conv-1', 'Encrypted DM via queue');
    expect(mockSendDMMessage).toHaveBeenCalledWith(
      'dm-conv-1',
      'encrypted-base64-content-that-is-long-enough-for-validation',
      {
        nonce: 'queued-dm-enc',
        keyVersion: 7,
        mentionMeta: undefined,
        replyToId: undefined,
      }
    );
  });
});

describe('useMessaging connection-ready gate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useChatStore.setState({
      messagesByChannel: new Map(),
      isConnected: true,
    });
    useUserStore.setState({ user: mockUser });
    useChannelStore.setState({ channels: [mockChannel, mockEncryptedChannel] });
    mockGetState.mockReturnValue(ConnectionState.CONNECTED);
    mockIsSubscribed.mockReturnValue(true);
    mockIsDMSubscribed.mockReturnValue(true);
    // Default: gate resolves immediately (no-op for tests that don't override)
    mockWhenConnectionReady.mockReturnValue(Promise.resolve());
  });

  it('calls whenConnectionReady before sending a queued message', async () => {
    mockEncryptForChannel.mockResolvedValue(
      'encrypted-base64-content-that-is-long-enough-for-validation'
    );
    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    await act(async () => {
      await processCallback({
        id: 'queued-gate-1',
        channelId: 'channel-1',
        content: 'Gate test message',
        type: 'message',
        status: 'pending',
        createdAt: Date.now(),
        retries: 0,
      });
    });

    // whenConnectionReady must have been called before sendMessage
    expect(mockWhenConnectionReady).toHaveBeenCalled();
    expect(mockSendMessage).toHaveBeenCalled();
    // Ordering: whenConnectionReady invocation order < sendMessage invocation order
    const readyOrder = mockWhenConnectionReady.mock.invocationCallOrder[0];
    const sendOrder = mockSendMessage.mock.invocationCallOrder[0];
    expect(readyOrder).toBeLessThan(sendOrder);
  });

  it('awaits the gate promise before proceeding (deferred resolution)', async () => {
    const encryptedDeferred = 'encrypted-deferred-base64-content-long-enough-for-validation-x';
    mockEncryptForChannel.mockResolvedValue(encryptedDeferred);
    mockGetCurrentKeyVersion.mockReturnValue(undefined);

    let resolveReady!: () => void;
    const readyPromise = new Promise<void>((r) => {
      resolveReady = r;
    });
    mockWhenConnectionReady.mockReturnValueOnce(readyPromise);

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    // Start processing but don't await — the gate is still unresolved
    const processPromise = processCallback({
      id: 'queued-gate-2',
      channelId: 'channel-1',
      content: 'Deferred gate message',
      type: 'message',
      status: 'pending',
      createdAt: Date.now(),
      retries: 0,
    });

    // Flush microtasks — gate still pending
    await Promise.resolve();
    expect(mockSendMessage).not.toHaveBeenCalled();

    // Resolve the gate and flush
    resolveReady();
    await processPromise;

    expect(mockSendMessage).toHaveBeenCalledWith('channel-1', encryptedDeferred, {
      nonce: 'queued-gate-2',
      keyVersion: undefined,
      mentionMeta: undefined,
      replyToId: undefined,
    });
  });

  it('proceeds best-effort when whenConnectionReady rejects (timeout fallback)', async () => {
    const encryptedFallback = 'encrypted-fallback-base64-content-long-enough-for-validation-x';
    mockEncryptForChannel.mockResolvedValue(encryptedFallback);
    mockGetCurrentKeyVersion.mockReturnValue(undefined);

    mockWhenConnectionReady.mockReturnValueOnce(
      Promise.reject(new Error('connection_ready timeout after 5s'))
    );

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];

    // Should not throw — fail-open
    await act(async () => {
      await processCallback({
        id: 'queued-gate-3',
        channelId: 'channel-1',
        content: 'Timeout fallback message',
        type: 'message',
        status: 'pending',
        createdAt: Date.now(),
        retries: 0,
      });
    });

    // Even after rejection, message should be sent (fail-open)
    expect(mockSendMessage).toHaveBeenCalledWith('channel-1', encryptedFallback, {
      nonce: 'queued-gate-3',
      keyVersion: undefined,
      mentionMeta: undefined,
      replyToId: undefined,
    });
  });

  it('warns only once per connection when gate rejects multiple times', async () => {
    // Gate always rejects (simulates a v1 hub that never acks connection_ready_probe)
    mockWhenConnectionReady.mockReturnValue(
      Promise.reject(new Error('connection_ready timeout after 5s'))
    );

    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];
    const makeMsg = (id: string) => ({
      id,
      channelId: 'channel-1',
      content: `msg-${id}`,
      type: 'message' as const,
      status: 'pending' as const,
      createdAt: Date.now(),
      retries: 0,
    });

    // Process three messages through the rejecting gate
    await act(async () => {
      await processCallback(makeMsg('g1'));
      await processCallback(makeMsg('g2'));
      await processCallback(makeMsg('g3'));
    });

    // Only one warn should fire despite three rejections
    const gateWarns = warnSpy.mock.calls.filter((args) =>
      String(args[0]).includes('connection_ready gate')
    );
    expect(gateWarns).toHaveLength(1);

    warnSpy.mockRestore();
  });

  it('resets warn flag on CONNECTED transition so next rejection logs again', async () => {
    // Gate always rejects
    mockWhenConnectionReady.mockReturnValue(
      Promise.reject(new Error('connection_ready timeout after 5s'))
    );

    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    renderHook(() => useMessaging());

    const connectionHandler = mockOnConnectionChange.mock.calls[0][0];
    // First CONNECTED — starts processing
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    const processCallback = mockStartProcessing.mock.calls[0][0];
    const makeMsg = (id: string) => ({
      id,
      channelId: 'channel-1',
      content: `msg-${id}`,
      type: 'message' as const,
      status: 'pending' as const,
      createdAt: Date.now(),
      retries: 0,
    });

    // Process one message — fires warn once
    await act(async () => {
      await processCallback(makeMsg('r1'));
    });
    const warnsAfterFirst = warnSpy.mock.calls.filter((args) =>
      String(args[0]).includes('connection_ready gate')
    ).length;
    expect(warnsAfterFirst).toBe(1);

    // Simulate reconnect: CONNECTED transition resets the flag
    act(() => {
      connectionHandler(ConnectionState.CONNECTED);
    });

    // Process another message — warn should fire again (flag was reset)
    await act(async () => {
      await processCallback(makeMsg('r2'));
    });
    const warnsAfterReset = warnSpy.mock.calls.filter((args) =>
      String(args[0]).includes('connection_ready gate')
    ).length;
    expect(warnsAfterReset).toBe(2);

    warnSpy.mockRestore();
  });
});
