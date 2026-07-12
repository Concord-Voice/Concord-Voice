import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';
const logoutOrder = vi.hoisted(() => [] as string[]);

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    disconnect: vi.fn(() => logoutOrder.push('websocket-disconnect')),
    sendProfileUpdate: vi.fn(),
  }),
  ConnectionState: { CONNECTED: 'connected', DISCONNECTED: 'disconnected' },
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    clearKeys: vi.fn(() => logoutOrder.push('e2ee-clear')),
    isInitialized: false,
    initialize: vi.fn(),
  },
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    init: vi.fn(),
    startWatching: vi.fn(),
    stopWatching: vi.fn(() => logoutOrder.push('preferences-stop')),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
    pushPreferences: vi.fn(),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    startWatching: vi.fn(),
    stopWatching: vi.fn(() => logoutOrder.push('saved-gifs-stop')),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: {
    startWatching: vi.fn(),
    stopWatching: vi.fn(() => logoutOrder.push('friend-org-stop')),
    fetchAndApply: vi.fn().mockResolvedValue(true),
  },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: {
    reset: vi.fn(() => logoutOrder.push('presence-override-reset')),
    fetchAndApply: vi.fn().mockResolvedValue(true),
  },
}));

vi.mock('@/renderer/services/resetService', () => ({
  gracefulReset: vi.fn(),
  nuclearReset: vi.fn(),
}));

// Spy on the color-sync suppression leaf so the regression test can assert the
// reset fires SYNCHRONOUSLY during logout() (a dynamic import would defer it).
vi.mock('@/renderer/stores/colorSyncSuppression', () => ({
  setSyncSuppressed: vi.fn(() => logoutOrder.push('sync-suppression-reset')),
  isSyncSuppressed: vi.fn(() => false),
}));

import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { setSyncSuppressed } from '@/renderer/stores/colorSyncSuppression';
import { e2eeService } from '@/renderer/services/e2eeService';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import { gracefulReset, nuclearReset } from '@/renderer/services/resetService';
import { hydratePostLogin } from '@/renderer/services/postLoginHydration';
import { beginPostLoginHydrationGuard } from '@/renderer/services/postLoginHydrationLifecycle';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockUser } from '../../mocks/fixtures';
import { server } from '../../mocks/server';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('userStore', () => {
  beforeEach(() => {
    resetAllStores();
    logoutOrder.length = 0;
    vi.clearAllMocks();
    vi.mocked(gracefulReset).mockImplementation(() => {
      logoutOrder.push('graceful-reset');
    });
    vi.mocked(nuclearReset).mockImplementation(() => {
      logoutOrder.push('nuclear-reset');
      useUserStore.getState().clearUser();
      useAuthStore.getState().clearAccessToken();
      globalThis.electron?.clearTokens?.();
    });
    window.electron.logout = vi.fn().mockResolvedValue(undefined);
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('starts with null user and not loading', () => {
    resetAllStores();
    const state = useUserStore.getState();
    expect(state.user).toBeNull();
    expect(state.isLoading).toBe(false);
    expect(state.error).toBeNull();
  });

  describe('setUser', () => {
    it('sets the user and clears loading/error', () => {
      useUserStore.setState({ isLoading: true, error: 'old' });
      useUserStore.getState().setUser(mockUser);
      const state = useUserStore.getState();
      expect(state.user).toEqual(mockUser);
      expect(state.isLoading).toBe(false);
      expect(state.error).toBeNull();
    });
  });

  describe('clearUser', () => {
    it('clears user state', () => {
      useUserStore.getState().setUser(mockUser);
      useUserStore.getState().clearUser();
      expect(useUserStore.getState().user).toBeNull();
    });
  });

  describe('fetchUser', () => {
    it('fetches user from API', async () => {
      await useUserStore.getState().fetchUser();
      expect(useUserStore.getState().user).not.toBeNull();
      expect(useUserStore.getState().user?.username).toBe(mockUser.username);
      expect(useUserStore.getState().isLoading).toBe(false);
    });

    it('clears auth on 401', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ error: 'Unauthorized' }, { status: 401 })
        )
      );
      await useUserStore.getState().fetchUser();
      expect(useUserStore.getState().user).toBeNull();
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

    it('does NOT wipe persisted tokens on a 401 when "Remember Me" is on (regression #1768)', async () => {
      // Regression for #1768 (defect B): a transient 401 (e.g. during a server
      // deploy) must NOT delete the on-disk refresh token when rememberMe is set.
      // apiClient.handleRefreshFailure already honors rememberMe ("DO NOT clear
      // disk tokens"), but fetchUser then fired a SECOND, rememberMe-blind
      // electron.clearTokens() that nuked secure-token.dat — so the next launch
      // landed on the login screen despite "Remember Me" being checked.
      const clearTokensSpy = vi.fn();
      (window.electron as unknown as { clearTokens: () => void }).clearTokens = clearTokensSpy;
      useAuthStore.getState().setRememberMe(true);
      useAuthStore.getState().setAccessToken('mock-token');

      server.use(
        http.get(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ error: 'Unauthorized' }, { status: 401 })
        )
      );

      await useUserStore.getState().fetchUser();

      // The remembered session must survive: disk tokens are NOT cleared.
      expect(clearTokensSpy).not.toHaveBeenCalled();
      // In-memory UI auth is still cleared (parity with prior behavior).
      expect(useUserStore.getState().user).toBeNull();
    });

    it('DOES wipe persisted tokens on a 401 when "Remember Me" is off (token-clear authority)', async () => {
      // Companion to the rememberMe=true case: with Remember Me OFF, a 401 whose
      // refresh fails routes through apiClient.handleRefreshFailure -> nuclearReset
      // -> electron.clearTokens(), wiping the disk session. Locks the "single
      // authority" invariant from BOTH sides (#1768 review finding #7).
      const clearTokensSpy = vi.fn();
      (window.electron as unknown as { clearTokens: () => void }).clearTokens = clearTokensSpy;
      useAuthStore.getState().setRememberMe(false);
      useAuthStore.getState().setAccessToken('mock-token');

      server.use(
        http.get(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ error: 'Unauthorized' }, { status: 401 })
        )
      );

      await useUserStore.getState().fetchUser();

      expect(clearTokensSpy).toHaveBeenCalled();
      expect(useUserStore.getState().user).toBeNull();
    });

    it('sets error on failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ error: 'Server error' }, { status: 500 })
        )
      );
      await useUserStore.getState().fetchUser();
      expect(useUserStore.getState().error).toBe('Server error');
    });

    it('does not apply a held response after the auth lifecycle switches accounts', async () => {
      useAuthStore.getState().setSessionId('session-a');
      let markRequestStarted: (() => void) | undefined;
      const requestStarted = new Promise<void>((resolve) => {
        markRequestStarted = resolve;
      });
      let releaseResponse: (() => void) | undefined;
      const responseReleased = new Promise<void>((resolve) => {
        releaseResponse = resolve;
      });
      server.use(
        http.get(`${API_BASE}/api/v1/users/me`, async () => {
          markRequestStarted?.();
          await responseReleased;
          return HttpResponse.json({
            user: { id: 'stale-account', username: 'stale-account' },
          });
        })
      );

      const guard = beginPostLoginHydrationGuard();
      const fetch = useUserStore.getState().fetchUser(guard);
      await requestStarted;

      useAuthStore.getState().setSessionId('session-b');
      useUserStore.getState().setUser({ id: 'current-account', username: 'current-account' });
      releaseResponse?.();
      await fetch;

      expect(guard.isCurrent()).toBe(false);
      expect(useUserStore.getState().user?.id).toBe('current-account');
    });

    it('cancels an unguarded held profile fetch synchronously on logout', async () => {
      let markRequestStarted: (() => void) | undefined;
      const requestStarted = new Promise<void>((resolve) => {
        markRequestStarted = resolve;
      });
      let releaseResponse: (() => void) | undefined;
      const responseReleased = new Promise<void>((resolve) => {
        releaseResponse = resolve;
      });
      let requestSignal: AbortSignal | undefined;
      server.use(
        http.get(`${API_BASE}/api/v1/users/me`, async ({ request }) => {
          requestSignal = request.signal;
          markRequestStarted?.();
          await responseReleased;
          return HttpResponse.json({
            user: { id: 'logged-out-account', username: 'logged-out-account' },
          });
        })
      );

      let releaseLogout: (() => void) | undefined;
      window.electron.logout = vi.fn(
        () =>
          new Promise<void>((resolve) => {
            releaseLogout = resolve;
          })
      );

      const fetch = useUserStore.getState().fetchUser();
      await requestStarted;
      const logout = useUserStore.getState().logout();

      expect(requestSignal?.aborted).toBe(true);
      releaseResponse?.();
      await fetch;
      expect(useUserStore.getState().user).toBeNull();

      releaseLogout?.();
      await logout;
    });
  });

  describe('updateProfile', () => {
    it('updates profile and sets user', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ user: { ...mockUser, display_name: 'Updated' } })
        )
      );
      await useUserStore.getState().updateProfile({ display_name: 'Updated' });
      expect(useUserStore.getState().user?.display_name).toBe('Updated');
    });

    it('throws on non-ok response', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ error: 'Invalid avatar' }, { status: 400 })
        )
      );
      await expect(useUserStore.getState().updateProfile({ avatar_url: 'bad' })).rejects.toThrow(
        'Invalid avatar'
      );
    });

    it('clears auth on 401', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/users/me`, () =>
          HttpResponse.json({ error: 'Unauthorized' }, { status: 401 })
        )
      );
      await expect(useUserStore.getState().updateProfile({ display_name: 'x' })).rejects.toThrow(
        'Session expired'
      );
      expect(useAuthStore.getState().accessToken).toBeNull();
    });
  });

  describe('logout', () => {
    it('invalidates a held post-login hydration before awaiting main-process logout', async () => {
      let releaseHydration: (() => void) | undefined;
      vi.mocked(preferencesSyncService.fetchAndApply).mockImplementationOnce(
        () =>
          new Promise<void>((resolve) => {
            releaseHydration = resolve;
          })
      );

      let releaseLogout: (() => void) | undefined;
      window.electron.logout = vi.fn(
        () =>
          new Promise<void>((resolve) => {
            releaseLogout = resolve;
          })
      );

      const hydration = hydratePostLogin();
      await vi.waitFor(() => expect(preferencesSyncService.fetchAndApply).toHaveBeenCalledOnce());
      const guard = vi.mocked(preferencesSyncService.fetchAndApply).mock.calls[0]?.[0];
      expect(guard?.isCurrent()).toBe(true);

      const logout = useUserStore.getState().logout();

      expect(window.electron.logout).toHaveBeenCalledOnce();
      expect(guard?.isCurrent()).toBe(false);
      expect(nuclearReset).not.toHaveBeenCalled();

      releaseHydration?.();
      await hydration;
      expect(savedGifsSyncService.startWatching).not.toHaveBeenCalled();

      releaseLogout?.();
      await logout;
    });

    it('synchronously stops encrypted-social sync before clearing E2EE keys', async () => {
      let releaseLogout: (() => void) | undefined;
      window.electron.logout = vi.fn(() => {
        logoutOrder.push('electron-logout');
        return new Promise<void>((resolve) => {
          releaseLogout = resolve;
        });
      });

      const pending = useUserStore.getState().logout();
      const e2eeClearIndex = logoutOrder.indexOf('e2ee-clear');

      expect(preferencesSyncService.stopWatching).toHaveBeenCalledOnce();
      expect(savedGifsSyncService.stopWatching).toHaveBeenCalledOnce();
      expect(friendOrgSyncService.stopWatching).toHaveBeenCalledOnce();
      expect(presenceOverrideSyncService.reset).toHaveBeenCalledOnce();
      for (const cancellation of [
        'preferences-stop',
        'saved-gifs-stop',
        'friend-org-stop',
        'presence-override-reset',
      ]) {
        expect(logoutOrder.indexOf(cancellation)).toBeLessThan(e2eeClearIndex);
      }
      expect(setSyncSuppressed).toHaveBeenCalledWith(false);
      expect(logoutOrder.indexOf('websocket-disconnect')).toBeGreaterThan(e2eeClearIndex);
      expect(nuclearReset).not.toHaveBeenCalled();

      releaseLogout?.();
      await pending;

      expect(nuclearReset).toHaveBeenCalledOnce();
      expect(logoutOrder.at(-1)).toBe('nuclear-reset');
      expect(e2eeService.clearKeys).toHaveBeenCalledOnce();
    });

    it('clears user, auth, and calls cleanup', async () => {
      useUserStore.getState().setUser(mockUser);
      await useUserStore.getState().logout();
      expect(useUserStore.getState().user).toBeNull();
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

    it('logs error when electron.logout throws, then still clears state', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      useUserStore.getState().setUser(mockUser);
      useAuthStore.getState().setAccessToken('test-token');
      window.electron.logout = vi.fn().mockRejectedValue(new Error('IPC logout failed'));

      await useUserStore.getState().logout();

      expect(consoleSpy).toHaveBeenCalledWith('Logout API error:', 'IPC logout failed');
      // State still clears
      expect(useUserStore.getState().user).toBeNull();
      consoleSpy.mockRestore();
    });

    // Regression: logout() must reset color-sync suppression via a SYNCHRONOUS
    // static call, not a fire-and-forget `import('./settingsStore')`. The old
    // dynamic import left settingsStore (and its static overlayColors import)
    // loading after the test resolved, racing vitest worker teardown and
    // intermittently failing CI shards with EnvironmentTeardownError. A dynamic
    // import would defer setSyncSuppressed to a microtask, so it would NOT have
    // been called at the point we assert below (before any await in this test).
    it('resets color-sync suppression synchronously during logout (no teardown-racing import)', async () => {
      useUserStore.getState().setUser(mockUser);
      const pending = useUserStore.getState().logout();
      // logout()'s synchronous prologue (everything before its first `await`)
      // has already run, including the suppression reset.
      expect(setSyncSuppressed).toHaveBeenCalledWith(false);
      await pending; // let logout finish so nothing dangles into teardown
    });
  });
});
