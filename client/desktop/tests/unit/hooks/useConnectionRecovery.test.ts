import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';

// Mock dependencies
const mockSetAggressiveReconnect = vi.fn();
const mockGetState = vi.fn().mockReturnValue('DISCONNECTED');

// handleReconnected calls `validateEpochsOnReconnect().catch(...)`, so this must
// return a promise. A bare vi.fn() returns undefined and throws on `.catch`.
const validateEpochs = vi.fn().mockResolvedValue(undefined);

const mockWsService = {
  setAggressiveReconnect: mockSetAggressiveReconnect,
  getState: mockGetState,
};

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => mockWsService,
  ConnectionState: {
    DISCONNECTED: 'DISCONNECTED',
    CONNECTING: 'CONNECTING',
    CONNECTED: 'CONNECTED',
    RECONNECTING: 'RECONNECTING',
  },
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    validateEpochs: vi.fn(),
    isInitialized: true,
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: vi.fn().mockResolvedValue(undefined),
}));

vi.mock('@/renderer/services/postLoginHydrationLifecycle', () => ({
  beginPostLoginHydrationGuard: vi.fn(() => ({
    signal: new AbortController().signal,
    isCurrent: () => true,
  })),
  isHydrationLifecycleCurrent: vi.fn(() => true),
  resetPostLoginHydrationLifecycle: vi.fn(),
}));

vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { emergencyCleanup: vi.fn(), joinChannel: vi.fn().mockResolvedValue(undefined) },
}));

vi.mock('@/renderer/services/recoveryService', () => ({
  runPreflight: vi.fn().mockResolvedValue({
    internet: 'ok',
    serverReachable: 'ok',
    tokenValid: 'ok',
    sessionRevoked: false,
    rendererStable: 'ok',
  }),
}));

vi.mock('@/renderer/services/resetService', () => ({
  gracefulReset: vi.fn(),
  recoveryReset: vi.fn(),
  softRestart: vi.fn(),
}));

import { useConnectionRecovery } from '@/renderer/hooks/voice/useConnectionRecovery';
import { gracefulReset, recoveryReset } from '@/renderer/services/resetService';
import { hydratePostLogin } from '@/renderer/services/postLoginHydration';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { voiceService } from '@/renderer/services/voiceService';

beforeEach(() => {
  vi.clearAllMocks();
  useConnectionStore.getState().reset();
  useVoiceStore.setState({
    activeChannelId: null,
    connectionState: 'disconnected',
  });
  // The recovery_a/preflight branch awaits fetchUser before hydrating.
  useUserStore.setState({ fetchUser: vi.fn().mockResolvedValue(undefined) } as never);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useConnectionRecovery', () => {
  it('starts grace period on RECONNECTING from stable', () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    result.current('RECONNECTING' as never);

    expect(useConnectionStore.getState().phase).toBe('grace_period');
    expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(true);
  });

  it('leaves an active voice session untouched on a transient disconnect', async () => {
    // A 1006 blip that reconnects within the grace period must not tear down
    // the (independent, still-healthy) media-plane session. Capture + cleanup
    // moved to grace expiry — see the extended suite for the expiry path.
    useVoiceStore.setState({
      activeChannelId: 'voice-123',
      connectionState: 'connected',
    });

    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    result.current('RECONNECTING' as never);
    await vi.dynamicImportSettled();

    expect(voiceService.emergencyCleanup).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
  });

  it('does not start grace period if already in recovery', () => {
    useConnectionStore.getState().enterRecoveryA();

    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    result.current('RECONNECTING' as never);

    // Phase should not change since it's already in recovery
    expect(useConnectionStore.getState().phase).toBe('recovery_a');
  });

  it('resets on CONNECTED during grace_period', () => {
    useConnectionStore.getState().startGracePeriod();

    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    result.current('CONNECTED' as never);

    expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
  });

  describe('recovery_a -> CONNECTED (#2199)', () => {
    it('calls recoveryReset, never gracefulReset', async () => {
      useConnectionStore.getState().enterRecoveryA();
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      await vi.waitFor(() => expect(recoveryReset).toHaveBeenCalledTimes(1));
      expect(gracefulReset).not.toHaveBeenCalled();
    });

    it('still hydrates the account after fencing', async () => {
      useConnectionStore.getState().enterRecoveryA();
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      await vi.waitFor(() => expect(hydratePostLogin).toHaveBeenCalledTimes(1));
    });

    it('returns the phase to stable', () => {
      useConnectionStore.getState().enterRecoveryA();
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      expect(useConnectionStore.getState().phase).toBe('stable');
    });

    it('takes the same path from preflight', async () => {
      useConnectionStore.getState().startGracePeriod();
      useConnectionStore.getState().enterPreflight();
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      await vi.waitFor(() => expect(recoveryReset).toHaveBeenCalledTimes(1));
      expect(gracefulReset).not.toHaveBeenCalled();
    });
  });
});
