/**
 * Extended tests for useConnectionRecovery — covers the 15-second grace period timer,
 * preflight diagnostics, recovery paths A/B, fatal path, voice rejoin on grace reconnect,
 * and E2EE epoch validation on reconnect.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useMemberStore } from '@/renderer/stores/memberStore';

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

const mockEmergencyCleanup = vi.fn();
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { emergencyCleanup: mockEmergencyCleanup, joinChannel: vi.fn() },
}));

const mockRunPreflight = vi.fn();
vi.mock('@/renderer/services/recoveryService', () => ({
  runPreflight: (...args: unknown[]) => mockRunPreflight(...args),
  clearCrashFlag: vi.fn(),
}));

const mockSoftRestart = vi.fn();
const mockGracefulReset = vi.fn();
vi.mock('@/renderer/services/resetService', () => ({
  softRestart: (...args: unknown[]) => mockSoftRestart(...args),
  gracefulReset: (...args: unknown[]) => mockGracefulReset(...args),
}));

const mockIsInitialized = { value: false };
const mockProcessPendingKeyRequests = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return mockIsInitialized.value;
    },
    processPendingKeyRequests: (...args: unknown[]) => mockProcessPendingKeyRequests(...args),
  },
}));

import { useConnectionRecovery } from '@/renderer/hooks/useConnectionRecovery';

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
  useConnectionStore.getState().reset();
  useVoiceStore.setState({
    activeChannelId: null,
    connectionState: 'disconnected',
  });
  useUserStore.getState().clearUser();
  mockIsInitialized.value = false;
  mockRunPreflight.mockResolvedValue({
    internet: 'ok',
    serverReachable: 'ok',
    tokenValid: 'ok',
    sessionRevoked: false,
    rendererStable: 'ok',
  });
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('useConnectionRecovery — extended', () => {
  describe('RECONNECTING from stable', () => {
    it('starts 15-second grace period and enables aggressive reconnect', () => {
      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);

      expect(useConnectionStore.getState().phase).toBe('grace_period');
      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(true);
    });

    it('captures voice channel before cleanup', () => {
      useVoiceStore.setState({
        activeChannelId: 'voice-123',
        connectionState: 'connected',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);

      expect(useConnectionStore.getState().lastVoiceChannelId).toBe('voice-123');
    });

    it('does not capture voice if already disconnected', () => {
      useVoiceStore.setState({
        activeChannelId: null,
        connectionState: 'disconnected',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);

      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
    });

    it('does not start grace if already in recovery', () => {
      useConnectionStore.getState().enterRecoveryA();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);

      expect(useConnectionStore.getState().phase).toBe('recovery_a');
    });
  });

  describe('grace period timeout (15 seconds)', () => {
    it('enters preflight after 15s if still disconnected', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      expect(useConnectionStore.getState().phase).toBe('grace_period');

      // Advance 15 seconds
      await vi.advanceTimersByTimeAsync(15_000);

      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
      expect(useConnectionStore.getState().phase).not.toBe('grace_period');
    });

    it('skips diagnostics if already reconnected within 15s', async () => {
      mockGetState.mockReturnValue('CONNECTED');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);

      // Advance 15 seconds — but WS is CONNECTED now
      await vi.advanceTimersByTimeAsync(15_000);

      // Should not have entered preflight
      expect(mockRunPreflight).not.toHaveBeenCalled();
    });

    it('enters fatal when session is revoked', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'failed',
        sessionRevoked: true,
        rendererStable: 'ok',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(useConnectionStore.getState().phase).toBe('fatal');
    });

    it('enters recovery A when server is unreachable', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(useConnectionStore.getState().phase).toBe('recovery_a');
    });

    it('enters recovery A when no internet', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      mockRunPreflight.mockResolvedValue({
        internet: 'failed',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'failed',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(useConnectionStore.getState().phase).toBe('recovery_a');
    });

    it('enters recovery B when renderer is unstable', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: false,
        rendererStable: 'failed',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(useConnectionStore.getState().phase).toBe('recovery_b');
      expect(mockSoftRestart).toHaveBeenCalled();
    });

    it('enters recovery A when everything is fine (WS glitch)', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      // Server reachable, token valid, renderer stable — just a WS glitch
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: false,
        rendererStable: 'ok',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);

      expect(useConnectionStore.getState().phase).toBe('recovery_a');
    });
  });

  describe('CONNECTED during grace_period', () => {
    it('resets connection state on grace reconnect', () => {
      useConnectionStore.getState().startGracePeriod();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
      expect(useConnectionStore.getState().phase).toBe('stable');
    });

    it('processes pending key requests when E2EE is initialized', () => {
      useConnectionStore.getState().startGracePeriod();
      mockIsInitialized.value = true;

      const validateEpochs = vi.fn().mockResolvedValue(undefined);
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      expect(mockProcessPendingKeyRequests).toHaveBeenCalled();
      expect(validateEpochs).toHaveBeenCalled();
    });

    it('rejoins voice channel if was in one before disconnect', () => {
      useConnectionStore.getState().startGracePeriod();
      useConnectionStore.getState().setLastVoiceChannelId('voice-123');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      // lastVoiceChannelId should be cleared
      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
    });
  });

  describe('CONNECTED during recovery_a', () => {
    it('performs graceful reset and fetches user', () => {
      useConnectionStore.getState().enterRecoveryA();

      const validateEpochs = vi.fn().mockResolvedValue(undefined);
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
      // Phase should be reset
      expect(useConnectionStore.getState().phase).toBe('stable');
    });

    it('validates E2EE epochs when initialized', () => {
      useConnectionStore.getState().enterRecoveryA();
      mockIsInitialized.value = true;

      const validateEpochs = vi.fn().mockResolvedValue(undefined);
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      expect(validateEpochs).toHaveBeenCalled();
    });
  });

  describe('CONNECTED during preflight', () => {
    it('performs graceful reset (same as recovery_a)', () => {
      useConnectionStore.getState().enterPreflight();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
      expect(useConnectionStore.getState().phase).toBe('stable');
    });
  });

  describe('CONNECTED from non-stable phase', () => {
    it('resets phase when in recovery_b', () => {
      useConnectionStore.getState().enterRecoveryB();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      expect(useConnectionStore.getState().phase).toBe('stable');
    });

    it('resets phase when in fatal', () => {
      useConnectionStore.getState().enterFatal();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      expect(useConnectionStore.getState().phase).toBe('stable');
    });
  });

  describe('self-presence on disconnect (#803)', () => {
    it('flips self to offline once the grace period expires while still disconnected', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      useMemberStore.getState().setSelfStatus('online');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      // During the grace period (the debounce) self stays online — no flicker.
      expect(useMemberStore.getState().selfStatus).toBe('online');

      // Sustained disconnect (>15s, still DISCONNECTED) → self genuinely offline.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(useMemberStore.getState().selfStatus).toBe('offline');
    });

    it('does NOT flip self to offline when reconnected within the grace period', async () => {
      mockGetState.mockReturnValue('CONNECTED');
      useMemberStore.getState().setSelfStatus('online');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never); // grace period starts
      result.current('CONNECTED' as never); // reconnect inside the grace window → phase resets to stable
      expect(useConnectionStore.getState().phase).toBe('stable');

      // The 15s timer still fires, but runPreflightDiagnostics bails on the phase
      // check (no longer grace_period), so self never flips offline — the grace
      // period absorbed the blip via the real debounce path, not just an early-return.
      await vi.advanceTimersByTimeAsync(15_000);
      expect(useMemberStore.getState().selfStatus).toBe('online');
    });

    it('preserves a deliberate dnd status on sustained disconnect (no clobber to offline)', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      useMemberStore.getState().setSelfStatus('dnd');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);

      // Sustained disconnect must only downgrade 'online'. A deliberate dnd/invisible
      // is preserved — otherwise the legacy online_user_ids reconnect path would
      // promote the clobbered 'offline' back to 'online', losing the user's choice.
      expect(useMemberStore.getState().selfStatus).toBe('dnd');
    });
  });

  describe('CONNECTED during stable', () => {
    it('does not reset when already stable (initial connect)', () => {
      // Phase is already 'stable' from reset in beforeEach

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      // Should remain stable — no reset call needed
      expect(useConnectionStore.getState().phase).toBe('stable');
    });
  });
});
