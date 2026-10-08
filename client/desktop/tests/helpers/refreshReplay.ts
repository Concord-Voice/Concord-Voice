import { vi } from 'vitest';
import { useAuthStore } from '@/renderer/stores/auth/authStore';

let session = 0;

/**
 * Signs in a fresh session whose next 401 apiFetch recovers the real way: one
 * refresh through the main-process bridge, then a resend of the request. The
 * generation is new each call because apiFetch's 10 s refresh cooldown is keyed
 * on it, so two replay tests in one file would otherwise see the second refresh
 * refused.
 */
export function signInRefreshableSession(): void {
  session += 1;
  useAuthStore.setState({
    accessToken: `access-${session}`,
    sessionId: `session-${session}`,
    authGeneration: 10_000 + session,
  });
  const g = globalThis as { electron?: Record<string, unknown> };
  g.electron = {
    ...g.electron,
    refreshToken: vi.fn().mockResolvedValue({ status: 'ok', accessToken: `access-${session}-b` }),
  };
}
