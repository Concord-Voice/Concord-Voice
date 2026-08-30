import { renderHook, act } from '@testing-library/react';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { PIN_MESSAGES } from '@/renderer/utils/permissions';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import { ConnectionState } from '@/renderer/services/messaging/websocketService';
import {
  clearIndex,
  indexMessage,
  removeMessage,
  searchMessages,
} from '@/renderer/services/messaging/searchService';
import type { ChatContext, MessageWithStatus } from '@/renderer/types/chat';
import { deferred } from '../../helpers/deferred';

// --- Mocks ---

// WebSocket service
const mockSendMessage = vi.fn(() => 'client-msg-1');
const mockSendDMMessage = vi.fn(() => 'client-msg-1');
const mockSendTypingIndicator = vi.fn();
const mockSendDMTypingIndicator = vi.fn();
const mockWsGetState = vi.fn(() => ConnectionState.CONNECTED);

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({
    sendMessage: mockSendMessage,
    sendDMMessage: mockSendDMMessage,
    sendTypingIndicator: mockSendTypingIndicator,
    sendDMTypingIndicator: mockSendDMTypingIndicator,
    getState: mockWsGetState,
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

// useMessaging — mock the entire hook to isolate useChatController tests
const mockMessagingSendMessage = vi.fn(() => 'client-msg-1');
const mockMessagingSendDMMessage = vi.fn(() => 'client-msg-1');
const mockMarkDelivered = vi.fn();
const mockMessagingSendTyping = vi.fn();

vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: () => ({
    sendMessage: mockMessagingSendMessage,
    sendDMMessage: mockMessagingSendDMMessage,
    markDelivered: mockMarkDelivered,
    sendTyping: mockMessagingSendTyping,
    getPendingCount: vi.fn(() => 0),
    getPendingMessagesForChannel: vi.fn(() => []),
  }),
}));

// API client
const mockApiFetch = vi.fn();

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (res: Response) => res.json(),
}));

// E2EE service
const mockEncryptForChannel = vi.fn();
const mockEncryptForChannelWithVersion = vi.fn();
const mockGetCurrentKeyVersion = vi.fn(() => 1);
const mockInvalidateChannelKey = vi.fn();
let mockE2EEIsInitialized = true;

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    encryptForChannel: (...args: unknown[]) => mockEncryptForChannel(...args),
    encryptForChannelWithVersion: (...args: unknown[]) => mockEncryptForChannelWithVersion(...args),
    getCurrentKeyVersion: (...args: unknown[]) => mockGetCurrentKeyVersion(...args),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    get isInitialized() {
      return mockE2EEIsInitialized;
    },
  },
}));

// Pin service
const mockPinMessage = vi.fn().mockResolvedValue({});
const mockUnpinMessage = vi.fn().mockResolvedValue({});

vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: (...args: unknown[]) => mockPinMessage(...args),
  unpinMessage: (...args: unknown[]) => mockUnpinMessage(...args),
}));

// Message queue (needed by useMessaging mock setup but not directly tested here)
vi.mock('@/renderer/services/messaging/messageQueue', () => ({
  getMessageQueue: () => ({
    enqueue: vi.fn(() => 'client-msg-1'),
    markAsSent: vi.fn(),
    markAsDelivered: vi.fn(),
    markAsFailed: vi.fn(),
    remove: vi.fn(),
    startProcessing: vi.fn(),
    stopProcessing: vi.fn(),
    size: vi.fn(() => 0),
    getMessagesForChannel: vi.fn(() => []),
  }),
}));

// --- Test contexts ---

const channelCtx: ChatContext = {
  type: 'channel',
  id: 'channel-1',
  serverId: 'server-1',
};

const encryptedChannelCtx: ChatContext = {
  type: 'channel',
  id: 'channel-2',
  serverId: 'server-1',
};

const dmCtx: ChatContext = {
  type: 'dm',
  id: 'conv-1',
};

const encryptedDMCtx: ChatContext = {
  type: 'dm',
  id: 'conv-2',
};

const voiceCtx: ChatContext = {
  type: 'voice',
  id: 'channel-3',
  serverId: 'server-1',
};

// --- Tests ---

describe('useChatController', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearIndex();
    mockEncryptForChannelWithVersion.mockResolvedValue({
      ciphertext: 'encrypted-content',
      keyVersion: 1,
    });
    resetAllStores();
    useUserStore.setState({ user: mockUser });
    mockWsGetState.mockReturnValue(ConnectionState.CONNECTED);
    mockE2EEIsInitialized = true;
  });

  // ── Return shape ─────────────────────────────────────────────────

  it('returns all expected properties', () => {
    const { result } = renderHook(() => useChatController(channelCtx));
    expect(result.current).toHaveProperty('sendMessage');
    expect(result.current).toHaveProperty('editMessage');
    expect(result.current).toHaveProperty('deleteMessage');
    expect(result.current).toHaveProperty('replyingTo');
    expect(result.current).toHaveProperty('handleReply');
    expect(result.current).toHaveProperty('cancelReply');
    expect(result.current).toHaveProperty('canPin');
    expect(result.current).toHaveProperty('handlePinToggle');
    expect(result.current).toHaveProperty('sendTyping');
    expect(result.current).toHaveProperty('chatContext');
  });

  // ── Send routing ─────────────────────────────────────────────────

  describe('sendMessage routing', () => {
    it('routes channel send to messaging.sendMessage', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.sendMessage('hello'));

      expect(mockMessagingSendMessage).toHaveBeenCalledWith(
        'channel-1',
        'hello',
        'testuser',
        expect.objectContaining({})
      );
      expect(mockMessagingSendDMMessage).not.toHaveBeenCalled();
    });

    it('routes voice send to messaging.sendMessage (channel transport)', () => {
      const { result } = renderHook(() => useChatController(voiceCtx));
      act(() => result.current.sendMessage('hello from voice'));

      expect(mockMessagingSendMessage).toHaveBeenCalledWith(
        'channel-3',
        'hello from voice',
        'testuser',
        expect.objectContaining({})
      );
      expect(mockMessagingSendDMMessage).not.toHaveBeenCalled();
    });

    it('routes DM send to messaging.sendDMMessage', () => {
      const { result } = renderHook(() => useChatController(dmCtx));
      act(() => result.current.sendMessage('hello DM'));

      expect(mockMessagingSendDMMessage).toHaveBeenCalledWith(
        'conv-1',
        'hello DM',
        'testuser',
        expect.objectContaining({})
      );
      expect(mockMessagingSendMessage).not.toHaveBeenCalled();
    });

    it('routes DM send for encrypted DM context', () => {
      const { result } = renderHook(() => useChatController(encryptedDMCtx));
      act(() => result.current.sendMessage('secret'));

      expect(mockMessagingSendDMMessage).toHaveBeenCalledWith(
        'conv-2',
        'secret',
        'testuser',
        expect.objectContaining({})
      );
    });

    it('passes send options through', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() =>
        result.current.sendMessage('hi', {
          mentionMeta: '@user',
          replyToId: 'msg-1',
          attachmentIds: ['att-1'],
        })
      );

      expect(mockMessagingSendMessage).toHaveBeenCalledWith(
        'channel-1',
        'hi',
        'testuser',
        expect.objectContaining({
          mentionMeta: '@user',
          replyToId: 'msg-1',
          attachmentIds: ['att-1'],
        })
      );
    });

    it('clears reply state after send', () => {
      // Set up reply state
      useChatStore.getState().setReplyingTo('channel-1', mockMessage);
      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.replyingTo).not.toBeNull();

      act(() => result.current.sendMessage('replying'));

      expect(result.current.replyingTo).toBeNull();
    });

    it('does nothing when ctx.id is empty', () => {
      const emptyCtx: ChatContext = {
        type: 'channel',
        id: '',
        serverId: 'server-1',
      };
      const { result } = renderHook(() => useChatController(emptyCtx));
      act(() => result.current.sendMessage('nope'));

      expect(mockMessagingSendMessage).not.toHaveBeenCalled();
      expect(mockMessagingSendDMMessage).not.toHaveBeenCalled();
    });
  });

  // ── Edit routing ────────────────────────────────────────────────

  describe('editMessage routing', () => {
    it('uses channel REST path for channel context', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: { content: 'edited', edited_at: '2025-01-01T13:00:00Z' },
          }),
      });

      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'edited');
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/messages/msg-1',
        expect.objectContaining({ method: 'PATCH' })
      );
    });

    it('uses DM REST path for DM context', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: { content: 'edited', edited_at: '2025-01-01T13:00:00Z' },
          }),
      });

      const { result } = renderHook(() => useChatController(dmCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'edited');
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/conv-1/messages/msg-1',
        expect.objectContaining({ method: 'PATCH' })
      );
    });

    it('binds channel edit ciphertext and key version from one encryption result', async () => {
      mockEncryptForChannel.mockResolvedValue('legacy-encrypted-content');
      mockEncryptForChannelWithVersion.mockResolvedValue({
        ciphertext: 'encrypted-content',
        keyVersion: 7,
      });
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: {
              content: 'encrypted-content',
              key_version: 1,
              edited_at: '2025-01-01T13:00:00Z',
            },
          }),
      });

      const { result } = renderHook(() => useChatController(encryptedChannelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'plaintext edit');
      });

      const channelRequest = mockApiFetch.mock.calls[0];
      expect(channelRequest?.[0]).toBe('/api/v1/messages/msg-1');
      const channelRequestOptions: unknown = channelRequest?.[1];
      expect(channelRequestOptions).toEqual(expect.objectContaining({ method: 'PATCH' }));
      if (
        typeof channelRequestOptions !== 'object' ||
        channelRequestOptions === null ||
        !('body' in channelRequestOptions) ||
        typeof channelRequestOptions.body !== 'string'
      ) {
        throw new Error('Expected channel edit request body to be a string');
      }
      const channelPayload: unknown = JSON.parse(channelRequestOptions.body);
      expect(channelPayload).toEqual({ content: 'encrypted-content', key_version: 7 });
      expect(mockEncryptForChannelWithVersion).toHaveBeenCalledWith('channel-2', 'plaintext edit');
    });

    it('binds DM edit ciphertext and key version from one encryption result', async () => {
      mockEncryptForChannel.mockResolvedValue('legacy-encrypted-dm-content');
      mockEncryptForChannelWithVersion.mockResolvedValue({
        ciphertext: 'encrypted-dm-content',
        keyVersion: 7,
      });
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: {
              content: 'encrypted-dm-content',
              key_version: 7,
              edited_at: '2025-01-01T13:00:00Z',
            },
          }),
      });

      const { result } = renderHook(() => useChatController(encryptedDMCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'plaintext DM edit');
      });

      const dmRequest = mockApiFetch.mock.calls[0];
      expect(dmRequest?.[0]).toBe('/api/v1/dm/conversations/conv-2/messages/msg-1');
      const dmRequestOptions: unknown = dmRequest?.[1];
      expect(dmRequestOptions).toEqual(expect.objectContaining({ method: 'PATCH' }));
      if (
        typeof dmRequestOptions !== 'object' ||
        dmRequestOptions === null ||
        !('body' in dmRequestOptions) ||
        typeof dmRequestOptions.body !== 'string'
      ) {
        throw new Error('Expected DM edit request body to be a string');
      }
      const dmPayload: unknown = JSON.parse(dmRequestOptions.body);
      expect(dmPayload).toEqual({ content: 'encrypted-dm-content', key_version: 7 });
      expect(mockEncryptForChannelWithVersion).toHaveBeenCalledWith('conv-2', 'plaintext DM edit');
    });

    it('fails closed without issuing an edit request before E2EE initialization', async () => {
      mockE2EEIsInitialized = false;
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: { content: 'plaintext edit', edited_at: '2025-01-01T13:00:00Z' },
          }),
      });

      const { result } = renderHook(() => useChatController(encryptedChannelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'plaintext edit');
      });

      expect(mockApiFetch).not.toHaveBeenCalled();
    });

    it('stores plaintext and the encryption epoch while clearing stale decrypt state', async () => {
      mockEncryptForChannel.mockResolvedValue('legacy-encrypted-content');
      mockEncryptForChannelWithVersion.mockResolvedValue({
        ciphertext: 'encrypted-content',
        keyVersion: 7,
      });
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: {
              content: 'encrypted-content',
              edited_at: '2025-01-01T13:00:00Z',
            },
          }),
      });

      // Pre-populate store with the message
      useChatStore.getState().addMessage('channel-2', {
        ...mockMessage,
        id: 'msg-1',
        channel_id: 'channel-2',
        key_version: 2,
        decryptFailed: true,
        pendingKeys: true,
      });

      const { result } = renderHook(() => useChatController(encryptedChannelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'plaintext edit');
      });

      // Store should have the plaintext, not ciphertext
      const updatedMsg = useChatStore
        .getState()
        .messagesByChannel.get('channel-2')
        ?.find((m) => m.id === 'msg-1');
      expect(updatedMsg?.content).toBe('plaintext edit');
      expect(updatedMsg?.key_version).toBe(7);
      expect(updatedMsg?.decryptFailed).toBe(false);
      expect(updatedMsg?.pendingKeys).toBe(false);
    });

    it('preserves a GIF slug inside the canonical plaintext edit envelope', async () => {
      mockEncryptForChannel.mockResolvedValue('legacy-encrypted-content');
      mockEncryptForChannelWithVersion.mockResolvedValue({
        ciphertext: 'encrypted-gif-envelope',
        keyVersion: 7,
      });
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            message: {
              content: 'encrypted-gif-envelope',
              key_version: 7,
              edited_at: '2025-01-01T13:00:00Z',
            },
          }),
      });
      useChatStore.getState().addMessage('channel-2', {
        ...mockMessage,
        id: 'msg-1',
        channel_id: 'channel-2',
        gif_slug: 'happy-cat-7',
      });

      const { result } = renderHook(() => useChatController(encryptedChannelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'edited GIF caption');
      });

      expect(mockEncryptForChannelWithVersion).toHaveBeenCalledWith(
        'channel-2',
        '{"text":"edited GIF caption","gif_slug":"happy-cat-7"}'
      );
      const updatedMsg = useChatStore
        .getState()
        .messagesByChannel.get('channel-2')
        ?.find((message) => message.id === 'msg-1');
      expect(updatedMsg?.content).toBe('edited GIF caption');
      expect(updatedMsg?.gif_slug).toBe('happy-cat-7');
    });

    it.each([
      ['channel', encryptedChannelCtx, '/api/v1/messages/msg-epoch-race'],
      ['DM', encryptedDMCtx, '/api/v1/dm/conversations/conv-2/messages/msg-epoch-race'],
    ] as const)(
      're-encrypts and retries a %s edit once after epoch revocation',
      async (_, ctx, url) => {
        mockEncryptForChannelWithVersion
          .mockResolvedValueOnce({ ciphertext: 'stale-edit-ciphertext', keyVersion: 2 })
          .mockResolvedValueOnce({ ciphertext: 'fresh-edit-ciphertext', keyVersion: 3 });
        mockApiFetch
          .mockResolvedValueOnce({
            ok: false,
            status: 409,
            json: () =>
              Promise.resolve({
                error: 'Key epoch has been revoked — re-encrypt with current epoch',
                code: 'epoch_revoked',
                current_epoch: 3,
              }),
          })
          .mockResolvedValueOnce({
            ok: true,
            status: 200,
            json: () =>
              Promise.resolve({
                message: {
                  content: 'fresh-edit-ciphertext',
                  key_version: 3,
                  edited_at: '2025-01-01T13:00:00Z',
                },
              }),
          });
        useChatStore.getState().addMessage(ctx.id, {
          ...mockMessage,
          id: 'msg-epoch-race',
          channel_id: ctx.id,
        });

        const { result } = renderHook(() => useChatController(ctx));
        await act(async () => {
          await result.current.editMessage('msg-epoch-race', 'fresh plaintext edit');
        });

        expect(mockInvalidateChannelKey).toHaveBeenCalledOnce();
        expect(mockInvalidateChannelKey).toHaveBeenCalledWith(ctx.id);
        expect(mockEncryptForChannelWithVersion).toHaveBeenNthCalledWith(
          1,
          ctx.id,
          'fresh plaintext edit'
        );
        expect(mockEncryptForChannelWithVersion).toHaveBeenNthCalledWith(
          2,
          ctx.id,
          'fresh plaintext edit'
        );
        expect(mockApiFetch).toHaveBeenCalledTimes(2);
        expect(mockApiFetch).toHaveBeenNthCalledWith(
          2,
          url,
          expect.objectContaining({
            method: 'PATCH',
            body: JSON.stringify({ content: 'fresh-edit-ciphertext', key_version: 3 }),
          })
        );
        expect(
          useChatStore
            .getState()
            .messagesByChannel.get(ctx.id)
            ?.find((message) => message.id === 'msg-epoch-race')
        ).toMatchObject({
          content: 'fresh plaintext edit',
          key_version: 3,
          decryptFailed: false,
          pendingKeys: false,
        });
      }
    );

    it.each([
      ['channel', encryptedChannelCtx],
      ['DM', encryptedDMCtx],
    ] as const)('replaces the local search document after a successful %s edit', async (_, ctx) => {
      const messageId = `msg-local-search-${ctx.type}`;
      indexMessage(messageId, 'obsoletezebra', ctx.id);
      mockEncryptForChannelWithVersion.mockResolvedValue({
        ciphertext: 'fresh-edit-ciphertext',
        keyVersion: 4,
      });
      mockApiFetch.mockResolvedValue({
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve({
            message: {
              content: 'fresh-edit-ciphertext',
              key_version: 4,
              edited_at: '2025-01-01T13:00:00Z',
            },
          }),
      });
      useChatStore.getState().addMessage(ctx.id, {
        ...mockMessage,
        id: messageId,
        channel_id: ctx.id,
        content: 'obsoletezebra',
      });

      const { result } = renderHook(() => useChatController(ctx));
      await act(async () => {
        await result.current.editMessage(messageId, 'freshwalrus');
      });

      expect(searchMessages('obsoletezebra', ctx.id)).not.toContain(messageId);
      expect(searchMessages('freshwalrus', ctx.id)).toContain(messageId);
    });

    it.each([
      ['channel', encryptedChannelCtx],
      ['DM', encryptedDMCtx],
    ] as const)(
      'does not restore a deleted message when a successful %s edit response arrives late',
      async (_, ctx) => {
        const messageId = `msg-deleted-during-edit-${ctx.type}`;
        const response = deferred<Response>();
        indexMessage(messageId, 'obsoletezebra', ctx.id);
        useChatStore.getState().addMessage(ctx.id, {
          ...mockMessage,
          id: messageId,
          channel_id: ctx.id,
          content: 'obsoletezebra',
        });
        mockApiFetch.mockReturnValueOnce(response.promise);

        const { result } = renderHook(() => useChatController(ctx));
        let editPromise!: Promise<void>;
        await act(async () => {
          editPromise = result.current.editMessage(messageId, 'freshwalrus');
          await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledOnce());
        });

        useChatStore.getState().deleteMessage(ctx.id, messageId);
        removeMessage(messageId);
        response.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              message: {
                content: 'fresh-edit-ciphertext',
                key_version: 4,
                edited_at: '2025-01-01T13:00:00Z',
              },
            }),
        } as Response);
        await act(async () => editPromise);

        expect(
          useChatStore
            .getState()
            .messagesByChannel.get(ctx.id)
            ?.some((message) => message.id === messageId)
        ).toBe(false);
        expect(searchMessages('freshwalrus', ctx.id)).not.toContain(messageId);
      }
    );

    it('handles edit failure gracefully', async () => {
      mockApiFetch.mockResolvedValue({
        ok: false,
        json: () => Promise.resolve({ error: 'Not found' }),
      });

      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'edited');
      });

      expect(consoleSpy).toHaveBeenCalled();
      consoleSpy.mockRestore();
    });
  });

  // ── Delete routing ────────────────────────────────────────────────

  describe('deleteMessage routing', () => {
    it('uses channel REST path for channel context', async () => {
      mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('msg-1');
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/messages/msg-1',
        expect.objectContaining({ method: 'DELETE' })
      );
    });

    it('uses DM REST path for DM context', async () => {
      mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

      const { result } = renderHook(() => useChatController(dmCtx));
      await act(async () => {
        await result.current.deleteMessage('msg-1');
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/conv-1/messages/msg-1',
        expect.objectContaining({ method: 'DELETE' })
      );
    });

    it('removes message from store on success', async () => {
      mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

      useChatStore.getState().addMessage('channel-1', { ...mockMessage, id: 'msg-to-delete' });

      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('msg-to-delete');
      });

      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs?.find((m) => m.id === 'msg-to-delete')).toBeUndefined();
    });

    it.each([
      ['channel', channelCtx],
      ['DM', dmCtx],
    ] as const)(
      'removes the local search document after a successful %s delete',
      async (_, ctx) => {
        const messageId = `msg-local-delete-${ctx.type}`;
        indexMessage(messageId, 'deletedlocalterm', ctx.id);
        mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

        const { result } = renderHook(() => useChatController(ctx));
        await act(async () => {
          await result.current.deleteMessage(messageId);
        });

        expect(searchMessages('deletedlocalterm', ctx.id)).not.toContain(messageId);
      }
    );
  });

  // ── Reply state ─────────────────────────────────────────────────

  describe('reply state management', () => {
    it('reads replyingTo from chatStore for the context ID', () => {
      useChatStore.getState().setReplyingTo('channel-1', mockMessage);

      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.replyingTo).toEqual(mockMessage);
    });

    it('returns null when no reply is set', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.replyingTo).toBeNull();
    });

    it('handleReply sets reply state for the context ID', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.handleReply(mockMessage));

      expect(result.current.replyingTo).toEqual(mockMessage);
      expect(useChatStore.getState().replyingTo.get('channel-1')).toEqual(mockMessage);
    });

    it('cancelReply clears reply state', () => {
      useChatStore.getState().setReplyingTo('channel-1', mockMessage);
      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.replyingTo).not.toBeNull();

      act(() => result.current.cancelReply());
      expect(result.current.replyingTo).toBeNull();
    });

    it('reply state is isolated between context IDs', () => {
      useChatStore.getState().setReplyingTo('channel-1', mockMessage);

      const { result: channelResult } = renderHook(() => useChatController(channelCtx));
      const { result: dmResult } = renderHook(() => useChatController(dmCtx));

      expect(channelResult.current.replyingTo).toEqual(mockMessage);
      expect(dmResult.current.replyingTo).toBeNull();
    });
  });

  // ── Pin ─────────────────────────────────────────────────────────

  describe('pin permissions', () => {
    it('canPin is always true for DM context regardless of RBAC', () => {
      // Ensure no permissions are set
      usePermissionStore.setState({ serverPermissions: {} });

      const { result } = renderHook(() => useChatController(dmCtx));
      expect(result.current.canPin).toBe(true);
    });

    it('canPin checks RBAC for channel context', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': PIN_MESSAGES },
      });

      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.canPin).toBe(true);
    });

    it('canPin is false when channel RBAC denies PIN_MESSAGES', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': 0n },
      });

      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.canPin).toBe(false);
    });

    it('canPin checks RBAC for voice context', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': PIN_MESSAGES },
      });

      const { result } = renderHook(() => useChatController(voiceCtx));
      expect(result.current.canPin).toBe(true);
    });

    it('canPin is false when serverId is missing', () => {
      const noServerCtx: ChatContext = { type: 'channel', id: 'channel-1' };
      const { result } = renderHook(() => useChatController(noServerCtx));
      expect(result.current.canPin).toBe(false);
    });
  });

  describe('handlePinToggle', () => {
    it('calls unpinMessage when message is already pinned', async () => {
      const pinnedMsg: MessageWithStatus = {
        ...mockMessage,
        pinned_at: '2025-01-01T12:00:00Z',
        pinned_by: 'user-1',
      };

      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.handlePinToggle(pinnedMsg);
      });

      expect(mockUnpinMessage).toHaveBeenCalledWith('msg-1');
      expect(mockPinMessage).not.toHaveBeenCalled();
    });

    it('calls pinMessage when message is not pinned', async () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.handlePinToggle(mockMessage);
      });

      expect(mockPinMessage).toHaveBeenCalledWith('msg-1');
      expect(mockUnpinMessage).not.toHaveBeenCalled();
    });
  });

  // ── Typing ──────────────────────────────────────────────────────

  describe('typing indicator routing', () => {
    it('routes channel typing to sendTypingIndicator', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendTypingIndicator).toHaveBeenCalledWith('channel-1', true);
      expect(mockSendDMTypingIndicator).not.toHaveBeenCalled();
    });

    it('routes voice typing to sendTypingIndicator (channel transport)', () => {
      const { result } = renderHook(() => useChatController(voiceCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendTypingIndicator).toHaveBeenCalledWith('channel-3', true);
      expect(mockSendDMTypingIndicator).not.toHaveBeenCalled();
    });

    it('routes DM typing to sendDMTypingIndicator', () => {
      const { result } = renderHook(() => useChatController(dmCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendDMTypingIndicator).toHaveBeenCalledWith('conv-1', true);
      expect(mockSendTypingIndicator).not.toHaveBeenCalled();
    });

    it('does not send typing when disconnected', () => {
      mockWsGetState.mockReturnValue(ConnectionState.DISCONNECTED);

      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendTypingIndicator).not.toHaveBeenCalled();
      expect(mockSendDMTypingIndicator).not.toHaveBeenCalled();
    });
  });

  // ── ChatContext derivation ──────────────────────────────────────

  describe('chatContext', () => {
    it('returns "channel" for channel context', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.chatContext).toBe('channel');
    });

    it('returns "dm" for DM context', () => {
      const { result } = renderHook(() => useChatController(dmCtx));
      expect(result.current.chatContext).toBe('dm');
    });

    it('returns "voice" for voice context', () => {
      const { result } = renderHook(() => useChatController(voiceCtx));
      expect(result.current.chatContext).toBe('voice');
    });
  });
});
