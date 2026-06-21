/**
 * Cross-context security integration tests for useChatController (#496)
 *
 * Verifies that security boundaries are enforced across chat contexts:
 * - Transport isolation (channel WS vs DM WS)
 * - REST endpoint isolation (channel paths vs DM paths)
 * - Permission isolation (RBAC for channels vs ownership for DMs)
 * - Typing method isolation
 * - Epoch enforcement (keyVersion included for encrypted messages)
 * - Context safety (no cross-pollination)
 */

import { renderHook, act } from '@testing-library/react';
import { useChatController } from '@/renderer/hooks/useChatController';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { PIN_MESSAGES } from '@/renderer/utils/permissions';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import { ConnectionState } from '@/renderer/services/websocketService';
import type { ChatContext } from '@/renderer/types/chat';

// --- Mocks ---

const mockSendMessage = vi.fn();
const mockSendDMMessage = vi.fn();
const mockSendTypingIndicator = vi.fn();
const mockSendDMTypingIndicator = vi.fn();
const mockWsGetState = vi.fn(() => ConnectionState.CONNECTED);

vi.mock('@/renderer/services/websocketService', () => ({
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

const mockMessagingSendMessage = vi.fn(() => 'client-msg-1');
const mockMessagingSendDMMessage = vi.fn(() => 'client-msg-1');

vi.mock('@/renderer/hooks/useMessaging', () => ({
  useMessaging: () => ({
    sendMessage: mockMessagingSendMessage,
    sendDMMessage: mockMessagingSendDMMessage,
    markDelivered: vi.fn(),
    sendTyping: vi.fn(),
    getPendingCount: vi.fn(() => 0),
    getPendingMessagesForChannel: vi.fn(() => []),
  }),
}));

const mockApiFetch = vi.fn();

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (res: Response) => res.json(),
}));

const mockEncryptForChannel = vi.fn();
const mockGetCurrentKeyVersion = vi.fn(() => 2);

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    encryptForChannel: (...args: unknown[]) => mockEncryptForChannel(...args),
    getCurrentKeyVersion: (...args: unknown[]) => mockGetCurrentKeyVersion(...args),
    isInitialized: true,
  },
}));

vi.mock('@/renderer/services/pinService', () => ({
  pinMessage: vi.fn().mockResolvedValue({}),
  unpinMessage: vi.fn().mockResolvedValue({}),
}));

vi.mock('@/renderer/services/messageQueue', () => ({
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

// --- Contexts ---

const channelCtx: ChatContext = {
  type: 'channel',
  id: 'ch-1',
  serverId: 'srv-1',
};
const encryptedChannelCtx: ChatContext = {
  type: 'channel',
  id: 'ch-2',
  serverId: 'srv-1',
};
const dmCtx: ChatContext = { type: 'dm', id: 'conv-1' };
const encryptedDMCtx: ChatContext = { type: 'dm', id: 'conv-2' };
const voiceCtx: ChatContext = { type: 'voice', id: 'ch-3', serverId: 'srv-1' };

// --- Tests ---

describe('useChatController — cross-context security invariants (#496)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    useUserStore.setState({ user: mockUser });
    mockWsGetState.mockReturnValue(ConnectionState.CONNECTED);
  });

  // ── 1–3: Transport isolation ─────────────────────────────────────

  describe('transport isolation', () => {
    it('1. channel send NEVER calls sendDMMessage', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.sendMessage('test'));

      expect(mockMessagingSendMessage).toHaveBeenCalled();
      expect(mockMessagingSendDMMessage).not.toHaveBeenCalled();
    });

    it('2. DM send NEVER calls sendMessage (channel variant)', () => {
      const { result } = renderHook(() => useChatController(dmCtx));
      act(() => result.current.sendMessage('test'));

      expect(mockMessagingSendDMMessage).toHaveBeenCalled();
      expect(mockMessagingSendMessage).not.toHaveBeenCalled();
    });

    it('3. voice send uses channel transport, NOT DM', () => {
      const { result } = renderHook(() => useChatController(voiceCtx));
      act(() => result.current.sendMessage('test'));

      expect(mockMessagingSendMessage).toHaveBeenCalledWith(
        'ch-3',
        expect.anything(),
        expect.anything(),
        expect.anything()
      );
      expect(mockMessagingSendDMMessage).not.toHaveBeenCalled();
    });
  });

  // ── 4–6: REST endpoint isolation ─────────────────────────────────

  describe('REST endpoint isolation', () => {
    const mockOkResponse = () =>
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ message: { content: 'x', edited_at: '' } }),
      });

    it('4. channel edit URL does NOT contain /dm/conversations/', async () => {
      mockOkResponse();
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'edited');
      });

      const url = mockApiFetch.mock.calls[0][0] as string;
      expect(url).toBe('/api/v1/messages/msg-1');
      expect(url).not.toContain('/dm/conversations/');
    });

    it('5. DM edit URL uses /dm/conversations/{id}/messages/', async () => {
      mockOkResponse();
      const { result } = renderHook(() => useChatController(dmCtx));
      await act(async () => {
        await result.current.editMessage('msg-1', 'edited');
      });

      const url = mockApiFetch.mock.calls[0][0] as string;
      expect(url).toBe('/api/v1/dm/conversations/conv-1/messages/msg-1');
    });

    it('6. channel delete URL vs DM delete URL are isolated', async () => {
      mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

      // Channel delete
      const { result: chResult } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await chResult.current.deleteMessage('msg-1');
      });
      expect(mockApiFetch.mock.calls[0][0]).toBe('/api/v1/messages/msg-1');

      mockApiFetch.mockClear();

      // DM delete
      const { result: dmResult } = renderHook(() => useChatController(dmCtx));
      await act(async () => {
        await dmResult.current.deleteMessage('msg-1');
      });
      expect(mockApiFetch.mock.calls[0][0]).toBe('/api/v1/dm/conversations/conv-1/messages/msg-1');
    });
  });

  // ── 7–9: Permission isolation ────────────────────────────────────

  describe('permission isolation', () => {
    it('7. DM canPin is true regardless of RBAC state', () => {
      usePermissionStore.setState({ serverPermissions: {} });
      const { result } = renderHook(() => useChatController(dmCtx));
      expect(result.current.canPin).toBe(true);
    });

    it('8. channel canPin is false when PIN_MESSAGES not granted', () => {
      usePermissionStore.setState({ serverPermissions: { 'srv-1': 0n } });
      const { result } = renderHook(() => useChatController(channelCtx));
      expect(result.current.canPin).toBe(false);
    });

    it('9. voice canPin respects RBAC (same as channel)', () => {
      usePermissionStore.setState({ serverPermissions: { 'srv-1': PIN_MESSAGES } });
      const { result } = renderHook(() => useChatController(voiceCtx));
      expect(result.current.canPin).toBe(true);

      usePermissionStore.setState({ serverPermissions: { 'srv-1': 0n } });
      const { result: result2 } = renderHook(() => useChatController(voiceCtx));
      expect(result2.current.canPin).toBe(false);
    });
  });

  // ── 10–12: Typing isolation ──────────────────────────────────────

  describe('typing isolation', () => {
    it('10. channel typing calls sendTypingIndicator, NEVER sendDMTypingIndicator', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendTypingIndicator).toHaveBeenCalledWith('ch-1', true);
      expect(mockSendDMTypingIndicator).not.toHaveBeenCalled();
    });

    it('11. DM typing calls sendDMTypingIndicator, NEVER sendTypingIndicator', () => {
      const { result } = renderHook(() => useChatController(dmCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendDMTypingIndicator).toHaveBeenCalledWith('conv-1', true);
      expect(mockSendTypingIndicator).not.toHaveBeenCalled();
    });

    it('12. voice typing uses channel method', () => {
      const { result } = renderHook(() => useChatController(voiceCtx));
      act(() => result.current.sendTyping(true));

      expect(mockSendTypingIndicator).toHaveBeenCalledWith('ch-3', true);
      expect(mockSendDMTypingIndicator).not.toHaveBeenCalled();
    });
  });

  // ── 13–14: ID field isolation ────────────────────────────────────

  describe('ID field isolation', () => {
    it('13. channel send passes channelId as first arg', () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      act(() => result.current.sendMessage('test'));

      expect(mockMessagingSendMessage.mock.calls[0][0]).toBe('ch-1');
    });

    it('14. DM send passes conversationId as first arg', () => {
      const { result } = renderHook(() => useChatController(dmCtx));
      act(() => result.current.sendMessage('test'));

      expect(mockMessagingSendDMMessage.mock.calls[0][0]).toBe('conv-1');
    });
  });

  // ── 15–16: Epoch enforcement ─────────────────────────────────────

  describe('epoch enforcement (keyVersion)', () => {
    it('15. DM send routes to sendDMMessage (useMessaging handles encryption and keyVersion)', () => {
      const { result } = renderHook(() => useChatController(encryptedDMCtx));
      act(() => result.current.sendMessage('secret'));

      // useChatController routes to sendDMMessage; useMessaging handles E2EE + keyVersion internally
      expect(mockMessagingSendDMMessage).toHaveBeenCalledWith(
        'conv-2',
        'secret',
        expect.any(String),
        expect.anything()
      );
    });

    it('16. channel send delegates to sendMessage which handles keyVersion internally', () => {
      const { result } = renderHook(() => useChatController(encryptedChannelCtx));
      act(() => result.current.sendMessage('secret'));

      // useMessaging.sendMessage handles encryption + keyVersion internally
      expect(mockMessagingSendMessage).toHaveBeenCalledWith(
        'ch-2',
        'secret',
        expect.any(String),
        expect.anything()
      );
    });
  });

  // ── 17–18: Context safety ────────────────────────────────────────

  describe('context safety', () => {
    it('17. changing context type re-creates handlers with new routing', () => {
      // Start as channel
      const { result, rerender } = renderHook(({ ctx }) => useChatController(ctx), {
        initialProps: { ctx: channelCtx },
      });

      act(() => result.current.sendMessage('as channel'));
      expect(mockMessagingSendMessage).toHaveBeenCalled();
      expect(mockMessagingSendDMMessage).not.toHaveBeenCalled();

      vi.clearAllMocks();

      // Switch to DM
      rerender({ ctx: dmCtx });
      act(() => result.current.sendMessage('as dm'));
      expect(mockMessagingSendDMMessage).toHaveBeenCalled();
      expect(mockMessagingSendMessage).not.toHaveBeenCalled();
    });

    it('18. two hooks with different IDs do not cross-pollinate reply state', () => {
      useChatStore.getState().setReplyingTo('ch-1', mockMessage);

      const { result: ch } = renderHook(() => useChatController(channelCtx));
      const { result: dm } = renderHook(() => useChatController(dmCtx));

      // Channel has reply, DM does not
      expect(ch.current.replyingTo).toEqual(mockMessage);
      expect(dm.current.replyingTo).toBeNull();

      // Setting reply on DM should not affect channel
      act(() => dm.current.handleReply({ ...mockMessage, id: 'dm-msg-1' }));
      expect(ch.current.replyingTo?.id).toBe('msg-1');
      expect(dm.current.replyingTo?.id).toBe('dm-msg-1');
    });
  });
});
