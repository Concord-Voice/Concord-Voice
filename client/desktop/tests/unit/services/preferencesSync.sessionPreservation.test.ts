// Regression for #1956: opening Settings -> Privacy & Security triggered a
// background preferences sync 401 that could tear down an otherwise valid session.

import { vi, describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
import { resetRuntimeServerBase } from '@/renderer/services/runtimeServerBase';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

const mockGracefulReset = vi.fn();
const mockNuclearReset = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    encryptPreferences: vi.fn().mockResolvedValue('encrypted-preferences'),
    decryptPreferences: vi.fn(),
  },
}));

import { _resetRefreshState, configureRefreshFailureReset } from '@/renderer/services/apiClient';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';

function initStubDeps(): void {
  (preferencesSyncService as unknown as { deps: unknown }).deps = null;
  preferencesSyncService.init({
    getAppearance: () => ({}) as never,
    setAppearance: () => {},
    getLayout: () => ({}) as never,
    setLayout: () => {},
  });
}

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());

describe('#1956: preferences sync 401 must not force logout', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
    configureRefreshFailureReset({
      gracefulReset: mockGracefulReset,
      nuclearReset: mockNuclearReset,
    });
    resetRuntimeServerBase();
    useConnectionStore.getState().reset();
    initStubDeps();
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
  });

  afterEach(() => {
    server.resetHandlers();
    preferencesSyncService.stopWatching();
    resetRuntimeServerBase();
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
  });

  it('preserves the session when a background preferences push returns 401', async () => {
    useAuthStore.getState().setAccessToken('valid-access-token');
    useAuthStore.getState().setRememberMe(false);

    server.use(
      http.put(`${API_BASE}/api/v1/users/me/preferences`, () =>
        HttpResponse.json({ error: 'unauthorized' }, { status: 401 })
      )
    );

    await preferencesSyncService.pushPreferences();

    expect(mockNuclearReset).not.toHaveBeenCalled();
    expect(mockGracefulReset).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('valid-access-token');
  });

  it('preserves the session when a background preferences fetch returns 401', async () => {
    useAuthStore.getState().setAccessToken('valid-access-token');
    useAuthStore.getState().setRememberMe(false);

    server.use(
      http.get(`${API_BASE}/api/v1/users/me/preferences`, () =>
        HttpResponse.json({ error: 'unauthorized' }, { status: 401 })
      )
    );

    await preferencesSyncService.fetchAndApply();

    expect(mockNuclearReset).not.toHaveBeenCalled();
    expect(mockGracefulReset).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('valid-access-token');
  });
});
