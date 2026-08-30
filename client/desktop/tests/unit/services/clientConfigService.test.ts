import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock apiFetch
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/system/apiClient';

const mockApiFetch = vi.mocked(apiFetch);
const mockSpaCheckForUpdate = vi.fn();
const mockSpaReloadLatest = vi.fn();
const MISSING_ACTIVITY_HISTORY_CAPABILITY = Symbol('missing-activity-history-capability');

function serverCapabilitiesPayload(
  activityHistorySupported:
    unknown | typeof MISSING_ACTIVITY_HISTORY_CAPABILITY = MISSING_ACTIVITY_HISTORY_CAPABILITY
): unknown {
  return {
    server: { name: 'Concord Voice', version: 'test', instanceType: 'saas' },
    auth: {
      emailVerificationRequired: true,
      oauthProviders: ['google'],
    },
    features: {
      voiceTiersSupported: true,
      ...(activityHistorySupported === MISSING_ACTIVITY_HISTORY_CAPABILITY
        ? {}
        : { activityHistorySupported }),
    },
    policyVersion: 'test',
  };
}

function jsonResponse(payload: unknown): Response {
  return {
    ok: true,
    json: () => Promise.resolve(payload),
  } as Response;
}

async function flushFetchPath(): Promise<void> {
  for (let i = 0; i < 8; i += 1) {
    await Promise.resolve();
  }
}

// Import after mocking
import { clientConfigService } from '@/renderer/services/system/clientConfigService';
import { deferred } from '../../helpers/deferred';

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  clientConfigService.stop();
  resetAllStores();
  useClientConfigStore.setState({
    minVersion: '',
    featureFlags: {},
    mediaPlaneUrl: '',
    turn: { host: '', realm: '' },
    spaUrl: '',
    spaIpcContract: 0,
    serverCapabilities: null,
    activityHistoryCapability: { status: 'loading' },
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
          json: () =>
            Promise.resolve({
              auth: { oauthProviders: ['google'] },
              features: {},
            }),
        } as Response);

      await clientConfigService.fetch();

      expect(mockApiFetch).toHaveBeenNthCalledWith(
        1,
        '/api/v1/client/config',
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      );
      expect(mockApiFetch).toHaveBeenNthCalledWith(
        2,
        '/api/v1/server/capabilities',
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      );
      expect(useClientConfigStore.getState().serverCapabilities).toEqual({
        auth: { oauthProviders: ['google'] },
        features: {},
      });
    });

    it('fails closed to null server capabilities when the capabilities request fails', async () => {
      useClientConfigStore.setState({
        serverCapabilities: {
          auth: { oauthProviders: ['google', 'apple'] },
          features: {},
        },
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

  describe('chunked-upload capability discovery — THROUGH THE PARSER', () => {
    // These go through refreshServerCapabilities, not through the store setter,
    // because the bug they exist to catch lived BETWEEN the two. The schema
    // omitted chunkedAttachmentUpload while the hand-written interface declared
    // it, and zod strips unknown keys -- so the parsed value was always
    // undefined, the capability always read confirmed-unsupported, and every
    // upload silently took the legacy path this feature replaces. tsc was happy
    // and every existing test passed, because they all set the store directly.

    /** A capabilities payload with `chunkedAttachmentUpload` present. */
    const withChunked = (v: unknown): unknown => ({
      server: { name: 'Concord Voice', version: 'test', instanceType: 'saas' },
      auth: { emailVerificationRequired: true, oauthProviders: ['google'] },
      features: { voiceTiersSupported: true, chunkedAttachmentUpload: v },
      policyVersion: 'test',
    });

    it('SURVIVES the parse — a true capability reaches the store', async () => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(withChunked(true)));

      await clientConfigService.refreshServerCapabilities();

      // The store state is the contract the uploader reads...
      expect(useClientConfigStore.getState().chunkedUploadCapability).toEqual({
        status: 'supported',
      });
      // ...and the parsed payload must still CARRY the field. Asserting only
      // the derived state would keep passing if the parser dropped it and some
      // other path set the flag.
      expect(
        useClientConfigStore.getState().serverCapabilities?.features.chunkedAttachmentUpload
      ).toBe(true);
    });

    it('an explicit false is confirmed-unsupported, not an error', async () => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(withChunked(false)));

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().chunkedUploadCapability).toEqual({
        status: 'confirmed-unsupported',
      });
    });

    it('an absent field is confirmed-unsupported — a server predating the field', async () => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(serverCapabilitiesPayload()));

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().chunkedUploadCapability).toEqual({
        status: 'confirmed-unsupported',
      });
      expect(
        useClientConfigStore.getState().serverCapabilities?.features.chunkedAttachmentUpload
      ).toBeUndefined();
    });

    it.each([
      ['string', 'yes'],
      ['number', 1],
      ['null', null],
    ])('rejects a non-boolean %s capability rather than coercing it', async (_label, value) => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(withChunked(value)));

      await clientConfigService.refreshServerCapabilities();

      // A payload that fails the schema is an ERROR, not a "no" -- we could
      // not ask, which is the third state.
      expect(useClientConfigStore.getState().chunkedUploadCapability).toEqual({
        status: 'error',
      });
    });
  });

  describe('Activity History capability discovery', () => {
    it('records a valid true capability as supported', async () => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(serverCapabilitiesPayload(true)));

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'supported',
      });
      expect(useClientConfigStore.getState().serverCapabilities).toEqual({
        auth: { oauthProviders: ['google'] },
        features: { activityHistorySupported: true },
      });
    });

    it.each([
      ['missing', MISSING_ACTIVITY_HISTORY_CAPABILITY],
      ['false', false],
    ])('records a valid %s capability as confirmed unsupported', async (_name, value) => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(serverCapabilitiesPayload(value)));

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'confirmed-unsupported',
      });
    });

    it.each([
      ['non-boolean value', serverCapabilitiesPayload('true')],
      ['null root', null],
      ['array root', []],
      ['missing features object', { auth: { oauthProviders: [] } }],
      ['array features value', { auth: { oauthProviders: [] }, features: [] }],
    ])('rejects a malformed %s', async (_name, payload) => {
      mockApiFetch.mockResolvedValueOnce(jsonResponse(payload));

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().serverCapabilities).toBeNull();
      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'error',
        lastConfirmedSupported: false,
      });
    });

    it('treats invalid JSON as an explicit capability error', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.reject(new SyntaxError('invalid JSON')),
      } as Response);

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'error',
        lastConfirmedSupported: false,
      });
    });

    it('treats a non-2xx response as an explicit capability error', async () => {
      mockApiFetch.mockResolvedValueOnce({ ok: false, status: 503 } as Response);

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'error',
        lastConfirmedSupported: false,
      });
    });

    it('treats a network failure as an explicit capability error', async () => {
      mockApiFetch.mockRejectedValueOnce(new Error('network unavailable'));

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'error',
        lastConfirmedSupported: false,
      });
    });

    it('discovers capability support even when client config fails', async () => {
      mockApiFetch
        .mockRejectedValueOnce(new Error('client config unavailable'))
        .mockResolvedValueOnce(jsonResponse(serverCapabilitiesPayload(true)));

      await clientConfigService.fetch();

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'supported',
      });
    });

    it('recovers from an error when an explicit retry succeeds', async () => {
      mockApiFetch
        .mockResolvedValueOnce({ ok: false, status: 503 } as Response)
        .mockResolvedValueOnce(jsonResponse(serverCapabilitiesPayload(true)));

      await clientConfigService.refreshServerCapabilities();
      expect(useClientConfigStore.getState().activityHistoryCapability.status).toBe('error');

      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'supported',
      });
    });

    it('preserves last-confirmed support when a later refresh errors', async () => {
      mockApiFetch
        .mockResolvedValueOnce(jsonResponse(serverCapabilitiesPayload(true)))
        .mockRejectedValueOnce(new Error('network unavailable'));

      await clientConfigService.refreshServerCapabilities();
      await clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'error',
        lastConfirmedSupported: true,
      });
    });

    it('does not flicker a confirmed state to loading during a background refresh', async () => {
      useClientConfigStore.setState({
        activityHistoryCapability: { status: 'supported' },
      });
      const pending = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(pending.promise);

      const refresh = clientConfigService.refreshServerCapabilities();

      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'supported',
      });

      pending.resolve(jsonResponse(serverCapabilitiesPayload(false)));
      await refresh;
      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'confirmed-unsupported',
      });
    });

    it('aborts stale config and capabilities and refreshes both after a server switch', async () => {
      const oldConfig = deferred<Response>();
      const oldCapabilities = deferred<Response>();
      const newConfig = deferred<Response>();
      const newCapabilities = deferred<Response>();
      mockApiFetch
        .mockReturnValueOnce(oldConfig.promise)
        .mockReturnValueOnce(oldCapabilities.promise)
        .mockReturnValueOnce(newConfig.promise)
        .mockReturnValueOnce(newCapabilities.promise);

      const oldFetch = clientConfigService.fetch();
      const oldConfigSignal = (mockApiFetch.mock.calls[0]?.[1] as RequestInit | undefined)?.signal;
      const oldCapabilitySignal = (mockApiFetch.mock.calls[1]?.[1] as RequestInit | undefined)
        ?.signal;

      useClientConfigStore.setState({
        minVersion: '0.2.0',
        featureFlags: { gifsEnabled: false },
        mediaPlaneUrl: 'https://old-stored-media.test',
        turn: { host: 'old-stored-turn.test', realm: 'old' },
        spaUrl: 'https://old-spa.test',
        spaIpcContract: 16,
        lastFetchedAt: 1,
      });

      const switchedRefresh = clientConfigService.resetAndRefreshRuntimeServer();

      expect(oldConfigSignal?.aborted).toBe(true);
      expect(oldCapabilitySignal?.aborted).toBe(true);
      expect(useClientConfigStore.getState()).toMatchObject({
        minVersion: '',
        featureFlags: {},
        mediaPlaneUrl: '',
        turn: { host: '', realm: '' },
        spaUrl: '',
        spaIpcContract: 0,
        lastFetchedAt: null,
      });
      expect(mockApiFetch).toHaveBeenNthCalledWith(
        3,
        '/api/v1/client/config',
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      );
      expect(mockApiFetch).toHaveBeenNthCalledWith(
        4,
        '/api/v1/server/capabilities',
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      );

      newConfig.resolve(
        jsonResponse({
          minVersion: '0.3.0',
          featureFlags: { gifsEnabled: true },
          mediaPlaneUrl: 'https://new-media.test',
          turn: { host: 'new-turn.test', realm: 'new' },
          spaUrl: '',
          spaIpcContract: 17,
        })
      );
      newCapabilities.resolve(jsonResponse(serverCapabilitiesPayload(true)));
      await switchedRefresh;

      oldConfig.resolve(
        jsonResponse({
          minVersion: '0.2.0',
          featureFlags: { gifsEnabled: false },
          mediaPlaneUrl: 'https://old-media.test',
          turn: { host: 'old-turn.test', realm: 'old' },
          spaUrl: '',
          spaIpcContract: 16,
        })
      );
      oldCapabilities.resolve(jsonResponse(serverCapabilitiesPayload(false)));
      await oldFetch;

      expect(useClientConfigStore.getState().mediaPlaneUrl).toBe('https://new-media.test');
      expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
        status: 'supported',
      });
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
      expect(mockApiFetch).toHaveBeenNthCalledWith(
        1,
        '/api/v1/client/config',
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      );
      expect(mockApiFetch).toHaveBeenNthCalledWith(
        2,
        '/api/v1/server/capabilities',
        expect.objectContaining({ signal: expect.any(AbortSignal) })
      );
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
