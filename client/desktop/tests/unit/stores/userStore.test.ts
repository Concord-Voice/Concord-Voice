import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { setSyncSuppressed } from '@/renderer/stores/colorSyncSuppression';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockUser } from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    disconnect: vi.fn(),
    sendProfileUpdate: vi.fn(),
  }),
  ConnectionState: { CONNECTED: 'connected', DISCONNECTED: 'disconnected' },
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: { clearKeys: vi.fn(), isInitialized: false, initialize: vi.fn() },
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { stopWatching: vi.fn(), pushPreferences: vi.fn() },
}));

// Spy on the color-sync suppression leaf so the regression test can assert the
// reset fires SYNCHRONOUSLY during logout() (a dynamic import would defer it).
vi.mock('@/renderer/stores/colorSyncSuppression', () => ({
  setSyncSuppressed: vi.fn(),
  isSyncSuppressed: vi.fn(() => false),
}));

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('userStore', () => {
  beforeEach(() => {
    resetAllStores();
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
