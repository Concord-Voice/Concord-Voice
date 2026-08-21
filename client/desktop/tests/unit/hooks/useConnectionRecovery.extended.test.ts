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
import { useAuthStore } from '@/renderer/stores/authStore';

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
const mockJoinChannel = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { emergencyCleanup: mockEmergencyCleanup, joinChannel: mockJoinChannel },
}));

const mockRunPreflight = vi.fn();
vi.mock('@/renderer/services/recoveryService', () => ({
  runPreflight: (...args: unknown[]) => mockRunPreflight(...args),
  clearCrashFlag: vi.fn(),
}));

const mockSoftRestart = vi.fn();
const mockGracefulReset = vi.fn();
const mockRecoveryReset = vi.fn();
vi.mock('@/renderer/services/resetService', () => ({
  softRestart: (...args: unknown[]) => mockSoftRestart(...args),
  gracefulReset: (...args: unknown[]) => mockGracefulReset(...args),
  recoveryReset: (...args: unknown[]) => mockRecoveryReset(...args),
}));

const mockHydratePostLogin = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: (...args: unknown[]) => mockHydratePostLogin(...args),
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
  useAuthStore.getState().clearAccessToken();
  useAuthStore.getState().setAccessToken('recovery-token');
  useAuthStore.getState().setSessionId('recovery-session');
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

    it('leaves voice untouched during the grace period (no capture, no cleanup)', async () => {
      // A control-plane 1006 that reconnects in <1s says nothing about the
      // health of the separate media-plane session; tearing it down on the
      // first disconnect event destroyed healthy in-progress joins.
      useVoiceStore.setState({
        activeChannelId: 'voice-123',
        connectionState: 'connected',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.dynamicImportSettled();

      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
      expect(mockEmergencyCleanup).not.toHaveBeenCalled();
    });

    it('does not start grace if already in recovery', () => {
      useConnectionStore.getState().enterRecoveryA();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);

      expect(useConnectionStore.getState().phase).toBe('recovery_a');
    });
  });

  describe('grace period timeout (15 seconds)', () => {
    it('tears down voice and captures the channel only at grace expiry', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');
      useVoiceStore.setState({
        activeChannelId: 'voice-123',
        connectionState: 'connected',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      // Still inside the grace window: untouched.
      expect(mockEmergencyCleanup).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(15_000);
      await vi.dynamicImportSettled();

      expect(useConnectionStore.getState().lastVoiceChannelId).toBe('voice-123');
      expect(mockEmergencyCleanup).toHaveBeenCalledTimes(1);
    });

    it('still cleans up a mid-join session (connecting, no channel id yet) at grace expiry', async () => {
      // Between setConnectionState('connecting') and setActiveChannel there is
      // an awaited authorize round-trip — a session can be active with no
      // channel id. Cleanup keys on connectionState alone; only the rejoin
      // stash needs the id.
      mockGetState.mockReturnValue('DISCONNECTED');
      useVoiceStore.setState({
        activeChannelId: null,
        connectionState: 'connecting',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);
      await vi.dynamicImportSettled();

      expect(mockEmergencyCleanup).toHaveBeenCalledTimes(1);
      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
    });

    it('retracts the stash and skips cleanup if voice ends during the import window (Gitar #2873)', async () => {
      // The stash is captured synchronously at preflight entry, then RETRACTED
      // if the cleanup that justifies it is skipped. Fire the 15s preflight timer
      // synchronously (dispatches the voice-service import but does NOT flush its
      // microtask, so the guarded .then() is still pending), then have the user
      // leave voice, then flush the import. Cleanup is correctly skipped (they
      // left) and the stash must end up null — otherwise the reconnect would
      // phantom-rejoin a channel the user deliberately left.
      mockGetState.mockReturnValue('DISCONNECTED');
      useVoiceStore.setState({ activeChannelId: 'voice-123', connectionState: 'connected' });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      vi.advanceTimersByTime(15_000); // sync: preflight fires, stash set, import dispatched, .then() pending
      useVoiceStore.setState({ activeChannelId: null, connectionState: 'disconnected' }); // user leaves
      await vi.dynamicImportSettled(); // flush import → guarded .then() re-checks + retracts

      expect(mockEmergencyCleanup).not.toHaveBeenCalled();
      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
    });

    it('does not touch voice at grace expiry when no session is active', async () => {
      mockGetState.mockReturnValue('DISCONNECTED');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      await vi.advanceTimersByTimeAsync(15_000);
      await vi.dynamicImportSettled();

      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
      expect(mockEmergencyCleanup).not.toHaveBeenCalled();
    });

    it('never tears down voice when the socket reconnects within the grace period', async () => {
      mockGetState.mockReturnValue('CONNECTED');
      useVoiceStore.setState({
        activeChannelId: 'voice-123',
        connectionState: 'connected',
      });

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('RECONNECTING' as never);
      result.current('CONNECTED' as never);
      await vi.advanceTimersByTimeAsync(15_000);
      await vi.dynamicImportSettled();

      expect(mockEmergencyCleanup).not.toHaveBeenCalled();
      expect(mockJoinChannel).not.toHaveBeenCalled();
      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
    });

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

    it('does not rejoin voice on a grace reconnect — the session was never torn down', async () => {
      useConnectionStore.getState().startGracePeriod();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);
      await vi.dynamicImportSettled();

      expect(mockJoinChannel).not.toHaveBeenCalled();
    });
  });

  describe('CONNECTED during recovery_a', () => {
    it('rejoins the voice channel captured at grace expiry', async () => {
      // Sustained outage: preflight tore voice down and stashed the channel;
      // the recovery reconnect restores it. (Short blips never set the stash.)
      useConnectionStore.getState().enterRecoveryA();
      useConnectionStore.getState().setLastVoiceChannelId('voice-123');

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);
      await vi.dynamicImportSettled();

      expect(mockJoinChannel).toHaveBeenCalledWith('voice-123');
      expect(useConnectionStore.getState().lastVoiceChannelId).toBeNull();
    });

    it('performs recovery reset and fetches user', async () => {
      useConnectionStore.getState().enterRecoveryA();

      const validateEpochs = vi.fn().mockResolvedValue(undefined);
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
      // Phase should be reset
      expect(useConnectionStore.getState().phase).toBe('stable');
      await vi.waitFor(() => expect(mockHydratePostLogin).toHaveBeenCalledOnce());
    });

    it('never runs the logout-class gracefulReset on a same-account recovery (#2199)', async () => {
      useConnectionStore.getState().enterRecoveryA();

      const validateEpochs = vi.fn().mockResolvedValue(undefined);
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      await vi.waitFor(() => expect(mockRecoveryReset).toHaveBeenCalledOnce());
      expect(mockGracefulReset).not.toHaveBeenCalled();
    });

    it('validates E2EE epochs when initialized', async () => {
      useConnectionStore.getState().enterRecoveryA();
      mockIsInitialized.value = true;

      const validateEpochs = vi.fn().mockResolvedValue(undefined);
      const { result } = renderHook(() =>
        useConnectionRecovery(mockWsService as never, validateEpochs)
      );

      result.current('CONNECTED' as never);

      expect(validateEpochs).toHaveBeenCalled();
      await vi.waitFor(() => expect(mockHydratePostLogin).toHaveBeenCalledOnce());
    });

    it('restores the user and then rehydrates account-bound sync after reset', async () => {
      useConnectionStore.getState().enterRecoveryA();
      const recoveryOrder: string[] = [];
      mockRecoveryReset.mockImplementationOnce(() => {
        recoveryOrder.push('reset');
        useUserStore.getState().clearUser();
      });
      const fetchUser = vi
        .spyOn(useUserStore.getState(), 'fetchUser')
        .mockImplementationOnce(async () => {
          recoveryOrder.push('fetch-user');
          useUserStore.setState({
            user: { id: 'recovered-user', username: 'recovered' },
            isLoading: false,
            error: null,
          });
        });
      mockHydratePostLogin.mockImplementationOnce(async () => {
        recoveryOrder.push('hydrate');
      });

      try {
        const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

        result.current('CONNECTED' as never);

        await vi.waitFor(() => expect(mockHydratePostLogin).toHaveBeenCalledOnce());
        expect(recoveryOrder).toEqual(['reset', 'fetch-user', 'hydrate']);
        expect(useUserStore.getState().user?.id).toBe('recovered-user');
      } finally {
        fetchUser.mockRestore();
      }
    });

    it('does not hydrate a new account after auth changes during a held user fetch', async () => {
      useConnectionStore.getState().enterRecoveryA();
      useAuthStore.getState().setAccessToken('recovery-token');
      useAuthStore.getState().setSessionId('recovery-session');

      let releaseFetch: (() => void) | undefined;
      const fetchUser = vi.spyOn(useUserStore.getState(), 'fetchUser').mockImplementationOnce(
        () =>
          new Promise<void>((resolve) => {
            releaseFetch = resolve;
          })
      );

      try {
        const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

        result.current('CONNECTED' as never);
        await vi.waitFor(() => expect(fetchUser).toHaveBeenCalledOnce());

        const fetchCalls = fetchUser.mock.calls as unknown[][];
        const recoveryGuard = fetchCalls[0]?.[0] as
          { signal: AbortSignal; isCurrent: () => boolean } | undefined;
        expect(recoveryGuard).toBeDefined();

        useAuthStore.getState().setSessionId('different-session');
        releaseFetch?.();

        await vi.waitFor(() => expect(recoveryGuard?.isCurrent()).toBe(false));
        expect(mockHydratePostLogin).not.toHaveBeenCalled();
      } finally {
        fetchUser.mockRestore();
      }
    });
  });

  describe('CONNECTED during preflight', () => {
    it('performs recovery reset (same as recovery_a)', async () => {
      useConnectionStore.getState().enterPreflight();

      const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

      result.current('CONNECTED' as never);

      expect(mockSetAggressiveReconnect).toHaveBeenCalledWith(false);
      expect(useConnectionStore.getState().phase).toBe('stable');
      await vi.waitFor(() => expect(mockHydratePostLogin).toHaveBeenCalledOnce());
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
