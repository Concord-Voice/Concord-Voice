import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

// Mock side-effecting services so the hook mounts cleanly (mirrors the
// harness in useWebSocketMessages.test.ts).
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({
  speak: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: { handleRemoteUpdate: vi.fn() },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
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
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
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

    expect(useRichPresenceStore.getState().customTextByUser).toEqual({});
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
