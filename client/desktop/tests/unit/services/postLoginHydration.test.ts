import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock every collaborator module before importing the unit under test. The
// repo convention (see subscriptionStore.test.ts) is a hoisted vi.mock factory
// driven via vi.mocked(...), because the unit imports these as ESM named
// bindings that vi.spyOn cannot reassign at the namespace level.
vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    init: vi.fn(),
    startWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    startWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/notificationPrefsService', () => ({
  tryHydrateNotificationPrefs: vi.fn().mockResolvedValue(undefined),
}));

vi.mock('@/renderer/stores/subscriptionStore', () => {
  const hydrate = vi.fn().mockResolvedValue(undefined);
  return {
    useSubscriptionStore: {
      getState: vi.fn(() => ({ hydrate })),
    },
  };
});

import { hydratePostLogin, buildPreferencesSyncDeps } from '@/renderer/services/postLoginHydration';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { tryHydrateNotificationPrefs } from '@/renderer/services/notificationPrefsService';
import { useSubscriptionStore } from '@/renderer/stores/subscriptionStore';

describe('hydratePostLogin', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('runs the full hydration cluster in one call', async () => {
    await hydratePostLogin();

    expect(preferencesSyncService.init).toHaveBeenCalledTimes(1);
    expect(preferencesSyncService.startWatching).toHaveBeenCalledTimes(1);
    expect(preferencesSyncService.fetchAndApply).toHaveBeenCalledTimes(1);
    expect(savedGifsSyncService.startWatching).toHaveBeenCalledTimes(1);
    expect(savedGifsSyncService.fetchAndApply).toHaveBeenCalledTimes(1);
    expect(tryHydrateNotificationPrefs).toHaveBeenCalledTimes(1);
    // useSubscriptionStore.getState().hydrate() is the entitlement hydration.
    expect(useSubscriptionStore.getState).toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).toHaveBeenCalledTimes(1);
  });

  it('initializes preferencesSync with a dependency bag', async () => {
    await hydratePostLogin();
    // init() is called with the deps built by buildPreferencesSyncDeps — a
    // bag exposing the four injection points.
    const deps = vi.mocked(preferencesSyncService.init).mock.calls[0]?.[0];
    expect(deps).toMatchObject({
      getAppearance: expect.any(Function),
      setAppearance: expect.any(Function),
      getLayout: expect.any(Function),
      setLayout: expect.any(Function),
    });
  });
});

describe('buildPreferencesSyncDeps', () => {
  it('returns the four-function dependency bag', () => {
    const deps = buildPreferencesSyncDeps();
    expect(deps).toMatchObject({
      getAppearance: expect.any(Function),
      setAppearance: expect.any(Function),
      getLayout: expect.any(Function),
      setLayout: expect.any(Function),
    });
  });
});
