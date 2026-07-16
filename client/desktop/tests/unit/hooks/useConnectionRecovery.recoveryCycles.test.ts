/**
 * Repeated Recovery-A cycle regression (#2199, AC #7).
 *
 * Unlike the sibling recovery suites, this file does NOT mock resetService — the
 * real recoveryReset runs against real Zustand stores. Only the e2eeService crypto
 * singleton is stubbed. That is what makes this an integration regression rather
 * than a call assertion: it fails if recoveryReset is ever widened to clear
 * account-scoped state, or if the handler is pointed back at gracefulReset.
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useDraftMessageStore } from '@/renderer/stores/draftMessageStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';

const mockWsService = {
  setAggressiveReconnect: vi.fn(),
  getState: vi.fn().mockReturnValue('CONNECTED'),
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

const fencePendingOperations = vi.fn();
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    fencePendingOperations: () => fencePendingOperations(),
    clearKeys: vi.fn(),
    revokeChannelAccess: vi.fn(),
    isInitialized: true,
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    validateEpochs: vi.fn(),
  },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
  // resetService's transitive sync-service imports pull apiFetch/safeJson from
  // apiClient. recoveryReset never calls them, but stub them so a future edit
  // that does cannot silently self-heal past the assertions (#2199 review, P2-2).
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

vi.mock('@/renderer/services/recoveryService', () => ({
  runPreflight: vi.fn(),
  clearCrashFlag: vi.fn(),
}));

const hydrateCalls = vi.fn();
vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: () => {
    hydrateCalls();
    return Promise.resolve();
  },
}));

vi.mock('@/renderer/services/postLoginHydrationLifecycle', () => ({
  beginPostLoginHydrationGuard: vi.fn(() => ({
    signal: new AbortController().signal,
    isCurrent: () => true,
  })),
  isHydrationLifecycleCurrent: vi.fn(() => true),
  resetPostLoginHydrationLifecycle: vi.fn(),
}));

import { useConnectionRecovery } from '@/renderer/hooks/useConnectionRecovery';
import { useUserStore } from '@/renderer/stores/userStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { resetAllStores } from '../../helpers/store-helpers';

const validateEpochs = vi.fn().mockResolvedValue(undefined);

function seedAccountState() {
  useServerStore.setState({
    servers: [{ id: 'server-1', name: 'Test' }] as never,
    activeServerId: 'server-1',
  });
  useDraftMessageStore.getState().setDraft('channel-1', {
    text: 'unsent text',
    updatedAt: 1,
  });
  useE2EEStore.getState().setReady(true);
}

beforeEach(() => {
  vi.clearAllMocks();
  // [internal]rules/tests.md: reset ALL stores in beforeEach. This suite touches
  // real stores, so without it, module-level store state (drafts, channels,
  // localStorage, fetch journals) can leak from earlier tests in the worker and
  // make the regression order-dependent (#2199 review, P2-1).
  resetAllStores();
  useConnectionStore.getState().reset();
  useVoiceStore.setState({ activeChannelId: null, connectionState: 'disconnected' });
  useUserStore.setState({ fetchUser: vi.fn().mockResolvedValue(undefined) } as never);
  seedAccountState();
});

describe('repeated Recovery-A cycles (#2199 AC #7)', () => {
  it('survives 5 consecutive recovery cycles with no cumulative state loss', async () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    for (let i = 0; i < 5; i++) {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
      await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(i + 1));

      // Every cycle: account state intact, readiness agrees with the service.
      expect(useServerStore.getState().servers).toHaveLength(1);
      expect(useServerStore.getState().activeServerId).toBe('server-1');
      expect(useDraftMessageStore.getState().getDraft('channel-1')?.text).toBe('unsent text');
      expect(useE2EEStore.getState().ready).toBe(true);
      expect(useConnectionStore.getState().phase).toBe('stable');
    }

    // One fence per cycle — no duplicate or skipped fencing.
    expect(fencePendingOperations).toHaveBeenCalledTimes(5);
  });

  it('keeps E2EE readiness in agreement with the service across cycles (AC #3)', async () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    useConnectionStore.getState().enterRecoveryA();
    result.current('CONNECTED' as never);
    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));

    expect(useE2EEStore.getState().ready).toBe(e2eeService.isInitialized);
  });

  it('never clears E2EE key material — the handler gates on isInitialized', async () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    useConnectionStore.getState().enterRecoveryA();
    result.current('CONNECTED' as never);
    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));

    expect(e2eeService.clearKeys).not.toHaveBeenCalled();
  });
});
