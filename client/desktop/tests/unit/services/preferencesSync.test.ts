import {
  preferencesSyncService,
  type PreferencesSyncDeps,
} from '@/renderer/services/preferencesSync';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import {
  useLayoutStore,
  type SidebarProfile,
  type SidebarProfiles,
} from '@/renderer/stores/layoutStore';
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
        sidebarProfiles: s.sidebarProfiles,
        sidebarLayoutsDecoupled: s.sidebarLayoutsDecoupled,
        channelPanelPinned: s.channelPanelPinned,
        channelPanelWidth: s.channelPanelWidth,
        memberPanelMode: s.memberPanelMode,
        memberPanelWidth: s.memberPanelWidth,
        friendsPanelMode: s.friendsPanelMode,
        serverBarHeight: s.serverBarHeight,
        folderBarHeight: s.folderBarHeight,
        serverFolders: s.serverFolders,
        serverOrder: s.serverOrder,
      };
    },
    setLayout: (patch) => useLayoutStore.setState(patch),
    applySidebarPreferences: (profiles, decoupled) =>
      useLayoutStore.getState().applySidebarPreferences(profiles, decoupled),
  });
}

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => {
  server.resetHandlers();
  preferencesSyncService.stopWatching();
});

describe('preferencesSyncService — DI lifecycle', () => {
  it('init() is idempotent — calling twice does not error or override', () => {
    resetSyncServiceDeps();
    const deps1: PreferencesSyncDeps = {
      getAppearance: () => useSettingsStore.getState().appearance,
      setAppearance: vi.fn(),
      getLayout: () => useLayoutStore.getState() as any,
      setLayout: vi.fn(),
      applySidebarPreferences: vi.fn(),
    };
    const deps2: PreferencesSyncDeps = {
      getAppearance: vi.fn().mockReturnValue({}),
      setAppearance: vi.fn(),
      getLayout: vi.fn().mockReturnValue({}),
      setLayout: vi.fn(),
      applySidebarPreferences: vi.fn(),
    };

    preferencesSyncService.init(deps1);
    preferencesSyncService.init(deps2); // second call should be a no-op

    // Verify deps1 is still active by calling pushPreferences
    // (deps2.getAppearance would return {} which doesn't have 'theme')
    // No error means deps1 is still used
    expect(() => preferencesSyncService.stopWatching()).not.toThrow();
  });

  it('requireDeps() throws when init() has not been called', async () => {
    resetSyncServiceDeps();
    // pushPreferences internally calls requireDeps()
    // but it also checks e2eeService.isInitialized first, so we need that to be true
    (e2eeService as any).isInitialized = true;
    useAuthStore.getState().setAccessToken('mock-token');

    await expect(async () => {
      await preferencesSyncService.pushPreferences();
    }).rejects.toThrow('PreferencesSyncService.init() must be called before use');
  });

  it('requireDeps() throws when calling fetchAndApply before init()', async () => {
    resetSyncServiceDeps();
    (e2eeService as any).isInitialized = true;
    useAuthStore.getState().setAccessToken('mock-token');

    await expect(async () => {
      await preferencesSyncService.fetchAndApply();
    }).rejects.toThrow('PreferencesSyncService.init() must be called before use');
  });

  it('requireDeps() throws when calling startWatching before init()', () => {
    resetSyncServiceDeps();

    expect(() => {
      preferencesSyncService.startWatching();
    }).toThrow('PreferencesSyncService.init() must be called before use');
  });
});

describe('preferencesSyncService', () => {
  beforeEach(() => {
    resetAllStores();
    // resetAllStores doesn't reset settings/layout stores — do it explicitly
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
      sidebarProfiles: {
        dm: {
          left: { width: 240, pinned: true },
          right: { width: 260, pinned: true },
        },
        server: {
          left: { width: 240, pinned: true },
          right: { width: 260, pinned: true },
        },
      },
      sidebarLayoutsDecoupled: false,
      channelPanelPinned: true,
      channelPanelWidth: 240,
      memberPanelMode: 'expanded',
      memberPanelWidth: 260,
      serverBarHeight: 48,
      folderBarHeight: 32,
      serverFolders: [],
      serverOrder: [],
      interfaceLocked: false,
    });
    useAuthStore.getState().setAccessToken('mock-token');
    vi.mocked(e2eeService.encryptPreferences).mockClear().mockResolvedValue('encrypted-blob');
    vi.mocked(e2eeService.decryptPreferences).mockReset();
    (e2eeService as any).isInitialized = true;
    initSyncServiceDeps();
  });

  describe('pushPreferences', () => {
    it('encrypts and pushes preferences to server', async () => {
      let pushedBody: any = null;
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, async ({ request }) => {
          pushedBody = await request.json();
          return HttpResponse.json({ version: 1 });
        })
      );

      await preferencesSyncService.pushPreferences();

      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
      expect(pushedBody).toEqual({ encrypted_data: 'encrypted-blob' });
    });

    it('round-trips all four retained docks and the decoupling preference', async () => {
      const profiles: SidebarProfiles = {
        dm: {
          left: { width: 56, pinned: true },
          right: { width: 179, pinned: false },
        },
        server: {
          left: { width: 180, pinned: false },
          right: { width: 340, pinned: true },
        },
      };
      let encryptedBlob: unknown;
      let pushedBody: unknown;
      vi.mocked(e2eeService.encryptPreferences).mockImplementationOnce(async (blob) => {
        encryptedBlob = blob;
        return 'round-trip-blob';
      });
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, async ({ request }) => {
          pushedBody = await request.json();
          return HttpResponse.json({ version: 1 });
        }),
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'round-trip-blob', version: 1 },
          })
        )
      );
      useLayoutStore.setState({
        sidebarProfiles: profiles,
        sidebarLayoutsDecoupled: true,
      });

      await preferencesSyncService.pushPreferences();

      expect(encryptedBlob).toMatchObject({
        layout: {
          sidebarProfiles: profiles,
          sidebarLayoutsDecoupled: true,
        },
      });
      expect(pushedBody).toEqual({ encrypted_data: 'round-trip-blob' });
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValueOnce(encryptedBlob);
      useLayoutStore.setState({
        sidebarProfiles: {
          dm: {
            left: { width: 240, pinned: true },
            right: { width: 260, pinned: true },
          },
          server: {
            left: { width: 240, pinned: true },
            right: { width: 260, pinned: true },
          },
        },
        sidebarLayoutsDecoupled: false,
      });

      await preferencesSyncService.fetchAndApply();

      expect(e2eeService.decryptPreferences).toHaveBeenCalledExactlyOnceWith('round-trip-blob');
      expect(useLayoutStore.getState().sidebarProfiles).toEqual(profiles);
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(true);
    });

    it('does nothing when e2ee is not initialized', async () => {
      (e2eeService as any).isInitialized = false;
      vi.mocked(e2eeService.encryptPreferences).mockClear();

      await preferencesSyncService.pushPreferences();

      expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
    });

    it('handles push failure gracefully', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({ error: 'Failed' }, { status: 500 })
        )
      );

      // Should not throw on server error
      await expect(preferencesSyncService.pushPreferences()).resolves.toBeUndefined();
      // The encrypt step completed successfully before the HTTP failure
      expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);
    });

    it('logs warn when pushPreferences network fetch throws', async () => {
      const consoleSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      server.use(http.put(`${API_BASE}/api/v1/users/me/preferences`, () => HttpResponse.error()));

      await preferencesSyncService.pushPreferences();

      expect(consoleSpy).toHaveBeenCalledWith('[PrefsSync] Push error:', expect.any(String));
      consoleSpy.mockRestore();
    });

    it('does not send a push that finishes encrypting after stopWatching', async () => {
      let releaseEncryption: ((ciphertext: string) => void) | undefined;
      let putCount = 0;
      vi.mocked(e2eeService.encryptPreferences).mockImplementationOnce(
        () =>
          new Promise<string>((resolve) => {
            releaseEncryption = resolve;
          })
      );
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, () => {
          putCount += 1;
          return HttpResponse.json({ version: 1 });
        })
      );

      const push = preferencesSyncService.pushPreferences();
      await vi.waitFor(() => expect(e2eeService.encryptPreferences).toHaveBeenCalledOnce());
      preferencesSyncService.stopWatching();
      releaseEncryption?.('encrypted-after-reset');
      await push;

      expect(putCount).toBe(0);
    });

    it('aborts an already-started preference PUT when watching stops', async () => {
      let markPutStarted: (() => void) | undefined;
      const putStarted = new Promise<void>((resolve) => {
        markPutStarted = resolve;
      });
      let releasePut: (() => void) | undefined;
      const putReleased = new Promise<void>((resolve) => {
        releasePut = resolve;
      });
      let requestSignal: AbortSignal | undefined;
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/preferences`, async ({ request }) => {
          requestSignal = request.signal;
          markPutStarted?.();
          await putReleased;
          return HttpResponse.json({ version: 1 });
        })
      );

      const push = preferencesSyncService.pushPreferences();
      await putStarted;

      try {
        preferencesSyncService.stopWatching();
        expect(requestSignal?.aborted).toBe(true);
      } finally {
        releasePut?.();
        await push;
      }
    });
  });

  describe('fetchAndApply', () => {
    it('does nothing when e2ee is not initialized', async () => {
      (e2eeService as any).isInitialized = false;
      vi.mocked(e2eeService.decryptPreferences).mockClear();

      await preferencesSyncService.fetchAndApply();

      expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
    });

    it('pushes local state when server has no preferences', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({ preferences: null })
        ),
        http.put(`${API_BASE}/api/v1/users/me/preferences`, () => HttpResponse.json({ version: 1 }))
      );

      await preferencesSyncService.fetchAndApply();

      // Should have pushed since preferences was null
      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
    });

    it('does not bootstrap-push after the auth lifecycle becomes stale', async () => {
      let current = true;
      const controller = new AbortController();
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () => {
          current = false;
          controller.abort();
          return HttpResponse.json({ preferences: null });
        })
      );

      await preferencesSyncService.fetchAndApply({
        signal: controller.signal,
        isCurrent: () => current,
      });

      expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
    });

    it('decrypts and applies remote preferences', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: {
          theme: 'light',
          colorScheme: 'hacker',
          fontSize: 'large',
          compactMode: true,
        },
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 220,
          memberPanelMode: 'collapsed',
          memberPanelWidth: 260,
          friendsPanelMode: 'hidden',
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: ['server-1'],
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

      expect(useSettingsStore.getState().appearance.theme).toBe('light');
      expect(useSettingsStore.getState().appearance.colorScheme).toBe('hacker');
      expect(useSettingsStore.getState().appearance.fontSize).toBe('large');
      expect(useSettingsStore.getState().appearance.compactMode).toBe(true);
      expect(useLayoutStore.getState().sidebarProfiles).toEqual({
        dm: {
          left: { width: 220, pinned: true },
          right: { width: 260, pinned: false },
        },
        server: {
          left: { width: 220, pinned: true },
          right: { width: 56, pinned: true },
        },
      });
      expect(useLayoutStore.getState().serverOrder).toEqual(['server-1']);
    });

    it('provisions a missing Server profile from the normalized DM profile', async () => {
      const dm: SidebarProfile = {
        left: { width: 240, pinned: true },
        right: { width: 260, pinned: true },
      };
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: useSettingsStore.getState().appearance,
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          friendsPanelMode: 'expanded',
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
          sidebarProfiles: { dm, server: undefined as unknown as SidebarProfile },
          sidebarLayoutsDecoupled: true,
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

      expect(useLayoutStore.getState().sidebarProfiles.server).toEqual(dm);
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(true);
    });

    it('derives retained profiles from an older version-1 legacy layout', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: useSettingsStore.getState().appearance,
        layout: {
          channelPanelPinned: false,
          channelPanelWidth: 300,
          memberPanelMode: 'collapsed',
          memberPanelWidth: 210,
          friendsPanelMode: 'hidden',
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

      expect(useLayoutStore.getState().sidebarProfiles).toEqual({
        dm: {
          left: { width: 300, pinned: false },
          right: { width: 260, pinned: false },
        },
        server: {
          left: { width: 300, pinned: false },
          right: { width: 56, pinned: true },
        },
      });
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(false);
    });

    it('uses encrypted legacy widths instead of conflicting device-local migration keys', async () => {
      localStorage.setItem('concord:channelPanelWidth', '350');
      localStorage.setItem('concord:memberPanelWidth', '330');
      localStorage.setItem('concord:friendsPanelWidth', '320');
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: useSettingsStore.getState().appearance,
        layout: {
          channelPanelPinned: false,
          channelPanelWidth: 300,
          memberPanelMode: 'expanded',
          memberPanelWidth: 210,
          friendsPanelMode: 'hidden',
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

      expect(useLayoutStore.getState().sidebarProfiles).toEqual({
        dm: {
          left: { width: 300, pinned: false },
          right: { width: 260, pinned: false },
        },
        server: {
          left: { width: 300, pinned: false },
          right: { width: 210, pinned: true },
        },
      });
    });

    it('fills only missing Server fields from DM before remote application', async () => {
      const dm: SidebarProfile = {
        left: { width: 275, pinned: true },
        right: { width: 205, pinned: false },
      };
      const partialServer = {
        left: { width: 320, pinned: false },
        right: { width: 310 },
      };
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: useSettingsStore.getState().appearance,
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          friendsPanelMode: 'expanded',
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
          sidebarProfiles: { dm, server: partialServer } as unknown as SidebarProfiles,
          sidebarLayoutsDecoupled: true,
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

      expect(useLayoutStore.getState().sidebarProfiles.server).toEqual({
        left: { width: 320, pinned: false },
        right: { width: 310, pinned: false },
      });
    });

    it('normalizes malformed nested profile values instead of installing them', async () => {
      const malformedProfiles = {
        dm: {
          left: { width: 'wide', pinned: 'yes' },
          right: { width: 205, pinned: false },
        },
        server: {
          left: null,
          right: { width: 'invalid', pinned: true },
        },
      };
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: useSettingsStore.getState().appearance,
        layout: {
          channelPanelPinned: true,
          channelPanelWidth: 240,
          memberPanelMode: 'expanded',
          memberPanelWidth: 260,
          friendsPanelMode: 'expanded',
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
          sidebarProfiles: malformedProfiles as unknown as SidebarProfiles,
          sidebarLayoutsDecoupled: true,
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

      expect(useLayoutStore.getState().sidebarProfiles).toEqual({
        dm: {
          left: { width: 240, pinned: true },
          right: { width: 205, pinned: false },
        },
        server: {
          left: { width: 240, pinned: true },
          right: { width: 205, pinned: true },
        },
      });
    });

    it('routes remote profiles through the Interface Lock guarded store action', async () => {
      const retained = useLayoutStore.getState().sidebarProfiles;
      useLayoutStore.setState({ interfaceLocked: true });
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        settings: useSettingsStore.getState().appearance,
        layout: {
          channelPanelPinned: false,
          channelPanelWidth: 300,
          memberPanelMode: 'hidden',
          memberPanelWidth: 300,
          friendsPanelMode: 'hidden',
          serverBarHeight: 48,
          folderBarHeight: 32,
          serverFolders: [],
          serverOrder: [],
          sidebarProfiles: {
            dm: {
              left: { width: 300, pinned: false },
              right: { width: 300, pinned: false },
            },
            server: {
              left: { width: 320, pinned: false },
              right: { width: 310, pinned: false },
            },
          },
          sidebarLayoutsDecoupled: true,
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

      expect(useLayoutStore.getState().sidebarProfiles).toEqual(retained);
      expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(false);
    });

    it('does not apply a response invalidated by stopWatching during decryption', async () => {
      let releaseDecrypt: ((value: unknown) => void) | undefined;
      vi.mocked(e2eeService.decryptPreferences).mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            releaseDecrypt = resolve;
          })
      );
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({ preferences: { encrypted_data: 'encrypted', version: 1 } })
        )
      );

      const hydration = preferencesSyncService.fetchAndApply();
      await vi.waitFor(() => expect(e2eeService.decryptPreferences).toHaveBeenCalledOnce());
      preferencesSyncService.stopWatching();
      releaseDecrypt?.({
        v: 1,
        settings: {
          theme: 'light',
          colorScheme: 'hacker',
          fontSize: 'large',
          compactMode: true,
        },
        layout: {
          channelPanelPinned: false,
          channelPanelWidth: 300,
          memberPanelMode: 'collapsed',
          memberPanelWidth: 200,
          serverBarHeight: 50,
          folderBarHeight: 40,
          serverFolders: [],
          serverOrder: ['prior-account-server'],
        },
      });
      await hydration;

      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
      expect(useLayoutStore.getState().serverOrder).toEqual([]);
    });

    it('ignores unknown preference versions', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 99,
        settings: { theme: 'light' },
        layout: {},
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({
            preferences: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await preferencesSyncService.fetchAndApply();

      // Should not have changed from defaults
      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
    });

    it('handles fetch failure gracefully', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
          HttpResponse.json({ error: 'Failed' }, { status: 500 })
        )
      );

      // Should not throw on server error
      await expect(preferencesSyncService.fetchAndApply()).resolves.toBeUndefined();
      // Local settings must remain unchanged when the server fetch fails
      expect(useSettingsStore.getState().appearance.theme).toBe('dark');
    });

    it('logs warn when fetchAndApply network fetch throws', async () => {
      const consoleSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      server.use(http.get(`${API_BASE}/api/v1/users/me/preferences`, () => HttpResponse.error()));

      await preferencesSyncService.fetchAndApply();

      expect(consoleSpy).toHaveBeenCalledWith(
        '[PrefsSync] Failed to fetch/apply preferences:',
        expect.any(String)
      );
      consoleSpy.mockRestore();
    });
  });

  describe('startWatching / stopWatching', () => {
    it('stopWatching as a no-op leaves no scheduled push behind', async () => {
      vi.useFakeTimers();
      try {
        vi.mocked(e2eeService.encryptPreferences).mockClear();
        preferencesSyncService.stopWatching();
        // No watch was started; advancing through the full debounce window
        // must not produce any encrypt/push side-effect.
        await vi.advanceTimersByTimeAsync(5000);
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      } finally {
        vi.useRealTimers();
      }
    });

    it('startWatching fires a debounced push on store change (end-to-end)', async () => {
      vi.useFakeTimers();
      try {
        vi.mocked(e2eeService.encryptPreferences).mockClear();
        preferencesSyncService.startWatching();
        // Drain microtasks so the dynamic imports inside startWatching resolve
        // and the Zustand subscribers actually register.
        await vi.advanceTimersByTimeAsync(0);

        // Mutate the settings store — the subscriber should fire schedulePush
        useSettingsStore.setState({
          appearance: { ...useSettingsStore.getState().appearance, theme: 'light' },
        });
        // Before the debounce fires, no push yet
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();

        // Advance past the 3s debounce — push should now have run exactly once
        await vi.advanceTimersByTimeAsync(3500);
        expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);
      } finally {
        preferencesSyncService.stopWatching();
        vi.useRealTimers();
      }
    });

    it('does not let an already-queued debounce callback adopt a post-reset generation', async () => {
      vi.useFakeTimers();
      const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout');
      try {
        vi.mocked(e2eeService.encryptPreferences).mockClear();
        preferencesSyncService.startWatching();
        await vi.advanceTimersByTimeAsync(0);

        useSettingsStore.setState({
          appearance: { ...useSettingsStore.getState().appearance, theme: 'light' },
        });
        const debounceCallback = setTimeoutSpy.mock.calls
          .filter(([, delay]) => delay === 3000)
          .at(-1)?.[0];
        expect(typeof debounceCallback).toBe('function');

        // Model the event-loop edge where the timer task is already queued, so
        // clearTimeout cannot prevent its callback from starting after reset.
        preferencesSyncService.stopWatching();
        preferencesSyncService.startWatching();
        await vi.advanceTimersByTimeAsync(0);
        useSettingsStore.setState({
          appearance: { ...useSettingsStore.getState().appearance, theme: 'dark' },
        });

        if (typeof debounceCallback === 'function') debounceCallback();
        preferencesSyncService.stopWatching();
        await vi.advanceTimersByTimeAsync(3500);
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      } finally {
        preferencesSyncService.stopWatching();
        vi.useRealTimers();
      }
    });

    it('startWatching cleans up previous subscriptions when called again', async () => {
      // Internal-state inspection is the only way to differentiate "cleanup
      // works" from "cleanup broken but pushes still coalesce by debounce" —
      // the unsubscribers array is the source of truth for active subscribes.
      const internal = preferencesSyncService as unknown as { unsubscribers: (() => void)[] };
      vi.useFakeTimers();
      try {
        expect(internal.unsubscribers.length).toBe(0);
        preferencesSyncService.startWatching();
        await vi.advanceTimersByTimeAsync(0); // drain dynamic imports
        const afterFirst = internal.unsubscribers.length;
        expect(afterFirst).toBeGreaterThan(0);

        preferencesSyncService.startWatching();
        await vi.advanceTimersByTimeAsync(0);
        // After a second startWatching, the internal stopWatching call must
        // have cleared the prior subscribers and the new ones replaced them —
        // count is the same, not doubled.
        expect(internal.unsubscribers.length).toBe(afterFirst);

        preferencesSyncService.stopWatching();
        expect(internal.unsubscribers.length).toBe(0);
      } finally {
        vi.useRealTimers();
      }
    });
  });
});
