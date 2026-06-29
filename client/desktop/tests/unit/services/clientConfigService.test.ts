import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';

const mockApiFetch = vi.mocked(apiFetch);
const mockSpaCheckForUpdate = vi.fn();
const mockSpaReloadLatest = vi.fn();

async function flushFetchPath(): Promise<void> {
  for (let i = 0; i < 8; i += 1) {
    await Promise.resolve();
  }
}

// Import after mocking
import { clientConfigService } from '@/renderer/services/clientConfigService';

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  clientConfigService.stop();
  useClientConfigStore.setState({
    minVersion: '',
    featureFlags: {},
    mediaPlaneUrl: '',
    turn: { host: '', realm: '' },
    spaUrl: '',
    spaIpcContract: 0,
    serverCapabilities: null,
    lastFetchedAt: null,
  });
  useVoiceStore.setState({
    activeChannelId: null,
    connectionState: 'disconnected',
    isScreenSharing: false,
    callState: { kind: 'idle' },
  });
  mockSpaCheckForUpdate.mockResolvedValue({
    currentMode: 'remote',
    remoteAvailable: false,
    newerBytesAvailable: false,
    reason: 'test',
  });
  mockSpaReloadLatest.mockResolvedValue({ mode: 'remote', changed: true });
  globalThis.electron = {
    ...(globalThis.electron ?? {}),
    spaUpdate: {
      checkForUpdate: mockSpaCheckForUpdate,
      reloadLatest: mockSpaReloadLatest,
    },
  };
});

afterEach(() => {
  clientConfigService.stop();
  vi.useRealTimers();
});

describe('clientConfigService', () => {
  describe('fetch', () => {
    // The prior `'fetches config and updates store'` test in this position
    // used snake_case keys (`min_version`, `feature_flags`, ...) that did
    // not match the ServerConfigResponse camelCase contract. Because all
    // the destructured fields were undefined, setConfig wrote undefined
    // values to the store and the test passed only because
    // `lastFetchedAt` was set unconditionally — proving nothing about
    // contract correctness. Deleted; the `log-on-change` block below
    // covers the first-fetch path with correct camelCase keys.

    it('handles fetch errors gracefully', async () => {
      mockApiFetch.mockRejectedValue(new Error('Network error'));

      // Should not throw
      await expect(clientConfigService.fetch()).resolves.not.toThrow();
    });

    it('fetches server capabilities and stores OAuth provider availability', async () => {
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              minVersion: '0.2.0',
              featureFlags: {},
              mediaPlaneUrl: '',
              turn: {},
              spaUrl: '',
              spaIpcContract: 0,
            }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ auth: { oauthProviders: ['google'] } }),
        } as Response);

      await clientConfigService.fetch();

      expect(mockApiFetch).toHaveBeenNthCalledWith(1, '/api/v1/client/config');
      expect(mockApiFetch).toHaveBeenNthCalledWith(2, '/api/v1/server/capabilities');
      expect(useClientConfigStore.getState().serverCapabilities).toEqual({
        auth: { oauthProviders: ['google'] },
      });
    });

    it('fails closed to null server capabilities when the capabilities request fails', async () => {
      useClientConfigStore.setState({
        serverCapabilities: { auth: { oauthProviders: ['google', 'apple'] } },
      });
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              minVersion: '0.2.0',
              featureFlags: {},
              mediaPlaneUrl: '',
              turn: {},
              spaUrl: '',
              spaIpcContract: 0,
            }),
        } as Response)
        .mockResolvedValueOnce({ ok: false } as Response);

      await clientConfigService.fetch();

      expect(useClientConfigStore.getState().serverCapabilities).toBeNull();
    });
  });

  describe('log-on-change', () => {
    // Polling fires every 5 minutes and previously logged "Updated config"
    // on every poll regardless of whether anything changed. The fetch path
    // now compares the new payload to the prior store snapshot and only
    // logs when the first fetch lands or a tracked field differs.

    const baseResponse = {
      minVersion: '0.2.0',
      featureFlags: { gifsEnabled: true },
      mediaPlaneUrl: 'https://media.test/',
      turn: { host: 'turn.test', realm: 'r' },
      spaUrl: '',
      spaIpcContract: 1,
    };

    it('logs Updated config on the first fetch', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve(baseResponse),
      } as Response);

      await clientConfigService.fetch();

      const updateLogs = debugSpy.mock.calls.filter(
        (c) => c[0] === '[ClientConfig] Updated config'
      );
      expect(updateLogs.length).toBe(1);

      debugSpy.mockRestore();
    });

    it('does not log Updated config when subsequent fetch returns identical payload', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve(baseResponse),
      } as Response);

      await clientConfigService.fetch(); // first — logs
      debugSpy.mockClear();
      await clientConfigService.fetch(); // identical — should NOT log

      const updateLogs = debugSpy.mock.calls.filter(
        (c) => c[0] === '[ClientConfig] Updated config'
      );
      expect(updateLogs.length).toBe(0);

      debugSpy.mockRestore();
    });

    it('logs Updated config when a tracked field changes between fetches', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve(baseResponse),
      } as Response);
      await clientConfigService.fetch(); // first

      debugSpy.mockClear();

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            ...baseResponse,
            featureFlags: { ...baseResponse.featureFlags, gifsEnabled: false }, // changed
          }),
      } as Response);
      await clientConfigService.fetch(); // changed — should log

      const updateLogs = debugSpy.mock.calls.filter(
        (c) => c[0] === '[ClientConfig] Updated config'
      );
      expect(updateLogs.length).toBe(1);

      debugSpy.mockRestore();
    });

    // When the SPA URL changes between two non-empty values, the fetch path
    // marks the SPA update check urgent and lets main perform the reload.
    // 'Updated config' must NOT fire if a reload is applied.
    it('does not log Updated config when SPA reload is applied', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      // Seed store with a prior spaUrl so the reload branch is reachable
      useClientConfigStore.setState({ spaUrl: 'https://old.app.test/', lastFetchedAt: Date.now() });
      mockSpaCheckForUpdate.mockResolvedValueOnce({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ ...baseResponse, spaUrl: 'https://new.app.test/' }),
      } as Response);

      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);
      const updateLogs = debugSpy.mock.calls.filter(
        (c) => c[0] === '[ClientConfig] Updated config'
      );
      expect(updateLogs.length).toBe(0);
      debugSpy.mockRestore();
    });

    // The `turn` object is compared via JSON.stringify. The other tracked
    // fields are primitives; this is the only structured field that goes
    // through the JSON-equality path on the changed side. Cover it
    // explicitly so a future refactor that swaps the comparison strategy
    // still preserves the contract.
    it('logs Updated config when turn.host changes between fetches', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve(baseResponse),
      } as Response);
      await clientConfigService.fetch();

      debugSpy.mockClear();

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            ...baseResponse,
            turn: { host: 'turn.other.test', realm: baseResponse.turn.realm },
          }),
      } as Response);
      await clientConfigService.fetch();

      const updateLogs = debugSpy.mock.calls.filter(
        (c) => c[0] === '[ClientConfig] Updated config'
      );
      expect(updateLogs.length).toBe(1);

      debugSpy.mockRestore();
    });
  });

  describe('start/stop', () => {
    it('stops clears timers', () => {
      clientConfigService.start();
      clientConfigService.stop();
      // Calling stop again should be idempotent (no double-clear errors)
      expect(() => clientConfigService.stop()).not.toThrow();
    });

    it('runs startup and periodic config fetches', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            minVersion: '0.2.0',
            featureFlags: {},
            mediaPlaneUrl: '',
            turn: {},
            spaUrl: '',
            spaIpcContract: 0,
          }),
      } as Response);

      clientConfigService.start();
      await vi.advanceTimersByTimeAsync(2_000);
      await flushFetchPath();
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
      expect(mockApiFetch).toHaveBeenNthCalledWith(1, '/api/v1/client/config');
      expect(mockApiFetch).toHaveBeenNthCalledWith(2, '/api/v1/server/capabilities');
      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
      await flushFetchPath();
      expect(mockApiFetch).toHaveBeenCalledTimes(4);
      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(2);
    });
  });

  describe('SPA update checks', () => {
    const baseResponse = {
      minVersion: '0.2.0',
      featureFlags: { gifsEnabled: true },
      mediaPlaneUrl: 'https://media.test/',
      turn: { host: 'turn.test', realm: 'r' },
      spaUrl: 'https://spa.concordvoice.chat/index.html',
      spaIpcContract: 1,
    };

    function mockConfigFetch(): void {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve(baseResponse),
      } as Response);
    }

    it('reloads through main when constant-url SPA bytes are newer', async () => {
      mockConfigFetch();
      mockSpaCheckForUpdate.mockResolvedValueOnce({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();

      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(1);
      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);
    });

    it('collapses concurrent SPA freshness checks into one in-flight call', async () => {
      mockConfigFetch();
      let resolveCheck: (value: unknown) => void = () => {};
      mockSpaCheckForUpdate.mockReturnValueOnce(
        new Promise((resolve) => {
          resolveCheck = resolve;
        })
      );

      const a = clientConfigService.fetch();
      const b = clientConfigService.fetch();
      await flushFetchPath();

      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(1);
      resolveCheck({
        currentMode: 'remote',
        remoteAvailable: false,
        newerBytesAvailable: false,
        reason: 'test',
      });
      await Promise.all([a, b]);
    });

    it('rate-limits consecutive completed SPA checks', async () => {
      mockConfigFetch();

      await clientConfigService.fetch();
      await clientConfigService.fetch();

      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(1);
    });

    it('allows SPA checks after the rate-limit interval', async () => {
      mockConfigFetch();

      await clientConfigService.fetch();
      await vi.advanceTimersByTimeAsync(60_001);
      await clientConfigService.fetch();

      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(2);
    });

    it('checks on focus and rate-limits repeated focus events', async () => {
      clientConfigService.start();

      globalThis.dispatchEvent(new Event('focus'));
      await flushFetchPath();
      globalThis.dispatchEvent(new Event('focus'));
      await flushFetchPath();

      expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(1);
    });

    it('checks on visible resume events', async () => {
      const descriptor = Object.getOwnPropertyDescriptor(Document.prototype, 'visibilityState');
      Object.defineProperty(document, 'visibilityState', {
        value: 'visible',
        configurable: true,
      });

      try {
        clientConfigService.start();
        document.dispatchEvent(new Event('visibilitychange'));
        await flushFetchPath();

        expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(1);
      } finally {
        if (descriptor) {
          Object.defineProperty(Document.prototype, 'visibilityState', descriptor);
        }
        Reflect.deleteProperty(document, 'visibilityState');
      }
    });

    it('defers reload while connected to voice and applies on a later safe check', async () => {
      mockConfigFetch();
      useVoiceStore.setState({ connectionState: 'connected' });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();
      expect(mockSpaReloadLatest).not.toHaveBeenCalled();

      useVoiceStore.setState({ connectionState: 'disconnected' });
      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);
    });

    it.each(['connecting', 'reconnecting'] as const)(
      'defers reload while voice is %s',
      async (connectionState) => {
        mockConfigFetch();
        useVoiceStore.setState({ connectionState });
        mockSpaCheckForUpdate.mockResolvedValue({
          currentMode: 'remote',
          remoteAvailable: true,
          newerBytesAvailable: true,
          reason: 'remote SPA compatible',
        });

        await clientConfigService.fetch();

        expect(mockSpaReloadLatest).not.toHaveBeenCalled();
      }
    );

    it('defers reload while screen sharing', async () => {
      mockConfigFetch();
      useVoiceStore.setState({ isScreenSharing: true });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).not.toHaveBeenCalled();
    });

    it('applies deferred reload after screen sharing stops', async () => {
      mockConfigFetch();
      useVoiceStore.setState({ isScreenSharing: true });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();
      useVoiceStore.setState({ isScreenSharing: false });
      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);
    });

    it('defers reload while a DM call is ringing', async () => {
      mockConfigFetch();
      useVoiceStore.setState({
        callState: {
          kind: 'outgoing-ringing',
          conversationId: 'dm-1',
          ringId: 'ring-1',
          calleeUserIds: ['u2'],
          startedAt: 1,
          declinedUserIds: [],
        },
      });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).not.toHaveBeenCalled();
    });

    it.each([
      [
        'incoming-ringing',
        {
          kind: 'incoming-ringing' as const,
          conversationId: 'dm-1',
          ringId: 'ring-1',
          caller: { userId: 'u2', username: 'caller' },
          expiresAt: Date.now() + 30_000,
          isGroup: false,
        },
      ],
      ['in-call', { kind: 'in-call' as const }],
      ['ending', { kind: 'ending' as const }],
    ])('defers reload while DM call state is %s', async (_name, callState) => {
      mockConfigFetch();
      useVoiceStore.setState({ callState });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).not.toHaveBeenCalled();
    });

    it('applies deferred reload after DM call state returns idle', async () => {
      mockConfigFetch();
      useVoiceStore.setState({ callState: { kind: 'in-call' } });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();
      useVoiceStore.setState({ callState: { kind: 'idle' } });
      await clientConfigService.fetch();

      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);
    });

    it('clears the deferred reload intent when remote becomes unavailable (no stuck-true)', async () => {
      mockConfigFetch();
      // 1. Active call → defer a genuinely-available update.
      useVoiceStore.setState({ connectionState: 'connected' });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });
      await clientConfigService.fetch();
      expect(mockSpaReloadLatest).not.toHaveBeenCalled();

      // 2. Remote goes unavailable while still deferred → the intent must clear.
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: false,
        newerBytesAvailable: false,
        reason: 'remote unreachable',
      });
      await clientConfigService.fetch();
      expect(mockSpaReloadLatest).not.toHaveBeenCalled();

      // 3. Later, safe + remote back but with AMBIGUOUS bytes (undefined). A
      // stuck-true deferral would treat `undefined !== false` as "apply" and
      // spuriously reload; with the intent cleared, nothing applies. Advance
      // past the 60s rate-limit window so the check actually runs (isolating
      // the deferred-flag behavior from rate-limiting).
      vi.advanceTimersByTime(61_000);
      useVoiceStore.setState({ connectionState: 'disconnected' });
      mockSpaCheckForUpdate.mockResolvedValue({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: undefined,
        reason: 'remote SPA compatible',
      });
      await clientConfigService.fetch();
      expect(mockSpaReloadLatest).not.toHaveBeenCalled();
    });
  });

  describe('log sanitization', () => {
    it('does not log server-returned SPA URL when SPA update triggers reload', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

      // Seed store with a previous spaUrl so the update-reload branch triggers
      useClientConfigStore.setState({ spaUrl: 'https://old.app.test/' });

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            minVersion: '0.3.0',
            featureFlags: { gifsEnabled: true },
            mediaPlaneUrl: 'https://media.test/',
            turn: { host: 'turn.test', realm: 'r' },
            spaUrl: 'https://new.app.test/secret-subdomain/',
            spaIpcContract: 1,
          }),
      } as Response);

      mockSpaCheckForUpdate.mockResolvedValueOnce({
        currentMode: 'remote',
        remoteAvailable: true,
        newerBytesAvailable: true,
        reason: 'remote SPA compatible',
      });

      await clientConfigService.fetch();

      // Sanity check: prove the SPA-update-reload branch was actually entered.
      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);

      // Assert NO log call contains the server-returned URL substrings
      for (const call of debugSpy.mock.calls) {
        for (const arg of call) {
          expect(String(arg)).not.toContain('new.app.test');
          expect(String(arg)).not.toContain('secret-subdomain');
          expect(String(arg)).not.toContain('0.3.0');
        }
      }
      for (const call of warnSpy.mock.calls) {
        for (const arg of call) {
          expect(String(arg)).not.toContain('new.app.test');
        }
      }

      debugSpy.mockRestore();
      warnSpy.mockRestore();
    });

    it('does not pass raw Error object to console.warn on fetch error', async () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

      mockApiFetch.mockRejectedValue(new Error('boom'));

      await clientConfigService.fetch();

      // Every warn call's arguments must be strings — never an Error instance
      for (const call of warnSpy.mock.calls) {
        for (const arg of call) {
          expect(arg).not.toBeInstanceOf(Error);
        }
      }

      warnSpy.mockRestore();
    });

    it('does not log HTTP status when fetch fails with non-ok response', async () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

      mockApiFetch.mockResolvedValue({
        ok: false,
        status: 503,
        json: () => Promise.resolve({}),
      } as Response);

      await clientConfigService.fetch();

      for (const call of warnSpy.mock.calls) {
        for (const arg of call) {
          expect(String(arg)).not.toContain('503');
        }
      }

      warnSpy.mockRestore();
    });
  });
});
