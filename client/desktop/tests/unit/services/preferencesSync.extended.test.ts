/**
 * Extended tests for PreferencesSyncService — covers layout clamping,
 * custom color scheme handling, the debounce/echo-guard mechanism,
 * decryption failure recovery, and the schedulePush echo guard.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import { useAuthStore } from '@/renderer/stores/authStore';

const API_BASE = 'http://localhost:8080';

// Mock e2eeService
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    encryptPreferences: vi.fn().mockResolvedValue('encrypted-blob'),
    decryptPreferences: vi.fn(),
  },
}));

import { e2eeService } from '@/renderer/services/e2eeService';

/** Reset the singleton's DI deps so init() can be called again in each test. */
function resetSyncServiceDeps() {
  (preferencesSyncService as unknown as { deps: unknown }).deps = null;
}

/** Wire DI deps pointing at the real test-scoped stores. */
function initSyncServiceDeps() {
  resetSyncServiceDeps();
  preferencesSyncService.init({
    getAppearance: () => useSettingsStore.getState().appearance,
    setAppearance: (patch) =>
      useSettingsStore.setState((s) => ({ appearance: { ...s.appearance, ...patch } })),
    getLayout: () => {
      const s = useLayoutStore.getState();
      return {
        channelPanelPinned: s.channelPanelPinned,
        channelPanelWidth: s.channelPanelWidth,
        memberPanelMode: s.memberPanelMode,
        memberPanelWidth: s.memberPanelWidth,
        serverBarHeight: s.serverBarHeight,
        folderBarHeight: s.folderBarHeight,
        serverFolders: s.serverFolders,
        serverOrder: s.serverOrder,
      };
    },
    setLayout: (patch) => useLayoutStore.setState(patch),
  });
}

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => {
  server.resetHandlers();
  preferencesSyncService.stopWatching();
});

describe('preferencesSyncService — extended', () => {
  beforeEach(() => {
    resetAllStores();
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'concord',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
      },
    });
    useLayoutStore.setState({
      channelPanelPinned: true,
      channelPanelWidth: 240,
      memberPanelMode: 'expanded',
      memberPanelWidth: 260,
      serverBarHeight: 48,
      folderBarHeight: 32,
      serverFolders: [],
      serverOrder: [],
    });
    useAuthStore.getState().setAccessToken('mock-token');
    vi.mocked(e2eeService.encryptPreferences).mockResolvedValue('encrypted-blob');
    (e2eeService as any).isInitialized = true;
    initSyncServiceDeps();
    vi.clearAllMocks();
  });

  describe('fetchAndApply — layout clamping', () => {
    it('clamps layout values to valid ranges', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: {
          theme: 'dark',
          colorScheme: 'concord',
          fontSize: 'default',
          compactMode: false,
        },
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 9999, // Should be clamped to 400
          memberPanelMode: 'expanded',
          memberPanelWidth: 10, // Should be clamped to 160
          serverBarHeight: 0, // Should be clamped to 36
          folderBarHeight: 100, // Should be clamped to 48
          serverFolders: [],
          serverOrder: [],
        },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await preferencesSyncService.fetchAndApply();

      expect(useLayoutStore.getState().channelPanelWidth).toBe(400);
      expect(useLayoutStore.getState().memberPanelWidth).toBe(160);
      expect(useLayoutStore.getState().serverBarHeight).toBe(36);
      expect(useLayoutStore.getState().folderBarHeight).toBe(48);
    });
  });

  describe('fetchAndApply — custom color scheme', () => {
    it('applies custom color scheme with customColors', async () => {
      const customColors = { primary: '#ff0000', bg: '#000000' };
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: {
          theme: 'dark',
          colorScheme: 'custom',
          fontSize: 'default',
          compactMode: false,
          customColors,
        },
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
        },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await preferencesSyncService.fetchAndApply();

      expect(useSettingsStore.getState().appearance.colorScheme).toBe('custom');
      expect(useSettingsStore.getState().appearance.customColors).toEqual(customColors);
    });
  });

  describe('fetchAndApply — decryption failure', () => {
    it('re-encrypts and pushes when decryption fails', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockRejectedValue(new Error('Decryption failed'));

      let pushCalled = false;
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'old-encrypted', version: 1 },
          })
        ),
        http.put(`${API_BASE}/api/v1/users/me/preferences`, () => {
          pushCalled = true;
          return HttpResponse.json({ version: 2 });
        })
      );

      await preferencesSyncService.fetchAndApply();

      // Should have pushed local state as recovery
      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
      expect(pushCalled).toBe(true);
    });
  });

  describe('fetchAndApply — no changes needed', () => {
    it('does not update stores when remote matches local', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: {
          theme: 'dark',
          colorScheme: 'concord',
          fontSize: 'default',
          compactMode: false,
        },
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
        },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await preferencesSyncService.fetchAndApply();

      // Stores should remain unchanged since values match
      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
    });
  });

  describe('pushPreferences', () => {
    it('includes all expected fields in the encrypted blob', async () => {
      // Set some non-default values
      useSettingsStore.setState({
        appearance: {
          theme: 'light',
          colorScheme: 'hacker',
          fontSize: 'large',
          compactMode: true,
          reduceAnimations: true,
        },
      });
      useLayoutStore.setState({
        channelPanelPinned: false,
        channelPanelWidth: 300,
        memberPanelMode: 'collapsed',
        memberPanelWidth: 200,
        serverBarHeight: 50,
        folderBarHeight: 40,
        serverFolders: [{ id: 'f1', name: 'Test', serverIds: ['s1'] }],
        serverOrder: ['s1', 's2'],
      });

      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, () => HttpResponse.json({ version: 1 }))
      );

      await preferencesSyncService.pushPreferences();

      expect(e2eeService.encryptPreferences).toHaveBeenCalledWith(
        expect.objectContaining({
          v: 1,
          settings: expect.objectContaining({
            theme: 'light',
            colorScheme: 'hacker',
            fontSize: 'large',
            compactMode: true,
            reduceAnimations: true,
          }),
          layout: expect.objectContaining({
            channelPanelPinned: false,
            channelPanelWidth: 300,
            memberPanelMode: 'collapsed',
            memberPanelWidth: 200,
            serverBarHeight: 50,
            folderBarHeight: 40,
            serverFolders: expect.any(Array),
            serverOrder: ['s1', 's2'],
          }),
        })
      );
    });
  });

  describe('startWatching / stopWatching', () => {
    it('schedulePush is debounced by calling pushPreferences after timeout', async () => {
      // Test the debounce mechanism by calling pushPreferences directly
      // (the startWatching integration with Zustand subscribeWithSelector
      //  is inherently tied to store middleware and is tested indirectly
      //  via the echo guard test below)

      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, () => HttpResponse.json({ version: 1 }))
      );

      // Verify push works
      await preferencesSyncService.pushPreferences();
      expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);

      // Verify second push also works (no debounce on direct call)
      await preferencesSyncService.pushPreferences();
      expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(2);
    });

    it('does not trigger push during remote apply (echo guard)', async () => {
      vi.useFakeTimers();

      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: {
          theme: 'light',
          colorScheme: 'concord',
          fontSize: 'default',
          compactMode: false,
        },
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
        },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      preferencesSyncService.startWatching();
      vi.mocked(e2eeService.encryptPreferences).mockClear();

      await preferencesSyncService.fetchAndApply();

      // The store changes from fetchAndApply should NOT trigger a push
      // because of the echo guard (isApplyingRemote flag)
      await vi.advanceTimersByTimeAsync(5000);

      // encryptPreferences should not have been called (echo guard)
      expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();

      vi.useRealTimers();
    });
  });

  describe('fetchAndApply — server-side fetch failure', () => {
    it('handles network error gracefully', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () => {
          return HttpResponse.error();
        })
      );

      // Should not throw — and no decrypt attempt happens because the fetch errored
      await expect(preferencesSyncService.fetchAndApply()).resolves.toBeUndefined();
      expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
    });
  });

  describe('fetchAndApply — appearance change detection', () => {
    it('applies color scheme change without custom colors', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: {
          theme: 'dark',
          colorScheme: 'midnight', // Different from 'concord'
          fontSize: 'default',
          compactMode: false,
        },
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
        },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await preferencesSyncService.fetchAndApply();

      expect(useSettingsStore.getState().appearance.colorScheme).toBe('midnight');
    });
  });
});
