import { describe, it, expect, beforeEach, vi } from 'vitest';
import { sendDMMessage } from '@/renderer/services/messaging/dmMessageSender';
import { describeMessagePreview } from '@/renderer/utils/messaging/messagePreview';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { mockUser } from '../../mocks/fixtures';
import { ConnectionState } from '@/renderer/services/messaging/websocketService';

const mockSendDMMessage = vi.fn();
const mockGetState = vi.fn(() => ConnectionState.CONNECTED);

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({
    sendDMMessage: mockSendDMMessage,
    getState: mockGetState,
  }),
  ConnectionState: {
    CONNECTED: 'connected',
    DISCONNECTED: 'disconnected',
    CONNECTING: 'connecting',
  },
}));

const mockEnqueue = vi.fn(() => 'client-msg-1');
const mockMarkAsSent = vi.fn();
const mockRemove = vi.fn();
const mockMarkAsFailed = vi.fn();
const mockMarkAsTerminallyFailed = vi.fn();

vi.mock('@/renderer/services/messaging/messageQueue', () => ({
  getMessageQueue: () => ({
    enqueue: mockEnqueue,
    markAsSent: mockMarkAsSent,
    remove: mockRemove,
    markAsFailed: mockMarkAsFailed,
    markAsTerminallyFailed: mockMarkAsTerminallyFailed,
  }),
}));

const mockEncryptForChannelWithVersion = vi.fn();
const mockGetCurrentKeyVersion = vi.fn((..._args: unknown[]): number | undefined => undefined);
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    encryptForChannelWithVersion: (...args: unknown[]) => mockEncryptForChannelWithVersion(...args),
    getCurrentKeyVersion: (...args: unknown[]) => mockGetCurrentKeyVersion(...args),
    invalidateChannelKey: vi.fn(),
    isInitialized: true,
  },
}));

describe('dmMessageSender.sendDMMessage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useChatStore.setState({ messagesByChannel: new Map(), isConnected: true });
    useUserStore.setState({ user: mockUser });
    useDMStore.setState({ conversations: [] } as Partial<ReturnType<typeof useDMStore.getState>>);
    mockGetState.mockReturnValue(ConnectionState.CONNECTED);
  });

  it('enqueues, adds an optimistic message, and returns the client id', () => {
    const id = sendDMMessage('dm-conv-1', 'https://invite.concordvoice.chat/GHJKMNPQ');
    expect(id).toBe('client-msg-1');
    expect(mockEnqueue).toHaveBeenCalledWith(
      'dm-conv-1',
      'https://invite.concordvoice.chat/GHJKMNPQ',
      'dm_message',
      undefined,
      undefined,
      undefined
    );
    const msgs = useChatStore.getState().messagesByChannel.get('dm-conv-1');
    expect(msgs?.find((m) => m.content.includes('GHJKMNPQ'))?.status).toBe('pending');
  });

  it('encrypts and sends via the websocket transport when connected', async () => {
    const encrypted = 'encrypted-base64-content-that-is-long-enough-for-validation';
    mockEncryptForChannelWithVersion.mockResolvedValue({
      ciphertext: encrypted,
      keyVersion: 1,
    });
    sendDMMessage('dm-conv-1', 'https://invite.concordvoice.chat/GHJKMNPQ');
    await vi.waitFor(() => {
      expect(mockSendDMMessage).toHaveBeenCalledWith(
        'dm-conv-1',
        encrypted,
        expect.objectContaining({ nonce: 'client-msg-1' })
      );
    });
  });

  it('does not send over the socket when disconnected (queues instead)', () => {
    mockGetState.mockReturnValue(ConnectionState.DISCONNECTED);
    sendDMMessage('dm-conv-1', 'https://invite.concordvoice.chat/GHJKMNPQ');
    expect(mockSendDMMessage).not.toHaveBeenCalled();
    expect(mockEnqueue).toHaveBeenCalled();
  });

  it('stores self-sent plaintext as an optimistic in-memory preview', () => {
    mockGetState.mockReturnValue(ConnectionState.DISCONNECTED);
    useDMStore.setState({
      conversations: [
        {
          id: 'dm-conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [],
          lastMessage: {
            content: 'old-ciphertext',
            userId: 'friend-id',
            username: 'friend',
            createdAt: '2026-01-01T00:00:00Z',
          },
          unreadCount: 0,
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    } as Partial<ReturnType<typeof useDMStore.getState>>);

    sendDMMessage('dm-conv-1', 'Latest self-sent DM', 'me');

    const conv = useDMStore.getState().conversations.find((c) => c.id === 'dm-conv-1');
    expect(conv?.lastMessage).toEqual(
      expect.objectContaining({
        content: 'Latest self-sent DM',
        plaintextPreview: 'Latest self-sent DM',
      })
    );
  });

  it('carries the attachment MIME into the optimistic preview so a PDF reads as a Doc', () => {
    mockGetState.mockReturnValue(ConnectionState.DISCONNECTED);
    useDMStore.setState({
      conversations: [
        {
          id: 'dm-conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    } as Partial<ReturnType<typeof useDMStore.getState>>);

    sendDMMessage('dm-conv-1', '', 'me', {
      attachments: [
        { id: 'file-1', file_type: 'file', mime_type: 'application/pdf', file_size: 1024 },
      ],
    });

    const last = useDMStore.getState().conversations.find((c) => c.id === 'dm-conv-1')?.lastMessage;

    // The consumer, not the handshake. `file_type: 'file'` alone classifies as
    // File; only the MIME promotes it to Doc. Asserting the field in isolation
    // would still pass on a writer that carried a MIME nothing could use.
    expect(
      describeMessagePreview({
        content: '',
        attachmentType: last?.attachmentType,
        attachmentMime: last?.attachmentMime,
      }).phrase
    ).toBe('a Doc');
  });

  // regression: dm-send-atomic-epoch
  it('sends the key_version of the key that encrypted the DM, even when the channel-key cache reads 0', async () => {
    const ciphertext = 'encrypted-base64-content-that-is-long-enough-for-validation';
    // The atomic API reports the epoch of the key it actually encrypted with.
    mockEncryptForChannelWithVersion.mockResolvedValue({ ciphertext, keyVersion: 4 });
    // The cache now reads the keyVersion:0 malformed-wrap marker slot.
    mockGetCurrentKeyVersion.mockReturnValue(0);
    mockGetCurrentKeyVersion.mockClear();

    sendDMMessage('dm-conv-1', 'https://invite.concordvoice.chat/GHJKMNPQ');

    await vi.waitFor(() => {
      expect(mockSendDMMessage, 'the DM must actually reach the transport').toHaveBeenCalledTimes(
        1
      );
    });

    const [, sentContent, sentOpts] = mockSendDMMessage.mock.calls[0];
    expect(sentContent).toBe(ciphertext);
    expect
      .soft(
        sentOpts.keyVersion,
        'the key_version sent with a DM ciphertext must equal the epoch of the key that encrypted it (4), not the separate channel-key cache read (0)'
      )
      .toBe(4);
    expect(
      mockGetCurrentKeyVersion,
      'the DM send path must not read the epoch via a separate getCurrentKeyVersion call after encrypting'
    ).not.toHaveBeenCalled();
  });
});
