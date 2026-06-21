import { vi, describe, it, expect, beforeEach } from 'vitest';

// ─── Setup: stub window.electron.onTokenRefreshed BEFORE apiClient loads ───
// The IPC subscription fires at module init, so the mock must exist first.
// We use vi.resetModules() + dynamic imports to get a fresh module graph
// where apiClient sees our mock during its initialization.

type TokenRefreshedCallback = (data: { accessToken: string; sessionId?: string }) => void;
let capturedCallback: TokenRefreshedCallback | null = null;
const mockUnsubscribe = vi.fn();

vi.stubGlobal('fetch', vi.fn());

// Set up window.electron with onTokenRefreshed BEFORE importing apiClient.
// setup.ts defines window.electron as writable, so direct assignment works.
window.electron = {
  onTokenRefreshed: vi.fn((cb: TokenRefreshedCallback) => {
    capturedCallback = cb;
    return mockUnsubscribe;
  }),
} as any;

// Reset module cache so apiClient re-runs its init code with our mock in place
vi.resetModules();

// Dynamic imports from the fresh module graph — these share the same
// authStore instance that apiClient's init code subscribes to.
const { useAuthStore } = await import('@/renderer/stores/authStore');
const { _resetRefreshState } = await import('@/renderer/services/apiClient');

describe('apiClient — onTokenRefreshed IPC (#254)', () => {
  beforeEach(() => {
    // clearAccessToken() resets accessToken, sessionId, and emailVerified to null.
    // We can't use resetAllStores() here because the static import from
    // store-helpers would reference a different module instance than our
    // dynamically-imported authStore (vi.resetModules() created a fresh graph).
    useAuthStore.getState().clearAccessToken();
    useAuthStore.getState().setRememberMe(true);
    _resetRefreshState();
  });

  it('subscribes to onTokenRefreshed at import time', () => {
    expect(window.electron!.onTokenRefreshed).toHaveBeenCalledTimes(1);
    expect(capturedCallback).toBeTypeOf('function');
  });

  it('updates authStore.accessToken when main process pushes a refreshed token', () => {
    expect(useAuthStore.getState().accessToken).toBeNull();

    capturedCallback!({ accessToken: 'proactive-token-abc' });

    expect(useAuthStore.getState().accessToken).toBe('proactive-token-abc');
  });

  it('updates authStore.sessionId when provided', () => {
    expect(useAuthStore.getState().sessionId).toBeNull();

    capturedCallback!({ accessToken: 'token-xyz', sessionId: 'session-456' });

    expect(useAuthStore.getState().accessToken).toBe('token-xyz');
    expect(useAuthStore.getState().sessionId).toBe('session-456');
  });

  it('does not update sessionId when omitted', () => {
    useAuthStore.getState().setSessionId('existing-session');

    capturedCallback!({ accessToken: 'token-no-session' });

    expect(useAuthStore.getState().accessToken).toBe('token-no-session');
    expect(useAuthStore.getState().sessionId).toBe('existing-session');
  });
});
