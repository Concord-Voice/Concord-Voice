/**
 * Extended tests for useWebSocket — covers validateEpochsOnReconnect,
 * the key rotation coordinator, and connection state listener mapping.
 * The base test file covers handler registration and basic functionality;
 * this focuses on the E2EE reconnect logic and rotation coordinator.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';
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
let mockChannelGuardGeneration = 0;
let mockRotationSessionGeneration = 0;
let mockRotationAccessGeneration = 0;
const mockInvalidateChannelKey = vi.fn(() => {
  mockChannelGuardGeneration += 1;
});
const mockRevokeChannelAccess = vi.fn(() => {
  mockRotationAccessGeneration += 1;
});
const mockFencePendingOperations = vi.fn(() => {
  mockRotationSessionGeneration += 1;
});
const mockRotateChannelKey = vi.fn().mockResolvedValue(undefined);
const mockCreateChannelOperationGuard = vi.fn(() => {
  const generation = mockChannelGuardGeneration;
  return {
    assertCurrent: () => {
      if (mockChannelGuardGeneration !== generation) throw new Error('stale channel operation');
    },
  };
});
const mockCreateChannelRotationGuard = vi.fn(() => {
  const sessionGeneration = mockRotationSessionGeneration;
  const accessGeneration = mockRotationAccessGeneration;
  return {
    assertCurrent: () => {
      if (
        mockRotationSessionGeneration !== sessionGeneration ||
        mockRotationAccessGeneration !== accessGeneration
      ) {
        throw new Error('stale rotation operation');
      }
    },
  };
});
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    decryptForChannel: vi.fn(),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    revokeChannelAccess: vi.fn((...args: unknown[]) => mockRevokeChannelAccess(...args)),
    fencePendingOperations: vi.fn((...args: unknown[]) => mockFencePendingOperations(...args)),
    getCurrentKeyVersion: (...args: unknown[]) => mockGetCurrentKeyVersion(...args),
    createChannelOperationGuard: () => mockCreateChannelOperationGuard(),
    createChannelRotationGuard: () => mockCreateChannelRotationGuard(),
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

import { useWebSocket } from '@/renderer/hooks/messaging/useWebSocket';

interface EpochRevocation {
  channel_id: string;
  revoked_epoch: number;
  successor_epoch: number;
  reason?: string;
}

function seedEpochChannels(count: number) {
  const channels = Array.from({ length: count }, (_, index) => ({
    id: `00000000-0000-4000-8000-${String(index).padStart(12, '0')}`,
    server_id: 'server-1',
    name: `channel-${index}`,
    type: 'text' as const,
    position: index,
    created_at: '',
    updated_at: '',
  }));
  const epochs = new Map(channels.map((channel, index) => [channel.id, index + 1]));

  useChannelStore.setState({
    channels,
    channelIdsByServer: { 'server-1': channels.map((channel) => channel.id) },
  });
  mockGetCurrentKeyVersion.mockImplementation((channelId: string) => epochs.get(channelId) ?? 0);

  return channels;
}

function successfulEpochResponse(revocations: EpochRevocation[] = [], accessLost: string[] = []) {
  return {
    ok: true,
    json: () => Promise.resolve({ revocations, access_lost: accessLost }),
  };
}

function defaultApiResponse() {
  return {
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  };
}

function epochValidationCalls() {
  return mockApiFetch.mock.calls.filter(([path]) => path === '/api/v1/e2ee/validate-epochs');
}

function epochsFromRequest(index: number): Record<string, number> {
  const options = epochValidationCalls()[index]?.[1] as RequestInit | undefined;
  if (!options) throw new Error(`Missing validate-epochs request ${index}`);
  return (JSON.parse(String(options.body)) as { epochs: Record<string, number> }).epochs;
}

function triggerEpochValidation() {
  useConnectionStore.getState().startGracePeriod();
  act(() => {
    fireConnectionChange('connected');
  });
}

beforeEach(async () => {
  resetRuntimeServerBase();
  resetAllStores();
  mockChannelGuardGeneration = 0;
  mockRotationSessionGeneration = 0;
  mockRotationAccessGeneration = 0;
  registeredHandlers.clear();
  connectionChangeHandlers = [];
  vi.clearAllMocks();
  // vi.clearAllMocks() clears call history but does NOT reset mockReturnValue
  // overrides — tests that mutate getState.mockReturnValue would leak that
  // value into subsequent tests. Reset to the documented default here.
  mockWsService.getState.mockReturnValue('disconnected');
  mockGetCurrentKeyVersion.mockReset();
  mockGetCurrentKeyVersion.mockReturnValue(0);
  mockApiFetch.mockReset();
  mockApiFetch.mockResolvedValue(defaultApiResponse());
  const { e2eeService } = await import('@/renderer/services/e2eeService');
  (e2eeService as unknown as { isInitialized: boolean }).isInitialized = false;
});

afterEach(() => {
  resetRuntimeServerBase();
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

  describe('subscribe/unsubscribe/sendTyping', () => {
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

  describe('epoch validation batching', () => {
    async function renderInitializedHook() {
      useAuthStore.getState().setAccessToken('test-token');
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
      return renderHook(() => useWebSocket());
    }

    it('skips validation when no cached channel has an epoch', async () => {
      seedEpochChannels(0);
      const hook = await renderInitializedHook();

      triggerEpochValidation();

      await Promise.resolve();
      expect(epochValidationCalls()).toHaveLength(0);
      hook.unmount();
    });

    it('sends 500 cached epochs in one request', async () => {
      const channels = seedEpochChannels(500);
      mockApiFetch.mockImplementation((path: string) =>
        Promise.resolve(
          path === '/api/v1/e2ee/validate-epochs' ? successfulEpochResponse() : defaultApiResponse()
        )
      );
      const hook = await renderInitializedHook();

      triggerEpochValidation();

      await waitFor(() => expect(epochValidationCalls()).toHaveLength(1));
      const epochs = epochsFromRequest(0);
      expect(Object.keys(epochs)).toHaveLength(500);
      expect(epochs[channels[0].id]).toBe(1);
      expect(epochs[channels[499].id]).toBe(500);
      hook.unmount();
    });

    it('purges a cached key after missed channel-access revocation', async () => {
      const [channel] = seedEpochChannels(1);
      mockApiFetch.mockImplementation((path: string) =>
        Promise.resolve(
          path === '/api/v1/e2ee/validate-epochs'
            ? successfulEpochResponse([], [channel.id])
            : defaultApiResponse()
        )
      );
      const hook = await renderInitializedHook();

      triggerEpochValidation();

      const { e2eeService } = await import('@/renderer/services/e2eeService');
      await waitFor(() => {
        expect(useChannelStore.getState().channels).toEqual([]);
        expect(e2eeService.revokeChannelAccess).toHaveBeenCalledWith(channel.id);
      });
      hook.unmount();
    });

    it('validates and purges a cached channel outside the active server', async () => {
      const [activeChannel] = seedEpochChannels(1);
      const cachedChannelId = '00000000-0000-4000-8000-000000000001';
      useChannelStore.setState({
        channelIdsByServer: {
          'server-1': [activeChannel.id],
          'server-2': [cachedChannelId],
        },
      });
      mockGetCurrentKeyVersion.mockReturnValue(1);
      mockApiFetch.mockImplementation((path: string) =>
        Promise.resolve(
          path === '/api/v1/e2ee/validate-epochs'
            ? successfulEpochResponse([], [cachedChannelId])
            : defaultApiResponse()
        )
      );
      const hook = await renderInitializedHook();
      const { e2eeService } = await import('@/renderer/services/e2eeService');

      triggerEpochValidation();

      await waitFor(() => {
        expect(epochsFromRequest(0)).toEqual({ [activeChannel.id]: 1, [cachedChannelId]: 1 });
        expect(useChannelStore.getState().channelIdsByServer['server-2']).toEqual([]);
        expect(e2eeService.revokeChannelAccess).toHaveBeenCalledWith(cachedChannelId);
      });
      hook.unmount();
    });

    it('rotates a cached channel outside the active server after a missed revocation', async () => {
      const [activeChannel] = seedEpochChannels(1);
      const cachedChannelId = '00000000-0000-4000-8000-000000000001';
      useChannelStore.setState({
        channelIdsByServer: {
          'server-1': [activeChannel.id],
          'server-2': [cachedChannelId],
        },
      });
      mockGetCurrentKeyVersion.mockReturnValue(1);
      mockApiFetch.mockImplementation((path: string) => {
        if (path === '/api/v1/e2ee/validate-epochs') {
          return Promise.resolve(
            successfulEpochResponse([
              { channel_id: cachedChannelId, revoked_epoch: 1, successor_epoch: 2 },
            ])
          );
        }
        if (path === `/api/v1/e2ee/keys/${cachedChannelId}`) {
          return Promise.resolve({ ok: false, json: () => Promise.resolve({}) });
        }
        if (path === '/api/v1/servers/server-2/member-public-keys') {
          return Promise.resolve({
            ok: true,
            json: () =>
              Promise.resolve({
                members: [{ user_id: 'user-1', public_key: 'mock-pk-1', key_version: 7 }],
              }),
          });
        }
        return Promise.resolve(defaultApiResponse());
      });
      const random = vi.spyOn(Math, 'random').mockReturnValue(0);
      const hook = await renderInitializedHook();

      try {
        triggerEpochValidation();

        await waitFor(() => {
          expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/server-2/member-public-keys');
          expect(mockRotateChannelKey).toHaveBeenCalledWith(
            cachedChannelId,
            2,
            new Map([['user-1', 'mock-pk-1']]),
            { 'user-1': 7 },
            expect.any(Object)
          );
        });
      } finally {
        random.mockRestore();
        hook.unmount();
      }
    });

    it('waits for each response and processes revocations from both batches', async () => {
      const channels = seedEpochChannels(501);
      let resolveFirst:
        ((response: ReturnType<typeof successfulEpochResponse>) => void) | undefined;
      const firstResponse = new Promise<ReturnType<typeof successfulEpochResponse>>((resolve) => {
        resolveFirst = resolve;
      });
      let epochRequest = 0;
      mockApiFetch.mockImplementation((path: string) => {
        if (path !== '/api/v1/e2ee/validate-epochs') {
          return Promise.resolve(defaultApiResponse());
        }
        epochRequest += 1;
        if (epochRequest === 1) return firstResponse;
        return Promise.resolve(
          successfulEpochResponse([
            { channel_id: 'revoked-b', revoked_epoch: 2, successor_epoch: 3 },
          ])
        );
      });
      const hook = await renderInitializedHook();
      const rotations: Array<{ channelId: string; newEpoch: number }> = [];
      const captureRotation = (event: Event) => {
        const detail = (event as CustomEvent<{ channelId: string; newEpoch: number }>).detail;
        rotations.push({ channelId: detail.channelId, newEpoch: detail.newEpoch });
      };
      globalThis.addEventListener('e2ee-key-rotation', captureRotation);

      try {
        triggerEpochValidation();
        expect(epochValidationCalls()).toHaveLength(1);
        const firstEpochs = epochsFromRequest(0);
        const expectedFirstEpochs = Object.fromEntries(
          channels.slice(0, 500).map((channel, index) => [channel.id, index + 1])
        );
        expect(firstEpochs).toEqual(expectedFirstEpochs);

        resolveFirst?.(
          successfulEpochResponse([
            { channel_id: 'revoked-a', revoked_epoch: 1, successor_epoch: 2 },
          ])
        );

        await waitFor(() => expect(epochValidationCalls()).toHaveLength(2));
        expect(epochsFromRequest(1)).toEqual({ [channels[500].id]: 501 });
        await waitFor(() => {
          expect(mockInvalidateChannelKey).toHaveBeenCalledTimes(2);
          expect(mockInvalidateChannelKey).toHaveBeenNthCalledWith(1, 'revoked-a');
          expect(mockInvalidateChannelKey).toHaveBeenNthCalledWith(2, 'revoked-b');
          expect(rotations).toEqual([
            { channelId: 'revoked-a', newEpoch: 2 },
            { channelId: 'revoked-b', newEpoch: 3 },
          ]);
        });
      } finally {
        globalThis.removeEventListener('e2ee-key-rotation', captureRotation);
        hook.unmount();
      }
    });

    it('supersedes an in-flight validation after a newer reconnect', async () => {
      seedEpochChannels(501);
      let resolveFirst:
        ((response: ReturnType<typeof successfulEpochResponse>) => void) | undefined;
      let resolveSecond:
        ((response: ReturnType<typeof successfulEpochResponse>) => void) | undefined;
      const firstResponse = new Promise<ReturnType<typeof successfulEpochResponse>>((resolve) => {
        resolveFirst = resolve;
      });
      const secondResponse = new Promise<ReturnType<typeof successfulEpochResponse>>((resolve) => {
        resolveSecond = resolve;
      });
      let requestCount = 0;
      mockApiFetch.mockImplementation((path: string) => {
        if (path !== '/api/v1/e2ee/validate-epochs') {
          return Promise.resolve(defaultApiResponse());
        }
        requestCount += 1;
        return requestCount === 1 ? firstResponse : secondResponse;
      });
      const hook = await renderInitializedHook();

      triggerEpochValidation();
      await waitFor(() => expect(epochValidationCalls()).toHaveLength(1));
      triggerEpochValidation();
      await waitFor(() => expect(epochValidationCalls()).toHaveLength(2));

      await act(async () => {
        resolveFirst?.(
          successfulEpochResponse([
            { channel_id: 'stale-revocation', revoked_epoch: 1, successor_epoch: 2 },
          ])
        );
        await Promise.resolve();
        await Promise.resolve();
      });

      expect(mockInvalidateChannelKey).not.toHaveBeenCalled();
      expect(epochValidationCalls()).toHaveLength(2);
      resolveSecond?.(successfulEpochResponse());
      hook.unmount();
    });

    it('drops a pending batch when the runtime server selection changes', async () => {
      seedEpochChannels(501);
      let resolveFirst:
        ((response: ReturnType<typeof successfulEpochResponse>) => void) | undefined;
      const firstResponse = new Promise<ReturnType<typeof successfulEpochResponse>>((resolve) => {
        resolveFirst = resolve;
      });
      let epochRequest = 0;
      mockApiFetch.mockImplementation((path: string) => {
        if (path !== '/api/v1/e2ee/validate-epochs') {
          return Promise.resolve(defaultApiResponse());
        }
        epochRequest += 1;
        return epochRequest === 1 ? firstResponse : Promise.resolve(successfulEpochResponse());
      });
      const hook = await renderInitializedHook();
      const rotations: Array<{ channelId: string; newEpoch: number }> = [];
      const captureRotation = (event: Event) => {
        const detail = (event as CustomEvent<{ channelId: string; newEpoch: number }>).detail;
        rotations.push({ channelId: detail.channelId, newEpoch: detail.newEpoch });
      };
      globalThis.addEventListener('e2ee-key-rotation', captureRotation);

      try {
        triggerEpochValidation();
        expect(epochValidationCalls()).toHaveLength(1);
        setRuntimeServerBase('https://successor-session.example');

        await act(async () => {
          resolveFirst?.(
            successfulEpochResponse([
              { channel_id: 'stale-revocation', revoked_epoch: 1, successor_epoch: 2 },
            ])
          );
          await Promise.resolve();
          await Promise.resolve();
          await Promise.resolve();
        });

        expect.soft(mockInvalidateChannelKey).not.toHaveBeenCalled();
        expect.soft(rotations).toEqual([]);
        expect.soft(epochValidationCalls()).toHaveLength(1);
      } finally {
        globalThis.removeEventListener('e2ee-key-rotation', captureRotation);
        hook.unmount();
      }
    });

    it('drops a pending batch when the auth generation changes', async () => {
      seedEpochChannels(501);
      let resolveFirst:
        ((response: ReturnType<typeof successfulEpochResponse>) => void) | undefined;
      const firstResponse = new Promise<ReturnType<typeof successfulEpochResponse>>((resolve) => {
        resolveFirst = resolve;
      });
      mockApiFetch.mockImplementation((path: string) =>
        path === '/api/v1/e2ee/validate-epochs'
          ? firstResponse
          : Promise.resolve(defaultApiResponse())
      );
      const hook = await renderInitializedHook();

      triggerEpochValidation();
      expect(epochValidationCalls()).toHaveLength(1);
      useAuthStore.getState().setAccessToken('successor-token');

      await act(async () => {
        resolveFirst?.(
          successfulEpochResponse([
            { channel_id: 'stale-revocation', revoked_epoch: 1, successor_epoch: 2 },
          ])
        );
        await Promise.resolve();
        await Promise.resolve();
      });

      expect(mockInvalidateChannelKey).not.toHaveBeenCalled();
      expect(epochValidationCalls()).toHaveLength(1);
      hook.unmount();
    });

    it('stops after a non-OK first batch response', async () => {
      seedEpochChannels(501);
      mockApiFetch.mockImplementation((path: string) => {
        if (path !== '/api/v1/e2ee/validate-epochs') {
          return Promise.resolve(defaultApiResponse());
        }
        return Promise.resolve({
          ok: false,
          json: () => Promise.resolve({ error: 'validation failed' }),
        });
      });
      const hook = await renderInitializedHook();

      triggerEpochValidation();

      await waitFor(() => expect(epochValidationCalls()).toHaveLength(1));
      expect(Object.keys(epochsFromRequest(0))).toHaveLength(500);
      await Promise.resolve();
      expect(epochValidationCalls()).toHaveLength(1);
      expect(mockInvalidateChannelKey).not.toHaveBeenCalled();
      hook.unmount();
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
          json: () =>
            Promise.resolve({
              members: [
                { user_id: 'user-1', public_key: 'mock-pk-1' },
                { user_id: 'user-2', public_key: 'mock-pk-2' },
              ],
            }),
        }); // fetch member public keys

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

    it('coalesces member public key lookups during a server rotation burst', async () => {
      useAuthStore.getState().setAccessToken('test-token');
      vi.useFakeTimers();
      const random = vi.spyOn(Math, 'random').mockReturnValue(0);
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
      useChannelStore.getState().addChannel({
        id: 'ch-rotate-a',
        server_id: 'server-1',
        name: 'first',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      useChannelStore.getState().addChannel({
        id: 'ch-rotate-b',
        server_id: 'server-1',
        name: 'second',
        type: 'text',
        position: 1,
        created_at: '',
        updated_at: '',
      });
      mockApiFetch.mockImplementation((path: string) => {
        if (path.startsWith('/api/v1/e2ee/keys/')) {
          return Promise.resolve({ ok: false, json: () => Promise.resolve({}) });
        }
        if (path === '/api/v1/servers/server-1/member-public-keys') {
          return Promise.resolve({
            ok: true,
            json: () =>
              Promise.resolve({
                members: [{ user_id: 'user-1', public_key: 'mock-pk-1', key_version: 1 }],
              }),
          });
        }
        return Promise.resolve(defaultApiResponse());
      });
      const hook = renderHook(() => useWebSocket());

      try {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'ch-rotate-a', newEpoch: 2 },
          })
        );
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'ch-rotate-b', newEpoch: 2 },
          })
        );

        await vi.advanceTimersByTimeAsync(1);

        expect(
          mockApiFetch.mock.calls.filter(
            ([path]) => path === '/api/v1/servers/server-1/member-public-keys'
          )
        ).toHaveLength(1);
        expect(mockRotateChannelKey).toHaveBeenCalledTimes(2);
      } finally {
        random.mockRestore();
        hook.unmount();
        vi.useRealTimers();
      }
    });

    it('starts a queued rotation after an unrelated cache invalidation', async () => {
      useAuthStore.getState().setAccessToken('test-token');
      vi.useFakeTimers();
      const random = vi.spyOn(Math, 'random').mockReturnValue(0);
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
      useChannelStore.getState().addChannel({
        id: 'ch-invalidate-before-rotation',
        server_id: 'server-1',
        name: 'queued rotation',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      mockApiFetch.mockImplementation((path: string) =>
        path.startsWith('/api/v1/e2ee/keys/')
          ? Promise.resolve({ ok: false, json: () => Promise.resolve({}) })
          : Promise.resolve({
              ok: true,
              json: () =>
                Promise.resolve({
                  members: [{ user_id: 'user-1', public_key: 'mock-pk-1', key_version: 1 }],
                }),
            })
      );
      const hook = renderHook(() => useWebSocket());

      try {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'ch-invalidate-before-rotation', newEpoch: 2 },
          })
        );
        mockInvalidateChannelKey('ch-invalidate-before-rotation');

        await vi.advanceTimersByTimeAsync(1);

        expect(mockRotateChannelKey).toHaveBeenCalledWith(
          'ch-invalidate-before-rotation',
          2,
          expect.any(Map),
          { 'user-1': 1 },
          expect.any(Object)
        );
      } finally {
        random.mockRestore();
        hook.unmount();
        vi.useRealTimers();
      }
    });

    it.each(['a pending E2EE session fence', 'channel access revocation'])(
      'drops a rotation queued before %s',
      async (cause) => {
        useAuthStore.getState().setAccessToken('test-token');
        vi.useFakeTimers();
        const random = vi.spyOn(Math, 'random').mockReturnValue(0);
        const { e2eeService } = await import('@/renderer/services/e2eeService');
        (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
        useChannelStore.getState().addChannel({
          id: 'ch-stale-e2ee',
          server_id: 'server-1',
          name: 'stale E2EE',
          type: 'text',
          position: 0,
          created_at: '',
          updated_at: '',
        });
        const hook = renderHook(() => useWebSocket());

        try {
          globalThis.dispatchEvent(
            new CustomEvent('e2ee-key-rotation', {
              detail: { channelId: 'ch-stale-e2ee', newEpoch: 2 },
            })
          );
          if (cause === 'a pending E2EE session fence') {
            (
              e2eeService as unknown as { fencePendingOperations: () => void }
            ).fencePendingOperations();
          } else {
            (
              e2eeService as unknown as { revokeChannelAccess: (channelId: string) => void }
            ).revokeChannelAccess('ch-stale-e2ee');
          }

          await vi.advanceTimersByTimeAsync(1);

          expect(mockApiFetch).not.toHaveBeenCalled();
          expect(mockRotateChannelKey).not.toHaveBeenCalled();
        } finally {
          random.mockRestore();
          hook.unmount();
          vi.useRealTimers();
        }
      }
    );

    it('reports completion when rotation invalidates its operation guard', async () => {
      useAuthStore.getState().setAccessToken('test-token');
      vi.useFakeTimers();
      const random = vi.spyOn(Math, 'random').mockReturnValue(0);
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => {});
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
      useChannelStore.getState().addChannel({
        id: 'ch-complete-rotation',
        server_id: 'server-1',
        name: 'complete rotation',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      mockRotateChannelKey.mockImplementationOnce(async () => {
        mockChannelGuardGeneration += 1;
      });
      mockApiFetch.mockImplementation((path: string) =>
        path.startsWith('/api/v1/e2ee/keys/')
          ? Promise.resolve({ ok: false, json: () => Promise.resolve({}) })
          : Promise.resolve({
              ok: true,
              json: () =>
                Promise.resolve({
                  members: [{ user_id: 'user-1', public_key: 'mock-pk-1', key_version: 1 }],
                }),
            })
      );
      const hook = renderHook(() => useWebSocket());

      try {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'ch-complete-rotation', newEpoch: 2 },
          })
        );

        await vi.advanceTimersByTimeAsync(1);

        expect(debug).toHaveBeenCalledWith(
          '[E2EE] Key rotation completed for',
          'ch-complete-rotation',
          'epoch',
          2
        );
        expect(debug).not.toHaveBeenCalledWith('[E2EE] Key rotation failed', expect.any(Object));
      } finally {
        random.mockRestore();
        hook.unmount();
        vi.useRealTimers();
      }
    });

    it('drops a rotation queued before an auth-generation change', async () => {
      useAuthStore.getState().setAccessToken('test-token');
      vi.useFakeTimers();
      const random = vi.spyOn(Math, 'random').mockReturnValue(0);
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
      useChannelStore.getState().addChannel({
        id: 'ch-stale-auth',
        server_id: 'server-1',
        name: 'stale auth',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      const hook = renderHook(() => useWebSocket());

      try {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'ch-stale-auth', newEpoch: 2 },
          })
        );
        useAuthStore.getState().setAccessToken('successor-token');

        await vi.advanceTimersByTimeAsync(1);

        expect(mockApiFetch).not.toHaveBeenCalled();
        expect(mockRotateChannelKey).not.toHaveBeenCalled();
      } finally {
        random.mockRestore();
        hook.unmount();
        vi.useRealTimers();
      }
    });

    it('stops an in-flight rotation when the runtime server changes', async () => {
      useAuthStore.getState().setAccessToken('test-token');
      vi.useFakeTimers();
      const random = vi.spyOn(Math, 'random').mockReturnValue(0);
      const { e2eeService } = await import('@/renderer/services/e2eeService');
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
      useChannelStore.getState().addChannel({
        id: 'ch-stale-server',
        server_id: 'server-1',
        name: 'stale server',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      let resolveKeyCheck: ((response: ReturnType<typeof defaultApiResponse>) => void) | undefined;
      const keyCheck = new Promise<ReturnType<typeof defaultApiResponse>>((resolve) => {
        resolveKeyCheck = resolve;
      });
      mockApiFetch.mockImplementation((path: string) =>
        path === '/api/v1/e2ee/keys/ch-stale-server'
          ? keyCheck
          : Promise.resolve(defaultApiResponse())
      );
      const hook = renderHook(() => useWebSocket());

      try {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'ch-stale-server', newEpoch: 2 },
          })
        );
        await vi.advanceTimersByTimeAsync(1);
        setRuntimeServerBase('https://successor-session.example');
        resolveKeyCheck?.(defaultApiResponse());
        await Promise.resolve();
        await Promise.resolve();

        expect(
          mockApiFetch.mock.calls.filter(
            ([path]) => path === '/api/v1/servers/server-1/member-public-keys'
          )
        ).toHaveLength(0);
        expect(mockRotateChannelKey).not.toHaveBeenCalled();
      } finally {
        random.mockRestore();
        hook.unmount();
        vi.useRealTimers();
      }
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
