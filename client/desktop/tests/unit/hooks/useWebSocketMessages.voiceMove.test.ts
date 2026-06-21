/**
 * useWebSocketMessages.voiceMove.test.ts — #487 Scope B/P4 handler coverage.
 *
 * Covers the two directed voice WS handlers:
 *   - voice_move → voiceService.leaveChannel() then joinChannel(to_channel_id)
 *   - channel_access_revoked → removeChannel + clearMessages + invalidateChannelKey
 *     (+ leaveChannel only when currently in that voice channel)
 *
 * Per [internal]rules/tests.md: resetAllStores() in beforeEach; behavior-not-
 * implementation. We do NOT test schema-rejection in-handler bails — those are
 * covered at the dispatch boundary (websocketService.dispatch.test.ts).
 */

import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

// ── Mocks ────────────────────────────────────────────────────────────────

const mockLeaveChannel = vi.fn<() => Promise<void>>().mockResolvedValue(undefined);
const mockJoinChannel = vi.fn<(channelId: string) => Promise<void>>().mockResolvedValue(undefined);
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    leaveChannel: (...args: unknown[]) => mockLeaveChannel(...(args as [])),
    joinChannel: (channelId: string) => mockJoinChannel(channelId),
  },
}));

const mockInvalidateChannelKey = vi.fn();
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    isInitialized: false,
    processPendingKeyRequests: vi.fn(),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({ speak: vi.fn() }));
vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi
    .fn()
    .mockResolvedValue({ ok: true, json: () => Promise.resolve({ participants: [] }) }),
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

import { useWebSocketMessages } from '@/renderer/hooks/useWebSocketMessages';

// ── Mock wsService that captures handlers ──────────────────────────────────
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
  };
}

const UUID_FROM = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const UUID_TO = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const UUID_SERVER = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const UUID_USER = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
const REVOKE_CHANNEL = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';

beforeEach(() => {
  resetAllStores();
  mockLeaveChannel.mockClear().mockResolvedValue(undefined);
  mockJoinChannel.mockClear().mockResolvedValue(undefined);
  mockInvalidateChannelKey.mockClear();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
});

describe('useWebSocketMessages — voice_move handler (#487)', () => {
  it('registers a voice_move handler on mount', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('voice_move')).toBeDefined();
  });

  it('leaves the current channel then joins the target on voice_move', async () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('voice_move');
    expect(handler).toBeDefined();

    await act(async () => {
      handler?.({
        type: 'voice_move',
        data: {
          user_id: UUID_USER,
          from_channel_id: UUID_FROM,
          to_channel_id: UUID_TO,
          server_id: UUID_SERVER,
        },
      });
      // Allow the leave→join promise chain to settle.
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(mockLeaveChannel).toHaveBeenCalledTimes(1);
    expect(mockJoinChannel).toHaveBeenCalledTimes(1);
    expect(mockJoinChannel).toHaveBeenCalledWith(UUID_TO);
  });

  it('joins only after the leave resolves (sequence order)', async () => {
    const order: string[] = [];
    mockLeaveChannel.mockImplementation(async () => {
      order.push('leave');
    });
    mockJoinChannel.mockImplementation(async () => {
      order.push('join');
    });
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('voice_move');

    await act(async () => {
      handler?.({
        type: 'voice_move',
        data: {
          user_id: UUID_USER,
          from_channel_id: UUID_FROM,
          to_channel_id: UUID_TO,
          server_id: UUID_SERVER,
        },
      });
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(order).toEqual(['leave', 'join']);
  });

  it('does not throw when leaveChannel rejects (error is swallowed + logged)', async () => {
    mockLeaveChannel.mockRejectedValueOnce(new Error('boom'));
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('voice_move');

    await act(async () => {
      handler?.({
        type: 'voice_move',
        data: {
          user_id: UUID_USER,
          from_channel_id: UUID_FROM,
          to_channel_id: UUID_TO,
          server_id: UUID_SERVER,
        },
      });
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(mockJoinChannel).not.toHaveBeenCalled();
    expect(errSpy).toHaveBeenCalled();
    errSpy.mockRestore();
  });
});

describe('useWebSocketMessages — channel_access_revoked handler (#487 P4)', () => {
  it('registers a channel_access_revoked handler on mount', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('channel_access_revoked')).toBeDefined();
  });

  it('removes the channel, purges its messages, and invalidates the key', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('channel_access_revoked');

    // Seed a channel + a cached message for it.
    useChannelStore.getState().addChannel({ ...mockChannel, id: REVOKE_CHANNEL });
    useChatStore.getState().addMessage(REVOKE_CHANNEL, {
      id: 'msg-revoke-1',
      channel_id: REVOKE_CHANNEL,
      user_id: 'user-1',
      content: 'secret',
      username: 'u',
      status: 'delivered',
      created_at: '2025-01-01T12:00:00Z',
      updated_at: '2025-01-01T12:00:00Z',
    });
    expect(useChannelStore.getState().channels.some((c) => c.id === REVOKE_CHANNEL)).toBe(true);
    expect(useChatStore.getState().messagesByChannel.get(REVOKE_CHANNEL)).toBeDefined();

    act(() => {
      handler?.({
        type: 'channel_access_revoked',
        data: { channel_id: REVOKE_CHANNEL, server_id: UUID_SERVER, reason: 'temp_access_revoked' },
      });
    });

    expect(useChannelStore.getState().channels.some((c) => c.id === REVOKE_CHANNEL)).toBe(false);
    expect(useChatStore.getState().messagesByChannel.get(REVOKE_CHANNEL)).toBeUndefined();
    expect(mockInvalidateChannelKey).toHaveBeenCalledWith(REVOKE_CHANNEL);
  });

  it('leaves voice when currently in the revoked channel', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('channel_access_revoked');

    useVoiceStore.getState().setActiveChannel(REVOKE_CHANNEL, 'voice', UUID_SERVER);

    act(() => {
      handler?.({
        type: 'channel_access_revoked',
        data: { channel_id: REVOKE_CHANNEL, server_id: UUID_SERVER, reason: 'temp_access_revoked' },
      });
    });

    expect(mockLeaveChannel).toHaveBeenCalledTimes(1);
  });

  it('does NOT leave voice when in a different channel', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('channel_access_revoked');

    useVoiceStore.getState().setActiveChannel(UUID_TO, 'other-voice', UUID_SERVER);

    act(() => {
      handler?.({
        type: 'channel_access_revoked',
        data: { channel_id: REVOKE_CHANNEL, server_id: UUID_SERVER, reason: 'temp_access_revoked' },
      });
    });

    expect(mockLeaveChannel).not.toHaveBeenCalled();
  });
});
