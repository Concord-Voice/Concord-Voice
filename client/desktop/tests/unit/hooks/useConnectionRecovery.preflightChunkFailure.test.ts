import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

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

vi.mock('@/renderer/services/e2eeService', () => ({ e2eeService: { isInitialized: false } }));
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { emergencyCleanup: vi.fn() },
}));

// The recoveryService chunk fails to load — a stale SPA chunk after a Pages
// redeploy, fetched on-demand during preflight (the origin-502-storm scenario).
vi.mock('@/renderer/services/recoveryService', () => {
  throw new Error('Failed to fetch dynamically imported module: recoveryService-abc123.js');
});

// Assert self-heal fires via the shared trigger that runRecoveryModule calls.
const mockTriggerChunkSelfHeal = vi.fn();
vi.mock('@/renderer/spaSelfHealClient', () => ({
  triggerChunkSelfHeal: (...args: unknown[]) => mockTriggerChunkSelfHeal(...args),
}));

import { useConnectionRecovery } from '@/renderer/hooks/useConnectionRecovery';

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
  useConnectionStore.getState().reset();
  useVoiceStore.setState({ activeChannelId: null, connectionState: 'disconnected' });
  mockGetState.mockReturnValue('DISCONNECTED');
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('useConnectionRecovery — stale recoveryService chunk during preflight', () => {
  it('triggers self-heal instead of silently stranding the store on import failure', async () => {
    const { result } = renderHook(() => useConnectionRecovery(mockWsService as never, vi.fn()));

    // RECONNECTING → grace_period + the 15s preflight timer.
    result.current('RECONNECTING' as never);
    expect(useConnectionStore.getState().phase).toBe('grace_period');

    // Fire the preflight timer; the recoveryService import rejects.
    await vi.advanceTimersByTimeAsync(15_000);

    // Pre-fix: the rejection was swallowed by the caller's `.catch(console.debug)`
    // and self-heal never fired. Post-fix: runRecoveryModule catches it and
    // triggers self-heal (a bounded reload to fetch fresh chunks).
    expect(mockTriggerChunkSelfHeal).toHaveBeenCalledWith('chunk-import-rejected');
  });
});
