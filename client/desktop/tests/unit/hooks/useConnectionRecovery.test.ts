import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

// Mock dependencies
const mockSetAggressiveReconnect = vi.fn();
const mockGetState = vi.fn().mockReturnValue('DISCONNECTED');

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
  e2eeService: { validateEpochs: vi.fn() },
}));

vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { emergencyCleanup: vi.fn() },
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
  softRestart: vi.fn(),
}));

import { useConnectionRecovery } from '@/renderer/hooks/useConnectionRecovery';

beforeEach(() => {
  vi.clearAllMocks();
  useConnectionStore.getState().reset();
  useVoiceStore.setState({
    activeChannelId: null,
    connectionState: 'disconnected',
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useConnectionRecovery', () => {
  it('starts grace period on RECONNECTING from stable', () => {
    const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

    result.current('RECONNECTING' as never);

    expect(useConnectionStore.getState().phase).toBe('grace_period');
    expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(true);
  });

  it('captures voice channel ID before cleanup', () => {
    useVoiceStore.setState({
      activeChannelId: 'voice-123',
      connectionState: 'connected',
    });

    const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

    result.current('RECONNECTING' as never);

    expect(useConnectionStore.getState().lastVoiceChannelId).toBe('voice-123');
  });

  it('does not start grace period if already in recovery', () => {
    useConnectionStore.getState().enterRecoveryA();

    const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

    result.current('RECONNECTING' as never);

    // Phase should not change since it's already in recovery
    expect(useConnectionStore.getState().phase).toBe('recovery_a');
  });

  it('resets on CONNECTED during grace_period', () => {
    useConnectionStore.getState().startGracePeriod();

    const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

    result.current('CONNECTED' as never);

    expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
  });
});
