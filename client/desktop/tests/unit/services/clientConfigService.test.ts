import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';

const mockApiFetch = vi.mocked(apiFetch);

// Import after mocking
import { clientConfigService } from '@/renderer/services/clientConfigService';

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  useClientConfigStore.setState({
    minVersion: '',
    featureFlags: {},
    mediaPlaneUrl: '',
    turn: { host: '', realm: '' },
    spaUrl: '',
    spaIpcContract: 0,
    lastFetchedAt: null,
  });
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
    // short-circuits to globalThis.location.reload() BEFORE reaching the
    // log gate — so 'Updated config' must NOT fire. This guards against
    // an inversion where a refactor moves the log above the reload.
    it('does not log Updated config when SPA-reload short-circuit fires', async () => {
      const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});

      // Seed store with a prior spaUrl so the reload branch is reachable
      useClientConfigStore.setState({ spaUrl: 'https://old.app.test/', lastFetchedAt: Date.now() });

      const reloadSpy = vi.fn();
      const originalLocationDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'location');

      try {
        Object.defineProperty(globalThis, 'location', {
          value: { reload: reloadSpy },
          writable: true,
          configurable: true,
        });

        mockApiFetch.mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ ...baseResponse, spaUrl: 'https://new.app.test/' }),
        } as Response);

        await clientConfigService.fetch();

        expect(reloadSpy).toHaveBeenCalled();
        const updateLogs = debugSpy.mock.calls.filter(
          (c) => c[0] === '[ClientConfig] Updated config'
        );
        expect(updateLogs.length).toBe(0);
      } finally {
        if (originalLocationDescriptor) {
          Object.defineProperty(globalThis, 'location', originalLocationDescriptor);
        }
        debugSpy.mockRestore();
      }
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

      // globalThis.location.reload would navigate; stub it. Capture the
      // original descriptor so we can restore jsdom's full Location object
      // after the test — otherwise the stub leaks into later tests.
      const reloadSpy = vi.fn();
      const originalLocationDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'location');

      try {
        Object.defineProperty(globalThis, 'location', {
          value: { reload: reloadSpy },
          writable: true,
          configurable: true,
        });

        await clientConfigService.fetch();

        // Sanity check: prove the SPA-update-reload branch was actually entered.
        // Without this, the sanitization asserts below are vacuous — they would
        // pass even if the code leaked nextSpaUrl, because the log line wouldn't
        // be reached. A prior iteration of this test used snake_case keys that
        // didn't match the ServerConfigResponse camelCase contract, making the
        // whole test a no-op.
        expect(reloadSpy).toHaveBeenCalled();

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
      } finally {
        // Restore original location descriptor so later tests get jsdom's
        // full Location object back.
        if (originalLocationDescriptor) {
          Object.defineProperty(globalThis, 'location', originalLocationDescriptor);
        }
        debugSpy.mockRestore();
        warnSpy.mockRestore();
      }
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
