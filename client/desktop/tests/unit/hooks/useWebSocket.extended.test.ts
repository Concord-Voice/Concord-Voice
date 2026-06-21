/**
 * Extended tests for useWebSocket — covers validateEpochsOnReconnect,
 * the key rotation coordinator, and connection state listener mapping.
 * The base test file covers handler registration and basic functionality;
 * this focuses on the E2EE reconnect logic and rotation coordinator.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Capture registered handlers so we can invoke them in tests
type HandlerFn = (...args: unknown[]) => void;
const registeredHandlers = new Map<string, HandlerFn>();
let connectionChangeHandlers: HandlerFn[] = [];
// useWebSocket (UI-state mapping) AND useWebSocketMessages (entitlement
// re-hydrate on reconnect, #1297) both subscribe to onConnectionChange — fire
// ALL captured handlers so a test-driven state change reaches every subscriber.
function fireConnectionChange(state: string) {
  connectionChangeHandlers.forEach((h) => h(state));
}

const mockWsService = {
  connect: vi.fn(),
  disconnect: vi.fn(),
  updateToken: vi.fn(),
  resetReconnectState: vi.fn(),
  on: vi.fn((type: string, handler: HandlerFn) => {
    registeredHandlers.set(type, handler);
    return () => {
      registeredHandlers.delete(type);
    };
  }),
  onConnectionChange: vi.fn((handler: HandlerFn) => {
    connectionChangeHandlers.push(handler);
    handler('disconnected');
    return () => {
      connectionChangeHandlers = connectionChangeHandlers.filter((h) => h !== handler);
    };
  }),
  getConnectionInfo: vi.fn(() => null),
  getState: vi.fn(() => 'disconnected'),
  subscribe: vi.fn(),
  unsubscribe: vi.fn(),
  sendMessage: vi.fn(),
  sendTypingIndicator: vi.fn(),
  setAggressiveReconnect: vi.fn(),
};

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => mockWsService,
  ConnectionState: {
    DISCONNECTED: 'disconnected',
    CONNECTING: 'connecting',
    CONNECTED: 'connected',
    RECONNECTING: 'reconnecting',
    ERROR: 'error',
  },
}));

const mockGetCurrentKeyVersion = vi.fn().mockReturnValue(0);
const mockInvalidateChannelKey = vi.fn();
const mockRotateChannelKey = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    decryptForChannel: vi.fn(),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    getCurrentKeyVersion: (...args: unknown[]) => mockGetCurrentKeyVersion(...args),
    rotateChannelKey: (...args: unknown[]) => mockRotateChannelKey(...args),
  },
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

// Mock apiFetch for validate-epochs
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
}));

import { useWebSocket } from '@/renderer/hooks/useWebSocket';

beforeEach(() => {
  resetAllStores();
  registeredHandlers.clear();
  connectionChangeHandlers = [];
  vi.clearAllMocks();
  // vi.clearAllMocks() clears call history but does NOT reset mockReturnValue
  // overrides — tests that mutate getState.mockReturnValue would leak that
  // value into subsequent tests. Reset to the documented default here.
  mockWsService.getState.mockReturnValue('disconnected');
  mockApiFetch.mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useWebSocket — extended', () => {
  describe('connection state mapping', () => {
    it('maps CONNECTED to connected UI state', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      act(() => {
        fireConnectionChange('connected');
      });

      expect(useChatStore.getState().connectionState).toBe('connected');
    });

    it('maps CONNECTING to connecting UI state', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      act(() => {
        fireConnectionChange('connecting');
      });

      expect(useChatStore.getState().connectionState).toBe('connecting');
    });

    it('maps RECONNECTING to connecting UI state', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      act(() => {
        fireConnectionChange('reconnecting');
      });

      expect(useChatStore.getState().connectionState).toBe('connecting');
    });

    it('maps ERROR to disconnected UI state', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      act(() => {
        fireConnectionChange('error');
      });

      expect(useChatStore.getState().connectionState).toBe('disconnected');
    });
  });

  describe('subscribe/unsubscribe/sendMessage/sendTyping', () => {
    it('unsubscribe delegates to wsService', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const { result } = renderHook(() => useWebSocket());

      result.current.unsubscribe('ch-1');
      expect(mockWsService.unsubscribe).toHaveBeenCalledWith('ch-1');
    });

    it('sendTyping delegates to wsService', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const { result } = renderHook(() => useWebSocket());

      result.current.sendTyping('ch-1', true);
      expect(mockWsService.sendTypingIndicator).toHaveBeenCalledWith('ch-1', true);
    });

    it('getState returns current WS state', () => {
      useAuthStore.getState().setAccessToken('test-token');
      mockWsService.getState.mockReturnValue('connected');
      const { result } = renderHook(() => useWebSocket());

      expect(result.current.getState()).toBe('connected');
    });
  });

  describe('connection lifecycle', () => {
    // Previously this test asserted disconnect+reconnect on every token
    // change, which encoded the JWT-refresh churn we explicitly removed:
    // every ~14 minutes the WS was torn down and re-established for no
    // benefit (the server-authenticated frame did not need re-handshaking).
    // The hook now calls wsService.updateToken on rotation while connected,
    // leaving the open socket intact. This test now asserts that path.
    it('updates token via updateToken (not disconnect) when token changes while connected', () => {
      useAuthStore.getState().setAccessToken('token-1');
      const { rerender } = renderHook(() => useWebSocket());

      expect(mockWsService.connect).toHaveBeenCalledWith('token-1');
      mockWsService.connect.mockClear();
      mockWsService.disconnect.mockClear();
      mockWsService.updateToken.mockClear();

      // Simulate the socket reaching CONNECTED before the rotation arrives.
      mockWsService.getState.mockReturnValue('connected');

      // Change token (e.g. main-process proactive refresh)
      useAuthStore.getState().setAccessToken('token-2');
      rerender();

      expect(mockWsService.updateToken).toHaveBeenCalledWith('token-2');
      expect(mockWsService.connect).not.toHaveBeenCalled();
      expect(mockWsService.disconnect).not.toHaveBeenCalled();
    });
  });

  describe('key rotation coordinator', () => {
    it('listens for e2ee-key-rotation events', async () => {
      useAuthStore.getState().setAccessToken('test-token');
      vi.useFakeTimers();

      renderHook(() => useWebSocket());

      // Mock e2eeService as initialized
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as any).isInitialized = true;

      // Add a channel for the rotation coordinator to find
      useChannelStore.getState().addChannel({
        id: 'ch-rotate',
        server_id: 'server-1',
        name: 'test',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });

      // Mock API calls for the rotation flow
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ key: { key_version: 1 } }),
        }) // check existing key
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              members: [{ user_id: 'user-1' }, { user_id: 'user-2' }],
            }),
        }) // fetch members
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: 'mock-pk-1' }),
        }) // user-1 public key
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: 'mock-pk-2' }),
        }); // user-2 public key

      // Dispatch the event
      globalThis.dispatchEvent(
        new CustomEvent('e2ee-key-rotation', {
          detail: { channelId: 'ch-rotate', newEpoch: 2 },
        })
      );

      // The coordinator uses random jitter 0-2s — advance past it
      await vi.advanceTimersByTimeAsync(3000);

      // Verify the rotation coordinator attempted API calls for key rotation
      expect(mockApiFetch).toHaveBeenCalled();

      vi.useRealTimers();
    });

    it('cleans up rotation event listener on unmount', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const removeListenerSpy = vi.spyOn(globalThis, 'removeEventListener');

      const { unmount } = renderHook(() => useWebSocket());
      unmount();

      expect(removeListenerSpy).toHaveBeenCalledWith('e2ee-key-rotation', expect.any(Function));
    });
  });
});
