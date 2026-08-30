import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { ConnectionState } from '@/renderer/services/messaging/websocketService';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

// Mock side-effecting services so the hook mounts cleanly (mirrors the
// harness in useWebSocketMessages.test.ts).
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

vi.mock('@/renderer/services/system/ttsService', () => ({
  speak: vi.fn(),
}));

vi.mock('@/renderer/services/system/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));

vi.mock('@/renderer/services/system/presenceOverrideSync', () => ({
  presenceOverrideSyncService: { handleRemoteUpdate: vi.fn() },
}));

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
}));

vi.mock('@/renderer/services/system/notificationSoundService', () => ({
  notificationSoundService: {
    play: vi.fn(),
    playLoop: vi.fn(),
    stopLoop: vi.fn(),
    stopAllLoops: vi.fn(),
    isLooping: vi.fn().mockReturnValue(false),
    init: vi.fn(),
  },
}));

import { useWebSocketMessages } from '@/renderer/hooks/messaging/useWebSocketMessages';
import { presenceOverrideSyncService } from '@/renderer/services/system/presenceOverrideSync';
import { createMockWsService } from '../../helpers/wsServiceMock';

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  vi.mocked(presenceOverrideSyncService.handleRemoteUpdate)
    .mockReset()
    .mockResolvedValue(undefined);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useWebSocketMessages — rich presence (#1233)', () => {
  it('registers rich_presence_update and rich_presence_clear handlers', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('rich_presence_update')).toBeDefined();
    expect(ws.handlers.get('rich_presence_clear')).toBeDefined();
    expect(ws.handlers.get('presence_overrides_updated')).toBeDefined();
  });

  it('rich_presence_update populates the store keyed by user_id', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    const handler = ws.handlers.get('rich_presence_update');
    expect(handler).toBeDefined();

    if (handler) {
      act(() => {
        handler({
          type: 'rich_presence_update',
          data: {
            user_id: 'user-2',
            category: 'custom_text',
            payload: { emoji: '🎮', text: 'gaming' },
            updated_at: 1_700_000_000,
          },
        });
      });

      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🎮',
        text: 'gaming',
      });
    }
  });

  it('rich_presence_clear removes the stored entry', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    // Seed an entry first.
    useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🎮', text: 'gaming' });

    const handler = ws.handlers.get('rich_presence_clear');
    expect(handler).toBeDefined();

    if (handler) {
      act(() => {
        handler({
          type: 'rich_presence_clear',
          data: {
            user_id: 'user-2',
            category: 'custom_text',
          },
        });
      });

      expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
    }
  });

  it('voice-category updates and clears never mutate Custom Status', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    useRichPresenceStore.getState().setCustomText('user-2', { text: 'keep me' });

    act(() => {
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'user-2',
          category: 'server_voice',
          payload: {
            channel_id: '11111111-1111-4111-8111-111111111111',
            server_id: '22222222-2222-4222-8222-222222222222',
          },
          updated_at: 1_700_000_000,
        },
      });
      ws.handlers.get('rich_presence_clear')?.({
        type: 'rich_presence_clear',
        data: { user_id: 'user-2', category: 'private_call' },
      });
    });

    expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({ text: 'keep me' });
  });

  it('treats a fresh presence snapshot as the boundary for other users custom text', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    useRichPresenceStore.getState().setCustomText('user-2', { text: 'previously visible' });
    useRichPresenceStore
      .getState()
      .setSelfPresence({ tier: 2, customText: 'my status', customTextEmoji: '🔒' });

    act(() => {
      ws.handlers.get('presence_snapshot')?.({
        type: 'presence_snapshot',
        data: { users: [{ user_id: 'user-2', status: 'online' }] },
      });
    });

    const state = useRichPresenceStore.getState() as unknown as {
      otherByUser?: unknown;
      customTextByUser?: unknown;
    };
    expect(state.otherByUser).toEqual({});
    expect(state.customTextByUser).toBeUndefined();
    expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
    expect(useRichPresenceStore.getState().self).toEqual({
      tier: 2,
      customText: 'my status',
      customTextEmoji: '🔒',
    });
  });

  it('forwards presence override versions to the dedicated sync service', async () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = ws.handlers.get('presence_overrides_updated');
    expect(handler).toBeDefined();

    await act(async () => {
      handler?.({
        type: 'presence_overrides_updated',
        data: { category: 'custom_text', version: 7 },
      });
      await Promise.resolve();
    });

    expect(presenceOverrideSyncService.handleRemoteUpdate).toHaveBeenCalledOnce();
    expect(presenceOverrideSyncService.handleRemoteUpdate).toHaveBeenCalledWith(7);
  });

  it('unregisters the presence override handler on unmount', () => {
    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('presence_overrides_updated')).toBeDefined();

    unmount();

    expect(ws.handlers.get('presence_overrides_updated')).toBeUndefined();
  });

  it('handles async refetch rejection without logging sensitive details', async () => {
    const sentinel = 'sentinel-private-ciphertext-33333333-3333-4333-8333-333333333333';
    vi.mocked(presenceOverrideSyncService.handleRemoteUpdate).mockRejectedValueOnce(
      new Error(sentinel)
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    try {
      await act(async () => {
        ws.handlers.get('presence_overrides_updated')?.({
          type: 'presence_overrides_updated',
          data: { category: 'custom_text', version: 8 },
        });
        await Promise.resolve();
        await Promise.resolve();
      });

      expect(warn).toHaveBeenCalledWith('[PresenceOverrideSync] remote-update refetch failed');
      expect(JSON.stringify(warn.mock.calls)).not.toContain(sentinel);
    } finally {
      warn.mockRestore();
    }
  });
});

describe('useWebSocketMessages — category-aware rich presence (#2233)', () => {
  const voice = {
    channel_id: '11111111-1111-4111-8111-111111111111',
    server_id: '22222222-2222-4222-8222-222222222222',
  };

  it('routes Server Voice and Private Call updates independently and ignores self', () => {
    useUserStore.setState({ user: { id: 'self-user', username: 'self' } } as never);
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => {
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'other-user',
          category: 'server_voice',
          minimized: true,
          payload: voice,
          updated_at: 1,
        },
      });
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'other-user',
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 2,
        },
      });
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'self-user',
          category: 'server_voice',
          minimized: true,
          payload: voice,
          updated_at: 3,
        },
      });
    });
    const state = useRichPresenceStore.getState() as unknown as {
      otherByUser?: Record<string, unknown>;
    };
    expect(state.otherByUser).toEqual({
      'other-user': {
        server_voice: { category: 'server_voice', minimized: true, payload: voice, updated_at: 1 },
        private_call: {
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 2,
        },
      },
    });
    expect(state.otherByUser?.['self-user']).toBeUndefined();
  });

  it('clears only the requested category and snapshots delete absent users', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => {
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'other-user',
          category: 'server_voice',
          minimized: true,
          payload: voice,
          updated_at: 1,
        },
      });
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'other-user',
          category: 'custom_text',
          payload: { text: 'keep' },
          updated_at: 2,
        },
      });
      ws.handlers.get('rich_presence_clear')?.({
        type: 'rich_presence_clear',
        data: { user_id: 'other-user', category: 'server_voice' },
      });
    });
    const state = useRichPresenceStore.getState() as unknown as {
      otherByUser?: Record<string, unknown>;
      customTextByUser?: unknown;
    };
    expect(state.otherByUser).toMatchObject({ 'other-user': { custom_text: expect.anything() } });
    expect(state.customTextByUser).toBeUndefined();
    expect(useRichPresenceStore.getState().getCustomText('other-user')).toEqual({ text: 'keep' });
  });

  it('replaces activity once, deletes omitted categories, then replays Custom Status separately', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const replace = vi.spyOn(useRichPresenceStore.getState(), 'replaceOtherPresence');
    act(() =>
      ws.handlers.get('presence_snapshot')?.({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              user_id: 'other-user',
              status: 'online',
              rich_presence: {
                server_voice: { minimized: true, payload: voice, updated_at: 4 },
                private_call: { minimized: true, payload: { call_type: 'dm' }, updated_at: 5 },
              },
            },
          ],
        },
      })
    );
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({
      'other-user': {
        server_voice: { category: 'server_voice', minimized: true, payload: voice, updated_at: 4 },
        private_call: {
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 5,
        },
      },
    });
    expect(replace).toHaveBeenCalledTimes(1);
    act(() =>
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'stale-user',
          category: 'server_voice',
          minimized: true,
          payload: voice,
          updated_at: 5,
        },
      })
    );
    act(() =>
      ws.handlers.get('presence_snapshot')?.({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              user_id: 'other-user',
              status: 'online',
              rich_presence: { server_voice: { minimized: true, payload: voice, updated_at: 6 } },
            },
          ],
        },
      })
    );
    expect(replace).toHaveBeenCalledTimes(2);
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({
      'other-user': {
        server_voice: { category: 'server_voice', minimized: true, payload: voice, updated_at: 6 },
      },
    });
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser: Record<string, unknown> })
        .otherByUser['stale-user']
    ).toBeUndefined();
    act(() =>
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'other-user',
          category: 'custom_text',
          payload: { text: 'replayed' },
          updated_at: 7,
        },
      })
    );
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toMatchObject({
      'other-user': {
        server_voice: expect.anything(),
        custom_text: { category: 'custom_text', payload: { text: 'replayed' }, updated_at: 7 },
      },
    });
    expect(
      (
        useRichPresenceStore.getState() as unknown as {
          otherByUser: Record<string, Record<string, unknown>>;
        }
      ).otherByUser['other-user'].private_call
    ).toBeUndefined();
  });

  it('ignores self snapshot entries and clears legacy snapshots while preserving self', () => {
    useUserStore.setState({ user: { id: 'self-user', username: 'self' } } as never);
    useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'mine' });
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() =>
      ws.handlers.get('presence_snapshot')?.({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              user_id: 'self-user',
              status: 'online',
              rich_presence: { server_voice: { minimized: true, payload: voice, updated_at: 4 } },
            },
          ],
        },
      })
    );
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({});
    expect(useRichPresenceStore.getState().self.customText).toBe('mine');
    act(() => ws.handlers.get('presence_snapshot')?.({ type: 'presence_snapshot', data: {} }));
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({});
  });

  it('uses the handshake identity before user-store hydration', () => {
    const ws = createMockWsService();
    ws.getConnectionInfo.mockReturnValue({ clientId: 'client-1', userId: 'self-user' });
    const clearOtherPresence = vi.spyOn(useRichPresenceStore.getState(), 'clearOtherPresence');
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      ws.handlers.get('presence_snapshot')?.({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              user_id: 'self-user',
              status: 'online',
              rich_presence: {
                server_voice: { minimized: true, payload: voice, updated_at: 1 },
              },
            },
            {
              user_id: 'other-user',
              status: 'online',
              rich_presence: {
                server_voice: { minimized: true, payload: voice, updated_at: 2 },
              },
            },
          ],
        },
      });
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'self-user',
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 3,
        },
      });
      ws.handlers.get('rich_presence_update')?.({
        type: 'rich_presence_update',
        data: {
          user_id: 'other-user',
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 4,
        },
      });
      ws.handlers.get('rich_presence_clear')?.({
        type: 'rich_presence_clear',
        data: { user_id: 'self-user', category: 'server_voice' },
      });
    });

    expect(clearOtherPresence).not.toHaveBeenCalled();
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({
      'other-user': {
        server_voice: { category: 'server_voice', minimized: true, payload: voice, updated_at: 2 },
        private_call: {
          category: 'private_call',
          minimized: true,
          payload: { call_type: 'dm' },
          updated_at: 4,
        },
      },
    });
  });

  it('connection freshness clears remote activity synchronously while self survives', () => {
    useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'mine' });
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    for (const state of [
      ConnectionState.DISCONNECTED,
      ConnectionState.RECONNECTING,
      ConnectionState.CONNECTED,
      ConnectionState.ERROR,
    ]) {
      useRichPresenceStore.getState().setCustomText('other-user', { text: 'stale' });
      act(() => ws.emitConnectionChange(state));
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
      expect(useRichPresenceStore.getState().self.customText).toBe('mine');
    }
  });
});
