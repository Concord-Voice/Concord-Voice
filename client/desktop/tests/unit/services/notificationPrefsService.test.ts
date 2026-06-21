import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

// Mock apiClient BEFORE importing anything that pulls in the service so the
// service-side `import { apiFetch } from './apiClient'` resolves to the mock.
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  // safeJson is used by the service to drain response bodies — return a noop
  // that the service can await without hitting the real fetch Response API.
  safeJson: vi.fn(async (res: Response) => {
    if (typeof (res as unknown as { json?: unknown }).json === 'function') {
      return await (res as Response).json();
    }
    return {};
  }),
}));

import { apiFetch } from '@/renderer/services/apiClient';
import {
  hydrateNotificationPreferences,
  setMutePreference,
  mutedUntilFromDuration,
  startExpirySweep,
  stopExpirySweep,
  tryHydrateNotificationPrefs,
  MUTE_DURATION_LABELS,
} from '@/renderer/services/notificationPrefsService';
import { useNotificationPrefsStore } from '@/renderer/stores/notificationPrefsStore';
import { resetAllStores } from '../../helpers/store-helpers';

const mockApiFetch = vi.mocked(apiFetch);

const SERVER_ID = '11111111-1111-1111-1111-111111111111';
const CHANNEL_ID = '22222222-2222-2222-2222-222222222222';

function mockJsonResponse(body: unknown, ok = true): Response {
  return {
    ok,
    status: ok ? 200 : 500,
    json: async () => body,
  } as unknown as Response;
}

beforeEach(() => {
  vi.clearAllMocks();
  resetAllStores();
});

afterEach(() => {
  stopExpirySweep();
  vi.useRealTimers();
});

describe('hydrateNotificationPreferences', () => {
  it('writes the server response into the store', async () => {
    mockApiFetch.mockResolvedValueOnce(
      mockJsonResponse({
        preferences: [
          {
            target_type: 'server',
            target_id: SERVER_ID,
            muted: true,
            muted_until: null,
            updated_at: new Date().toISOString(),
          },
        ],
      })
    );
    await hydrateNotificationPreferences();
    expect(useNotificationPrefsStore.getState().mutedServers.has(SERVER_ID)).toBe(true);
  });

  it('throws on a non-OK response', async () => {
    mockApiFetch.mockResolvedValueOnce(mockJsonResponse({}, false));
    await expect(hydrateNotificationPreferences()).rejects.toThrow();
  });
});

describe('setMutePreference', () => {
  it('updates the store optimistically BEFORE the network resolves', async () => {
    // Hold the apiFetch promise pending so we can observe the store mid-flight.
    let resolveFetch: (r: Response) => void = () => {};
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((resolve) => {
        resolveFetch = resolve;
      })
    );

    const promise = setMutePreference('server', SERVER_ID, true, null);

    // Network hasn't completed yet, but the store should already reflect it.
    expect(useNotificationPrefsStore.getState().mutedServers.get(SERVER_ID)?.muted).toBe(true);

    resolveFetch(mockJsonResponse({ status: 'ok' }));
    await promise;
  });

  it('sends muted_until on a timed mute', async () => {
    mockApiFetch.mockResolvedValueOnce(mockJsonResponse({ status: 'ok' }));
    const future = new Date(Date.now() + 60_000);
    await setMutePreference('channel', CHANNEL_ID, true, future);

    const call = mockApiFetch.mock.calls[0];
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.target_type).toBe('channel');
    expect(body.target_id).toBe(CHANNEL_ID);
    expect(body.muted).toBe(true);
    expect(body.muted_until).toBe(future.toISOString());
  });

  it('omits muted_until on an unmute', async () => {
    mockApiFetch.mockResolvedValueOnce(mockJsonResponse({ status: 'ok' }));
    await setMutePreference('server', SERVER_ID, false, new Date(Date.now() + 60_000));

    const call = mockApiFetch.mock.calls[0];
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.muted).toBe(false);
    // Sending muted_until alongside muted=false would be nonsense (the row
    // is unmuted, an expiry has nothing to expire). The service strips it.
    expect(body.muted_until).toBeUndefined();
  });

  it('throws when the network call fails (and leaves the optimistic update in place)', async () => {
    mockApiFetch.mockResolvedValueOnce(mockJsonResponse({ error: 'boom' }, false));
    await expect(setMutePreference('server', SERVER_ID, true, null)).rejects.toThrow('boom');
    // We don't roll back automatically — callers decide on retry/rollback —
    // so the optimistic write stays.
    expect(useNotificationPrefsStore.getState().mutedServers.has(SERVER_ID)).toBe(true);
  });
});

describe('mutedUntilFromDuration', () => {
  it('returns null for indefinite', () => {
    expect(mutedUntilFromDuration('indefinite')).toBeNull();
  });

  it.each([
    ['15m', 15],
    ['1h', 60],
    ['8h', 480],
    ['24h', 1440],
  ] as const)('returns the right offset for %s', (duration, minutes) => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const result = mutedUntilFromDuration(duration);
    expect(result).not.toBeNull();
    expect(result!.getTime()).toBe(Date.now() + minutes * 60_000);
  });

  it('exposes a label for every duration', () => {
    // Defensive check — a new duration added without a label would render a
    // blank menu item. This test fails loudly in that case.
    expect(Object.keys(MUTE_DURATION_LABELS).sort()).toEqual(
      ['15m', '1h', '8h', '24h', 'indefinite'].sort()
    );
  });
});

describe('expiry sweep timer', () => {
  it('coalesces multiple start calls into a single interval', async () => {
    vi.useFakeTimers();
    const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
    startExpirySweep();
    startExpirySweep();
    startExpirySweep();
    // Three starts should only ever register one underlying timer — otherwise
    // logout/login cycles would leak intervals.
    expect(setIntervalSpy).toHaveBeenCalledTimes(1);
    stopExpirySweep();
  });

  it('runs clearExpiredMutes on each tick', () => {
    vi.useFakeTimers();
    useNotificationPrefsStore
      .getState()
      .setMute('server', SERVER_ID, true, new Date(Date.now() - 1_000));
    startExpirySweep();
    vi.advanceTimersByTime(60_000);
    expect(useNotificationPrefsStore.getState().mutedServers.has(SERVER_ID)).toBe(false);
    stopExpirySweep();
  });
});

describe('tryHydrateNotificationPrefs', () => {
  it('hydrates and starts the expiry sweep on success', async () => {
    const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
    mockApiFetch.mockResolvedValueOnce(
      mockJsonResponse({
        preferences: [
          {
            target_type: 'server',
            target_id: SERVER_ID,
            muted: true,
            muted_until: null,
          },
        ],
      })
    );

    await tryHydrateNotificationPrefs();

    // Hydration ran (store populated)
    expect(useNotificationPrefsStore.getState().mutedServers.has(SERVER_ID)).toBe(true);
    // Sweep started (setInterval called exactly once for the sweep timer)
    expect(setIntervalSpy).toHaveBeenCalledTimes(1);
    stopExpirySweep();
  });

  it('swallows hydrate failures with a console.warn (Error case)', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    mockApiFetch.mockResolvedValueOnce(mockJsonResponse({}, false));

    // Must not throw — the helper swallows the rejection.
    await expect(tryHydrateNotificationPrefs()).resolves.toBeUndefined();

    expect(warnSpy).toHaveBeenCalledTimes(1);
    const args = warnSpy.mock.calls[0];
    expect(args[0]).toBe('Failed to hydrate notification preferences:');
    expect(args[1]).toBe('Failed to fetch notification preferences');
    warnSpy.mockRestore();
  });

  it('swallows non-Error throws with the unknown_error fallback', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    // Force apiFetch to throw a non-Error value (rare but covered by errorMessage).
    mockApiFetch.mockImplementationOnce(() => {
      throw 'sso_mfa_failed';
    });

    await expect(tryHydrateNotificationPrefs()).resolves.toBeUndefined();

    expect(warnSpy).toHaveBeenCalledTimes(1);
    const args = warnSpy.mock.calls[0];
    expect(args[0]).toBe('Failed to hydrate notification preferences:');
    expect(args[1]).toBe('unknown_error');
    warnSpy.mockRestore();
  });

  it('repeated invocation is safe — startExpirySweep stays single-shot', async () => {
    const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
    mockApiFetch.mockResolvedValue(mockJsonResponse({ preferences: [] }));

    await tryHydrateNotificationPrefs();
    await tryHydrateNotificationPrefs();

    // The first call registers the sweep timer; the second's startExpirySweep
    // is a no-op (sweepTimer already set), so exactly one setInterval call.
    expect(setIntervalSpy).toHaveBeenCalledTimes(1);
    // Both invocations resolved without throwing — proven by the line above
    // being reached.
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
    stopExpirySweep();
  });
});
