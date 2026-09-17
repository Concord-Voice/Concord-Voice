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
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { E2EEEpochClaimStaleError } from '@/renderer/services/e2ee/e2eeErrors';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
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

vi.mock('@/renderer/services/messaging/websocketService', () => ({
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
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
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

vi.mock('@/renderer/services/system/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
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

// Mock apiFetch for validate-epochs
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
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
  // clearAllMocks drains neither a mockRejectedValueOnce queue nor an
  // implementation; the DM rotation tests below queue both.
  mockRotateChannelKey.mockReset();
  mockRotateChannelKey.mockResolvedValue(undefined);
  mockApiFetch.mockReset();
  mockApiFetch.mockResolvedValue(defaultApiResponse());
  const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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

      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');

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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
        const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
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

// ─── DM rotation coordinator ──────────────────────────────────────────────
//
// A DM has no server to resolve members through, so the channel branch of
// performKeyRotation returned before posting anything and a DM revocation
// never got its successor epoch — every participant then held only the
// revoked one (prod, 2026-09-17). The DM branch wraps for every participant
// from their individual public keys.
describe('useWebSocket — DM rotation coordinator', () => {
  const dmConversation = (overrides: Partial<DMConversation> = {}): DMConversation => ({
    id: 'conv-dm-1',
    isGroup: false,
    isPersonal: false,
    name: null,
    participants: [
      { userId: 'user-1', username: 'alice' },
      { userId: 'user-2', username: 'bob' },
    ],
    lastMessage: null,
    unreadCount: 0,
    createdAt: '2026-01-01T00:00:00Z',
    ...overrides,
  });

  const publicKeyRoutes = (
    keys: Record<string, { public_key: string; key_version?: number } | null>
  ) =>
    mockApiFetch.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/e2ee/keys/')) {
        return Promise.resolve({ ok: false, json: () => Promise.resolve({}) });
      }
      const match = /^\/api\/v1\/users\/([^/]+)\/public-key$/.exec(path);
      if (match) {
        const key = keys[match[1]];
        return Promise.resolve(
          key ? { ok: true, json: () => Promise.resolve(key) } : { ok: false, status: 404 }
        );
      }
      return Promise.resolve(defaultApiResponse());
    });

  async function armRotation() {
    useAuthStore.getState().setAccessToken('test-token');
    vi.useFakeTimers();
    const random = vi.spyOn(Math, 'random').mockReturnValue(0);
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
    const hook = renderHook(() => useWebSocket());
    return {
      dispatch: async (newEpoch: number) => {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: { channelId: 'conv-dm-1', newEpoch },
          })
        );
        await vi.advanceTimersByTimeAsync(1);
      },
      done: () => {
        random.mockRestore();
        hook.unmount();
        vi.useRealTimers();
      },
    };
  }

  it('wraps the successor for every participant from their own public keys', async () => {
    useDMStore.getState().addConversation(dmConversation());
    publicKeyRoutes({
      'user-1': { public_key: 'pk-1', key_version: 3 },
      'user-2': { public_key: 'pk-2', key_version: 1 },
    });
    const rotation = await armRotation();
    try {
      await rotation.dispatch(2);

      expect(mockRotateChannelKey).toHaveBeenCalledTimes(1);
      const [channelId, epoch, keys, versions] = mockRotateChannelKey.mock.calls[0];
      expect(channelId).toBe('conv-dm-1');
      expect(epoch).toBe(2);
      expect(keys).toEqual(
        new Map([
          ['user-1', 'pk-1'],
          ['user-2', 'pk-2'],
        ])
      );
      expect(versions).toEqual({ 'user-1': 3, 'user-2': 1 });
      expect(
        mockApiFetch.mock.calls.some(([path]) => String(path).includes('member-public-keys'))
      ).toBe(false);
    } finally {
      rotation.done();
    }
  });

  it('skips the rotation when a participant public key carries no version', async () => {
    useDMStore.getState().addConversation(dmConversation());
    publicKeyRoutes({
      'user-1': { public_key: 'pk-1', key_version: 3 },
      // user-2 has a key but no version: a versionless wrap on a successor
      // claim would bypass the server's #2420 freshness guard, so the
      // coordinator skips this cycle (the next cue retries) rather than post it.
      'user-2': { public_key: 'pk-2' },
    });
    const rotation = await armRotation();
    try {
      await rotation.dispatch(2);
      expect(mockRotateChannelKey).not.toHaveBeenCalled();
    } finally {
      rotation.done();
    }
  });

  // A cue that ran ahead of the conversation (this device's highest-seen
  // epoch was stale) is refused with the real epoch; claim ITS successor.
  it('retries a stale claim once at the successor of the epoch the server names', async () => {
    useDMStore.getState().addConversation(dmConversation());
    publicKeyRoutes({
      'user-1': { public_key: 'pk-1', key_version: 1 },
      'user-2': { public_key: 'pk-2', key_version: 1 },
    });
    mockRotateChannelKey
      .mockRejectedValueOnce(new E2EEEpochClaimStaleError(2))
      .mockResolvedValueOnce(undefined);
    const rotation = await armRotation();
    try {
      await rotation.dispatch(5);

      expect(mockRotateChannelKey).toHaveBeenCalledTimes(2);
      expect(mockRotateChannelKey.mock.calls[0][1]).toBe(5);
      expect(mockRotateChannelKey.mock.calls[1][1]).toBe(3);
    } finally {
      rotation.done();
    }
  });

  it('stops after a second stale refusal', async () => {
    useDMStore.getState().addConversation(dmConversation());
    publicKeyRoutes({
      'user-1': { public_key: 'pk-1', key_version: 1 },
      'user-2': { public_key: 'pk-2', key_version: 1 },
    });
    mockRotateChannelKey
      .mockRejectedValueOnce(new E2EEEpochClaimStaleError(2))
      .mockRejectedValueOnce(new E2EEEpochClaimStaleError(3));
    const rotation = await armRotation();
    try {
      await rotation.dispatch(5);

      expect(mockRotateChannelKey).toHaveBeenCalledTimes(2);
    } finally {
      rotation.done();
    }
  });

  // The refusal names an epoch at or past the one this cue asked for: another
  // claimant already established it. Re-keying on top would rotate everyone
  // a second time for nothing.
  it('does not re-key a conversation the server says is already past the cue', async () => {
    useDMStore.getState().addConversation(dmConversation());
    publicKeyRoutes({
      'user-1': { public_key: 'pk-1', key_version: 1 },
      'user-2': { public_key: 'pk-2', key_version: 1 },
    });
    mockRotateChannelKey.mockRejectedValueOnce(new E2EEEpochClaimStaleError(3));
    const rotation = await armRotation();
    try {
      await rotation.dispatch(2);

      expect(mockRotateChannelKey).toHaveBeenCalledTimes(1);
    } finally {
      rotation.done();
    }
  });

  it('does not claim a successor when a participant key is unavailable', async () => {
    useDMStore.getState().addConversation(dmConversation());
    publicKeyRoutes({
      'user-1': { public_key: 'pk-1', key_version: 1 },
      'user-2': null,
    });
    const rotation = await armRotation();
    try {
      await rotation.dispatch(2);

      expect(mockRotateChannelKey).not.toHaveBeenCalled();
    } finally {
      rotation.done();
    }
  });
});

// ─── Pending rewrap queue at login ────────────────────────────────────────
//
// Login publishes the token (and connects the socket) BEFORE the keys
// unwrap, so neither transition alone can run the holder's queue; the effect
// waits for both.
describe('useWebSocket — pending rewrap queue on ready + connected', () => {
  it('runs the queue once E2EE is ready and the socket is connected', async () => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    useAuthStore.getState().setAccessToken('test-token');
    renderHook(() => useWebSocket());

    act(() => {
      fireConnectionChange('connected');
    });
    expect(e2eeService.processPendingKeyRequests).not.toHaveBeenCalled();

    act(() => {
      useE2EEStore.getState().setReady(true);
    });

    await waitFor(() => expect(e2eeService.processPendingKeyRequests).toHaveBeenCalledTimes(1));
  });

  it('runs the queue when the socket connects after the keys are ready', async () => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    useAuthStore.getState().setAccessToken('test-token');
    act(() => {
      useE2EEStore.getState().setReady(true);
    });
    renderHook(() => useWebSocket());
    expect(e2eeService.processPendingKeyRequests).not.toHaveBeenCalled();

    act(() => {
      fireConnectionChange('connected');
    });

    await waitFor(() => expect(e2eeService.processPendingKeyRequests).toHaveBeenCalledTimes(1));
  });
});
