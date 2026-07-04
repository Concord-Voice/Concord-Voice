// Regression for #1957 — "Preserve session when refreshing after local GIF
// load failure."
//
// A beta user sent a GIF that failed to load locally ("everybody could see it
// but myself"), hit Ctrl+R, and was logged out (`[App] Session restore failed:
// no_session`). Root cause: the KLIPY GIF proxy travels through the SAME
// apiFetch 401 recovery as authoritative API calls, so a third-party
// content-proxy 401 tears down a perfectly valid Concord session
// (handleRefreshFailure → nuclearReset → clearTokens). A content-proxy 401 is
// NOT authoritative about session validity and must not log the user out.
//
// This test drives the REAL user path — GifEmbed → gifProvider.getBySlug →
// klipyClient.doFetch → apiFetch — so apiClient is intentionally NOT mocked.

import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { resetRuntimeServerBase } from '@/renderer/services/runtimeServerBase';

// Observe every session-teardown side effect. resetService is dynamically
// imported by apiClient's 401 path; vi.mock intercepts that import (mirrors
// apiClient.test.ts).
const mockGracefulReset = vi.fn();
const mockNuclearReset = vi.fn();
vi.mock('@/renderer/services/resetService', () => ({
  gracefulReset: mockGracefulReset,
  nuclearReset: mockNuclearReset,
  softRestart: vi.fn(),
  stopProactiveRefresh: vi.fn(),
}));

// Mock fetch globally — vi.stubGlobal is hoisted, so static import works.
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// REAL apiFetch (not mocked) — this is the surface the KLIPY proxy shares with
// authoritative API calls, and where the teardown-on-401 lives.
import { _resetRefreshState } from '@/renderer/services/apiClient';
import { klipyClient } from '@/renderer/services/gifProvider/klipyClient';

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('#1957 — a KLIPY GIF proxy 401 must not tear down a valid session', () => {
  beforeEach(() => {
    resetAllStores();
    _resetRefreshState();
    vi.clearAllMocks();
    resetRuntimeServerBase();
    useConnectionStore.getState().reset(); // phase = 'stable' (matches the reported diagnostic)
    (klipyClient as unknown as { _resetForTesting: () => void })._resetForTesting();
    klipyClient.setPersonalizationEnabled(false); // ephemeral customer_id → no extra network call
    localStorage.clear();
    // No electron bridge in the renderer test env → refreshAccessToken() resolves
    // null, so the 401 recovery reaches its failure branch (the teardown site).
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
  });

  afterEach(() => {
    resetRuntimeServerBase();
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
  });

  it('preserves the session when a GIF embed resolves against a 401 proxy response', async () => {
    // A valid, authenticated session — the user was able to send the GIF.
    useAuthStore.getState().setAccessToken('valid-access-token');
    useAuthStore.getState().setRememberMe(false); // session-only: the reported no_session case

    // The KLIPY GIF proxy 401s (upstream hiccup / proxy-side auth), NOT an
    // authoritative statement that the Concord session is dead.
    mockFetch.mockResolvedValue(jsonResponse({ error: 'unauthorized' }, 401));

    // Exact user path: rendering a GIF embed resolves its slug.
    const result = await klipyClient.getBySlug('some-gif-slug');

    // The GIF gracefully fails to resolve — the embed shows its error
    // placeholder ("everybody could see it but myself"). This is fine.
    expect(result).toBeNull();

    // But the Concord session MUST remain intact. No teardown of any tier, and
    // the access token is untouched — so a subsequent Ctrl+R restores instead
    // of logging out with `no_session`.
    expect(mockNuclearReset).not.toHaveBeenCalled();
    expect(mockGracefulReset).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBe('valid-access-token');
  });
});
