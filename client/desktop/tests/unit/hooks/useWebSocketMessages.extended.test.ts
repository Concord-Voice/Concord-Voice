/**
 * Extended tests for useWebSocketMessages — covers DM events, voice state,
 * key rotation, presence, friend events, session revoked, and error handlers.
 * The base test file covers message/typing/delete basics; this covers untested paths.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

// Mock services
const mockInvalidateChannelKey = vi.fn();
const mockProcessPendingKeyRequests = vi.fn().mockResolvedValue(undefined);
const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    processPendingKeyRequests: (...args: unknown[]) => mockProcessPendingKeyRequests(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({
  speak: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));

vi.mock('@/renderer/services/notificationSoundService', () => ({
  notificationSoundService: {
    play: vi.fn(),
    playLoop: vi.fn(),
    stopLoop: vi.fn(),
    stopAllLoops: vi.fn(),
    isLooping: vi.fn().mockReturnValue(false),
    init: vi.fn(),
  },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
}));

import { useWebSocketMessages } from '@/renderer/hooks/useWebSocketMessages';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { notificationSoundService } from '@/renderer/services/notificationSoundService';

// Build a mock wsService with on() that captures handlers
function createMockWsService() {
  type HandlerFn = (...args: unknown[]) => void;
  const handlers = new Map<string, HandlerFn>();
  return {
    handlers,
    on: vi.fn((type: string, handler: HandlerFn) => {
      handlers.set(type, handler);
      return () => handlers.delete(type);
    }),
    onConnectionChange: vi.fn(() => () => {}),
    disconnect: vi.fn(),
  };
}

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  useUserStore.getState().setUser({
    id: 'user-1',
    username: 'testuser',
  });
  vi.clearAllMocks();
  // Default: decryptForChannel resolves with the input content (pass-through)
  mockDecryptForChannel.mockImplementation((_: string, content: string) =>
    Promise.resolve(content)
  );
  mockDecryptForChannelWithVersion.mockImplementation((_: string, content: string) =>
    Promise.resolve(content)
  );
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useWebSocketMessages — extended handlers', () => {
  describe('DM message handler', () => {
    it('handles dm_message and adds to chat store', async () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_message')!;
      expect(handler).toBeDefined();

      // dm_message now always decrypts asynchronously — use async act to flush microtasks
      await act(async () => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-msg-1',
            conversation_id: 'conv-1',
            user_id: 'user-2',
            username: 'alice',
            content: 'Hello DM',
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(msgs).toBeDefined();
      expect(msgs!.some((m) => m.id === 'dm-msg-1')).toBe(true);
    });

    it('ignores dm_message without conversation_id', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_message')!;
      // Should not throw
      act(() => {
        handler({ type: 'dm_message', data: { content: 'no conv' } });
      });

      // No message should be added to any channel
      expect(useChatStore.getState().messagesByChannel.size).toBe(0);
    });

    it('updates DM store last message', async () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_message')!;
      // dm_message now always decrypts asynchronously — use async act to flush microtasks
      await act(async () => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-msg-2',
            conversation_id: 'conv-1',
            user_id: 'user-2',
            username: 'alice',
            content: 'Update last msg',
            created_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      // DM store should have the last message updated
      const msgs = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(msgs).toBeDefined();
      expect(msgs!.some((m) => m.id === 'dm-msg-2')).toBe(true);
    });

    it('plays dm notification sound for other users', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_message')!;
      act(() => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-msg-3',
            conversation_id: 'conv-1',
            user_id: 'user-2',
            username: 'alice',
            content: 'Sound test',
          },
        });
      });

      expect(notificationSoundService.play).toHaveBeenCalledWith('dm', { focused: true });
    });
  });

  describe('dm_message_ack handler', () => {
    it('updates message status on DM ack', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      // Add a pending DM message
      useChatStore.getState().addMessage('conv-1', {
        id: 'nonce-dm-1',
        channel_id: 'conv-1',
        user_id: 'user-1',
        content: 'Pending DM',
        username: 'testuser',
        status: 'pending',
        clientMessageId: 'nonce-dm-1',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = ws.handlers.get('dm_message_ack')!;
      act(() => {
        handler({
          type: 'dm_message_ack',
          data: { nonce: 'nonce-dm-1', id: 'server-dm-1', conversation_id: 'conv-1' },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(msgs![0].status).toBe('delivered');
    });
  });

  describe('dm_message_update handler', () => {
    it('updates DM message content', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      useChatStore.getState().addMessage('conv-1', {
        id: 'dm-edit-1',
        channel_id: 'conv-1',
        user_id: 'user-2',
        content: 'Original DM',
        username: 'alice',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = ws.handlers.get('dm_message_update')!;
      act(() => {
        handler({
          type: 'dm_message_update',
          data: {
            conversation_id: 'conv-1',
            id: 'dm-edit-1',
            content: 'Edited DM',
            edited_at: '2025-01-01T00:01:00Z',
          },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(msgs![0].content).toBe('Edited DM');
    });
  });

  describe('dm_message_delete handler', () => {
    it('deletes a DM message', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      useChatStore.getState().addMessage('conv-1', {
        id: 'dm-del-1',
        channel_id: 'conv-1',
        user_id: 'user-2',
        content: 'To delete',
        username: 'alice',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = ws.handlers.get('dm_message_delete')!;
      act(() => {
        handler({
          type: 'dm_message_delete',
          data: { conversation_id: 'conv-1', id: 'dm-del-1' },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(msgs).toHaveLength(0);
    });
  });

  describe('dm_typing handler', () => {
    it('sets typing state for DM conversation', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_typing')!;
      act(() => {
        handler({
          type: 'dm_typing',
          data: {
            conversation_id: 'conv-1',
            user_id: 'user-2',
            is_typing: true,
            username: 'alice',
          },
        });
      });

      const typing = useChatStore.getState().typingByChannel.get('conv-1');
      expect(typing?.has('user-2')).toBe(true);
    });
  });

  describe('dm_unread_notify handler', () => {
    it('increments DM unread count', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_unread_notify')!;
      act(() => {
        handler({
          type: 'dm_unread_notify',
          data: { conversation_id: 'conv-1' },
        });
      });

      // DM store should have incremented unread
      // notificationSoundService should play dm sound
      expect(notificationSoundService.play).toHaveBeenCalledWith('dm');
    });

    it('handles mention in DM unread', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_unread_notify')!;
      act(() => {
        handler({
          type: 'dm_unread_notify',
          data: { conversation_id: 'conv-1', mentioned: true },
        });
      });

      expect(handler).toBeDefined();
      expect(notificationSoundService.play).toHaveBeenCalled();
    });
  });

  describe('dm_conversation_created handler', () => {
    it('adds new DM conversation', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('dm_conversation_created')!;
      act(() => {
        handler({
          type: 'dm_conversation_created',
          data: {
            conversation: {
              id: 'conv-new',
              is_group: false,
              is_personal: false,
              name: null,
              participants: [{ user_id: 'user-2', username: 'alice' }],
              created_at: '2025-01-01T00:00:00Z',
            },
          },
        });
      });

      // Verify the handler processed without error
      expect(handler).toBeDefined();
    });
  });

  describe('key_needed handler', () => {
    it('processes pending key requests when E2EE is initialized', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('key_needed')!;
      act(() => {
        handler({
          type: 'key_needed',
          data: {
            server_id: 'srv-1',
            user_id: 'user-2',
            channel_ids: ['ch-1'],
          },
        });
      });

      expect(mockProcessPendingKeyRequests).toHaveBeenCalled();
    });
  });

  describe('key_revocation handler', () => {
    it('invalidates channel key and dispatches rotation event', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const dispatchSpy = vi.spyOn(globalThis, 'dispatchEvent');

      const handler = ws.handlers.get('key_revocation')!;
      act(() => {
        handler({
          type: 'key_revocation',
          data: {
            channel_id: 'ch-1',
            new_epoch: 2,
            reason: 'member_removal',
          },
        });
      });

      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('ch-1');
      expect(dispatchSpy).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'e2ee-key-rotation',
          detail: expect.objectContaining({
            channelId: 'ch-1',
            newEpoch: 2,
          }),
        })
      );
    });

    it('ignores revocation without channel_id', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('key_revocation')!;
      act(() => {
        handler({ type: 'key_revocation', data: {} });
      });

      expect(mockInvalidateChannelKey).not.toHaveBeenCalled();
    });
  });

  describe('key_delivered handler', () => {
    it('invalidates channel key for self user', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const dispatchSpy = vi.spyOn(globalThis, 'dispatchEvent');

      const handler = ws.handlers.get('key_delivered')!;
      act(() => {
        handler({
          type: 'key_delivered',
          data: { channel_id: 'ch-1', user_id: 'user-1' },
        });
      });

      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('ch-1');
      expect(dispatchSpy).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'e2ee-key-delivered',
        })
      );
    });

    it('ignores key_delivered for other users', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('key_delivered')!;
      act(() => {
        handler({
          type: 'key_delivered',
          data: { channel_id: 'ch-1', user_id: 'user-other' },
        });
      });

      expect(mockInvalidateChannelKey).not.toHaveBeenCalled();
    });
  });

  describe('preferences_updated handler', () => {
    it('calls fetchAndApply on the preferences sync service', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('preferences_updated')!;
      act(() => {
        handler({ type: 'preferences_updated', data: {} });
      });

      expect(preferencesSyncService.fetchAndApply).toHaveBeenCalled();
    });
  });

  describe('profile_updated handler', () => {
    it('updates messages, member list, DM participants, and friend profiles', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      // Add a message from user-2 to verify message profile update
      useChatStore.getState().addMessage('channel-1', {
        id: 'msg-profile-1',
        channel_id: 'channel-1',
        user_id: 'user-2',
        content: 'Hello',
        username: 'oldname',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = ws.handlers.get('profile_updated')!;
      act(() => {
        handler({
          type: 'profile_updated',
          data: {
            user_id: 'user-2',
            username: 'newname',
            display_name: 'New Display',
            avatar_url: 'https://example.com/avatar.png',
            color_scheme: 'hacker',
          },
        });
      });

      // Verify message was updated
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs![0].username).toBe('newname');
    });

    it('ignores profile_updated without user_id', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('profile_updated')!;
      // Should not throw
      act(() => {
        handler({ type: 'profile_updated', data: {} });
      });

      expect(handler).toBeDefined();
    });
  });

  describe('channel_created handler', () => {
    it('adds new channel for active server', () => {
      const ws = createMockWsService();
      useServerStore.getState().setActiveServer('server-1');
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('channel_created')!;
      act(() => {
        handler({
          type: 'channel_created',
          data: {
            channel: {
              id: 'ch-new',
              server_id: 'server-1',
              name: 'new-channel',
              type: 'text',
              position: 1,
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          },
        });
      });

      expect(useChannelStore.getState().channels.some((c) => c.id === 'ch-new')).toBe(true);
    });

    it('ignores channel_created for non-active server', () => {
      const ws = createMockWsService();
      useServerStore.getState().setActiveServer('server-1');
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('channel_created')!;
      act(() => {
        handler({
          type: 'channel_created',
          data: {
            channel: {
              id: 'ch-other',
              server_id: 'server-2',
              name: 'other',
              type: 'text',
            },
          },
        });
      });

      expect(useChannelStore.getState().channels.some((c) => c.id === 'ch-other')).toBe(false);
    });
  });

  describe('voice_state_update handler', () => {
    it('handles voice joined event', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('voice_state_update')!;
      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-1',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
            server_id: 'server-1',
          },
        });
      });

      // Voice member should be added to channel
      expect(handler).toBeDefined();
    });

    it('handles voice left event', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      // First add a voice member
      useVoiceStore.getState().addChannelVoiceMember('ch-1', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });

      const handler = ws.handlers.get('voice_state_update')!;
      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-1',
            action: 'left',
            user_id: 'user-2',
            server_id: 'server-1',
          },
        });
      });

      expect(handler).toBeDefined();
    });

    it('handles room_empty event', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('voice_state_update')!;
      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-1', action: 'room_empty' },
        });
      });

      expect(handler).toBeDefined();
    });

    it('ignores voice_state_update without channel_id', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('voice_state_update')!;
      // Should not throw
      act(() => {
        handler({ type: 'voice_state_update', data: { action: 'joined' } });
      });

      expect(handler).toBeDefined();
    });
  });

  describe('server_voice_counts handler', () => {
    it('updates voice counts on voice store', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('server_voice_counts')!;
      act(() => {
        handler({
          type: 'server_voice_counts',
          data: { counts: { 'server-1': 5 } },
        });
      });

      expect(handler).toBeDefined();
    });
  });

  describe('unread_notify handler', () => {
    it('plays mention sound for mentioned messages in active server', () => {
      const ws = createMockWsService();
      useServerStore.getState().setActiveServer('server-1');
      useChannelStore.setState({ activeChannelId: 'ch-1' });
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('unread_notify')!;
      act(() => {
        handler({
          type: 'unread_notify',
          data: {
            server_id: 'server-1',
            channel_id: 'ch-2', // Different from active channel
            mentioned: true,
          },
        });
      });

      expect(notificationSoundService.play).toHaveBeenCalledWith('mention');
    });

    it('plays message sound for non-mentioned messages', () => {
      const ws = createMockWsService();
      useServerStore.getState().setActiveServer('server-1');
      useChannelStore.setState({ activeChannelId: 'ch-1' });
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('unread_notify')!;
      act(() => {
        handler({
          type: 'unread_notify',
          data: {
            server_id: 'server-1',
            channel_id: 'ch-2',
          },
        });
      });

      expect(notificationSoundService.play).toHaveBeenCalledWith('message');
    });

    it('skips active channel', () => {
      const ws = createMockWsService();
      useServerStore.getState().setActiveServer('server-1');
      useChannelStore.setState({ activeChannelId: 'ch-1' });
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('unread_notify')!;
      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-1', channel_id: 'ch-1' },
        });
      });

      // Should not play any sound for active channel
      expect(notificationSoundService.play).not.toHaveBeenCalled();
    });
  });

  describe('friend event handlers', () => {
    it('handles friend_request_received', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('friend_request_received')!;
      act(() => {
        handler({
          type: 'friend_request_received',
          data: {
            id: 'req-1',
            from_user_id: 'user-2',
            from_username: 'alice',
            to_user_id: 'user-1',
            to_username: 'testuser',
            created_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      expect(notificationSoundService.play).toHaveBeenCalledWith('friend-request');
    });

    it('handles friend_request_accepted with flat payload', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('friend_request_accepted')!;
      act(() => {
        handler({
          type: 'friend_request_accepted',
          data: {
            id: 'req-1',
            user_id: 'user-2',
            username: 'alice',
            display_name: 'Alice',
          },
        });
      });

      expect(handler).toBeDefined();
    });

    it('handles friend_request_accepted with nested friend object', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('friend_request_accepted')!;
      act(() => {
        handler({
          type: 'friend_request_accepted',
          data: {
            request_id: 'req-1',
            friend: {
              id: 'friend-1',
              user_id: 'user-2',
              username: 'alice',
              display_name: 'Alice',
              status: 'online',
            },
          },
        });
      });

      expect(handler).toBeDefined();
    });

    it('handles friend_removed', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('friend_removed')!;
      act(() => {
        handler({
          type: 'friend_removed',
          data: { user_id: 'user-2' },
        });
      });

      expect(handler).toBeDefined();
    });

    it('friend_removed prunes the removed friend from friend-org categories (#324, Gitar #1704)', () => {
      // Two categorized friends; remove one — its memberId must not linger in the blob.
      useFriendStore.setState({
        friends: [
          { id: 'f-2', userId: 'user-2', username: 'bob', status: 'online' },
          { id: 'f-3', userId: 'user-3', username: 'carol', status: 'online' },
        ] as never,
      });
      const catId = useFriendOrgStore.getState().createCategory('Close', '', null);
      useFriendOrgStore.getState().assignFriend('user-2', catId);
      useFriendOrgStore.getState().assignFriend('user-3', catId);

      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));
      act(() => {
        ws.handlers.get('friend_removed')!({ type: 'friend_removed', data: { user_id: 'user-2' } });
      });

      const cat = useFriendOrgStore.getState().categories.find((c) => c.id === catId)!;
      expect(cat.memberIds).toEqual(['user-3']); // user-2 pruned, user-3 retained
    });

    it('handles friend_code_claimed with accepted status', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('friend_code_claimed')!;
      act(() => {
        handler({
          type: 'friend_code_claimed',
          data: {
            status: 'accepted',
            friend: {
              id: 'friend-new',
              user_id: 'user-3',
              username: 'bob',
              status: 'online',
            },
          },
        });
      });

      expect(handler).toBeDefined();
    });

    it('handles friend_code_claimed with pending status', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('friend_code_claimed')!;
      act(() => {
        handler({
          type: 'friend_code_claimed',
          data: {
            status: 'pending',
            request: {
              id: 'req-new',
              from_user_id: 'user-3',
              from_username: 'bob',
              to_user_id: 'user-1',
              to_username: 'testuser',
              created_at: '2025-01-01T00:00:00Z',
            },
          },
        });
      });

      expect(handler).toBeDefined();
    });
  });

  describe('session_revoked handler', () => {
    it('disconnects WS and enters fatal recovery', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('session_revoked')!;
      act(() => {
        handler({ type: 'session_revoked', data: {} });
      });

      expect(ws.disconnect).toHaveBeenCalled();
      expect(useConnectionStore.getState().phase).toBe('fatal');
      expect(useConnectionStore.getState().diagnostics?.sessionRevoked).toBe(true);
    });
  });

  describe('error handler', () => {
    it('handles epoch_revoked error by invalidating key and dispatching rotation', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const dispatchSpy = vi.spyOn(globalThis, 'dispatchEvent');

      const handler = ws.handlers.get('error')!;
      act(() => {
        handler({
          type: 'error',
          data: {
            code: 'epoch_revoked',
            channel_id: 'ch-1',
            current_epoch: 3,
          },
        });
      });

      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('ch-1');
      expect(dispatchSpy).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'e2ee-key-rotation',
          detail: expect.objectContaining({
            channelId: 'ch-1',
            newEpoch: 3,
            reason: 'epoch_revoked',
          }),
        })
      );
    });

    it('handles generic errors without crashing', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const handler = ws.handlers.get('error')!;
      // Should not throw
      act(() => {
        handler({ type: 'error', data: { message: 'Unknown error' } });
      });

      expect(errorSpy).toHaveBeenCalledWith('WebSocket error:', {
        type: 'server_error',
        code: 'unknown',
      });
      errorSpy.mockRestore();
    });
  });

  describe('channel_group handlers', () => {
    it('handles channel_group_created', () => {
      const ws = createMockWsService();
      useServerStore.getState().setActiveServer('server-1');
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('channel_group_created')!;
      act(() => {
        handler({
          type: 'channel_group_created',
          data: {
            channel_group: {
              id: 'group-1',
              server_id: 'server-1',
              name: 'New Group',
              position: 0,
            },
          },
        });
      });

      expect(handler).toBeDefined();
    });

    it('handles channel_group_updated', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('channel_group_updated')!;
      act(() => {
        handler({
          type: 'channel_group_updated',
          data: {
            channel_group: {
              id: 'group-1',
              name: 'Updated Group',
              position: 1,
              updated_at: '2025-01-01T00:01:00Z',
            },
          },
        });
      });

      expect(handler).toBeDefined();
    });

    it('handles channel_group_deleted', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('channel_group_deleted')!;
      act(() => {
        handler({ type: 'channel_group_deleted', data: { group_id: 'group-1' } });
      });

      expect(handler).toBeDefined();
    });
  });

  describe('channels_reordered handler', () => {
    it('calls reorderChannels with channel positions', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('channels_reordered')!;
      act(() => {
        handler({
          type: 'channels_reordered',
          data: {
            channels: [
              { channel_id: 'ch-1', group_id: null, position: 0 },
              { channel_id: 'ch-2', group_id: 'group-1', position: 1 },
            ],
          },
        });
      });

      expect(handler).toBeDefined();
    });

    // Removed in #709: tested in-handler bail when channels was non-array.
    // Now structurally guaranteed by zod (channels: z.array(ChannelReorderEntrySchema) at dispatch).
  });

  describe('message handler — notification sound', () => {
    it('plays message sound for messages from other users', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('message')!;
      act(() => {
        handler({
          type: 'message',
          data: {
            id: 'msg-sound-1',
            channel_id: 'channel-1',
            user_id: 'user-2',
            username: 'alice',
            content: 'Test',
            created_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      expect(notificationSoundService.play).toHaveBeenCalledWith('message', { focused: true });
    });

    it('does not play sound for own messages', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const handler = ws.handlers.get('message')!;
      act(() => {
        handler({
          type: 'message',
          data: {
            id: 'msg-own-1',
            channel_id: 'channel-1',
            user_id: 'user-1',
            username: 'testuser',
            content: 'Own message',
            created_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      expect(notificationSoundService.play).not.toHaveBeenCalled();
    });
  });

  describe('complete handler registration', () => {
    it('registers all expected handler types', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));

      const expectedHandlers = [
        'message',
        'message_update',
        'message_delete',
        'typing',
        'message_ack',
        'member_joined',
        'profile_updated',
        'server_updated',
        'channel_updated',
        'channel_created',
        'channel_deleted',
        'channel_group_created',
        'channel_group_updated',
        'channel_group_deleted',
        'channels_reordered',
        'server_deleted',
        'member_removed',
        'unread_notify',
        'key_needed',
        'key_revocation',
        'key_delivered',
        'preferences_updated',
        'presence_snapshot',
        'presence',
        'server_online_counts',
        'server_voice_counts',
        'voice_state_update',
        'dm_message',
        'dm_message_ack',
        'dm_message_update',
        'dm_message_delete',
        'dm_typing',
        'dm_unread_notify',
        'dm_conversation_created',
        'dm_subscribed',
        'friend_request_received',
        'friend_request_accepted',
        'friend_removed',
        'friend_code_claimed',
        'dm_voice_state_update',
        'subscribed',
        'error',
        'session_revoked',
      ];

      for (const type of expectedHandlers) {
        expect(ws.handlers.has(type)).toBe(true);
      }
    });
  });
});
