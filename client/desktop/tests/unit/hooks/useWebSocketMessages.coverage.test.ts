/**
 * Coverage boost tests for useWebSocketMessages — covers handlers that
 * lack test coverage in the base and extended test files:
 *
 * NEW DM handlers: dm_participant_added, dm_participant_removed,
 *   dm_role_changed, dm_group_deleted
 *
 * Pre-existing uncovered handlers: message_update, message_ack,
 *   member_joined, server_updated, channel_updated, channel_deleted,
 *   server_deleted, member_removed, presence_snapshot, presence,
 *   server_online_counts, dm_voice_state_update, dm_subscribed, subscribed,
 *   voice_state_update (muted/unmuted/video/screen actions),
 *   unread_notify edge cases, message missing channel_id
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockServer } from '../../mocks/fixtures';

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

const mockNotificationPlay = vi.fn();
vi.mock('@/renderer/services/notificationSoundService', () => ({
  notificationSoundService: {
    play: (...args: unknown[]) => mockNotificationPlay(...args),
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

vi.mock('@/renderer/services/searchService', () => ({
  indexMessage: vi.fn(),
}));

const mockShouldNotify = vi.fn().mockReturnValue(false);
const mockNotify = vi.fn();
const mockIncrementBadge = vi.fn();
vi.mock('@/renderer/services/desktopNotificationService', () => ({
  desktopNotificationService: {
    shouldNotify: (...args: unknown[]) => mockShouldNotify(...args),
    notify: (...args: unknown[]) => mockNotify(...args),
    incrementBadge: (...args: unknown[]) => mockIncrementBadge(...args),
  },
}));

import { useWebSocketMessages } from '@/renderer/hooks/useWebSocketMessages';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyHandler = (...args: any[]) => void;

function createMockWsService() {
  const handlers = new Map<string, AnyHandler>();
  return {
    handlers,
    on: vi.fn((type: string, handler: AnyHandler) => {
      handlers.set(type, handler);
      return () => handlers.delete(type);
    }),
    onConnectionChange: vi.fn(() => () => {}),
    disconnect: vi.fn(),
  };
}

/** Helper to render the hook and get a handler by event name. */
function setupHandler(eventName: string) {
  const ws = createMockWsService();
  renderHook(() => useWebSocketMessages(ws as never));
  const handler = ws.handlers.get(eventName)!;
  expect(handler).toBeDefined();
  return { ws, handler };
}

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  useUserStore.getState().setUser({
    id: 'user-1',
    username: 'testuser',
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useWebSocketMessages — coverage boost', () => {
  // ── NEW DM handlers ──────────────────────────────────────────────────

  describe('dm_participant_added handler', () => {
    it('calls fetchConversations when participant added', () => {
      const { handler } = setupHandler('dm_participant_added');
      const fetchSpy = vi.spyOn(useDMStore.getState(), 'fetchConversations');

      act(() => {
        handler({
          type: 'dm_participant_added',
          data: { conversation_id: 'conv-1', user_id: 'user-3' },
        });
      });

      expect(fetchSpy).toHaveBeenCalled();
    });

    // Removed in #709: tested in-handler bail when conversation_id was
    // missing. Now structurally guaranteed by zod at dispatch boundary.
  });

  describe('dm_participant_removed handler', () => {
    it('removes conversation when current user is removed', () => {
      const { handler } = setupHandler('dm_participant_removed');

      // Add a conversation first
      useDMStore.getState().addConversation({
        id: 'conv-1',
        isGroup: true,
        isPersonal: false,
        name: 'Group Chat',
        participants: [
          { userId: 'user-1', username: 'testuser' },
          { userId: 'user-2', username: 'alice' },
        ],
        iconUrl: undefined,
        createdBy: 'user-2',
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2025-01-01T00:00:00Z',
      });

      const removeSpy = vi.spyOn(useDMStore.getState(), 'removeConversation');

      act(() => {
        handler({
          type: 'dm_participant_removed',
          data: { conversation_id: 'conv-1', user_id: 'user-1' },
        });
      });

      expect(removeSpy).toHaveBeenCalledWith('conv-1');
    });

    it('removes participant from conversation when another user is removed', () => {
      const { handler } = setupHandler('dm_participant_removed');

      useDMStore.getState().addConversation({
        id: 'conv-1',
        isGroup: true,
        isPersonal: false,
        name: 'Group Chat',
        participants: [
          { userId: 'user-1', username: 'testuser' },
          { userId: 'user-2', username: 'alice' },
          { userId: 'user-3', username: 'bob' },
        ],
        iconUrl: undefined,
        createdBy: 'user-1',
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2025-01-01T00:00:00Z',
      });

      act(() => {
        handler({
          type: 'dm_participant_removed',
          data: { conversation_id: 'conv-1', user_id: 'user-3' },
        });
      });

      const conv = useDMStore.getState().conversations.find((c) => c.id === 'conv-1');
      expect(conv).toBeDefined();
      expect(conv!.participants.some((p) => p.userId === 'user-3')).toBe(false);
      expect(conv!.participants).toHaveLength(2);
    });

    it('ignores event without conversation_id', () => {
      const { handler } = setupHandler('dm_participant_removed');

      act(() => {
        handler({ type: 'dm_participant_removed', data: { user_id: 'user-2' } });
      });

      // No crash
    });

    it('ignores event without user_id', () => {
      const { handler } = setupHandler('dm_participant_removed');

      act(() => {
        handler({ type: 'dm_participant_removed', data: { conversation_id: 'conv-1' } });
      });

      // No crash
    });
  });

  describe('dm_role_changed handler', () => {
    it('updates participant role in conversation', () => {
      const { handler } = setupHandler('dm_role_changed');

      useDMStore.getState().addConversation({
        id: 'conv-1',
        isGroup: true,
        isPersonal: false,
        name: 'Group Chat',
        participants: [
          { userId: 'user-1', username: 'testuser', role: 'admin' },
          { userId: 'user-2', username: 'alice', role: 'member' },
        ],
        iconUrl: undefined,
        createdBy: 'user-1',
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2025-01-01T00:00:00Z',
      });

      act(() => {
        handler({
          type: 'dm_role_changed',
          data: { conversation_id: 'conv-1', user_id: 'user-2', role: 'admin' },
        });
      });

      const conv = useDMStore.getState().conversations.find((c) => c.id === 'conv-1');
      const participant = conv!.participants.find((p) => p.userId === 'user-2');
      expect(participant!.role).toBe('admin');
    });

    it('ignores event without required fields', () => {
      const { handler } = setupHandler('dm_role_changed');

      // Missing conversation_id
      act(() => {
        handler({ type: 'dm_role_changed', data: { user_id: 'user-2', role: 'admin' } });
      });

      // Missing user_id
      act(() => {
        handler({ type: 'dm_role_changed', data: { conversation_id: 'conv-1', role: 'admin' } });
      });

      // Missing role
      act(() => {
        handler({
          type: 'dm_role_changed',
          data: { conversation_id: 'conv-1', user_id: 'user-2' },
        });
      });

      // No crash
    });
  });

  describe('dm_group_deleted handler', () => {
    it('removes conversation from DM store', () => {
      const { handler } = setupHandler('dm_group_deleted');

      useDMStore.getState().addConversation({
        id: 'conv-del',
        isGroup: true,
        isPersonal: false,
        name: 'To Delete',
        participants: [],
        iconUrl: undefined,
        createdBy: 'user-1',
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2025-01-01T00:00:00Z',
      });

      act(() => {
        handler({ type: 'dm_group_deleted', data: { conversation_id: 'conv-del' } });
      });

      expect(useDMStore.getState().conversations.find((c) => c.id === 'conv-del')).toBeUndefined();
    });

    it('ignores event without conversation_id', () => {
      const { handler } = setupHandler('dm_group_deleted');

      act(() => {
        handler({ type: 'dm_group_deleted', data: {} });
      });

      // No crash
    });
  });

  // ── Pre-existing uncovered handlers ────────────────────────────────

  describe('message_update handler', () => {
    it('updates message content and edit timestamp', () => {
      const { handler } = setupHandler('message_update');

      useChatStore.getState().addMessage('channel-1', {
        id: 'msg-edit-1',
        channel_id: 'channel-1',
        user_id: 'user-2',
        content: 'Original content',
        username: 'alice',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      act(() => {
        handler({
          type: 'message_update',
          data: {
            channel_id: 'channel-1',
            id: 'msg-edit-1',
            content: 'Edited content',
            edited_at: '2025-01-01T00:01:00Z',
            updated_at: '2025-01-01T00:01:00Z',
          },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs![0].content).toBe('Edited content');
    });

    it('ignores update without channel_id', () => {
      const { handler } = setupHandler('message_update');

      act(() => {
        handler({ type: 'message_update', data: { id: 'msg-1', content: 'x' } });
      });

      // No crash
    });

    it('ignores update without message id', () => {
      const { handler } = setupHandler('message_update');

      act(() => {
        handler({ type: 'message_update', data: { channel_id: 'channel-1', content: 'x' } });
      });

      // No crash
    });
  });

  describe('message_ack handler', () => {
    it('updates message status from pending to delivered', () => {
      const { handler } = setupHandler('message_ack');

      useChatStore.getState().addMessage('channel-1', {
        id: 'nonce-1',
        channel_id: 'channel-1',
        user_id: 'user-1',
        content: 'Pending msg',
        username: 'testuser',
        status: 'pending',
        clientMessageId: 'nonce-1',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      act(() => {
        handler({
          type: 'message_ack',
          data: { nonce: 'nonce-1', id: 'server-id-1', channel_id: 'channel-1' },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs![0].status).toBe('delivered');
    });

    it('ignores ack with missing fields', () => {
      const { handler } = setupHandler('message_ack');

      act(() => {
        handler({ type: 'message_ack', data: { nonce: 'nonce-1' } });
      });

      // No crash
    });
  });

  describe('member_joined handler', () => {
    it('adds member when event is for active server', () => {
      useServerStore.getState().setActiveServer('server-1');
      const { handler } = setupHandler('member_joined');

      const addSpy = vi.spyOn(useMemberStore.getState(), 'addMember');

      act(() => {
        handler({
          type: 'member_joined',
          data: {
            server_id: 'server-1',
            user_id: 'user-3',
            username: 'newmember',
            display_name: 'New Member',
            avatar_url: 'https://example.com/avatar.png',
            role: 'member',
          },
        });
      });

      expect(addSpy).toHaveBeenCalledWith(
        expect.objectContaining({
          user_id: 'user-3',
          username: 'newmember',
        })
      );
    });

    it('ignores member_joined for non-active server', () => {
      useServerStore.getState().setActiveServer('server-1');
      const { handler } = setupHandler('member_joined');

      const addSpy = vi.spyOn(useMemberStore.getState(), 'addMember');

      act(() => {
        handler({
          type: 'member_joined',
          data: { server_id: 'server-2', user_id: 'user-3', username: 'other' },
        });
      });

      expect(addSpy).not.toHaveBeenCalled();
    });
  });

  describe('server_updated handler', () => {
    it('updates server properties', () => {
      useServerStore.getState().addServer(mockServer);
      const { handler } = setupHandler('server_updated');

      act(() => {
        handler({
          type: 'server_updated',
          data: {
            server_id: 'server-1',
            name: 'Renamed Server',
            icon_url: 'https://example.com/icon.png',
            banner_url: 'https://example.com/banner.png',
            allow_embedded_content: true,
          },
        });
      });

      const server = useServerStore.getState().servers.find((s) => s.id === 'server-1');
      expect(server?.name).toBe('Renamed Server');
    });

    it('ignores event without server_id', () => {
      const { handler } = setupHandler('server_updated');

      act(() => {
        handler({ type: 'server_updated', data: { name: 'No ID' } });
      });

      // No crash
    });
  });

  describe('channel_updated handler', () => {
    it('updates channel properties', () => {
      const { handler } = setupHandler('channel_updated');

      act(() => {
        handler({
          type: 'channel_updated',
          data: {
            channel_id: 'channel-1',
            name: 'renamed-general',
            type: 'text',
            emoji: '🎉',
            group_id: null,
          },
        });
      });

      const ch = useChannelStore.getState().channels.find((c) => c.id === 'channel-1');
      expect(ch?.name).toBe('renamed-general');
    });

    it('ignores event without channel_id', () => {
      const { handler } = setupHandler('channel_updated');

      act(() => {
        handler({ type: 'channel_updated', data: { name: 'no-id' } });
      });

      // No crash
    });
  });

  describe('channel_deleted handler', () => {
    it('removes channel from store', () => {
      const { handler } = setupHandler('channel_deleted');

      act(() => {
        handler({ type: 'channel_deleted', data: { channel_id: 'channel-1' } });
      });

      expect(useChannelStore.getState().channels.find((c) => c.id === 'channel-1')).toBeUndefined();
    });

    it('ignores event without channel_id', () => {
      const { handler } = setupHandler('channel_deleted');

      act(() => {
        handler({ type: 'channel_deleted', data: {} });
      });

      // No crash
    });
  });

  describe('server_deleted handler', () => {
    it('removes server from store', () => {
      useServerStore.getState().addServer(mockServer);
      const { handler } = setupHandler('server_deleted');

      act(() => {
        handler({ type: 'server_deleted', data: { server_id: 'server-1' } });
      });

      expect(useServerStore.getState().servers.find((s) => s.id === 'server-1')).toBeUndefined();
    });

    it('ignores event without server_id', () => {
      const { handler } = setupHandler('server_deleted');

      act(() => {
        handler({ type: 'server_deleted', data: {} });
      });

      // No crash
    });
  });

  describe('member_removed handler', () => {
    it('removes server when current user is removed', () => {
      useServerStore.getState().addServer(mockServer);
      const { handler } = setupHandler('member_removed');

      act(() => {
        handler({
          type: 'member_removed',
          data: { server_id: 'server-1', user_id: 'user-1' },
        });
      });

      expect(useServerStore.getState().servers.find((s) => s.id === 'server-1')).toBeUndefined();
    });

    it('removes member from member list when another user is removed from active server', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().setActiveServer('server-1');
      useMemberStore.getState().addMember({
        user_id: 'user-2',
        username: 'alice',
        role: 'member',
        joined_at: '2025-01-01T00:00:00Z',
        roles: [],
      });

      const { handler } = setupHandler('member_removed');

      act(() => {
        handler({
          type: 'member_removed',
          data: { server_id: 'server-1', user_id: 'user-2' },
        });
      });

      expect(useMemberStore.getState().members.find((m) => m.user_id === 'user-2')).toBeUndefined();
    });

    it('ignores event without server_id or user_id', () => {
      const { handler } = setupHandler('member_removed');

      act(() => {
        handler({ type: 'member_removed', data: { server_id: 'server-1' } });
      });

      act(() => {
        handler({ type: 'member_removed', data: { user_id: 'user-2' } });
      });

      // No crash
    });
  });

  describe('presence_snapshot handler', () => {
    it('sets presence snapshot with enhanced user+status format', () => {
      const { handler } = setupHandler('presence_snapshot');
      const snapshotSpy = vi.spyOn(useMemberStore.getState(), 'setPresenceSnapshot');

      act(() => {
        handler({
          type: 'presence_snapshot',
          data: {
            users: [
              { user_id: 'user-2', status: 'online' },
              { user_id: 'user-3', status: 'dnd' },
            ],
          },
        });
      });

      expect(snapshotSpy).toHaveBeenCalledWith([
        { user_id: 'user-2', status: 'online' },
        { user_id: 'user-3', status: 'dnd' },
      ]);
    });

    it('handles backward-compatible online_user_ids format', () => {
      const { handler } = setupHandler('presence_snapshot');
      const setOnlineSpy = vi.spyOn(useMemberStore.getState(), 'setOnlineUsers');

      act(() => {
        handler({
          type: 'presence_snapshot',
          data: { online_user_ids: ['user-2', 'user-3'] },
        });
      });

      expect(setOnlineSpy).toHaveBeenCalledWith(['user-2', 'user-3']);
    });

    it('also updates DM participant and friend presence', () => {
      const { handler } = setupHandler('presence_snapshot');
      const dmUpdateSpy = vi.spyOn(useDMStore.getState(), 'updateParticipantProfile');
      const friendUpdateSpy = vi.spyOn(useFriendStore.getState(), 'updateFriendPresence');

      act(() => {
        handler({
          type: 'presence_snapshot',
          data: { users: [{ user_id: 'user-2', status: 'online' }] },
        });
      });

      expect(dmUpdateSpy).toHaveBeenCalledWith('user-2', { status: 'online' });
      expect(friendUpdateSpy).toHaveBeenCalledWith('user-2', 'online');
    });

    it('updates DM/friend presence from backward-compatible format', () => {
      const { handler } = setupHandler('presence_snapshot');
      const dmUpdateSpy = vi.spyOn(useDMStore.getState(), 'updateParticipantProfile');
      const friendUpdateSpy = vi.spyOn(useFriendStore.getState(), 'updateFriendPresence');

      act(() => {
        handler({
          type: 'presence_snapshot',
          data: { online_user_ids: ['user-4'] },
        });
      });

      expect(dmUpdateSpy).toHaveBeenCalledWith('user-4', { status: 'online' });
      expect(friendUpdateSpy).toHaveBeenCalledWith('user-4', 'online');
    });
  });

  describe('presence handler', () => {
    it('updates user status for another user', () => {
      const { handler } = setupHandler('presence');
      const setStatusSpy = vi.spyOn(useMemberStore.getState(), 'setUserStatus');

      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'user-2', status: 'dnd' },
        });
      });

      expect(setStatusSpy).toHaveBeenCalledWith('user-2', 'dnd');
    });

    it('sets last_seen timestamp on offline status', () => {
      const { handler } = setupHandler('presence');
      const setLastSeenSpy = vi.spyOn(useMemberStore.getState(), 'setUserLastSeen');

      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'user-2', status: 'offline', timestamp: 1704067200 },
        });
      });

      expect(setLastSeenSpy).toHaveBeenCalledWith('user-2', 1704067200);
    });

    it('skips self user to preserve local source of truth', () => {
      const { handler } = setupHandler('presence');
      const setStatusSpy = vi.spyOn(useMemberStore.getState(), 'setUserStatus');

      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'user-1', status: 'offline' },
        });
      });

      expect(setStatusSpy).not.toHaveBeenCalled();
    });

    // Removed in #709: tested in-handler bail when user_id was missing.
    // Now structurally guaranteed by zod at dispatch boundary.

    it('updates DM participant and friend presence', () => {
      const { handler } = setupHandler('presence');
      const dmUpdateSpy = vi.spyOn(useDMStore.getState(), 'updateParticipantProfile');
      const friendUpdateSpy = vi.spyOn(useFriendStore.getState(), 'updateFriendPresence');

      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'user-2', status: 'online' },
        });
      });

      expect(dmUpdateSpy).toHaveBeenCalledWith('user-2', { status: 'online' });
      expect(friendUpdateSpy).toHaveBeenCalledWith('user-2', 'online');
    });
  });

  describe('server_online_counts handler', () => {
    it('updates online counts on server store', () => {
      useServerStore.getState().addServer(mockServer);
      const { handler } = setupHandler('server_online_counts');
      const updateSpy = vi.spyOn(useServerStore.getState(), 'updateOnlineCounts');

      act(() => {
        handler({
          type: 'server_online_counts',
          data: { counts: { 'server-1': 42 } },
        });
      });

      expect(updateSpy).toHaveBeenCalledWith({ 'server-1': 42 });
    });

    it('ignores event without counts', () => {
      const { handler } = setupHandler('server_online_counts');

      act(() => {
        handler({ type: 'server_online_counts', data: {} });
      });

      // No crash
    });
  });

  describe('voice_state_update — participant state actions', () => {
    it('updates muted state for active channel participant', () => {
      const { handler } = setupHandler('voice_state_update');

      // Set up active channel and add participant
      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'muted', user_id: 'user-2' },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].isMuted).toBe(true);
    });

    it('updates unmuted state', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: true,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'unmuted', user_id: 'user-2' },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].isMuted).toBe(false);
    });

    it('updates video_on state', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'video_on', user_id: 'user-2' },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].isVideoOn).toBe(true);
    });

    it('updates screen_on state', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'screen_on', user_id: 'user-2' },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].isScreenSharing).toBe(true);
    });

    it('plays user-join notification for other users in active voice channel', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
            server_id: 'server-1',
          },
        });
      });

      expect(mockNotificationPlay).toHaveBeenCalledWith('user-join');
    });

    it('plays user-leave notification for other users in active voice channel', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'left',
            user_id: 'user-2',
            server_id: 'server-1',
          },
        });
      });

      expect(mockNotificationPlay).toHaveBeenCalledWith('user-leave');
    });

    it('does not play notification for own join/leave', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'joined',
            user_id: 'user-1',
            username: 'testuser',
            server_id: 'server-1',
          },
        });
      });

      expect(mockNotificationPlay).not.toHaveBeenCalled();
    });

    it('ignores voice_state_update without action', () => {
      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-1', user_id: 'user-2' },
        });
      });

      // No crash
    });
  });

  describe('voice_state_update — server enforcement actions', () => {
    it('updates sidebar voice member on server_muted', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_muted', user_id: 'user-2' },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-voice'];
      const alice = members?.find((m) => m.userId === 'user-2');
      expect(alice?.serverMuted).toBe(true);

      // Also updates active-channel participant
      expect(useVoiceStore.getState().participants['user-2'].serverMuted).toBe(true);
    });

    it('updates sidebar voice member on server_unmuted', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        serverMuted: true,
        serverDeafened: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_unmuted', user_id: 'user-2' },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-voice'];
      const alice = members?.find((m) => m.userId === 'user-2');
      expect(alice?.serverMuted).toBe(false);
    });

    it('updates sidebar voice member on server_deafened (sets both serverDeafened and serverMuted)', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_deafened', user_id: 'user-2' },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-voice'];
      const alice = members?.find((m) => m.userId === 'user-2');
      expect(alice?.serverDeafened).toBe(true);
      expect(alice?.serverMuted).toBe(true);
    });

    it('updates sidebar voice member on server_undeafened (clears both)', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        serverMuted: true,
        serverDeafened: true,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_undeafened', user_id: 'user-2' },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-voice'];
      const alice = members?.find((m) => m.userId === 'user-2');
      expect(alice?.serverDeafened).toBe(false);
      expect(alice?.serverMuted).toBe(false);
    });

    it('shows toast log when local user is server_muted', () => {
      const { handler } = setupHandler('voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-1',
        username: 'testuser',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_muted', user_id: 'user-1' },
        });
      });

      expect(debugSpy).toHaveBeenCalledWith('[Voice]', 'You have been server-muted by a moderator');
      debugSpy.mockRestore();
    });

    it('shows toast log when local user is server_unmuted', () => {
      const { handler } = setupHandler('voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-1',
        username: 'testuser',
        isMuted: false,
        serverMuted: true,
        serverDeafened: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_unmuted', user_id: 'user-1' },
        });
      });

      expect(debugSpy).toHaveBeenCalledWith('[Voice]', 'A moderator has removed your server mute');
      debugSpy.mockRestore();
    });

    it('shows toast log when local user is server_deafened', () => {
      const { handler } = setupHandler('voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-1',
        username: 'testuser',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_deafened', user_id: 'user-1' },
        });
      });

      expect(debugSpy).toHaveBeenCalledWith(
        '[Voice]',
        'You have been server-deafened by a moderator'
      );
      debugSpy.mockRestore();
    });

    it('shows toast log when local user is server_undeafened', () => {
      const { handler } = setupHandler('voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-1',
        username: 'testuser',
        isMuted: false,
        serverMuted: true,
        serverDeafened: true,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_undeafened', user_id: 'user-1' },
        });
      });

      expect(debugSpy).toHaveBeenCalledWith(
        '[Voice]',
        'A moderator has removed your server deafen'
      );
      debugSpy.mockRestore();
    });

    it('does not show toast for non-local users on enforcement actions', () => {
      const { handler } = setupHandler('voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_muted', user_id: 'user-2' },
        });
      });

      expect(debugSpy).not.toHaveBeenCalledWith(
        '[Voice]',
        expect.stringContaining('You have been')
      );
      debugSpy.mockRestore();
    });
  });

  describe('unread_notify — edge cases', () => {
    it('marks server unread for non-active server', () => {
      useServerStore.getState().setActiveServer('server-1');
      const { handler } = setupHandler('unread_notify');
      const markSpy = vi.spyOn(useUnreadStore.getState(), 'markServerUnread');

      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-2', channel_id: 'ch-5' },
        });
      });

      expect(markSpy).toHaveBeenCalledWith('server-2');
    });

    it('marks server mention for non-active server with mention flag', () => {
      useServerStore.getState().setActiveServer('server-1');
      const { handler } = setupHandler('unread_notify');
      const mentionSpy = vi.spyOn(useUnreadStore.getState(), 'markServerMention');

      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-2', channel_id: 'ch-5', mentioned: true },
        });
      });

      expect(mentionSpy).toHaveBeenCalledWith('server-2');
    });

    it('ignores event without channel_id when server is active', () => {
      useServerStore.getState().setActiveServer('server-1');
      const { handler } = setupHandler('unread_notify');

      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-1' },
        });
      });

      // Should not crash; no unread incremented
    });

    it('increments unread and mention counts for non-active channel', () => {
      useServerStore.getState().setActiveServer('server-1');
      useChannelStore.setState({ activeChannelId: 'ch-1' });
      const { handler } = setupHandler('unread_notify');
      const incrementSpy = vi.spyOn(useUnreadStore.getState(), 'incrementUnread');
      const mentionSpy = vi.spyOn(useUnreadStore.getState(), 'incrementMention');

      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-1', channel_id: 'ch-2', mentioned: true },
        });
      });

      expect(incrementSpy).toHaveBeenCalledWith('ch-2');
      expect(mentionSpy).toHaveBeenCalledWith('ch-2');
    });
  });

  // Removed in #709: `message handler — edge cases` tested in-handler bail
  // when channel_id was missing. Now structurally guaranteed by zod at
  // dispatch boundary; equivalent coverage in websocketService.dispatch.test.ts.

  // Removed in #709: `message_delete handler — edge cases` and
  // `typing handler — edge cases` tested in-handler bail when required
  // fields were missing. Now structurally guaranteed by zod at dispatch
  // boundary; equivalent coverage in websocketService.dispatch.test.ts.
  // See [internal]rules/frontend.md § WebSocket payload validation (rule 7).

  describe('dm_voice_state_update handler', () => {
    it('handles dm voice state update without crashing', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'joined',
            user_id: 'user-2',
          },
        });
      });

      // Just a debug log handler — verify no crash
    });

    it('ignores event without conversation_id', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: { action: 'joined', user_id: 'user-2' },
        });
      });

      // No crash
    });
  });

  describe('dm_subscribed handler', () => {
    it('handles subscription confirmation without crashing', () => {
      const { handler } = setupHandler('dm_subscribed');

      act(() => {
        handler({
          type: 'dm_subscribed',
          data: { conversation_id: 'conv-1' },
        });
      });

      // Debug log only
    });
  });

  describe('subscribed handler', () => {
    it('handles channel subscription confirmation without crashing', () => {
      const { handler } = setupHandler('subscribed');

      act(() => {
        handler({
          type: 'subscribed',
          data: { channel_id: 'channel-1' },
        });
      });

      // Debug log only
    });
  });

  describe('dm_unread_notify — last_message preview', () => {
    it('updates last message preview on DM unread notification', () => {
      const { handler } = setupHandler('dm_unread_notify');

      useDMStore.getState().addConversation({
        id: 'conv-1',
        isGroup: false,
        isPersonal: false,
        name: null,
        participants: [],
        iconUrl: undefined,
        createdBy: 'user-2',
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2025-01-01T00:00:00Z',
      });

      const updateSpy = vi.spyOn(useDMStore.getState(), 'updateLastMessage');

      act(() => {
        handler({
          type: 'dm_unread_notify',
          data: {
            conversation_id: 'conv-1',
            last_message: {
              content: 'Preview text',
              user_id: 'user-2',
              username: 'alice',
              created_at: '2025-01-01T01:00:00Z',
            },
          },
        });
      });

      expect(updateSpy).toHaveBeenCalledWith(
        'conv-1',
        expect.objectContaining({ content: 'Preview text' })
      );
    });
  });

  describe('profile_updated — voice participant updates', () => {
    it('updates voice participant when user is in active call', () => {
      const { handler } = setupHandler('profile_updated');

      // Set up a voice participant
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'old-alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'profile_updated',
          data: {
            user_id: 'user-2',
            username: 'new-alice',
            display_name: 'New Alice',
            avatar_url: 'https://example.com/new.png',
          },
        });
      });

      const participant = useVoiceStore.getState().participants['user-2'];
      expect(participant.username).toBe('new-alice');
    });

    it('updates channel voice members sidebar display', () => {
      const { handler } = setupHandler('profile_updated');

      useVoiceStore
        .getState()
        .setChannelVoiceMembers('ch-1', [
          { userId: 'user-2', username: 'old-alice', isMuted: false },
        ]);

      act(() => {
        handler({
          type: 'profile_updated',
          data: {
            user_id: 'user-2',
            username: 'new-alice',
            display_name: 'New Alice',
          },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-1'];
      expect(members[0].username).toBe('new-alice');
    });

    it('updates available screen shares', () => {
      const { handler } = setupHandler('profile_updated');

      useVoiceStore.setState({
        availableScreenShares: [{ userId: 'user-2', username: 'old-alice', streamId: 'stream-1' }],
      });

      act(() => {
        handler({
          type: 'profile_updated',
          data: {
            user_id: 'user-2',
            username: 'new-alice',
            display_name: 'New Alice',
          },
        });
      });

      const shares = useVoiceStore.getState().availableScreenShares;
      expect(shares[0].username).toBe('new-alice');
    });
  });

  describe('channel_group_created — non-active server', () => {
    it('ignores group creation for non-active server', () => {
      useServerStore.getState().setActiveServer('server-1');
      const { handler } = setupHandler('channel_group_created');

      const addSpy = vi.spyOn(useChannelStore.getState(), 'addChannelGroup');

      act(() => {
        handler({
          type: 'channel_group_created',
          data: {
            channel_group: {
              id: 'group-1',
              server_id: 'server-other',
              name: 'Other Group',
              position: 0,
            },
          },
        });
      });

      expect(addSpy).not.toHaveBeenCalled();
    });

    // Removed in #709: tested in-handler bail when channel_group was absent.
    // Now structurally guaranteed by zod (channel_group required at dispatch).
  });

  // Removed in #709: `channel_group_updated — missing data` tested
  // in-handler bail when channel_group envelope was absent. Now
  // structurally guaranteed by zod (channel_group: ChannelGroupPayloadSchema
  // at dispatch).

  // Removed in #709: `channel_group_deleted — missing data` tested
  // in-handler bail when group_id was absent. Now structurally guaranteed
  // by zod (group_id required at dispatch).

  // ── Additional branch coverage ───────────────────────────────────────

  describe('key_needed — E2EE not initialized branch', () => {
    it('ignores key_needed when E2EE is not initialized', async () => {
      // Temporarily override e2eeService.isInitialized
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      const original = e2eeService.isInitialized;
      Object.defineProperty(e2eeService, 'isInitialized', { value: false, writable: true });

      const { handler } = setupHandler('key_needed');

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

      expect(mockProcessPendingKeyRequests).not.toHaveBeenCalled();

      // Restore
      Object.defineProperty(e2eeService, 'isInitialized', { value: original, writable: true });
    });

    it('handles processPendingKeyRequests rejection gracefully', async () => {
      mockProcessPendingKeyRequests.mockRejectedValueOnce(new Error('key fetch failed'));

      const { handler } = setupHandler('key_needed');

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

      // The catch handler should swallow the error
      await vi.waitFor(() => {
        expect(mockProcessPendingKeyRequests).toHaveBeenCalled();
      });
    });
  });

  describe('dm_message — encrypted path', () => {
    it('handles encrypted DM message', () => {
      mockDecryptForChannel.mockResolvedValue('decrypted DM content');

      const { handler } = setupHandler('dm_message');

      act(() => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-enc-1',
            conversation_id: 'conv-enc',
            user_id: 'user-2',
            username: 'alice',
            content: 'encrypted-ciphertext',
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      expect(mockDecryptForChannel).toHaveBeenCalledWith('conv-enc', 'encrypted-ciphertext');
    });

    it('handles encrypted DM message with key version > 1', () => {
      mockDecryptForChannelWithVersion.mockResolvedValue('decrypted v2 content');

      const { handler } = setupHandler('dm_message');

      act(() => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-enc-2',
            conversation_id: 'conv-enc',
            user_id: 'user-2',
            username: 'alice',
            content: 'encrypted-v2-ciphertext',
            key_version: 2,
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      expect(mockDecryptForChannelWithVersion).toHaveBeenCalledWith(
        'conv-enc',
        'encrypted-v2-ciphertext',
        2
      );
    });

    it('handles DM message with attachments in last message preview', () => {
      const { handler } = setupHandler('dm_message');
      // Ensure the conversation exists so bumpConversation isn't a no-op
      useDMStore.getState().addConversation({
        id: 'conv-1',
        isGroup: false,
        isPersonal: false,
        name: null,
        participants: [],
        lastMessage: null,
        unreadCount: 0,
        createdAt: '2025-01-01T00:00:00Z',
      });
      const updateSpy = vi.spyOn(useDMStore.getState(), 'bumpConversation');

      act(() => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-attach-1',
            conversation_id: 'conv-1',
            user_id: 'user-2',
            username: 'alice',
            content: '',
            attachments: [{ file_type: 'image/png', url: 'https://example.com/img.png' }],
            created_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      expect(updateSpy).toHaveBeenCalledWith(
        'conv-1',
        expect.objectContaining({ attachmentType: 'image/png' })
      );
    });
  });

  describe('shouldSuppressLinkedTextNotification branch', () => {
    it('suppresses unread for voice-linked text channel when not in that voice channel', () => {
      // Set up a channel with a linked voice channel
      useChannelStore.getState().addChannel({
        id: 'ch-linked',
        server_id: 'server-1',
        name: 'linked-text',
        type: 'text',
        position: 1,
        linked_voice_channel_id: 'ch-voice-linked',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      // User is NOT in the linked voice channel
      useVoiceStore.getState().setActiveChannel('ch-other-voice');
      useServerStore.getState().setActiveServer('server-1');
      useChannelStore.setState({ activeChannelId: 'ch-1' });

      const { handler } = setupHandler('unread_notify');

      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-1', channel_id: 'ch-linked' },
        });
      });

      // Should be suppressed — no sound played
      expect(mockNotificationPlay).not.toHaveBeenCalled();
    });
  });

  // ── Desktop notification wiring (#175) ────────────────────────────

  describe('desktop notifications for channel messages', () => {
    it('fires desktop notification when shouldNotify returns true', () => {
      mockShouldNotify.mockReturnValue(true);

      const { handler } = setupHandler('message');
      const activeServerId = useServerStore.getState().activeServerId;

      act(() => {
        handler({
          type: 'message',
          data: {
            id: 'msg-notif-1',
            channel_id: mockChannel.id,
            server_id: activeServerId || 'server-1',
            user_id: 'other-user',
            username: 'otheruser',
            content: 'hey there!',
            created_at: new Date().toISOString(),
          },
        });
      });

      expect(mockShouldNotify).toHaveBeenCalledWith(expect.objectContaining({ type: 'message' }));
      expect(mockNotify).toHaveBeenCalledWith(
        expect.objectContaining({
          targetType: 'channel',
          targetId: mockChannel.id,
          body: 'hey there!',
        })
      );
      expect(mockIncrementBadge).toHaveBeenCalled();

      mockShouldNotify.mockReturnValue(false);
    });

    it('does not fire desktop notification when shouldNotify returns false', () => {
      mockShouldNotify.mockReturnValue(false);

      const { handler } = setupHandler('message');

      act(() => {
        handler({
          type: 'message',
          data: {
            id: 'msg-notif-2',
            channel_id: mockChannel.id,
            server_id: 'server-1',
            user_id: 'other-user',
            username: 'otheruser',
            content: 'silent',
            created_at: new Date().toISOString(),
          },
        });
      });

      expect(mockNotify).not.toHaveBeenCalled();
      expect(mockIncrementBadge).not.toHaveBeenCalled();
    });

    it('passes mention type when data.mentioned is true', () => {
      mockShouldNotify.mockReturnValue(true);

      const { handler } = setupHandler('message');

      act(() => {
        handler({
          type: 'message',
          data: {
            id: 'msg-notif-3',
            channel_id: mockChannel.id,
            server_id: 'server-1',
            user_id: 'other-user',
            username: 'mentioner',
            content: '@testuser hey',
            mentioned: true,
            created_at: new Date().toISOString(),
          },
        });
      });

      expect(mockShouldNotify).toHaveBeenCalledWith(expect.objectContaining({ type: 'mention' }));

      mockShouldNotify.mockReturnValue(false);
    });
  });

  describe('desktop notifications for DM messages', () => {
    it('fires desktop notification for DM when shouldNotify returns true', () => {
      mockShouldNotify.mockReturnValue(true);

      // Set up a DM conversation
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
            createdAt: new Date().toISOString(),
          },
        ],
        activeConversationId: null,
      });

      const { handler } = setupHandler('dm_message');

      act(() => {
        handler({
          type: 'dm_message',
          data: {
            id: 'dm-msg-1',
            conversation_id: 'dm-conv-1',
            user_id: 'other-user',
            username: 'alice',
            display_name: 'Alice',
            content: 'hello from DM',
            created_at: new Date().toISOString(),
          },
        });
      });

      expect(mockShouldNotify).toHaveBeenCalledWith(expect.objectContaining({ type: 'dm' }));
      expect(mockNotify).toHaveBeenCalledWith(
        expect.objectContaining({
          targetType: 'dm',
          targetId: 'dm-conv-1',
          title: 'DM from Alice',
        })
      );
      expect(mockIncrementBadge).toHaveBeenCalled();

      mockShouldNotify.mockReturnValue(false);
    });
  });

  // ── Enforcement coverage gaps ─────────────────────────────────────────

  describe('voice_state_update — joined action defaults', () => {
    it('sets serverMuted: false and serverDeafened: false on addChannelVoiceMember', () => {
      const { handler } = setupHandler('voice_state_update');
      const addSpy = vi.spyOn(useVoiceStore.getState(), 'addChannelVoiceMember');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
            display_name: 'Alice',
            server_id: 'server-1',
          },
        });
      });

      expect(addSpy).toHaveBeenCalledWith('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        displayName: 'Alice',
        isMuted: false,
        isDeafened: false,
        serverMuted: false,
        serverDeafened: false,
      });
    });

    it('increments server voice count on joined', () => {
      const { handler } = setupHandler('voice_state_update');
      const incSpy = vi.spyOn(useVoiceStore.getState(), 'incrementServerVoiceCount');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
            server_id: 'server-1',
          },
        });
      });

      expect(incSpy).toHaveBeenCalledWith('server-1');
    });

    it('decrements server voice count on left', () => {
      const { handler } = setupHandler('voice_state_update');
      const decSpy = vi.spyOn(useVoiceStore.getState(), 'decrementServerVoiceCount');

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'left',
            user_id: 'user-2',
            server_id: 'server-1',
          },
        });
      });

      expect(decSpy).toHaveBeenCalledWith('server-1');
    });

    it('removes voice member from channel on left', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-voice',
            action: 'left',
            user_id: 'user-2',
            server_id: 'server-1',
          },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-voice'];
      expect(members?.find((m) => m.userId === 'user-2')).toBeUndefined();
    });
  });

  describe('voice_state_update — room_empty action', () => {
    it('clears all voice members for the channel', () => {
      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });
      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-3',
        username: 'bob',
        isMuted: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'room_empty' },
        });
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-voice'];
      expect(members).toEqual([]);
    });
  });

  describe('voice_state_update — enforcement without userId', () => {
    it('does not crash when enforcement action has no userId', () => {
      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_muted' },
        });
      });

      // No crash — the `&& userId` guard prevents sidebar/toast updates
    });

    it('does not update sidebar members when userId is undefined', () => {
      const { handler } = setupHandler('voice_state_update');
      const updateSpy = vi.spyOn(useVoiceStore.getState(), 'updateChannelVoiceMember');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'server_deafened' },
        });
      });

      expect(updateSpy).not.toHaveBeenCalled();
    });
  });

  describe('voice_state_update — non-enforcement actions skip enforcement block', () => {
    it('does not call updateChannelVoiceMember for muted action', () => {
      const { handler } = setupHandler('voice_state_update');
      const updateSpy = vi.spyOn(useVoiceStore.getState(), 'updateChannelVoiceMember');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'muted', user_id: 'user-2' },
        });
      });

      // muted is NOT an enforcement action, so updateChannelVoiceMember should NOT be called
      expect(updateSpy).not.toHaveBeenCalled();
    });

    it('does not call updateChannelVoiceMember for video_on action', () => {
      const { handler } = setupHandler('voice_state_update');
      const updateSpy = vi.spyOn(useVoiceStore.getState(), 'updateChannelVoiceMember');

      useVoiceStore.getState().setActiveChannel('ch-voice');
      useVoiceStore.getState().addChannelVoiceMember('ch-voice', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-voice', action: 'video_on', user_id: 'user-2' },
        });
      });

      expect(updateSpy).not.toHaveBeenCalled();
    });
  });

  describe('voice_state_update — scheduleVoiceRefetch', () => {
    it('calls apiFetch after debounce on joined action', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');

      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-refetch',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
          },
        });
      });

      // Before debounce fires
      expect(mockApiFetch).not.toHaveBeenCalledWith(
        '/api/v1/channels/ch-refetch/voice/participants'
      );

      // Advance past the 2000ms debounce
      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/channels/ch-refetch/voice/participants');

      vi.useRealTimers();
    });

    it('calls apiFetch after debounce on left action', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');

      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().addChannelVoiceMember('ch-refetch', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-refetch',
            action: 'left',
            user_id: 'user-2',
          },
        });
      });

      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/channels/ch-refetch/voice/participants');

      vi.useRealTimers();
    });

    it('maps server_muted and server_deafened fields from API response', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');
      (mockApiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            participants: [
              {
                user_id: 'user-2',
                username: 'alice',
                display_name: 'Alice',
                avatar_url: 'https://example.com/a.png',
                is_muted: true,
                server_muted: true,
                server_deafened: false,
              },
            ],
          }),
      });

      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-refetch-map',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
          },
        });
      });

      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      // Wait for the async response to be processed
      await act(async () => {
        await Promise.resolve();
      });

      const members = useVoiceStore.getState().channelVoiceMembers['ch-refetch-map'];
      expect(members).toBeDefined();
      if (members && members.length > 0) {
        expect(members[0]).toEqual(
          expect.objectContaining({
            userId: 'user-2',
            username: 'alice',
            displayName: 'Alice',
            avatarUrl: 'https://example.com/a.png',
            isMuted: true,
            serverMuted: true,
            serverDeafened: false,
          })
        );
      }

      vi.useRealTimers();
    });

    it('coalesces rapid bursts into a single API call', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');
      (mockApiFetch as ReturnType<typeof vi.fn>).mockClear();

      const { handler } = setupHandler('voice_state_update');

      // Simulate 3 rapid joins
      for (const uid of ['user-3', 'user-4', 'user-5']) {
        act(() => {
          handler({
            type: 'voice_state_update',
            data: {
              channel_id: 'ch-burst',
              action: 'joined',
              user_id: uid,
              username: uid,
            },
          });
        });
      }

      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      // Only one API call despite 3 events
      const calls = (mockApiFetch as ReturnType<typeof vi.fn>).mock.calls.filter(
        (c: string[]) => c[0] === '/api/v1/channels/ch-burst/voice/participants'
      );
      expect(calls).toHaveLength(1);

      vi.useRealTimers();
    });

    it('handles API fetch failure gracefully', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');
      (mockApiFetch as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('Network error'));

      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-fail',
            action: 'joined',
            user_id: 'user-2',
            username: 'alice',
          },
        });
      });

      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      // Should not crash — error is swallowed
      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/channels/ch-fail/voice/participants');

      vi.useRealTimers();
    });

    it('handles non-ok API response gracefully', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');
      (mockApiFetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
        ok: false,
        status: 500,
      });

      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: {
            channel_id: 'ch-500',
            action: 'left',
            user_id: 'user-2',
          },
        });
      });

      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      // Should not crash — early return on !res.ok
      const members = useVoiceStore.getState().channelVoiceMembers['ch-500'];
      // Members should not be overwritten on failure
      expect(members).toBeUndefined();

      vi.useRealTimers();
    });

    it('does not schedule refetch for non-joined/left actions', async () => {
      vi.useFakeTimers();
      const { apiFetch: mockApiFetch } = await import('@/renderer/services/apiClient');
      (mockApiFetch as ReturnType<typeof vi.fn>).mockClear();

      const { handler } = setupHandler('voice_state_update');

      useVoiceStore.getState().setActiveChannel('ch-no-refetch');
      useVoiceStore.getState().addChannelVoiceMember('ch-no-refetch', {
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-no-refetch', action: 'server_muted', user_id: 'user-2' },
        });
      });

      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      const calls = (mockApiFetch as ReturnType<typeof vi.fn>).mock.calls.filter(
        (c: string[]) => c[0] === '/api/v1/channels/ch-no-refetch/voice/participants'
      );
      expect(calls).toHaveLength(0);

      vi.useRealTimers();
    });
  });

  describe('dm_voice_state_update — enforcement actions', () => {
    it('updates participant on server_muted in DM call', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_muted',
            user_id: 'user-2',
          },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].serverMuted).toBe(true);
    });

    it('updates participant on server_unmuted in DM call', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
        serverMuted: true,
      });

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_unmuted',
            user_id: 'user-2',
          },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].serverMuted).toBe(false);
    });

    it('updates participant on server_deafened in DM call (sets both flags)', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_deafened',
            user_id: 'user-2',
          },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].serverDeafened).toBe(true);
      expect(useVoiceStore.getState().participants['user-2'].serverMuted).toBe(true);
    });

    it('updates participant on server_undeafened in DM call (clears both flags)', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
        serverMuted: true,
        serverDeafened: true,
      });

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_undeafened',
            user_id: 'user-2',
          },
        });
      });

      expect(useVoiceStore.getState().participants['user-2'].serverDeafened).toBe(false);
      expect(useVoiceStore.getState().participants['user-2'].serverMuted).toBe(false);
    });

    it('shows toast log when local user is enforcement-targeted in DM call', () => {
      const { handler } = setupHandler('dm_voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_muted',
            user_id: 'user-1',
          },
        });
      });

      expect(debugSpy).toHaveBeenCalledWith('[Voice]', 'You have been server-muted by a moderator');
      debugSpy.mockRestore();
    });

    it('shows toast for server_deafened on local user in DM call', () => {
      const { handler } = setupHandler('dm_voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_deafened',
            user_id: 'user-1',
          },
        });
      });

      expect(debugSpy).toHaveBeenCalledWith(
        '[Voice]',
        'You have been server-deafened by a moderator'
      );
      debugSpy.mockRestore();
    });

    it('does not show toast for non-local user enforcement in DM call', () => {
      const { handler } = setupHandler('dm_voice_state_update');
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_muted',
            user_id: 'user-2',
          },
        });
      });

      expect(debugSpy).not.toHaveBeenCalledWith(
        '[Voice]',
        expect.stringContaining('You have been')
      );
      debugSpy.mockRestore();
    });

    it('does not crash when enforcement action has no userId in DM', () => {
      const { handler } = setupHandler('dm_voice_state_update');

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'server_muted',
          },
        });
      });

      // No crash — the `&& userId` guard prevents updates
    });

    it('ignores non-enforcement actions in DM voice handler', () => {
      const { handler } = setupHandler('dm_voice_state_update');
      const updateSpy = vi.spyOn(useVoiceStore.getState(), 'updateParticipant');

      act(() => {
        handler({
          type: 'dm_voice_state_update',
          data: {
            conversation_id: 'conv-1',
            action: 'muted',
            user_id: 'user-2',
          },
        });
      });

      // 'muted' is not in the enforcement list, so updateParticipant is not called
      // (the DM handler only processes enforcement actions)
      expect(updateSpy).not.toHaveBeenCalled();
    });
  });

  describe('voice_state_update — joined without channel_id guard', () => {
    it('ignores voice_state_update without channel_id', () => {
      const { handler } = setupHandler('voice_state_update');

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { action: 'joined', user_id: 'user-2', username: 'alice' },
        });
      });

      // No crash — early return when channelId is missing
    });
  });

  describe('voice_state_update — participant update skipped for non-active channel', () => {
    it('does not call updateParticipant when action is for a different channel', () => {
      const { handler } = setupHandler('voice_state_update');
      const updateSpy = vi.spyOn(useVoiceStore.getState(), 'updateParticipant');

      useVoiceStore.getState().setActiveChannel('ch-active');
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'alice',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      act(() => {
        handler({
          type: 'voice_state_update',
          data: { channel_id: 'ch-OTHER', action: 'muted', user_id: 'user-2' },
        });
      });

      expect(updateSpy).not.toHaveBeenCalled();
    });
  });
});
