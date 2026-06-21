import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

// Mock side-effecting services so the hook mounts cleanly (mirrors the
// harness in useWebSocketMessages.test.ts).
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({
  speak: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
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

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
});

describe('useWebSocketMessages — rich presence (#1233)', () => {
  it('registers rich_presence_update and rich_presence_clear handlers', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('rich_presence_update')).toBeDefined();
    expect(ws.handlers.get('rich_presence_clear')).toBeDefined();
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
});
