import { describe, it, expect, beforeEach, vi } from 'vitest';
import { sendDMMessage } from '@/renderer/services/dmMessageSender';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { mockUser } from '../../mocks/fixtures';
import { ConnectionState } from '@/renderer/services/websocketService';

const mockSendDMMessage = vi.fn();
const mockGetState = vi.fn(() => ConnectionState.CONNECTED);

vi.mock('@/renderer/services/websocketService', () => ({
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

vi.mock('@/renderer/services/messageQueue', () => ({
  getMessageQueue: () => ({
    enqueue: mockEnqueue,
    markAsSent: mockMarkAsSent,
    remove: mockRemove,
    markAsFailed: mockMarkAsFailed,
    markAsTerminallyFailed: mockMarkAsTerminallyFailed,
  }),
}));

const mockEncryptForChannel = vi.fn();
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    encryptForChannel: (...args: unknown[]) => mockEncryptForChannel(...args),
    getCurrentKeyVersion: () => undefined,
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
      undefined
    );
    const msgs = useChatStore.getState().messagesByChannel.get('dm-conv-1');
    expect(msgs?.find((m) => m.content.includes('GHJKMNPQ'))?.status).toBe('pending');
  });

  it('encrypts and sends via the websocket transport when connected', async () => {
    const encrypted = 'encrypted-base64-content-that-is-long-enough-for-validation';
    mockEncryptForChannel.mockResolvedValue(encrypted);
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
});
