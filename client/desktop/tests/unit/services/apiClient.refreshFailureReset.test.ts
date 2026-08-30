import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockServer } from '../../mocks/fixtures';
import {
  _resetRefreshState,
  apiFetch,
  configureRefreshFailureReset,
} from '@/renderer/services/apiClient';
import { e2eeService } from '@/renderer/services/e2eeService';
import { gracefulReset, nuclearReset } from '@/renderer/services/resetService';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';

const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

describe('apiClient authoritative refresh-failure reset', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
    mockFetch.mockReset();
    configureRefreshFailureReset({ gracefulReset, nuclearReset });
    globalThis.electron = {
      refreshToken: vi.fn().mockResolvedValue({ status: 'error' }),
    } as NonNullable<typeof globalThis.electron>;
  });

  afterEach(() => {
    vi.restoreAllMocks();
    globalThis.electron = undefined;
  });

  it('uses the real reset primitive to erase E2EE custody and content', async () => {
    useAuthStore.getState().setAccessToken('old-token');
    useAuthStore.getState().setSessionId('old-session');
    useAuthStore.getState().setRememberMe(true);
    useE2EEStore.getState().setReady(true);
    useServerStore.getState().addServer(mockServer);
    const clearKeys = vi.spyOn(e2eeService, 'clearKeys');
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

    const response = await apiFetch('/api/v1/users/me');

    expect(response.status).toBe(401);
    expect(clearKeys).toHaveBeenCalledOnce();
    expect(useE2EEStore.getState().ready).toBe(false);
    expect(useServerStore.getState().servers).toEqual([]);
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useAuthStore.getState().sessionId).toBeNull();
  });

  it('does not clear a successor lifecycle established by the reset callback', async () => {
    const gracefulResetForOldLifecycle = vi.fn(() => {
      useAuthStore.getState().setAccessToken('successor-token');
      useAuthStore.getState().setSessionId('successor-session');
    });
    const nuclearResetForOldLifecycle = vi.fn();
    configureRefreshFailureReset({
      gracefulReset: gracefulResetForOldLifecycle,
      nuclearReset: nuclearResetForOldLifecycle,
    });
    useAuthStore.getState().setAccessToken('old-token');
    useAuthStore.getState().setSessionId('old-session');
    useAuthStore.getState().setRememberMe(true);
    mockFetch.mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));

    const response = await apiFetch('/api/v1/users/me');

    expect(response.status).toBe(401);
    expect(gracefulResetForOldLifecycle).toHaveBeenCalledOnce();
    expect(nuclearResetForOldLifecycle).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('successor-token');
    expect(useAuthStore.getState().sessionId).toBe('successor-session');
  });
});
